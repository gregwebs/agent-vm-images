#!/usr/bin/env bash
# Daemon reconfiguration is permitted only on fresh disposable CI, never operators.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_OS:-}" = Linux ] || { echo 'disposable Linux Actions runner only' >&2; exit 1; }
sudo python3 "$ROOT/script/release/prepare-ci-daemon.py"
timeout --kill-after=10s 120 sudo systemctl restart docker
