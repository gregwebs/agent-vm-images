#!/bin/sh
# Repository base identity, verified inside the base and standard builds (#95).
#
# The committed manifest lists the images/ files this repository built from;
# re-hashing them proves the build consumed those bytes. `stamp` writes the
# manifest's own hash into the base image so a local consumer can compare it, and
# `verify` compares a stamp inherited from the parent with this checkout's
# manifest. Only source identity is at stake: the same sources may still produce
# a different image ID, and this script never claims otherwise.
#
# No apt, network, helper binary or manifest-walking code is involved, so the
# check cannot be skipped by a missing tool.
set -eu

usage() {
    echo 'usage: verify-base-inputs.sh ROOT stamp|verify STAMP' >&2
    exit 2
}

[ "$#" -eq 3 ] || usage
root=$1
action=$2
stamp=$3
case "$action" in
    stamp | verify) ;;
    *) usage ;;
esac
manifest=$root/base-inputs.sha256

fail() {
    echo "base-inputs: $*" >&2
    exit 1
}

[ -f "$manifest" ] || fail "missing $root/base-inputs.sha256; rebuild the base from this checkout"
# The manifest is machine-written and must be read defensively: a normalized or
# duplicated path would silently hash a different file than the one it names.
# Ordering is part of the format, so an out-of-order file is one the generator
# would not have produced.
grammar=$(LC_ALL=C awk '
    {
        line = $0
        if (length(line) < 67) { print "malformed manifest entry at line " NR; exit 1 }
        if (substr(line, 1, 64) ~ /[^0-9a-f]/) { print "malformed manifest hash at line " NR; exit 1 }
        if (substr(line, 65, 2) != "  ") { print "malformed manifest separator at line " NR; exit 1 }
        path = substr(line, 67)
        if (path !~ /^[A-Za-z0-9._\/-]+$/) { print "malformed manifest path at line " NR; exit 1 }
        if (path ~ /^\//) { print "absolute manifest path at line " NR; exit 1 }
        if (path ~ /\/\//) { print "empty manifest path component at line " NR; exit 1 }
        if (path ~ /(^|\/)\.\.?(\/|$)/) { print "relative manifest path component at line " NR; exit 1 }
        if (path == "base-inputs.sha256") { print "manifest lists itself at line " NR; exit 1 }
        if (count > 0 && !(("x" previous) < ("x" path))) {
            print "manifest paths are not strictly ordered at line " NR; exit 1
        }
        previous = path
        count++
    }
    END { if (NR == 0) { print "the manifest is empty"; exit 1 } }
' "$manifest") || fail "$grammar"

# A final line without a newline is still an entry: `read` returns non-zero at
# EOF, and dropping that entry would skip its symlink and type checks while
# `sha256sum -c` happily follows the symlink.
while IFS= read -r entry || [ -n "$entry" ]; do
    path=${entry#*  }
    rest=$path
    prefix=''
    while [ "${rest%%/*}" != "$rest" ]; do
        component=${rest%%/*}
        rest=${rest#*/}
        prefix=${prefix:+$prefix/}$component
        [ ! -L "$root/$prefix" ] || fail "symlinked manifest component: $prefix"
        [ -d "$root/$prefix" ] || fail "missing manifest component: $prefix"
    done
    [ ! -L "$root/$path" ] || fail "symlinked manifest entry: $path"
    [ -f "$root/$path" ] || fail "missing or non-regular manifest entry: $path"
done <"$manifest"

# --strict makes a malformed line fatal; the grammar pass above already proved
# the shape, so this is the content check.
if ! ( cd "$root" && sha256sum --strict --quiet --check base-inputs.sha256 ); then
    echo 'base-inputs: inputs/default drift; run: bash script/build/base-inputs.sh --write' >&2
    exit 1
fi

sum=$(sha256sum <"$manifest")
identity=sha256:${sum%% *}

if [ "$action" = stamp ]; then
    mkdir -p "$(dirname "$stamp")"
    printf '%s\n' "$identity" >"$stamp"
    echo "base-inputs: stamped $stamp with $identity"
    exit 0
fi

[ -f "$stamp" ] || fail 'BASE_IMAGE has no repository base identity; rebuild the base from this checkout'
declared=$(cat "$stamp")
if [ "$(wc -l <"$stamp")" -ne 1 ] || ! printf '%s' "$declared" | grep -Eqx 'sha256:[0-9a-f]{64}'; then
    fail 'BASE_IMAGE declares a malformed repository base identity; rebuild the base from this checkout'
fi
if [ "$declared" != "$identity" ]; then
    fail "BASE_IMAGE declares $declared; this checkout expects $identity; rebuild the base"
fi
echo "base-inputs: verified $stamp ($identity)"
