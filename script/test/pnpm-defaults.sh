#!/usr/bin/env bash
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
root=$(cd "$(dirname "$0")/../.." && pwd)
exec python3 "$root/script/test/pnpm-defaults.py" "$@"
