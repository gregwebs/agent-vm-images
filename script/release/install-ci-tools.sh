#!/usr/bin/env bash
# Provision only invocation-owned tools on disposable Actions Linux hosts.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_OS:-}" = Linux ] || { echo 'disposable Linux Actions runner only' >&2; exit 1; }
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) exit 1 ;; esac
timeout --kill-after=10s 600 sudo apt-get update
timeout --kill-after=10s 600 sudo apt-get install -y skopeo coreutils jq python3
out=$(mktemp -d "$RUNNER_TEMP/release-tools.XXXXXX")
mkdir "$out/bin"
version=$(jq -r .syft_version "$ROOT/script/release/tool-pins.json")
hash=$(jq -r ".syft_linux_${arch}_sha256" "$ROOT/script/release/tool-pins.json")
env -i PATH="$PATH" HOME=/nonexistent CURL_HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent \
    curl -q --netrc-file /dev/null --max-filesize 100000000 --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 \
    "https://github.com/anchore/syft/releases/download/v$version/syft_${version}_linux_${arch}.tar.gz" -o "$out/syft.tar.gz"
printf '%s  %s\n' "$hash" "$out/syft.tar.gz" | sha256sum -c -
tar -xzf "$out/syft.tar.gz" -C "$out/bin" syft
chmod 0755 "$out/bin/syft"
printf '%s\n' "$out/bin" >> "$GITHUB_PATH"
skopeo --version
python3 --version
jq --version
split --version
"$out/bin/syft" version
# The verifier's policy and JSON schema are security inputs, not runner defaults.
gh_version=$(jq -er .gh_version "$ROOT/script/release/tool-pins.json")
gh_hash=$(jq -er ".gh_linux_${arch}_sha256" "$ROOT/script/release/tool-pins.json")
env -i PATH="$PATH" HOME=/nonexistent CURL_HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent \
    curl -q --netrc-file /dev/null --max-filesize 100000000 --fail --silent --show-error --location \
    --proto '=https' --proto-redir '=https' --connect-timeout 15 --max-time 300 \
    "https://github.com/cli/cli/releases/download/v$gh_version/gh_${gh_version}_linux_${arch}.tar.gz" -o "$out/gh.tar.gz"
printf '%s  %s\n' "$gh_hash" "$out/gh.tar.gz" | sha256sum -c -
tar -xzf "$out/gh.tar.gz" -C "$out" "gh_${gh_version}_linux_${arch}/bin/gh"
cp "$out/gh_${gh_version}_linux_${arch}/bin/gh" "$out/bin/gh"
export PATH="$out/bin:$PATH"
gh --version | tee "$out/gh-version.log"
gh attestation verify --help > "$out/gh-verifier-help.txt"
for flag in deny-self-hosted-runners source-ref source-digest predicate-type bundle; do
    grep -q -- "--$flag" "$out/gh-verifier-help.txt" || { echo "gh verifier lacks --$flag" >&2; exit 1; }
done
