#!/bin/sh
# Installed-command gate for the opencode layer. Bind-mounted, never COPYed.
#
# The policy is the `opencode-verify` subcommand of the composed helper (ADR 0017,
# #218): the lstat path state, the absence record, the bounded status-checked
# `--version`, the exact report, the stderr contradiction and the access audit all
# live there. This file is only the entry point the recipe runs.
#
# Reached after install-opencode.sh: a hard failure already aborted the build, and
# a softened transport failure exited 0 having left an `absent-transport CODE`
# record and no command. The driver accepts that degraded state only when the
# record is present.
set -eu

CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
# Production prefix is the installer's $HOME; the seam matches install-opencode.sh.
prefix="${AGENT_VM_OPENCODE_PREFIX:-/opt/agent}"
# The requested slot is the assertion the report must name, so an unset one is
# refused here with the status the retired gate used rather than trusted as empty.
slot="${AGENT_VM_VERSION_OPENCODE:?verify-opencode.sh needs AGENT_VM_VERSION_OPENCODE}"
status_dir="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"

exec "$CONTRACT/recipe-contract" opencode-verify \
    --contract "$CONTRACT" \
    --prefix "$prefix" \
    --opencode-version "$slot" \
    --status-dir "$status_dir"
