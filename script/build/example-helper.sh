#!/bin/bash
# Host build of an example layer's own helper module.
#
# An example layer is self-contained (ADR 0017): its helper is its own Go module
# under `helper/`, and the layer's pinned `golang:` stage is what compiles it for
# the image. This wrapper compiles the same module for the host with the same
# flags, so a suite can run the binary the Dockerfile would mount without building
# an image. It is the only place the host flags are written down.
#
# It prints one absolute path: the binary it wrote. Pass `--fixture` to build with
# `-tags example_fixture`, which is how a suite gets the fixture seams (decision
# D1); the shipped binary never carries them.
set -euo pipefail

usage() {
    cat <<'HELP'
usage: script/build/example-helper.sh LAYER [--root DIR] [--os linux|darwin]
                                         [--arch amd64|arm64] [--binary NAME]
                                         [--fixture]

Compiles examples/layers/LAYER/helper with the flags the layer's Dockerfile stage
uses and prints the binary's absolute path. Defaults: --root is this checkout,
--os is linux, --arch is the host's, --binary is LAYER-helper (LAYER-helper-fixture
with --fixture), and no fixture tag.
HELP
}

# Every error path prints usage to stderr and exits 2; --help alone exits 0.
invalid() { usage >&2; exit 2; }

layer=
root=
os=linux
arch=
binary=
fixture=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --help)
            [ "$#" -eq 1 ] || invalid
            usage
            exit 0
            ;;
        --root)
            [ "$#" -ge 2 ] && [ -z "$root" ] || invalid
            root=$2
            shift 2
            ;;
        --os)
            [ "$#" -ge 2 ] || invalid
            os=$2
            shift 2
            ;;
        --arch)
            [ "$#" -ge 2 ] || invalid
            arch=$2
            shift 2
            ;;
        --binary)
            [ "$#" -ge 2 ] || invalid
            binary=$2
            shift 2
            ;;
        --fixture)
            [ "$fixture" = false ] || invalid
            fixture=true
            shift
            ;;
        -*)
            invalid
            ;;
        *)
            [ -z "$layer" ] || invalid
            layer=$1
            shift
            ;;
    esac
done
[ -n "$layer" ] || invalid

# dirname, not ${BASH_SOURCE[0]%/*}: the latter yields the bare file name when the
# script is invoked as `bash example-helper.sh` from its own directory.
script_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
root=${root:-$script_root}
root=$(cd "$root" && pwd)
module="$root/examples/layers/$layer/helper"
[ -f "$module/go.mod" ] || { echo "example-helper: no helper module: $module/go.mod" >&2; exit 2; }

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

# The binary keeps the layer's name so a caller never has to know Go's default
# (the module directory's base name would be `helper`). A fixture build gets its
# own name, so the two builds cannot overwrite each other: a gate that runs the
# shipped binary while a differential suite builds the fixture one would otherwise
# race on one path, and the loser would run the wrong program.
default_binary=$layer-helper
if [ "$fixture" = true ]; then
    default_binary=$layer-helper-fixture
fi
binary=${binary:-$default_binary}
case "$binary" in
    ''|*/*|.|..) echo "unsupported --binary: $binary" >&2; usage >&2; exit 2 ;;
esac

out="$root/target/example-helper/$layer/$os-$arch"
mkdir -p "$out"

tags=()
if [ "$fixture" = true ]; then
    tags=(-tags example_fixture)
fi

# The stage runs with GOTOOLCHAIN=local because there the compiler must be the one
# helper/go.mod names (the first link of the Dockerfile's guard chain, which fails
# the build otherwise). On the host the module's own toolchain selection is what
# makes the same rule hold, so this uses GOTOOLCHAIN=auto: go rewrites the
# compiler to go.mod's `toolchain` line or refuses.
(
    cd "$module" || exit 1
    env -u GOEXPERIMENT -u GOFLAGS -u CGO_ENABLED -u GOOS -u GOARCH -u GOWORK \
        GOTOOLCHAIN=auto GOENV=off GOWORK=off GOOS="$os" GOARCH="$arch" \
        GOAMD64=v1 GOARM64=v8.0 GOFLAGS=-mod=vendor GOPROXY=off CGO_ENABLED=0 \
        go build -trimpath -buildvcs=false -o "$out/$binary" ${tags[@]+"${tags[@]}"} .
) >&2

printf '%s\n' "$out/$binary"
