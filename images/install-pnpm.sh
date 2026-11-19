#!/usr/bin/env bash
# Base-owned pnpm bootstrap (#183): `install-pnpm.sh [--fixture-archive PATH]`.
#
# The authenticated download, the safe extraction and the publication are the
# `pnpm-bootstrap` subcommand of the recipe-contract helper, so this file is only
# the entry point the base image mounts at /tmp/install-pnpm.sh. The pin and its
# mounts are the whole input: the reviewed pin, the launcher, the interpreter
# check and the fixed destinations live in the helper.
#
# The helper has to be supplied by the host compile (ADR 0017): the build mounts
# the composed directory and names it here. AGENT_VM_CONTRACT_DIR, which the
# standard recipe and the verifier fixtures have always exported, is honoured
# before the fixed build mount so an alternate composed directory keeps working.
set -euo pipefail
if [ -n "${RECIPE_CONTRACT_HELPER-}" ]; then
    helper=$RECIPE_CONTRACT_HELPER
else
    helper="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}/recipe-contract"
fi
if [ ! -x "$helper" ]; then
    echo "install-pnpm.sh: no recipe-contract helper at $helper" >&2
    exit 1
fi
exec "$helper" pnpm-bootstrap "$@"
