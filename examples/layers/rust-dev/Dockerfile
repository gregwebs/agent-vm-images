# examples/layers/rust-dev — Rust development toolchain custom boot image.
#
# Build it, import it, and select it to build, test, lint and formally verify
# agent-vm's Rust code inside the microVM (there is no launcher-side layering,
# #259):
#
#   agent-vm build --builder native-oci \
#     --build-arg BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de \
#     --tag agent-vm-rust-dev:dev examples/layers/rust-dev
#   agent-vm claude --image agent-vm-rust-dev:dev
#
# It provides the toolchain agent-vm pins (its rust-toolchain.toml) with clippy and
# rustfmt, the musl target a vendored agentd/msb build needs, the native libraries
# agent-vm's ci.yml installs, shellcheck for its script/test/ci-contracts.sh, the
# pinned kache compiler cache that reuses builds across worktrees, and the pinned
# Verus release CI verifies with. See examples/layers/README.md and agent-vm's
# docs/adr/0035-consume-user-owned-boot-images.md.
#
# The steps that need real shell live as standalone, independently runnable
# scripts beside this file and are bind-mounted in at build time:
# install-rust.sh, install-kache.sh, install-verus.sh, verify-toolchain.sh. This
# file only wires the pins and locations into them.
#
# The `ARG BASE_IMAGE`/`FROM ${BASE_IMAGE}` pair lets `--build-arg BASE_IMAGE=…`
# retarget the build, and every `ENV` keeps `PATH` additive
# (`PATH=<new>:${PATH}`) so the base's directories are never dropped.

ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de
FROM ${BASE_IMAGE}

# Runtime network requests (ADR 0014; examples/layers/README.md#declaring-network-requests):
# cargo's sparse index, crate downloads and the crates.io API. Only this layer's
# own key: the parent's requests stay under their own keys.
LABEL org.agent-vm.network-requests.image.rust-dev="crates.io,index.crates.io,static.crates.io"

# Pins. These mirror agent-vm's canonical pins: RUST_TOOLCHAIN its
# rust-toolchain.toml channel, VERUS_RELEASE/VERUS_SHA256 its
# .github/workflows/verus.yml. This repository does not check out/read those files;
# docs/image-source-ownership.md states the bump order and what is and is not
# machine-checked. Move the two Verus values together.
ARG RUST_TOOLCHAIN=1.98.1
ARG VERUS_RELEASE=0.2026.09.20.aef82ed
ARG VERUS_SHA256=7b870fa12bc589015c2fab60a8b3d9f07c7b1adb3444eb0fadffcbf7f0447b33

# kache pins (https://github.com/kunobi-ninja/kache, Apache-2.0). Upstream ships
# one static musl tarball per host with a `.sha256` sidecar and a detached
# OpenPGP signature; kache/ commits that sidecar and the release key
# ("Kunobi Release Signing <feedback@kunobi.ninja>", RSA-4096, fingerprint
# 6069 18FD A7F1 854E 78B6 83CE 9A99 0E6B CBCC A73B, taken from
# https://r2.kunobi.com/kache/apt/gpg.key). install-kache.sh verifies the key
# digest, the tarball digest, the published sidecar and the signature before
# anything is extracted. Move all four values together with kache/.
ARG KACHE_VERSION=2.0.0
ARG KACHE_AMD64_SHA256=6c59665faf1b14d0944391e6f2a1b510195e6be92c63060a9d1770ed00676824
ARG KACHE_ARM64_SHA256=8c5ca4d1d4b6fc66e5175c8a1150e1daa85099080646322058d9083c72986bba
ARG KACHE_RELEASE_CERT_SHA256=8f5c02d62885c8d28647cc655e5e28f1243ce445a495cec73085f2975a767150

# Where rustup keeps the toolchain and its shims. Both live under /opt so the
# arbitrary-uid guest (ADR-0001) can read them. RUSTUP_HOME is exported at
# runtime so the shims find the toolchain; CARGO_HOME is deliberately *not*,
# so cargo keeps its registry/build cache in the guest's own writable
# $HOME/.cargo instead of the read-only shared tree.
ARG RUSTUP_HOME=/opt/rustup
ARG CARGO_HOME=/opt/cargo

# Where kache keeps its two binaries. /opt so the arbitrary-uid guest (ADR-0001)
# can read them; its cache and log stay in each guest's own $HOME.
ARG KACHE_DIR=/opt/kache

# Native build dependencies for cargo build/test/clippy on this workspace,
# mirroring agent-vm's ci.yml's "Install build dependencies" step (GitHub's runner
# already has build-essential; this slim base does not):
#   - build-essential, pkg-config: cc/pkg-config for the -sys crates
#     (libcap-ng, dbus) the microsandbox SDK links against;
#   - musl-tools: the *-unknown-linux-musl target the guest agentd needs
#     when building the vendored msb (agent-vm itself does not need it);
#   - shellcheck: agent-vm's script/test/ci-contracts.sh lints its scripts;
#   - python3 + gnupg + ca-certificates: the reviewed pin verifier needs a real
#     gpg (OpenPGP verification of the manifest and of every selected archive)
#     and authenticated HTTPS to fetch them. The released parent may predate
#     these, so this layer installs them explicitly from official Debian apt.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        build-essential pkg-config \
        libcap-ng-dev libdbus-1-dev \
        musl-tools \
        shellcheck \
        python3 gnupg ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Rust toolchain: the reviewed closure in rust-toolchain/ (exact bootstrap,
# signed manifest and every selected archive hash for this host) is installed by
# rust_toolchain.py, which verifies all of it before anything executes. The
# helper and the data are mounted read-only for this one step and are not part of
# the image; the installed /opt toolchain is.
RUN --mount=type=bind,source=install-rust.sh,target=/tmp/install-rust.sh,ro \
    --mount=type=bind,source=rust_toolchain.py,target=/tmp/rust_toolchain.py,ro \
    --mount=type=bind,source=rust-toolchain,target=/tmp/rust-toolchain,ro \
    bash /tmp/install-rust.sh

# kache, the compiler cache every guest cargo invocation runs through. Both
# binaries of the paired release are installed from the committed pins; a
# mismatch, a changed key or a signature that does not verify fails the build
# before extraction. The helper and the evidence are mounted read-only for this
# one step and are not part of the image.
RUN --mount=type=bind,source=install-kache.sh,target=/tmp/install-kache.sh,ro \
    --mount=type=bind,source=kache,target=/tmp/kache-pin,ro \
    KACHE_VERSION=$KACHE_VERSION \
    KACHE_AMD64_SHA256=$KACHE_AMD64_SHA256 KACHE_ARM64_SHA256=$KACHE_ARM64_SHA256 \
    KACHE_RELEASE_CERT_SHA256=$KACHE_RELEASE_CERT_SHA256 KACHE_DIR=$KACHE_DIR KACHE_DATA=/tmp/kache-pin \
    bash /tmp/install-kache.sh

# Verus, the SMT-backed verifier for the verus! contracts (ADR-0018). Pinned
# release + digest on linux/amd64; a clean no-op where upstream publishes no
# asset (run the host's arm64-macos Verus there instead; see agent-vm's CONTRIBUTING.md).
ARG VERUS_DIR=/opt/verus
RUN --mount=type=bind,source=install-verus.sh,target=/tmp/install-verus.sh,ro \
    bash /tmp/install-verus.sh

ENV RUSTUP_HOME=/opt/rustup
# Every cargo compile in a guest goes through the pinned cache: the wrapper is
# installed at a fixed path and the cache lives in the guest's own writable
# $HOME, so nothing is shared but the binaries. `KACHE_DISABLED=1` turns it off
# for one command, and a guest that wants the C/C++ shims or a login service runs
# `kache init`. Verus is unaffected: `cargo verus` overrides RUSTC_WRAPPER with
# its own driver, so those invocations never reach kache.
ENV RUSTC_WRAPPER=/opt/kache/bin/kache
# A project's rust-toolchain file must never make a guest command fetch a
# toolchain: with automatic installation disabled an absent channel fails
# locally. There is deliberately no RUSTUP_TOOLCHAIN and no CARGO_HOME here, so
# project selection still works and cargo keeps a per-user $HOME/.cargo cache.
ENV RUSTUP_AUTO_INSTALL=0
# PATH stays additive: the base's entries are preserved via ${PATH}, the kache
# directory is what RUSTC_WRAPPER names, and the Verus directory is harmless on a
# host that skipped it.
ENV PATH=/opt/kache/bin:/opt/cargo/bin:/opt/verus/verus-x86-linux:${PATH}

# Post-install sanity with no network at all: a present-but-broken binary, a
# missing component/target or a stale layer fails the build here instead of
# surfacing as command-not-found in the guest, and the amd64 Verus check verifies
# a real contract rather than only `--version`. The verifier bundle is mounted
# read-only for this step and nothing is copied into the image.
RUN --network=none \
    --mount=type=bind,source=verify-toolchain.sh,target=/run/rust-verify/verify-toolchain.sh,ro \
    --mount=type=bind,source=kache_probe.py,target=/run/rust-verify/kache_probe.py,ro \
    --mount=type=bind,source=rust_toolchain.py,target=/run/rust-verify/rust_toolchain.py,ro \
    --mount=type=bind,source=rust-toolchain,target=/run/rust-verify/rust-toolchain,ro \
    RUST_TOOLCHAIN=$RUST_TOOLCHAIN KACHE_VERSION=$KACHE_VERSION VERUS_DIR=/opt/verus PYTHONDONTWRITEBYTECODE=1 \
    /bin/bash /run/rust-verify/verify-toolchain.sh
