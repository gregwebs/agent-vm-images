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

The recipe-contract helper is compiled first, with the host Go toolchain
(script/build/recipe-contract.sh), and passed as the `recipe-contract-bin` named
build context: the Dockerfiles contain no Go compiler (ADR 0017). Go with
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
if [ "$#" -gt 0 ]; then
    if [ "$1" != -- ]; then usage >&2; exit 2; fi
    shift
fi
# This script owns the recipe-contract-bin name and appends it last, so a
# caller-supplied replacement would be silently overridden. A caller who supplies
# the helper themselves is invoking docker, not this script, so refuse the
# collision rather than letting the later context win.
conflict() {
    echo 'the recipe-contract-bin build context is owned by images/build.sh' >&2
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
# The host build prints one absolute context root on stdout; diagnostics go to
# stderr, so this substitution is the whole interface. --fresh because this build
# owns what reaches the image: a RECIPE_CONTRACT_CONTEXT inherited from a test run
# must not stand in for it.
host_context="$(bash "$images/../script/build/recipe-contract.sh" --fresh --os linux --arch "$arch")"
exec docker build --builder "$builder" --platform "$platform" -t "$tag" \
    -f "$images/$dockerfile" --build-context "recipe-contract-bin=$host_context" \
    "$@" "$images"
