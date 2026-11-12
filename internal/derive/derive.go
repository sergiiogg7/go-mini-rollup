// Package derive rebuilds the canonical L2 chain from L1 data only. It is a deterministic function
// of the L1 chain: it can restart from L1 at any time and must always reach the same blocks.
//
// Rules (see docs/SPEC.md):
//  1. A batch counts only if it was sent to the inbox by the configured batcher.
//  2. It must decode within the size limits and use the batch version of its epoch.
//  3. It must be included at an L1 block in [epoch, epoch+SeqWindow] and cover an epoch not yet resolved.
//  4. 1..MaxBlocksPerEpoch blocks; strictly increasing timestamps, >= the epoch's L1 timestamp,
//     <= that timestamp + drift, and greater than the previous L2 block's timestamp.
//  5. The first valid batch (in L1 order) for an epoch wins; later ones are ignored.
//  6. The epoch's deposits are applied at the start of its first block.
//  7. If no valid batch exists once L1 reaches epoch+SeqWindow, one empty block is derived.
//  8. L1 reorgs roll the derivation back to the common ancestor and reprocess.
package derive

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"

	"github.com/sergiiogg7/go-mini-rollup/internal/exec"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
)

// L1Source is the read-only view of L1 the derivation needs (a port).
type L1Source interface {
	Head() (uint64, error)
	// Block returns nil, nil when the block does not exist (yet).
	Block(n uint64) (*proto.L1BlockData, error)
}

// ErrDeepReorg means an L1 reorg is deeper than the retained snapshots; a full resync is needed.
var ErrDeepReorg = errors.New("L1 reorg deeper than retained snapshots")

// Derived is a safe L2 block and the L1 block at which it was derived.
type Derived struct {
	Block *proto.Block
	L1    uint64
}

type candidate struct {
	incl  uint64
	batch *proto.EpochBatch
}

type epochInfo struct {
	ref      proto.L1BlockRef
	deposits []proto.Deposit
}

type state struct {
	cursor    *proto.L1BlockRef
	nextEpoch uint64
	cands     map[uint64][]candidate
	epochs    map[uint64]epochInfo
	head      *proto.Block
}

func (s state) clone() state {
	c := state{nextEpoch: s.nextEpoch, head: s.head, cands: map[uint64][]candidate{}, epochs: map[uint64]epochInfo{}}
	if s.cursor != nil {
		r := *s.cursor
		c.cursor = &r
	}
	for k, v := range s.cands {
		c.cands[k] = slices.Clone(v)
	}
	for k, v := range s.epochs {
		c.epochs[k] = v
	}
	return c
}

type snapshot struct {
	st      state
	nblocks int
}

// Status is a point-in-time summary for RPC and metrics.
type Status struct {
	Safe, Finalized     uint64
	L1Cursor, NextEpoch uint64
	L1Head              uint64
	Reorgs              int
}

// Derivator is the verifier node.
type Derivator struct {
	mu     sync.Mutex
	cfg    *proto.Config
	ex     *exec.Executor
	l1     L1Source
	m      metrics.Sink
	blocks []Derived
	st     state
	snaps  map[uint64]snapshot
	reorgs int
}

// New creates a derivator at the L2 genesis.
func New(cfg *proto.Config, ex *exec.Executor, l1 L1Source, m metrics.Sink) (*Derivator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.L1Start == 0 {
		return nil, errors.New("L1Start must be >= 1")
	}
	g, err := ex.Genesis(cfg)
	if err != nil {
		return nil, err
	}
	if m == nil {
		m = metrics.Nop{}
	}
	ex.SetBlockHash(0, g.Hash())
	d := &Derivator{cfg: cfg, ex: ex, l1: l1, m: m, blocks: []Derived{{g, 0}}, snaps: map[uint64]snapshot{}}
	d.st = state{nextEpoch: cfg.L1Start, head: g, cands: map[uint64][]candidate{}, epochs: map[uint64]epochInfo{}}
	d.snaps[cfg.L1Start-1] = snapshot{st: d.st.clone(), nblocks: 1}
	return d, nil
}

// Step processes at most one L1 block (or one reorg). It returns true if progress was made.
func (d *Derivator) Step() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	head, err := d.l1.Head()
	if err != nil {
		return false, err
	}
	d.m.Set("rollup_l1_head", float64(head))
	next := d.cfg.L1Start
	if d.st.cursor != nil {
		next = d.st.cursor.Number + 1
	}
	if next > head { // idle: make sure our tip is still canonical
		if d.st.cursor == nil {
			return false, nil
		}
		if head < d.st.cursor.Number {
			return true, d.rewind()
		}
		tip, err := d.l1.Block(d.st.cursor.Number)
		if err != nil || tip == nil {
			return false, err
		}
		if tip.Ref.Hash != d.st.cursor.Hash {
			return true, d.rewind()
		}
		return false, nil
	}
	data, err := d.l1.Block(next)
	if err != nil || data == nil {
		return false, err
	}
	if d.st.cursor != nil && data.Ref.ParentHash != d.st.cursor.Hash {
		return true, d.rewind()
	}
	d.process(data)
	return true, nil
}

// SyncAll steps until no more progress. Useful for tests and one-shot verification.
func (d *Derivator) SyncAll() error {
	for {
		ok, err := d.Step()
		if err != nil || !ok {
			return err
		}
	}
}

// rewind rolls back to the newest snapshot whose L1 block is still canonical.
func (d *Derivator) rewind() error {
	cur := d.st.cursor.Number
	for n := cur; ; n-- {
		s, ok := d.snaps[n]
		if !ok {
			return fmt.Errorf("%w (cursor %d)", ErrDeepReorg, cur)
		}
		canonical := n == d.cfg.L1Start-1
		if !canonical {
			data, err := d.l1.Block(n)
			if err != nil {
				return err
			}
			canonical = data != nil && data.Ref.Hash == s.st.cursor.Hash
		}
		if canonical {
			d.restore(n, s)
			d.reorgs++
			d.m.Inc("rollup_reorg_total", "depth", fmt.Sprint(cur-n))
			return nil
		}
		if n == 0 {
			return ErrDeepReorg
		}
	}
}

func (d *Derivator) restore(n uint64, s snapshot) {
	for i := s.nblocks; i < len(d.blocks); i++ {
		d.ex.SetBlockHash(uint64(i), common.Hash{})
	}
	d.blocks = d.blocks[:s.nblocks]
	d.st = s.st.clone()
	for k := range d.snaps {
		if k > n {
			delete(d.snaps, k)
		}
	}
}

func (d *Derivator) process(data *proto.L1BlockData) {
	ref := data.Ref
	d.st.epochs[ref.Number] = epochInfo{ref: ref, deposits: data.Deposits}
	for _, bt := range data.Batches {
		d.handleBatch(ref, bt)
	}
	d.resolve(ref)
	r := ref
	d.st.cursor = &r
	d.snaps[ref.Number] = snapshot{st: d.st.clone(), nblocks: len(d.blocks)}
	keep := d.cfg.ParamsAt(ref.Number).FinalityDepth + d.cfg.ParamsAt(ref.Number).SeqWindow + 32
	for k := range d.snaps {
		if k != d.cfg.L1Start-1 && k+keep < ref.Number {
			delete(d.snaps, k)
		}
	}
	d.m.Set("rollup_safe_head", float64(d.st.head.Number()))
	d.m.Set("rollup_l1_cursor", float64(ref.Number))
	d.m.Set("rollup_next_epoch", float64(d.st.nextEpoch))
}

func (d *Derivator) reject(reason string) { d.m.Inc("rollup_batch_rejected_total", "reason", reason) }

func (d *Derivator) handleBatch(ref proto.L1BlockRef, bt proto.BatchTx) {
	if bt.From != d.cfg.Batcher {
		d.reject("sender")
		return
	}
	pInc := d.cfg.ParamsAt(ref.Number)
	b, err := proto.DecodeBatch(bt.Data, pInc.MaxCompressedBytes, pInc.MaxDecompressedBytes)
	if err != nil {
		d.reject("decode")
		return
	}
	switch {
	case b.Epoch > ref.Number:
		d.reject("future_epoch")
		return
	case b.Epoch < d.st.nextEpoch:
		d.reject("stale_epoch")
		return
	}
	p := d.cfg.ParamsAt(b.Epoch)
	switch {
	case ref.Number > b.Epoch+p.SeqWindow:
		d.reject("late")
	case b.Version != p.BatchVersion:
		d.reject("version")
	case len(b.Blocks) == 0 || len(b.Blocks) > p.MaxBlocksPerEpoch:
		d.reject("block_count")
	case b.Version >= 1 && b.OriginHash != d.st.epochs[b.Epoch].ref.Hash:
		d.reject("origin_hash")
	default:
		d.st.cands[b.Epoch] = append(d.st.cands[b.Epoch], candidate{incl: ref.Number, batch: b})
		d.m.Inc("rollup_batch_accepted_total")
	}
}

// resolve derives every epoch that can be decided with the L1 data seen so far, in order.
func (d *Derivator) resolve(ref proto.L1BlockRef) {
	for d.st.nextEpoch <= ref.Number {
		e := d.st.nextEpoch
		ei := d.st.epochs[e]
		p := d.cfg.ParamsAt(e)
		var blocks []*proto.Block
		// A candidate that fails the chain-dependent checks can never become valid later (the
		// chain it is checked against only moves when this epoch resolves), so drop it once.
		cands := d.st.cands[e]
		for len(cands) > 0 {
			c := cands[0]
			cands = cands[1:]
			if !d.timesValid(c.batch, ei, p) {
				d.reject("timestamps")
				continue
			}
			blocks = d.execute(c.batch, ei)
			break
		}
		if blocks == nil {
			d.st.cands[e] = cands
		}
		if blocks == nil {
			if ref.Number < e+p.SeqWindow {
				return // a valid batch may still arrive
			}
			blocks = d.fallback(ei)
			d.m.Inc("rollup_fallback_epochs_total")
		}
		for _, b := range blocks {
			d.blocks = append(d.blocks, Derived{b, ref.Number})
			d.ex.SetBlockHash(b.Number(), b.Hash())
			d.st.head = b
		}
		delete(d.st.cands, e)
		delete(d.st.epochs, e)
		d.st.nextEpoch++
	}
}

func (d *Derivator) timesValid(b *proto.EpochBatch, ei epochInfo, p proto.Params) bool {
	prev := d.st.head.Time()
	for _, bb := range b.Blocks {
		if bb.Time <= prev || bb.Time < ei.ref.Time || bb.Time > ei.ref.Time+p.MaxDriftSeconds {
			return false
		}
		prev = bb.Time
	}
	return true
}

func (d *Derivator) execute(b *proto.EpochBatch, ei epochInfo) []*proto.Block {
	p := d.cfg.ParamsAt(b.Epoch)
	parent := d.st.head
	out := make([]*proto.Block, 0, len(b.Blocks))
	for i, bb := range b.Blocks {
		var txs []*gtypes.Transaction
		for _, raw := range bb.Txs {
			if len(raw) > p.MaxTxBytes {
				d.m.Inc("rollup_tx_skipped_total", "reason", "too_large")
				continue
			}
			tx := new(gtypes.Transaction)
			if err := tx.UnmarshalBinary(raw); err != nil {
				d.m.Inc("rollup_tx_skipped_total", "reason", "undecodable")
				continue
			}
			txs = append(txs, tx)
		}
		in := exec.Input{Number: parent.Number() + 1, Time: bb.Time, ParentHash: parent.Hash(), OriginNumber: ei.ref.Number, OriginHash: ei.ref.Hash, Txs: txs}
		if i == 0 {
			in.Deposits = ei.deposits
		}
		res, err := d.ex.Execute(parent.Header.Root, in)
		if err != nil {
			panic(fmt.Sprintf("derivation: internal execution error: %v", err)) // never expected: bad input is skipped, not fatal
		}
		for range res.Skipped {
			d.m.Inc("rollup_tx_skipped_total", "reason", "invalid")
		}
		out = append(out, res.Block)
		parent = res.Block
		d.ex.SetBlockHash(res.Block.Number(), res.Block.Hash())
	}
	return out
}

func (d *Derivator) fallback(ei epochInfo) []*proto.Block {
	parent := d.st.head
	t := max(parent.Time()+1, ei.ref.Time)
	res, err := d.ex.Execute(parent.Header.Root, exec.Input{
		Number: parent.Number() + 1, Time: t, ParentHash: parent.Hash(),
		OriginNumber: ei.ref.Number, OriginHash: ei.ref.Hash, Deposits: ei.deposits,
	})
	if err != nil {
		panic(fmt.Sprintf("derivation: internal execution error: %v", err))
	}
	return []*proto.Block{res.Block}
}

// ---------------------------------------------------------------- read side

// SafeHead returns the latest derived block.
func (d *Derivator) SafeHead() *proto.Block {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.st.head
}

// SafeBlock returns a derived block by number.
func (d *Derivator) SafeBlock(n uint64) *proto.Block {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n < uint64(len(d.blocks)) {
		return d.blocks[n].Block
	}
	return nil
}

// DerivedAt returns the L1 block at which block n was derived.
func (d *Derivator) DerivedAt(n uint64) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n < uint64(len(d.blocks)) {
		return d.blocks[n].L1, true
	}
	return 0, false
}

// Finalized is the newest safe block derived at least FinalityDepth L1 blocks ago.
func (d *Derivator) Finalized() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.finalizedLocked()
}

func (d *Derivator) finalizedLocked() uint64 {
	if d.st.cursor == nil {
		return 0
	}
	depth := d.cfg.ParamsAt(d.st.cursor.Number).FinalityDepth
	for i := len(d.blocks) - 1; i > 0; i-- {
		if d.blocks[i].L1+depth <= d.st.cursor.Number {
			return uint64(i)
		}
	}
	return 0
}

// Status summarizes the derivation.
func (d *Derivator) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := Status{Safe: d.st.head.Number(), Finalized: d.finalizedLocked(), NextEpoch: d.st.nextEpoch, Reorgs: d.reorgs}
	if d.st.cursor != nil {
		s.L1Cursor = d.st.cursor.Number
	}
	return s
}

// FindTx looks a transaction up in the derived chain (newest first).
func (d *Derivator) FindTx(h common.Hash) (*proto.Block, int, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.blocks) - 1; i > 0; i-- {
		for j, tx := range d.blocks[i].Block.Txs {
			if tx.Hash() == h {
				return d.blocks[i].Block, j, true
			}
		}
	}
	return nil, 0, false
}

// BlockByHash finds a derived block by hash.
func (d *Derivator) BlockByHash(h common.Hash) *proto.Block {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.blocks) - 1; i >= 0; i-- {
		if d.blocks[i].Block.Hash() == h {
			return d.blocks[i].Block
		}
	}
	return nil
}
