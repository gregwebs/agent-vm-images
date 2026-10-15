#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
export PYTHONDONTWRITEBYTECODE=1
exec python3 "$ROOT/script/test/opencode-plugins-verify.py"
