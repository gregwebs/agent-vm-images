#!/usr/bin/env bash
# Maintainer-host writer for the rust-dev example's committed toolchain closure (#102).
#
# This is the only writer of examples/layers/rust-dev/rust-toolchain/ and of the
# example's ARG RUST_TOOLCHAIN assertion. Ordinary builds verify those committed
# bytes; they never resolve a channel, fetch a manifest or refresh a hash. Every
# run stages and authenticates the whole two-architecture closure without
# executing a foreign bootstrap, then publishes the fixed destination list as one
# transaction. Rollback only ever restores or removes paths this run backed up or
# created.
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

UPDATER=script/dev/rust-toolchain-update.py
EXAMPLE=examples/layers/rust-dev
DATA=$EXAMPLE/rust-toolchain
DOCKERFILE=$EXAMPLE/Dockerfile

usage() {
    cat >&2 <<'USAGE'
usage: upgrade-rust-toolchain.sh CHANNEL [--rustup VERSION] [--key PATH]
           [--review-bootstrap-exception YYYY-MM-DD --reviewer NAME]
       upgrade-rust-toolchain.sh --check

Stage the full linux/amd64 + linux/arm64 closure for an exact release channel and
publish the reviewed pins, evidence and the Dockerfile channel assertion as one
transaction. --rustup, --key and the bootstrap-exception review are for the first
adoption; later bumps retain the committed rustup version and key. --check
verifies the committed bytes offline and publishes nothing.

Prerequisites: python3 >= 3.11, gpg, sha256sum, cmp, network access. Never in CI.
USAGE
    exit 2
}

refuse() {
    echo "rust-toolchain: refusing $*" >&2
    exit 1
}

# --- signal and exit state --------------------------------------------------

publishing=0 published=0 decided=0 status=0 keep_recovery=false
work='' backup='' stage_dir=''
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

# The fixed directories this writer may create, parent first. A left-over empty
# parent after a restored failure would make the next first adoption look like a
# partial pinned state, so cleanup walks this list child first and only ever
# removes directories rmdir reports as empty.
owned_directories=("$DATA" "$DATA/evidence")

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
        if [ -n "$work" ] && [ -f "$work/.upgrade-rust-toolchain" ]; then
            rm -rf "$work"
        fi
    fi
    exit "$rc"
}

allocate_work() {
    work=$(mktemp -d "${TMPDIR:-/tmp}/upgrade-rust-toolchain.XXXXXX")
    work=$(cd "$work" && pwd -P)
    : >"$work/.upgrade-rust-toolchain"
    backup=$work/publish-backup
}

# --- arguments --------------------------------------------------------------

channel='' rustup='' key='' reviewed_on='' reviewer='' check_only=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --check)
            [ "$#" -eq 1 ] || usage
            check_only=true
            shift
            ;;
        --rustup) [ "$#" -ge 2 ] || usage; rustup=$2; shift 2 ;;
        --key) [ "$#" -ge 2 ] || usage; key=$2; shift 2 ;;
        --review-bootstrap-exception) [ "$#" -ge 2 ] || usage; reviewed_on=$2; shift 2 ;;
        --reviewer) [ "$#" -ge 2 ] || usage; reviewer=$2; shift 2 ;;
        -*) usage ;;
        *)
            [ -z "$channel" ] || usage
            channel=$1
            shift
            ;;
    esac
done
if [ "$check_only" = false ]; then
    [ -n "$channel" ] || usage
    printf '%s' "$channel" | grep -Eqx '(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)' ||
        { echo "rust-toolchain: refusing a channel other than exact X.Y.Z: $channel" >&2; exit 2; }
    if [ -n "$reviewed_on" ] || [ -n "$reviewer" ]; then
        [ -n "$reviewed_on" ] && [ -n "$reviewer" ] || usage
        printf '%s' "$reviewed_on" | grep -Eqx '[0-9]{4}-[0-9]{2}-[0-9]{2}' ||
            { echo "rust-toolchain: refusing a review date other than YYYY-MM-DD: $reviewed_on" >&2; exit 2; }
    fi
    [ "${GITHUB_ACTIONS:-}" != true ] || {
        echo 'rust-toolchain: refusing GitHub Actions; pin maintenance is maintainer-host work' >&2
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
GPG=$(resolve_tool gpg)
for tool in sha256sum cmp; do command -v "$tool" >/dev/null || { echo "missing prerequisite: $tool" >&2; exit 1; }; done
child_path=$(dirname "$PYTHON"):$(dirname "$GPG"):/usr/bin:/bin

# --- fixed destination list -------------------------------------------------

add_destination() { destinations+=("$1"); }

dockerfile_channel() {
    local count value
    count=$(grep -cE '^ARG RUST_TOOLCHAIN=' "$ROOT/$DOCKERFILE" || true)
    [ "$count" -eq 1 ] || refuse "expected exactly one ARG RUST_TOOLCHAIN line in $DOCKERFILE"
    value=$(sed -nE 's/^ARG RUST_TOOLCHAIN=(.*)$/\1/p' "$ROOT/$DOCKERFILE")
    printf '%s\n' "$value"
}

build_destinations() {
    destinations=()
    add_destination "$DATA/pin.json"
    add_destination "$DATA/channel.toml"
    add_destination "$DATA/channel.toml.sha256"
    add_destination "$DATA/channel.toml.asc"
    add_destination "$DATA/rust-release-key.asc"
    local arch package
    for arch in amd64 arm64; do
        add_destination "$DATA/evidence/rustup-$arch.sha256"
        for package in rustc cargo rust-std clippy-preview rustfmt-preview; do
            add_destination "$DATA/evidence/$arch-$package.asc"
        done
        add_destination "$DATA/evidence/$arch-rust-std-musl.asc"
    done
    add_destination "$DOCKERFILE"
}

# --- --check ----------------------------------------------------------------

if [ "$check_only" = true ]; then
    build_destinations
    [ -d "$ROOT/$DATA" ] || refuse "no committed Rust toolchain data at $DATA"
    ( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" check "$DATA" >/dev/null ) || exit $?
    current=$( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" current "$DATA" )
    expected=$(dockerfile_channel)
    for line in $current; do
        case "$line" in
            channel=*)
                [ "${line#channel=}" = "$expected" ] ||
                    refuse "Dockerfile ARG RUST_TOOLCHAIN=$expected does not equal the pinned channel ${line#channel=}"
                ;;
        esac
    done
    echo "rust-toolchain: committed closure verified"
    printf '%s\n' "$current"
    exit 0
fi

# --- preflight --------------------------------------------------------------

for tool in "$PYTHON" "$GPG"; do
    [ -x "$tool" ] || { echo "not executable: $tool" >&2; exit 1; }
done

# Every ancestor must already be a real directory inside the checkout; a
# symlinked component would silently redirect publication outside it. Only the
# two directories this operation owns may be missing.
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
                "$DATA" | "$DATA/evidence") ;;
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
            refuse 'partial or unrecognized Rust toolchain data; recover or restore it before publishing'
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

print_proposal() {
    cat "$stage_dir/summary.txt"
    echo
    echo 'Review before publishing is automatic; review the diff before committing:'
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
stage_arguments=(stage --channel "$channel" --current "$ROOT/$DATA" --out "$stage_dir")
if [ -n "$rustup" ]; then stage_arguments+=(--rustup "$rustup"); fi
if [ -n "$key" ]; then stage_arguments+=(--key "$key"); fi
if [ -n "$reviewed_on" ]; then stage_arguments+=(--verify-bootstrap-exception "$reviewed_on"); fi
if [ -n "$reviewer" ]; then stage_arguments+=(--reviewer "$reviewer"); fi
status=0
( cd "$ROOT" && PATH=$child_path "$PYTHON" "$UPDATER" "${stage_arguments[@]}" ) \
    >"$work/stage.log" 2>&1 || status=$?
cat "$work/stage.log"
[ "$status" -eq 0 ] || exit "$status"
cp "$work/stage.log" "$stage_dir/summary.txt"

unchanged=true
for relative in "${destinations[@]}"; do
    staged=$stage_dir/rust-toolchain/${relative#"$DATA"/}
    if [ "$relative" = "$DOCKERFILE" ]; then staged=$stage_dir/Dockerfile; fi
    if [ ! -f "$staged" ] || [ -L "$staged" ]; then
        refuse "the stager did not produce $relative"
    fi
    if [ ! -e "$ROOT/$relative" ] || ! cmp -s "$staged" "$ROOT/$relative"; then
        unchanged=false
    fi
done
if [ "$unchanged" = true ]; then
    echo "rust-toolchain: unchanged ($channel); nothing published"
    exit 0
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
    staged=$stage_dir/rust-toolchain/${relative#"$DATA"/}
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

echo "rust-toolchain: published ($channel):"
for relative in "${destinations[@]}"; do
    echo "  $relative"
done
cat <<'EOF'

Next gates for this change:
  bash script/dev/upgrade-rust-toolchain.sh --check
  python3 examples/layers/rust-dev/rust_toolchain.py check \
    examples/layers/rust-dev/rust-toolchain --expect-channel CHANNEL
  python3 script/test/rust-toolchain-pins.py
  python3 script/test/rust-toolchain-maintenance.py
  bash script/test/example-layers.sh --self-test
  bash script/test/contracts.sh
EOF
