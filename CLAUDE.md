# CLAUDE.md

Educational rollup in Go (sequencer, batcher, derivation from L1, real EVM via go-ethereum v1.17.10). Takes precedence over the parent folder `CLAUDE.md`.

## Commands

```bash
source /c/Users/srg/tools/env.sh            # Go 1.27.4 (CGO_ENABLED=0), JDK, Maven on this machine
go build -o bin/rollup ./cmd/rollup
gofmt -l . ; go vet ./... ; go test ./...                 # fast, no network
go test -coverpkg=./internal/... -coverprofile=c.out ./internal/... && go tool cover -func=c.out
go test ./internal/proto -run xxx -fuzz=FuzzDecodeBatch -fuzztime=20s
REQUIRE_ANVIL=1 go test ./internal/l1 -v                  # adapter tests against a real Anvil (without REQUIRE_ANVIL they skip when anvil is missing)
(cd contracts && forge test)                              # Solidity (forge-std via: forge install foundry-rs/forge-std --no-git)
bash scripts/demo.sh                                      # full end-to-end against a local Anvil (L1_PORT L2_PORT VER_PORT, defaults 18545/19545/19546; KEEP=1 leaves it running)
bash scripts/observability-up.sh                          # native Prometheus + Grafana (env: PROMETHEUS_BIN GRAFANA_BIN GRAFANA_HOME, ports); stop: scripts/observability-down.sh
go build -o bin/obscheck ./cmd/obscheck && bin/obscheck --prom http://127.0.0.1:9090 --grafana http://127.0.0.1:3000 --auth admin:admin   # verify the stack through its APIs
promtool check rules monitoring/alerts.yml
```

## Architecture (see docs/SPEC.md and docs/ADR.md)

`internal/proto` types + batch codec + fork schedule · `internal/exec` go-ethereum EVM/state executor · `internal/derive` canonical chain from L1 (the verifier) · `internal/sequencer` unsafe chain + tx pool · `internal/batcher` publishes epochs to L1 · `internal/l1` JSON-RPC adapter · `internal/rpcapi` Ethereum-compatible RPC + `rollup_*` · `internal/fakel1` scriptable L1 for tests · `internal/anviltest` throwaway Anvil for integration tests · `internal/sim` end-to-end simulations · `internal/metrics` registry + metric catalog · `cmd/rollup` binary (`devnet`, `verifier`) · `cmd/obscheck` monitoring verifier · `monitoring/` Prometheus, alerts, Grafana.

## Conventions

- Determinism is the contract: nothing in `exec`/`derive` may read the clock, use randomness or iterate maps in an order-dependent way. Bad input is dropped or skipped, never fatal.
- Ports are small interfaces defined by the consumer. The derivation must never receive a type that can write to L1.
- Tests: table-driven, named after the behaviour; protocol rules change only together with docs/SPEC.md and a test.
- Metrics: names are constants in `internal/metrics/catalog.go`; a new metric is added there (with HELP text and initial counters), used by the component, and, if it matters, shown in `monitoring/` (a unit test checks the files against the catalog). Keep label sets bounded (no error strings).
- Slither runs in CI only (no Python here). Silence a finding inline with the reason, never with a global detector exclusion.
- Never claim an audit. No commit or push without the owner's approval; no `Co-Authored-By` lines.
- Do not run `python`/`python3` on this machine (Windows Store stubs hang). Do not kill processes by image name (other tools may be running); kill by PID.
