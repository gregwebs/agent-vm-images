#!/bin/bash
# Canonical recipe-contract build recipe.
#
# Both delivery adapters call this: the Docker builder stage (composed directory at
# /out) and script/build/recipe-contract.sh (composed directory at
# target/recipe-contract/OS-ARCH/out). It is invocation-only, it owns the compiler
# flags so the adapters cannot diverge, and it publishes the composed directory the
# image used to bind-mount: the helper sources plus the executable.
set -euo pipefail

usage() {
    echo 'usage: bash images/recipe-contract/build.sh ABSOLUTE_COMPOSED_DIRECTORY' >&2
    exit 2
}

[ "$#" -eq 1 ] || usage
output=$1
case "$output" in
    /*) ;;
    *) usage ;;
esac
module="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
case "$output" in
    "$module"/*) echo "composed directory must be outside the Go module: $output" >&2; exit 2 ;;
esac

# The publish step replaces only a directory this recipe wrote, so the marker is
# the ownership proof: deleting or renaming over anything else would destroy a
# caller's tree (`build.sh "$HOME"` is one typo away).
marker=.recipe-contract-composed

# The composed directory is mounted read-only into audit containers and read by
# numeric users, so publication cannot inherit a private umask.
umask 022
stage="$output.stage.$$"
trap 'rm -rf "$stage"' EXIT
mkdir -p "$(dirname "$output")"
# A previous stage from a killed run must not contribute stale files.
rm -rf "$stage"
mkdir -m 0755 "$stage"

# -p keeps the reviewed helper modes (0644 sources, 0755 programs). The Go sources
# travel with the directory so the mounted tree stays canonical source with no
# maintained copy.
(cd "$module" && cp -R -p . "$stage/")

(cd "$stage" && GOWORK=off GOFLAGS='' CGO_ENABLED=0 \
    go build -trimpath -buildvcs=false -o "$stage/recipe-contract" ./cmd/recipe-contract)

chmod 0755 "$stage/recipe-contract"
find "$stage" -type d -exec chmod 0755 {} +
# The composed directory is mounted into every audit container. Go sources and
# testdata are inert read-only data there, but Python is not: it would be
# executable helper code the Go-only rule forbids, and `cp -R` also carries any
# untracked file. Refuse to publish it rather than relying on the repository-side
# guard having run first (ADR 0004, "What the mount carries").
if python_leftovers=$(find "$stage" \( -name '*.py' -o -name '*.pyc' -o -name __pycache__ \) -print); then :; else
    status=$?; echo "cannot inspect the composed directory for Python (status $status)" >&2; exit "$status"
fi
[ -z "$python_leftovers" ] || {
    printf '%s\n' "$python_leftovers" >&2
    echo "refusing to publish Python in the composed directory: the helpers are Go and shell only" >&2
    exit 2
}
printf 'recipe-contract composed directory\n' >"$stage/$marker"
# Record what the binary is so a caller-supplied context cannot be reused for a
# different target: the adapters compare this before honouring
# RECIPE_CONTRACT_CONTEXT. Both real callers pass GOOS/GOARCH explicitly; a direct
# invocation records `unknown-unknown`, which refuses every later reuse rather
# than claiming a target the recipe was never told.
printf '%s-%s\n' "${GOOS:-unknown}" "${GOARCH:-unknown}" >"$stage/.recipe-contract-platform"

# Publication is serialized. Two builds of one output (a suite run beside
# `images/build.sh --host-helpers`, say) must not interleave the two renames, so
# one cannot move the directory out from under the other. Each rename is atomic,
# but the swap is still two of them: a reader that resolves the path in that
# microsecond window sees it absent. The window is exclusive to publishers and an
# already-open reader keeps the old inode, which is why the previous tree is only
# removed after the new one is in place. The owner pid lets a killed publisher be
# taken over. A lock without a pid, or a killed reaper's claim, requires manual
# inspection after the bounded wait; age alone cannot prove it is abandoned.
lock="$output.publish-lock"
attempt=0
while ! mkdir "$lock" 2>/dev/null; do
    attempt=$((attempt + 1))
    if [ "$attempt" -gt 600 ]; then
        echo "cannot publish $output: another build holds $lock" >&2
        exit 1
    fi
    owner=""
    [ ! -f "$lock/pid" ] || owner=$(cat "$lock/pid" 2>/dev/null || :)
    # Only one waiter may reap a dead owner. Re-read under the claim: the
    # original observation might describe a lock already replaced by a waiter.
    # Empty locks fail closed (the owner may not have written its pid yet).
    if [ -n "$owner" ] && ! kill -0 "$owner" 2>/dev/null && mkdir "$lock/reaping" 2>/dev/null; then
        current=$(cat "$lock/pid" 2>/dev/null || :)
        if [ "$current" = "$owner" ] && ! kill -0 "$current" 2>/dev/null; then
            mv "$lock" "$stage.stale-lock"
            rm -rf "$stage.stale-lock"
            continue
        fi
        rmdir "$lock/reaping"
    fi
    sleep 0.1
done
printf '%s\n' "$$" >"$lock/pid"
# Do not unlink a waiter's reaping claim during cleanup. rmdir cannot remove
# the lock until that waiter has finished checking the owner, so a claim always
# protects the same directory generation through its recheck and rename.
cleanup() {
    rm -rf "$stage"
    rm -f "$lock/pid"
    local retries=0
    until rmdir "$lock" 2>/dev/null; do
        retries=$((retries + 1))
        if [ "$retries" -ge 600 ]; then
            echo "cannot release $lock: a reaping claim remains; inspect it before removing the empty lock" >&2
            return
        fi
        sleep 0.1
    done
}
trap cleanup EXIT

# Publish by rename: a reader either sees the previous complete directory or this
# one, never a half-built tree, and a failed build leaves the previous result in
# place instead of a stale "successful" binary that no one rebuilt. The previous
# tree is unlinked only here, after the swap, so nothing is deleted while it is
# still the path a reader would resolve.
if [ -e "$output" ]; then
    if [ ! -d "$output" ] || [ ! -f "$output/$marker" ]; then
        echo "refusing to replace $output: it has no $marker, so this recipe did not write it" >&2
        exit 2
    fi
    previous="$output.previous.$$"
    mv "$output" "$previous"
    mv "$stage" "$output"
    rm -rf "$previous"
else
    mv "$stage" "$output"
fi
