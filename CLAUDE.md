# CLAUDE.md

Educational rollup in Go (sequencer, batcher, derivation from L1, real EVM via go-ethereum v1.17.10). Takes precedence over the parent folder `CLAUDE.md`.

## Commands

```bash
source /c/Users/srg/tools/env.sh            # Go 1.27.4 (CGO_ENABLED=0), JDK, Maven on this machine
go build -o bin/rollup ./cmd/rollup
gofmt -l . ; go vet ./... ; go test ./...                 # fast, no network
go test -coverpkg=./internal/... -coverprofile=c.out ./internal/... && go tool cover -func=c.out
go test ./internal/proto -run xxx -fuzz=FuzzDecodeBatch -fuzztime=20s
(cd contracts && forge test)                              # Solidity (forge-std via: forge install foundry-rs/forge-std --no-git)
bash scripts/demo.sh                                      # full end-to-end against a local Anvil (ports 18545/19545/19546)
```

## Architecture (see docs/SPEC.md and docs/ADR.md)

`internal/proto` types + batch codec + fork schedule · `internal/exec` go-ethereum EVM/state executor · `internal/derive` canonical chain from L1 (the verifier) · `internal/sequencer` unsafe chain + tx pool · `internal/batcher` publishes epochs to L1 · `internal/l1` JSON-RPC adapter · `internal/rpcapi` Ethereum-compatible RPC + `rollup_*` · `internal/fakel1` scriptable L1 for tests · `internal/sim` end-to-end simulations · `cmd/rollup` binary (`devnet`, `verifier`).

## Conventions

- Determinism is the contract: nothing in `exec`/`derive` may read the clock, use randomness or iterate maps in an order-dependent way. Bad input is dropped or skipped, never fatal.
- Ports are small interfaces defined by the consumer. The derivation must never receive a type that can write to L1.
- Tests: table-driven, named after the behaviour; protocol rules change only together with docs/SPEC.md and a test.
- Never claim an audit. No commit or push without the owner's approval; no `Co-Authored-By` lines.
- Do not run `python`/`python3` on this machine (Windows Store stubs hang). Do not kill processes by image name (other tools may be running); kill by PID.
