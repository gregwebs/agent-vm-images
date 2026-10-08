#!/usr/bin/env bash
# Offline pin/linkage controls; fixture hashes change, production has no bypass.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin" "$work/tree/script/release" "$work/fixtures"
cp "$ROOT/script/release/install-ci-msb.sh" "$work/tree/script/release/"
cat > "$work/bin/curl" <<SH
#!/usr/bin/env bash
set -euo pipefail
url= target=
while [ "\$#" -gt 0 ]; do
    case "\$1" in
        https://*) url=\$1 ;;
        -o) shift; target=\$1 ;;
    esac
    shift
done
printf '%s\n' "\$url" >> "$work/urls"
[ -f "$work/fixtures/\${url##*/}" ] || exit 22
cp "$work/fixtures/\${url##*/}" "\$target"
SH
cat > "$work/bin/uname" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "${FAKE_ARCH:-x86_64}"
SH
cat > "$work/bin/ldd" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "${FAKE_LDD:-libc.so.6 => /lib/libc.so.6}"
exit "${FAKE_LDD_STATUS:-0}"
SH
chmod +x "$work/bin/"*
export PATH="$work/bin:$PATH"
fail() { echo "runtime regression: $*" >&2; exit 1; }
for number in 1 2 3 4 5 6 7 8; do
    case_dir="$work/case-$number"
    mkdir -p "$case_dir/tmp"
    rm -f "$work/urls"
    version=0.7.7
    [ "$number" != 4 ] || version=0.7.6
    printf '#!/bin/sh\necho "msb %s"\n' "$version" > "$work/fixtures/msb-linux-x86_64"
    printf firmware > "$work/fixtures/libkrunfw-linux-x86_64.so"
    jq --arg msb "$(sha256sum "$work/fixtures/msb-linux-x86_64" | cut -d ' ' -f 1)" \
       --arg fw "$(sha256sum "$work/fixtures/libkrunfw-linux-x86_64.so" | cut -d ' ' -f 1)" \
       '.msb_linux_amd64_sha256=$msb | .libkrunfw_linux_amd64_sha256=$fw' \
       "$ROOT/script/release/tool-pins.json" > "$work/tree/script/release/tool-pins.json"
    case "$number" in
        2) echo corrupt >> "$work/fixtures/msb-linux-x86_64" ;;
        3) echo corrupt >> "$work/fixtures/libkrunfw-linux-x86_64.so" ;;
    esac
    status=0
    (export GITHUB_ACTIONS=true RUNNER_OS=Linux RUNNER_TEMP="$case_dir/tmp" GITHUB_OUTPUT="$case_dir/out"
     unset FAKE_ARCH FAKE_LDD FAKE_LDD_STATUS
     case "$number" in
         5) export FAKE_LDD='libkrun.so.1 => /opt/libkrun.so.1' ;;
         6) unset GITHUB_ACTIONS ;;
         7) export FAKE_ARCH=aarch64 ;;
         8) export FAKE_LDD='ldd: dependency inspection failed' FAKE_LDD_STATUS=1 ;;
     esac
     bash "$work/tree/script/release/install-ci-msb.sh") > "$case_dir/log" 2>&1 || status=$?
    if [ "$number" = 1 ]; then
        [ "$status" = 0 ] || fail 'valid pins rejected'
        [ "$(wc -l < "$case_dir/out" | tr -d ' ')" = 4 ] || fail 'output count'
        dir=$(sed -n 's/^dir=//p' "$case_dir/out")
        {
            echo "msb=$dir/bin/msb"
            echo "libkrunfw=$dir/lib/libkrunfw.so.5.6.1"
            echo 'libkrun_version=msb_krun 0.1.40 (statically linked into microsandbox v0.7.7)'
            echo "dir=$dir"
        } > "$case_dir/expected"
        cmp "$case_dir/expected" "$case_dir/out" || fail 'output declaration'
        [ "$(stat -c %a "$dir/bin/msb")" = 755 ] || fail 'msb mode'
        printf '%s\n' \
            'https://github.com/superradcompany/microsandbox/releases/download/v0.7.7/msb-linux-x86_64' \
            'https://github.com/superradcompany/microsandbox/releases/download/v0.7.7/libkrunfw-linux-x86_64.so' \
            > "$case_dir/expected-urls"
        cmp "$case_dir/expected-urls" "$work/urls" || fail 'unexpected fetch origin/version'
    else
        [ "$status" != 0 ] || fail "case $number unexpectedly passed"
        [ ! -s "$case_dir/out" ] || fail "case $number published outputs"
        case "$number" in
            5|8)
                if [ "$number" = 5 ]; then
                    linkage='libkrun.so.1 => /opt/libkrun.so.1'
                else
                    linkage='ldd: dependency inspection failed'
                fi
                grep -Fq "$linkage" "$case_dir/log" || fail 'failed linkage missing from console'
                grep -Fq 'msb-linux-x86_64 sha256:' "$case_dir/log" || fail 'msb provenance missing from console'
                grep -Fq 'libkrunfw-linux-x86_64.so sha256:' "$case_dir/log" || fail 'firmware provenance missing from console'
                ;;
            6|7) [ ! -e "$work/urls" ] || fail 'guard attempted network' ;;
        esac
    fi
    echo "runtime case $number passed"
done
echo 'ci boot runtime controls passed'
