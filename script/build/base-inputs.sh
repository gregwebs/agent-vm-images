#!/usr/bin/env bash
# Host generator and drift gate for the committed repository base-input manifest
# (#95).
#
# The base image can only re-hash the entries a recipe names, so it cannot
# discover a tracked input that a manifest forgot. That completeness question is
# a host/CI question and lives here: the tracked set comes from git, and the
# manifest written here is what `images/verify-base-inputs.sh` verifies inside
# the base and standard builds and what the copied extension compares against
# its committed default. Ordinary builds never run this; editing a listed input
# means regenerating, reviewing and rebuilding.
#
# Status: 0 ok or published/unchanged, 1 drift, a missing prerequisite or a failed
# publication (sources unchanged or fully restored), 2 usage, 3 undecided or
# incomplete recovery (the work directory is retained and printed).
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
# shellcheck source=/dev/null
. "$ROOT/script/build/transactional-publish.sh"

MANIFEST=images/base-inputs.sha256
EXTENSION=examples/base-extension/Dockerfile
IDENTITY_ARG=AGENT_VM_BASE_IDENTITY
# Every images/ entry the base image consumes, and nothing else. Each listed
# root must contribute at least one tracked file; the manifest, the mount list in
# both recipes and this array change together.
BASE_INPUT_ROOTS=(.dockerignore Dockerfile apt-policy install-build-ca.sh install-pnpm.sh
                  install-zellij.sh pnpm recipe-contract verify-base-inputs.sh)

usage() {
    cat >&2 <<'USAGE'
usage: bash script/build/base-inputs.sh inputs
       bash script/build/base-inputs.sh --check
       bash script/build/base-inputs.sh --write
       bash script/build/base-inputs.sh --stage TREE OUT

inputs  print the images-relative input roots, one per line.
--check render the manifest and the extension default; fail if they differ from
        the committed ones (names the differing files).
--write render both and publish them transactionally with a full diff.
--stage render both from TREE (an images/-shaped directory) into the new
        directory OUT instead of publishing. Used by the parent-pin refresh.

New inputs must be git-added before a render: the tracked set is the selection.
USAGE
    exit 2
}

refuse() {
    echo "base-inputs: $*" >&2
    exit 1
}

drift() {
    echo 'base-inputs: inputs/default drift; run: bash script/build/base-inputs.sh --write' >&2
    cat "$1" >&2
    exit 1
}

# The manifest is a sha256sum checklist and stock macOS ships only `shasum`, so
# refuse before a render can fail with a bare 127 from a subshell.
if ! command -v sha256sum >/dev/null 2>&1; then
    refuse 'sha256sum (GNU coreutils) is not on PATH; see CONTRIBUTING.md prerequisites'
fi

scratch() {
    local directory
    directory=$(mktemp -d "${TMPDIR:-/tmp}/base-inputs.XXXXXX")
    ( cd "$directory" && pwd -P )
}

# --- tracked selection ------------------------------------------------------

tracked_listing() {
    local -a pathspecs=() root
    for root in "${BASE_INPUT_ROOTS[@]}"; do
        pathspecs+=("images/$root")
    done
    git -C "$ROOT" ls-files -s -z -- "${pathspecs[@]}"
}

# Rejected rather than normalized: a normalized path could name a different
# file than the one the index records.
validate_path() {
    local path=$1 rest component
    case "$path" in
        '' | /*) refuse "unsupported base input path: $path" ;;
        *[!A-Za-z0-9._/-]*) refuse "unsupported character in base input path: $path" ;;
    esac
    rest=$path
    while :; do
        component=${rest%%/*}
        case "$component" in
            '' | . | ..) refuse "unsupported base input path component in: $path" ;;
        esac
        [ "$component" != "$rest" ] || break
        rest=${rest#*/}
    done
}

under_roots() {
    local relative=$1 root
    for root in "${BASE_INPUT_ROOTS[@]}"; do
        case "$relative" in
            "$root" | "$root"/*) return 0 ;;
        esac
    done
    return 1
}

# The tree is hashed, not the working tree: a caller that supplies an overlay
# must be able to prove the identity of exactly those bytes.
check_tree_file() {
    local tree=$1 relative=$2 rest=$2 prefix='' component
    while [ "${rest%%/*}" != "$rest" ]; do
        component=${rest%%/*}
        rest=${rest#*/}
        prefix=${prefix:+$prefix/}$component
        [ ! -L "$tree/$prefix" ] || refuse "symlinked base input component: $prefix"
        [ -d "$tree/$prefix" ] || refuse "missing base input directory: $prefix"
    done
    [ ! -L "$tree/$relative" ] || refuse "symlinked base input: $relative"
    [ -f "$tree/$relative" ] || refuse "missing or non-regular base input: $relative"
}

render_manifest() {
    local tree=$1 record meta path relative previous='' hash root found
    local -a paths=() sorted=()
    [ -d "$tree" ] || refuse "not a directory: $tree"
    while IFS= read -r -d '' record; do
        meta=${record%$'\t'*}
        path=${record##*$'\t'}
        case "${meta%% *}" in
            100644 | 100755) ;;
            *) refuse "tracked base input is not a regular file: $path" ;;
        esac
        [ "${meta##* }" = 0 ] || refuse "tracked base input is not at index stage 0: $path"
        case "$path" in
            images/*) relative=${path#images/} ;;
            *) refuse "tracked base input is outside images/: $path" ;;
        esac
        validate_path "$relative"
        under_roots "$relative" ||
            refuse "tracked file does not belong to a base input root: $relative"
        check_tree_file "$tree" "$relative"
        paths+=("$relative")
    done < <(tracked_listing)
    [ "${#paths[@]}" -gt 0 ] || refuse 'the tracked base input set is empty'
    while IFS= read -r relative; do
        sorted+=("$relative")
    done < <(printf '%s\n' "${paths[@]}" | LC_ALL=C sort)
    for root in "${BASE_INPUT_ROOTS[@]}"; do
        found=false
        for relative in "${sorted[@]}"; do
            case "$relative" in
                "$root" | "$root"/*) found=true; break ;;
            esac
        done
        [ "$found" = true ] || refuse "base input root contributes no tracked file: $root"
    done
    for relative in "${sorted[@]}"; do
        [ "$relative" != "$previous" ] || refuse "duplicate base input: $relative"
        previous=$relative
        hash=$(sha256sum <"$tree/$relative")
        printf '%s  %s\n' "${hash%% *}" "$relative"
    done
}

manifest_identity() {
    local hash
    hash=$(sha256sum <"$1")
    printf 'sha256:%s\n' "${hash%% *}"
}

render_extension() {
    local identity=$1 count
    printf '%s' "$identity" | grep -Eqx 'sha256:[0-9a-f]{64}' ||
        refuse "not a base identity: $identity"
    count=$(grep -cE "^[Aa][Rr][Gg][[:space:]]+$IDENTITY_ARG=" "$ROOT/$EXTENSION" || true)
    [ "$count" = 1 ] ||
        refuse "expected exactly one global $IDENTITY_ARG default in $EXTENSION, found $count"
    awk -v arg="$IDENTITY_ARG" -v identity="$identity" '
        BEGIN { sawfrom = 0 }
        {
            line = $0
            if (!sawfrom && line ~ "^[Ff][Rr][Oo][Mm][ \t]") sawfrom = 1
            if (!sawfrom && line ~ ("^[Aa][Rr][Gg][ \t]+" arg "=")) sub("=.*", "=" identity, line)
            print line
        }
    ' "$ROOT/$EXTENSION"
}

committed_extension_identity() {
    grep -E "^[Aa][Rr][Gg][[:space:]]+$IDENTITY_ARG=" "$ROOT/$EXTENSION" 2>/dev/null |
        head -n 1 | cut -d= -f2- || true
}

# --- comparison ------------------------------------------------------------

manifest_drift() {
    awk '
        FNR == NR { if ($0 != "") wanted[substr($0, 67)] = substr($0, 1, 64); next }
        { if ($0 != "") found[substr($0, 67)] = substr($0, 1, 64) }
        END {
            for (path in wanted) {
                if (!(path in found)) print "  new: " path
                else if (wanted[path] != found[path]) print "  changed: " path
            }
            for (path in found) if (!(path in wanted)) print "  missing: " path
        }
    ' "$1" "$2" | LC_ALL=C sort
}

check_mode() {
    local work rendered committed identity drift_output
    work=$(scratch)
    # A refusal must not leak the scratch directory either.
    trap 'rm -rf "$work"' EXIT
    rendered=$work/base-inputs.sha256
    drift_output=$work/drift
    render_manifest "$ROOT/images" >"$rendered"
    : >"$drift_output"
    if [ -f "$ROOT/$MANIFEST" ]; then
        manifest_drift "$rendered" "$ROOT/$MANIFEST" >>"$drift_output"
        if ! cmp -s "$rendered" "$ROOT/$MANIFEST" && [ ! -s "$drift_output" ]; then
            printf '  format: %s is not byte-identical to the rendered manifest\n' "$MANIFEST" \
                >>"$drift_output"
        fi
    else
        printf '  missing: %s\n' "$MANIFEST" >>"$drift_output"
    fi
    identity=$(manifest_identity "$rendered")
    committed=$(committed_extension_identity)
    if [ "$identity" != "$committed" ]; then
        printf '  default: %s -> %s in %s\n' "${committed:-absent}" "$identity" "$EXTENSION" \
            >>"$drift_output"
    fi
    [ ! -s "$drift_output" ] || drift "$drift_output"
    echo "base-inputs: inputs and $IDENTITY_ARG default agree ($identity)"
}

# --- publication -----------------------------------------------------------

publishing=0 published=0 keep_recovery=false work='' backup=''
existed_manifest=no existed_extension=no

restore_sources() {
    local status=0
    if [ "$existed_extension" = yes ]; then
        if [ ! -f "$backup/1" ] || ! cp -p "$backup/1" "$ROOT/$EXTENSION"; then status=3; fi
    else
        rm -f "$ROOT/$EXTENSION" || status=3
    fi
    if [ "$existed_manifest" = yes ]; then
        if [ ! -f "$backup/0" ] || ! cp -p "$backup/0" "$ROOT/$MANIFEST"; then status=3; fi
    else
        rm -f "$ROOT/$MANIFEST" || status=3
    fi
    return "$status"
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
            echo "error: publication interrupted; sources restored; recovery material retained at $work" >&2
        else
            code=3
            echo "error: publication interrupted and restore incomplete; recovery material retained at $work" >&2
        fi
    elif [ "$publishing" -eq 1 ]; then
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
    local status=$?
    if [ "$keep_recovery" = false ] && [ -n "$work" ] && [ -d "$work" ]; then
        rm -rf "$work"
    fi
    exit "$status"
}

write_mode() {
    local staged=$work/staged identity status=0
    mkdir "$staged"
    render_manifest "$ROOT/images" >"$staged/base-inputs.sha256"
    identity=$(manifest_identity "$staged/base-inputs.sha256")
    render_extension "$identity" >"$staged/extension.Dockerfile"
    if [ -f "$ROOT/$MANIFEST" ] && cmp -s "$staged/base-inputs.sha256" "$ROOT/$MANIFEST" &&
        cmp -s "$staged/extension.Dockerfile" "$ROOT/$EXTENSION"; then
        echo "base-inputs: unchanged ($identity)"
        return 0
    fi
    if [ -f "$ROOT/$MANIFEST" ]; then
        diff -u --label "a/$MANIFEST" --label "b/$MANIFEST" \
            "$ROOT/$MANIFEST" "$staged/base-inputs.sha256" || true
    else
        diff -u --label "a/$MANIFEST" --label "b/$MANIFEST" /dev/null \
            "$staged/base-inputs.sha256" || true
    fi
    diff -u --label "a/$EXTENSION" --label "b/$EXTENSION" \
        "$ROOT/$EXTENSION" "$staged/extension.Dockerfile" || true
    if [ -f "$ROOT/$MANIFEST" ]; then existed_manifest=yes; fi
    if [ -f "$ROOT/$EXTENSION" ]; then existed_extension=yes; fi
    backup=$work/backup
    publishing=1
    publish_transactional "$backup" \
        "$staged/base-inputs.sha256" "$ROOT/$MANIFEST" \
        "$staged/extension.Dockerfile" "$ROOT/$EXTENSION" || status=$?
    published=1
    publishing=0
    case "$status" in
        0)
            keep_recovery=false
            echo "base-inputs: published ($identity): $MANIFEST $EXTENSION"
            ;;
        1)
            keep_recovery=false
            echo 'error: publication failed; sources restored' >&2
            ;;
        *)
            keep_recovery=true
            echo "error: publication result undecided; backups 0=$MANIFEST 1=$EXTENSION plus .complete; recovery material retained at $work" >&2
            echo "STOP: recover $work before further edits/builds/commits" >&2
            ;;
    esac
    return "$status"
}

stage_mode() {
    local tree=$1 out=$2 identity
    [ ! -e "$out" ] || refuse "refusing an existing output directory: $out"
    mkdir "$out"
    render_manifest "$tree" >"$out/base-inputs.sha256"
    identity=$(manifest_identity "$out/base-inputs.sha256")
    render_extension "$identity" >"$out/extension.Dockerfile"
    echo "base-inputs: staged $identity into $out" >&2
}

# --- CLI -------------------------------------------------------------------

command=${1:-}
case "$command" in
    inputs)
        [ "$#" -eq 1 ] || usage
        printf '%s\n' "${BASE_INPUT_ROOTS[@]}"
        ;;
    --check)
        [ "$#" -eq 1 ] || usage
        check_mode
        ;;
    --write)
        [ "$#" -eq 1 ] || usage
        work=$(scratch)
        trap on_exit EXIT
        trap 'on_signal 130' INT
        trap 'on_signal 143' TERM
        trap 'on_signal 129' HUP
        write_mode
        ;;
    --stage)
        [ "$#" -eq 3 ] || usage
        stage_mode "$2" "$3"
        ;;
    *) usage ;;
esac
