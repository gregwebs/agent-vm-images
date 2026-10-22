#!/bin/bash
# Host delivery adapter for the recipe-contract build helper.
#
# Compiles the helper with the host's Go toolchain and prints one absolute
# composed-directory path. Pass it as a Docker named build context
# (`--build-context recipe-contract-bin=PATH`) to replace the builder stage.
set -euo pipefail

usage() {
    cat <<'HELP'
usage: script/build/recipe-contract.sh [--native | --platform linux/ARCH[/v8] | --os linux|darwin --arch amd64|arm64]

Builds images/recipe-contract with the host Go toolchain and prints the absolute
context root containing out/recipe-contract. Defaults: --os linux, host arch.
Requires Go (GOTOOLCHAIN=auto may download the version go.mod selects).
HELP
}

# Every error path prints usage to stderr and exits 2; --help alone exits 0.
invalid() { usage >&2; exit 2; }

os=linux
arch=
selection=explicit
platform=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --help)
            [ "$#" -eq 1 ] || invalid
            usage
            exit 0
            ;;
        --native)
            [ "$selection" = explicit ] && [ -z "$arch" ] && [ "$os" = linux ] || invalid
            selection=native
            shift
            ;;
        --platform)
            [ "$#" -ge 2 ] && [ "$selection" = explicit ] && [ -z "$arch" ] && [ "$os" = linux ] || invalid
            selection=platform
            platform=$2
            shift 2
            ;;
        --os)
            [ "$#" -ge 2 ] || invalid
            [ "$selection" = explicit ] || invalid
            os=$2
            shift 2
            ;;
        --os=*)
            [ "$selection" = explicit ] || invalid
            os=${1#--os=}
            shift
            ;;
        --arch)
            [ "$#" -ge 2 ] || invalid
            [ "$selection" = explicit ] || invalid
            arch=$2
            shift 2
            ;;
        --arch=*)
            [ "$selection" = explicit ] || invalid
            arch=${1#--arch=}
            shift
            ;;
        *) invalid ;;
    esac
done
case "$selection" in
    native) os=$(uname -s | tr '[:upper:]' '[:lower:]') ;;
    platform)
        case "$platform" in
            linux/amd64) os=linux; arch=amd64 ;;
            linux/arm64|linux/arm64/v8) os=linux; arch=arm64 ;;
            *) echo "unsupported --platform: $platform" >&2; invalid ;;
        esac
        ;;
esac
case "$os" in
    linux|darwin) ;;
    *) echo "unsupported --os: $os" >&2; usage >&2; exit 2 ;;
esac
if [ -z "$arch" ]; then
    case "$(uname -m)" in
        arm64|aarch64) arch=arm64 ;;
        x86_64|amd64) arch=amd64 ;;
        *) echo "unsupported host architecture; pass --arch amd64|arm64" >&2; usage >&2; exit 2 ;;
    esac
fi
case "$arch" in
    amd64|arm64) ;;
    *) echo "unsupported --arch: $arch" >&2; usage >&2; exit 2 ;;
esac

# dirname, not ${BASH_SOURCE[0]%/*}: the latter yields the bare file name when the
# script is invoked as `bash recipe-contract.sh` from its own directory.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
context="$root/target/recipe-contract/$os-$arch"

# A caller may supply the composed directory it already holds: the owning run
# built this platform before entering a restricted fixture environment, and a
# fixture stands in for the build. Reuse is honoured for any supplied context that
# carries the binary, not only the canonical path, because a fixture's context has
# to live under its own scratch root. One context is often exported for a whole
# suite run, so a context that records another platform is ignored and this build
# proceeds; contexts written before the record existed are taken as the caller's
# declaration.
supplied=${RECIPE_CONTRACT_CONTEXT:-}
if [ -n "$supplied" ] && [ -f "$supplied/out/recipe-contract" ]; then
    recorded=
    [ ! -f "$supplied/out/.recipe-contract-platform" ] ||
        recorded=$(cat "$supplied/out/.recipe-contract-platform" 2>/dev/null || :)
    if [ -z "$recorded" ] || [ "$recorded" = "$os-$arch" ]; then
        printf '%s\n' "$supplied"
        exit 0
    fi
fi

# A predictable build: no ambient target/platform/feature overrides, no
# workspace, no user flags. GOTOOLCHAIN=auto lets go.mod select the patch release.
env -u GOOS -u GOARCH -u GOAMD64 -u GOARM64 -u GOEXPERIMENT -u GOFLAGS -u CGO_ENABLED -u GOWORK \
    GOTOOLCHAIN=auto \
    GOOS="$os" GOARCH="$arch" GOAMD64=v1 GOARM64=v8.0 \
    bash "$root/images/recipe-contract/build.sh" "$context/out" >&2

printf '%s\n' "$context"
