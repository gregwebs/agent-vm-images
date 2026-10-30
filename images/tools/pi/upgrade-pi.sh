#!/usr/bin/env bash
# Bump the pinned Pi version and publish the four reviewed inputs plus one
# central Dockerfile label in a single transaction. See images/tools/pi/README.md
# for usage and docs/standard-pnpm-migration.md for the graph evidence rules.
#
# Host-side only. The Dockerfile neither COPYs nor bind-mounts this file, so it
# never reaches the image. Run it as `bash upgrade-pi.sh`: like every file under
# images/tools/ it is committed 0644 and run with `bash` (the recipes bind-mount
# their scripts rather than COPYing them with the execute bit).
#
# Two seams stay separate on purpose:
#   * `script/build/npm-pin.sh` resolves a developer dist-tag. That is a QUERY
#     only, and it is the last npm call this path makes (PR 4 retires the seam);
#   * the graph itself is resolved by the authenticated base pnpm 11.11.0 through
#     `prepare-lock.sh`, which owns the candidate comparison, the byte/graph
#     policy and the four-input transaction.
# There is no `npm view` refill anywhere: the pnpm lock is the only source of
# integrity, and supplementation is the converter's authenticated business.
#
# The four inputs and the central LABEL are published together: a run that
# succeeds leaves a consistent source tree, and a run that fails leaves the old
# generation (status 1) or retained recovery material (status 3).
set -euo pipefail

PACKAGE=@earendil-works/pi-coding-agent

usage() {
    cat <<EOF
Usage: bash images/tools/pi/upgrade-pi.sh [VERSION]

Pin ${PACKAGE} to VERSION, an exact version or a dist-tag (default: the
registry's \`latest\` dist-tag), and publish package.json, pnpm-lock.yaml,
pnpm-workspace.yaml, pnpm-inputs.sha256 and the central Dockerfile LABEL.
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

[ -z "${NODE_OPTIONS-}${NODE_PATH-}${BUN_OPTIONS-}" ] || {
    echo 'error: runtime injection is unsupported' >&2
    exit 1
}

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$DIR/../../.." && pwd -P)"
# Shared prerequisite check and version resolution; see the helper's header for
# why the copy is not inlined. source=/dev/null: shellcheck cannot follow a path
# built from $DIR, and npm-pin.sh is linted on its own.
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/npm-pin.sh"
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/dockerfile-label.sh"
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/transactional-publish.sh"

# A developer upgrade contacts the real registry and must never run in CI. The
# fixture harness proves the same code path against a fake manager, so the
# refusal is scoped to the real manager rather than to every invocation.
if [ "${GITHUB_ACTIONS-}" = true ] && [ -z "${AGENT_VM_FIXTURE_PNPM-}" ]; then
    echo 'error: the developer Pi upgrade cannot run in GitHub Actions' >&2
    exit 1
fi

require_tools jq node python3
PROJECT="$REPO_ROOT/images/tools/pnpm-project.py"
[ "$(jq -r .packageManager "$DIR/package.json")" = 'pnpm@11.11.0' ] || {
    echo 'error: packageManager disagrees with the reviewed base pin' >&2
    exit 1
}

current=$(jq -r --arg p "$PACKAGE" '.dependencies[$p]' "$DIR/package.json")

version_arg="${1:-}"
version=$(resolve_npm_version "$PACKAGE" "$version_arg")
if [ -z "$version_arg" ]; then
    echo "==> latest ${PACKAGE} is ${version}"
fi

echo "==> pinning ${PACKAGE}: ${current} -> ${version}"

work=$(mktemp -d "${TMPDIR:-/tmp}/pi-upgrade.XXXXXX")
work=$(cd "$work" && pwd -P)
keep_recovery=false
trap 'if [ "$keep_recovery" = false ]; then rm -rf "$work"; fi' EXIT
# The four inputs and the central Dockerfile LABEL, in publication order. Every
# backup is taken before the first copy, so a signal mid-publication can restore
# them all.
publish_backup=$work/publish-backup
destinations=("$DIR/package.json" "$DIR/pnpm-lock.yaml" "$DIR/pnpm-workspace.yaml" \
    "$DIR/pnpm-inputs.sha256" "$REPO_ROOT/images/standard/Dockerfile")
publishing=0
published=0
decided=0
restore_published() {
    local index status=0
    for ((index = ${#destinations[@]} - 1; index >= 0; index--)); do
        # Only a completed backup carries its numeric name (an interrupted copy is
        # still under .tmpN), so a partial file can never be restored.
        if [ -f "$publish_backup/$index" ]; then
            cp -p "$publish_backup/$index" "${destinations[index]}" || status=3
        elif [ -f "$publish_backup/.complete" ]; then
            # Completed backups and no backup for this destination means the file
            # did not exist before publication: restoring it means removing the
            # copy the transaction made.
            rm -f "${destinations[index]}" || status=3
        fi
    done
    return "$status"
}
on_signal() {
    local signum=$1
    trap '' INT TERM HUP
    if [ "$publishing" -eq 1 ] && [ "$published" -eq 0 ] && [ ! -f "$publish_backup/.complete" ]; then
        # The signal landed in the backup phase, so no source has been written and
        # no complete backup set exists: restoring is unnecessary and a partial
        # backup must never be copied over an untouched source. The staged graph
        # and every completed backup stay in $work, and status 3 reports that the
        # recovery material is incomplete.
        keep_recovery=true
        signum=3
        echo "error: publication interrupted while taking backups; sources unchanged; recovery material retained at $work" >&2
    elif [ "$publishing" -eq 1 ]; then
        # A signal must never delete the backups or leave half-published sources.
        keep_recovery=true
        # Before the transaction returns the copies are live, so they are
        # always restored; afterwards `status` is the publication result and only
        # a non-zero one means the sources were not (completely) restored.
        if [ "$published" -eq 0 ] || [ "$status" -ne 0 ]; then
            restore_published || signum=3
        fi
        if [ "$published" -eq 1 ]; then
            # The copies finished but the recovery decision had not run: the
            # sources and the retained backups describe the same publication, and
            # only status 3 plus $work can carry that undecided outcome to a caller.
            signum=3
            echo "error: publication result undecided when the signal arrived; recovery material retained at $work" >&2
        else
            echo "error: publication interrupted; sources restored from the retained backups at $work" >&2
        fi
    elif [ "$decided" -eq 1 ]; then
        # The outcome is decided but this script has not exited yet. Neither the
        # success message nor the decided failure status is safe to report, so the
        # signal keeps the whole recovery material and reports status 3.
        keep_recovery=true
        signum=3
        echo "error: publication result undecided when the signal arrived; recovery material retained at $work" >&2
    fi
    exit "$signum"
}
trap 'on_signal 130' INT
trap 'on_signal 143' TERM
trap 'on_signal 129' HUP

mkdir -p "$work/pi" "$work/pnpm"
for input in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do
    cp -p "$DIR/$input" "$work/pi/$input"
done
# The staged fixture root carries the reviewed base pin, so the shared
# authorization can recognize the staged prefix as a fixture child.
cp -p "$REPO_ROOT/images/pnpm/pin.json" "$work/pnpm/pin.json"
cp -p "$REPO_ROOT/images/standard/Dockerfile" "$work/Dockerfile"

export AGENT_VM_FIXTURE_ROOT=$work
export AGENT_VM_PI_WORK_DIR=$work
if [ -z "${AGENT_VM_CONTRACT_DIR-}" ]; then
    context=$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native) || {
        echo 'error: cannot build the native recipe-contract helper' >&2
        exit 1
    }
    export AGENT_VM_CONTRACT_DIR="$context/out"
fi
CONTRACT=$(python3 "$PROJECT" --consumer pi contract-dir "$work/pi") || exit 1
"$CONTRACT/recipe-contract" pnpm-lock check pi "$work/pi"

# A real developer run acquires the authenticated stock 11.11.0 distribution
# (never host pnpm); the fixture harness supplies its own authorized fake.
if [ -z "${AGENT_VM_FIXTURE_PNPM-}" ]; then
    manager_json=$(python3 "$REPO_ROOT/images/tools/pnpm-host-manager.py" --consumer pi "$work/pi") || {
        echo 'error: cannot acquire the authenticated 11.11.0 distribution' >&2
        exit 1
    }
    AGENT_VM_PI_PNPM_PACKAGE=$(printf '%s' "$manager_json" | jq -r .package)
    AGENT_VM_PI_PNPM_ARCHIVE=$(printf '%s' "$manager_json" | jq -r .archive)
    AGENT_VM_PI_NODE=$(command -v node)
    export AGENT_VM_PI_PNPM_PACKAGE AGENT_VM_PI_PNPM_ARCHIVE AGENT_VM_PI_NODE
fi

echo "==> resolving the pnpm graph for ${version}"
status=0
sh "$DIR/prepare-lock.sh" "$work/pi" "$version" --refresh-lock || status=$?
case $status in
    0) ;;
    3)
        keep_recovery=true
        echo "error: preparation rollback is incomplete; recovery material retained at $work" >&2
        exit 3
        ;;
    *)
        echo "error: preparation failed; the committed inputs are unchanged" >&2
        exit 1
        ;;
esac

staged=$(jq -r --arg p "$PACKAGE" '.dependencies[$p]' "$work/pi/package.json")
[ "$staged" = "$version" ] || {
    echo "error: staged manifest pins ${staged}, expected ${version}" >&2
    exit 1
}
"$CONTRACT/recipe-contract" pnpm-lock check pi "$work/pi" || {
    echo 'error: the staged inputs failed pinned-input validation' >&2
    exit 1
}

set_version_label "$work/Dockerfile" pi AGENT_VERSION_PI "$version"
grep -Fq "org.agent-vm.version.pi=\"\${AGENT_VERSION_PI:-${version}}\"" "$work/Dockerfile" || {
    echo "error: the staged Dockerfile pi label does not mirror ${version}" >&2
    exit 1
}

status=0
# From here to the end of the publication a signal is rolled back inclusively.
publishing=1
publish_transactional "$publish_backup" \
    "$work/pi/package.json" "$DIR/package.json" \
    "$work/pi/pnpm-lock.yaml" "$DIR/pnpm-lock.yaml" \
    "$work/pi/pnpm-workspace.yaml" "$DIR/pnpm-workspace.yaml" \
    "$work/pi/pnpm-inputs.sha256" "$DIR/pnpm-inputs.sha256" \
    "$work/Dockerfile" "$REPO_ROOT/images/standard/Dockerfile" || status=$?
# The transaction has returned. Until the recovery decision below runs, a signal
# must not be handled as a mid-copy one nor be allowed to delete $work.
published=1
case "$status" in
    0 | 1) keep_recovery=false ;;
    3) keep_recovery=true; echo "error: recovery material retained at $work" >&2 ;;
    *) keep_recovery=true ;;
esac
# The outcome is fixed from here on: a signal before the exit still may not claim
# it, so the handler treats this window as undecided (status 3, $work retained).
decided=1
# Only a decided outcome may drop the recovery material or expose the sources to
# a later signal's rollback.
publishing=0
[ "$status" -eq 0 ] || exit "$status"

cat <<EOF
==> pinned ${PACKAGE}@${version}; four inputs and the Dockerfile LABEL updated
Next:
  bash script/test/contracts.sh
  bash images/build.sh standard
  bash script/test/standard-image.sh agent-vm-base:local agent-vm-standard:local
  Review the central Dockerfile and the owning four-input diff; see CONTRIBUTING.md.
EOF
