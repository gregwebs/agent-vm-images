#!/bin/sh
# Native probe dispatch stays separate from host orchestration.
set -eu
[ -z "${NODE_OPTIONS:-}${NODE_PATH:-}${BUN_OPTIONS:-}" ] || {
    echo 'standard-node-consumers: manager runtime overrides are unsupported' >&2
    exit 1
}
exec python3 /source/script/test/fixtures/standard-node-consumers.py "$@"
