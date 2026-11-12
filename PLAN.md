# Plan: go-mini-rollup

Origin: portfolio project P1 (see `../JOBS.md`). Started as a Java plan; the owner chose Go and go-ethereum as the main client on 2026-11-12, which also made a real EVM trivial (go-ethereum as a library).

| Date | Change |
|---|---|
| 2026-11-12 | Built and verified: protocol types and batch codec, executor on go-ethereum (EVM + MPT), derivation with reorg handling and fork schedule, sequencer with tx pool, batcher with fee bump and reorg rebase, Ethereum-compatible RPC, binary, Solidity `DepositPortal`, simulations, demo. |

## Status

| Area | State |
|---|---|
| Executor (real EVM, deposits, skip-invalid) | done, 4 tests |
| Derivation (rules, fallback, reorgs, forks, finality) | done, 12 tests incl. 60 random reorg scripts, adversarial table, fork divergence |
| Sequencer + tx pool + reconciliation with L1 and with the canonical chain | done, covered by `internal/sim` |
| Batcher (recovery from L1, fee bump, nonce rebase after reorg) | done, covered by `internal/sim` |
| JSON-RPC (cast/forge compatible) + `rollup_syncStatus` + status page + `/metrics` | done, verified by the demo |
| Solidity `DepositPortal` | done, 5 Foundry tests (unit, fuzz); Slither not run (no Python here) |
| End-to-end demo | done, real output in `docs/DEMO.md` |
| Docs (README, SPEC, ADR, SECURITY) | done |

## Deviations from the first plan
- Go instead of Java; go-ethereum as a library, so the state root is a real MPT (the simplified Merkle root idea was dropped) and the EVM phase was not optional.
- The derivator is stateless across restarts (replays L1) instead of using RocksDB; the batcher recovers by scanning L1 instead of a checkpoint file.
- No Docker/Grafana: metrics are exposed in Prometheus format and a built-in status page replaces the dashboard.
- Unit coverage of the adapters (`l1`, `rpcapi`, `config`) is thin; the demo covers them.

## Next (optional)
- Integration test of `internal/l1` and `internal/rpcapi` against Anvil in `go test` (today only the demo does it).
- Checkpoint file for the derivator; forced inclusion of ordinary L2 transactions from L1; blobs instead of calldata.
- Connect it to `rollup-bridge` (withdrawals).

## GitHub metadata (when published)
- About: "Educational rollup in Go: sequencer, batcher and deterministic derivation from L1 on a real EVM (go-ethereum), with reorg handling, a hard fork and a one-command demo."
- Topics: `go`, `rollup`, `layer2`, `ethereum`, `go-ethereum`, `evm`, `sequencer`, `blockchain`, `foundry`.
