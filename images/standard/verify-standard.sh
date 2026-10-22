#!/bin/sh
# Build-only certification: selection labels and readable files are not proof
# that the finished PATH chooses the exact commands checked by fixed-path gates.
set -eu

if [ "$#" -ne 0 ]; then
    echo 'verify-standard.sh: no arguments accepted' >&2
    exit 2
fi
AGENT_INSTALL_SOFT_FAIL=
export AGENT_INSTALL_SOFT_FAIL
AGENT_VM_CONTRACT_DIR="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
AGENT_VM_INSTALL_STATUS_DIR="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"
# The dsh verifier resolves its own helpers from its directory, but its pnpm
# identity checks need the mount name; exporting keeps one source for both.
PNPM_SOURCE_DIR="${PNPM_SOURCE_DIR:-/tmp/pnpm-source}"
export AGENT_VM_CONTRACT_DIR AGENT_VM_INSTALL_STATUS_DIR PNPM_SOURCE_DIR
tools="${AGENT_VM_TOOL_SOURCE_DIR:-/tmp/standard-tools}"

require_installed() {
    for name in dsh pi pi-claude-bridge codex opencode claude copilot opencode-claude; do
        if record=$("$AGENT_VM_CONTRACT_DIR/recipe-contract" install-status record "$AGENT_VM_INSTALL_STATUS_DIR/$name"); then
            if [ "$record" = installed ]; then continue; fi
        fi
        echo "verify-standard.sh: $name requires exact installed record" >&2
        return 1
    done
}

check_path_identity() {
    # The dsh target is the installed lib entrypoint the image links directly;
    # a generated .bin wrapper or a same-version copy is a different file.
    # The helper is a sibling: host certification runs this script in place, and
    # the final RUN binds the .py beside it in /tmp.
    python3 "$(dirname "$0")/verify-standard-path-identity.py" \
        "${AGENT_VM_DSH_PREFIX:-/opt/agent-vm/dsh}/node_modules/@deepseek-ai/dsh/lib/bin.js" \
        "${AGENT_VM_CODEX_PREFIX:-/opt/agent}/.local/bin/codex" \
        "${AGENT_VM_OPENCODE_PREFIX:-/opt/agent}/.opencode/bin/opencode" \
        "${AGENT_VM_CLAUDE_PREFIX:-/opt/agent}/.local/bin/claude" \
        "${AGENT_VM_PI_WRAPPER:-/usr/local/bin/pi}"
}

require_installed
check_path_identity
sh "$PNPM_SOURCE_DIR/verify-pnpm.sh"
for tool in dsh pi codex opencode claude copilot; do
    sh "$tools/$tool/verify-$tool.sh"
done
AGENT_VM_OPENCODE_SOURCE_DIR="$tools/opencode"
export AGENT_VM_OPENCODE_SOURCE_DIR AGENT_VM_VERSION_OPENCODE_CLAUDE
sh "$tools/opencode/verify-opencode-plugins.sh"
sh "$PNPM_SOURCE_DIR/verify-pnpm.sh"
check_path_identity
require_installed
