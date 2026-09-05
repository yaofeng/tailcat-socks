#!/usr/bin/env bash
# Stop tailcat-dns-proxy (and its auto-launched tailcat socks child, which
# the proxy terminates on SIGTERM via its signal handler).
# Usage: bin/stop.sh [go|python]   (no arg = stop both)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/run"

IMPL="${1:-}"
case "$IMPL" in
  "")     PID_FILES=("$RUN_DIR/tailcat-dns-proxy-go.pid" "$RUN_DIR/tailcat-dns-proxy-python.pid") ;;
  go)     PID_FILES=("$RUN_DIR/tailcat-dns-proxy-go.pid") ;;
  python) PID_FILES=("$RUN_DIR/tailcat-dns-proxy-python.pid") ;;
  *)      echo "usage: $0 [go|python]" >&2; exit 2 ;;
esac

STOPPED=0
for PID_FILE in "${PID_FILES[@]}"; do
  [[ -f "$PID_FILE" ]] || continue
  STOPPED=1
  PID="$(cat "$PID_FILE")"
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "stale pid file $PID_FILE (pid $PID not alive); removing"
    rm -f "$PID_FILE"
    continue
  fi
  echo "stopping pid $PID ..."
  kill -TERM "$PID" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.25
  done
  if kill -0 "$PID" 2>/dev/null; then
    echo "forcing kill -9"
    kill -KILL "$PID" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
done

if [[ "$STOPPED" == 1 ]]; then
  echo "stopped"
else
  echo "not running"
fi
