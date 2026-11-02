#!/bin/sh
# Owning install hook for the claude layer: validate the exact version, require
# the committed pin for it, run the pinned vendored installer, and (only) soften
# a positively classified transport failure by removing the recipe's own partial
# artifacts and writing an absence record. Bind-mounted, never COPYed, so it
# stays out of the shipped image.
#
# The vendored installer at $AGENT_VM_VENDOR_DIR/install.sh is Anthropic's
# published script with the committed-selection, classified-download and
# placement patches in images/tools/claude/vendor/install.patch. It selects the
# executable from images/tools/claude/pin.json (ADR 0013) instead of a live
# manifest, and it never runs `claude install`: that native step has its own
# offline RUN (setup-claude.sh) after the verified payload is in place. Running
# the installer as a child (not through run-install's own interpreter) is what
# lets this hook inspect the classified 75 and decide.
set -eu

VENDOR="${AGENT_VM_VENDOR_DIR:-/opt/agent-vm/recipe-vendor}"
# Production prefix is the native installer's $HOME. The seam lets the host
# black-box test point at a temp root; production always uses the default.
prefix="${AGENT_VM_CLAUDE_PREFIX:-/opt/agent}"
version="${AGENT_VM_VERSION_CLAUDE:?install-claude.sh needs AGENT_VM_VERSION_CLAUDE}"
receipt="${AGENT_VM_TRANSPORT_RECEIPT:-}"
status_dir="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"

# The committed closure (pin.json, its evidence, the release key and
# claude_pin.py) lives in the claude source directory. Production mounts it beside
# this hook; the explicit seam is what keeps a hook-only mount from silently
# pinning nothing. There is no ambient override for the pin contents, the expected
# digest, the URL or the signer policy: the directory is the whole input.
if [ -n "${AGENT_VM_CLAUDE_SOURCE_DIR:-}" ]; then
    pin_dir="$AGENT_VM_CLAUDE_SOURCE_DIR"
else
    case "$0" in
        */*) pin_dir="${0%/*}" ;;
        *) pin_dir="." ;;
    esac
fi
[ -r "$pin_dir/pin.json" ] && [ -r "$pin_dir/claude_pin.py" ] || {
    echo "install-claude.sh: no committed Claude pin at $pin_dir" >&2
    exit 1
}

# --- destructive-cleanup authorization (review B6) --------------------------
# The softened-transport branch below deletes recipe-owned partial artifacts.
# The prefix seam is env-overridable, so before deleting anything require the
# target to be the exact production path or a canonical child of
# AGENT_VM_FIXTURE_ROOT. Resolving the leaf's parent physically also rejects an
# intermediate directory symlink -- e.g. an env-overridden prefix symlinked at
# another real tree -- that would otherwise make `rm -rf` delete outside the
# authorized tree.
FIXTURE_ROOT="${AGENT_VM_FIXTURE_ROOT:-}"
# Resolve a path to its physical location without following a final symlink; a
# non-existent tail is resolved against its deepest existing ancestor. This
# rejects an intermediate directory symlink escaping the authorized tree.
physical_parent() { # $1 path
    _p=$1
    _dir=$(dirname "$_p")
    _tail=$(basename "$_p")
    while [ ! -d "$_dir" ] && [ "$_dir" != "/" ] && [ "$_dir" != "." ]; do
        _tail="$(basename "$_dir")/$_tail"
        _dir=$(dirname "$_dir")
    done
    if [ -d "$_dir" ]; then
        printf '%s/%s' "$(cd "$_dir" && pwd -P)" "$_tail"
    else
        printf '%s' "$_p"
    fi
}
cleanup_authorized() { # $1 target path, $2 exact production target
    _full=$(physical_parent "$1")
    if [ "$_full" = "$2" ]; then
        return 0
    fi
    if [ -z "$FIXTURE_ROOT" ]; then
        echo "  claude: refusing to clean $_full (not the production path $2; AGENT_VM_FIXTURE_ROOT is unset)" >&2
        return 1
    fi
    if [ ! -d "$FIXTURE_ROOT" ]; then
        echo "  claude: AGENT_VM_FIXTURE_ROOT is not a directory: $FIXTURE_ROOT" >&2
        return 1
    fi
    _root=$(cd "$FIXTURE_ROOT" && pwd -P)
    case "$_full" in
        "$_root"/*) return 0 ;;
    esac
    echo "  claude: refusing to clean $_full (outside the fixture root $_root)" >&2
    return 1
}

case "$version" in
    *[!0-9A-Za-z.+-]*)
        echo "install-claude.sh: not an exact version: '$version'" >&2
        exit 1
        ;;
esac
if ! printf '%s' "$version" |
    grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$'; then
    echo "install-claude.sh: not an exact semver version: '$version'" >&2
    exit 1
fi
[ -r "$VENDOR/install.sh" ] || {
    echo "install-claude.sh: vendored installer missing at $VENDOR/install.sh" >&2
    exit 1
}
# An exact but unpinned version is refused here, before the vendored installer
# runs and therefore before any download. `check` verifies the committed evidence,
# the signature and the signed manifest for exactly this version.
if ! python3 "$pin_dir/claude_pin.py" check --pin-dir "$pin_dir" --version "$version"; then
    echo "install-claude.sh: the committed Claude pin does not cover '$version'" >&2
    exit 1
fi

HOME=$prefix
export HOME
# The verified bytes are the selection; the vendored installer reads them through
# this directory and nowhere else.
CLAUDE_PIN_DIR="$pin_dir"
export CLAUDE_PIN_DIR

rc=0
bash "$VENDOR/install.sh" "$version" || rc=$?
if [ "$rc" -eq 0 ]; then
    exit 0
fi

# Only a classified transport failure with a fresh receipt may be softened, and
# only when AGENT_INSTALL_SOFT_FAIL is non-empty. Anything else stays hard.
if [ "$rc" -eq 75 ] && [ -n "$receipt" ] &&
    grep -Eq '^transport download [0-9]+$' "$receipt"; then
    if [ -n "${AGENT_INSTALL_SOFT_FAIL:-}" ]; then
        code=$(awk '{print $3}' "$receipt")
        # Recipe-owned partial artifacts only: the download cache and any
        # half-published command. Never a broader tree. Authorize BOTH targets
        # (exact production path, or a canonical fixture-root child) before
        # deleting either, so a mistaken seam cannot remove another real tree.
        cleanup_authorized "$prefix/.claude/downloads" "/opt/agent/.claude/downloads" || exit 1
        cleanup_authorized "$prefix/.local/bin/claude" "/opt/agent/.local/bin/claude" || exit 1
        rm -rf "$prefix/.claude/downloads"
        rm -f "$prefix/.local/bin/claude"
        mkdir -p "$status_dir"
        printf 'absent-transport %s\n' "$code" >"$status_dir/claude"
        echo "==> claude: ABSENT (transport download $code; soft-fail mode)"
        exit 0
    fi
fi

echo "install-claude.sh: claude installer failed (exit $rc)" >&2
exit 1
