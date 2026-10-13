#!/usr/bin/env bash
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
source_dir=${PNPM_SOURCE_DIR:-/tmp/pnpm-source}
exec python3 "$source_dir/bootstrap.py" "$@"
