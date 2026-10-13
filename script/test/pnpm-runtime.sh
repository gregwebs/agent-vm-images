#!/usr/bin/env bash
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
root=$(cd "$(dirname "$0")/../.." && pwd)
[ "$#" -ge 1 ] || { echo 'usage: pnpm-runtime.sh BASE_IMAGE [STANDARD_IMAGE] [--platform linux/ARCH]' >&2; exit 2; }
images=()
platform=()
while [ "$#" -gt 0 ]; do
    case "$1" in
        --platform) [ "$#" -ge 2 ] || exit 2; platform=(--platform "$2"); shift 2 ;;
        -*) exit 2 ;;
        *) images+=("$1"); shift ;;
    esac
done
[ "${#images[@]}" -ge 1 ] && [ "${#images[@]}" -le 2 ] || exit 2
container="agent-vm-pnpm-$$-$(date +%s)"
cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for image in "${images[@]}"; do
    docker image inspect "$image" >/dev/null
    for pair in 0:0 12345:23456 12345:45678 54321:12345; do
        echo "pnpm runtime: $image uid:gid=$pair HOME=/tmp (network none)"
        docker run --rm --name "$container" "${platform[@]}" --network none --cap-drop ALL --user "$pair" \
            -e HOME=/tmp -v "$root:/source:ro" "$image" \
            python3 /source/script/test/pnpm-runtime.py
    done
    echo "pnpm runtime: $image writable HOME overmount"
    docker run --rm --name "$container" "${platform[@]}" --network none --cap-drop ALL \
        --user 12345:23456 --tmpfs /mounted-home:rw,exec,mode=1777 -e HOME=/mounted-home \
        -e PNPM_TEST_OVERMOUNT=1 -v "$root:/source:ro" "$image" \
        python3 /source/script/test/pnpm-runtime.py
done
