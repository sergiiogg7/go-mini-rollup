// Package fakel1 is a scriptable in-memory L1 for tests: it mines blocks, includes deposits and
// inbox transactions, replaces stuck transactions and reorganizes on demand. Unlike a mock it
// behaves like a chain, so the same tests exercise the real derivation and batcher code paths.
package fakel1

import (
	"crypto/ecdsa"
	"encoding/binary"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

const ChainID = 31337

type block struct {
	data proto.L1BlockData
	txs  []*gtypes.Transaction
}

type queuedDeposit struct {
	to    common.Address
	value *big.Int
}

// FakeL1 is safe for concurrent use.
type FakeL1 struct {
	mu     sync.Mutex
	Inbox  common.Address
	blocks []*block
	pool   []*gtypes.Transaction
	deps   []queuedDeposit
	salt   uint64
	signer gtypes.Signer
}

// New creates an L1 with only the genesis block.
func New(inbox common.Address) *FakeL1 {
	f := &FakeL1{Inbox: inbox, signer: gtypes.LatestSignerForChainID(big.NewInt(ChainID))}
	h := crypto.Keccak256Hash([]byte("l1-genesis"))
	f.blocks = []*block{{data: proto.L1BlockData{Ref: proto.L1BlockRef{Number: 0, Hash: h, Time: 1000}}}}
	return f
}

func (f *FakeL1) Head() (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return uint64(len(f.blocks) - 1), nil
}

func (f *FakeL1) ChainID() (uint64, error) { return ChainID, nil }

func (f *FakeL1) Block(n uint64) (*proto.L1BlockData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= uint64(len(f.blocks)) {
		return nil, nil
	}
	d := f.blocks[n].data
	return &d, nil
}

func (f *FakeL1) depositCount() uint64 {
	var c uint64
	for _, b := range f.blocks {
		c += uint64(len(b.data.Deposits))
	}
	return c
}

// QueueDeposit adds a deposit to the next mined block.
func (f *FakeL1) QueueDeposit(to common.Address, value *big.Int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deps = append(f.deps, queuedDeposit{to, value})
}

func (f *FakeL1) mineLocked() {
	prev := f.blocks[len(f.blocks)-1].data.Ref
	n := prev.Number + 1
	buf := make([]byte, 16)
	binary.BigEndian.PutUint64(buf, n)
	binary.BigEndian.PutUint64(buf[8:], f.salt)
	parts := [][]byte{prev.Hash[:], buf}
	b := &block{}
	for _, tx := range f.pool {
		from, err := gtypes.Sender(f.signer, tx)
		if err != nil {
			continue
		}
		b.txs = append(b.txs, tx)
		parts = append(parts, tx.Hash().Bytes())
		if tx.To() != nil && *tx.To() == f.Inbox {
			b.data.Batches = append(b.data.Batches, proto.BatchTx{TxHash: tx.Hash(), From: from, Data: tx.Data()})
		}
	}
	f.pool = nil
	id := f.depositCount()
	for _, d := range f.deps {
		b.data.Deposits = append(b.data.Deposits, proto.Deposit{ID: id, To: d.to, Value: d.value})
		parts = append(parts, d.to[:], d.value.Bytes())
		id++
	}
	f.deps = nil
	b.data.Ref = proto.L1BlockRef{Number: n, Hash: crypto.Keccak256Hash(parts...), ParentHash: prev.Hash, Time: 1000 + n*2}
	f.blocks = append(f.blocks, b)
}

// Mine mines n blocks.
func (f *FakeL1) Mine(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := 0; i < n; i++ {
		f.mineLocked()
	}
}

// Reorg replaces the last depth blocks by new ones with different hashes. With replay the dropped
// transactions and deposits are re-included in the first replacement block; without it they vanish.
func (f *FakeL1) Reorg(depth int, replay bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if depth >= len(f.blocks) {
		depth = len(f.blocks) - 1
	}
	dropped := f.blocks[len(f.blocks)-depth:]
	f.blocks = f.blocks[:len(f.blocks)-depth]
	if replay {
		for _, b := range dropped {
			f.pool = append(f.pool, b.txs...)
			for _, d := range b.data.Deposits {
				f.deps = append(f.deps, queuedDeposit{d.To, d.Value})
			}
		}
	}
	f.salt++
	for i := 0; i < depth; i++ {
		f.mineLocked()
	}
}

func (f *FakeL1) confirmedNonce(a common.Address) uint64 {
	var c uint64
	for _, b := range f.blocks {
		for _, tx := range b.txs {
			if from, err := gtypes.Sender(f.signer, tx); err == nil && from == a {
				c++
			}
		}
	}
	return c
}

// Nonce is the confirmed nonce (transactions in the canonical chain).
func (f *FakeL1) Nonce(a common.Address) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.confirmedNonce(a), nil
}

func (f *FakeL1) PendingNonce(a common.Address) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pendingNonce(a), nil
}

func (f *FakeL1) pendingNonce(a common.Address) uint64 {
	n := f.confirmedNonce(a)
	for _, tx := range f.pool {
		if from, err := gtypes.Sender(f.signer, tx); err == nil && from == a {
			n++
		}
	}
	return n
}

func (f *FakeL1) GasPrice() (*big.Int, error) { return big.NewInt(1_000_000_000), nil }

// Send adds a signed transaction to the pool. A transaction with the nonce of a pooled one
// replaces it only if its gas price is at least 10 % higher (like a real node).
func (f *FakeL1) Send(raw []byte) (common.Hash, error) {
	tx := new(gtypes.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, err
	}
	from, err := gtypes.Sender(f.signer, tx)
	if err != nil {
		return common.Hash{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	conf := f.confirmedNonce(from)
	if tx.Nonce() < conf {
		return common.Hash{}, errors.New("nonce too low")
	}
	idx := int(tx.Nonce() - conf)
	var mine []int
	for i, p := range f.pool {
		if pf, e := gtypes.Sender(f.signer, p); e == nil && pf == from {
			mine = append(mine, i)
		}
	}
	switch {
	case idx < len(mine):
		old := f.pool[mine[idx]]
		min := new(big.Int).Mul(old.GasPrice(), big.NewInt(110))
		min.Div(min, big.NewInt(100))
		if tx.GasPrice().Cmp(min) < 0 {
			return common.Hash{}, errors.New("replacement transaction underpriced")
		}
		f.pool[mine[idx]] = tx
	case idx == len(mine):
		f.pool = append(f.pool, tx)
	default:
		return common.Hash{}, fmt.Errorf("nonce gap: got %d want %d", tx.Nonce(), conf+uint64(len(mine)))
	}
	return tx.Hash(), nil
}

// Receipt returns where a transaction was mined, or nil if it is not in the canonical chain.
func (f *FakeL1) Receipt(h common.Hash) (*proto.TxInclusion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.blocks) - 1; i >= 0; i-- {
		for _, tx := range f.blocks[i].txs {
			if tx.Hash() == h {
				return &proto.TxInclusion{BlockNumber: f.blocks[i].data.Ref.Number, BlockHash: f.blocks[i].data.Ref.Hash}, nil
			}
		}
	}
	return nil, nil
}

// SendData signs and sends a transaction carrying data from the given key (test helper).
func (f *FakeL1) SendData(key *ecdsa.PrivateKey, to common.Address, data []byte) (common.Hash, error) {
	from := crypto.PubkeyToAddress(key.PublicKey)
	nonce, _ := f.PendingNonce(from)
	tx, err := gtypes.SignNewTx(key, f.signer, &gtypes.LegacyTx{
		Nonce: nonce, GasPrice: big.NewInt(1_000_000_000), Gas: 21000 + uint64(len(data))*16 + 10_000, To: &to, Data: data,
	})
	if err != nil {
		return common.Hash{}, err
	}
	raw, _ := tx.MarshalBinary()
	return f.Send(raw)
}
