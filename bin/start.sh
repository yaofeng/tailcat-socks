#!/usr/bin/env bash
# Start tailcat-socks in the background.
# Config via env (all optional):
#   LISTEN=127.0.0.1:1080  DATA_PATH=<root>/data  PROXY_ARGS="extra flags"
# DATA_PATH holds dns.txt, config.yaml, tailcat-client-key and history.db.
# The binary itself reads ADMIN_ADDR (flag > env > default), so just export it;
# PROXY_ARGS can also pass --admin-addr.
# PROXY_ARGS is word-split and glob-expanded by the shell (intended for
# multiple flags); avoid spaces inside individual flag values.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/run"

BIN="$ROOT/bin/tailcat-socks"
PID_FILE="$RUN_DIR/tailcat-socks-go.pid"
LOG_FILE="$RUN_DIR/tailcat-socks-go.log"

LISTEN="${LISTEN:-127.0.0.1:1080}"
DATA_PATH="${DATA_PATH:-$ROOT/data}"

mkdir -p "$RUN_DIR"

# A pid file can outlive a reboot and land on an unrelated live process; only
# refuse to start when the pid really is ours (same exact-argv-token check as
# stop.sh — `tail -f run/…-go.log` mentions the proxy but is not the proxy).
is_ours() {
  local token
  while IFS= read -r -d '' token; do
    [[ "$token" == *tailcat-socks ]] && return 0
  done 2>/dev/null < "/proc/$1/cmdline"
  return 1
}

if [[ -f "$PID_FILE" ]]; then
  OLD_PID="$(cat "$PID_FILE")"
  if kill -0 "$OLD_PID" 2>/dev/null && is_ours "$OLD_PID"; then
    echo "already running (pid $OLD_PID)"
    exit 1
  fi
  echo "stale pid file $PID_FILE (pid $OLD_PID not alive or not ours); removing"
  rm -f "$PID_FILE"
fi

# Build the proxy if the binary is missing (requires the Go toolchain). The
# version comes from .version so a bare script build reports the same number as
# the published image.
if [[ ! -x "$BIN" ]]; then
  echo "go binary missing; building (go build)..."
  VER="$(tr -d '[:space:]' < "$ROOT/.version" 2>/dev/null || echo dev)"
  (cd "$ROOT/src" && go build -ldflags="-X main.version=$VER" -o "$ROOT/bin/tailcat-socks" .)
fi

: > "$LOG_FILE"   # truncate on each fresh start

ARGS=(--listen "$LISTEN" --data-path "$DATA_PATH")
nohup "$BIN" "${ARGS[@]}" ${PROXY_ARGS:-} >>"$LOG_FILE" 2>&1 &
PID=$!
echo "$PID" > "$PID_FILE"

# Bail out loudly if the proxy died immediately (bad --data-path, port in use).
# 0.4s catches fast fails only; a later crash still leaves a stale pid file
# for stop.sh to clean up.
sleep 0.4
if ! kill -0 "$PID" 2>/dev/null; then
  echo "proxy exited immediately (pid $PID); last log lines:" >&2
  tail -5 "$LOG_FILE" >&2 || true
  rm -f "$PID_FILE"
  exit 1
fi

echo "started pid $PID  listen=$LISTEN  log=$LOG_FILE"
