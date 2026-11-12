# Observability

The node exports Prometheus metrics on `/metrics` (a small handwritten registry in `internal/metrics`, no client library). `monitoring/` holds a Prometheus config, alert rules and a Grafana dashboard, and `scripts/observability-up.sh` runs both natively (no Docker) against a running demo.

Everything below was run for real on 2026-11-12 (Windows 10, Prometheus 3.16.0, Grafana 13.3.1, Anvil with 1 s blocks, ports L1 28545, L2 29545, verifier 29546, Prometheus 29090, Grafana 23000). No browser was available, so the proof is API output, not screenshots: see [evidence/](evidence/). The dashboard JSON was validated by Grafana (provisioned, 17 panels) and every panel query returned data, but nobody looked at the rendered panels.

## Run it

```bash
KEEP=1 bash scripts/demo.sh                 # rollup + verifier + Anvil keep running (ports 18545/19545/19546)
bash scripts/observability-up.sh            # needs prometheus and grafana (see the variables at the top of the script)
go build -o bin/obscheck ./cmd/obscheck
bin/obscheck --prom http://127.0.0.1:9090 --grafana http://127.0.0.1:3000 --auth admin:admin
bash scripts/observability-down.sh          # stops both by PID
```

`obscheck` is the validity test of the stack: it fails unless every Prometheus target is up, all 8 rules are loaded and healthy, every query of every dashboard panel returns data, Grafana has the dashboard and the datasource provisioned, and Grafana can query Prometheus. The committed configs use the demo ports; `observability-up.sh` rewrites them when `L2_PORT`, `VER_PORT`, `PROM_PORT` or `GRAFANA_PORT` are set. Output of the final run: [evidence/obscheck.txt](evidence/obscheck.txt) (48 checks, 0 failures).

A unit test (`internal/metrics/monitoring_test.go`) fails when the dashboard or the alert rules mention a metric that no component exports, and `promtool check rules monitoring/alerts.yml` passes (8 rules).

## Metrics

All names are constants in `internal/metrics/catalog.go`, which also documents them (`# HELP`) and exports every counter at 0 so that `rate()`/`increase()` see the first event. Counters count processing events: after an L1 reorg the derivation processes the replayed L1 blocks again, so `rollup_batch_accepted_total` can exceed the number of distinct batches.

| Metric | Type | Meaning |
|---|---|---|
| `rollup_unsafe_head`, `rollup_safe_head`, `rollup_finalized_head` | gauge | L2 heads (the verifier has no unsafe head) |
| `rollup_l1_head`, `rollup_l1_cursor`, `rollup_next_epoch` | gauge | L1 head, last L1 block derived, next epoch to resolve |
| `rollup_derivation_lag_l1_blocks` | gauge | `l1_head - l1_cursor`; published after every derivation step, also idle ones |
| `rollup_l1_reorg_total`, `rollup_l1_reorg_last_depth` | counter, gauge | reorgs handled by the derivation, depth of the last one |
| `rollup_unsafe_reorg_total{cause}` | counter | sequencer dropped unsafe blocks: `l1_reorg` or `derivation_mismatch` |
| `rollup_batch_accepted_total`, `rollup_batch_rejected_total{reason}` | counter | derivation verdicts; reasons are the SPEC rules (`sender`, `decode`, `future_epoch`, `stale_epoch`, `late`, `version`, `block_count`, `origin_hash`, `timestamps`) |
| `rollup_fallback_epochs_total` | counter | epochs derived as an empty block because no valid batch arrived in time |
| `rollup_batch_submitted_total`, `rollup_batcher_inflight`, `rollup_batcher_oldest_inflight_seconds`, `rollup_batcher_next_epoch` | counter, gauges | batcher progress; the age is of the oldest batch not yet in an L1 block |
| `rollup_batcher_resubmitted_total{cause}` | counter | `fee_bump`, `l1_reorg`, `nonce_rebase` |
| `rollup_batcher_errors_total{stage}` | counter | failed batcher ticks (`track`, `submit`) |
| `rollup_txpool_size`, `rollup_tx_rejected_total{reason}`, `rollup_tx_skipped_total{reason}` | gauge, counters | pool backlog; refusals by the pool (bounded reason set); transactions skipped inside blocks |
| `rollup_blocks_built_total{kind}` | counter | unsafe blocks built (`epoch`, `tx`) |
| `rollup_block_build_seconds` | histogram | EVM execution time of one sequencer block |
| `rollup_loop_errors_total{loop}` | counter | failed iterations of the derivation, sequencer and batcher loops |

## Alerts (`monitoring/alerts.yml`)

Thresholds assume the demo (1 s L1 blocks, `SeqWindow` 8): on a 12 s L1 scale the durations.

| Alert | Fires when | Why |
|---|---|---|
| `RollupNodeDown` | a scrape target is down for 30 s | process gone |
| `SafeHeadStalled` | safe head did not move for 3 min while the L1 head did | derivation stuck (a silent batcher does not trigger it: the fallback keeps the chain moving) |
| `DerivationLagHigh` | lag above 10 L1 blocks for 1 min | slow L1 RPC or a long catch-up after a restart |
| `BatcherStuck` | batches in flight and the oldest unincluded one older than 60 s, for 30 s | L1 not mining, fee too low, wrong nonce |
| `BatcherErrors` | batcher ticks failing for 30 s | typically an unfunded batcher account |
| `FallbackEpochsIncreasing` | any fallback epoch in the last minute, for 30 s | sequencer or batcher silent, or its batches rejected |
| `ReorgSpike` | at least 2 L1 reorgs in 10 min | unstable L1 |
| `L1Unreachable` | more than 5 failed derivation or sequencer iterations in a minute, for 30 s | L1 RPC down (the batcher loop has its own alert) |

## Dashboard (`monitoring/grafana/dashboards/rollup.json`)

Provisioned as `go-mini-rollup`: stat tiles (safe, finalized, unsafe, L1 head, lag, pool), heads over time (sequencer unsafe/safe, verifier safe, finalized, L1), derivation lag per node, batcher in-flight and age, batcher sends/resubmissions/errors, batches accepted vs rejected by reason, fallback epochs, L1 and unsafe reorgs, block build p50/p95/p99, pool size with rejections and skips, blocks built and loop errors, and a table of pending or firing alerts.

## Controlled failures (alerts firing and resolving)

Polling logs, one line per 10 s, taken with the API (`/api/v1/alerts` and instant queries): `evidence/failure-*.log`.

**A. L1 stops mining** (`evm_setIntervalMining 0`). The batcher keeps a batch in flight. `rollup_batcher_oldest_inflight_seconds` grew linearly; `BatcherStuck` went pending at 17:20:32 (63 s after the stall) and firing at 17:21:03. After `evm_setIntervalMining 1` the batch was included and the alert was gone within 11 s. ([log](evidence/failure-a-l1-stalled.log))

**B. The batcher account runs out of L1 funds** (`anvil_setBalance 0`). Batch submissions fail, no batch reaches L1, and after `SeqWindow` every epoch is derived as an empty block: `rollup_fallback_epochs_total` rose by about 1 per second. `BatcherErrors` and `FallbackEpochsIncreasing` (sequencer and verifier) went pending after 12 s and firing after about 45 s. After refunding the account the batcher resumed, `BatcherErrors` cleared about 63 s later and `FallbackEpochsIncreasing` after about 96 s (its 1-minute window has to slide past the last fallback block). The run was done twice; the first time `L1Unreachable` fired as well, which was wrong (a funding problem is not an L1 outage), so the rule now looks only at the derivation and sequencer loops. The log is from the second run. ([log](evidence/failure-b-batcher-unfunded.log))

**C. Two L1 reorgs** (`anvil_reorg 2` twice, 22 s apart). `ReorgSpike` was firing at the first sample after the second reorg (17:28:43) on both nodes and cleared when the 10-minute window moved past them (17:38:31). ([log](evidence/failure-c-l1-reorgs.log))

## What the exercise found

- **Fee bumps compounded without limit.** In failure A the batcher replaced its stuck batches every 5 ticks with a fee 25 % higher: 65 bumps in under three minutes. Fixed in the batcher (`MaxGasPrice`, default 200 gwei, covered by `TestFeeBumpsStopAtTheGasPriceCap`). The evidence for A was recorded before that fix.
- **After an outage the batcher posts stale batches.** In B, once refunded, the batcher re-sent the batches of epochs the derivation had already resolved with a fallback block: the counter `rollup_batch_rejected_total{reason="stale_epoch"}` reached 155 after the first run and 289 after the second (see the end of the log). They cost L1 gas for nothing. The batcher should skip epochs that the derivation has already resolved; listed in [ROADMAP.md](ROADMAP.md).
- A misconfigured alert (above) and a metric with an unbounded label (the pool rejection reason was the raw error text) were caught while building the dashboard and fixed.

## Limits

- Not a production monitoring setup: no Alertmanager, no authentication, retention of 2 hours, Grafana with a throwaway admin password on `127.0.0.1`.
- The Prometheus and Grafana run is not part of CI (it needs both binaries); CI covers the metric catalog, the dashboard JSON and the rule file with unit tests.
- Gauges are only exported after the owning loop has run once; counters from the catalog are exported from the start.
- On Windows, stopping Grafana with a plain `kill` leaves its plugin sub-processes running (a hard kill gives Grafana no chance to stop them). On Linux `SIGTERM` shuts them down.
