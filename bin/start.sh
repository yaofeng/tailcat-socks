#!/usr/bin/env bash
# Start tailcat-socks in the background.
# Usage: bin/start.sh [go|python|rust]    (default: go)
# Config via env (all optional):
#   LISTEN=127.0.0.1:1080  DNS_FILE=<root>/dns.txt  UPSTREAM=127.0.0.1:0
#   TAILCAT_BIN=tailcat    PROXY_ARGS="extra flags"
# PROXY_ARGS is word-split and glob-expanded by the shell (intended for
# multiple flags); avoid spaces inside individual flag values.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_DIR="$ROOT/run"

if [[ $# -gt 1 ]]; then
  echo "usage: $0 [go|python|rust]" >&2
  exit 2
fi

IMPL="${1:-go}"
case "$IMPL" in
  go)
    BIN="$ROOT/go/bin/tailcat-socks"
    PID_FILE="$RUN_DIR/tailcat-socks-go.pid"
    LOG_FILE="$RUN_DIR/tailcat-socks-go.log"
    ;;
  python)
    SRC="$ROOT/python/tailcat_socks.py"
    PID_FILE="$RUN_DIR/tailcat-socks-python.pid"
    LOG_FILE="$RUN_DIR/tailcat-socks-python.log"
    ;;
  rust)
    BIN="$ROOT/rust/target/release/tailcat-socks"
    PID_FILE="$RUN_DIR/tailcat-socks-rust.pid"
    LOG_FILE="$RUN_DIR/tailcat-socks-rust.log"
    ;;
  *)
    echo "usage: $0 [go|python|rust]" >&2
    exit 2
    ;;
esac

LISTEN="${LISTEN:-127.0.0.1:1080}"
DNS_FILE="${DNS_FILE:-$ROOT/dns.txt}"
UPSTREAM="${UPSTREAM:-127.0.0.1:0}"
TAILCAT_BIN="${TAILCAT_BIN:-tailcat}"

mkdir -p "$RUN_DIR"

# A pid file can outlive a reboot and land on an unrelated live process; only
# refuse to start when the pid really is ours (same exact-argv-token check as
# stop.sh — `tail -f run/…-go.log` mentions the proxy but is not the proxy).
is_ours() {
  local token
  while IFS= read -r -d '' token; do
    [[ "$token" == *tailcat-socks || "$token" == *tailcat_socks.py ]] && return 0
  done 2>/dev/null < "/proc/$1/cmdline"
  return 1
}

if [[ -f "$PID_FILE" ]]; then
  OLD_PID="$(cat "$PID_FILE")"
  if kill -0 "$OLD_PID" 2>/dev/null && is_ours "$OLD_PID"; then
    echo "already running [$IMPL] (pid $OLD_PID)"
    exit 1
  fi
  echo "stale pid file $PID_FILE (pid $OLD_PID not alive or not ours); removing"
  rm -f "$PID_FILE"
fi

# Build the proxy if the binary is missing (requires the toolchain).
if [[ "$IMPL" == go && ! -x "$BIN" ]]; then
  echo "go binary missing; building (go build)..."
  (cd "$ROOT/go" && go build -o bin/tailcat-socks .)
fi
if [[ "$IMPL" == rust && ! -x "$BIN" ]]; then
  echo "rust binary missing; building (cargo build --release)..."
  if command -v cargo >/dev/null 2>&1; then
    (cd "$ROOT/rust" && cargo build --release)
  elif [[ -f "$HOME/.cargo/env" ]]; then
    (source "$HOME/.cargo/env" && cd "$ROOT/rust" && cargo build --release)
  else
    echo "cargo not found (install via https://rustup.rs)" >&2
    exit 1
  fi
fi

: > "$LOG_FILE"   # truncate on each fresh start

ARGS=(--listen "$LISTEN" --dns-file "$DNS_FILE" --upstream "$UPSTREAM" --tailcat-bin "$TAILCAT_BIN")
if [[ "$IMPL" != python ]]; then
  nohup "$BIN" "${ARGS[@]}" ${PROXY_ARGS:-} >>"$LOG_FILE" 2>&1 &
else
  nohup python3 -u "$SRC" "${ARGS[@]}" ${PROXY_ARGS:-} >>"$LOG_FILE" 2>&1 &
fi
PID=$!
echo "$PID" > "$PID_FILE"

# Bail out loudly if the proxy died immediately (bad --dns-file, port in use).
# 0.4s catches fast fails only; a later crash still leaves a stale pid file
# for stop.sh to clean up.
sleep 0.4
if ! kill -0 "$PID" 2>/dev/null; then
  echo "proxy [$IMPL] exited immediately (pid $PID); last log lines:" >&2
  tail -5 "$LOG_FILE" >&2 || true
  rm -f "$PID_FILE"
  exit 1
fi

echo "started [$IMPL] pid $PID  listen=$LISTEN  log=$LOG_FILE"
