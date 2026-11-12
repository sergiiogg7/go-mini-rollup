# Roadmap and design notes

Nothing here is implemented. These are the next steps with the reasoning, so that a reader can judge the trade-offs. Items are ordered by value for the effort.

## 1. Batcher: skip epochs the derivation already resolved

*Observed* in `docs/OBSERVABILITY.md` (failure B): after an outage the batcher posts every missed epoch; the derivation had already resolved them with fallback blocks and rejects each as `stale_epoch` (155 rejections after the first run, 289 after two runs; each one an L1 transaction paid for nothing).

*Design*: give the batcher a small port, `Resolved() uint64` (the derivation's `NextEpoch`), and let it drop any tracked or queued epoch below it, before submitting and when fee-bumping. No protocol change (the derivation already ignores them); the care point is nonces: dropping an unconfirmed batch from tracking while its transaction is still in the L1 pool would leave a nonce gap for the later ones, so those transactions must be replaced (same nonce, next live batch) rather than forgotten; a test in `internal/sim` extends `TestBatcherOutageLongerThanTheWindowDoesNotLoseTransactions` with "and posted zero stale batches". The port must be read-only so the "derivation never writes to L1" invariant (ADR 8) is untouched: the batcher reads the derivation, not the other way round.

## 2. Derivator checkpoint (restart without replaying L1)

*Today* the derivator and the executor keep everything in memory (`rawdb.NewMemoryDatabase`) and replay L1 from `L1Start` on start (ADR 6). Start-up time grows with L1 history, and every restart re-executes every block.

*Why it is not a small change*: go-ethereum's persistent database makes the state survive, but the derivator's own state does not live in it. A restart needs three things persisted consistently:

1. **State trie and code**: a disk-backed `rawdb` (Pebble) and a regular `triedb.Commit(root)` for the root of the checkpointed block. The hash scheme keeps dirty nodes in memory until committed, so the checkpoint must commit before it is written.
2. **Derived blocks**: headers, transactions, receipts, deposits, origin and the L1 block each was derived at (`derive.Derived`). RLP encodes headers, transactions and receipts already; deposits, origin and the L1 block need a small envelope.
3. **The `state` struct**: L1 cursor (number and hash), `nextEpoch`, pending batch candidates and the epoch infos not yet resolved (`cands`, `epochs`).

*Safety rule that keeps it simple*: only checkpoint a point that is already **finalized** (at least `FinalityDepth` L1 blocks behind the cursor). A reorg cannot reach it (deeper reorgs already stop the node with `ErrDeepReorg`), so after restoring the derivator needs a single snapshot, the checkpoint, and then resumes `Step()` from the stored cursor. Restoring must verify that the stored cursor hash is still the canonical L1 block at that height; if not, discard the checkpoint and replay.

*Correctness test*: property "derive from a checkpoint at any finalized point = derive from scratch" over the existing random reorg scripts, comparing the safe head hash after every step. That test is the real work (it needs a restart harness in `internal/sim`); the serialization is mechanical.

*Also lost on restart, regardless*: the sequencer's unsafe chain and the transaction pool. Transactions that were only in unsafe blocks are gone; a persisted pool is a separate feature.

*Decision*: not built. A half-working checkpoint that can silently disagree with a fresh derivation is worse than the documented stateless design, and the lab's L1 histories are short. Estimated effort: 2 to 3 days with the equivalence test.

## 3. Forced inclusion of ordinary L2 transactions from L1

*Gap* (SPEC "Guarantees"): deposits are forced, ordinary L2 transactions are not. A censoring sequencer can ignore a user forever; the user's only escape is to deposit and wait for a sequencer that includes them.

*Design* (OP Stack style deposit-transaction path, simplified):

- **L1**: `DepositPortal.forceTransaction(bytes rawL2Tx) payable` emits `ForcedTx(address indexed from, bytes rawTx)`. The payload is a normal signed, replay-protected L2 transaction, so the L2 sender comes from its signature, not from `msg.sender`, and a forced transaction cannot be forged for someone else.
- **Derivation**: `L1BlockData` gains `Forced []ForcedTx` (read by block hash, like deposits). In the first L2 block of an epoch, after the deposits, the forced transactions of that L1 block run in L1 log order, before the batch's transactions. They follow the existing rule: a transaction that fails to decode, has a bad nonce or lacks funds is skipped deterministically and never invalidates the block (SPEC 3). This keeps the executor path identical for both kinds.
- **Sequencer**: it already mirrors deposits when it opens an epoch (`advanceEpoch` passes `data.Deposits`), so it would mirror forced transactions the same way and stay in sync. A transaction the sequencer also had in its pool is skipped by nonce.
- **Spam and block size**: forced gas per epoch needs a bound, otherwise one L1 transaction can fill every L2 block. Either burn L1 gas proportional to the forced L2 gas limit (the cheapest honest model: the attacker pays L1 prices) or enforce `MaxForcedGasPerEpoch` with a deterministic overflow rule. The first is simpler and has no carry-over queue.
- **Fork**: new behaviour changes block contents, so it is gated by a fork (`V3`, activation by L1 block number, ADR 5). An un-upgraded node would derive different blocks from the same L1, exactly the divergence `TestForkBoundaryAndOldNodeDiverges` demonstrates.
- **Tests**: `fakel1` gains `QueueForced`; a sim where the sequencer drops a user's transaction and the transaction still lands in the safe chain within one epoch; an adversarial table (garbage payload, wrong chain id, nonce gap, oversized payload) that must derive without crashing; reorg of the L1 block that holds a forced transaction (the property test already covers reorgs once `fakel1` can emit forced transactions).

*Decision*: documented, not built. It changes the protocol (a fork, a contract change and a new derivation input) and deserves its own spec section and tests; a minimal version without the gas bound would be a denial-of-service lever.

## 4. Smaller items

- Persist the transaction pool and the batcher's in-flight list (the batcher recovers by scanning L1, the pool does not recover at all).
- Blobs instead of calldata for batches: only the L1 adapter and the batch inclusion check change (ADR 9).
- Alertmanager and a provisioned Grafana alert contact point; today alerts are visible in Prometheus and in the dashboard table only.
- Withdrawals and the connection to `rollup-bridge`.
