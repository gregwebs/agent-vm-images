#!/bin/sh
# Offline native setup for the claude layer: run Anthropic's `claude install <v>`
# with no network at all, from the payload the install RUN already verified and
# placed. Bind-mounted, never COPYed.
#
# Why a separate RUN: `claude install` has a hardcoded CDN base and re-downloads a
# secondary payload (manifest.zst.json, manifest.json, manifest.json.raw-sig.json,
# <platform>/claude.zst) that no hook can verify before use. The verified payload
# is already at $HOME/.local/share/claude/versions/<v> with its launcher symlink, so
# this step only writes local state. `RUN --network=none` makes that structural: a
# future release that insists on downloading fails the build instead of silently
# adopting bytes (docs/adr/0013-claude-native-payload-pin.md).
#
# The bytes are re-verified immediately before they are executed: the native step
# does not re-check the placed payload itself, so a check after the fact would
# prove nothing about what ran.
set -eu

CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
# Production prefix is the native installer's $HOME; the seam matches
# install-claude.sh and lets the host suites use a temp root.
prefix="${AGENT_VM_CLAUDE_PREFIX:-/opt/agent}"
version="${AGENT_VM_VERSION_CLAUDE:?setup-claude.sh needs AGENT_VM_VERSION_CLAUDE}"
status_dir="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"
status="$status_dir/claude"

# The committed closure lives in the claude source directory. Production mounts it
# beside this hook; there is no ambient override for the pin contents.
if [ -n "${AGENT_VM_CLAUDE_SOURCE_DIR:-}" ]; then
    pin_dir="$AGENT_VM_CLAUDE_SOURCE_DIR"
else
    case "$0" in
        */*) pin_dir="${0%/*}" ;;
        *) pin_dir="." ;;
    esac
fi
[ -r "$pin_dir/pin.json" ] && [ -r "$pin_dir/claude_pin.py" ] || {
    echo "setup-claude.sh: no committed Claude pin at $pin_dir" >&2
    exit 1
}

launcher="$prefix/.local/bin/claude"
installed="$prefix/.local/share/claude/versions/$version"

# Absence is accepted only when the install RUN positively classified a transport
# failure and recorded it. Anything else -- a dangling symlink, a present but
# unusable file, a permission error on an ancestor -- is a contract violation and
# hard, exactly like verify-claude.sh.
bin_state=0
"$CONTRACT/recipe-contract" install-status path-state "$launcher" || bin_state=$?
if [ "$bin_state" -eq 0 ]; then
    if "$CONTRACT/recipe-contract" install-status code "$status" >/dev/null 2>&1; then
        echo "  claude: ABSENT ($(cat "$status")); no native setup"
        exit 0
    fi
    echo "  claude: command missing with no valid absence record -- hard failure" >&2
    exit 1
fi
if [ "$bin_state" -eq 2 ]; then
    echo "  claude: cannot stat $launcher (permission/loop) -- hard failure" >&2
    exit 1
fi

case "$(uname -m)" in
    x86_64|amd64) platform="linux-x64" ;;
    arm64|aarch64) platform="linux-arm64" ;;
    *)
        echo "  claude: unsupported architecture $(uname -m)" >&2
        exit 1
        ;;
esac

# Verify before execute: the bytes checked here are the bytes the native step runs.
python3 "$pin_dir/claude_pin.py" installed --pin-dir "$pin_dir" --version "$version" \
    --platform "$platform" --prefix "$prefix" --owner "$(id -u):$(id -g)"

# The verified file is what runs (not the launcher): the bytes just checked are the
# bytes executed. NPM_CONFIG_UPDATE_NOTIFIER: the native step runs
# `npm uninstall --global @anthropic-ai/claude-code`, whose update notifier is its
# only other network use. CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC keeps any
# telemetry attempt out of the build. The receipt is removed so a native 75 is
# never read as a transport classification.
rc=0
env -u AGENT_VM_TRANSPORT_RECEIPT HOME="$prefix" CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
    NPM_CONFIG_UPDATE_NOTIFIER=false timeout --kill-after=5 300 "$installed" install "$version" || rc=$?
if [ "$rc" -ne 0 ]; then
    echo "  claude: native setup exited $rc -- hard failure" >&2
    exit 1
fi

# The native step must have left the verified payload and layout as they were.
python3 "$pin_dir/claude_pin.py" installed --pin-dir "$pin_dir" --version "$version" \
    --platform "$platform" --prefix "$prefix" --owner "$(id -u):$(id -g)"
python3 "$pin_dir/claude_pin.py" native-state --prefix "$prefix"
echo "  claude: native setup ran offline for $version"
