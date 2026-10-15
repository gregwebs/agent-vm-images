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
version=$(jq -r '.dependencies["@openchamber/opencode-claude"]' "$ROOT/images/tools/opencode/plugins/package.json")
for pair in 0:0 12345:23456 12345:45678 54321:12345; do
    python3 "$ROOT/script/test/host-watchdog.py" 300 docker run --rm --name "$container" --network none --cap-drop ALL \
        --user "$pair" --tmpfs /home/probe:rw,exec,mode=1777 -e HOME=/home/probe \
        -v "$ROOT/images/tools/opencode:/opencode-source:ro" \
        -v "$ROOT/images/recipe-contract:/contract:ro" -v "$ROOT/script/test:/audit:ro" \
        --entrypoint bash "$2" /audit/opencode-guest-audit.sh /opencode-source /audit/opencode-audit.py "$version"
    python3 "$ROOT/script/test/host-watchdog.py" 300 docker run --rm --name "$container" --network none --cap-drop ALL \
        --user "$pair" --tmpfs /home/probe:rw,exec,mode=1777 -e HOME=/home/probe \
        -v "$ROOT/images/tools/opencode:/opencode-source:ro" \
        -v "$ROOT/images/recipe-contract:/contract:ro" -v "$ROOT/script/test:/audit:ro" \
        --entrypoint python3 "$2" /audit/opencode-runtime-mutations.py
done

python3 "$ROOT/script/test/host-watchdog.py" 900 docker run --rm --name "$container" --network none --cap-drop ALL \
    --user 12345:23456 -e HOME=/tmp -v "$ROOT:/source:ro" \
    --entrypoint python3 "$1" /source/script/test/opencode-pnpm-policy.py
