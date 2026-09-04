#!/usr/bin/env bash
# Stop tailcat-dns-proxy (and its auto-launched tailcat socks child, which the
# proxy terminates on SIGTERM via its signal handler).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_FILE="$ROOT/run/tailcat-dns-proxy.pid"

if [[ ! -f "$PID_FILE" ]]; then
  echo "not running (no pid file)"
  exit 0
fi

PID="$(cat "$PID_FILE")"
if ! kill -0 "$PID" 2>/dev/null; then
  echo "stale pid file (pid $PID not alive); removing"
  rm -f "$PID_FILE"
  exit 0
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
echo "stopped"
