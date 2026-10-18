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
        if record=$(python3 "$AGENT_VM_CONTRACT_DIR/install-status.py" record "$AGENT_VM_INSTALL_STATUS_DIR/$name"); then
            if [ "$record" = installed ]; then continue; fi
        fi
        echo "verify-standard.sh: $name requires exact installed record" >&2
        return 1
    done
}

check_path_identity() {
    # The dsh target is the installed lib entrypoint the image links directly;
    # a generated .bin wrapper or a same-version copy is a different file.
    python3 - \
        "${AGENT_VM_DSH_PREFIX:-/opt/agent-vm/dsh}/node_modules/@deepseek-ai/dsh/lib/bin.js" \
        "${AGENT_VM_CODEX_PREFIX:-/opt/agent}/.local/bin/codex" \
        "${AGENT_VM_OPENCODE_PREFIX:-/opt/agent}/.opencode/bin/opencode" \
        "${AGENT_VM_CLAUDE_PREFIX:-/opt/agent}/.local/bin/claude" \
        "${AGENT_VM_PI_WRAPPER:-/usr/local/bin/pi}" <<'PY'
import os
import stat
import sys

for name, verified in zip(("dsh", "codex", "opencode", "claude", "pi"), sys.argv[1:]):
    candidate = next(
        (os.path.join(directory, name)
         for directory in os.environ.get("PATH", "").split(os.pathsep)
         if os.path.lexists(os.path.join(directory, name))),
        None,
    )
    try:
        if candidate is None:
            raise ValueError("missing command")
        if not stat.S_ISREG(os.stat(candidate).st_mode) or not os.access(candidate, os.X_OK):
            raise ValueError("not a usable executable regular file")
        if not os.path.samefile(candidate, verified):
            raise ValueError("PATH selects a different file than the exact-version verifier")
    except (OSError, ValueError) as error:
        print(f"verify-standard.sh: {name}: PATH candidate {candidate!r}, verified {verified!r}: {error}", file=sys.stderr)
        sys.exit(1)
    print(f"  {name}: PATH {candidate} -> verified {os.path.realpath(verified)}")
PY
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
