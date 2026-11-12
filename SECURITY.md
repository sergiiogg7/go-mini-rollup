# Security

**This project has had no external audit and must not hold real value.** It is a learning and portfolio lab. All keys in the repo and in the demo are Anvil's public test keys.

## Threat model in one page

| Actor | Can | Cannot |
|---|---|---|
| Sequencer | Order and censor ordinary L2 transactions, delay them, build blocks that a verifier will reject | Change the canonical chain: verifiers derive it from L1 only. Mint funds: L2 money comes from L1 deposits or from existing balances |
| Batcher key holder | Decide what is published for each epoch (it is the only authenticated sender) | Break determinism; publish invalid data without it being dropped; stop deposits (they are derived even without a batch) |
| Anyone on L1 | Deposit; post junk to the inbox (ignored: wrong sender) | Affect the derivation |
| L1 reorgs | Remove batches and deposits | Corrupt a verifier: it rolls back to the common ancestor and re-derives |
| Malicious RPC client | Send malformed transactions, huge requests | Crash the node (decoders are strict and bounded; see tests) |

## What the tests check (adversarial)

Named tests in `internal/derive/derive_test.go`, `internal/proto/proto_test.go`, `internal/sim/sim_test.go`: non-batcher sender, garbage bytes, decompression bomb, future epoch, wrong version, zero or too many blocks, timestamp before the epoch / beyond the drift / not increasing, duplicate batches, invalid transactions inside a valid batch, wrong origin hash after the fork, L1 reorgs of random shape, derivation overriding the sequencer, batcher outage longer than the window, stuck batcher transaction, restart with no state, fork boundary with an un-upgraded node. `FuzzDecodeBatch` fuzzes the batch decoder. The configuration loader rejects unknown keys, so a misspelled `forkV2At` cannot silently leave the hard fork unscheduled on one node. Operational failures (L1 stalled, batcher unfunded, L1 reorgs) were provoked on a live stack and their alerts verified; see `docs/OBSERVABILITY.md`.

## Known limitations (accepted for a lab)

- **Single sequencer, no decentralization, no proofs.** Validity is defined by the rules in `docs/SPEC.md`, not proven.
- **Censorship**: an ordinary L2 transaction can be censored by the sequencer. There is no forced-inclusion path from L1 except deposits.
- **No withdrawals**: ETH deposited into `DepositPortal` cannot leave (see the `rollup-bridge` project). The contract is intentionally minimal.
- **Batcher key** is read from an environment variable; no HSM, no rotation procedure.
- **RPC** has no authentication, TLS or rate limiting; `rpc` request size limits are go-ethereum's defaults. Do not expose it.
- **L1 adapter** trusts the configured L1 node; a malicious L1 RPC could feed false data.
- **Memory**: state trie nodes are never pruned; the derivator replays L1 on every restart.
- **Deep reorgs**: deeper than `FinalityDepth + SeqWindow + 32` L1 blocks the derivation stops (`ErrDeepReorg`) and needs a resync.
- Static analysis: Slither runs in CI only (`crytic/slither-action`, `fail-on: low`, `--exclude-dependencies`, config `contracts/slither.config.json`); there is no Python on the development machine, so it has not been run locally. It reports no findings for `DepositPortal`. One detector is silenced inline with the reason next to the contract: `locked-ether`, because the portal has no withdrawals by design (see above). The contract is 40 lines, fuzz-tested, and uses custom errors and a checks-effects pattern with no external calls.
- Test coverage: 88 % of the statements in `internal/` (per package in the README). The L1 adapter is tested against a real Anvil in CI, the RPC layer over HTTP; `cmd/rollup` is covered only by the demo.
- **Batcher fees**: a stuck batch is replaced with a 25 % higher fee until `MaxGasPrice` (200 gwei by default); beyond that the `BatcherStuck` alert is the only reaction. After a long outage the batcher still publishes batches for epochs the derivation already resolved (wasted L1 gas, rejected as `stale_epoch`).

## Reporting

This is a personal portfolio project; open an issue in the repository.
