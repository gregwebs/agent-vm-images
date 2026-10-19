#!/usr/bin/env bash
# Provision the ShellCheck that script/test/contracts.sh asserts, on CI and dev
# hosts alike, so a local pass is the same lint CI runs. The committed checksums
# are the review; there is no latest lookup. Bump together with the version
# assertion in contracts.sh: CI fails while they disagree.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
usage="usage: ${0##*/} ABSOLUTE_DIR"
[ "$#" -eq 1 ] || { echo "$usage" >&2; exit 2; }
dest=$1
# A destination in the checkout would leave an untracked binary in the tree.
# Textual prefix check only: ./.., repeated slashes and symlinks are not normalized.
case "$dest" in
    "$ROOT" | "$ROOT"/*) echo "install outside the checkout, not $dest" >&2; exit 2 ;;
    /*) ;;
    *) echo "$usage" >&2; exit 2 ;;
esac
version=0.11.0
case "$(uname -s).$(uname -m)" in
    Linux.x86_64) asset=linux.x86_64 hash=8c3be12b05d5c177a04c29e3c78ce89ac86f1595681cab149b65b97c4e227198 ;;
    Linux.aarch64) asset=linux.aarch64 hash=12b331c1d2db6b9eb13cfca64306b1b157a86eb69db83023e261eaa7e7c14588 ;;
    Darwin.arm64) asset=darwin.aarch64 hash=56affdd8de5527894dca6dc3d7e0a99a873b0f004d7aabc30ae407d3f48b0a79 ;;
    Darwin.x86_64) asset=darwin.x86_64 hash=3c89db4edcab7cf1c27bff178882e0f6f27f7afdf54e859fa041fca10febe4c6 ;;
    *) echo "no pinned shellcheck $version asset for $(uname -s) $(uname -m)" >&2; exit 1 ;;
esac
work=$(mktemp -d "${TMPDIR:-/tmp}/shellcheck.XXXXXX")
# Keep the failure that brought us here; a failed cleanup only fails a success.
cleanup() {
    status=$?
    rm -rf "$work" || { echo "cleanup failed: could not remove $work" >&2; [ "$status" -ne 0 ] || status=1; }
    exit "$status"
}
trap cleanup EXIT
# A dev host behind a proxy needs its settings to survive the clean environment.
# Only the proxy is inherited: curlrc and netrc stay neutralised, and the
# committed checksum is the integrity check, not the network path.
set -- PATH="$PATH" HOME=/nonexistent CURL_HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent
for name in HTTPS_PROXY https_proxy NO_PROXY no_proxy; do
    value="${!name:-}"
    [ -z "$value" ] || set -- "$@" "$name=$value"
done
env -i "$@" \
    curl -q --netrc-file /dev/null --max-filesize 100000000 --fail --silent --show-error --location \
    --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 \
    "https://github.com/koalaman/shellcheck/releases/download/v$version/shellcheck-v$version.$asset.tar.xz" \
    -o "$work/shellcheck.tar.xz"
printf '%s  %s\n' "$hash" "$work/shellcheck.tar.xz" | sha256sum -c -
tar -xJf "$work/shellcheck.tar.xz" -C "$work" "shellcheck-v$version/shellcheck"
reported=$("$work/shellcheck-v$version/shellcheck" --version | sed -n 's/^version: //p')
[ "$reported" = "$version" ] || { echo "pinned shellcheck reports '$reported', expected '$version'" >&2; exit 1; }
mkdir -p "$dest"
install -m 0755 "$work/shellcheck-v$version/shellcheck" "$dest/shellcheck"
echo "shellcheck $version: $dest/shellcheck"
