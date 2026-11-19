#!/bin/sh
# Owning install hook for the dsh layer, bind-mounted and never COPYed so it stays
# out of the shipped image.
#
# The policy is the `dsh-install` subcommand of the composed helper: authorization,
# the base-pin assertions, the changed-selection preparation, the frozen install and
# the published-permission rules all live there. This file is only the entry point the
# recipe runs.
#
# The helper is located through the shared project module, never through a fixed
# build-mount path: `contract-dir` authorizes the directory whose code this invocation
# will execute, and it is invoked before anything from it runs. That decision is dsh's
# own -- the pnpm entry points fall back to the fixed mount, but this hook must not,
# because an unset AGENT_VM_CONTRACT_DIR has to resolve to this checkout's or
# fixture's own copy.
#
# Env contract (container defaults; fixture overrides only under
# AGENT_VM_FIXTURE_ROOT): AGENT_VM_DSH_PREFIX, AGENT_VM_DSH_SOURCE_DIR,
# AGENT_VM_DSH_COMMAND, AGENT_VM_VERSION_DSH, AGENT_VM_VERSION_PNPM.
set -eu

PREFIX=${AGENT_VM_DSH_PREFIX:-/opt/agent-vm/dsh}
SOURCE=${AGENT_VM_DSH_SOURCE_DIR:-/tmp/dsh-source}
COMMAND=${AGENT_VM_DSH_COMMAND:-/usr/local/bin/dsh}
PROJECT=$SOURCE/project.py

# project.py is the shared module's adapter (#62), so a missing source directory is
# named here rather than reported as an interpreter error.
[ -f "$PROJECT" ] || {
    echo "install-dsh.sh: dsh project helper is missing at $PROJECT" >&2
    exit 1
}
CONTRACT=$(python3 "$PROJECT" contract-dir "$PREFIX") || exit 1

exec "$CONTRACT/recipe-contract" dsh-install \
    --source "$SOURCE" \
    --contract "$CONTRACT" \
    --prefix "$PREFIX" \
    --command "$COMMAND" \
    --dsh-version "${AGENT_VM_VERSION_DSH:-}" \
    --pnpm-version "${AGENT_VM_VERSION_PNPM:-}"
