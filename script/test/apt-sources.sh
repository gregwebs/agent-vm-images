#!/usr/bin/env bash
# Fixture-root mutations for the parent Debian apt baseline checker (#95).
#
# The checker runs inside the base build against a pinned parent; this suite runs
# the same script against a copied filesystem root, because the build itself is
# the expensive gate that the verification phase runs. Synthetic key bytes are
# hashed into a fixture policy manifest, so the byte comparison, keyring set and
# configuration rules under test are the real ones.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
CHECKER="$ROOT/images/apt-policy/check-sources.sh"
COMMITTED_POLICY="$ROOT/images/apt-policy"
work=$(mktemp -d "${TMPDIR:-/tmp}/apt-sources.XXXXXX")
trap 'rm -rf "$work"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}
command -v sha256sum >/dev/null || fail 'sha256sum required (GNU coreutils)'

case_dir='' root='' policy=''
# A baseline root that must pass: the committed Debian source bytes, one
# trusted.gpg.d keyring and the Signed-By archive keyring, both hashed.
baseline() {
    case_dir=$work/$1
    root=$case_dir/root
    policy=$case_dir/policy
    mkdir -p "$root/etc/apt/sources.list.d" "$root/etc/apt/trusted.gpg.d" \
        "$root/etc/apt/apt.conf.d" "$root/usr/share/keyrings" "$policy"
    cp "$COMMITTED_POLICY/debian.sources" "$policy/debian.sources"
    cp "$COMMITTED_POLICY/debian.sources" "$root/etc/apt/sources.list.d/debian.sources"
    printf 'fixture archive key\n' >"$root/usr/share/keyrings/debian-archive-keyring.pgp"
    printf 'fixture trusted key\n' >"$root/etc/apt/trusted.gpg.d/debian-fixture.asc"
    (
        cd "$root"
        sha256sum usr/share/keyrings/debian-archive-keyring.pgp \
            etc/apt/trusted.gpg.d/debian-fixture.asc >"$policy/keyrings.sha256"
    )
    # The parent's shipped configuration explains apt in comments; only active
    # directives may fail the gate.
    cat >"$root/etc/apt/apt.conf.d/70-fixture" <<'CONF'
# apt-get install in a Dockerfile is the documented case.
// Acquire::AllowInsecureRepositories "true";
Acquire::GzipIndexes "true";
CONF
}

run() {
    ( cd "$case_dir" && bash "$CHECKER" --root "$root" --policy "$policy" ) >"$case_dir/out" 2>&1
}

accepts() {
    local label=$1
    if ! run; then
        fail "$label: refused the baseline: $(cat "$case_dir/out")"
    fi
    grep -q 'Debian apt baseline verified' "$case_dir/out" ||
        fail "$label: no verification line: $(cat "$case_dir/out")"
    echo "ok: $label"
}

refuses() {
    local label=$1 needle=$2 status=0
    run || status=$?
    [ "$status" = 1 ] || fail "$label: status $status, wanted 1: $(cat "$case_dir/out")"
    grep -Fq -- "$needle" "$case_dir/out" ||
        fail "$label: missing '$needle' in: $(cat "$case_dir/out")"
    echo "ok: $label"
}

baseline baseline
accepts 'committed Debian baseline'

baseline changed-source
printf 'Types: deb\n' >>"$root/etc/apt/sources.list.d/debian.sources"
refuses 'changed source bytes' '/etc/apt/sources.list.d/debian.sources differs'

baseline missing-source
rm "$root/etc/apt/sources.list.d/debian.sources"
refuses 'missing source file' '/etc/apt/sources.list.d/debian.sources differs'

baseline extra-list
printf 'deb http://evil.example stable main\n' >"$root/etc/apt/sources.list.d/evil.list"
refuses 'extra .list source' '/etc/apt/sources.list.d/evil.list'

baseline extra-save
printf 'deb http://evil.example stable main\n' >"$root/etc/apt/sources.list.d/debian.sources.save"
refuses 'ignored-suffix source entry' '/etc/apt/sources.list.d/debian.sources.save'

baseline extra-hidden
printf 'deb http://evil.example stable main\n' >"$root/etc/apt/sources.list.d/.hidden.list"
refuses 'hidden source entry' '/etc/apt/sources.list.d/.hidden.list'

baseline source-symlink
ln -s ../debian.sources "$root/etc/apt/sources.list.d/evil.sources"
refuses 'symlinked source entry' 'apt source entry is a symlink: /etc/apt/sources.list.d/evil.sources'

baseline sources-list-comments
printf '# deb http://evil.example stable main\n\n' >"$root/etc/apt/sources.list"
accepts 'comment-only sources.list'

baseline sources-list-active
printf '# reviewed\n deb http://evil.example stable main\n' >"$root/etc/apt/sources.list"
refuses 'active sources.list line' 'active apt source line in /etc/apt/sources.list'

baseline trusted-gpg
: >"$root/etc/apt/trusted.gpg"
refuses 'shared trusted.gpg store' 'unexpected shared apt keyring store: /etc/apt/trusted.gpg'

baseline extra-keyring
printf 'another key\n' >"$root/etc/apt/trusted.gpg.d/evil.asc"
refuses 'extra keyring' 'unexpected apt keyring: /etc/apt/trusted.gpg.d/evil.asc'

baseline missing-keyring
rm "$root/etc/apt/trusted.gpg.d/debian-fixture.asc"
refuses 'missing keyring' 'missing apt keyring: /etc/apt/trusted.gpg.d/debian-fixture.asc'

baseline changed-keyring
printf 'replaced trusted key\n' >"$root/etc/apt/trusted.gpg.d/debian-fixture.asc"
refuses 'changed keyring bytes' 'apt keyring bytes do not match images/apt-policy/keyrings.sha256'

baseline changed-signed-by-key
printf 'replaced archive key\n' >"$root/usr/share/keyrings/debian-archive-keyring.pgp"
refuses 'changed Signed-By keyring' 'apt keyring bytes do not match images/apt-policy/keyrings.sha256'

baseline keyring-symlink
ln -s ../../usr/share/keyrings/debian-archive-keyring.pgp \
    "$root/etc/apt/trusted.gpg.d/debian-linked.asc"
refuses 'symlinked keyring' 'apt keyring is a symlink: /etc/apt/trusted.gpg.d/debian-linked.asc'

for directive in \
    'Acquire::AllowInsecureRepositories "true";' \
    'ACQUIRE::ALLOWDOWNGRADETOINSECUREREPOSITORIES "true";' \
    'Acquire::AllowUnauthenticated "true";' \
    'Acquire::Check-Valid-Until "false";'; do
    name=config-$RANDOM
    baseline "$name"
    printf '%s\n' "$directive" >"$root/etc/apt/apt.conf.d/99-insecure"
    refuses "insecure configuration: $directive" '/etc/apt/apt.conf.d/99-insecure'
done

baseline config-apt-conf
printf 'Acquire::Check-Valid-Until "false";\n' >"$root/etc/apt/apt.conf"
refuses 'insecure apt.conf directive' 'insecure apt configuration in /etc/apt/apt.conf'

# apt.conf(5) runs `#include` and `#clear` as directives, so a file that would
# otherwise be documentation can pull in arbitrary configuration. This is the
# review probe: both lines together used to be read as two comments.
baseline config-include-and-dir-override
cat >"$root/etc/apt/apt.conf.d/99-evil" <<'CONF'
#include "/opt/evil.conf";
Dir::Etc::SourceParts "/opt/evil-sources/";
CONF
refuses 'include directive with a Dir:: override' \
    'apt configuration directive in /etc/apt/apt.conf.d/99-evil: #include "/opt/evil.conf";'

baseline config-include
printf '#include "/opt/evil.conf";\n' >"$root/etc/apt/apt.conf.d/99-include"
refuses '#include in apt.conf.d' 'apt configuration directive in /etc/apt/apt.conf.d/99-include'

baseline config-include-apt-conf
printf '#include "/opt/evil.conf";\n' >"$root/etc/apt/apt.conf"
refuses '#include in apt.conf' 'apt configuration directive in /etc/apt/apt.conf'

baseline config-clear
printf '#clear Dir::Etc::SourceList;\n' >"$root/etc/apt/apt.conf.d/99-clear"
refuses '#clear directive' 'apt configuration directive in /etc/apt/apt.conf.d/99-clear: #clear'

# A Dir:: override redirects the directories every check above compares, so the
# verified sources and keyrings would no longer be the ones apt reads.
for directive in \
    'Dir::Etc::SourceParts "/opt/evil-sources/";' \
    'DIR::ETC::SOURCELIST "/opt/evil.list";' \
    'Dir::Etc::Trusted "/opt/evil.gpg";' \
    'Dir::Parts "/opt/evil-parts/";'; do
    name=config-$RANDOM
    baseline "$name"
    printf '%s\n' "$directive" >"$root/etc/apt/apt.conf.d/99-dir"
    refuses "apt configuration directory override: $directive" \
        "apt configuration directory override in /etc/apt/apt.conf.d/99-dir: $directive"
done

baseline policy-missing-manifest
rm "$policy/keyrings.sha256"
refuses 'missing policy manifest' 'missing reviewed baseline file: images/apt-policy/keyrings.sha256'

baseline policy-missing-sources
rm "$policy/debian.sources"
refuses 'missing policy sources' 'missing reviewed baseline file: images/apt-policy/debian.sources'

baseline missing-sources-dir
rm -rf "$root/etc/apt/sources.list.d"
refuses 'missing source directory' 'missing apt source directory: /etc/apt/sources.list.d'

baseline usage
status=0
bash "$CHECKER" --root "$root" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 2 ] || fail "usage: missing --policy exited $status, wanted 2"
status=0
bash "$CHECKER" --policy "$policy" --extra >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 2 ] || fail "usage: unknown argument exited $status, wanted 2"
echo 'ok: usage errors exit 2'

status=0
bash "$CHECKER" --root "$work/absent-root" --policy "$policy" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 1 ] || fail "absent root exited $status, wanted 1"
grep -Fq 'root directory is missing' "$case_dir/out" || fail 'absent root is not named'
status=0
bash "$CHECKER" --root "$root" --policy "$work/absent-policy" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 1 ] || fail "absent policy exited $status, wanted 1"
grep -Fq 'policy directory is missing' "$case_dir/out" || fail 'absent policy is not named'
echo 'ok: absent roots are refused and named'

echo 'apt-sources: all cases passed'
