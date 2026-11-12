# go-mini-rollup

An educational **rollup in Go**: a sequencer, a batcher and a deterministic derivation that rebuilds the L2 chain from L1 data alone. It runs on a real Ethereum EVM (`go-ethereum` as a library) and uses a local **Anvil** as L1. It handles L1 reorgs, scheduled protocol upgrades (a "hard fork"), and malicious input, and it comes with a one-command demo.

> Lab project. Single sequencer, no fault or validity proofs, no withdrawals (see the `rollup-bridge` project), not audited, do not use with real value. See [SECURITY.md](SECURITY.md).

## What you can see in 3 minutes

```bash
scripts/demo.sh        # needs: go 1.27+, foundry (anvil, cast, forge)
```

Real recorded output is in [docs/DEMO.md](docs/DEMO.md). The demo:

1. deposits ETH on L1 and watches it appear on L2 (a forced, trustless mint),
2. sends an L2 transfer with plain `cast send`,
3. deploys and calls a Solidity contract on L2 (a real EVM),
4. shows `unsafe → safe → finalized` heads,
5. starts an **independent verifier** that only reads L1 and gets the **identical block hash**,
6. reorganizes L1 so a deposit disappears, and shows the L2 balance rolling back and both nodes converging,
7. prints Prometheus metrics.

`KEEP=1 scripts/demo.sh` leaves everything running; open `http://127.0.0.1:19545/` for a live status page (heads, reorg counters, last blocks).

## How it works

```mermaid
flowchart LR
  U[User / cast / forge] -->|JSON-RPC| S[Sequencer]
  S -->|closed epoch| B[Batcher]
  B -->|batch tx to inbox| L1[(L1: Anvil)]
  P[DepositPortal.sol] -. Deposit event .-> L1
  L1 -->|blocks, batches, deposits| D[Derivation]
  D -->|canonical blocks| S
  D --> V[Verifier node]
  S -. unsafe head .-> U
  D -. safe / finalized .-> U
```

- **Sequencer** (`internal/sequencer`): orders transactions and builds the *unsafe* chain. It is a preview, not the truth.
- **Batcher** (`internal/batcher`): publishes one compressed `EpochBatch` per closed epoch to an L1 inbox address. It survives crashes (it recovers its position by scanning L1), stuck transactions (fee bump) and L1 reorgs (it re-signs on top of the confirmed nonce).
- **Derivation** (`internal/derive`): reads **only L1** and deterministically rebuilds the canonical (*safe*) chain. If the sequencer disagrees, the sequencer converges to the derived chain and re-queues the transactions it lost.
- **Executor** (`internal/exec`): go-ethereum's real EVM, state trie and receipts. Same code path for sequencer and derivation, so both produce the same block hash.
- **Epoch**: one L1 block. Deposits of that L1 block are applied at the start of the epoch's first L2 block (forced inclusion), even if the sequencer disappears.

Rules, parameters and the fork schedule are in [docs/SPEC.md](docs/SPEC.md). Design decisions are in [docs/ADR.md](docs/ADR.md).

## Why a recruiter or tech lead might care

| Topic | Where to look |
|---|---|
| Sequencing, batch submission, derivation, safe/finalized heads | `internal/sequencer`, `internal/batcher`, `internal/derive` |
| Determinism and "invalid input never crashes the node" | adversarial table in `internal/derive/derive_test.go`, `FuzzDecodeBatch` |
| L1 reorgs | `TestRandomReorgScriptsConvergeToFreshDerivation` (60 random scripts), `internal/sim` |
| Protocol upgrade (hard fork) and what happens to a node that did not upgrade | `TestForkBoundaryAndOldNodeDiverges` |
| Real EVM and state | `internal/exec` (go-ethereum `core/vm`, MPT state root) |
| Operation | `/metrics`, `/status` page, `rollup_syncStatus` RPC |
| Solidity | `contracts/` (Foundry: `DepositPortal`, tests, fuzz) |

## Build, test, run

```bash
go build -o bin/rollup ./cmd/rollup
go vet ./... && go test ./...                               # 28 tests (+14 subtests) incl. property tests and end-to-end simulations
go test ./internal/proto -run xxx -fuzz=FuzzDecodeBatch      # native Go fuzzing
(cd contracts && forge install foundry-rs/forge-std --no-git && forge test)

# manual run: see scripts/demo.sh for the exact commands
BATCHER_KEY=0x... bin/rollup devnet   --config rollup.json --l1 http://127.0.0.1:8545 --listen 127.0.0.1:9545
bin/rollup verifier --config rollup.json --l1 http://127.0.0.1:8545 --listen 127.0.0.1:9546
```

Coverage of the protocol packages with all tests (`go test -coverpkg=./internal/...`): `proto` 97 %, `derive` 79 %, `batcher` 76 %, `exec` 73 %, `sequencer` 67 %. The L1 adapter, RPC layer and config loader are exercised by the demo, not by unit tests.

## Limits (on purpose)

Single sequencer; no proofs; no L2→L1 withdrawals; batches go in calldata, not blobs; the derivator replays L1 from the start on every restart instead of keeping a database; the L1 adapter has no TLS or authentication. Each is documented with its trade-off in [SECURITY.md](SECURITY.md) and [docs/ADR.md](docs/ADR.md).
