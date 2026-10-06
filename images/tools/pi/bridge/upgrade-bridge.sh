#!/usr/bin/env bash
# Bump the pinned pi-claude-bridge version: rewrite package.json's pin, update
# package-lock.json, and move the standard Dockerfile's LABEL fallback in
# lockstep. See images/tools/README.md ("The bridge packages") and
# images/tools/pi/README.md.
#
# Host-side only. The Dockerfile COPYs only package.json and package-lock.json
# from this directory, so this file never reaches the image. Run it as
# `bash upgrade-bridge.sh`: like every file under images/tools/ it is committed
# 0644 and run with `bash` (the recipes bind-mount their scripts rather than
# COPYing them with the execute bit).
#
# The DEVELOPER-only path: it resolves a dist-tag at the host seam, then calls
# the shared bridge prepare-lock.sh with `--refresh-lock`, which regenerates the
# project with the mandatory `--legacy-peer-deps` (without it npm installs the
# bridge's @earendil-works peers, a second version-skewed Pi). Everything is
# staged in a scratch directory and only copied over the committed files once
# every check below passes.

set -euo pipefail

PACKAGE=pi-claude-bridge

usage() {
    cat <<EOF
Usage: bash images/tools/pi/bridge/upgrade-bridge.sh [VERSION]

Pin ${PACKAGE} to VERSION, an exact version or a dist-tag (default: the
registry's \`latest\` dist-tag), and update package.json + package-lock.json
beside this script.
EOF
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac
if [ $# -gt 1 ]; then
    usage >&2
    exit 2
fi

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DIR/../../../.." && pwd)"
# Shared prerequisite check and version resolution; see the helper's header for
# why the copy is not inlined. source=/dev/null: shellcheck cannot follow a path
# built from $DIR, and npm-pin.sh is linted on its own.
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/npm-pin.sh"
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/dockerfile-label.sh"
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/transactional-publish.sh"

require_tools jq npm

current=$(jq -r --arg p "$PACKAGE" '.dependencies[$p]' "$DIR/package.json")

version_arg="${1:-}"
version=$(resolve_npm_version "$PACKAGE" "$version_arg")
if [ -z "$version_arg" ]; then
    echo "==> latest ${PACKAGE} is ${version}"
fi

echo "==> pinning ${PACKAGE}: ${current} -> ${version}"

work=$(mktemp -d)
keep_recovery=false
trap 'if [ "$keep_recovery" = false ]; then rm -rf "$work"; fi' EXIT

mkdir -p "$work/bridge"
cp "$DIR/package.json" "$work/bridge/package.json"
cp "$DIR/package-lock.json" "$work/bridge/package-lock.json"
cp "$REPO_ROOT/images/standard/Dockerfile" "$work/Dockerfile"

echo "==> updating package-lock.json"
AGENT_VM_FIXTURE_ROOT="$work" sh "$DIR/prepare-lock.sh" "$work/bridge" "$version" --refresh-lock

staged=$(jq -r --arg p "$PACKAGE" '.dependencies[$p]' "$work/bridge/package.json")
[ "$staged" = "$version" ] || {
    echo "error: staged manifest pins ${staged}, expected ${version}" >&2
    exit 1
}

set_version_label "$work/Dockerfile" pi-claude-bridge AGENT_VERSION_PI_CLAUDE_BRIDGE "$version"
grep -Fq "org.agent-vm.version.pi-claude-bridge=\"\${AGENT_VERSION_PI_CLAUDE_BRIDGE:-${version}}\"" "$work/Dockerfile" || {
    echo "error: the staged Dockerfile bridge label does not mirror ${version}" >&2
    exit 1
}

if publish_transactional "$work/publish-backup" \
    "$work/bridge/package.json" "$DIR/package.json" \
    "$work/bridge/package-lock.json" "$DIR/package-lock.json" \
    "$work/Dockerfile" "$REPO_ROOT/images/standard/Dockerfile"; then
    status=0
else
    status=$?
fi
if [ "$status" -ne 0 ]; then
    if [ "$status" -eq 3 ]; then
        keep_recovery=true
        echo "error: recovery material retained at $work" >&2
    fi
    exit "$status"
fi

# The bridge declares the image's own Pi as a peer. --legacy-peer-deps is
# mandatory on the install and hides a mismatch, so surface the new range here
# for the reviewer -- information only: the range grammar is npm's, so we do not
# evaluate it, and a mismatch is not a failure.
pi_pin=$(jq -r '.dependencies["@earendil-works/pi-coding-agent"]' "$DIR/../package.json")
peer=$(jq -r '.packages["node_modules/pi-claude-bridge"].peerDependencies["@earendil-works/pi-coding-agent"] // empty' \
    "$DIR/package-lock.json")

cat <<EOF
==> pinned ${PACKAGE}@${version}; parent Dockerfile LABEL updated
==> peer @earendil-works/pi-coding-agent: ${peer:-<none>} (image pins ${pi_pin}; check compatibility)
Next:
  bash script/test/contracts.sh
  bash images/build.sh standard
  bash script/test/standard-image.sh agent-vm-base:local agent-vm-standard:local
  Review the central Dockerfile and owning manifest/lock diff; see CONTRIBUTING.md.
EOF
