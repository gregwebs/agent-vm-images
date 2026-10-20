#!/usr/bin/env bash
# Offline controls for script/dev/install-shellcheck.sh: the pin table, the
# checksum and version assertions, cleanup status preservation, the destination
# guard, and the environment curl receives. Fake curl and fake uname keep it off
# the network; the payloads are locally built tarballs.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
INSTALLER="$ROOT/script/dev/install-shellcheck.sh"
work=$(mktemp -d "${TMPDIR:-/tmp}/install-shellcheck-test.XXXXXX")
# Keep a failing case's status: a failed cleanup must not mask it.
cleanup() {
    status=$?
    rm -rf "$work" || { echo "could not remove $work" >&2; [ "$status" -ne 0 ] || status=1; }
    exit "$status"
}
trap cleanup EXIT
fail() { echo "install-shellcheck regression: $*" >&2; exit 1; }
# The installer's literal is the pin; contracts.sh asserts the real binary against
# it. Deriving it here keeps a pin bump to the two documented files.
version=$(sed -n 's/^version=//p' "$INSTALLER")
[ -n "$version" ] || fail 'installer version literal not found'

# A host proxy must not decide these controls; the proxy case sets its own.
unset HTTPS_PROXY https_proxy NO_PROXY no_proxy
# Canaries: the installer's env -i must drop them, so curl never sees them.
export ALL_PROXY=http://canary.invalid CURL_CA_BUNDLE=/canary
real_rm=$(command -v rm)
mkdir -p "$work/bin" "$work/bin-fail-rm" "$work/tree/script/dev"

cat > "$work/bin/curl" <<SH
#!/usr/bin/env bash
set -euo pipefail
target=
while [ "\$#" -gt 0 ]; do
    case "\$1" in
        -o) shift; target=\$1 ;;
        https://*) printf '%s\n' "\$1" >> "$work/curls" ;;
    esac
    shift
done
printf '%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
    "\${HOME:-}" "\${CURL_HOME:-}" "\${XDG_CONFIG_HOME:-}" \
    "\${ALL_PROXY:-}" "\${CURL_CA_BUNDLE:-}" \
    "\${HTTPS_PROXY:-}" "\${https_proxy:-}" "\${NO_PROXY:-}" "\${no_proxy:-}" \
    > "$work/curl-env"
printf '%s\n' "\$target" > "$work/curl-out"
status=\$(cat "$work/curl-status")
[ "\$status" = 0 ] || exit "\$status"
cp "$work/payload" "\$target"
SH
cat > "$work/bin/uname" <<'SH'
#!/usr/bin/env bash
case "$1" in
    -s) printf 'Darwin\n' ;;
    -m) printf 'arm64\n' ;;
esac
SH
cat > "$work/bin-fail-rm/rm" <<SH
#!/usr/bin/env bash
$real_rm "\$@"
exit 13
SH
chmod +x "$work/bin/"* "$work/bin-fail-rm/"*

make_payload() {  # version the packaged binary reports
    rm -rf "$work/stage"
    mkdir -p "$work/stage/shellcheck-v$version"
    printf '#!/bin/sh\necho "version: %s"\n' "$1" > "$work/stage/shellcheck-v$version/shellcheck"
    chmod +x "$work/stage/shellcheck-v$version/shellcheck"
    (cd "$work/stage" && tar -cJf "$work/payload" "shellcheck-v$version/shellcheck")
}

# The fixture hash replaces the committed pin for the branch the fake uname selects.
pin_fixture() {
    sed 's|^    Darwin.arm64) asset=darwin.aarch64 hash=.*|    Darwin.arm64) asset=darwin.aarch64 hash='"$1"' ;;|' \
        "$INSTALLER" > "$work/tree/script/dev/install-shellcheck.sh"
}

new_case() {
    case_dir="$work/case-$1"
    mkdir -p "$case_dir/tmp"
    : > "$case_dir/log"
    rm -f "$work/curls" "$work/curl-env" "$work/curl-out"
    printf '0\n' > "$work/curl-status"
}

for name in valid proxy checksum wrong-version curl-fail-cleanup-fail success-cleanup-fail; do
    new_case "$name"
    payload_version=$version
    fake_rm=false
    proxy=false
    case "$name" in
        wrong-version) payload_version=0.9.0 ;;
        curl-fail-cleanup-fail) printf '37\n' > "$work/curl-status"; fake_rm=true ;;
        success-cleanup-fail) fake_rm=true ;;
        proxy) proxy=true ;;
    esac
    make_payload "$payload_version"
    if [ "$name" = checksum ]; then
        # The committed pin against a locally built payload is a real mismatch.
        cp "$INSTALLER" "$work/tree/script/dev/install-shellcheck.sh"
    else
        pin_fixture "$(sha256sum "$work/payload" | cut -d ' ' -f 1)"
    fi
    path="$work/bin:$PATH"
    [ "$fake_rm" = false ] || path="$work/bin-fail-rm:$path"
    status=0
    if [ "$proxy" = true ]; then
        HTTPS_PROXY=http://proxy.invalid:3128 https_proxy=http://proxy.invalid:3128 \
            NO_PROXY=localhost no_proxy=localhost PATH="$path" TMPDIR="$case_dir/tmp" \
            bash "$work/tree/script/dev/install-shellcheck.sh" "$case_dir/dest" > "$case_dir/log" 2>&1 ||
            status=$?
    else
        PATH="$path" TMPDIR="$case_dir/tmp" \
            bash "$work/tree/script/dev/install-shellcheck.sh" "$case_dir/dest" > "$case_dir/log" 2>&1 ||
            status=$?
    fi
    # curl's whole environment, in this order: HOME|CURL_HOME|XDG_CONFIG_HOME|
    # ALL_PROXY|CURL_CA_BUNDLE|HTTPS_PROXY|https_proxy|NO_PROXY|no_proxy. A dropped
    # env -i, neutralisation or proxy pass-through changes this string.
    if [ "$proxy" = true ]; then
        expected='/nonexistent|/nonexistent|/nonexistent|||http://proxy.invalid:3128|http://proxy.invalid:3128|localhost|localhost'
    else
        expected='/nonexistent|/nonexistent|/nonexistent||||||'
    fi
    recorded=$(cat "$work/curl-env")
    [ "$recorded" = "$expected" ] || fail "$name: curl environment: $recorded"
    # The private TMPDIR is only proven if the download target is inside it.
    out=$(cat "$work/curl-out")
    case "$out" in
        "$case_dir/tmp"/*) ;;
        *) fail "$name: download outside the private TMPDIR: $out" ;;
    esac
    case "$name" in
        valid | proxy)
            [ "$status" = 0 ] || fail "$name: expected rc 0, got $status"
            "$case_dir/dest/shellcheck" --version | grep -qx "version: $version" ||
                fail "$name: installed binary reports the wrong version"
            fetched=$(cat "$work/curls")
            [ "$fetched" = "https://github.com/koalaman/shellcheck/releases/download/v$version/shellcheck-v$version.darwin.aarch64.tar.xz" ] ||
                fail "$name: unexpected fetch: $fetched"
            ;;
        checksum)
            [ "$status" = 1 ] || fail "checksum: expected rc 1, got $status"
            grep -Fq 'shellcheck.tar.xz: FAILED' "$case_dir/log" ||
                fail 'checksum: checksum failure not reported'
            [ ! -e "$case_dir/dest" ] || fail 'checksum: destination created'
            ;;
        wrong-version)
            [ "$status" = 1 ] || fail "wrong-version: expected rc 1, got $status"
            grep -Fq "pinned shellcheck reports '0.9.0', expected '$version'" "$case_dir/log" ||
                fail 'wrong-version: mismatch not reported'
            [ ! -e "$case_dir/dest" ] || fail 'wrong-version: destination created'
            ;;
        curl-fail-cleanup-fail)
            [ "$status" = 37 ] || fail "curl failure status replaced by cleanup: $status"
            grep -Fq 'cleanup failed' "$case_dir/log" || fail 'cleanup failure not reported'
            [ ! -e "$case_dir/dest" ] || fail 'curl-fail-cleanup-fail: destination created'
            ;;
        success-cleanup-fail)
            [ "$status" = 1 ] || fail "cleanup failure did not fail the run: $status"
            grep -Fq 'cleanup failed' "$case_dir/log" || fail 'cleanup failure not reported'
            "$case_dir/dest/shellcheck" --version | grep -qx "version: $version" ||
                fail 'success-cleanup-fail: verified binary missing'
            ;;
    esac
    [ -z "$(ls -A "$case_dir/tmp")" ] || fail "$name: scratch directory not empty"
    echo "install-shellcheck case $name passed"
done

expect_usage() {  # name, expected diagnostic, installer arguments
    name=$1
    expected=$2
    shift 2
    usage_dir="$work/usage-$name"
    mkdir -p "$usage_dir/tmp"
    cp "$INSTALLER" "$work/tree/script/dev/install-shellcheck.sh"
    status=0
    PATH="$work/bin:$PATH" TMPDIR="$usage_dir/tmp" \
        bash "$work/tree/script/dev/install-shellcheck.sh" "$@" > "$usage_dir/log" 2>&1 || status=$?
    [ "$status" = 2 ] || fail "$name: expected rc 2, got $status"
    grep -Fq "$expected" "$usage_dir/log" || fail "$name: expected diagnostic: $expected"
    written=$(ls -A "$work/tree")
    [ "$written" = script ] || fail "$name: wrote into the checkout copy: $written"
    [ -z "$(ls -A "$usage_dir/tmp")" ] || fail "$name: scratch directory not empty"
    echo "install-shellcheck case $name passed"
}

expect_usage argc0 'usage:'
expect_usage argc2 'usage:' "$work/usage-argc2/dest" extra
expect_usage relative 'usage:' relative
expect_usage root 'install outside the checkout' "$work/tree"
expect_usage root-slash 'install outside the checkout' "$work/tree/"
expect_usage root-bin 'install outside the checkout' "$work/tree/bin"

echo 'install-shellcheck controls passed'
