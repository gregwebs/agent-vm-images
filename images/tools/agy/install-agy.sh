#!/bin/sh
# Owning install hook for the agy layer: validate the exact version slot, require
# the committed agy pin for it, and hand the install to the shared release-artifact
# installer. Bind-mounted, never COPYed, so it stays out of the shipped image.
#
# agy publishes no signature, so this hook adds no verification of its own: the
# installer is the one place that re-checks the committed corroboration against the
# pin and accepts only the committed digests. What the hook owns is the boundary
# refusal — an exact-but-unpinned version stops here, before any fetch, so the pin
# stays the only authority on what may be installed.
set -eu

version="${AGENT_VM_VERSION_AGY:?install-agy.sh needs AGENT_VM_VERSION_AGY}"
source_dir="${AGENT_VM_AGY_SOURCE_DIR:?install-agy.sh needs AGENT_VM_AGY_SOURCE_DIR}"
artifact_source="${ARTIFACT_SOURCE_DIR:-/tmp/artifact-source}"
arch="${AGENT_VM_AGY_ARCH:?install-agy.sh needs AGENT_VM_AGY_ARCH}"

# Reject an embedded newline first: `grep` is line-oriented, so a multi-line value
# whose first line looks exact would otherwise reach the pin check.
if [ "$(printf '%s' "$version" | wc -l | tr -d ' ')" -ne 0 ]; then
    echo "install-agy.sh: not an exact agy version: '$version'" >&2
    exit 1
fi
if ! printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'; then
    echo "install-agy.sh: not an exact agy version: '$version'" >&2
    exit 1
fi

case "$arch" in
    amd64 | arm64) ;;
    *)
        echo "install-agy.sh: unsupported architecture '$arch'" >&2
        exit 1
        ;;
esac

[ -r "$source_dir/pin.json" ] || {
    echo "install-agy.sh: no committed agy pin at $source_dir/pin.json" >&2
    exit 1
}

# The pin is read through the shared validator, so a pin file the installer would
# reject cannot select a version this hook then claims is pinned.
pinned=$(PYTHONDONTWRITEBYTECODE=1 python3 "$artifact_source/artifacts/read-pin-version.py" \
    "$source_dir/pin.json") || {
    echo "install-agy.sh: the committed agy pin is not readable" >&2
    exit 1
}
if [ "$pinned" != "$version" ]; then
    echo "install-agy.sh: the committed agy pin selects $pinned, not $version" >&2
    exit 1
fi

# No soft-fail branch: agy is required and has no absence producer.
exec bash "$artifact_source/install-artifact.sh" agy "$source_dir/pin.json" "$arch"
