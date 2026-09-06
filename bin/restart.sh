#!/usr/bin/env bash
# Restart tailcat-socks. Usage: bin/restart.sh [go|python|rust]
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$DIR/stop.sh" "${1:-}"
exec "$DIR/start.sh" "${1:-}"
