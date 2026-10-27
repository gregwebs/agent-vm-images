#!/usr/bin/env bash
# Maintainer-host writer for the go-dev example's committed gopls closure (#103).
#
# This is the only writer of examples/layers/go-dev/gopls-closure/ and of the
# example's ARG GOPLS_VERSION assertion. Ordinary builds verify those committed
# bytes; they never resolve a gopls version, regenerate the module graph or
# refresh a checksum. Every run stages the whole two-architecture closure with the
# layer's pinned Go and publishes the fixed destination list as one transaction.
# Rollback only ever restores or removes paths this run backed up or created.
#
# A run that changes the reviewed scope prints the complete proposal and refuses
# until a human supplies --review-determination YYYY-MM-DD --reviewer NAME for the
# day they actually reviewed it. The command never invents that date and never
# treats its own success as approval.
#
# Status: 0 published or unchanged; 1 failed (sources unchanged or fully
# restored); 2 usage; 3 undecided or incomplete recovery ($work retained and
# printed); 128+N when a signal arrives and the sources were safely decided.
set -euo pipefail
# Ordinary checks must not write generated state into the checkout: every
# Python child (including the test launcher) inherits this.
export PYTHONDONTWRITEBYTECODE=1
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
# shellcheck source=/dev/null
. "$ROOT/script/build/transactional-publish.sh"

UPDATER=script/dev/gopls-closure-update.py
EXAMPLE=examples/layers/go-dev
DATA=$EXAMPLE/gopls-closure
DOCKERFILE=$EXAMPLE/Dockerfile

usage() {
    cat >&2 <<'USAGE'
usage: upgrade-gopls.sh VERSION [--review-determination YYYY-MM-DD --reviewer NAME]
                               [--proposal DIR]
       upgrade-gopls.sh --check

Stage golang.org/x/tools/gopls VERSION for linux/amd64 and linux/arm64 with the
Go version the example pins, and publish the reviewed module closure, its
licenses and the Dockerfile assertion as one transaction. The review flags are
required whenever the reviewed scope changes (a new module graph, sums, notice
layout or policy); a rerun of the committed version changes nothing. --proposal
binds renewed review to a proposal this command retained earlier and refuses if
those bytes changed. --check verifies the committed bytes offline and publishes
nothing.

Prerequisites: python3 >= 3.11, go >= 1.24, network access. Never in CI.
USAGE
    exit 2
}

refuse() {
    echo "gopls-closure: refusing $*" >&2
    exit 1
}

# --- signal and exit state --------------------------------------------------

publishing=0 published=0 decided=0 status=0 keep_recovery=false
work='' backup='' stage_dir='' scratch_root=''
existed=() destinations=() targets=()

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

# The fixed directories this writer may create. A left-over empty parent after a
# restored failure would make the next first adoption look like a partial pinned
# state, so cleanup only ever removes directories rmdir reports as empty.
owned_directories=("$DATA")

remove_owned_directories() {
    local index=${#owned_directories[@]}
    [ -f "$work/owned-directories" ] || return 0
    while [ "$index" -gt 0 ]; do
        index=$((index - 1))
        rmdir "$ROOT/${owned_directories[index]}" 2>/dev/null || true
    done
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
    else
        : # Staging or directory creation: on_exit removes this run's empty directories.
    fi
    if [ "$keep_recovery" = true ]; then
        echo "STOP: recover $work before further edits/builds/commits" >&2
    fi
    exit "$code"
}

on_exit() {
    local rc=$?
    if [ "$keep_recovery" = false ]; then
        # Status 0/1 and pre-publication exits are settled: the sources are
        # published, unchanged or fully restored, so this invocation's own empty
        # directories go before the ledger that names them.
        remove_owned_directories
        if [ -n "$work" ] && [ -f "$work/.upgrade-gopls" ]; then
            rm -rf "$work"
        fi
    fi
    exit "$rc"
}

allocate_work() {
    # scratch_base is called directly, not in $(...): a refusal inside it must
    # abort this script, not only the command substitution. With set -e, the
    # only way to keep that is to have the base computed as a side effect.
    scratch_base
    work=$(mktemp -d "$scratch_root/upgrade-gopls.XXXXXX")
    work=$(cd "$work" && pwd -P)
    : >"$work/.upgrade-gopls"
    backup=$work/publish-backup
}

# --- arguments --------------------------------------------------------------

version='' reviewed_on='' reviewer='' proposal='' check_only=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --check)
            [ "$#" -eq 1 ] || usage
            check_only=true
            shift
            ;;
        --review-determination) [ "$#" -ge 2 ] || usage; reviewed_on=$2; shift 2 ;;
        --reviewer) [ "$#" -ge 2 ] || usage; reviewer=$2; shift 2 ;;
        --proposal) [ "$#" -ge 2 ] || usage; proposal=$2; shift 2 ;;
        -*) usage ;;
        *)
            [ -z "$version" ] || usage
            version=$1
            shift
            ;;
    esac
done
if [ "$check_only" = false ]; then
    [ -n "$version" ] || usage
    # An exact release only: a range, a branch name or `latest` would make the
    # committed record describe a moving target.
    printf '%s' "$version" | grep -Eqx 'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)' ||
        { echo "gopls-closure: refusing a version other than exact vX.Y.Z: $version" >&2; exit 2; }
    if [ -n "$reviewed_on" ] || [ -n "$reviewer" ]; then
        [ -n "$reviewed_on" ] && [ -n "$reviewer" ] || usage
        printf '%s' "$reviewed_on" | grep -Eqx '[0-9]{4}-[0-9]{2}-[0-9]{2}' ||
            { echo "gopls-closure: refusing a review date other than YYYY-MM-DD: $reviewed_on" >&2; exit 2; }
    fi
    [ "${GITHUB_ACTIONS:-}" != true ] || {
        echo 'gopls-closure: refusing GitHub Actions; pin maintenance is maintainer-host work' >&2
        exit 2
    }
fi

# --- prerequisites ----------------------------------------------------------

# Absolute executables, resolved before any private environment is built: a
# Homebrew or ~/.local path is fine for maintenance, but the child PATH is then
# pinned to exactly these tool directories plus the base system.
resolve_tool() {
    local name=$1 path
    path=$(command -v "$name") || { echo "missing prerequisite: $name" >&2; exit 1; }
    case "$path" in
        /*) ;;
        *) path=$(cd "$(dirname "$path")" && pwd)/$(basename "$path") ;;
    esac
    printf '%s\n' "$path"
}
PYTHON=$(resolve_tool python3)
GO=$(resolve_tool go)
for tool in sha256sum cmp; do command -v "$tool" >/dev/null || { echo "missing prerequisite: $tool" >&2; exit 1; }; done
child_path=$(dirname "$PYTHON"):$(dirname "$GO"):/usr/bin:/bin

# --- fixed destination list -------------------------------------------------

add_destination() { destinations+=("$1"); }

dockerfile_arg() {
    local name=$1 count value
    count=$(grep -cE "^ARG ${name}=" "$ROOT/$DOCKERFILE" || true)
    [ "$count" -eq 1 ] || refuse "expected exactly one ARG ${name} line in $DOCKERFILE"
    value=$(sed -nE "s/^ARG ${name}=(.*)$/\1/p" "$ROOT/$DOCKERFILE")
    printf '%s\n' "$value"
}

build_destinations() {
    destinations=()
    add_destination "$DATA/go.mod"
    add_destination "$DATA/go.sum"
    add_destination "$DATA/pin.json"
    add_destination "$DATA/gopls.go.mod"
    add_destination "$DATA/THIRD_PARTY_LICENSES"
    add_destination "$DOCKERFILE"
}

# --- --check ----------------------------------------------------------------

if [ "$check_only" = true ]; then
    build_destinations
    [ -d "$ROOT/$DATA" ] || refuse "to continue without committed gopls closure data at $DATA"
    ( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" check "$DATA" >/dev/null ) || exit $?
    current=$( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" current "$DATA" )
    expected_gopls=$(dockerfile_arg GOPLS_VERSION)
    expected_go=$(dockerfile_arg GO_VERSION)
    for line in $current; do
        case "$line" in
            gopls=*)
                [ "${line#gopls=}" = "$expected_gopls" ] ||
                    refuse "Dockerfile ARG GOPLS_VERSION=$expected_gopls does not equal the pinned ${line#gopls=}"
                ;;
            go=*)
                [ "${line#go=}" = "$expected_go" ] ||
                    refuse "Dockerfile ARG GO_VERSION=$expected_go does not equal the pinned pin ${line#go=}"
                ;;
        esac
    done
    echo "gopls-closure: committed closure verified"
    printf '%s\n' "$current"
    exit 0
fi

# --- preflight --------------------------------------------------------------

for tool in "$PYTHON" "$GO"; do
    [ -x "$tool" ] || { echo "not executable: $tool" >&2; exit 1; }
done
# The pinned compiler is the Dockerfile's, never an ambient default: a Go bump
# therefore forces this writer to run again before the closure can be adopted.
pin_go=$(dockerfile_arg GO_VERSION)

# Every ancestor must already be a real directory inside the checkout; a
# symlinked component would silently redirect publication outside it. Only the
# directory this operation owns may be missing.
check_ancestors() {
    local relative=$1 rest prefix='' component absolute
    rest=$relative
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
            case "$prefix" in
                "$DATA") ;;
                *) refuse "missing destination ancestor: $prefix" ;;
            esac
        fi
    done
    if [ -L "$ROOT/$relative" ]; then refuse "symlinked destination: $relative"; fi
}

# Partial pinned state is refused: either the committed set verifies, or nothing
# of it is present yet (first adoption). A complete but uncommitted proposal also
# verifies, which is what makes a repeated run idempotent.
preflight() {
    local relative
    build_destinations
    for relative in "${destinations[@]}"; do
        check_ancestors "$relative"
    done
    if [ -e "$ROOT/$DATA" ] || [ -L "$ROOT/$DATA" ]; then
        [ -d "$ROOT/$DATA" ] || refuse "non-directory destination: $DATA"
        if ! ( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" check "$DATA" >/dev/null ); then
            refuse 'partial or unrecognized gopls closure data; recover or restore it before publishing'
        fi
    fi
}

snapshot_destinations() {
    local target=$1 relative
    : >"$target"
    for relative in "${destinations[@]}"; do
        if [ -e "$ROOT/$relative" ] || [ -L "$ROOT/$relative" ]; then
            ( cd "$ROOT" && sha256sum -- "$relative" ) >>"$target"
        else
            printf 'absent  %s\n' "$relative" >>"$target"
        fi
    done
}

create_owned_directories() {
    local relative
    # Recorded before creating anything: a signal between two mkdir calls must
    # still have the complete list, or the parent is left behind.
    printf '%s\n' "${owned_directories[@]}" >"$work/owned-directories"
    for relative in "${owned_directories[@]}"; do
        if [ -e "$ROOT/$relative" ] || [ -L "$ROOT/$relative" ]; then
            [ -d "$ROOT/$relative" ] && [ ! -L "$ROOT/$relative" ] ||
                refuse "not a real directory: $relative"
            continue
        fi
        mkdir "$ROOT/$relative" || refuse "cannot create $relative"
    done
}

# The scratch root must resolve outside the checkout: a retained proposal inside it
# would be untracked repository state that the transactional publisher never owns.
scratch_base() {
    # Sets scratch_root; every refusal here is a direct `exit 1`, which aborts
    # the whole script because callers invoke this function directly rather than
    # through $(...) (where exit would only end the substitution subshell).
    local base real_base real_root
    base=${TMPDIR:-/tmp}
    base=${base%/}
    case "$base" in
        /*) ;;
        *) refuse "TMPDIR must be an absolute path: $base" ;;
    esac
    [ -d "$base" ] || refuse "TMPDIR is not a directory: $base"
    real_base=$(cd "$base" && pwd -P) || refuse "cannot resolve TMPDIR: $base"
    real_root=$(cd "$ROOT" && pwd -P) || refuse "cannot resolve the repository root"
    case "$real_base/" in
        "$real_root"/*) refuse "to work inside the repository: TMPDIR is $real_base" ;;
    esac
    scratch_root=$real_base
}

# A renewal refusal must leave the exact bytes to review. The stager writes them
# under --out (which is this run's work directory), so they are copied out before
# on_exit removes it; nothing is published into the checkout.
retain_proposal() {
    local kept
    scratch_base
    kept=$(mktemp -d "$scratch_root/gopls-closure-proposal.XXXXXX")
    cp -R "$stage_dir/gopls-closure" "$kept/gopls-closure" ||
        refuse "cannot retain the proposal in $kept"
    cp "$stage_dir/Dockerfile" "$kept/Dockerfile" ||
        refuse "cannot retain the proposed Dockerfile in $kept"
    cp "$work/stage.log" "$kept/proposal.txt" || true
    printf '%s\n' "$kept"
}

print_proposal() {
    cat "$stage_dir/summary.txt"
    echo
    echo 'Publishing does not approve anything; review the diff before committing:'
    echo "  git diff -- $DATA $DOCKERFILE"
    echo
}

write_recovery() {
    local recovery=$work/recovery.json index first entry
    {
        printf '{ "work": "%s", "destinations": [' "$work"
        for index in "${!destinations[@]}"; do
            [ "$index" -eq 0 ] || printf ', '
            printf '{ "path": "%s", "existed": "%s", "backup": "%s" }' \
                "${destinations[index]}" "${existed[index]}" "$index"
        done
        printf ' ], "owned_directories": ['
        first=1
        while IFS= read -r entry; do
            [ -n "$entry" ] || continue
            [ "$first" -eq 1 ] || printf ', '
            printf '"%s"' "$entry"
            first=0
        done <"$work/owned-directories"
        printf ' ] }\n'
    } >"$recovery"
    [ -s "$recovery" ] || refuse 'cannot record the recovery plan'
}

# --- run --------------------------------------------------------------------

trap on_exit EXIT
trap 'on_signal 130' INT
trap 'on_signal 143' TERM
trap 'on_signal 129' HUP
allocate_work

preflight
snapshot_destinations "$work/inputs.before"
stage_dir=$work/stage
stage_arguments=(stage --gopls "$version" --go "$pin_go" --current "$ROOT/$DATA" --out "$stage_dir")
if [ -n "$reviewed_on" ]; then stage_arguments+=(--review-determination "$reviewed_on"); fi
if [ -n "$reviewer" ]; then stage_arguments+=(--reviewer "$reviewer"); fi
status=0
( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" "${stage_arguments[@]}" ) \
    >"$work/stage.log" 2>&1 || status=$?
cat "$work/stage.log"
if [ "$status" -ne 0 ]; then
    if [ "$status" -eq 1 ] && [ -f "$stage_dir/gopls-closure/pin.proposal.json" ]; then
        kept=$(retain_proposal)
        {
            echo "gopls-closure: proposal retained at $kept"
            echo '  review those exact bytes, then rerun the renewal against them:'
            echo "    bash $0 $version --review-determination YYYY-MM-DD --reviewer NAME \\"
            echo "      --proposal $kept"
        } >&2
    fi
    exit "$status"
fi

if [ -n "$proposal" ]; then
    [ -d "$proposal" ] || refuse "--proposal is not a directory: $proposal"
    [ -f "$proposal/gopls-closure/pin.proposal.json" ] ||
        refuse "--proposal is not a retained proposal (missing pin.proposal.json): $proposal"
    [ -f "$proposal/Dockerfile" ] ||
        refuse "--proposal is not a retained proposal (missing Dockerfile): $proposal"
fi

unchanged=true
for relative in "${destinations[@]}"; do
    staged=$stage_dir/gopls-closure/${relative#"$DATA"/}
    if [ "$relative" = "$DOCKERFILE" ]; then staged=$stage_dir/Dockerfile; fi
    if [ ! -f "$staged" ] || [ -L "$staged" ]; then
        refuse "the stager did not produce $relative"
    fi
    if [ ! -e "$ROOT/$relative" ] || ! cmp -s "$staged" "$ROOT/$relative"; then
        unchanged=false
    fi
done
if [ "$unchanged" = true ]; then
    echo "gopls-closure: unchanged ($version); nothing published"
    exit 0
fi

if [ -n "$proposal" ]; then
    [ -d "$proposal" ] || refuse "--proposal is not a directory: $proposal"
    ( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" compare \
        "$proposal" "$stage_dir" ) || exit $?
fi

print_proposal
# Nothing may change while the proposal is reviewed: revalidate the committed
# set and the exact bytes seen before staging, then publish.
preflight
snapshot_destinations "$work/inputs.after"
if ! cmp -s "$work/inputs.before" "$work/inputs.after"; then
    refuse 'a destination changed during staging; rerun the upgrade'
fi
create_owned_directories

existed=()
targets=()
for index in "${!destinations[@]}"; do
    targets+=("$ROOT/${destinations[index]}")
    if [ -e "$ROOT/${destinations[index]}" ] || [ -L "$ROOT/${destinations[index]}" ]; then
        existed[index]=yes
    else
        existed[index]=no
    fi
done
write_recovery

pairs=()
for index in "${!destinations[@]}"; do
    relative=${destinations[index]}
    staged=$stage_dir/gopls-closure/${relative#"$DATA"/}
    if [ "$relative" = "$DOCKERFILE" ]; then staged=$stage_dir/Dockerfile; fi
    pairs+=("$staged" "${targets[index]}")
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

echo "gopls-closure: published ($version):"
for relative in "${destinations[@]}"; do
    echo "  $relative"
done
cat <<'EOF'

Next gates for this change:
  bash script/dev/upgrade-gopls.sh --check
  python3 examples/layers/go-dev/gopls_closure.py check \
    examples/layers/go-dev/gopls-closure --expect-gopls VERSION --expect-go GOVERSION
  python3 script/dev/gopls-closure-update.py compare PROPOSAL_DIR DATA_DIR
  python3 script/test/gopls-closure-pins.py --go-root PATH
  python3 script/test/gopls-closure-maintenance.py --go-root PATH
  bash script/test/example-layers.sh --self-test
  bash script/test/contracts.sh
EOF
