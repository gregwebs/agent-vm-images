#!/usr/bin/env bash
# Post-install sanity for the rust-dev image. Runs from the Dockerfile as
# `RUN --network=none` with the verifier bundle mounted at /run/rust-verify, and
# standalone from the checkout.
#
# It verifies the *installed* state against the committed pins (exact release and
# host, rustup version, the five host components and the musl target, no extra
# toolchains, minimal profile, auto-install/self-update disabled, the musl
# standard library present) and then invokes real tools. On amd64 the Verus check
# goes beyond `--version`: it verifies a trivial contract and requires the
# `N verified, 0 errors` line, so a Verus tree that cannot reach its bundled
# vstd + z3, or that exits 0 while verifying nothing (ADR-0018), fails the build.
# arm64 keeps the documented no-op: upstream publishes no Verus asset there.
#
# Environment:
#   RUST_TOOLCHAIN  required; the exact channel the pins record
#   VERUS_DIR       default /opt/verus
set -euo pipefail

: "${RUST_TOOLCHAIN:?RUST_TOOLCHAIN must be set to the pinned toolchain}"
VERUS_DIR=${VERUS_DIR:-/opt/verus}
VERIFY_DIR=$(cd "${BASH_SOURCE[0]%/*}" && pwd)
export PYTHONDONTWRITEBYTECODE=1
export PATH=/opt/cargo/bin:/opt/verus/verus-x86-linux:${PATH}

arch=$(dpkg --print-architecture)
/usr/bin/python3 "$VERIFY_DIR/rust_toolchain.py" verify-installed "$VERIFY_DIR/rust-toolchain" \
    --arch "$arch" --expect-channel "$RUST_TOOLCHAIN" \
    --rustup-home /opt/rustup --cargo-home /opt/cargo

cargo --version
cargo clippy --version
rustfmt --version

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
