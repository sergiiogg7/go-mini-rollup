package rpcapi

import (
	"errors"

	"github.com/ethereum/go-ethereum/common"
	gtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/sergiiogg7/go-mini-rollup/internal/batcher"
	"github.com/sergiiogg7/go-mini-rollup/internal/derive"
	"github.com/sergiiogg7/go-mini-rollup/internal/metrics"
	"github.com/sergiiogg7/go-mini-rollup/internal/proto"
	"github.com/sergiiogg7/go-mini-rollup/internal/sequencer"
)

// SeqChain serves the sequencer node: "latest" is the unsafe head, "safe" and "finalized" come
// from the derivation.
type SeqChain struct {
	Seq *sequencer.Sequencer
	Der *derive.Derivator
	L1  derive.L1Source
	Bat *batcher.Batcher
	Reg *metrics.Registry
}

func (c *SeqChain) Resolve(tag rpc.BlockNumber) *proto.Block {
	switch tag {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		return c.Seq.Head()
	case rpc.SafeBlockNumber:
		return c.Der.SafeHead()
	case rpc.FinalizedBlockNumber:
		return c.Der.SafeBlock(c.Der.Finalized())
	case rpc.EarliestBlockNumber:
		return c.Seq.Block(0)
	}
	if tag < 0 {
		return nil
	}
	return c.Seq.Block(uint64(tag))
}

func (c *SeqChain) ByHash(h common.Hash) *proto.Block { return c.Seq.BlockByHash(h) }

func (c *SeqChain) Tx(h common.Hash) (*gtypes.Transaction, *proto.Block, int, bool) {
	l, ok := c.Seq.Tx(h)
	if !ok {
		return nil, nil, 0, false
	}
	return l.Block.Txs[l.Index], l.Block, l.Index, true
}

func (c *SeqChain) Pending(h common.Hash) bool                     { return c.Seq.Pending(h) }
func (c *SeqChain) PendingNonce(a common.Address, n uint64) uint64 { return c.Seq.PoolNonce(a, n) }
func (c *SeqChain) Submit(raw []byte) (common.Hash, error)         { return c.Seq.Submit(raw) }

func (c *SeqChain) Status() map[string]any {
	st := c.Der.Status()
	l1, _ := c.L1.Head()
	m := map[string]any{
		"role": "sequencer", "unsafe": c.Seq.Head().Number(), "safe": st.Safe, "finalized": st.Finalized,
		"l1Head": l1, "l1Cursor": st.L1Cursor, "nextEpoch": st.NextEpoch, "currentEpoch": c.Seq.CurrentEpoch(),
		"l1Reorgs": st.Reorgs, "unsafeReorgs": c.Seq.UnsafeReorgs(), "txpool": c.Seq.PoolSize(),
		"derivationLagL1": int64(l1) - int64(st.L1Cursor),
	}
	if c.Bat != nil {
		m["batcherNextEpoch"] = c.Bat.NextEpoch()
	}
	if c.Reg != nil {
		m["batchesSubmitted"] = c.Reg.Get("rollup_batch_submitted_total")
		m["batchesAccepted"] = c.Reg.Get("rollup_batch_accepted_total")
		m["fallbackEpochs"] = c.Reg.Get("rollup_fallback_epochs_total")
	}
	return m
}

// VerifierChain serves a verifier node: it only knows what it derived from L1.
type VerifierChain struct {
	Der *derive.Derivator
	L1  derive.L1Source
}

func (c *VerifierChain) Resolve(tag rpc.BlockNumber) *proto.Block {
	switch tag {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber, rpc.SafeBlockNumber:
		return c.Der.SafeHead()
	case rpc.FinalizedBlockNumber:
		return c.Der.SafeBlock(c.Der.Finalized())
	case rpc.EarliestBlockNumber:
		return c.Der.SafeBlock(0)
	}
	if tag < 0 {
		return nil
	}
	return c.Der.SafeBlock(uint64(tag))
}

func (c *VerifierChain) ByHash(h common.Hash) *proto.Block { return c.Der.BlockByHash(h) }

func (c *VerifierChain) Tx(h common.Hash) (*gtypes.Transaction, *proto.Block, int, bool) {
	b, i, ok := c.Der.FindTx(h)
	if !ok {
		return nil, nil, 0, false
	}
	return b.Txs[i], b, i, true
}

func (c *VerifierChain) Pending(common.Hash) bool                       { return false }
func (c *VerifierChain) PendingNonce(_ common.Address, n uint64) uint64 { return n }
func (c *VerifierChain) Submit([]byte) (common.Hash, error) {
	return common.Hash{}, errors.New("read-only verifier node: send transactions to the sequencer")
}

func (c *VerifierChain) Status() map[string]any {
	st := c.Der.Status()
	l1, _ := c.L1.Head()
	return map[string]any{
		"role": "verifier", "unsafe": st.Safe, "safe": st.Safe, "finalized": st.Finalized, "l1Head": l1,
		"l1Cursor": st.L1Cursor, "nextEpoch": st.NextEpoch, "l1Reorgs": st.Reorgs, "derivationLagL1": int64(l1) - int64(st.L1Cursor),
	}
}
