#!/usr/bin/env bash
# Derive peer requirements without old overrides; publish only reviewed, runtime-proven inputs.
set -euo pipefail

usage() {
    echo 'usage: upgrade-dsh.sh [VERSION] [--pnpm EXACT] [--propose-cordis-overrides FILE | --cordis-overrides FILE]' >&2
}
fail() { echo "error: $*" >&2; exit 1; }
version='' pnpm_version='' proposal='' approved='' seen_pnpm=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --pnpm)
            [ "$#" -ge 2 ] && [ "$seen_pnpm" -eq 0 ] || { usage; exit 2; }
            case "$2" in --*) usage; exit 2 ;; esac
            pnpm_version=$2; seen_pnpm=1; shift 2 ;;
        --propose-cordis-overrides | --cordis-overrides)
            [ "$#" -ge 2 ] && [ -z "$proposal$approved" ] && [ -n "$2" ] || { usage; exit 2; }
            case "$2" in --*) usage; exit 2 ;; esac
            case "$1" in --propose-cordis-overrides) proposal=$2 ;; *) approved=$2 ;; esac
            shift 2 ;;
        --help | -h) usage; exit 0 ;;
        -*) usage; exit 2 ;;
        *) [ -z "$version" ] && [ -n "$1" ] || { usage; exit 2; }; version=$1; shift ;;
    esac
done
[ -z "${NODE_OPTIONS-}${NODE_PATH-}${BUN_OPTIONS-}" ] || fail 'runtime injection is unsupported'
# A developer upgrade contacts the real registry and must never run in CI. The
# fixture harness proves the same code path against a fake manager, so the
# refusal is scoped to the real manager rather than to every invocation.
if [ "${GITHUB_ACTIONS-}" = true ] && [ -z "${AGENT_VM_FIXTURE_PNPM-}" ]; then
    fail 'developer upgrade cannot run in GitHub Actions'
fi
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "$DIR/../../.." && pwd -P)"
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/npm-pin.sh"
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/dockerfile-label.sh"
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/transactional-publish.sh"
require_tools jq npm node python3 timeout docker curl git
python3 "$DIR/project.py" guard "$DIR"
base=$(jq -er .version "$REPO_ROOT/images/pnpm/pin.json")
[ "$base" = 11.11.0 ] || fail 'unsupported base manager'
[ -z "$pnpm_version" ] || [ "$pnpm_version" = "$base" ] || fail '--pnpm must equal the reviewed base pin'
[ "$(jq -r .packageManager "$DIR/package.json")" = "pnpm@$base" ] || fail 'packageManager disagrees with base'
# The contract directory executes policy, so project.py authorizes it before the
# first graph check. A caller-selected directory is authorized by the same rules
# as a build mount or a fixture copy.
CONTRACT=$(python3 "$DIR/project.py" contract-dir "$DIR") || fail 'contract directory rejected'
node "$CONTRACT/pnpm-lock.mjs" check dsh "$DIR"
current=$(jq -r '.dependencies["@deepseek-ai/dsh"]' "$DIR/package.json")
work=$(mktemp -d "${TMPDIR:-/tmp}/dsh-upgrade.XXXXXX")
work=$(cd "$work" && pwd -P)
keep_recovery=false
trap 'if [ "$keep_recovery" = false ]; then rm -rf "$work"; fi' EXIT
# The five publication destinations, in publication order. Every backup below is
# taken before the first copy, so a signal mid-publication can restore them all.
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
        [ -f "$publish_backup/$index" ] || continue
        cp -p "$publish_backup/$index" "${destinations[index]}" || status=3
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

# A fake is available only in an explicitly authorized disposable source repo.
# A developer upgrade of a checkout is not a fixture: it names only the scratch
# root that authorizes its staged prefixes, so the checkout's own contract
# directory stays authorized rather than being claimed as a fixture child.
if [ -n "${AGENT_VM_FIXTURE_PNPM-}" ]; then
    # The fixture harness exported AGENT_VM_FIXTURE_ROOT with its fake manager;
    # authorize() re-checks that this source repo really lives below it.
    python3 "$DIR/project.py" authorize "$DIR" >/dev/null
else
    mkdir "$work/pnpm"
    cp "$REPO_ROOT/images/pnpm/pin.json" "$work/pnpm/pin.json"
    export AGENT_VM_SCRATCH_ROOT=$work
fi
export HOME=$work/home XDG_CONFIG_HOME=$work/config XDG_CACHE_HOME=$work/cache
export XDG_DATA_HOME=$work/data XDG_STATE_HOME=$work/state TMPDIR=$work/tmp
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$TMPDIR"
export PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440 PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=true
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=false
export PNPM_CONFIG_TRUST_POLICY=no-downgrade PNPM_CONFIG_IGNORE_SCRIPTS=true

# npm is metadata-only. It cannot install, execute, or generate a lock here.
printf '{"private":true}\n' >"$work/package.json"
: >"$work/npmrc-user"
: >"$work/npmrc-global"
export npm_config_registry=https://registry.npmjs.org/
export npm_config_userconfig=$work/npmrc-user npm_config_globalconfig=$work/npmrc-global
export npm_config_ignore_scripts=true
version=$(cd "$work" && timeout --kill-after=5 60 bash -c '. "$1"; resolve_npm_version @deepseek-ai/dsh "$2"' _ \
    "$REPO_ROOT/script/build/npm-pin.sh" "$version")
(cd "$work" && timeout --kill-after=5 60 npm view "@deepseek-ai/dsh@$version" version dist peerDependencies --json) >"$work/metadata.json"
unset npm_config_registry npm_config_userconfig npm_config_globalconfig npm_config_ignore_scripts
echo "==> pinning @deepseek-ai/dsh: $current -> $version"
mkdir "$work/manager-prefix"
for input in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do
    cp "$DIR/$input" "$work/manager-prefix/$input"
done
node "$DIR/upgrade-project.mjs" tentative "$DIR" "$work/tentative" "$version"
if [ -z "${AGENT_VM_FIXTURE_PNPM-}" ]; then
    python3 "$DIR/host-manager.py" "$work/manager-prefix" >"$work/manager.json"
    export AGENT_VM_DSH_PNPM_PACKAGE AGENT_VM_DSH_PNPM_ARCHIVE AGENT_VM_DSH_NODE
    AGENT_VM_DSH_PNPM_PACKAGE=$(jq -r .package "$work/manager.json")
    AGENT_VM_DSH_PNPM_ARCHIVE=$(jq -r .archive "$work/manager.json")
    AGENT_VM_DSH_NODE=$(command -v node)
fi
manager=$(python3 "$DIR/project.py" manager "$work/manager-prefix")
MANAGER=()
while IFS= read -r word; do MANAGER+=("$word"); done <<EOF
$manager
EOF
timeout --kill-after=5 20 "${MANAGER[@]}" --version --pm-on-fail=error --config.update-notifier=false \
    --ignore-pnpmfile --registry=https://registry.npmjs.org/ >"$work/version"
[ "$(cat "$work/version")" = "$base" ] || fail 'manager is not authenticated pnpm 11.11.0'
POLICY=(--config.minimum-release-age=1440 --config.minimum-release-age-strict=true
    --config.minimum-release-age-ignore-missing-time=false --config.trust-policy=no-downgrade --ignore-scripts
    --ignore-pnpmfile --pm-on-fail=error --config.update-notifier=false --registry=https://registry.npmjs.org/
    --package-import-method=copy --store-dir "$work/store")
(cd "$work/tentative" && sh "$CONTRACT/run-pnpm.sh" "$work/tentative.ndjson" -- \
    "${MANAGER[@]}" install --lockfile-only --no-frozen-lockfile "${POLICY[@]}")
DERIVE=("$DIR/derive-cordis-overrides.mjs" "$work/tentative" --before "$DIR")
if [ -n "$proposal" ]; then
    # Existing/tracked files are never proposal destinations, even if unchanged.
    if git -C "$REPO_ROOT" ls-files --error-unmatch "$proposal" >/dev/null 2>&1; then fail 'proposal collides with tracked source'; fi
    node "${DERIVE[@]}" --propose "$proposal"
    exit 0
fi
if [ -n "$approved" ]; then DERIVE+=(--cordis-overrides "$approved"); fi
node "${DERIVE[@]}" >"$work/proof.json"
cat "$work/proof.json"
FINAL=(final "$DIR" "$work/final" "$work/proof.json")
if [ -n "$approved" ]; then FINAL+=("$approved"); fi
node "$DIR/upgrade-project.mjs" "${FINAL[@]}"
# Unchanged maps use the same ordinary refresh path as preparation, including
# comparison. Reviewed changed maps have their own explicit developer boundary.
if [ "$(jq '.delta | length' "$work/proof.json")" = 0 ]; then
    cp "$DIR/pnpm-inputs.sha256" "$work/final/pnpm-inputs.sha256"
    # The staged starting manifest must still match its committed pins.
    cp "$DIR/package.json" "$work/final/package.json"
    prepare_status=0
    sh "$DIR/prepare-lock.sh" "$work/final" "$version" "$base" --refresh-lock || prepare_status=$?
    if [ "$prepare_status" -eq 3 ]; then
        keep_recovery=true
        echo "error: recovery material retained at $work" >&2
    fi
    [ "$prepare_status" -eq 0 ] || exit "$prepare_status"
else
    (cd "$work/final" && sh "$CONTRACT/run-pnpm.sh" "$work/final.ndjson" -- \
        "${MANAGER[@]}" install --lockfile-only --no-frozen-lockfile "${POLICY[@]}")
fi
CHECK=(check-final "$DIR" "$work/final" "$work/proof.json")
if [ -n "$approved" ]; then CHECK+=("$approved"); fi
node "$DIR/upgrade-project.mjs" "${CHECK[@]}"
[ ! -e "$work/tentative/node_modules" ] && [ ! -e "$work/final/node_modules" ] || fail 'lock generation installed a tree'

# The real focused harness consumes the staged four files, never the tentative
# graph or the committed source. No publication can precede this cold gate.
bash "$REPO_ROOT/script/test/standard-node-consumers.sh" "${AGENT_VM_DSH_BASE_IMAGE:-agent-vm-base:local}" \
    --consumer dsh --version "$version" --inputs "$work/final"
cp "$REPO_ROOT/images/standard/Dockerfile" "$work/Dockerfile"
set_version_label "$work/Dockerfile" dsh AGENT_VERSION_DSH "$version"
status=0
# From here to the end of the publication a signal is rolled back inclusively.
publishing=1
publish_transactional "$publish_backup" \
    "$work/final/package.json" "$DIR/package.json" \
    "$work/final/pnpm-lock.yaml" "$DIR/pnpm-lock.yaml" \
    "$work/final/pnpm-workspace.yaml" "$DIR/pnpm-workspace.yaml" \
    "$work/final/pnpm-inputs.sha256" "$DIR/pnpm-inputs.sha256" \
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
echo "==> pinned @deepseek-ai/dsh@$version (base pnpm $base); four inputs and Dockerfile updated"
echo 'Next: bash script/test/contracts.sh; review graph/override delta and run both native architecture gates.'
