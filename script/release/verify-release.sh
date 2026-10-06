#!/usr/bin/env bash
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
exec python3 "$ROOT/script/release/verify.py" "$@"
