#!/bin/sh
# Base-owned pnpm certification (#183).
#
# `pnpm-verify --certify` is the whole former body: the distribution audit, the
# exact report, the exec-target access audit and the five policy defaults. This
# file is only the entry point two callers mount: the base recipe runs it beside
# install-pnpm.sh, and images/standard/verify-standard.sh runs it twice. The
# helper's `pnpm-verify` without `--certify` is the distribution audit alone,
# which is what the dsh hook re-runs around its own health gate.
set -eu
helper=${RECIPE_CONTRACT_HELPER:-/tmp/recipe-contract/recipe-contract}
if [ ! -x "$helper" ]; then
    echo "verify-pnpm.sh: no recipe-contract helper at $helper" >&2
    exit 1
fi
exec "$helper" pnpm-verify --certify "$@"
