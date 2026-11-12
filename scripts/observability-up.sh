#!/usr/bin/env bash
# Starts Prometheus and Grafana natively (no Docker) against a running rollup, with the configs of
# monitoring/ copied into the git-ignored .obs/ directory. Stop with scripts/observability-down.sh.
#
#   PROMETHEUS_BIN   prometheus binary            (default: prometheus on PATH)
#   GRAFANA_BIN      grafana binary               (default: grafana on PATH; run as `grafana server`)
#   GRAFANA_HOME     Grafana install dir with conf/ and public/   (default: /usr/share/grafana)
#   L2_PORT VER_PORT ports of `rollup devnet` and `rollup verifier` (defaults 19545 / 19546, as the demo)
#   PROM_PORT GRAFANA_PORT                        (defaults 9090 / 3000)
#   GRAFANA_PASSWORD admin password               (default: admin; Grafana only listens on 127.0.0.1)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; cd "$ROOT"
OBS="$ROOT/.obs"
L2P=${L2_PORT:-19545}; VP=${VER_PORT:-19546}; PP=${PROM_PORT:-9090}; GP=${GRAFANA_PORT:-3000}
PROM=${PROMETHEUS_BIN:-prometheus}; GRAF=${GRAFANA_BIN:-grafana}; GHOME=${GRAFANA_HOME:-/usr/share/grafana}
PASS=${GRAFANA_PASSWORD:-admin}
native() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else echo "$1"; fi; }  # Windows-style path for native .exe

[ -f "$OBS/pids" ] && { echo "already running (see $OBS/pids); run scripts/observability-down.sh first" >&2; exit 1; }
mkdir -p "$OBS"/{prometheus-data,grafana/data,grafana/logs,grafana/plugins,grafana/dashboards,grafana/provisioning/datasources,grafana/provisioning/dashboards}

# Configs: the committed files use the default ports; rewrite them for this run.
sed -e "s/127.0.0.1:19545/127.0.0.1:$L2P/; s/127.0.0.1:19546/127.0.0.1:$VP/" monitoring/prometheus.yml >"$OBS/prometheus.yml"
cp monitoring/alerts.yml "$OBS/alerts.yml"
cp monitoring/grafana/dashboards/*.json "$OBS/grafana/dashboards/"
sed -e "s#127.0.0.1:9090#127.0.0.1:$PP#" monitoring/grafana/provisioning/datasources/prometheus.yml >"$OBS/grafana/provisioning/datasources/prometheus.yml"
sed -e "s#/var/lib/grafana/dashboards.*#$(native "$OBS/grafana/dashboards")#" monitoring/grafana/provisioning/dashboards/rollup.yml >"$OBS/grafana/provisioning/dashboards/rollup.yml"

wait_http() { for _ in $(seq 60); do curl -fsS "$1" >/dev/null 2>&1 && return 0; sleep 1; done; echo "timeout waiting for $1 (see $OBS/*.log)" >&2; return 1; }

"$PROM" --config.file="$(native "$OBS/prometheus.yml")" --storage.tsdb.path="$(native "$OBS/prometheus-data")" \
  --storage.tsdb.retention.time=2h --web.listen-address="127.0.0.1:$PP" >"$OBS/prometheus.log" 2>&1 &
echo "prometheus $!" >>"$OBS/pids"

GF_PATHS_DATA="$(native "$OBS/grafana/data")" GF_PATHS_LOGS="$(native "$OBS/grafana/logs")" \
GF_PATHS_PLUGINS="$(native "$OBS/grafana/plugins")" GF_PATHS_PROVISIONING="$(native "$OBS/grafana/provisioning")" \
GF_SECURITY_ADMIN_PASSWORD="$PASS" GF_SERVER_HTTP_ADDR=127.0.0.1 GF_SERVER_HTTP_PORT="$GP" \
GF_USERS_ALLOW_SIGN_UP=false GF_ANALYTICS_REPORTING_ENABLED=false GF_ANALYTICS_CHECK_FOR_UPDATES=false \
  "$GRAF" server --homepath "$(native "$GHOME")" >"$OBS/grafana.log" 2>&1 &
echo "grafana $!" >>"$OBS/pids"

wait_http "http://127.0.0.1:$PP/-/ready"
wait_http "http://127.0.0.1:$GP/api/health"
echo "Prometheus: http://127.0.0.1:$PP   (targets: /targets, alerts: /alerts)"
echo "Grafana:    http://127.0.0.1:$GP   (admin / $PASS, dashboard \"go-mini-rollup\")"
echo "Verify:     bin/obscheck --prom http://127.0.0.1:$PP --grafana http://127.0.0.1:$GP --auth admin:$PASS"
