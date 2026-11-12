# Protocol specification (v0.1)

This is the contract between the sequencer, the batcher and every verifier. Two honest nodes that follow it derive the same chain from the same L1 chain.

## Actors and data

- **L1**: any EVM chain (Anvil in the lab). The derivation reads, per L1 block: its header (`number`, `hash`, `parentHash`, `time`), the transactions sent to the **inbox** address, and the `Deposit` logs of the **portal** contract (queried by block hash).
- **Inbox**: an address with no code. A batch is an L1 transaction to the inbox. It only counts if its recovered sender is the configured **batcher** address.
- **Portal** (`contracts/src/DepositPortal.sol`): `Deposit(address indexed from, address indexed to, uint256 value, uint256 depositId)`.
- **Epoch**: one L1 block `E`. L2 blocks of epoch `E` have `l1Origin = E`. The first epoch is `L1Start`.

## Parameters (selected per epoch by the fork schedule)

| Name | V1 | V2 | Meaning |
|---|---|---|---|
| `MaxBlocksPerEpoch` | 16 | 32 | Max L2 blocks in one epoch batch |
| `SeqWindow` | 8 | 8 | L1 blocks after the epoch during which a batch is still valid |
| `MaxDriftSeconds` | 600 | 1800 | L2 timestamp may exceed the epoch's L1 timestamp by at most this |
| `OriginConfirmations` | 1 | 1 | The sequencer only opens an epoch for L1 blocks this deep |
| `FinalityDepth` | 12 | 12 | L1 depth after which derived blocks are *finalized* |
| `MaxCompressedBytes` / `MaxDecompressedBytes` | 120 000 / 1 MiB | same | Batch size limits (decompression bomb guard) |
| `MaxTxBytes` | 128 KiB | same | Max size of one L2 transaction |
| `BatchVersion` | 0 | 1 | V1 batches carry no origin hash, V2 batches commit to it |
| `GasLimit` | 30 000 000 | same | L2 block gas limit |

`ParamsAt(epoch)` returns the parameters of the latest fork whose activation is `<= epoch`. Forks are activated by **L1 block number of the epoch**, never by local time.

## Batch format

`batch = version (1 byte) || zlib( rlp( [epoch, (originHash if version=1), [ [time, [rawTx...]], ... ]] ) )`

The batch contains no hashes: the derivation recomputes everything. Decoding is strict (canonical RLP, no trailing bytes, size limits); any failure drops the whole batch.

## Derivation rules

For every L1 block `B` in order (the derivation never looks at the future):

1. For each inbox transaction in `B` from the batcher: decode; the batch is **rejected** unless
   - it decodes within the limits (`decode`),
   - `epoch <= B` (`future_epoch`) and `epoch >= nextEpoch`, i.e. not already resolved (`stale_epoch`),
   - `B <= epoch + SeqWindow` (`late`),
   - `version == BatchVersion(epoch)` (`version`),
   - `1 <= blocks <= MaxBlocksPerEpoch(epoch)` (`block_count`),
   - for version 1, `originHash == hash(L1 block epoch)` (`origin_hash`).
   Accepted batches are appended to the candidates of their epoch, in L1 order.
2. Resolve epochs **in order** (`nextEpoch`, `nextEpoch+1`, ...) as long as `nextEpoch <= B`:
   - take the first candidate whose timestamps are valid: strictly increasing, `>= epochTime`, `<= epochTime + MaxDrift`, and greater than the previous L2 block's timestamp (`timestamps`). Invalid candidates are dropped once.
   - If there is none and `B >= epoch + SeqWindow`, derive **one empty block** with timestamp `max(previous+1, epochTime)` (fallback).
   - If there is none and the window is still open, wait.
3. Executing a block: apply the epoch's deposits to their recipients (first block of the epoch only), then run the transactions in order on the EVM. A transaction that is undecodable, too large, a blob transaction, unprotected (no chain id), has a bad signature/nonce/funds or exceeds the gas pool is **skipped deterministically** and never invalidates the block.
4. **Reorgs**: the derivation keeps a snapshot per processed L1 block. If a new block's `parentHash` is not the last processed hash (or the tip changed), it rolls back to the newest snapshot that is still canonical and reprocesses. Deeper than the retained snapshots (`FinalityDepth + SeqWindow + 32`) it stops with `ErrDeepReorg` instead of guessing.

## Block

An Ethereum header (go-ethereum `types.Header`) with real MPT state root, transactions root, receipts root and bloom. `Extra = rlp([originNumber, originHash, depositsHash])`, so the hash commits to the L1 origin and to the deposits. Base fee is 0; the fee `gasPrice * gasUsed` goes to the fee vault (`Coinbase`).

## Heads

- **unsafe**: the sequencer's latest block.
- **safe**: the latest block derived from L1.
- **finalized**: the newest safe block derived at an L1 block at least `FinalityDepth` behind the L1 cursor.

When the derivation disagrees with an unsafe block (or an unsafe block's L1 origin left the canonical chain), the sequencer truncates its chain, re-queues the transactions of the dropped blocks and continues from the canonical chain.

## Guarantees (and what is not guaranteed)

- Determinism: a fresh node deriving the final L1 chain reaches the same safe head as a node that followed it live through reorgs (property test, 60 random scripts).
- Liveness against a silent or censoring sequencer: after `SeqWindow` L1 blocks the epoch is derived as an empty block and **deposits are still applied**.
- Not guaranteed: censorship resistance for ordinary L2 transactions (no forced-inclusion path from L1 other than deposits), validity of the sequencer's blocks (no proofs: validity is whatever the rules above derive), withdrawals.
