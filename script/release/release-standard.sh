#!/usr/bin/env bash
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
case "${1:-}" in prepare | publish) ;; *) echo "usage: $0 prepare|publish [named options]" >&2; exit 2 ;; esac
exec python3 "$ROOT/script/release/operations.py" "$@"
