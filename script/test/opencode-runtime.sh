#!/usr/bin/env bash
# Credential-free finished-image cases; native builds remain prerequisites.
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
[ "$#" -eq 2 ] || { echo 'usage: opencode-runtime.sh BASE_IMAGE STANDARD_IMAGE' >&2; exit 2; }
container="agent-vm-opencode-$$-$(date +%s)"
cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker image inspect "$1" "$2" >/dev/null
# The audit containers run a Linux helper, so the platform comes from the image
# rather than the host, and the composed directory is built once.
platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$2")
case "$platform" in
    linux/arm64|linux/arm64/v8) contract_arch=arm64 ;;
    linux/amd64) contract_arch=amd64 ;;
    *) echo "opencode-runtime: unsupported audit platform $platform" >&2; exit 1 ;;
esac
base_platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$1")
case "$base_platform" in
    linux/arm64|linux/arm64/v8) base_arch=arm64 ;;
    linux/amd64) base_arch=amd64 ;;
    *) echo "opencode-runtime: unsupported base platform $base_platform" >&2; exit 1 ;;
esac
[ "$contract_arch" = "$base_arch" ] ||
    { echo "opencode-runtime: base and standard platforms disagree" >&2; exit 1; }
CONTRACT_CONTEXT="$(bash "$ROOT/script/build/recipe-contract.sh" --platform "$platform")"
version=$(jq -r '.dependencies["@openchamber/opencode-claude"]' "$ROOT/images/tools/opencode/plugins/package.json")
for pair in 0:0 12345:23456 12345:45678 54321:12345; do
    python3 "$ROOT/script/test/host-watchdog.py" 300 docker run --rm --name "$container" --network none --cap-drop ALL \
        --user "$pair" --tmpfs /home/probe:rw,exec,mode=1777 -e HOME=/home/probe \
        -v "$ROOT/images/tools/opencode:/opencode-source:ro" \
        -v "$CONTRACT_CONTEXT/out:/contract:ro" -v "$ROOT/script/test:/audit:ro" \
        --entrypoint bash "$2" /audit/opencode-guest-audit.sh /opencode-source /audit/opencode-audit.py "$version"
    python3 "$ROOT/script/test/host-watchdog.py" 300 docker run --rm --name "$container" --network none --cap-drop ALL \
        --user "$pair" --tmpfs /home/probe:rw,exec,mode=1777 -e HOME=/home/probe \
        -v "$ROOT/images/tools/opencode:/opencode-source:ro" \
        -v "$CONTRACT_CONTEXT/out:/contract:ro" -v "$ROOT/script/test:/audit:ro" \
        --entrypoint python3 "$2" /audit/opencode-runtime-mutations.py
done

python3 "$ROOT/script/test/host-watchdog.py" 900 docker run --rm --name "$container" --network none --cap-drop ALL \
    --user 12345:23456 -e HOME=/tmp -v "$ROOT:/source:ro" \
    --entrypoint python3 "$1" /source/script/test/opencode-pnpm-policy.py
