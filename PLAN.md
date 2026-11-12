# Plan: go-mini-rollup

Origin: portfolio project P1 (see `../JOBS.md`). Started as a Java plan; the owner chose Go and go-ethereum as the main client on 2026-11-12, which also made a real EVM trivial (go-ethereum as a library).

| Date | Change |
|---|---|
| 2026-11-12 | Built and verified: protocol types and batch codec, executor on go-ethereum (EVM + MPT), derivation with reorg handling and fork schedule, sequencer with tx pool, batcher with fee bump and reorg rebase, Ethereum-compatible RPC, binary, Solidity `DepositPortal`, simulations, demo. Published with CI. |
| 2026-11-12 | Monitoring: metric catalog, histograms, derivation lag and finalized head, Prometheus rules, Grafana dashboard, native scripts, `obscheck`; verified on a live stack with three controlled failures. Found and fixed unbounded fee bumps. |
| 2026-11-12 | Adapter tests: `internal/l1` against a real Anvil, `internal/rpcapi` over HTTP, `internal/config`; pending transactions are now returned by hash; the config loader rejects unknown keys. Overall statement coverage of `internal/` 88 %. |
| 2026-11-12 | Slither in CI for `DepositPortal`, MIT license, changelog, GitHub About and topics. |

## Status

| Area | State |
|---|---|
| Executor (real EVM, deposits, skip-invalid) | done, 4 tests |
| Derivation (rules, fallback, reorgs, forks, finality, exported heads and lag) | done, 14 tests incl. 60 random reorg scripts, adversarial table, fork divergence |
| Sequencer + tx pool + reconciliation with L1 and with the canonical chain | done, covered by `internal/sim` and the RPC tests |
| Batcher (recovery from L1, fee bump with cap, nonce rebase after reorg) | done, covered by `internal/sim` |
| JSON-RPC (cast/forge compatible) + `rollup_syncStatus` + status page + `/metrics` | done, tested over HTTP (`internal/rpcapi`) and by the demo |
| L1 adapter | done, 7 integration tests against a real Anvil (CI job with `REQUIRE_ANVIL=1`) |
| Monitoring (Prometheus, 8 alerts, Grafana dashboard) | done, verified through the APIs, see `docs/OBSERVABILITY.md`; not in CI |
| Solidity `DepositPortal` | done, 5 Foundry tests (unit, fuzz), Slither in CI (0 findings, `locked-ether` silenced inline with the reason) |
| End-to-end demo | done, real output in `docs/DEMO.md`, runs in CI |
| Docs (README, SPEC, ADR, SECURITY, OBSERVABILITY, ROADMAP) | done |

## Deviations from the first plan
- Go instead of Java; go-ethereum as a library, so the state root is a real MPT (the simplified Merkle root idea was dropped) and the EVM phase was not optional.
- The derivator is stateless across restarts (replays L1) instead of using RocksDB; the batcher recovers by scanning L1 instead of a checkpoint file. The analysis of a checkpoint is in `docs/ROADMAP.md`.
- No Docker: Prometheus and Grafana run natively via `scripts/observability-up.sh`; the built-in status page stays as a zero-dependency view.
- The Prometheus and Grafana stack is verified by hand and by `obscheck`, not in CI.

## Next (optional)
See `docs/ROADMAP.md`: derivator checkpoint, forced inclusion of ordinary L2 transactions, batcher skipping epochs already resolved, blobs instead of calldata, connection to `rollup-bridge` (withdrawals).

## GitHub metadata
- About: "Educational rollup in Go: sequencer, batcher and deterministic derivation from L1 on a real EVM (go-ethereum), with reorg handling, a hard fork and a one-command demo."
- Topics: `go`, `rollup`, `layer2`, `ethereum`, `go-ethereum`, `evm`, `sequencer`, `blockchain`, `foundry`, `solidity`.
