#!/bin/sh
# A stale record cannot certify activation or the SDK/Claude handoff.
set -eu
CONTRACT=${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}
SOURCE=${AGENT_VM_OPENCODE_SOURCE_DIR:-/tmp/opencode-source}
PREFIX=${AGENT_VM_OPENCODE_PLUGINS_PREFIX:-/opt/agent-vm/opencode-plugins}
CLAUDE=${AGENT_VM_CLAUDE_PREFIX:-/opt/agent}/.local/bin/claude
OPENCODE=${AGENT_VM_OPENCODE_PREFIX:-/opt/agent}/.opencode/bin/opencode
CONFIG=${AGENT_VM_OPENCODE_CONFIG_PATH:-/opt/agent-vm/opencode/opencode.json}
status_dir=${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
# Reject preload injection before starting the Node policy checker, as the install
# and prepare paths do: a preloaded module can patch node:crypto and subvert a
# digest comparison.
[ -z "${NODE_OPTIONS:-}${NODE_PATH:-}${BUN_OPTIONS:-}" ] || { echo 'manager runtime overrides are unsupported' >&2; exit 1; }
# Validate immutable inputs and actual ENV, rather than injecting configuration.
node "$SOURCE/plugins/check-lock.mjs" "$PREFIX" >"$work/version"
selected=$(cat "$work/version")
[ -z "${AGENT_VM_VERSION_OPENCODE_CLAUDE:-}" ] || [ "$AGENT_VM_VERSION_OPENCODE_CLAUDE" = "$selected" ]
[ "${OPENCODE_CONFIG:-}" = "$CONFIG" ] || { echo 'opencode-claude: image config selection missing' >&2; exit 1; }
node "$SOURCE/check-claude-payload.mjs" config "$CONFIG" "$PREFIX"
if ! sh "$CONTRACT/run-report.sh" 20 "$work/runtime.json" "$work/runtime.err" \
    node "$SOURCE/check-claude-runtime.mjs" "$PREFIX" "$CLAUDE"; then
    cat "$work/runtime.err" >&2
    exit 1
fi
node "$SOURCE/check-claude-payload.mjs" report "$work/runtime.json" "$selected"
if ! sh "$CONTRACT/run-report.sh" 90 "$work/evidence.json" "$work/probe.err" \
    python3 "$SOURCE/probe-opencode.py" capture --binary "$OPENCODE" --state-root "$work" --timeout 60; then
    cat "$work/probe.err" >&2
    exit 1
fi
python3 "$SOURCE/probe-opencode.py" check --evidence "$work/evidence.json" --plugin-entry "$PREFIX/index.js"
# Supply physical and logical regular files; the access auditor checks ancestors.
node "$SOURCE/check-claude-payload.mjs" files "$PREFIX" >"$work/files"
xargs -0 python3 "$CONTRACT/check-tool-access.py" --quiet --content <"$work/files"
python3 "$CONTRACT/check-tool-access.py" --content "$CONFIG"
python3 "$CONTRACT/check-tool-access.py" "$CLAUDE" "$OPENCODE"
mkdir -p "$status_dir"
printf 'installed\n' >"$status_dir/opencode-claude"
