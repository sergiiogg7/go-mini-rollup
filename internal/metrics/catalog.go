package metrics

import "sort"

// Metric names. Every component reports through these constants so that the dashboard, the alert
// rules and the code cannot drift apart silently (monitoring_test.go checks the files against
// this list).
const (
	SafeHead         = "rollup_safe_head"
	FinalizedHead    = "rollup_finalized_head"
	UnsafeHead       = "rollup_unsafe_head"
	L1Head           = "rollup_l1_head"
	L1Cursor         = "rollup_l1_cursor"
	DerivationLag    = "rollup_derivation_lag_l1_blocks"
	NextEpoch        = "rollup_next_epoch"
	L1Reorgs         = "rollup_l1_reorg_total"
	L1ReorgDepth     = "rollup_l1_reorg_last_depth"
	UnsafeReorgs     = "rollup_unsafe_reorg_total"
	BatchAccepted    = "rollup_batch_accepted_total"
	BatchRejected    = "rollup_batch_rejected_total"
	FallbackEpochs   = "rollup_fallback_epochs_total"
	TxSkipped        = "rollup_tx_skipped_total"
	TxRejected       = "rollup_tx_rejected_total"
	TxPoolSize       = "rollup_txpool_size"
	BlocksBuilt      = "rollup_blocks_built_total"
	BlockBuild       = "rollup_block_build_seconds"
	BatchSubmitted   = "rollup_batch_submitted_total"
	BatcherInflight  = "rollup_batcher_inflight"
	BatcherOldestAge = "rollup_batcher_oldest_inflight_seconds"
	BatcherNextEpoch = "rollup_batcher_next_epoch"
	BatcherResubmit  = "rollup_batcher_resubmitted_total"
	BatcherErrors    = "rollup_batcher_errors_total"
	LoopErrors       = "rollup_loop_errors_total"
)

// RejectReasons are the rules by which the derivation drops a batch (docs/SPEC.md, rule 1).
var RejectReasons = []string{"sender", "decode", "future_epoch", "stale_epoch", "late", "version", "block_count", "origin_hash", "timestamps"}

// TxRejectReasons are the reasons the sequencer pool refuses a transaction. Anything else is
// reported as "invalid" so that the label set stays bounded.
var TxRejectReasons = []string{"known", "nonce_too_low", "nonce_gap", "insufficient_funds", "pool_full", "underpriced", "invalid"}

var descriptions = map[string]string{
	SafeHead:         "Latest L2 block derived from L1 (the canonical chain).",
	FinalizedHead:    "Latest L2 block derived at an L1 block at least FinalityDepth behind the L1 cursor.",
	UnsafeHead:       "Latest L2 block built by the sequencer (not yet confirmed by L1).",
	L1Head:           "Latest L1 block number reported by the L1 node.",
	L1Cursor:         "Last L1 block processed by the derivation.",
	DerivationLag:    "L1 blocks the derivation is behind the L1 head.",
	NextEpoch:        "Next epoch (L1 block) the derivation has to resolve.",
	L1Reorgs:         "L1 reorgs handled by the derivation.",
	L1ReorgDepth:     "Depth in L1 blocks of the last L1 reorg handled.",
	UnsafeReorgs:     "Times the sequencer dropped unsafe blocks, by cause.",
	BatchAccepted:    "Batches that passed the derivation rules.",
	BatchRejected:    "Batches dropped by the derivation, by rule.",
	FallbackEpochs:   "Epochs derived as an empty block because no valid batch arrived within SeqWindow.",
	TxSkipped:        "Transactions skipped deterministically inside a block, by reason.",
	TxRejected:       "Transactions refused by the sequencer pool, by reason.",
	TxPoolSize:       "Transactions waiting in the sequencer pool.",
	BlocksBuilt:      "Unsafe L2 blocks built by the sequencer, by kind.",
	BlockBuild:       "Seconds the sequencer spends executing one block on the EVM.",
	BatchSubmitted:   "Batch transactions sent to L1 by the batcher.",
	BatcherInflight:  "Batches sent to L1 and not yet confirmed.",
	BatcherOldestAge: "Seconds since the oldest unconfirmed batch was first sent (0 if none).",
	BatcherNextEpoch: "Next epoch the batcher will publish.",
	BatcherResubmit:  "Batches sent again, by cause.",
	BatcherErrors:    "Batcher ticks that failed, by stage.",
	LoopErrors:       "Failed iterations of the background loops (usually the L1 node being unreachable), by loop.",
}

// Names lists every metric the rollup exports, sorted.
func Names() []string {
	names := make([]string, 0, len(descriptions))
	for n := range descriptions {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RegisterAll documents every metric and exports the counters at 0, so that rate() and
// increase() see the first event and a dashboard shows 0 instead of "no data".
func RegisterAll(r *Registry) {
	for n, h := range descriptions {
		r.Describe(n, h)
	}
	r.Set(L1ReorgDepth, 0)
	for _, c := range []string{L1Reorgs, BatchAccepted, FallbackEpochs, BatchSubmitted} {
		r.Init(c)
	}
	for _, v := range RejectReasons {
		r.Init(BatchRejected, "reason", v)
	}
	for _, v := range TxRejectReasons {
		r.Init(TxRejected, "reason", v)
	}
	for _, v := range []string{"l1_reorg", "derivation_mismatch"} {
		r.Init(UnsafeReorgs, "cause", v)
	}
	for _, v := range []string{"fee_bump", "l1_reorg", "nonce_rebase"} {
		r.Init(BatcherResubmit, "cause", v)
	}
	for _, v := range []string{"track", "submit"} {
		r.Init(BatcherErrors, "stage", v)
	}
	for _, v := range []string{"derivation", "sequencer", "batcher"} {
		r.Init(LoopErrors, "loop", v)
	}
	for _, v := range []string{"epoch", "tx"} {
		r.Init(BlocksBuilt, "kind", v)
	}
	for _, v := range []string{"too_large", "undecodable", "invalid", "sequencer"} {
		r.Init(TxSkipped, "reason", v)
	}
}
