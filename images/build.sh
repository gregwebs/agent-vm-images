#!/bin/bash
# Convenience only: Docker owns inheritance, cache, options and failure semantics.
set -euo pipefail

usage() {
    cat <<'HELP'
Usage: bash images/build.sh base|standard [-- DOCKER_BUILD_OPTIONS...]
       bash images/build.sh --help

Builds one recipe with images/ as context and a native Linux platform.
Requires the current context's named builder to use Driver: docker.
Standard requires an already-built agent-vm-base:local (or explicit BASE_IMAGE).
Use ordinary Docker commands for a different builder/platform or remote daemon.
HELP
}

if [ "$#" -eq 1 ] && [ "$1" = --help ]; then
    usage
    exit 0
fi
if [ "$#" -eq 0 ]; then usage >&2; exit 2; fi
recipe=$1
shift
case "$recipe" in
    base) dockerfile=Dockerfile; tag=agent-vm-base:local ;;
    standard) dockerfile=standard/Dockerfile; tag=agent-vm-standard:local ;;
    *) usage >&2; exit 2 ;;
esac
if [ "$#" -gt 0 ]; then
    if [ "$1" != -- ]; then usage >&2; exit 2; fi
    shift
fi
case "$(uname -m)" in
    arm64|aarch64) platform=linux/arm64 ;;
    x86_64|amd64) platform=linux/amd64 ;;
    *) echo 'Unsupported native architecture; run ordinary Docker commands with an explicit Linux platform.' >&2; exit 1 ;;
esac
images="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
builder="$(docker context show)"
inspection="$(docker buildx inspect "$builder")"
if ! printf '%s\n' "$inspection" | grep -Eq '^Driver:[[:space:]]+docker[[:space:]]*$'; then
    echo "Builder $builder is not daemon-backed (Driver: docker). Inspect docker buildx ls and select the current context's daemon-backed builder with ordinary Docker commands." >&2
    exit 1
fi
exec docker build --builder "$builder" --platform "$platform" -t "$tag" \
    -f "$images/$dockerfile" "$@" "$images"
