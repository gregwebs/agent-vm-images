#!/usr/bin/env bash
# Post-install sanity for the rust-dev image. Runs from the Dockerfile as
# `RUN --network=none` with the verifier bundle mounted at /run/rust-verify, and
# standalone from the checkout.
#
# It verifies the *installed* state against the committed pins (exact release and
# host, rustup version, the five host components and the musl target, no extra
# toolchains, minimal profile, auto-install/self-update disabled, the musl
# standard library present, and the paired kache release behind RUSTC_WRAPPER and
# behind the C/C++ shims on PATH) and then invokes real tools. On amd64 the Verus
# check goes beyond `--version`:
# it verifies a trivial contract and requires the `N verified, 0 errors` line, so
# a Verus tree that cannot reach its bundled vstd + z3, or that exits 0 while
# verifying nothing (ADR-0018), fails the build. arm64 keeps the documented no-op:
# upstream publishes no Verus asset there. kache's own probe compiles, stores and
# restores a small Rust library in an isolated cache with no remote, and a
# trivial C file is compiled and run through the shim, so a wrapper, a shim, the
# daemon or the store that is broken fails here instead of in a guest.
#
# Environment:
#   RUST_TOOLCHAIN  required; the exact channel the pins record
#   KACHE_VERSION   required; the exact kache release the pins record
#   KACHE_SHIM_DIR  default /opt/kache/shims
#   KACHE_CONFIG, KACHE_CACHE_DIR  dropped: the verifier proves the installed
#                   binaries against an isolated scratch store, not the image's
#                   policy (which is checked by the Dockerfile separately)
#   VERUS_DIR       default /opt/verus
set -euo pipefail

: "${RUST_TOOLCHAIN:?RUST_TOOLCHAIN must be set to the pinned toolchain}"
: "${KACHE_VERSION:?KACHE_VERSION must be set to the pinned kache release}"
VERUS_DIR=${VERUS_DIR:-/opt/verus}
KACHE_DIR=${KACHE_DIR:-/opt/kache}
KACHE_SHIM_DIR=${KACHE_SHIM_DIR:-/opt/kache/shims}
VERIFY_DIR=$(cd "${BASH_SOURCE[0]%/*}" && pwd)
export PYTHONDONTWRITEBYTECODE=1
# The image's own PATH (see the Dockerfile): the shim farm first, so `cc` and
# friends are kache, then the two toolchain directories.
export PATH=$KACHE_SHIM_DIR:/opt/kache/bin:/opt/cargo/bin:/opt/verus/verus-x86-linux:${PATH}

arch=$(dpkg --print-architecture)
/usr/bin/python3 "$VERIFY_DIR/rust_toolchain.py" verify-installed "$VERIFY_DIR/rust-toolchain" \
    --arch "$arch" --expect-channel "$RUST_TOOLCHAIN" \
    --rustup-home /opt/rustup --cargo-home /opt/cargo

cargo --version
cargo clippy --version
rustfmt --version

# kache: the wrapper every guest cargo invocation runs through, both binaries of
# the paired release at the pinned version, and a real compile-store-restore
# cycle. The probe is given a scratch HOME because kache logs and caches under it
# even for `--version`, and the image must not carry that state. The image's own
# kache policy is dropped for the duration: this step proves the installed
# binaries work, and it is specified against a kache with no config file, no
# remote and a local store it can throw away. Without this the same script, run
# inside the finished image (and it is, from script/test/rust-example.sh), would
# inherit KACHE_CONFIG and KACHE_CACHE_DIR and silently prove something else.
unset KACHE_CONFIG KACHE_CACHE_DIR
kache_scratch=$(mktemp -d)
[ "${RUSTC_WRAPPER:-}" = "$KACHE_DIR/bin/kache" ] || {
    rm -rf "$kache_scratch"
    echo "verify-toolchain: RUSTC_WRAPPER is '${RUSTC_WRAPPER:-}', not $KACHE_DIR/bin/kache" >&2
    exit 1
}
[ "$(HOME="$kache_scratch" "$KACHE_DIR/bin/kache" --version)" = "kache $KACHE_VERSION" ] || {
    echo "verify-toolchain: kache does not report the pinned release $KACHE_VERSION" >&2
    exit 1
}
[ "$(HOME="$kache_scratch" "$KACHE_DIR/bin/kache-engine" --version)" = "kache $KACHE_VERSION" ] || {
    echo "verify-toolchain: kache-engine does not report the pinned release $KACHE_VERSION" >&2
    exit 1
}
# The C/C++ shims are image state, not a guest setup step (ADR 0015): a farm of
# native symlinks to the pinned binary, first on PATH, with the `.kache-shims`
# marker that makes kache skip the directory when it resolves the real compiler.
# Rust keeps running through RUSTC_WRAPPER alone, so no `cargo` or `rustdoc` link
# belongs here. The compile below then proves the whole chain offline - PATH, the
# shim, the real compiler it finds, and a cache restore - rather than only that
# the links exist.
[ -f "$KACHE_SHIM_DIR/.kache-shims" ] || {
    echo "verify-toolchain: $KACHE_SHIM_DIR carries no .kache-shims marker" >&2
    exit 1
}
for name in cc c++ gcc g++ clang clang++; do
    [ "$(readlink "$KACHE_SHIM_DIR/$name" 2>/dev/null)" = "$KACHE_DIR/bin/kache" ] || {
        echo "verify-toolchain: $KACHE_SHIM_DIR/$name is not a symlink to $KACHE_DIR/bin/kache" >&2
        exit 1
    }
done
for name in cargo rustdoc; do
    [ ! -e "$KACHE_SHIM_DIR/$name" ] || {
        echo "verify-toolchain: $KACHE_SHIM_DIR/$name must not be a shim: Rust runs through RUSTC_WRAPPER" >&2
        exit 1
    }
done
[ "$(command -v cc)" = "$KACHE_SHIM_DIR/cc" ] || {
    echo "verify-toolchain: PATH leads cc to $(command -v cc), not the kache shim" >&2
    exit 1
}
mkdir -p "$kache_scratch/c"
printf 'int main(void) { return 0; }\n' >"$kache_scratch/c/smoke.c"
HOME="$kache_scratch" TMPDIR="$kache_scratch" \
    cc -o "$kache_scratch/c/smoke" "$kache_scratch/c/smoke.c"
HOME="$kache_scratch" "$kache_scratch/c/smoke"
# `doctor` reports findings and still exits 0, so the JSON is what decides: no
# non-optional check may fail, the compile/store/restore probe must pass, and the
# shims the image installs must be reported active (kache_probe.py).
HOME="$kache_scratch" TMPDIR="$kache_scratch" "$KACHE_DIR/bin/kache" doctor --probe --json \
    >"$kache_scratch/doctor.json"
/usr/bin/python3 "$VERIFY_DIR/kache_probe.py" "$kache_scratch/doctor.json"
# Explicitly, not through the EXIT trap: the Verus branch below installs its own.
rm -rf "$kache_scratch"

if [ "$arch" = amd64 ]; then
    verus="$VERUS_DIR/verus-x86-linux/verus"
    "$verus" --version
    "$VERUS_DIR/verus-x86-linux/z3" --version

    smoke=$(mktemp -d)
    trap 'rm -rf "$smoke"' EXIT
    cat > "$smoke/smoke.rs" <<'RS'
use vstd::prelude::*;

verus! {
proof fn layer_smoke()
    ensures true,
{
}
}
RS
    smoke_out=$( (cd "$smoke" && "$verus" --crate-type=lib smoke.rs) 2>&1 )
    printf '%s\n' "$smoke_out"
    printf '%s\n' "$smoke_out" |
        grep -Eq 'verification results:: [1-9][0-9]* verified, 0 errors'
fi

echo "== rust-dev layer sanity OK =="
