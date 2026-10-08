#!/usr/bin/env bash
# Offline retention/schema regressions; no sudo, network or VM required.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
exec python3 "$ROOT/script/test/ci-boot-evidence.py" "$@"
