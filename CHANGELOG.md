# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and is derived from the Conventional Commits history. There are no git tags or releases yet; `0.1.0` is the version string the binary reports.

## [Unreleased]

### Added
- Monitoring: Prometheus config, 8 alert rules, a provisioned Grafana dashboard (17 panels), `scripts/observability-up.sh` and `observability-down.sh` (native, no Docker) and `cmd/obscheck`, which verifies a live stack through the Prometheus and Grafana APIs. Evidence in `docs/OBSERVABILITY.md`.
- Metrics: finalized head, derivation lag in L1 blocks, L1 reorg counter and last depth, batcher oldest in-flight age, block build time histogram, loop error counters, `# HELP` text, and a metric catalog that exports counters at 0. A unit test checks the dashboard and the alert rules against the catalog.
- Integration tests: `internal/l1` against a real Anvil (new `internal/anviltest` helper; skip without `anvil`, fail with `REQUIRE_ANVIL=1`), `internal/rpcapi` over HTTP with go-ethereum's client, `internal/config`.
- CI: `integration (real Anvil)` job and Slither (`crytic/slither-action`, `fail-on: low`) for `contracts/src/DepositPortal.sol`.
- `LICENSE` (MIT), CI and license badges, `docs/OBSERVABILITY.md`, `docs/ROADMAP.md`, this changelog.

### Changed
- `rollup_reorg_total{depth}` is now `rollup_l1_reorg_total` plus `rollup_l1_reorg_last_depth`; the pool rejection label is a bounded reason instead of the error text.
- `eth_getTransactionByHash` returns pending transactions (null block fields).
- `DepositPortal.sol` carries an inline Slither silence for `locked-ether`, with the reason.
- Overall statement coverage of `internal/` is 88 % (was 67 to 97 % for the protocol packages and none for the adapters).

### Fixed
- Batcher fee bumps compounded without limit while L1 did not mine; they are now capped by `MaxGasPrice` (default 200 gwei).
- The config loader ignored unknown keys (a misspelled `forkV2At` would silently leave the fork unscheduled) and accepted `l1Start` 0; both are rejected.

## [0.1.0] - 2026-11-12

### Added
- Protocol types, epoch batch codec (zlib + RLP, strict decoding) and fork schedule (`internal/proto`).
- Executor on go-ethereum's EVM and state with real MPT roots and receipts (`internal/exec`).
- Deterministic derivation from L1 with batch rules, fallback blocks, snapshots for L1 reorgs, forks by L1 block number and finality (`internal/derive`).
- Sequencer with a transaction pool and reconciliation with L1 and with the canonical chain (`internal/sequencer`).
- Batcher with recovery from L1, fee bump and nonce rebase after reorgs (`internal/batcher`).
- L1 JSON-RPC adapter (`internal/l1`) and an Ethereum-compatible RPC with the `rollup_*` namespace (`internal/rpcapi`).
- `rollup` binary with `devnet` and `verifier` modes, status page and `/metrics`.
- Solidity `DepositPortal` and `Counter` with Foundry tests, including fuzz.
- Scriptable in-memory L1 with reorgs (`internal/fakel1`), end-to-end simulations (`internal/sim`), property tests with 60 random reorg scripts, batch decoder fuzzing.
- `scripts/demo.sh` end-to-end demo, README, protocol spec, ADRs, security notes, CI.
