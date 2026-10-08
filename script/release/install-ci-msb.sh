#!/usr/bin/env bash
# Provision the pinned, reviewed standalone microsandbox runtime (msb plus its
# matching libkrunfw) on a disposable amd64 Actions runner for verify-release --boot.
# The committed pin is the review; there is no latest lookup (bump per
# docs/standard-image-releases.md "Hosted amd64 boot verification").
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_OS:-}" = Linux ] || { echo 'disposable Linux Actions runner only' >&2; exit 1; }
[ "$(uname -m)" = x86_64 ] || { echo 'the pinned msb runtime is provisioned for hosted amd64 only' >&2; exit 1; }
pins="$ROOT/script/release/tool-pins.json"
version=$(jq -er .msb_version "$pins")
msb_hash=$(jq -er .msb_linux_amd64_sha256 "$pins")
firmware_version=$(jq -er .libkrunfw_version "$pins")
firmware_hash=$(jq -er .libkrunfw_linux_amd64_sha256 "$pins")
libkrun_version=$(jq -er .msb_libkrun_version "$pins")
out=$(mktemp -d "$RUNNER_TEMP/msb-runtime.XXXXXX")
mkdir "$out/bin" "$out/lib"
fetch() {
    local url="https://github.com/superradcompany/microsandbox/releases/download/v$version/$1"
    env -i PATH="$PATH" HOME=/nonexistent CURL_HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent \
        curl -q --netrc-file /dev/null --max-filesize 100000000 --fail --silent --show-error --location \
        --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 "$url" -o "$2"
    printf '%s  %s\n' "$3" "$2" | sha256sum -c -
    printf '%s sha256:%s\n' "$url" "$3" >> "$out/msb-runtime.log"
}
fetch msb-linux-x86_64 "$out/msb.download" "$msb_hash"
fetch libkrunfw-linux-x86_64.so "$out/libkrunfw.download" "$firmware_hash"
install -m 0755 "$out/msb.download" "$out/bin/msb"
install -m 0644 "$out/libkrunfw.download" "$out/lib/libkrunfw.so.$firmware_version"
rm "$out/msb.download" "$out/libkrunfw.download"
reported=$("$out/bin/msb" --version)
[ "$reported" = "msb $version" ] || { echo "pinned msb reports '$reported', expected 'msb $version'" >&2; exit 1; }
# MSB_LIBKRUN_EMBEDDED=1 is honest only while the executable resolves no libkrun;
# verify.py re-checks the same linkage and records it.
ldd "$out/bin/msb" > "$out/msb-ldd.log"
if grep -q 'libkrun\.' "$out/msb-ldd.log"; then
    echo 'pinned msb links libkrun dynamically; declare MSB_LIBKRUN_PATH instead of embedded' >&2; exit 1
fi
printf '%s\nlibkrun: %s\n' "$reported" "$libkrun_version" >> "$out/msb-runtime.log"
{
    echo "msb=$out/bin/msb"
    echo "libkrunfw=$out/lib/libkrunfw.so.$firmware_version"
    echo "libkrun_version=$libkrun_version"
    echo "dir=$out"
} >> "$GITHUB_OUTPUT"
