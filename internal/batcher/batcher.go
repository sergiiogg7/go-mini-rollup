// Package batcher publishes one EpochBatch per closed epoch to the L1 inbox. It survives crashes
// (it recovers its position by scanning L1, no local database), stuck transactions (fee bump with
// the same nonce) and L1 reorgs (batches that disappear are re-signed on top of the confirmed
// nonce and sent again).
package batcher

import (
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/sergiiogg7/go-mini-rollup/internal/derive"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

// UnsafeSource gives the batcher the epochs the sequencer has finished (a port).
type UnsafeSource interface {
	ClosedEpoch(epoch uint64) (*proto.Draft, bool)
}

// L1Submitter is the write side of L1 (a port). The derivation never receives this interface.
type L1Submitter interface {
	Head() (uint64, error)
	ChainID() (uint64, error)
	// Nonce is the confirmed nonce (latest block); PendingNonce includes the node's pool.
	Nonce(common.Address) (uint64, error)
	PendingNonce(common.Address) (uint64, error)
	GasPrice() (*big.Int, error)
	Send(raw []byte) (common.Hash, error)
	// Receipt returns nil, nil while the transaction is not in the canonical chain.
	Receipt(common.Hash) (*proto.TxInclusion, error)
}

type tracked struct {
	epoch    uint64
	data     []byte
	hash     common.Hash
	nonce    uint64
	gasPrice *big.Int
	incl     *proto.TxInclusion // nil while not in a block
	age      int                // ticks since the last (re)send without inclusion
	sentAt   time.Time          // first send, or the moment an L1 reorg dropped it again (fee bumps keep it)
}

// Batcher is driven by Tick (no goroutines inside, easy to test).
type Batcher struct {
	cfg  *proto.Config
	src  UnsafeSource
	l1   derive.L1Source
	sub  L1Submitter
	key  *ecdsa.PrivateKey
	addr common.Address
	m    metrics.Sink

	MaxInflight   int
	Confirmations uint64 // blocks on top of the inclusion block before a batch stops counting as in flight
	BumpAfter     int    // ticks without inclusion before replacing with a higher fee
	Now           func() time.Time
	MaxGasPrice   *big.Int // ceiling for fee bumps (default 200 gwei); nil disables the cap

	started   bool
	nextEpoch uint64
	nonce     uint64
	inflight  []*tracked // not yet confirmed
	watched   []*tracked // confirmed but still watched for reorgs until buried
}

func New(cfg *proto.Config, src UnsafeSource, l1 derive.L1Source, sub L1Submitter, key *ecdsa.PrivateKey, m metrics.Sink) *Batcher {
	if m == nil {
		m = metrics.Nop{}
	}
	return &Batcher{
		cfg: cfg, src: src, l1: l1, sub: sub, key: key, addr: crypto.PubkeyToAddress(key.PublicKey), m: m,
		MaxInflight: 4, Confirmations: 1, BumpAfter: 5, Now: time.Now,
		MaxGasPrice: new(big.Int).Mul(big.NewInt(200), big.NewInt(1_000_000_000)),
	}
}

// recover finds the first epoch that has no batch from us on L1 yet.
func (b *Batcher) recover() error {
	head, err := b.l1.Head()
	if err != nil {
		return err
	}
	have := map[uint64]bool{}
	for n := b.cfg.L1Start; n <= head; n++ {
		d, err := b.l1.Block(n)
		if err != nil {
			return err
		}
		if d == nil {
			break
		}
		p := b.cfg.ParamsAt(n)
		for _, bt := range d.Batches {
			if bt.From != b.addr {
				continue
			}
			if eb, err := proto.DecodeBatch(bt.Data, p.MaxCompressedBytes, p.MaxDecompressedBytes); err == nil {
				have[eb.Epoch] = true
			}
		}
	}
	b.nextEpoch = b.cfg.L1Start
	for have[b.nextEpoch] {
		b.nextEpoch++
	}
	if b.nonce, err = b.sub.PendingNonce(b.addr); err != nil {
		return err
	}
	b.started = true
	return nil
}

// Tick advances the batcher one step: track in-flight batches, then submit new ones.
func (b *Batcher) Tick() error {
	if !b.started {
		if err := b.recover(); err != nil {
			return err
		}
	}
	head, err := b.sub.Head()
	if err != nil {
		return err
	}
	if err := b.track(head); err != nil {
		b.m.Inc(metrics.BatcherErrors, "stage", "track")
		return err
	}
	for len(b.inflight) < b.MaxInflight {
		d, ok := b.src.ClosedEpoch(b.nextEpoch)
		if !ok {
			break
		}
		if err := b.submit(d); err != nil {
			b.m.Inc(metrics.BatcherErrors, "stage", "submit")
			return err
		}
		b.nextEpoch++
	}
	b.m.Set(metrics.BatcherNextEpoch, float64(b.nextEpoch))
	b.m.Set(metrics.BatcherInflight, float64(len(b.inflight)))
	b.m.Set(metrics.BatcherOldestAge, b.oldestUnincludedAge().Seconds())
	return nil
}

func (b *Batcher) sign(t *tracked) ([]byte, error) {
	cid, err := b.sub.ChainID()
	if err != nil {
		return nil, err
	}
	tx, err := gtypes.SignNewTx(b.key, gtypes.LatestSignerForChainID(new(big.Int).SetUint64(cid)), &gtypes.LegacyTx{
		Nonce: t.nonce, GasPrice: t.gasPrice, Gas: 21000 + 16*uint64(len(t.data)) + 20_000, To: &b.cfg.Inbox, Data: t.data,
	})
	if err != nil {
		return nil, err
	}
	return tx.MarshalBinary()
}

func (b *Batcher) send(t *tracked) error {
	raw, err := b.sign(t)
	if err != nil {
		return err
	}
	h, err := b.sub.Send(raw)
	if err != nil {
		return err
	}
	t.hash, t.incl, t.age = h, nil, 0
	return nil
}

func nonceError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "nonce")
}

func (b *Batcher) submit(d *proto.Draft) error {
	p := b.cfg.ParamsAt(d.Epoch)
	eb := &proto.EpochBatch{Version: p.BatchVersion, Epoch: d.Epoch, Blocks: d.Blocks}
	if p.BatchVersion >= 1 {
		eb.OriginHash = d.OriginHash
	}
	data, err := proto.EncodeBatch(eb)
	if err != nil {
		return err
	}
	if len(data) > p.MaxCompressedBytes {
		return fmt.Errorf("epoch %d batch is %d bytes, over the %d byte limit", d.Epoch, len(data), p.MaxCompressedBytes)
	}
	price, err := b.sub.GasPrice()
	if err != nil {
		return err
	}
	t := &tracked{epoch: d.Epoch, data: data, nonce: b.nonce, gasPrice: price, sentAt: b.Now()}
	if err := b.send(t); err != nil {
		if !nonceError(err) {
			return err
		}
		// A reorg dropped batches we never saw included (so we could not tell them from pending
		// ones), or another sender used our key: re-sign everything unconfirmed on top of the
		// confirmed nonce, then retry this one.
		if err := b.rebase(b.allTracked()); err != nil {
			return err
		}
		t.nonce = b.nonce
		if err := b.send(t); err != nil {
			return err
		}
	}
	b.nonce = t.nonce + 1
	b.inflight = append(b.inflight, t)
	b.m.Inc(metrics.BatchSubmitted)
	return nil
}

// bump raises a gas price by 25 % (the minimum most nodes accept for a replacement), never above
// MaxGasPrice: without a cap, a batch stuck for minutes would compound the fee exponentially.
func (b *Batcher) bump(p *big.Int) *big.Int {
	np := new(big.Int).Mul(p, big.NewInt(125))
	np.Div(np, big.NewInt(100))
	np.Add(np, big.NewInt(1))
	if b.MaxGasPrice != nil && np.Cmp(b.MaxGasPrice) > 0 {
		np = new(big.Int).Set(b.MaxGasPrice)
	}
	return np
}

func (b *Batcher) allTracked() []*tracked {
	all := append(append([]*tracked{}, b.watched...), b.inflight...)
	sort.Slice(all, func(i, j int) bool { return all[i].epoch < all[j].epoch })
	return all
}

func (b *Batcher) track(head uint64) error {
	all := b.allTracked()
	reorged := false
	for _, t := range all {
		r, err := b.sub.Receipt(t.hash)
		if err != nil {
			return err
		}
		switch {
		case r == nil && t.incl != nil: // the block that held it left the canonical chain
			t.incl = nil
			t.age = 0
			t.sentAt = b.Now()
			reorged = true
			b.m.Inc(metrics.BatcherResubmit, "cause", "l1_reorg")
		case r == nil:
			t.age++
		default:
			t.incl = r
		}
	}
	if reorged {
		if err := b.rebase(all); err != nil {
			return err
		}
	} else {
		for _, t := range all {
			if t.incl == nil && t.age >= b.BumpAfter {
				np := b.bump(t.gasPrice)
				if np.Cmp(t.gasPrice) <= 0 {
					continue // already at the cap: resending the same transaction would be refused as known
				}
				t.gasPrice = np
				if err := b.send(t); err != nil {
					if !nonceError(err) {
						return fmt.Errorf("fee bump of epoch %d failed: %w", t.epoch, err)
					}
					// its nonce was taken or dropped: rebuild the whole unconfirmed queue
					if err := b.rebase(all); err != nil {
						return err
					}
					b.m.Inc(metrics.BatcherResubmit, "cause", "nonce_rebase")
					break
				}
				b.m.Inc(metrics.BatcherResubmit, "cause", "fee_bump")
			}
		}
	}
	b.inflight, b.watched = nil, nil
	for _, t := range all {
		switch {
		case t.incl == nil || head+1 < t.incl.BlockNumber+b.Confirmations:
			b.inflight = append(b.inflight, t)
		case head < t.incl.BlockNumber+b.cfg.ParamsAt(t.incl.BlockNumber).FinalityDepth:
			b.watched = append(b.watched, t)
		}
	}
	return nil
}

// rebase re-signs every batch that is not in a block on top of the confirmed nonce, in epoch
// order, with a higher fee so it replaces whatever is left in the node's pool.
func (b *Batcher) rebase(all []*tracked) error {
	base, err := b.sub.Nonce(b.addr)
	if err != nil {
		return err
	}
	for _, t := range all {
		if t.incl != nil {
			continue
		}
		t.nonce, t.gasPrice = base, b.bump(t.gasPrice)
		base++
		if err := b.send(t); err != nil {
			return fmt.Errorf("rebase of epoch %d failed: %w", t.epoch, err)
		}
	}
	b.nonce = base
	return nil
}

// NextEpoch is the next epoch the batcher will publish.
func (b *Batcher) NextEpoch() uint64 { return b.nextEpoch }

// oldestUnincludedAge is how long the oldest batch that is not in an L1 block has been waiting.
func (b *Batcher) oldestUnincludedAge() time.Duration {
	var oldest time.Duration
	now := b.Now()
	for _, t := range b.inflight {
		if t.incl == nil {
			oldest = max(oldest, now.Sub(t.sentAt))
		}
	}
	return oldest
}
