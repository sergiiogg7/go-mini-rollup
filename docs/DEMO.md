# Demo output

Real output of `scripts/demo.sh` (recorded 2026-11-12, Windows 10 + Git Bash, Go 1.27.4, Foundry 1.7.1, go-ethereum v1.17.10 as a library, Anvil as L1 with 1 s blocks).
Every step is executed for real; nothing is mocked. Addresses and hashes change between runs. Step 7 was re-recorded from a later run (same day) after the metrics were extended; see [OBSERVABILITY.md](OBSERVABILITY.md) for the Prometheus and Grafana side.

What each step proves:

1. **Deposit**: an L1 `Deposit` event becomes a forced mint on L2, derived from L1 data alone.
2. **Transfer**: a plain `cast send --legacy` against the L2 RPC works; the sequencer orders it.
3. **Real EVM**: a Solidity contract is deployed and called on L2 (the executor is go-ethereum's `core/vm`).
4. **Unsafe, safe, finalized**: the batcher publishes the epochs to L1; the derivation rebuilds the same blocks.
5. **Independent verifier**: a second node that only reads L1 reproduces the exact same block hash.
6. **L1 reorg**: a deposit is minted, then its L1 block is reorganized away; the L2 balance rolls back and both nodes converge again.
7. **Metrics**: Prometheus text format at `/metrics`.

```text

==> Building the rollup binary
go-mini-rollup/0.1.0

==> Starting L1 (Anvil, 1 s blocks)

==> Deploying the L1 DepositPortal (Solidity, Foundry)
DepositPortal: 0x5FbDB2315678afecb367f032d93F642f64180aa3

==> Starting the rollup: sequencer + batcher + derivation (L2 chain id 901)
L2 chain id: 901   status page: http://127.0.0.1:19545/

==> 1. Alice deposits 5 ETH on L1 -> it is minted on L2 by the derivation (a forced, trustless transaction)
Alice L2 balance: 5.000000000000000000 ETH

==> 2. Alice sends 1 ETH to Bob on L2 (plain cast send, nothing special)
blockNumber          3
gasUsed              21000
status               1 (success)
Bob L2 balance:   1.000000000000000000 ETH

==> 3. A real EVM: deploy a Solidity contract on L2 and call it
Counter at 0x2E983A1Ba5e8b38AAAeC4B440B9dDcFBf72E15d1, number() = 2

==> 4. Unsafe -> safe -> finalized: the batcher publishes to L1 and the derivation rebuilds the chain from L1
{"batcherNextEpoch":8, "batchesAccepted":4, "batchesSubmitted":5, "currentEpoch":8, "derivationLagL1":0, "fallbackEpochs":0, "finalized":0, "l1Cursor":9, "l1Head":9, "l1Reorgs":0, "nextEpoch":7, "role":"sequencer", "safe":8, "txpool":0, "unsafe":10, "unsafeReorgs":0}

==> 5. Anyone can verify: start an independent verifier node that only reads L1
block 8 hash on the sequencer: 0x80981391b1b0216a22baf1fc7cc8c3636b1d4970f0d8d98371ba5d6ea8eb3202
block 8 hash on the verifier:  0x80981391b1b0216a22baf1fc7cc8c3636b1d4970f0d8d98371ba5d6ea8eb3202
IDENTICAL: the verifier rebuilt the same chain from L1 alone.
Alice on the verifier: 3.999807149000000000 ETH

==> 6. L1 reorg: Bob deposits 3 ETH, it appears on L2, then the L1 block is reorged away
Bob on L2 after the deposit: 4.000000000000000000 ETH
anvil_reorg 4 executed: the L1 blocks holding Bob's deposit no longer exist
Bob on L2 after the reorg: 1.000000000000000000 ETH   (the unsafe mint was rolled back, nothing was printed out of thin air)
{"batcherNextEpoch":16, "batchesAccepted":14, "batchesSubmitted":13, "currentEpoch":16, "derivationLagL1":0, "fallbackEpochs":0, "finalized":0, "l1Cursor":17, "l1Head":17, "l1Reorgs":1, "nextEpoch":14, "role":"sequencer", "safe":15, "txpool":0, "unsafe":18, "unsafeReorgs":1}
After the reorg both nodes still agree on block 8: 0x80981391b1b0216a22baf1fc7cc8c3636b1d4970f0d8d98371ba5d6ea8eb3202

==> 7. Metrics (Prometheus format)
rollup_batch_accepted_total 16
rollup_batch_submitted_total 13
rollup_derivation_lag_l1_blocks 0
rollup_fallback_epochs_total 0
rollup_finalized_head 0
rollup_l1_reorg_last_depth 4
rollup_l1_reorg_total 1
rollup_safe_head 16
rollup_unsafe_reorg_total{cause="derivation_mismatch"} 1
rollup_unsafe_reorg_total{cause="l1_reorg"} 1

==> Done. Stopping everything.
```
