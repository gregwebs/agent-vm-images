#!/usr/bin/env bash
# Post-install sanity for the rust-dev image. Runs from the Dockerfile as
# `RUN --network=none` with the verifier bundle mounted at /run/rust-verify, and
# standalone from the checkout.
#
# It verifies the *installed* state against the committed pins (exact release and
# host, rustup version, the five host components and the musl target, no extra
# toolchains, minimal profile, auto-install/self-update disabled, the musl
# standard library present, and the paired kache release behind RUSTC_WRAPPER)
# and then invokes real tools. On amd64 the Verus check goes beyond `--version`:
# it verifies a trivial contract and requires the `N verified, 0 errors` line, so
# a Verus tree that cannot reach its bundled vstd + z3, or that exits 0 while
# verifying nothing (ADR-0018), fails the build. arm64 keeps the documented no-op:
# upstream publishes no Verus asset there. kache's own probe compiles, stores and
# restores a small Rust library in an isolated cache with no remote, which fails
# here instead of in a guest if the wrapper, the daemon or the store is broken.
#
# Environment:
#   RUST_TOOLCHAIN  required; the exact channel the pins record
#   KACHE_VERSION   required; the exact kache release the pins record
#   VERUS_DIR       default /opt/verus
set -euo pipefail

: "${RUST_TOOLCHAIN:?RUST_TOOLCHAIN must be set to the pinned toolchain}"
: "${KACHE_VERSION:?KACHE_VERSION must be set to the pinned kache release}"
VERUS_DIR=${VERUS_DIR:-/opt/verus}
KACHE_DIR=${KACHE_DIR:-/opt/kache}
VERIFY_DIR=$(cd "${BASH_SOURCE[0]%/*}" && pwd)
export PYTHONDONTWRITEBYTECODE=1
export PATH=/opt/kache/bin:/opt/cargo/bin:/opt/verus/verus-x86-linux:${PATH}

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
# even for `--version`, and the image must not carry that state.
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
# `doctor` reports findings and still exits 0, so the JSON is what decides: no
# non-optional check may fail and the compile/store/restore probe must pass.
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
