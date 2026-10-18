#!/bin/sh
# Empty/equal inputs are immutable; changed dsh selections use a staged graph.
# --refresh-lock is developer-only and never bypasses policy or exact peers.
#
# Usage: prepare-lock.sh PREFIX DSH_VERSION PNPM_VERSION [--refresh-lock]
#
# PREFIX owns the four reviewed inputs. Each version is exact or empty (empty
# keeps the committed selection). PNPM_VERSION accepts only empty or the base
# pin, because the base image owns the manager selection and launcher.
#
# Empty or equal selections start no package manager at all: they re-check the
# committed bytes, structure and effective peers. A changed selection stages the
# old lock plus the changed manifest, resolves a fresh graph under the owning
# policy, validates structure, effective peers and the update rules, writes
# build-local pins, and only then publishes all four files transactionally.
set -eu

usage() {
    echo 'usage: prepare-lock.sh PREFIX DSH_VERSION PNPM_VERSION [--refresh-lock]' >&2
}

fail() {
    echo "prepare-lock.sh: $*" >&2
    exit 1
}

# A Node preload would make every later check untrustworthy, so refuse injected
# runtime options before any Node process starts.
[ -z "${NODE_OPTIONS-}" ] || fail 'NODE_OPTIONS is not supported'
[ -z "${NODE_PATH-}" ] || fail 'NODE_PATH is not supported'
[ -z "${BUN_OPTIONS-}" ] || fail 'BUN_OPTIONS is not supported'

if [ "$#" -lt 3 ]; then
    usage
    exit 2
fi
PREFIX=$1
DSH_VERSION=$2
PNPM_VERSION=$3
shift 3

refresh=0
while [ "$#" -gt 0 ]; do
    case $1 in
        --refresh-lock)
            [ "$refresh" -eq 0 ] || { usage; exit 2; }
            refresh=1
            ;;
        *)
            usage
            exit 2
            ;;
    esac
    shift
done

self_dir=$(cd "$(dirname "$0")" && pwd -P)
PROJECT=$self_dir/project.py

# Canonical semver only: a multi-line range or a leading-zero component must not
# reach the manager as if it were an exact selection.
exact_version() {
    [ "$(printf '%s' "$1" | wc -l | tr -d ' ')" -eq 0 ] || return 1
    printf '%s' "$1" |
        grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}

for slot in "$DSH_VERSION" "$PNPM_VERSION"; do
    [ -n "$slot" ] || continue
    exact_version "$slot" || fail "not an exact version: '$slot'"
done

# Authorization first: the prefix, its inputs, the base pin and the scratch
# parent are all validated before anything is created or written.
facts=$(python3 "$PROJECT" authorize "$PREFIX") || exit 1
base_pin=$(printf '%s\n' "$facts" | jq -er '.basePin') || fail 'authorization returned no base pin'
# Ambient configuration is rejected before HOME is replaced; caller HOME/XDG is
# deliberately ignored because the manager never sees it.
python3 "$PROJECT" guard "$PREFIX" || exit 1

PNPM_SOURCE=${PNPM_SOURCE_DIR:-$self_dir/../../pnpm}
[ -f "$PNPM_SOURCE/read-pin-version.py" ] || fail "cannot locate the reviewed pnpm sources at $PNPM_SOURCE"
base_pnpm=$(python3 "$PNPM_SOURCE/read-pin-version.py" "$base_pin") || fail 'cannot read the base pin'
[ "$base_pnpm" = '11.11.0' ] || fail "unsupported base manager selection: $base_pnpm"
[ -z "$PNPM_VERSION" ] || [ "$PNPM_VERSION" = "$base_pnpm" ] ||
    fail 'pnpm compatibility selection disagrees with base pin'

manifest="$PREFIX/package.json"
package_manager=$(jq -er '.packageManager' "$manifest") || fail 'committed manifest has no packageManager'
[ "$package_manager" = "pnpm@$base_pnpm" ] ||
    fail "manifest packageManager $package_manager disagrees with base pin $base_pnpm"
committed_dsh=$(jq -er '.dependencies["@deepseek-ai/dsh"]' "$manifest") || fail 'committed manifest selects no dsh version'
effective_dsh=${DSH_VERSION:-$committed_dsh}

# The contract directory executes policy (its graph module, its manager envelope),
# so project.py authorizes it before any of it is spawned. The build mount is
# named by AGENT_VM_CONTRACT_DIR; a host checkout keeps its own tracked copy.
CONTRACT=$(python3 "$PROJECT" contract-dir "$PREFIX") || exit 1
[ -f "$CONTRACT/pnpm-lock.mjs" ] || fail "cannot locate the graph module in $CONTRACT"

changed=0
[ "$effective_dsh" = "$committed_dsh" ] || changed=1
[ "$refresh" -eq 0 ] || changed=1

if [ "$changed" -eq 0 ]; then
    # Comparing the committed project with itself proves all four bytes, the
    # structure, the effective peers and the base assertions without a manager.
    node "$self_dir/check-lock-update.js" "$PREFIX" "$PREFIX" '' || exit 1
    echo "prepare-lock.sh: dsh $effective_dsh unchanged; reviewed inputs reused"
    exit 0
fi

work=$(python3 "$PROJECT" scratch "$PREFIX") || exit 1
stage=$work/prefix
# Status 3 (incomplete rollback) must retain the whole scratch: removing the
# staged graph or the recovery material would destroy the evidence.
keep=0
cleanup() {
    status=$?
    if [ "$keep" -eq 0 ] &&
        ! python3 "$PROJECT" cleanup "$work" >/dev/null 2>&1; then
        echo 'prepare-lock.sh: invocation scratch cleanup failed' >&2
        [ "$status" -ne 0 ] || status=1
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

mkdir "$stage"
cp "$PREFIX/pnpm-lock.yaml" "$PREFIX/pnpm-workspace.yaml" "$PREFIX/pnpm-inputs.sha256" "$stage/"
jq --arg d "$effective_dsh" '.dependencies["@deepseek-ai/dsh"] = $d' "$manifest" >"$stage/package.json" ||
    fail 'cannot stage the changed manifest'
# The workspace is staged even when byte-identical: resolution must run against
# the reviewed workspace, never an ambient one.

# Every manager call owns private state so no caller or image cache is read or
# written, and the five policy defaults are explicit rather than inherited.
HOME=$work/home
XDG_CONFIG_HOME=$work/config
XDG_CACHE_HOME=$work/cache
XDG_DATA_HOME=$work/data
XDG_STATE_HOME=$work/state
TMPDIR=$work/tmp
export HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME TMPDIR
export PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=true
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=false
export PNPM_CONFIG_TRUST_POLICY=no-downgrade
export PNPM_CONFIG_IGNORE_SCRIPTS=true
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$TMPDIR"

# project.py owns manager selection: the fixed base launcher, or an authorized
# fixture fake. Reading its argv back line by line keeps spaces intact.
manager=$(python3 "$PROJECT" manager "$PREFIX") || exit 1
set --
while IFS= read -r word; do
    set -- "$@" "$word"
done <<EOF
$manager
EOF
[ "$#" -ge 1 ] || fail 'authorization returned no manager argv'

# Every manager call, including the identity check, carries the policy flags: a
# caller pnpmfile or an ambient registry must never influence even `--version`.
timeout --kill-after=5 20 "$@" --version --pm-on-fail=error --config.update-notifier=false \
    --ignore-pnpmfile --registry=https://registry.npmjs.org/ >"$work/version.out" 2>"$work/version.err" || fail 'manager version check failed'
[ "$(cat "$work/version.out")" = "$base_pnpm" ] || fail "manager is not the base pin $base_pnpm"

echo "prepare-lock.sh: dsh $committed_dsh -> $effective_dsh via $*"
status=0
(cd "$stage" && sh "$CONTRACT/run-pnpm.sh" "$work/resolve.ndjson" -- \
    "$@" install --lockfile-only --no-frozen-lockfile \
    --config.minimum-release-age=1440 \
    --config.minimum-release-age-strict=true \
    --config.minimum-release-age-ignore-missing-time=false \
    --config.trust-policy=no-downgrade --ignore-scripts \
    --ignore-pnpmfile --pm-on-fail=error --config.update-notifier=false \
    --registry=https://registry.npmjs.org/ \
    --package-import-method=copy --store-dir "$work/store") || status=$?
# A signal reaches the owning shell unchanged instead of becoming a plain failure.
case $status in
    0) ;;
    130 | 143 | 129) exit "$status" ;;
    *) fail 'staged resolution failed' ;;
esac
# Lock-only generation must not create an installed tree in the staged project.
[ ! -e "$stage/node_modules" ] || fail 'lock-only generation created node_modules'

# Pins are written only after the staged structure and every effective exact
# peer validate; the following check re-reads them as reviewed bytes.
node "$CONTRACT/pnpm-lock.mjs" pin dsh "$stage" || fail 'staged graph failed structural validation'
node "$CONTRACT/pnpm-lock.mjs" check dsh "$stage" || fail 'staged graph failed pinned-input validation'
if [ "$refresh" -eq 1 ]; then
    node "$self_dir/check-lock-update.js" "$PREFIX" "$stage" "$DSH_VERSION" --refresh-lock || exit 1
else
    node "$self_dir/check-lock-update.js" "$PREFIX" "$stage" "$DSH_VERSION" || exit 1
fi

status=0
python3 "$PROJECT" publish "$stage" "$PREFIX" || status=$?
case $status in
    0) ;;
    3)
        keep=1
        echo 'prepare-lock.sh: publication rollback is incomplete; staged inputs and recovery retained' >&2
        exit 3
        ;;
    *)
        fail 'publication failed; committed inputs were restored and staged graph discarded'
        ;;
esac
echo "prepare-lock.sh: published four dsh inputs for $effective_dsh"
