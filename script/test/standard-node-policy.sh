#!/usr/bin/env bash
# The watchdog owns the entire matrix, including container/volume setup and
# removal, so a hung registry, manager or docker client cannot outlive the
# audit job that started it.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
# Reject preload injection before any Node subprocess can start.
[ -z "${NODE_OPTIONS:-}${NODE_PATH:-}${BUN_OPTIONS:-}" ] || {
    echo 'standard-node-policy: manager runtime overrides are unsupported' >&2
    exit 1
}
if [ "${1:-}" = --self-test ]; then
    [ "$#" -eq 1 ] || exit 2
    exec python3 "$ROOT/script/test/standard-node-policy-self-test.py"
fi
exec python3 "$ROOT/script/test/host-watchdog.py" 3600 \
    python3 "$ROOT/script/test/standard-node-policy.py" "$@"
