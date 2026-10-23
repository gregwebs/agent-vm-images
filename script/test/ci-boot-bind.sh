#!/usr/bin/env bash
# Exercise the actual workflow bind block, including output-injection rejection.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
fail() { echo "bind regression: $*" >&2; exit 1; }
# No YAML dependency: extract the literal run block belonging to id: bind.
python3 "$ROOT/script/test/ci-boot-bind-extract.py" "$ROOT/.github/workflows/verify-release-boot.yml" "$work/bind.sh"
# The same workflow's conditional signed-source Go setup: an old Python-only
# release must skip it, a new Go source must use only its own go.mod.
mkdir -p "$work/workspace"
python3 "$ROOT/script/test/ci-boot-go-setup.py" "$ROOT/.github/workflows/verify-release-boot.yml" "$work/workspace"
# A real clean checkout satisfies the harness guard without stubbing git; no git child may
# outlive the commit and race the EXIT trap's rm -rf (#85). gc.auto=0 covers older git, where
# commit ran gc --auto itself; a global core.fsmonitor=true would detach fsmonitor--daemon into
# the checkout and race that rm -rf as well (#89). Both repositories get the same pins, because
# the control below is only a control while it mirrors the harness.
for repo in harness control; do
    git init -q "$work/$repo"
    git -C "$work/$repo" config maintenance.auto false
    git -C "$work/$repo" config gc.auto 0
    git -C "$work/$repo" config core.fsmonitor false
done
# Assert the settings by value as well as by effect: gc.auto is inert on git >=2.30, where
# commit no longer runs gc --auto itself, so nothing else would show that line's removal.
# --local reads only the file written just above; a developer whose global config already sets
# maintenance.auto=false or gc.auto=0 would satisfy --get with both script lines deleted.
[ "$(git -C "$work/harness" config --local --get maintenance.auto)" = false ] \
    || fail "harness repository is missing 'maintenance.auto false'"
[ "$(git -C "$work/harness" config --local --get gc.auto)" = 0 ] \
    || fail "harness repository is missing 'gc.auto 0'"
# From git 2.30 commit spawns `maintenance run --auto`; before that it spawned `git gc --auto`
# on every commit whatever the configuration says, so the trace assertion below cannot hold
# there. Report the version instead of a spawn the settings cannot prevent.
version=$(git version); version=${version#git version }
major=${version%%.*}; minor=${version#*.}; minor=${minor%%.*}
# [ -lt ] reports a non-numeric operand as a status set -e ignores inside `if`, which would skip
# the gate and turn an unparseable version back into the confusing spawn report.
case $major.$minor in
    *[!0-9.]*|.*|*.) fail "cannot parse git version '$version'" ;;
esac
if [ "$major" -lt 2 ] || { [ "$major" -eq 2 ] && [ "$minor" -lt 30 ]; }; then
    fail "self-check needs git >=2.30 to observe commit's maintenance child, found $version"
fi
trace="$work/harness-trace.json"
GIT_TRACE2_EVENT="$trace" git -C "$work/harness" -c user.name=Test -c user.email=test@example.invalid commit -qm fixture --allow-empty
# The spawn check is only evidence when this run actually wrote a stream: a missing file reaches
# the predicate as its traceback, and an empty one lets the check pass without examining anything.
# The control gets the same guard, so a removed or retargeted trace fails by name instead of being
# blamed on the commit spawning nothing.
[ -s "$trace" ] || fail "harness commit wrote no trace2 stream; the spawn check would be vacuous"
spawned=$(python3 "$ROOT/script/test/trace2_children.py" "$trace" maintenance gc)
case "$spawned" in
    '') ;;
    *maintenance*) fail "harness commit spawned '$spawned' despite maintenance.auto=false" ;;
    *) fail "harness commit spawned '$spawned' despite gc.auto=0" ;;
esac
GITHUB_SHA="$(git -C "$work/harness" rev-parse HEAD)"
export GITHUB_SHA
export GITHUB_REPOSITORY=gregwebs/agent-vm-images
# A predicate that never matches would pass silently, and -c outranks repository config, so
# this control commit must show a maintenance child to the same check (#88). It needs its own
# repository: the bind block requires the harness to stand at GITHUB_SHA, so the harness keeps
# exactly one commit. Nothing it spawns can outlive it either: run_auto_maintenance() waits for
# its child, from 2.47 the control's own `-c maintenance.autoDetach=false` stops it passing
# `--detach`, and `gc.auto 0` above keeps the maintenance gc task from daemonizing `gc --auto`.
# core.fsmonitor is pinned in both repositories, so a global setting cannot start a daemon that
# races the trap (#89).
GIT_TRACE2_EVENT="$work/control-trace.json" git -C "$work/control" \
    -c maintenance.auto=true -c maintenance.autoDetach=false \
    -c user.name=Test -c user.email=test@example.invalid commit -qm control --allow-empty
[ -s "$work/control-trace.json" ] || fail "control commit wrote no trace2 stream"
[ -n "$(python3 "$ROOT/script/test/trace2_children.py" "$work/control-trace.json" maintenance gc)" ] \
    || fail "control commit spawned no maintenance child; the trace assertion cannot fail"
check() {
    local name=$1 input=$2 ref=$3 expected=$4 status=0
    mkdir -p "$work/$name/tmp"
    (cd "$work"
     export VERSION_INPUT="$input" GITHUB_REF="$ref" RUNNER_TEMP="$work/$name/tmp"
     export GITHUB_ENV="$work/$name/env" GITHUB_OUTPUT="$work/$name/out"
     bash "$work/bind.sh") > "$work/$name/log" 2>&1 || status=$?
    if [ -n "$expected" ]; then
        [ "$status" = 0 ] || fail "$name rejected valid version"
        printf 'version=%s\n' "$expected" > "$work/$name/expected"
        cmp "$work/$name/expected" "$work/$name/out" || fail "$name wrong output"
    else
        [ "$status" != 0 ] || fail "$name unexpectedly passed"
        [ ! -s "$work/$name/out" ] || fail "$name published version output"
    fi
    echo "bind case $name passed"
}
check dispatch 0.1.3 refs/heads/main 0.1.3
check call 12.34.56 refs/heads/main 12.34.56
check precedence 0.1.3 refs/heads/test/release-boot/0.1.4 0.1.3
check branch '' refs/heads/test/release-boot/0.1.3 0.1.3
check branch-suffix '' refs/heads/test/release-boot/0.1.3/try-2 0.1.3
check lf $'0.1.3\n' refs/heads/main ''
check cr $'0.1.3\r' refs/heads/main ''
check embedded-lf $'0.1.3\n0.1.4' refs/heads/main ''
check embedded-cr $'0.1.3\r0.1.4' refs/heads/main ''
check duplicate-version $'0.1.3\nversion=0.1.4' refs/heads/main ''
check leading-junk x0.1.3 refs/heads/main ''
check trailing-junk 0.1.3x refs/heads/main ''
check leading-line $'junk\n0.1.3' refs/heads/main ''
check trailing-line $'0.1.3\njunk' refs/heads/main ''
check branch-invalid '' refs/heads/test/release-boot/latest ''
check empty-nonbranch '' refs/heads/main ''
echo 'ci boot bind controls passed'
