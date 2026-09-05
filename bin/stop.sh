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

# Only signal a pid that really is a tailcat-dns-proxy: a pid file can outlive
# a reboot and land on an unrelated process. Match exact argv tokens, not
# substrings — e.g. `tail -f run/tailcat-dns-proxy-go.log` mentions the proxy
# but is not the proxy.
is_ours() {
  local token
  while IFS= read -r -d '' token; do
    [[ "$token" == *tailcat-dns-proxy || "$token" == *tailcat_dns_proxy.py ]] && return 0
  done 2>/dev/null < "/proc/$1/cmdline"
  return 1
}

STOPPED=0
for PID_FILE in "${PID_FILES[@]}"; do
  [[ -f "$PID_FILE" ]] || continue
  PID="$(cat "$PID_FILE")"
  if ! kill -0 "$PID" 2>/dev/null || ! is_ours "$PID"; then
    echo "stale pid file $PID_FILE (pid $PID not alive or not ours); removing"
    rm -f "$PID_FILE"
    continue
  fi
  STOPPED=1
  echo "stopping pid $PID ..."
  kill -TERM "$PID" 2>/dev/null || true
  # 8s > the proxy's own 5s child-escalation so it can finish cleaning up.
  for _ in $(seq 1 32); do
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
