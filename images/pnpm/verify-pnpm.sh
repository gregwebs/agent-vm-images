#!/bin/sh
# Base-owned pnpm certification (#183).
#
# `pnpm-verify --certify` is the whole former body: the distribution audit, the
# exact report, the exec-target access audit and the five policy defaults. This
# file is only the entry point two callers mount: the base recipe runs it beside
# install-pnpm.sh, and images/standard/verify-standard.sh runs it twice. The
# helper's `pnpm-verify` without `--certify` is the distribution audit alone,
# which is what the dsh hook re-runs around its own health gate.
#
# The helper is named by RECIPE_CONTRACT_HELPER, else by AGENT_VM_CONTRACT_DIR --
# which verify-standard.sh and the fixtures have always exported -- else by the
# fixed build mount.
set -eu
if [ -n "${RECIPE_CONTRACT_HELPER-}" ]; then
    helper=$RECIPE_CONTRACT_HELPER
else
    helper="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}/recipe-contract"
fi
if [ ! -x "$helper" ]; then
    echo "verify-pnpm.sh: no recipe-contract helper at $helper" >&2
    exit 1
fi
exec "$helper" pnpm-verify --certify "$@"
