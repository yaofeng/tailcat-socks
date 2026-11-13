#!/usr/bin/env bash
# Stop tailcat-socks (and its auto-launched tailcat socks child, which
# the proxy terminates on SIGTERM via its signal handler).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PID_FILE="$ROOT/run/tailcat-socks-go.pid"

# Only signal a pid that really is a tailcat-socks: a pid file can outlive
# a reboot and land on an unrelated process. Match exact argv tokens, not
# substrings — e.g. `tail -f run/tailcat-socks-go.log` mentions the proxy
# but is not the proxy.
is_ours() {
  local token
  while IFS= read -r -d '' token; do
    [[ "$token" == *tailcat-socks ]] && return 0
  done 2>/dev/null < "/proc/$1/cmdline"
  return 1
}

if [[ ! -f "$PID_FILE" ]]; then
  echo "not running"
  exit 0
fi

PID="$(cat "$PID_FILE")"
if ! kill -0 "$PID" 2>/dev/null || ! is_ours "$PID"; then
  echo "stale pid file $PID_FILE (pid $PID not alive or not ours); removing"
  rm -f "$PID_FILE"
  exit 0
fi

echo "stopping pid $PID ..."
kill -TERM "$PID" 2>/dev/null || true
# 20s covers the proxy's two longest exit paths: its 5s TERM->KILL child
# escalation, and a TERM latched during the <=15s upstream-startup wait
# (acted on only once that wait returns). Only a proxy that truly ignores
# TERM waits out the full window before the KILL below.
for _ in $(seq 1 80); do
  kill -0 "$PID" 2>/dev/null || break
  sleep 0.25
done
if kill -0 "$PID" 2>/dev/null; then
  echo "forcing kill -9"
  kill -KILL "$PID" 2>/dev/null || true
fi
rm -f "$PID_FILE"
echo "stopped"
