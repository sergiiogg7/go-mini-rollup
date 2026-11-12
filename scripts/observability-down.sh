#!/usr/bin/env bash
# Stops the Prometheus and Grafana processes started by observability-up.sh (by PID, never by name).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"; PIDS="$ROOT/.obs/pids"
[ -f "$PIDS" ] || { echo "nothing to stop"; exit 0; }
while read -r name pid; do
  kill "$pid" 2>/dev/null && echo "stopped $name ($pid)" || echo "$name ($pid) was not running"
done <"$PIDS"
sleep 1
while read -r name pid; do
  if kill -0 "$pid" 2>/dev/null; then kill -9 "$pid" 2>/dev/null || true; echo "killed $name ($pid)"; fi
done <"$PIDS"
rm -f "$PIDS"
