#!/usr/bin/env bash
# Start tailcat-dns-proxy in the background.
# Config via env (all optional):
#   LISTEN=127.0.0.1:1080  DNS_FILE=<root>/dns.txt  UPSTREAM=127.0.0.1:0
#   TAILCAT_BIN=tailcat    PROXY_ARGS="extra flags"
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$ROOT/src/tailcat_dns_proxy.py"
RUN_DIR="$ROOT/run"
PID_FILE="$RUN_DIR/tailcat-dns-proxy.pid"
LOG_FILE="$RUN_DIR/tailcat-dns-proxy.log"

LISTEN="${LISTEN:-127.0.0.1:1080}"
DNS_FILE="${DNS_FILE:-$ROOT/dns.txt}"
UPSTREAM="${UPSTREAM:-127.0.0.1:0}"
TAILCAT_BIN="${TAILCAT_BIN:-tailcat}"

mkdir -p "$RUN_DIR"

if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
  echo "already running (pid $(cat "$PID_FILE"))"
  exit 1
fi

: > "$LOG_FILE"   # truncate on each fresh start
nohup python3 -u "$SRC" \
  --listen "$LISTEN" \
  --dns-file "$DNS_FILE" \
  --upstream "$UPSTREAM" \
  --tailcat-bin "$TAILCAT_BIN" \
  ${PROXY_ARGS:-} \
  >>"$LOG_FILE" 2>&1 &
echo $! > "$PID_FILE"

echo "started pid $(cat "$PID_FILE")  listen=$LISTEN  log=$LOG_FILE"
