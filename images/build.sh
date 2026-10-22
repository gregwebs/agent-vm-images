#!/bin/bash
# Convenience only: Docker owns inheritance, cache, options and failure semantics.
set -euo pipefail

usage() {
    cat <<'HELP'
Usage: bash images/build.sh base|standard [--host-helpers] [-- DOCKER_BUILD_OPTIONS...]
       bash images/build.sh --help

Builds one recipe with images/ as context and a native Linux platform.
Requires the current context's named builder to use Driver: docker.
Standard requires an already-built agent-vm-base:local (or explicit BASE_IMAGE).
Use ordinary Docker commands for a different builder/platform or remote daemon.

--host-helpers compiles images/recipe-contract with the host Go toolchain
(script/build/recipe-contract.sh) and passes it as the `recipe-contract-bin`
named build context instead of running the Dockerfile's Go builder stage. Go with
GOTOOLCHAIN=auto (which may download the version go.mod selects) is required.
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
host_helpers=false
if [ "$#" -gt 0 ] && [ "$1" = --host-helpers ]; then
    host_helpers=true
    shift
fi
if [ "$#" -gt 0 ]; then
    if [ "$1" != -- ]; then usage >&2; exit 2; fi
    shift
fi
# --host-helpers owns the recipe-contract-bin name and appends it last, so a
# caller-supplied replacement would be silently overridden: refuse that only in
# host mode. A raw caller who names the seam themselves is using it, not
# colliding with it, and ordinary Docker argv is passed through untouched.
if [ "$host_helpers" = true ]; then
    conflict() {
        echo 'the recipe-contract-bin build context is owned by --host-helpers' >&2
        exit 2
    }
    previous=
    for argument in "$@"; do
        case "$argument" in
            --build-context=recipe-contract-bin=*) conflict ;;
            recipe-contract-bin=*)
                [ "${previous:-}" = --build-context ] && conflict
                ;;
        esac
        previous=$argument
    done
fi
case "$(uname -m)" in
    arm64|aarch64) platform=linux/arm64; arch=arm64 ;;
    x86_64|amd64) platform=linux/amd64; arch=amd64 ;;
    *) echo 'Unsupported native architecture; run ordinary Docker commands with an explicit Linux platform.' >&2; exit 1 ;;
esac
# dirname, not ${BASH_SOURCE[0]%/*}: that expansion yields "build.sh" when the
# script is invoked by name from this directory, which `cd` cannot enter.
images="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
builder="$(docker context show)"
inspection="$(docker buildx inspect "$builder")"
if ! printf '%s\n' "$inspection" | grep -Eq '^Driver:[[:space:]]+docker[[:space:]]*$'; then
    echo "Builder $builder is not daemon-backed (Driver: docker). Inspect docker buildx ls and select the current context's daemon-backed builder with ordinary Docker commands." >&2
    exit 1
fi
contexts=()
if [ "$host_helpers" = true ]; then
    # The host build prints one absolute context root on stdout; diagnostics go to
    # stderr, so this substitution is the whole interface.
    host_context="$(bash "$images/../script/build/recipe-contract.sh" --os linux --arch "$arch")"
    contexts+=(--build-context "recipe-contract-bin=$host_context")
fi
exec docker build --builder "$builder" --platform "$platform" -t "$tag" \
    -f "$images/$dockerfile" ${contexts[@]+"${contexts[@]}"} "$@" "$images"
