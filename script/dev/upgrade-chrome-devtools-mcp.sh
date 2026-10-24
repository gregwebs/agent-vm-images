#!/usr/bin/env bash
# Maintainer-host maintenance for the Chrome example's pinned MCP selection (#41).
#
# This is the only writer of the pinned inputs: the build verifies them and the
# wrapper runs the installed copy. Everything else -- resolving the lock,
# fetching and verifying provenance, publishing five files as one transaction --
# lives here, on a host, never in CI.
#
# Status: 0 published or unchanged; 1 failed (sources unchanged or fully
# restored); 2 usage; 3 undecided or incomplete recovery ($work retained and
# printed); 128+N when a signal arrives before publication.
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
# shellcheck source=/dev/null
. "$ROOT/script/build/npm-pin.sh"
# shellcheck source=/dev/null
. "$ROOT/script/build/transactional-publish.sh"
# shellcheck source=/dev/null
. "$ROOT/script/build/chrome-mcp-provenance.sh"

# agent-vm launches `npx -y chrome-devtools-mcp@<this>` (crates/agent-vm/src/secrets.rs:1719),
# and the wrapper maps exactly that spelling. Publishing a different version
# makes the launcher's MCP entry refuse until agent-vm changes its argv (Q4).
LAUNCHER_ARGV_VERSION=1.0.1

usage() {
    cat >&2 <<'USAGE'
usage: upgrade-chrome-devtools-mcp.sh [VERSION]
Resolve, pin, verify and publish the Chrome example's chrome-devtools-mcp
selection on this host. VERSION defaults to the registry's `latest`; the pinned
release-age and trust policy are enforced by the resolve step.
USAGE
    exit 2
}

die() { echo "error: $*" >&2; exit 1; }

publishing=0 published=0 decided=0 status=0 keep_recovery=false
work='' cid='' backup=''
existed=() src=() dest=()

restore_sources() { # signal path only; mirrors publish_transactional inclusively
    local i rc=0
    for ((i = ${#dest[@]} - 1; i >= 0; i--)); do
        if [ "${existed[i]}" = yes ]; then
            # Only a complete backup carries its numeric name.
            if [ -f "$backup/$i" ]; then cp -p "$backup/$i" "${dest[i]}" || rc=3; else rc=3; fi
        else
            rm -f "${dest[i]}" || rc=3 # first adoption: the file did not exist
        fi
    done
    return "$rc"
}

on_signal() {
    local code=$1
    trap '' INT TERM HUP
    if [ "$publishing" -eq 1 ] && [ "$published" -eq 0 ] && [ ! -f "$backup/.complete" ]; then
        keep_recovery=true
        code=3
        echo "error: interrupted while taking backups; sources unchanged; recovery material retained at $work" >&2
    elif [ "$publishing" -eq 1 ] && [ "$published" -eq 0 ]; then
        keep_recovery=true
        if restore_sources; then
            echo "error: publication interrupted; sources restored (originally absent files removed); recovery material retained at $work" >&2
        else
            code=3
            echo "error: publication interrupted and restore incomplete; recovery material retained at $work" >&2
        fi
    elif [ "$publishing" -eq 1 ]; then
        keep_recovery=true
        code=3
        # status 3 is reported either way; a nonzero status gets one more restore.
        if [ "$status" -ne 0 ]; then restore_sources || :; fi
        echo "error: publication result undecided when the signal arrived; recovery material retained at $work" >&2
    elif [ "$decided" -eq 1 ]; then
        keep_recovery=true
        code=3
        echo "error: publication result undecided when the signal arrived; recovery material retained at $work" >&2
    fi
    exit "$code"
}

on_exit() {
    local rc=$?
    if [ -n "${cid:-}" ] && ! docker rm -f "$cid" >/dev/null 2>&1; then
        echo "error: could not remove resolve container $cid" >&2
        [ "$rc" -ne 0 ] || rc=1
    fi
    if [ "$keep_recovery" = false ]; then rm -rf "$work"; fi
    exit "$rc"
}

case ${1:-} in
    -h | --help) usage ;;
esac
[ "$#" -le 1 ] || usage
[ "${GITHUB_ACTIONS:-}" != true ] || die 'refusing GitHub Actions: this is maintainer-host maintenance only'

require_tools docker jq gh curl sha256sum sha512sum base64 od awk timeout cmp
timeout --version | grep -q 'GNU coreutils' || die 'GNU timeout (coreutils) is required'

EX=$ROOT/examples/layers/chrome-devtools
DIR=$EX/chrome-devtools-mcp
HELPER=$EX/chrome-devtools-mcp.sh
BUNDLE=$EX/chrome-devtools-mcp.sigstore.json
[ -f "$HELPER" ] || die "missing helper: $HELPER"
parent=$(awk 'NR == 1 && sub(/^ARG BASE_IMAGE=/, "") { print; exit }' "$EX/Dockerfile")
printf '%s' "$parent" | grep -Eqx 'ghcr\.io/gregwebs/agent-vm-standard@sha256:[0-9a-f]{64}' ||
    die 'Dockerfile line 1 must be ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:<64 hex>'
arch=$(docker version -f '{{.Server.Arch}}')
[ -n "$arch" ] || die 'cannot determine the Docker server architecture'

lock=$DIR/pnpm-lock.yaml
pins=$DIR/pnpm-inputs.sha256
present=0
for file in "$lock" "$pins" "$BUNDLE"; do
    if [ -e "$file" ] || [ -L "$file" ]; then present=$((present + 1)); fi
done
case "$present" in
    0) first_adoption=true ;;
    3) first_adoption=false ;;
    *) die 'partial pinned state; restore the committed files first' ;;
esac
if [ "$first_adoption" = false ]; then
    sh "$HELPER" check "$DIR" >/dev/null || die 'committed inputs do not pass check; restore them first'
fi

if [ "$#" -eq 1 ]; then
    version=$1
    printf '%s' "$version" | grep -Eqx "$(
        printf '%s' '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
    )" || die 'VERSION must be one canonical exact version'
else
    version=$(curl -fsS --max-time 60 "$CHROME_MCP_REGISTRY/chrome-devtools-mcp" | jq -er '."dist-tags".latest') ||
        die 'cannot read dist-tags.latest'
    printf '%s' "$version" | grep -Eqx '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$' ||
        die 'registry latest is not a canonical version'
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/chrome-mcp-upgrade.XXXXXX")
work=$(cd "$work" && pwd -P)
backup=$work/publish-backup
trap on_exit EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM
trap 'on_signal 129' HUP

# 7. Stage the proposal without a lock: the parent resolves it.
mkdir "$work/stage"
jq --arg v "$version" '.dependencies["chrome-devtools-mcp"] = $v' "$DIR/package.json" >"$work/stage/package.json"
cp "$DIR/pnpm-workspace.yaml" "$work/stage/"

# 8. Resolve inside the parent image (no bind mounts: colima shares only $HOME).
mkdir "$work/ctx"
cp -R "$work/stage" "$work/ctx/project"
cp "$HELPER" "$work/ctx/"
cid=$(timeout --kill-after=10 900 docker create --platform "linux/$arch" "$parent" \
    sh /work/chrome-devtools-mcp.sh resolve /work/project) || die "cannot create the resolve container from $parent"
docker cp "$work/ctx" "$cid:/work"
resolve_rc=0
timeout --kill-after=10 900 docker start -a "$cid" >"$work/resolve.log" 2>&1 || resolve_rc=$?
container_rc=$(docker inspect -f '{{.State.ExitCode}}' "$cid")
if [ "$resolve_rc" -ne 0 ] || [ "$container_rc" != 0 ]; then
    echo "error: resolve failed in $parent (status $resolve_rc, container $container_rc); log follows" >&2
    sed 's/^/  /' "$work/resolve.log" >&2
    exit 1
fi
docker cp "$cid:/work/project/pnpm-lock.yaml" "$work/stage/pnpm-lock.yaml"
docker rm -f "$cid" >/dev/null
cid=

# 9. Pins: the redirection is opened inside the stage, so it records the stage.
( cd "$work/stage" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml >pnpm-inputs.sha256 )
[ -s "$work/stage/pnpm-inputs.sha256" ] || die 'failed to write pins'

# 10. The proposal must pass the same check the build runs.
checked=$(sh "$HELPER" check "$work/stage") || die 'the resolved proposal does not pass check'
[ "$checked" = "$version" ] || die "resolved selection is $checked, expected $version"

# 11. Content identity plus provenance under the trusted identity.
hex=$(chrome_mcp_lock_integrity_hex "$work/stage/pnpm-lock.yaml")
chrome_mcp_fetch_tarball "$version" "$hex" "$work/pkg.tgz"
chrome_mcp_fetch_bundle "$version" "$work/sigstore.json"
chrome_mcp_verify_bundle "$work/pkg.tgz" "$work/sigstore.json" "$version" "$hex" "$work"

src=("$work/stage/package.json" "$work/stage/pnpm-lock.yaml" "$work/stage/pnpm-workspace.yaml" "$work/stage/pnpm-inputs.sha256" "$work/sigstore.json")
dest=("$DIR/package.json" "$lock" "$DIR/pnpm-workspace.yaml" "$pins" "$BUNDLE")

# 12. Nothing to publish when all five already match.
if [ "$first_adoption" = false ]; then
    unchanged=true
    for i in "${!src[@]}"; do
        cmp -s "${src[i]}" "${dest[i]}" || unchanged=false
    done
    if [ "$unchanged" = true ]; then
        echo "chrome-devtools-mcp@$version: unchanged (re-resolved and re-verified; nothing published)"
        exit 0
    fi
fi

# 13. Publish as one transaction, with signals handled by this caller.
for i in "${!dest[@]}"; do
    if [ -e "${dest[i]}" ] || [ -L "${dest[i]}" ]; then existed[i]=yes; else existed[i]=no; fi
done
pairs=()
for i in "${!src[@]}"; do pairs+=("${src[i]}" "${dest[i]}"); done
status=0
publishing=1
publish_transactional "$backup" "${pairs[@]}" || status=$?
published=1
case "$status" in
    0 | 1) keep_recovery=false ;;
    *)
        keep_recovery=true
        echo "error: recovery material retained at $work" >&2
        ;;
esac
decided=1
publishing=0
[ "$status" -eq 0 ] || exit "$status"

echo "chrome-devtools-mcp@$version: published $DIR inputs and $BUNDLE"
cat <<EOF

Review before committing:
  git diff -- examples/layers/chrome-devtools
  bash script/test/contracts.sh
  bash script/test/chrome-example.sh
EOF
if [ "$version" != "$LAUNCHER_ARGV_VERSION" ]; then
    cat <<EOF
warning: the wrapper will refuse agent-vm's npx -y chrome-devtools-mcp@$LAUNCHER_ARGV_VERSION until
agent-vm changes its argv; coordinate before merging.
EOF
fi
