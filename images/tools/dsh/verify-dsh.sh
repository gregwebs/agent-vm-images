#!/bin/sh
# Installed-payload gate for the dsh layer (bind-mounted, never COPYed).
#
# The policy is the `dsh-verify` subcommand of the composed helper: the exact version
# reports, the same-file PATH identity, the runtime probes, the bounded web readiness
# driver, the all-uid access audit and the `installed` status record all live there.
# This file is only the entry point the standard recipe runs.
#
# The helper's directory comes from the shared project module's `contract-dir` verb,
# never from a fixed build-mount path: that verb authorizes the directory whose code
# this gate executes, so an unset AGENT_VM_CONTRACT_DIR resolves to this checkout's or
# fixture's own copy instead of an unrelated mount.
set -eu

PREFIX=${AGENT_VM_DSH_PREFIX:-/opt/agent-vm/dsh}
# The command the image links to the installed bin. The production value is fixed by
# the shared project module; a fixture prefix may point at its own fixture command.
COMMAND=${AGENT_VM_DSH_COMMAND:-/usr/local/bin/dsh}
status_dir=${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}

SOURCE=${AGENT_VM_DSH_SOURCE_DIR:-}
if [ -z "$SOURCE" ]; then
    SOURCE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
fi
PROJECT=$SOURCE/project.py
[ -f "$PROJECT" ] || {
    echo "  dsh: build helper is missing at $PROJECT" >&2
    exit 1
}
CONTRACT=$(python3 "$PROJECT" contract-dir "$PREFIX") || exit 1

exec "$CONTRACT/recipe-contract" dsh-verify \
    --source "$SOURCE" \
    --contract "$CONTRACT" \
    --prefix "$PREFIX" \
    --command "$COMMAND" \
    --status-dir "$status_dir" \
    --dsh-version "${AGENT_VM_VERSION_DSH:-}" \
    --pnpm-version "${AGENT_VM_VERSION_PNPM:-}"
