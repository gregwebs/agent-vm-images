#!/usr/bin/env bash
# Maintainer-host parent-pin refresh (#95).
#
# This is the only writer of images/parent-pins.json and of the extracted Debian
# apt baseline. Builds verify the committed pins; they never resolve a parent or
# fetch a digest. Every run resolves against the live registries, authenticates
# the released-standard metadata with the committed signer policy, stages
# complete candidate copies, and publishes them as one transaction.
#
# Publication is restricted to the destination list the stager reports, so a
# resolution bug cannot write outside images/ and the four example layers, and
# rollback only ever restores or removes paths this run backed up or created.
#
# Status: 0 published or unchanged; 1 failed (sources unchanged or fully
# restored); 2 usage; 3 undecided or incomplete recovery ($work retained and
# printed); 128+N when a signal arrives and the sources were restored.
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
# shellcheck source=/dev/null
. "$ROOT/script/build/transactional-publish.sh"

PYTHON=script/dev/parent_pins.py
INVENTORY=images/parent-pins.json
OWNED_DIRECTORY=images/apt-policy
# Inputs the base image consumes besides the destinations. A local edit here
# would silently change what a published identity describes, so a dirty one
# stops a refresh even when it is not itself a destination. Names that do not
# exist yet are skipped: later PRs add install-build-ca.sh and the manifest.
BASE_INPUTS=(.dockerignore Dockerfile install-build-ca.sh install-pnpm.sh install-zellij.sh
             pnpm recipe-contract apt-policy)

usage() {
    cat >&2 <<'USAGE'
usage: refresh-parent-pins.sh debian
       refresh-parent-pins.sh golang-builder [golang:VERSION-trixie]
       refresh-parent-pins.sh standard-release X.Y.Z
       refresh-parent-pins.sh --bootstrap

Resolve the selected parents against their registries, authenticate the
released-standard metadata, and publish images/parent-pins.json plus the
recipes and the extracted Debian apt baseline in one transaction. Ordinary
builds never run this. --bootstrap is the single first adoption of every pin.

Prerequisites: Docker with a reachable Linux daemon and buildx, python3, git,
GNU coreutils (sha256sum), diff, jq, and gh with `attestation verify` (tested
2.97.0). `gh` needs no login: parent attestations are checked anonymously.
USAGE
    exit 2
}

refuse() {
    echo "parent-pins: refusing $*" >&2
    exit 1
}

refuse_dirty() {
    echo "parent-pins: refusing dirty or untracked input/destination: $1" >&2
    exit 1
}

# --- signal and exit state --------------------------------------------------

publishing=0 published=0 decided=0 status=0 keep_recovery=false owns_lock=false
work='' backup='' lock='' stage_dir='' inputs_file=''
# `destinations` are checkout-relative (they are what the plan reports and what
# recovery.json records); `targets` are their absolute counterparts, because the
# publisher runs from the caller's working directory, not from the checkout.
existed=() destinations=() targets=()

release_lock() {
    # Only a lock this invocation took may be removed: another refresh may hold it.
    [ "$owns_lock" = true ] || return 0
    if [ -n "$lock" ] && [ -d "$lock" ]; then rmdir "$lock" 2>/dev/null || true; fi
}

restore_sources() { # signal path only; mirrors publish_transactional inclusively
    local i rc=0
    for ((i = ${#destinations[@]} - 1; i >= 0; i--)); do
        if [ "${existed[i]}" = yes ]; then
            # Only a complete backup carries its numeric name.
            if [ -f "$backup/$i" ]; then cp -p "$backup/$i" "${targets[i]}" || rc=3; else rc=3; fi
        else
            rm -f "${targets[i]}" || rc=3 # first adoption: the file did not exist
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
        if [ "$status" -ne 0 ]; then restore_sources || :; fi
        echo "error: publication result undecided when the signal arrived; recovery material retained at $work" >&2
    elif [ "$decided" -eq 1 ]; then
        keep_recovery=true
        code=3
        echo "error: publication result undecided when the signal arrived; recovery material retained at $work" >&2
    fi
    if [ "$keep_recovery" = true ]; then
        echo "STOP: recover $work before further edits/builds/commits" >&2
    fi
    exit "$code"
}

on_exit() {
    local rc=$?
    # Only a directory this invocation created and marked may be removed, and
    # only when no recovery material has to be preserved.
    if [ "$keep_recovery" = false ] && [ -n "$work" ] && [ -f "$work/.refresh-parent-pins" ]; then
        rm -rf "$work"
    fi
    release_lock
    exit "$rc"
}

allocate_work() {
    work=$(mktemp -d "${TMPDIR:-/tmp}/refresh-parent-pins.XXXXXX")
    work=$(cd "$work" && pwd -P)
    : >"$work/.refresh-parent-pins"
    backup=$work/publish-backup
    inputs_file=$work/inputs.list
}

# --- selection and prerequisites -------------------------------------------

selection=( "$@" )
case "${1:-}" in
    --bootstrap) [ "$#" -eq 1 ] || usage ;;
    debian) [ "$#" -eq 1 ] || usage ;;
    golang-builder) [ "$#" -le 2 ] || usage ;;
    standard-release) [ "$#" -eq 2 ] || usage ;;
    *) usage ;;
esac
[ "${GITHUB_ACTIONS:-}" != true ] || {
    echo 'parent-pins: refusing GitHub Actions; pin refresh is maintainer-host maintenance (#105)' >&2
    exit 2
}
for argument in "${selection[@]}"; do
    case "$argument" in
        latest | *:latest | *@latest) echo "parent-pins: refusing a floating selector: $argument" >&2; exit 2 ;;
    esac
done
case "${selection[0]}" in
    golang-builder)
        if [ "$#" -eq 2 ]; then
            printf '%s' "$2" | grep -Eqx 'golang:[0-9]+\.[0-9]+\.[0-9]+-trixie' ||
                { echo 'parent-pins: refusing a Go selection other than golang:VERSION-trixie' >&2; exit 2; }
        fi
        ;;
    standard-release)
        printf '%s' "$2" | grep -Eqx '[0-9]+\.[0-9]+\.[0-9]+' ||
            { echo 'parent-pins: refusing a release selection other than X.Y.Z' >&2; exit 2; }
        ;;
esac
bootstrap=false
[ "${selection[0]}" != --bootstrap ] || bootstrap=true

missing=false
for tool in docker python3 git gh sha256sum cmp diff jq; do
    if ! command -v "$tool" >/dev/null; then echo "missing prerequisite: $tool" >&2; missing=true; fi
done
[ "$missing" = false ] || exit 1
docker info >/dev/null 2>&1 || { echo 'the Docker daemon is not reachable' >&2; exit 1; }
docker buildx version >/dev/null 2>&1 || { echo 'Docker buildx is required' >&2; exit 1; }
gh attestation verify --help >/dev/null 2>&1 || {
    echo 'gh with `attestation verify` is required (tested 2.97.0)' >&2
    exit 1
}

# --- preflight --------------------------------------------------------------

remember_input() {
    if [ -s "$inputs_file" ] && grep -qxF -- "$1" "$inputs_file"; then return 0; fi
    printf '%s\n' "$1" >>"$inputs_file"
}

# Every ancestor must already be a real directory inside the checkout: a
# symlinked or non-directory component would silently redirect publication
# outside it. Only this operation's own directory may be missing.
check_ancestors() {
    local rest=$1 prefix='' component absolute
    while [ "${rest%%/*}" != "$rest" ]; do
        component=${rest%%/*}
        rest=${rest#*/}
        prefix=${prefix:+$prefix/}$component
        absolute=$ROOT/$prefix
        if [ -L "$absolute" ]; then
            refuse "symlinked destination ancestor: $prefix"
        elif [ -d "$absolute" ]; then
            continue
        elif [ -e "$absolute" ]; then
            refuse "non-directory destination ancestor: $prefix"
        else
            [ "$bootstrap" = true ] && [ "$prefix" = "$OWNED_DIRECTORY" ] ||
                refuse "missing destination ancestor: $prefix"
        fi
    done
    if [ -L "$ROOT/$1" ]; then refuse "symlinked destination: $1"; fi
}

check_clean() {
    local relative=$1
    if ! git -C "$ROOT" ls-files --error-unmatch -- "$relative" >/dev/null 2>&1; then
        if [ -e "$ROOT/$relative" ] || [ -L "$ROOT/$relative" ]; then
            refuse_dirty "$relative"
        fi
        return 0
    fi
    git -C "$ROOT" diff --quiet -- "$relative" || refuse_dirty "$relative"
    git -C "$ROOT" diff --cached --quiet -- "$relative" || refuse_dirty "$relative"
}

preflight() {
    local phase=$1 computed relative dependency tracked
    computed=$(cd "$ROOT" && python3 "$PYTHON" destinations "${selection[@]}" --root "$ROOT")
    destinations=()
    targets=()
    while IFS= read -r relative; do
        [ -n "$relative" ] || continue
        case "$relative" in
            /* | .. | ../* | */../* | */..) refuse "non-checkout-relative destination: $relative" ;;
        esac
        destinations+=("$relative")
        targets+=("$ROOT/$relative")
        remember_input "$relative"
        check_ancestors "$relative"
    done <<<"$computed"
    [ "${#destinations[@]}" -gt 0 ] || refuse 'the stager reported no destinations'
    for dependency in "${BASE_INPUTS[@]}"; do
        [ -e "$ROOT/images/$dependency" ] || continue
        while IFS= read -r tracked; do
            [ -n "$tracked" ] || continue
            remember_input "$tracked"
        done < <(git -C "$ROOT" ls-files -- "images/$dependency")
    done
    while IFS= read -r relative; do
        check_clean "$relative"
    done <"$inputs_file"
    echo "parent-pins: preflight ($phase) ok: ${#destinations[@]} destinations"
}

snapshot_inputs() {
    local target=$1 relative
    : >"$target"
    while IFS= read -r relative; do
        if [ -e "$ROOT/$relative" ] || [ -L "$ROOT/$relative" ]; then
            ( cd "$ROOT" && sha256sum -- "$relative" ) >>"$target"
        else
            printf 'absent  %s\n' "$relative" >>"$target"
        fi
    done <"$inputs_file"
}

# --- proposal ---------------------------------------------------------------

stage_candidates() {
    stage_dir=$work/stage
    ( cd "$ROOT" && python3 "$PYTHON" stage "${selection[@]}" --root "$ROOT" --out "$stage_dir" )
    [ -s "$stage_dir/destinations.json" ] || refuse 'the stager wrote no destination list'
    jq -e 'type == "array" and length > 0' "$stage_dir/destinations.json" >/dev/null ||
        refuse 'the stager destination list is malformed'
    local relative staged
    local -a reported=()
    while IFS= read -r relative; do
        case " ${destinations[*]} " in
            *" $relative "*) ;;
            *) refuse "the stager reported an unplanned destination: $relative" ;;
        esac
        staged=$stage_dir/staged/$relative
        [ -f "$staged" ] && [ ! -L "$staged" ] ||
            refuse "the stager reported a missing or non-regular file: $relative"
        reported+=("$relative")
    done < <(jq -r '.[]' "$stage_dir/destinations.json")
    [ "${#reported[@]}" -eq "${#destinations[@]}" ] ||
        refuse 'the stager destination list is incomplete'
    for relative in "${destinations[@]}"; do
        case " ${reported[*]-} " in
            *" $relative "*) ;;
            *) refuse "the stager did not stage a planned destination: $relative" ;;
        esac
    done
}

print_proposal() {
    cat "$stage_dir/summary.txt"
    local relative staged
    for relative in "${destinations[@]}"; do
        staged=$stage_dir/staged/$relative
        if [ -e "$ROOT/$relative" ]; then
            diff -u --label "a/$relative" --label "b/$relative" "$ROOT/$relative" "$staged" || true
        else
            diff -u --label "a/$relative" --label "b/$relative" /dev/null "$staged" || true
        fi
    done
}

unchanged() {
    local relative
    for relative in "${destinations[@]}"; do
        if [ ! -e "$ROOT/$relative" ] || ! cmp -s "$stage_dir/staged/$relative" "$ROOT/$relative"; then
            return 1
        fi
    done
    return 0
}

create_owned_directory() {
    : >"$work/created-directories"
    if [ -e "$ROOT/$OWNED_DIRECTORY" ] || [ -L "$ROOT/$OWNED_DIRECTORY" ]; then
        return 0
    fi
    local planned=false relative
    for relative in "${destinations[@]}"; do
        case "$relative" in "$OWNED_DIRECTORY"/*) planned=true ;; esac
    done
    [ "$planned" = true ] || return 0
    # One component at a time, never a blind mkdir -p. The chain passed
    # preflight; recheck it here, immediately before any backup or copy.
    local parent=${OWNED_DIRECTORY%/*}
    if [ ! -d "$ROOT/$parent" ] || [ -L "$ROOT/$parent" ]; then
        refuse "cannot create $OWNED_DIRECTORY below a missing or symlinked parent"
    fi
    mkdir "$ROOT/$OWNED_DIRECTORY" || refuse "cannot create $OWNED_DIRECTORY"
    if [ ! -d "$ROOT/$OWNED_DIRECTORY" ] || [ -L "$ROOT/$OWNED_DIRECTORY" ]; then
        refuse "created directory is not a real directory: $OWNED_DIRECTORY"
    fi
    echo "$OWNED_DIRECTORY" >>"$work/created-directories"
}

write_recovery() {
    local recovery=$work/recovery.json index entry first
    {
        printf '{ "work": "%s", "destinations": [' "$work"
        for index in "${!destinations[@]}"; do
            [ "$index" -eq 0 ] || printf ', '
            printf '{ "path": "%s", "existed": "%s", "backup": "%s" }' \
                "${destinations[index]}" "${existed[index]}" "$index"
        done
        printf ' ], "created_directories": ['
        first=1
        while IFS= read -r entry; do
            [ -n "$entry" ] || continue
            [ "$first" -eq 1 ] || printf ', '
            printf '"%s"' "$entry"
            first=0
        done <"$work/created-directories"
        printf ' ] }\n'
    } >"$recovery"
    [ -s "$recovery" ] || refuse 'cannot record the recovery plan'
}

# --- run --------------------------------------------------------------------

# Serialize writers through the git directory, not $ROOT/.git: in a linked
# worktree `.git` is a file, and `mkdir` there reports a misleading held lock.
git_dir=$(git -C "$ROOT" rev-parse --absolute-git-dir 2>/dev/null) || {
    echo "parent-pins: cannot locate the git directory of $ROOT" >&2
    exit 1
}
# The traps come before the lock and before the work directory: everything from
# here on can fail or be interrupted, and a lock this invocation took must be
# released on every one of those paths.
trap on_exit EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM
trap 'on_signal 129' HUP
lock=$git_dir/parent-pins.lock
if ! mkdir "$lock" 2>/dev/null; then
    echo "parent-pins: another refresh holds $lock; remove it only if no refresh is running" >&2
    exit 1
fi
owns_lock=true
allocate_work

preflight before-network
# The stager refuses every mixed adoption state; this adds the one condition it
# cannot see: an inventory that is still committed but deleted from the worktree.
if [ "$bootstrap" = true ] && git -C "$ROOT" cat-file -e "HEAD:$INVENTORY" 2>/dev/null; then
    refuse 'partial adoption; restore or recover before refreshing'
fi
snapshot_inputs "$work/inputs.before"
stage_candidates
print_proposal
if unchanged; then
    echo "parent-pins: unchanged (${selection[*]}); nothing published"
    exit 0
fi
# Nothing may change while the proposal is reviewed: revalidate the whole plan
# and the exact bytes seen before staging, then publish.
preflight before-publication
snapshot_inputs "$work/inputs.after"
if ! cmp -s "$work/inputs.before" "$work/inputs.after"; then
    refuse 'a checked input changed during resolution; rerun the refresh'
fi
create_owned_directory

existed=()
for index in "${!targets[@]}"; do
    if [ -e "${targets[index]}" ] || [ -L "${targets[index]}" ]; then
        existed[index]=yes
    else
        existed[index]=no
    fi
done
write_recovery

pairs=()
for index in "${!targets[@]}"; do
    pairs+=("$stage_dir/staged/${destinations[index]}" "${targets[index]}")
done
status=0
publishing=1
publish_transactional "$backup" "${pairs[@]}" || status=$?
published=1
case "$status" in
    0 | 1) keep_recovery=false ;;
    *)
        keep_recovery=true
        echo "error: publication result undecided; recovery.json and publish-backup are retained" >&2
        echo "STOP: recover $work before further edits/builds/commits" >&2
        ;;
esac
decided=1
publishing=0
[ "$status" -eq 0 ] || exit "$status"

echo "parent-pins: published (${selection[*]}):"
for relative in "${destinations[@]}"; do
    echo "  $relative"
done
cat <<'EOF'

Review before committing:
  git diff -- images examples
  python3 script/test/parent-pins.py
  bash script/test/contracts.sh
  bash images/build.sh base
  bash images/build.sh standard
EOF
