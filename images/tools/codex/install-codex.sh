#!/bin/sh
# Owning install hook for the codex layer: validate the exact `rust-v` slot,
# require the committed content pin for it, run the pinned vendored installer,
# and (only) soften a positively classified transport failure by removing the
# recipe's own partial artifacts and writing an absence record. Bind-mounted,
# never COPYed, so it stays out of the shipped image.
#
# The vendored installer at $AGENT_VM_VENDOR_DIR/install.sh is OpenAI's published
# installer for the rust-v0.159.3 release with the committed-selection,
# classified-download and latest-branch-removal patches in
# images/tools/codex/vendor/install.patch. Running it as a child (not through
# run-install's own interpreter) is what lets this hook inspect the classified
# 75 and decide.
set -eu

VENDOR="${AGENT_VM_VENDOR_DIR:-/opt/agent-vm/recipe-vendor}"
# Production prefix is the installer's $HOME. The seam lets the host black-box
# test point at a temp root; production always uses the default.
prefix="${AGENT_VM_CODEX_PREFIX:-/opt/agent}"
version="${AGENT_VM_VERSION_CODEX:?install-codex.sh needs AGENT_VM_VERSION_CODEX}"
receipt="${AGENT_VM_TRANSPORT_RECEIPT:-}"
status_dir="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"

# The committed closure (pin.json, its evidence and codex_pin.py) lives in the
# codex source directory. Production mounts it beside this hook; the explicit
# seam is what keeps a hook-only mount from silently pinning nothing. There is no
# ambient override for the pin contents, the expected hash, the URL or the trust
# policy: the directory is the whole input.
if [ -n "${AGENT_VM_CODEX_SOURCE_DIR:-}" ]; then
    pin_dir="$AGENT_VM_CODEX_SOURCE_DIR"
else
    case "$0" in
        */*) pin_dir="${0%/*}" ;;
        *) pin_dir="." ;;
    esac
fi
[ -r "$pin_dir/pin.json" ] && [ -r "$pin_dir/codex_pin.py" ] || {
    echo "install-codex.sh: no committed Codex pin at $pin_dir" >&2
    exit 1
}

# The codex contract accepts only the canonical `rust-v` spelling: no bare
# version, no `v` alias, no dist-tag, range or URL. The vendored installer
# normalizes the prefix and installs the exact numeric release. The grammar is
# canonical semver for its supported alpha/beta subset: leading-zero numeric
# components and leading-zero prerelease identifiers are NOT exact versions.
case "$version" in
    rust-v*) numeric="${version#rust-v}" ;;
    *)
        echo "install-codex.sh: expected rust-v<version>, got '$version'" >&2
        exit 1
        ;;
esac
# Reject an embedded newline first: `grep` is line-oriented, so a multi-line
# range whose first line looks exact would otherwise reach the installer.
if [ "$(printf '%s' "$numeric" | wc -l | tr -d ' ')" -ne 0 ]; then
    echo "install-codex.sh: not an exact Codex version: '$version'" >&2
    exit 1
fi
if ! printf '%s' "$numeric" |
    grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-alpha(\.(0|[1-9][0-9]*)){0,2}|-beta(\.(0|[1-9][0-9]*))?)?$'; then
    echo "install-codex.sh: not an exact Codex version: '$version'" >&2
    exit 1
fi
[ -r "$VENDOR/install.sh" ] || {
    echo "install-codex.sh: vendored installer missing at $VENDOR/install.sh" >&2
    exit 1
}

# Refuse an exact-but-unpinned version here, before the vendor runs at all. The
# vendored installer repeats this through `select`; the hook does not rely on
# that, because a reviewable refusal at the boundary is what keeps the pin
# authority from depending on the installer's own control flow.
python3 "$pin_dir/codex_pin.py" check --pin-dir "$pin_dir" --release "$version" || {
    echo "install-codex.sh: the committed Codex pin does not cover '$version'" >&2
    exit 1
}

# A build is a fresh, non-interactive install into an empty prefix. Clearing the
# inherited selection/update guards keeps a stale ambient value from turning it
# into a conditional updater that can exit before the content check.
unset CODEX_INSTALL_DEFER_SELECTION CODEX_INSTALL_IF_LATEST CODEX_INSTALL_IF_CURRENT \
    CODEX_UPDATE_FROM_RELEASE

HOME=$prefix
export HOME
CODEX_PIN_DIR="$pin_dir"
CODEX_RELEASE="$version"
CODEX_NON_INTERACTIVE=1
CODEX_HOME="$prefix/.codex"
CODEX_INSTALL_DIR="$prefix/.local/bin"
CODEX_INSTALL_DAEMON_ONLY=0
export CODEX_PIN_DIR CODEX_RELEASE CODEX_NON_INTERACTIVE CODEX_HOME CODEX_INSTALL_DIR \
    CODEX_INSTALL_DAEMON_ONLY

rc=0
sh "$VENDOR/install.sh" || rc=$?
if [ "$rc" -eq 0 ]; then
    # Repair recipe-owned modes so any uid can traverse and execute. The audit in
    # verify-codex.sh is the gate; this is the repair the contract requires.
    if [ -d "$prefix/.codex" ]; then chmod -R a+rX "$prefix/.codex" || true; fi
    if [ -d "$prefix/.local/bin" ]; then chmod a+rx "$prefix/.local/bin" || true; fi
    exit 0
fi

# Only a classified transport failure with a fresh receipt may be softened, and
# only when AGENT_INSTALL_SOFT_FAIL is non-empty. Anything else stays hard.
if [ "$rc" -eq 75 ] && [ -n "$receipt" ] &&
    grep -Eq '^transport download [0-9]+$' "$receipt"; then
    if [ -n "${AGENT_INSTALL_SOFT_FAIL:-}" ]; then
        code=$(awk '{print $3}' "$receipt")
        # Recipe-owned partial artifacts only: the half-published command. The
        # vendored installer's own EXIT trap already removed its download temp
        # dir before propagating a failure.
        rm -f "$prefix/.local/bin/codex"
        mkdir -p "$status_dir"
        printf 'absent-transport %s\n' "$code" >"$status_dir/codex"
        echo "==> codex: ABSENT (transport download $code; soft-fail mode)"
        exit 0
    fi
fi

echo "install-codex.sh: codex installer failed (exit $rc)" >&2
exit 1
