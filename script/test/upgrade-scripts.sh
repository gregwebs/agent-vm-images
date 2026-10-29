#!/usr/bin/env bash
# Black-box tests for the tool-layer upgrade scripts
# (images/tools/pi/upgrade-pi.sh, images/tools/dsh/upgrade-dsh.sh,
# images/tools/pi/bridge/upgrade-bridge.sh) and the shared resolver they source
# (script/build/npm-pin.sh).
#
# No registry and no network: a fake `npm` first on PATH answers `view` from a
# small fixture registry and, for `install --package-lock-only`, rewrites the
# lock in the working directory with a configurable jq filter. So the REAL
# scripts run end to end -- argument parsing, resolution, the scratch dir, and
# the copy-back -- against fixtures, and every failure policy (unpublished
# version, query failure, refused layout, unchanged files on failure) is
# exercised hermetically. `jq` is the real one.
#
# Several assertions grep for literal `${...}` LABEL patterns; the single quotes
# are deliberate, so disable SC2016 for the whole file.
# shellcheck disable=SC2016

set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/upgrade-scripts-test.XXXXXX")"
TEST_ROOT="$(cd "$TEST_ROOT" && pwd -P)"
trap 'rm -rf "$TEST_ROOT"' EXIT

REAL_JQ="$(command -v jq || true)"
[[ -n "$REAL_JQ" ]] || { echo "FAIL: jq is required" >&2; exit 1; }
JQ_DIR="$(dirname "$REAL_JQ")"
# The Pi/bridge upgrades require node; keep it reachable from the fixture PATH.
NODE_DIR="$(dirname "$(command -v node)")"
# The four reviewed pnpm inputs every migrated upgrade publishes.
MIGRATED_INPUTS=(package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)
# The upgrade fixture must exercise the same bytes the image would ship, so it
# reads the committed four inputs and refuses an incomplete set.
reviewed_dir() { # $1 committed dir
    local committed="$1" file
    for file in "${MIGRATED_INPUTS[@]}"; do
        [ -f "$REPO_ROOT/$committed/$file" ] || {
            echo "the committed $committed project is missing $file" >&2
            return 1
        }
    done
    printf '%s' "$REPO_ROOT/$committed"
}

# The scripts under test are bash-only; run them under the same bash as this
# test, so `bash script/test/upgrade-scripts.sh` and `/bin/bash ...` each cover
# their own interpreter (bash 3.2 on macOS).
BASH_BIN="${BASH:-/bin/bash}"

# The committed pins the fixture registry resolves to, so a re-pin is a no-op.
PI_PIN="$(jq -r '.dependencies["@earendil-works/pi-coding-agent"]' "$REPO_ROOT/images/tools/pi/package.json")"
DSH_PIN="$(jq -r '.dependencies["@deepseek-ai/dsh"]' "$REPO_ROOT/images/tools/dsh/package.json")"
PNPM_PIN="$(jq -r .version "$REPO_ROOT/images/pnpm/pin.json")"
BRIDGE_PIN="$(jq -r '.dependencies["pi-claude-bridge"]' "$REPO_ROOT/images/tools/pi/bridge/package.json")"
PNPM_BUMP=12.0.0

STUB_BIN="$TEST_ROOT/bin"
mkdir -p "$STUB_BIN"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_contains() {
    [[ "$1" == *"$2"* ]] || fail "expected output to contain: $2 (got: $1)"
}

assert_not_contains() {
    [[ "$1" != *"$2"* ]] || fail "expected output NOT to contain: $2 (got: $1)"
}

assert_status_ok() {
    [[ $RUN_STATUS -eq 0 ]] || fail "expected success, got $RUN_STATUS: $RUN_OUTPUT"
}

assert_status_fail() {
    [[ $RUN_STATUS -ne 0 ]] || fail "expected failure, got success: $RUN_OUTPUT"
}

assert_label() { # $1 file, $2 exact literal
    grep -Fq "$2" "$1" || fail "expected $1 to contain the label: $2"
}

# The fake `npm`. `view` answers only the two forms the scripts under test
# issue -- `view PKG dist-tags versions --json` (the resolver) and
# `view PKG@VERSION dist.integrity` (upgrade-pi.sh's sibling refill) -- from
# $STUB_REGISTRY (a JSON fixture); an unknown package or version is npm's E404
# (exit 1), STUB_VIEW_FAIL models an unreachable registry, and any *other* view
# form fails loudly so a test cannot pass against an unmodelled query. `install`
# copies $STUB_SEED_LOCK into the cwd when no lock is present (pi regenerates
# from nothing) and applies $STUB_INSTALL_JQ, modelling what the registry would
# produce.
cat >"$STUB_BIN/npm" <<'SH'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"${STUB_LOG:-/dev/null}"

npm_404() {
    echo "npm error code E404" >&2
    echo "npm error 404 Not Found - GET https://registry.npmjs.org/$1 - Not found" >&2
    echo "npm error 404  '$1@*' is not in this registry." >&2
    exit 1
}

case "${1:-}" in
    view)
        shift
        if [ "${STUB_VIEW_FAIL:-}" = 1 ]; then
            echo "npm error network request to the registry failed" >&2
            exit 1
        fi
        if [ "${2:-}" = dist-tags ] && [ "${3:-}" = versions ] && [ "${4:-}" = --json ]; then
            pkg=$1
            jq -e --arg p "$pkg" '.packages[$p]' "$STUB_REGISTRY" >/dev/null || npm_404 "$pkg"
            jq -c --arg p "$pkg" \
                '{ "dist-tags": .packages[$p]["dist-tags"], versions: .packages[$p].versions }' \
                "$STUB_REGISTRY"
        elif [ "${2:-}" = dist.integrity ]; then
            spec=$1
            pkg=${spec%@*}
            ver=${spec##*@}
            jq -e --arg p "$pkg" '.packages[$p]' "$STUB_REGISTRY" >/dev/null || npm_404 "$pkg"
            jq -e --arg p "$pkg" --arg v "$ver" \
                '(.packages[$p].versions // []) | index($v)' "$STUB_REGISTRY" >/dev/null \
                || npm_404 "${pkg}@${ver}"
            jq -r --arg p "$pkg" --arg v "$ver" '.packages[$p].integrity[$v] // empty' "$STUB_REGISTRY"
        else
            echo "stub npm: unhandled view form: $*" >&2
            exit 3
        fi
        ;;
    install)
        if [ -n "${STUB_SEED_LOCK:-}" ] && [ ! -f package-lock.json ]; then
            cp "$STUB_SEED_LOCK" package-lock.json
        fi
        [ -f package-lock.json ] || {
            echo "stub npm: no package-lock.json to update" >&2
            exit 4
        }
        jq "${STUB_INSTALL_JQ:-.}" package-lock.json >package-lock.json.tmp
        mv package-lock.json.tmp package-lock.json
        ;;
    *)
        echo "stub npm: unhandled invocation: $*" >&2
        exit 5
        ;;
esac
SH
chmod +x "$STUB_BIN/npm"

# A `cp` shim that passes through except for one configured destination, so a
# final-publication failure can be simulated deterministically (a read-only
# file would not fail a root-run test).
cat >"$STUB_BIN/cp" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
last=
for arg in "$@"; do last=$arg; done
norm() {
    local dir base
    dir="$(cd "$(dirname "$1")" 2>/dev/null && pwd -P)" || { printf '%s' "$1"; return; }
    base="$(basename "$1")"
    printf '%s/%s' "$dir" "$base"
}
src=${@: -2:1}
# A deterministic signal while one destination is being written: the copy has
# already truncated it and exits successfully, so only the caller's own signal
# handling can restore the file. `$PPID` is the upgrade script that started this
# copy, exactly where a human's Ctrl-C or a supervisor's TERM would land.
# One shot only: the restore the caller is about to make copies to the same
# destination, and the signal must not be replayed into it.
signalled() {
    [ ! -e "${STUB_SIGNAL_MARKER:-/nonexistent}" ] && : >"${STUB_SIGNAL_MARKER:?}"
}
if [ -n "${STUB_CP_SIGNAL_DEST:-}" ] && [ ! -e "${STUB_SIGNAL_MARKER:-/nonexistent}" ] \
    && [ "$(norm "$last")" = "$(norm "$STUB_CP_SIGNAL_DEST")" ]; then
    signalled
    printf partial >"$last"
    kill -TERM "$PPID"
    exit 0
fi
if [ -n "${STUB_CP_SIGNAL_BACKUP:-}" ] && [ ! -e "${STUB_SIGNAL_MARKER:-/nonexistent}" ] \
    && [[ "$last" == */publish-backup/* ]]; then
    signalled
    kill -TERM "$PPID"
    exit 0
fi
if [ -n "${STUB_CP_FAIL_DEST:-}" ] && [ "$(norm "$last")" = "$(norm "$STUB_CP_FAIL_DEST")" ]; then
    if [[ "$src" == */publish-backup/* ]]; then
        [[ "${STUB_ROLLBACK_FAIL:-}" == 1 ]] && exit 9
    else
        printf partial >"$last"
        echo "cp: simulated partial publication failure for $last" >&2
        exit 8
    fi
fi
exec /bin/cp "$@"
SH
chmod +x "$STUB_BIN/cp"

CASE=""

new_case() {
    unset STUB_ROLLBACK_FAIL STUB_VIEW_FAIL STUB_SEED_LOCK STUB_INSTALL_JQ PI_INTEGRITY STUB_CP_FAIL_DEST \
        STUB_CP_SIGNAL_DEST STUB_CP_SIGNAL_BACKUP STUB_SIGNAL_MARKER
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/home" "$CASE/script/build" "$CASE/images/tools/pi/bridge" \
        "$CASE/images/tools/dsh" "$CASE/expected/pi" "$CASE/expected/dsh" \
        "$CASE/expected/bridge"
    cp "$REPO_ROOT/script/build/npm-pin.sh" "$CASE/script/build/"
    cp "$REPO_ROOT/script/build/dockerfile-label.sh" "$CASE/script/build/"
    cp "$REPO_ROOT/script/build/transactional-publish.sh" "$CASE/script/build/"
    cp "$REPO_ROOT/images/tools/pnpm-project.py" "$REPO_ROOT/images/tools/pnpm-host-manager.py" \
        "$CASE/images/tools/"
    cp "$REPO_ROOT/images/tools/pi/upgrade-pi.sh" "$REPO_ROOT/images/tools/pi/prepare-lock.sh" \
        "$CASE/images/tools/pi/"
    cp "$REPO_ROOT/images/tools/pi/bridge/upgrade-bridge.sh" \
        "$REPO_ROOT/images/tools/pi/bridge/prepare-lock.sh" \
        "$CASE/images/tools/pi/bridge/"
    local pi_dir bridge_dir
    pi_dir="$(reviewed_dir images/tools/pi)"
    bridge_dir="$(reviewed_dir images/tools/pi/bridge)"
    for name in "${MIGRATED_INPUTS[@]}"; do
        cp "$pi_dir/$name" "$CASE/images/tools/pi/$name"
        cp "$bridge_dir/$name" "$CASE/images/tools/pi/bridge/$name"
    done
    cp "$REPO_ROOT/images/tools/dsh/upgrade-dsh.sh" \
        "$REPO_ROOT/images/tools/dsh/package.json" "$REPO_ROOT/images/tools/dsh/pnpm-lock.yaml" \
        "$REPO_ROOT/images/tools/dsh/pnpm-workspace.yaml" "$REPO_ROOT/images/tools/dsh/pnpm-inputs.sha256" \
        "$REPO_ROOT/images/tools/dsh/prepare-lock.sh" \
        "$CASE/images/tools/dsh/"
    mkdir -p "$CASE/images/standard" "$CASE/images/pnpm"
    cp "$REPO_ROOT/images/pnpm/pin.json" "$CASE/images/pnpm/pin.json"
    cp "$REPO_ROOT/images/standard/Dockerfile" "$CASE/images/standard/Dockerfile"
    # Pristine snapshots: a failed run must leave the live files byte-identical.
    for name in "${MIGRATED_INPUTS[@]}"; do
        cp "$CASE/images/tools/pi/$name" "$CASE/expected/pi/$name"
        cp "$CASE/images/tools/pi/bridge/$name" "$CASE/expected/bridge/$name"
    done
    cp "$CASE/images/tools/dsh/package.json" "$CASE/images/tools/dsh/pnpm-lock.yaml" \
        "$CASE/images/tools/dsh/pnpm-workspace.yaml" "$CASE/images/tools/dsh/pnpm-inputs.sha256" "$CASE/expected/dsh/"
    # Dockerfile snapshots: the LABEL mirrors must not change on a failed run
    # either, and the bridge bump edits the PARENT pi Dockerfile.
    cp "$CASE/images/standard/Dockerfile" "$CASE/expected/pi/Dockerfile"
    cp "$CASE/images/standard/Dockerfile" "$CASE/expected/bridge/Dockerfile"
    cp "$CASE/images/standard/Dockerfile" "$CASE/expected/dsh/Dockerfile"
    # The staged prefixes live outside the fixture checkout, so the composed
    # helper must be the checkout's own `images/recipe-contract` directory: that
    # is the authorized shape for an upgrade whose scratch tree is a canonical
    # child of the checkout it came from.
    mkdir -p "$CASE/images/recipe-contract"
    cp -p "$COMPOSED_CONTRACT/recipe-contract" "$CASE/images/recipe-contract/"
    : >"$CASE/npm.log"
    write_registry
}

# The fixture registry, defaulting to the committed pins. Only tag discovery is
# left: the Pi/bridge upgrades take every archive identity and SRI from the pnpm
# graph, so there is no npm `view … dist.integrity` refill to model.
write_registry() {
    jq -n \
        --arg pi "$PI_PIN" --arg dsh "$DSH_PIN" --arg pnpm "$PNPM_PIN" \
        --arg pnb "$PNPM_BUMP" --arg bridge "$BRIDGE_PIN" '
        {
          packages: {
            "@earendil-works/pi-coding-agent": {"dist-tags": {latest: $pi, next: $pi}, "versions": [$pi]},
            "@deepseek-ai/dsh": {"dist-tags": {latest: $dsh, next: $dsh}, "versions": [$dsh]},
            "pnpm": {"dist-tags": {latest: $pnpm, next: $pnpm}, "versions": [$pnpm, $pnb]},
            "pi-claude-bridge": {"dist-tags": {latest: $bridge, next: $bridge}, "versions": [$bridge]}
          }
        }' >"$CASE/registry.json"
}

# Add a publishable version for PACKAGE, optionally pointing dist-tag TAG at it.
registry_add_version() {
    local pkg="$1" ver="$2" tag="${3:-}"
    if [ -n "$tag" ]; then
        jq --arg p "$pkg" --arg v "$ver" --arg t "$tag" \
            '.packages[$p].versions += [$v] | .packages[$p]["dist-tags"][$t] = $v' \
            "$CASE/registry.json" >"$CASE/registry.json.tmp"
    else
        jq --arg p "$pkg" --arg v "$ver" '.packages[$p].versions += [$v]' \
            "$CASE/registry.json" >"$CASE/registry.json.tmp"
    fi
    mv "$CASE/registry.json.tmp" "$CASE/registry.json"
}

run_upgrade() {
    local script="$1"
    shift
    set +e
    RUN_OUTPUT="$(env -i \
        "PATH=$STUB_BIN:$JQ_DIR:$NODE_DIR:/usr/bin:/bin:/usr/sbin:/sbin" \
        "HOME=$CASE/home" \
        "TMPDIR=$CASE" \
        "AGENT_VM_CONTRACT_DIR=$CASE/images/recipe-contract" \
        "AGENT_VM_FIXTURE_PNPM=${AGENT_VM_FIXTURE_PNPM:-}" \
        "STUB_ROLLBACK_FAIL=${STUB_ROLLBACK_FAIL:-}" \
        "STUB_REGISTRY=$CASE/registry.json" \
        "STUB_LOG=$CASE/npm.log" \
        "STUB_VIEW_FAIL=${STUB_VIEW_FAIL:-}" \
        "STUB_SEED_LOCK=${STUB_SEED_LOCK:-}" \
        "STUB_INSTALL_JQ=${STUB_INSTALL_JQ:-.}" \
        "STUB_CP_FAIL_DEST=${STUB_CP_FAIL_DEST:-}" \
        "STUB_CP_SIGNAL_DEST=${STUB_CP_SIGNAL_DEST:-}" \
        "STUB_CP_SIGNAL_BACKUP=${STUB_CP_SIGNAL_BACKUP:-}" \
        "STUB_SIGNAL_MARKER=$CASE/signal-fired" \
        "$BASH_BIN" "$script" "$@" 2>&1)"
    RUN_STATUS=$?
    set -e
}

assert_files_unchanged() {
    local which="$1" base exp dockerfile
    case "$which" in
        pi) base=images/tools/pi; dockerfile="$CASE/images/standard/Dockerfile" ;;
        dsh) base=images/tools/dsh; dockerfile="$CASE/images/standard/Dockerfile" ;;
        bridge) base=images/tools/pi/bridge; dockerfile="$CASE/images/standard/Dockerfile" ;;
    esac
    local inputs=("${MIGRATED_INPUTS[@]}")
    [ "$which" = dsh ] && inputs=(package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)
    for exp in "${inputs[@]}"; do
        cmp -s "$CASE/expected/$which/$exp" "$CASE/$base/$exp" \
            || fail "$which/$exp changed (expected byte-identical)"
    done
    cmp -s "$CASE/expected/$which/Dockerfile" "$dockerfile" \
        || fail "$which Dockerfile changed (expected byte-identical)"
}

# --- pi/bridge: four-input publication, partial failure, absent pre-image ----
#
# The upgrades publish FOUR pnpm inputs plus one central Dockerfile LABEL in one
# transaction. Resolution itself belongs to the owning preparation driver and is
# proved by script/test/pi-prepare-project.py against a recorded manager; what
# these cases own is the publication half, so the fixture binds a driver that
# stages the reviewed four inputs at the requested version (keeping every archive
# SRI) while the REAL composed `recipe-contract` still gates the result with
# `pnpm-lock check` and `pnpm-lock graph`.

# The fixture driver. It is deliberately a version-only re-pin: the graph's
# identities, SRI, peers and edges stay the reviewed bytes, which is exactly the
# shape the publication transaction must carry through unchanged.
fixture_prepare() { # $1 case dir
    cat >"$1/images/tools/pi/prepare-lock.sh" <<'SH'
#!/bin/sh
# Fixture-only preparation: stage the reviewed inputs at the requested exact
# version. No manager, no network, no resolution.
set -eu
prefix=$1 version=$2
shift 2
[ "${1:-}" = --refresh-lock ] || exit 2
[ -f "$prefix/package.json" ] || exit 1
name=$(jq -r '.dependencies | keys[0]' "$prefix/package.json")
old=$(jq -r --arg n "$name" '.dependencies[$n]' "$prefix/package.json")
[ "$old" != "$version" ] || exit 1
sed -e "s|^        specifier: $old\$|        specifier: $version|" \
    -e "s|^        version: $old|        version: $version|" \
    -e "s|^  '$name@$old|  '$name@$version|" \
    -e "s|^  $name@$old|  $name@$version|" \
    "$prefix/pnpm-lock.yaml" >"$prefix/pnpm-lock.yaml.tmp" || exit 1
mv "$prefix/pnpm-lock.yaml.tmp" "$prefix/pnpm-lock.yaml"
jq --arg n "$name" --arg v "$version" '.dependencies[$n] = $v' "$prefix/package.json" \
    >"$prefix/package.json.tmp" || exit 1
mv "$prefix/package.json.tmp" "$prefix/package.json"
(cd "$prefix" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml >pnpm-inputs.sha256)
SH
    chmod +x "$1/images/tools/pi/prepare-lock.sh"
    cp "$1/images/tools/pi/prepare-lock.sh" "$1/images/tools/pi/bridge/prepare-lock.sh"
}

# assert_published WHICH VERSION: the four live inputs are the staged bytes (the
# pin file agrees with them), the project's label mirrors VERSION, and the other
# project's inputs and label are untouched.
assert_published() { # $1 which, $2 version
    local which="$1" version="$2" base target label_arg pinned other
    case "$which" in
        pi) base=images/tools/pi; target=pi; label_arg=AGENT_VERSION_PI
            pinned=@earendil-works/pi-coding-agent; other=bridge ;;
        bridge) base=images/tools/pi/bridge; target=pi-claude-bridge
            label_arg=AGENT_VERSION_PI_CLAUDE_BRIDGE
            pinned=pi-claude-bridge; other=pi ;;
    esac
    [ "$(jq -r --arg n "$pinned" '.dependencies[$n]' "$CASE/$base/package.json")" = "$version" ] ||
        fail "$which package.json was not published at $version"
    (cd "$CASE/$base" && sha256sum -c pnpm-inputs.sha256 >/dev/null) ||
        fail "$which pnpm-inputs.sha256 does not describe the published inputs"
    grep -Fq "org.agent-vm.version.$target=\"\${$label_arg:-$version}\"" \
        "$CASE/images/standard/Dockerfile" || fail "$which label was not published at $version"
    for file in "${MIGRATED_INPUTS[@]}"; do
        cmp -s "$CASE/expected/$other/$file" "$CASE/images/tools/$([ "$other" = pi ] && printf pi || printf pi/bridge)/$file" ||
            fail "the $other project changed while $which was published"
    done
}

# The composed helper is the real gate for the staged graph, so the native build
# is resolved once and copied into each fixture checkout.
COMPOSED_CONTRACT="${RECIPE_CONTRACT_CONTEXT:-$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)}/out"
[ -x "$COMPOSED_CONTRACT/recipe-contract" ] ||
    { echo 'FAIL: no composed recipe-contract helper' >&2; exit 1; }
export AGENT_VM_FIXTURE_PNPM=1

new_case pi-upgrade-publishes-four-inputs
fixture_prepare "$CASE"
registry_add_version @earendil-works/pi-coding-agent 1.4.1
run_upgrade "$CASE/images/tools/pi/upgrade-pi.sh" 1.4.1
assert_status_ok
assert_published pi 1.4.1

new_case bridge-upgrade-publishes-four-inputs
fixture_prepare "$CASE"
registry_add_version pi-claude-bridge 0.8.1
run_upgrade "$CASE/images/tools/pi/bridge/upgrade-bridge.sh" 0.8.1
assert_status_ok
assert_published bridge 0.8.1

# A failure at any one of the five destinations restores every destination --
# including the one whose write had already truncated -- and leaves the checkout
# byte-identical.
new_case pi-upgrade-partial-publish-restores
fixture_prepare "$CASE"
registry_add_version @earendil-works/pi-coding-agent 1.4.1
for destination in \
    images/tools/pi/package.json images/tools/pi/pnpm-lock.yaml \
    images/tools/pi/pnpm-workspace.yaml images/tools/pi/pnpm-inputs.sha256 \
    images/standard/Dockerfile; do
    STUB_CP_FAIL_DEST="$CASE/$destination" \
        run_upgrade "$CASE/images/tools/pi/upgrade-pi.sh" 1.4.1
    assert_status_fail
    assert_contains "$RUN_OUTPUT" "failed to publish"
    assert_files_unchanged pi
done

# Rollback that cannot restore is status 3, and the recovery material is kept.
new_case pi-upgrade-rollback-incomplete
fixture_prepare "$CASE"
registry_add_version @earendil-works/pi-coding-agent 1.4.1
STUB_CP_FAIL_DEST="$CASE/images/tools/pi/pnpm-inputs.sha256" STUB_ROLLBACK_FAIL=1 \
    run_upgrade "$CASE/images/tools/pi/upgrade-pi.sh" 1.4.1
[ "$RUN_STATUS" -eq 3 ] || fail "an incomplete rollback must exit 3 (got $RUN_STATUS)"
assert_contains "$RUN_OUTPUT" "recovery material retained at"

# An absent pre-image is not "nothing to restore": the transaction records the
# absence and removes the partially published file on failure.
new_case bridge-upgrade-absent-preimage
fixture_prepare "$CASE"
registry_add_version pi-claude-bridge 0.8.1
rm "$CASE/images/tools/pi/bridge/pnpm-workspace.yaml"
STUB_CP_FAIL_DEST="$CASE/images/tools/pi/bridge/pnpm-inputs.sha256" \
    run_upgrade "$CASE/images/tools/pi/bridge/upgrade-bridge.sh" 0.8.1
assert_status_fail
[ ! -e "$CASE/images/tools/pi/bridge/pnpm-workspace.yaml" ] ||
    fail "an originally absent input was invented by the rollback"
cmp -s "$CASE/expected/bridge/Dockerfile" "$CASE/images/standard/Dockerfile" ||
    fail "the Dockerfile changed although the publication failed"

# A signal during publication must never leave a half-published tree: the
# interrupted transaction restores every destination from the backups it took
# before the first copy, and the recovery material stays for a human. The second
# destination is the lock, so the first input is already published when the TERM
# lands and the reverse-inclusive restore has real work to do.
assert_recovery_retained() { # $1 scratch name prefix
    local dir index
    dir=$(find "$CASE" -maxdepth 1 -name "$1-upgrade.*" -type d | head -1)
    [ -n "$dir" ] || fail "no retained recovery material for $1"
    [ -f "$dir/publish-backup/.complete" ] || fail "$1: retained backups are not complete"
    for index in 0 1 2 3 4; do
        [ -f "$dir/publish-backup/$index" ] || fail "$1: retained backup $index is missing"
    done
}

new_case pi-upgrade-signal-during-publish
fixture_prepare "$CASE"
registry_add_version @earendil-works/pi-coding-agent 1.4.1
STUB_CP_SIGNAL_DEST="$CASE/images/tools/pi/pnpm-lock.yaml" \
    run_upgrade "$CASE/images/tools/pi/upgrade-pi.sh" 1.4.1
[ "$RUN_STATUS" -eq 143 ] || fail "a TERM during publication must exit 143 (got $RUN_STATUS): $RUN_OUTPUT"
assert_contains "$RUN_OUTPUT" "publication interrupted; sources restored"
assert_files_unchanged pi
assert_recovery_retained pi

new_case bridge-upgrade-signal-during-publish
fixture_prepare "$CASE"
registry_add_version pi-claude-bridge 0.8.1
STUB_CP_SIGNAL_DEST="$CASE/images/tools/pi/bridge/pnpm-lock.yaml" \
    run_upgrade "$CASE/images/tools/pi/bridge/upgrade-bridge.sh" 0.8.1
[ "$RUN_STATUS" -eq 143 ] || fail "a TERM during publication must exit 143 (got $RUN_STATUS): $RUN_OUTPUT"
assert_contains "$RUN_OUTPUT" "publication interrupted; sources restored"
assert_files_unchanged bridge
assert_recovery_retained bridge

# A signal while the backups themselves are being taken must not restore from a
# partial backup: nothing has been written yet, so the sources must be untouched
# and the incomplete recovery material is what carries status 3.
new_case pi-upgrade-signal-during-backup
fixture_prepare "$CASE"
registry_add_version @earendil-works/pi-coding-agent 1.4.1
STUB_CP_SIGNAL_BACKUP=1 run_upgrade "$CASE/images/tools/pi/upgrade-pi.sh" 1.4.1
[ "$RUN_STATUS" -eq 3 ] || fail "a TERM during the backup phase must exit 3 (got $RUN_STATUS): $RUN_OUTPUT"
assert_contains "$RUN_OUTPUT" "publication interrupted while taking backups"
assert_files_unchanged pi

# A repository still carrying the retired npm model cannot be re-pinned: the
# reviewed-pnpm guard fires before any resolution.
new_case pi-upgrade-refuses-npm-model
fixture_prepare "$CASE"
jq 'del(.packageManager)' "$CASE/images/tools/pi/package.json" >"$CASE/images/tools/pi/package.json.tmp"
mv "$CASE/images/tools/pi/package.json.tmp" "$CASE/images/tools/pi/package.json"
run_upgrade "$CASE/images/tools/pi/upgrade-pi.sh"
assert_status_fail
assert_contains "$RUN_OUTPUT" "packageManager disagrees with the reviewed base pin"

new_case bridge-upgrade-refuses-npm-model
fixture_prepare "$CASE"
jq 'del(.packageManager)' "$CASE/images/tools/pi/bridge/package.json" \
    >"$CASE/images/tools/pi/bridge/package.json.tmp"
mv "$CASE/images/tools/pi/bridge/package.json.tmp" "$CASE/images/tools/pi/bridge/package.json"
run_upgrade "$CASE/images/tools/pi/bridge/upgrade-bridge.sh"
assert_status_fail
assert_contains "$RUN_OUTPUT" "packageManager disagrees with the reviewed base pin"

new_case pi-upgrade-ci-refusal
set +e
RUN_OUTPUT="$(env -i "PATH=$STUB_BIN:/usr/bin:/bin" "HOME=$CASE/home" "TMPDIR=$CASE" \
    "GITHUB_ACTIONS=true" "$BASH_BIN" "$CASE/images/tools/pi/upgrade-pi.sh" 2>&1)"
RUN_STATUS=$?
set -e
assert_status_fail
assert_contains "$RUN_OUTPUT" "cannot run in GitHub Actions"
assert_files_unchanged pi

new_case pi-upgrade-runtime-injection
set +e
RUN_OUTPUT="$(env -i "PATH=$STUB_BIN:/usr/bin:/bin" "HOME=$CASE/home" "TMPDIR=$CASE" \
    "NODE_OPTIONS=--require=/tmp/evil.js" "$BASH_BIN" "$CASE/images/tools/pi/upgrade-pi.sh" 2>&1)"
RUN_STATUS=$?
set -e
assert_status_fail
assert_contains "$RUN_OUTPUT" "runtime injection is unsupported"
assert_files_unchanged pi

python3 "$REPO_ROOT/script/test/dsh-upgrade.py"

# --- the resolver itself: exact-version membership, npm's one-version shape --
# The end-to-end cases above resolve the committed pin through the `latest`
# dist-tag and reject an unpublished version. These call resolve_npm_version
# directly, with a registry whose `versions` is npm's single-version shape (a
# bare string, not a one-element array), so the membership test and the shape
# normalisation each fail loudly when broken.
single_registry="$TEST_ROOT/single-registry.json"
jq -n '{packages: {"single-pkg": {versions: "1.2.3", "dist-tags": {latest: "1.2.3"}}}}' \
    >"$single_registry"
run_resolver() {
    set +e
    # The -c body is deliberately single-quoted: $1/$2 are the inner bash's
    # positional parameters, not this shell's (SC2016).
    # shellcheck disable=SC2016
    RUN_OUTPUT="$(env -i \
        "PATH=$STUB_BIN:$JQ_DIR:/usr/bin:/bin" \
        "STUB_REGISTRY=$single_registry" \
        "STUB_VIEW_FAIL=" \
        "$BASH_BIN" -c '. "$1"; resolve_npm_version single-pkg "$2"' _ \
        "$REPO_ROOT/script/build/npm-pin.sh" "$1" 2>&1)"
    RUN_STATUS=$?
    set -e
}
run_resolver 1.2.3
assert_status_ok
[[ "$RUN_OUTPUT" == "1.2.3" ]] || fail "resolver returned '$RUN_OUTPUT', expected 1.2.3"
run_resolver latest
assert_status_ok
[[ "$RUN_OUTPUT" == "1.2.3" ]] || fail "resolver returned '$RUN_OUTPUT', expected 1.2.3"
run_resolver 9.9.9
assert_status_fail
assert_contains "$RUN_OUTPUT" "single-pkg@9.9.9 is not a published version"

# --- the helpers cannot drift out of the scripts they are meant to back -----

for s in images/tools/pi/upgrade-pi.sh images/tools/dsh/upgrade-dsh.sh \
    images/tools/pi/bridge/upgrade-bridge.sh; do
    grep -Fq 'script/build/npm-pin.sh' "$REPO_ROOT/$s" \
        || fail "$s no longer sources the shared npm-pin.sh helper"
done

# --- dockerfile-label.sh: exactly one match, or fail ------------------------
# shellcheck source=/dev/null
. "$REPO_ROOT/script/build/dockerfile-label.sh"
label_probe="$TEST_ROOT/label-probe.Dockerfile"
printf 'ARG X=\nLABEL org.agent-vm.version.dsh="${AGENT_VERSION_DSH:-1.0.0}" \\\n      org.agent-vm.version.pnpm="${AGENT_VERSION_PNPM:-2.0.0}"\n' >"$label_probe"
set_version_label "$label_probe" dsh AGENT_VERSION_DSH 1.5.0
assert_label "$label_probe" 'org.agent-vm.version.dsh="${AGENT_VERSION_DSH:-1.5.0}"'
assert_label "$label_probe" 'org.agent-vm.version.pnpm="${AGENT_VERSION_PNPM:-2.0.0}"'
printf 'LABEL nothing-here\n' >"$label_probe"
set +e
set_version_label "$label_probe" dsh AGENT_VERSION_DSH 1.5.0
label_status=$?
set -e
[[ $label_status -ne 0 ]] || fail "a missing label must fail"
printf 'LABEL org.agent-vm.version.dsh="${AGENT_VERSION_DSH:-1.0.0}"\nLABEL org.agent-vm.version.dsh="${AGENT_VERSION_DSH:-1.0.0}"\n' >"$label_probe"
set +e
set_version_label "$label_probe" dsh AGENT_VERSION_DSH 1.5.0
label_status=$?
set -e
[[ $label_status -ne 0 ]] || fail "a duplicated label must fail"

for s in images/tools/dsh/upgrade-dsh.sh images/tools/pi/upgrade-pi.sh \
    images/tools/pi/bridge/upgrade-bridge.sh; do
    grep -Fq 'script/build/dockerfile-label.sh' "$REPO_ROOT/$s" \
        || fail "$s no longer sources the shared dockerfile-label.sh helper"
done

echo 'upgrade-script black-box tests passed'
