#!/usr/bin/env bash
# End-to-end demo of go-mini-rollup on a local Anvil L1. Needs: go, foundry (anvil, cast, forge).
#   scripts/demo.sh            run the demo and stop everything at the end
#   KEEP=1 scripts/demo.sh     leave the L1, the rollup and a verifier running (status page on :9545)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
WORK="$ROOT/.demo"; rm -rf "$WORK"; mkdir -p "$WORK" bin
L1P=${L1_PORT:-18545}; L2P=${L2_PORT:-19545}; VP=${VER_PORT:-19546}
L1=http://127.0.0.1:$L1P; L2=http://127.0.0.1:$L2P; VER=http://127.0.0.1:$VP

# Anvil's public dev accounts (test keys, never use them anywhere real)
DEPLOYER_KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80
BATCHER_KEY=0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d
BATCHER=0x70997970C51812dc3A010C7d01b50e0d17dc79C8
ALICE_KEY=$(cast wallet private-key --mnemonic 'test test test test test test test test test test test junk' --mnemonic-index 2)
ALICE=$(cast wallet address --private-key "$ALICE_KEY")
BOB=$(cast wallet address --mnemonic 'test test test test test test test test test test test junk' --mnemonic-index 3)
INBOX=0x00000000000000000000000000000000000ba7c4
FEE_VAULT=0x000000000000000000000000000000000000fee5

PIDS=()
cleanup() { [ "${KEEP:-0}" = 1 ] && return; for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT

say()  { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
eth()  { cast from-wei "$1"; }
wait_for() { # wait_for "<description>" <seconds> <command...>
  local d="$1" t="$2"; shift 2; for _ in $(seq "$t"); do if "$@" >/dev/null 2>&1; then return 0; fi; sleep 1; done
  echo "timeout waiting for: $d" >&2; exit 1; }
rpc() { cast rpc --rpc-url "$1" "${@:2}"; }
deployed_to() { tr -d '\n ' | grep -o '"deployedTo":"0x[0-9a-fA-F]*"' | grep -o '0x[0-9a-fA-F]*'; }
status() { rpc "$1" rollup_syncStatus | tr -d '\n ' | sed 's/,/, /g'; echo; }
bal_gt0() { [ "$(cast balance "$1" --rpc-url "$2")" != "0" ]; }
bal_eq0() { [ "$(cast balance "$1" --rpc-url "$2")" = "0" ]; }

say "Building the rollup binary"
go build -o bin/rollup ./cmd/rollup && ./bin/rollup version

say "Starting L1 (Anvil, 1 s blocks)"
anvil --port "$L1P" --block-time 1 --silent >"$WORK/anvil.log" 2>&1 & PIDS+=($!)
wait_for "anvil" 20 cast block-number --rpc-url "$L1"

say "Deploying the L1 DepositPortal (Solidity, Foundry)"
PORTAL=$(forge create src/DepositPortal.sol:DepositPortal --root contracts --rpc-url "$L1" \
  --private-key "$DEPLOYER_KEY" --broadcast --json | deployed_to)
echo "DepositPortal: $PORTAL"
L1START=$(( $(cast block-number --rpc-url "$L1") + 1 ))

cat >"$WORK/rollup.json" <<JSON
{"chainId":901,"l1Start":$L1START,"genesisTime":0,"inbox":"$INBOX","batcher":"$BATCHER","portal":"$PORTAL","feeVault":"$FEE_VAULT","alloc":{},"forkV2At":0}
JSON

say "Starting the rollup: sequencer + batcher + derivation (L2 chain id 901)"
BATCHER_KEY=$BATCHER_KEY ./bin/rollup devnet --config "$WORK/rollup.json" --l1 "$L1" --listen 127.0.0.1:$L2P >"$WORK/devnet.log" 2>&1 & PIDS+=($!)
wait_for "L2 rpc" 20 cast chain-id --rpc-url "$L2"
echo "L2 chain id: $(cast chain-id --rpc-url "$L2")   status page: $L2/"

say "1. Alice deposits 5 ETH on L1 -> it is minted on L2 by the derivation (a forced, trustless transaction)"
cast send "$PORTAL" "deposit(address)" "$ALICE" --value 5ether --rpc-url "$L1" --private-key "$DEPLOYER_KEY" >/dev/null
wait_for "deposit on L2" 40 bal_gt0 "$ALICE" "$L2"
echo "Alice L2 balance: $(eth "$(cast balance "$ALICE" --rpc-url "$L2")") ETH"

say "2. Alice sends 1 ETH to Bob on L2 (plain cast send, nothing special)"
cast send "$BOB" --value 1ether --legacy --rpc-url "$L2" --private-key "$ALICE_KEY" | grep -E 'blockNumber|status|gasUsed'
echo "Bob L2 balance:   $(eth "$(cast balance "$BOB" --rpc-url "$L2")") ETH"

say "3. A real EVM: deploy a Solidity contract on L2 and call it"
COUNTER=$(forge create src/Counter.sol:Counter --root contracts --rpc-url "$L2" --private-key "$ALICE_KEY" \
  --legacy --broadcast --json | deployed_to)
cast send "$COUNTER" "increment()" --legacy --rpc-url "$L2" --private-key "$ALICE_KEY" >/dev/null
cast send "$COUNTER" "increment()" --legacy --rpc-url "$L2" --private-key "$ALICE_KEY" >/dev/null
echo "Counter at $COUNTER, number() = $(cast call "$COUNTER" 'number()(uint256)' --rpc-url "$L2")"

say "4. Unsafe -> safe -> finalized: the batcher publishes to L1 and the derivation rebuilds the chain from L1"
HEAD=$(cast block-number --rpc-url "$L2")
safe_ok() { [ "$(cast block-number --rpc-url "$L2" safe 2>/dev/null || echo 0)" -ge "$HEAD" ]; }
wait_for "safe head to reach $HEAD" 60 safe_ok
status "$L2"

say "5. Anyone can verify: start an independent verifier node that only reads L1"
./bin/rollup verifier --config "$WORK/rollup.json" --l1 "$L1" --listen 127.0.0.1:$VP >"$WORK/verifier.log" 2>&1 & PIDS+=($!)
wait_for "verifier rpc" 20 cast chain-id --rpc-url "$VER"
ver_ok() { [ "$(cast block-number --rpc-url "$VER")" -ge "$HEAD" ]; }
wait_for "verifier sync" 60 ver_ok
A=$(cast block "$HEAD" --field hash --rpc-url "$L2"); B=$(cast block "$HEAD" --field hash --rpc-url "$VER")
echo "block $HEAD hash on the sequencer: $A"
echo "block $HEAD hash on the verifier:  $B"
[ "$A" = "$B" ] && echo "IDENTICAL: the verifier rebuilt the same chain from L1 alone." || { echo "MISMATCH"; exit 1; }
echo "Alice on the verifier: $(eth "$(cast balance "$ALICE" --rpc-url "$VER")") ETH"

say "6. L1 reorg: Bob deposits 3 ETH, it appears on L2, then the L1 block is reorged away"
cast send "$PORTAL" "deposit(address)" "$BOB" --value 3ether --rpc-url "$L1" --private-key "$DEPLOYER_KEY" >/dev/null
BOB_BEFORE=$(cast balance "$BOB" --rpc-url "$L2")
dep_seen() { [ "$(cast balance "$BOB" --rpc-url "$L2")" != "$BOB_BEFORE" ]; }
wait_for "deposit visible on L2" 30 dep_seen
echo "Bob on L2 after the deposit: $(eth "$(cast balance "$BOB" --rpc-url "$L2")") ETH"
rpc "$L1" anvil_reorg 4 '[]' >/dev/null
echo "anvil_reorg 4 executed: the L1 blocks holding Bob's deposit no longer exist"
dep_gone() { [ "$(cast balance "$BOB" --rpc-url "$L2")" = "$BOB_BEFORE" ]; }
wait_for "L2 to roll the deposit back" 40 dep_gone
echo "Bob on L2 after the reorg: $(eth "$(cast balance "$BOB" --rpc-url "$L2")") ETH   (the unsafe mint was rolled back, nothing was printed out of thin air)"
sleep 4; status "$L2"
A=$(cast block "$HEAD" --field hash --rpc-url "$L2"); B=$(cast block "$HEAD" --field hash --rpc-url "$VER")
[ "$A" = "$B" ] && echo "After the reorg both nodes still agree on block $HEAD: $A"

say "7. Metrics (Prometheus format)"
curl -s "$L2/metrics" | grep -E 'rollup_(reorg|batch_accepted|batch_submitted|unsafe_reorg|safe_head|rollup_l1)' | head -12

say "Done. $( [ "${KEEP:-0}" = 1 ] && echo "Everything keeps running: open $L2/ in a browser. Stop with: kill ${PIDS[*]}   (Windows: taskkill //PID <pid> //F)" || echo "Stopping everything." )"
