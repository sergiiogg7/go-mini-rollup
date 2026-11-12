# Architecture decisions

Short records of the non-obvious decisions. Format: context, decision, consequence.

## 1. go-ethereum as a library for execution
*Context*: a rollup needs an EVM and Ethereum state. *Decision*: import `core/vm`, `core/state` and `core/types` from go-ethereum v1.17.10 instead of writing a toy VM or a custom state tree. *Consequence*: a real EVM, a real MPT state root and receipts for free; the lab's value is the rollup logic around it. The price is a heavy dependency and API churn between go-ethereum versions (pin it).

## 2. State is addressed by root, so snapshots are free
*Decision*: the executor keeps one trie database; every block records its state root. Rolling back after an L1 reorg means going back to an older root, with no copying. *Consequence*: derivation snapshots store only small maps and a root. Memory grows (nodes are never pruned); acceptable for a lab, a real node would prune.

## 3. Epoch = one L1 block; batches are per epoch; first valid batch wins
*Decision*: each L1 block is an epoch, the sequencer publishes one batch per closed epoch, and the first valid batch for an epoch (in L1 order) wins. *Consequence*: duplicates and replays are harmless, and a derivation made at any moment gives the same result as one made later. The cost is one L1 transaction per epoch, even if empty; real systems batch across epochs.

## 4. Fallback block instead of waiting forever
*Decision*: once L1 reaches `epoch + SeqWindow` without a valid batch, derive one empty block. *Consequence*: the chain stays live and deposits are forced even if the sequencer or batcher is down. The sequencer's transactions of that period are not lost: it re-queues them when it sees the canonical chain (`TestBatcherOutageLongerThanTheWindowDoesNotLoseTransactions`).

## 5. Forks are activated by the epoch's L1 block number
*Decision*: `ParamsAt(epoch)` selects both the batch format and the limits. *Consequence*: the batcher and every verifier agree without clocks. A node that did not upgrade rejects V2 batches and forks away: `TestForkBoundaryAndOldNodeDiverges` shows it.

## 6. The derivator is stateless across restarts
*Decision*: on restart it replays L1 from `L1Start` instead of persisting its own database. *Consequence*: nothing to corrupt and a trivial recovery story ("a rollup is a function of L1"), at the cost of start-up time proportional to the L1 history. A checkpoint file would fix that and is future work. The batcher recovers the same way (it scans L1 for its own batches).

## 7. Pull loops, no event callbacks
*Decision*: every component polls with a cursor and idempotent steps (`Step`, `Tick`). *Consequence*: easy to reason about and to test deterministically (the simulation drives rounds by hand); a little more latency than push.

## 8. Ports are small and defined by the consumer
*Decision*: `derive.L1Source` (read only), `batcher.L1Submitter` (write), `batcher.UnsafeSource`, `sequencer.SafeChain`. The derivation never receives a type that can publish to L1, so "derivation only reads L1" is enforced by the compiler. *Consequence*: the same fake L1 drives unit tests and the end-to-end simulations.

## 9. Calldata, not blobs
*Decision*: batches go in transaction calldata. *Consequence*: simple and works on Anvil; real rollups moved to blobs for cost. The batch format and the derivation do not depend on where the bytes live, so switching is an L1 adapter change.

## 10. No fees market, no proofs, single sequencer
Out of scope for a lab; documented in SECURITY.md. The sequencer cannot lie about the state (verifiers rebuild it) but nothing proves an invalid state transition is impossible: validity is defined by the rules, not proven.
