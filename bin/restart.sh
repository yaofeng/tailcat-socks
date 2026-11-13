#!/usr/bin/env bash
# Restart tailcat-socks.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
"$DIR/stop.sh"
exec "$DIR/start.sh"
