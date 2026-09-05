#!/usr/bin/env bash
# Start tailcat-dns-proxy in the background.
# Usage: bin/start.sh [go|python]    (default: go)
# Config via env (all optional):
#   LISTEN=127.0.0.1:1080  DNS_FILE=<root>/dns.txt  UPSTREAM=127.0.0.1:0
#   TAILCAT_BIN=tailcat    PROXY_ARGS="extra flags"
# PROXY_ARGS is word-split and glob-expanded by the shell (intended for
# multiple flags); avoid spaces inside individual flag values.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/run"

if [[ $# -gt 1 ]]; then
  echo "usage: $0 [go|python]" >&2
  exit 2
fi

IMPL="${1:-go}"
case "$IMPL" in
  go)
    BIN="$ROOT/go/bin/tailcat-dns-proxy"
    PID_FILE="$RUN_DIR/tailcat-dns-proxy-go.pid"
    LOG_FILE="$RUN_DIR/tailcat-dns-proxy-go.log"
    ;;
  python)
    SRC="$ROOT/python/tailcat_dns_proxy.py"
    PID_FILE="$RUN_DIR/tailcat-dns-proxy-python.pid"
    LOG_FILE="$RUN_DIR/tailcat-dns-proxy-python.log"
    ;;
  *)
    echo "usage: $0 [go|python]" >&2
    exit 2
    ;;
esac

LISTEN="${LISTEN:-127.0.0.1:1080}"
DNS_FILE="${DNS_FILE:-$ROOT/dns.txt}"
UPSTREAM="${UPSTREAM:-127.0.0.1:0}"
TAILCAT_BIN="${TAILCAT_BIN:-tailcat}"

mkdir -p "$RUN_DIR"

if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  echo "already running [$IMPL] (pid $(cat "$PID_FILE"))"
  exit 1
fi

# Build the Go proxy if the binary is missing (requires a Go toolchain).
if [[ "$IMPL" == go && ! -x "$BIN" ]]; then
  echo "go binary missing; building (go build)..."
  (cd "$ROOT/go" && go build -o bin/tailcat-dns-proxy .)
fi

: > "$LOG_FILE"   # truncate on each fresh start

ARGS=(--listen "$LISTEN" --dns-file "$DNS_FILE" --upstream "$UPSTREAM" --tailcat-bin "$TAILCAT_BIN")
if [[ "$IMPL" == go ]]; then
  nohup "$BIN" "${ARGS[@]}" ${PROXY_ARGS:-} >>"$LOG_FILE" 2>&1 &
else
  nohup python3 -u "$SRC" "${ARGS[@]}" ${PROXY_ARGS:-} >>"$LOG_FILE" 2>&1 &
fi
PID=$!
echo "$PID" > "$PID_FILE"

# Bail out loudly if the proxy died immediately (bad --dns-file, port in use).
sleep 0.4
if ! kill -0 "$PID" 2>/dev/null; then
  echo "proxy [$IMPL] exited immediately (pid $PID); last log lines:" >&2
  tail -5 "$LOG_FILE" >&2 || true
  rm -f "$PID_FILE"
  exit 1
fi

echo "started [$IMPL] pid $PID  listen=$LISTEN  log=$LOG_FILE"
