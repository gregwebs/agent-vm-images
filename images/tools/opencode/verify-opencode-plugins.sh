#!/bin/sh
# A stale record cannot certify activation or the SDK/Claude handoff.
#
# The policy is the `opencode-verify-plugins` subcommand of the composed helper
# (ADR 0017, #218): the reviewed-bytes check, the guest config assertion, the
# payload shape, the runtime handoff, the loopback probe and the access audit all
# live there. The `.mjs` programs and `probe-opencode.py` deliberately stay where
# they are -- they are the plugin's own runtime and the released image's probe --
# so this file is only the entry point, and the driver runs them as children.
set -eu

CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
SOURCE="${AGENT_VM_OPENCODE_SOURCE_DIR:-/tmp/opencode-source}"
PREFIX="${AGENT_VM_OPENCODE_PLUGINS_PREFIX:-/opt/agent-vm/opencode-plugins}"
CLAUDE="${AGENT_VM_CLAUDE_PREFIX:-/opt/agent}/.local/bin/claude"
OPENCODE="${AGENT_VM_OPENCODE_PREFIX:-/opt/agent}/.opencode/bin/opencode"
CONFIG="${AGENT_VM_OPENCODE_CONFIG_PATH:-/opt/agent-vm/opencode/opencode.json}"
status_dir="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"

exec "$CONTRACT/recipe-contract" opencode-verify-plugins \
    --source "$SOURCE" \
    --contract "$CONTRACT" \
    --prefix "$PREFIX" \
    --claude "$CLAUDE" \
    --opencode "$OPENCODE" \
    --config "$CONFIG" \
    --opencode-config "${OPENCODE_CONFIG:-}" \
    --plugin-version "${AGENT_VM_VERSION_OPENCODE_CLAUDE:-}" \
    --status-dir "$status_dir"
