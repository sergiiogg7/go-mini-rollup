// Package sequencer orders L2 transactions and builds the unsafe chain. It is a preview of the
// canonical chain: the derivation (from L1 only) has the final word, and the sequencer converges to
// it, re-queueing the transactions of any unsafe block that the canonical chain replaced.
package sequencer

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/sergiiogg7/go-mini-rollup/internal/derive"
	"github.com/sergiiogg7/go-mini-rollup/internal/exec"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

// SafeChain is the canonical, derived chain (a port; implemented by derive.Derivator).
type SafeChain interface {
	SafeHead() *proto.Block
	SafeBlock(n uint64) *proto.Block
	Finalized() uint64
}

var _ SafeChain = (*derive.Derivator)(nil)

// Loc is where a transaction lives in the unsafe chain.
type Loc struct {
	Block *proto.Block
	Index int
}

// Sequencer builds blocks. All mutation happens under mu; readers take RLock.
type Sequencer struct {
	mu     sync.RWMutex
	cfg    *proto.Config
	ex     *exec.Executor
	l1     derive.L1Source
	safe   SafeChain
	m      metrics.Sink
	pool   *Pool
	chain  []*proto.Block
	txIdx  map[common.Hash]Loc
	signer gtypes.Signer
	Now    func() uint64

	epoch        uint64 // origin of the head block
	epochTime    uint64
	closed       bool // no more blocks may be added to the current epoch
	inEpoch      int
	verified     uint64 // chain[0..verified] equals the safe chain
	unsafeReorgs int
}

// New creates a sequencer at the L2 genesis. The executor must be shared with the derivator.
func New(cfg *proto.Config, ex *exec.Executor, l1 derive.L1Source, safe SafeChain, m metrics.Sink) (*Sequencer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := safe.SafeBlock(0)
	if g == nil {
		return nil, errors.New("safe chain has no genesis")
	}
	if m == nil {
		m = metrics.Nop{}
	}
	p := cfg.Forks[0].Params
	return &Sequencer{
		cfg: cfg, ex: ex, l1: l1, safe: safe, m: m, pool: NewPool(4096), chain: []*proto.Block{g},
		txIdx: map[common.Hash]Loc{}, signer: gtypes.LatestSignerForChainID(new(big.Int).SetUint64(p.ChainID)),
		Now: func() uint64 { return uint64(time.Now().Unix()) }, epoch: cfg.L1Start - 1,
	}, nil
}

func (s *Sequencer) head() *proto.Block { return s.chain[len(s.chain)-1] }

// ---------------------------------------------------------------- write side

// Submit validates and queues a raw signed transaction.
func (s *Sequencer) Submit(raw []byte) (common.Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.cfg.ParamsAt(s.epoch + 1)
	if len(raw) > p.MaxTxBytes {
		return common.Hash{}, errors.New("transaction too large")
	}
	tx := new(gtypes.Transaction)
	if err := tx.UnmarshalBinary(raw); err != nil {
		return common.Hash{}, fmt.Errorf("decode: %w", err)
	}
	switch {
	case tx.Type() == gtypes.BlobTxType:
		return common.Hash{}, errors.New("blob transactions are not supported")
	case !tx.Protected():
		return common.Hash{}, errors.New("only replay-protected transactions are accepted")
	case tx.Gas() > p.GasLimit:
		return common.Hash{}, errors.New("gas limit exceeds block gas limit")
	}
	from, err := gtypes.Sender(s.signer, tx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("invalid sender: %w", err)
	}
	st, err := s.ex.StateAt(s.head().Header.Root)
	if err != nil {
		return common.Hash{}, err
	}
	if err := s.pool.Add(tx, from, st.GetNonce(from), st.GetBalance(from).ToBig()); err != nil {
		s.m.Inc("rollup_tx_rejected_total", "reason", err.Error())
		return common.Hash{}, err
	}
	s.m.Set("rollup_txpool_size", float64(s.pool.Len()))
	return tx.Hash(), nil
}

// Tick runs one sequencing round: reconcile with L1 and with the canonical chain, advance epochs,
// and build a block from the pool. It returns the number of blocks produced.
func (s *Sequencer) Tick() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head, err := s.l1.Head()
	if err != nil {
		return 0, err
	}
	if err := s.reconcileL1(); err != nil {
		return 0, err
	}
	s.reconcileSafe()
	produced := 0
	p := s.cfg.ParamsAt(s.epoch + 1)
	for head >= p.OriginConfirmations && s.epoch < head-p.OriginConfirmations && produced < 64 {
		ok, err := s.advanceEpoch()
		if err != nil {
			return produced, err
		}
		if !ok {
			break
		}
		produced++
	}
	if s.epoch >= s.cfg.L1Start && s.pool.Len() > 0 {
		ok, err := s.buildTxBlock()
		if err != nil {
			return produced, err
		}
		if ok {
			produced++
		}
	}
	s.m.Set("rollup_unsafe_head", float64(s.head().Number()))
	s.m.Set("rollup_txpool_size", float64(s.pool.Len()))
	return produced, nil
}

func (s *Sequencer) advanceEpoch() (bool, error) {
	next := s.epoch + 1
	data, err := s.l1.Block(next)
	if err != nil || data == nil {
		return false, err
	}
	parent := s.head()
	in := exec.Input{
		Number: parent.Number() + 1, Time: max(parent.Time()+1, data.Ref.Time), ParentHash: parent.Hash(),
		OriginNumber: next, OriginHash: data.Ref.Hash, Deposits: data.Deposits,
	}
	res, err := s.ex.Execute(parent.Header.Root, in)
	if err != nil {
		return false, err
	}
	s.append(res.Block)
	s.epoch, s.epochTime, s.inEpoch, s.closed = next, data.Ref.Time, 1, false
	s.m.Inc("rollup_blocks_built_total", "kind", "epoch")
	return true, nil
}

func (s *Sequencer) buildTxBlock() (bool, error) {
	p := s.cfg.ParamsAt(s.epoch)
	if s.closed || s.inEpoch >= p.MaxBlocksPerEpoch {
		return false, nil
	}
	parent := s.head()
	ts := max(s.Now(), parent.Time()+1)
	if ts > s.epochTime+p.MaxDriftSeconds {
		return false, nil // too far from the origin: wait for the next epoch
	}
	root := parent.Header.Root
	st, err := s.ex.StateAt(root)
	if err != nil {
		return false, err
	}
	s.pool.Prune(st.GetNonce)
	txs := s.pool.Select(st.GetNonce, 256, p.GasLimit)
	if len(txs) == 0 {
		return false, nil
	}
	res, err := s.ex.Execute(root, exec.Input{
		Number: parent.Number() + 1, Time: ts, ParentHash: parent.Hash(),
		OriginNumber: s.epoch, OriginHash: parent.OriginHash, Txs: txs,
	})
	if err != nil {
		return false, err
	}
	for _, t := range txs {
		s.pool.Remove(t.Hash()) // included or skipped as invalid: either way it leaves the pool
	}
	for range res.Skipped {
		s.m.Inc("rollup_tx_skipped_total", "reason", "sequencer")
	}
	if len(res.Block.Txs) == 0 {
		return false, nil
	}
	s.append(res.Block)
	s.inEpoch++
	s.m.Inc("rollup_blocks_built_total", "kind", "tx")
	return true, nil
}

func (s *Sequencer) append(b *proto.Block) {
	s.chain = append(s.chain, b)
	s.ex.SetBlockHash(b.Number(), b.Hash())
	for i, tx := range b.Txs {
		s.txIdx[tx.Hash()] = Loc{b, i}
	}
}

// truncate drops blocks above n, re-queueing their transactions.
func (s *Sequencer) truncate(n uint64) {
	var back []*gtypes.Transaction
	for i := uint64(len(s.chain)) - 1; i > n; i-- {
		b := s.chain[i]
		for _, tx := range b.Txs {
			delete(s.txIdx, tx.Hash())
			back = append(back, tx)
		}
		s.ex.SetBlockHash(b.Number(), common.Hash{})
	}
	s.chain = s.chain[:n+1]
	s.pool.Requeue(back, func(tx *gtypes.Transaction) (common.Address, error) { return gtypes.Sender(s.signer, tx) })
	if s.verified > n {
		s.verified = n
	}
	s.closed = false
	s.resetEpoch()
}

// resetEpoch recomputes the current epoch from the head block.
func (s *Sequencer) resetEpoch() {
	h := s.head()
	s.epoch = h.OriginNumber
	s.inEpoch = 0
	for i := len(s.chain) - 1; i > 0 && s.chain[i].OriginNumber == s.epoch; i-- {
		s.inEpoch++
	}
	s.epochTime = 0
	if s.epoch >= s.cfg.L1Start {
		if d, err := s.l1.Block(s.epoch); err == nil && d != nil {
			s.epochTime = d.Ref.Time
		}
	}
}

// reconcileL1 drops unsafe blocks whose L1 origin is no longer on the canonical L1 chain.
func (s *Sequencer) reconcileL1() error {
	if len(s.chain) == 1 {
		return nil
	}
	n := uint64(len(s.chain) - 1)
	checked := map[uint64]bool{}
	for ; n > 0; n-- {
		b := s.chain[n]
		if n <= s.verified {
			break // equals a derived block: its origin is canonical by construction
		}
		ok, seen := checked[b.OriginNumber]
		if !seen {
			d, err := s.l1.Block(b.OriginNumber)
			if err != nil {
				return err
			}
			ok = d != nil && d.Ref.Hash == b.OriginHash
			checked[b.OriginNumber] = ok
		}
		if ok {
			break
		}
	}
	if n < uint64(len(s.chain)-1) {
		s.truncate(n)
		s.unsafeReorgs++
		s.m.Inc("rollup_unsafe_reorg_total", "cause", "l1_reorg")
	}
	return nil
}

// reconcileSafe makes the unsafe chain agree with the canonical chain.
func (s *Sequencer) reconcileSafe() {
	sh := s.safe.SafeHead().Number()
	// the canonical chain may itself have been rewound by an L1 reorg
	v := min(s.verified, sh, uint64(len(s.chain)-1))
	for v > 0 {
		sb := s.safe.SafeBlock(v)
		if sb != nil && s.chain[v].Hash() == sb.Hash() {
			break
		}
		v--
	}
	s.verified = v
	for n := v + 1; n <= sh; n++ {
		sb := s.safe.SafeBlock(n)
		if sb == nil {
			break
		}
		if n < uint64(len(s.chain)) {
			if s.chain[n].Hash() == sb.Hash() {
				s.verified = n
				continue
			}
			s.truncate(n - 1)
			s.unsafeReorgs++
			s.m.Inc("rollup_unsafe_reorg_total", "cause", "derivation_mismatch")
		}
		s.append(sb) // the canonical block, already executed (shared executor)
		s.verified = n
		s.resetEpoch()
		s.closed = true // the derivation already decided this epoch: no more blocks for it
	}
}

// ---------------------------------------------------------------- read side

// Head is the unsafe head.
func (s *Sequencer) Head() *proto.Block {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.head()
}

// Block returns an unsafe-chain block by number.
func (s *Sequencer) Block(n uint64) *proto.Block {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n < uint64(len(s.chain)) {
		return s.chain[n]
	}
	return nil
}

// BlockByHash scans the unsafe chain backwards.
func (s *Sequencer) BlockByHash(h common.Hash) *proto.Block {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := len(s.chain) - 1; i >= 0; i-- {
		if s.chain[i].Hash() == h {
			return s.chain[i]
		}
	}
	return nil
}

// Tx finds an included transaction.
func (s *Sequencer) Tx(h common.Hash) (Loc, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	l, ok := s.txIdx[h]
	return l, ok
}

// Pending reports whether a transaction is waiting in the pool.
func (s *Sequencer) Pending(h common.Hash) bool { return s.pool.Has(h) }

// PendingNonce is the nonce a wallet should use next.
func (s *Sequencer) PendingNonce(a common.Address) uint64 {
	s.mu.RLock()
	root := s.head().Header.Root
	s.mu.RUnlock()
	st, err := s.ex.StateAt(root)
	if err != nil {
		return 0
	}
	return s.pool.PendingNonce(a, st.GetNonce(a))
}

// CurrentEpoch is the epoch being filled.
func (s *Sequencer) CurrentEpoch() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.epoch
}

// UnsafeReorgs counts how many times the unsafe chain was rewritten.
func (s *Sequencer) UnsafeReorgs() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.unsafeReorgs
}

// ClosedEpoch returns the blocks of an epoch the sequencer has finished (epoch < current).
func (s *Sequencer) ClosedEpoch(epoch uint64) (*proto.Draft, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if epoch >= s.epoch || epoch < s.cfg.L1Start {
		return nil, false
	}
	d := &proto.Draft{Epoch: epoch}
	for i := len(s.chain) - 1; i > 0; i-- {
		b := s.chain[i]
		if b.OriginNumber < epoch {
			break
		}
		if b.OriginNumber != epoch {
			continue
		}
		d.OriginHash = b.OriginHash
		bb := proto.BatchBlock{Time: b.Time()}
		for _, tx := range b.Txs {
			raw, err := tx.MarshalBinary()
			if err != nil {
				return nil, false
			}
			bb.Txs = append(bb.Txs, raw)
		}
		d.Blocks = append([]proto.BatchBlock{bb}, d.Blocks...)
	}
	return d, len(d.Blocks) > 0
}

// PoolNonce returns the next usable nonce counting consecutive pooled transactions.
func (s *Sequencer) PoolNonce(a common.Address, stateNonce uint64) uint64 {
	return s.pool.PendingNonce(a, stateNonce)
}

// PoolSize is the number of pending transactions.
func (s *Sequencer) PoolSize() int { return s.pool.Len() }
