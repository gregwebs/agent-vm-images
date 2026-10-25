#!/usr/bin/env bash
# Parent Debian apt baseline check (#95).
#
# Why: the base recipe inherits its apt configuration from a pinned Debian
# parent, but apt state is mutable and the reviewed baseline is a committed
# file. This gate compares the *parent* baseline byte for byte and refuses extra
# source, key or insecure-configuration state, because a silently changed URI,
# suite or key path would quietly authenticate a different archive.
#
# It deliberately describes only the parent baseline. NodeSource and the GitHub
# CLI repository are written by later RUN steps; they stay recorded #96
# violations and are ratcheted by script/test/image-sources, not resolved here.
#
# `--root` exists so the fixture suite can point the checks at a copied
# filesystem tree. Production runs pass no root and read `/`.
set -euo pipefail

usage() {
    echo "usage: $0 [--root DIR] --policy DIR" >&2
    exit 2
}

root=/
policy=''
while [ "$#" -gt 0 ]; do
    case "$1" in
        --root)
            [ "$#" -ge 2 ] || usage
            root=$2
            shift 2
            ;;
        --policy)
            [ "$#" -ge 2 ] || usage
            policy=$2
            shift 2
            ;;
        *) usage ;;
    esac
done
[ -n "$policy" ] || usage
[ -d "$root" ] || { echo "apt-baseline: root directory is missing: $root" >&2; exit 1; }
[ -d "$policy" ] || { echo "apt-baseline: policy directory is missing: $policy" >&2; exit 1; }
# Hash entries are root-relative, so both paths must survive the later `cd`.
root=$(cd "$root" && pwd)
policy=$(cd "$policy" && pwd)

refuse() {
    echo "apt-baseline: $*" >&2
    exit 1
}

sources_dir=/etc/apt/sources.list.d
[ -d "$root$sources_dir" ] || refuse "missing apt source directory: $sources_dir"
for name in debian.sources keyrings.sha256; do
    [ -f "$policy/$name" ] || refuse "missing reviewed baseline file: images/apt-policy/$name"
done

scratch=$(mktemp -d "${TMPDIR:-/tmp}/apt-baseline.XXXXXX") || refuse 'cannot create a scratch directory'
trap 'rm -rf "$scratch"' EXIT

# A future parent that changes a source byte, a suite, a URI or the Signed-By
# key path is a policy change: refuse it here instead of accepting whatever the
# newest parent happens to ship.
cmp -s "$policy/debian.sources" "$root$sources_dir/debian.sources" ||
    refuse "$sources_dir/debian.sources differs from images/apt-policy/debian.sources"

while IFS= read -r -d '' entry; do
    name=${entry##*/}
    [ -L "$entry" ] && refuse "apt source entry is a symlink: $sources_dir/$name"
    [ -f "$entry" ] || refuse "apt source entry is not a regular file: $sources_dir/$name"
    [ "$name" = debian.sources ] ||
        refuse "unexpected apt source entry: $sources_dir/$name"
done < <(find "$root$sources_dir" -mindepth 1 -maxdepth 1 -print0)

if [ -e "$root/etc/apt/sources.list" ] || [ -L "$root/etc/apt/sources.list" ]; then
    [ -L "$root/etc/apt/sources.list" ] && refuse 'apt source list is a symlink: /etc/apt/sources.list'
    [ -f "$root/etc/apt/sources.list" ] ||
        refuse 'apt source list is not a regular file: /etc/apt/sources.list'
    # APT merges this file with sources.list.d, so an active line would be a
    # source that no committed policy describes.
    while IFS= read -r line || [ -n "$line" ]; do
        trimmed=${line#"${line%%[![:space:]]*}"}
        case "$trimmed" in
            '' | '#'*) continue ;;
        esac
        refuse "active apt source line in /etc/apt/sources.list: $trimmed"
    done <"$root/etc/apt/sources.list"
fi

# A shared trusted.gpg store cannot be attributed to one reviewed source, so
# every keyring must be a path-scoped file that keyrings.sha256 lists.
if [ -e "$root/etc/apt/trusted.gpg" ] || [ -L "$root/etc/apt/trusted.gpg" ]; then
    refuse 'unexpected shared apt keyring store: /etc/apt/trusted.gpg'
fi

trusted_dir=/etc/apt/trusted.gpg.d
awk '$2 ~ /^etc\/apt\/trusted\.gpg\.d\// { sub(/^etc\/apt\/trusted\.gpg\.d\//, "", $2); print $2 }' \
    "$policy/keyrings.sha256" | LC_ALL=C sort >"$scratch/keyrings"
if [ -s "$scratch/keyrings" ] && [ ! -d "$root$trusted_dir" ]; then
    refuse "missing apt keyring directory: $trusted_dir"
fi
if [ -e "$root$trusted_dir" ] || [ -L "$root$trusted_dir" ]; then
    [ -d "$root$trusted_dir" ] && [ ! -L "$root$trusted_dir" ] ||
        refuse "apt keyring directory is not a regular directory: $trusted_dir"
    while IFS= read -r -d '' entry; do
        name=${entry##*/}
        [ -L "$entry" ] && refuse "apt keyring is a symlink: $trusted_dir/$name"
        [ -f "$entry" ] || refuse "apt keyring is not a regular file: $trusted_dir/$name"
        grep -qxF -- "$name" "$scratch/keyrings" ||
            refuse "unexpected apt keyring: $trusted_dir/$name"
    done < <(find "$root$trusted_dir" -mindepth 1 -maxdepth 1 -print0)
    while IFS= read -r name; do
        [ -f "$root$trusted_dir/$name" ] || refuse "missing apt keyring: $trusted_dir/$name"
    done <"$scratch/keyrings"
fi

# Every listed keyring, including the Signed-By archive keyring, is verified
# against the reviewed bytes; a replaced key is a policy change.
if ! hash_output=$(cd "$root" && sha256sum --strict --quiet -c "$policy/keyrings.sha256" 2>&1); then
    printf '%s\n' "$hash_output" >&2
    refuse 'apt keyring bytes do not match images/apt-policy/keyrings.sha256'
fi

# APT configuration can re-enable unauthenticated or insecure sources, accept
# expired repository metadata, point apt at another sources or keys directory, or
# include further configuration, without touching a source file. Only active
# directives count: the parent's shipped comments explain apt behaviour and must
# not fail the gate, and `#include`/`#clear` are directives rather than comments
# (apt.conf(5)), so they are refused instead of skipped.
check_config() {
    local path=$1 display=$2 line trimmed lower
    [ -f "$path" ] || return 0
    while IFS= read -r line || [ -n "$line" ]; do
        trimmed=${line#"${line%%[![:space:]]*}"}
        case "$trimmed" in
            '' | '//'*) continue ;;
        esac
        lower=$(printf '%s' "$trimmed" | LC_ALL=C tr '[:upper:]' '[:lower:]')
        case "$lower" in
            '#include'* | '#clear'*)
                refuse "apt configuration directive in $display: $trimmed"
                ;;
            '#'*) continue ;;
        esac
        # A Dir:: override moves the directories the checks above look at, so the
        # verified sources and keyrings would no longer be the ones apt reads.
        case "$lower" in
            *dir::etc* | *dir::parts*)
                refuse "apt configuration directory override in $display: $trimmed"
                ;;
        esac
        case "$lower" in
            *allowinsecurerepositories* | *allowdowngradetoinsecurerepositories* | \
                *allowunauthenticated* | *check-valid-until*)
                refuse "insecure apt configuration in $display: $trimmed"
                ;;
        esac
    done <"$path"
}
check_config "$root/etc/apt/apt.conf" /etc/apt/apt.conf
for path in "$root"/etc/apt/apt.conf.d/*; do
    [ -e "$path" ] || continue
    check_config "$path" "/etc/apt/apt.conf.d/${path##*/}"
done

echo '==> Debian apt baseline verified'
