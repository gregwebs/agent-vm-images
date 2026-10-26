#!/usr/bin/env bash
# Install the reviewed Rust toolchain closure for this host.
#
# Why it is this thin: the reviewed bytes, hashes and signatures live in
# rust-toolchain/, and rust_toolchain.py is the only program that verifies and
# installs them. This adapter exists so the Dockerfile keeps one plain RUN line
# and a standalone host build behaves the same way. There is no alternate install
# path, no live bootstrap download and no "latest" resolution here.
#
# Environment:
#   RUST_TOOLCHAIN  required; the exact channel the committed pin records
#   RUSTUP_HOME     fixed /opt/rustup  (a different value cannot work: the image
#   CARGO_HOME      fixed /opt/cargo   ENV names these, so it is refused)
set -euo pipefail

: "${RUST_TOOLCHAIN:?RUST_TOOLCHAIN must be set to the pinned channel (e.g. 1.98.1)}"
RUSTUP_HOME=${RUSTUP_HOME:-/opt/rustup}
CARGO_HOME=${CARGO_HOME:-/opt/cargo}
[ "$RUSTUP_HOME" = /opt/rustup ] || {
    echo "install-rust: RUSTUP_HOME must be the fixed /opt/rustup (got $RUSTUP_HOME)" >&2
    exit 1
}
[ "$CARGO_HOME" = /opt/cargo ] || {
    echo "install-rust: CARGO_HOME must be the fixed /opt/cargo (got $CARGO_HOME)" >&2
    exit 1
}
HERE=$(cd "${BASH_SOURCE[0]%/*}" && pwd)
export PYTHONDONTWRITEBYTECODE=1

arch=$(dpkg --print-architecture)
case "$arch" in
    amd64 | arm64) ;;
    *)
        echo "install-rust: unsupported architecture: $arch" >&2
        exit 1
        ;;
esac

/usr/bin/python3 "$HERE/rust_toolchain.py" install "$HERE/rust-toolchain" \
    --arch "$arch" --expect-channel "$RUST_TOOLCHAIN" \
    --rustup-home "$RUSTUP_HOME" --cargo-home "$CARGO_HOME"

# The guest runs as an arbitrary uid (ADR-0001): the shared tree is readable and
# searchable by everyone and writable by no one else.
chmod -R a+rX "$RUSTUP_HOME" "$CARGO_HOME"
echo "==> install-rust: $RUST_TOOLCHAIN ($arch) installed from committed pins"
