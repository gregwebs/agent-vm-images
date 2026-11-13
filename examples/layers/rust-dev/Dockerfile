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
# pinned kache compiler cache whose Rust wrapper and C/C++ shims reuse builds
# across worktrees, and the pinned Verus release CI verifies with. See
# examples/layers/README.md and agent-vm's
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
# can read them; its cache and log stay in each guest's own $HOME. The C/C++
# shims go beside them in /opt/kache/shims, and are first on PATH (below).
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
    KACHE_SHIM_DIR=$KACHE_DIR/shims \
    bash /tmp/install-kache.sh

# Verus, the SMT-backed verifier for the verus! contracts (ADR-0018). Pinned
# release + digest on linux/amd64; a clean no-op where upstream publishes no
# asset (run the host's arm64-macos Verus there instead; see agent-vm's CONTRIBUTING.md).
ARG VERUS_DIR=/opt/verus
RUN --mount=type=bind,source=install-verus.sh,target=/tmp/install-verus.sh,ro \
    bash /tmp/install-verus.sh

ENV RUSTUP_HOME=/opt/rustup
# Every cargo compile in a guest goes through the pinned cache: the wrapper is
# installed at a fixed path and the store is that guest's own, so nothing is
# shared but the binaries and the artifacts below. Every C/C++ compile goes
# through the shim farm the installer created under /opt/kache/shims, which is
# first on PATH below; a guest needs no setup step for either. `KACHE_DISABLED=1`
# turns both off for one command, and `kache init --no-service` still adds what
# the image deliberately leaves alone (test pacing, completions, a login service,
# and versioned compiler names). Verus is unaffected: `cargo verus` overrides
# RUSTC_WRAPPER with its own driver, so those invocations never reach kache.
ENV RUSTC_WRAPPER=/opt/kache/bin/kache
# A project's rust-toolchain file must never make a guest command fetch a
# toolchain: with automatic installation disabled an absent channel fails
# locally. There is deliberately no RUSTUP_TOOLCHAIN and no CARGO_HOME here, so
# project selection still works and cargo keeps a per-user $HOME/.cargo cache.
ENV RUSTUP_AUTO_INSTALL=0
# PATH stays additive: the base's entries are preserved via ${PATH}. The shim farm
# comes first because a shim only works as the first `cc`, `gcc` or `clang` the
# shell finds; kache then skips that directory (its `.kache-shims` marker) and
# runs the real compiler it found further down PATH. The kache directory is what
# RUSTC_WRAPPER names, and the Verus directory is harmless on a host that skipped
# it.
ENV PATH=/opt/kache/shims:/opt/kache/bin:/opt/cargo/bin:/opt/verus/verus-x86-linux:${PATH}

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

# --- the shared kache remote and the cache layout ---------------------------
#
# This deliberately comes after the verifier, and the ordering is a contract,
# not an accident. The verifier is specified to exercise kache with *no* remote
# configured: kache_probe.py decides a `doctor --probe` report taken against an
# isolated local cache, and configuring a remote changes what that report says —
# kache then requires its daemon, because the daemon performs remote fetches, so
# the daemon checks stop being optional and the report can no longer pass in a
# build where no daemon was ever started. Installing the policy afterwards keeps
# the verifier proving exactly what it was written to prove, and still fails the
# build on a malformed config or a wrong value (below).
#
# The policy splits the cache in two, because its halves have opposite
# requirements (kache-config.toml states them): the store is SQLite plus a WAL
# index that needs single-writer local storage, while the remote is an immutable
# content-addressed blob tree that any number of guests may share. The store
# therefore lives in /var/tmp/kache on the sandbox's own disk, *not* in
# $HOME/.cache/kache: in a sandbox $HOME is a host-backed virtio-fs mount, and a
# WAL index on it has corrupted here once. Only the remote crosses a boundary —
# through /mnt/kache-remote, which the launcher bind-mounts from the host for
# every guest (the contract is in examples/layers/README.md#kache-build-cache).
# Mounting the *store* there instead is the mistake that corrupts its index.
#
# `config get` is the check rather than `doctor`: it reads the installed file with
# the pinned binary, so an unparsable file or a wrong value fails the build, and
# it does not need a daemon or a mounted remote to answer.
RUN --mount=type=bind,source=kache-config.toml,target=/tmp/kache-config.toml,ro \
    install -D -m 0644 /tmp/kache-config.toml /opt/kache/config.toml \
 && mkdir -p /mnt/kache-remote \
 && kache_check=$(mktemp -d) \
 && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml \
        /opt/kache/bin/kache config get cache.local_store | grep -qxF /var/tmp/kache \
 && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml \
        /opt/kache/bin/kache config get cache.local_max_size | grep -qxF 4G \
 && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml \
        /opt/kache/bin/kache config get cache.gc_evict_shared | grep -qx false \
 && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml \
        /opt/kache/bin/kache config get cache.remote.type | grep -qx filesystem \
 && HOME=$kache_check TMPDIR=$kache_check KACHE_CONFIG=/opt/kache/config.toml \
        /opt/kache/bin/kache config get cache.remote.path | grep -qxF /mnt/kache-remote \
 && rm -rf "$kache_check"

# Image-owned under /opt (readable by the arbitrary-uid guest, ADR-0001) rather
# than in $HOME, because $HOME is per-guest and writable while this policy is
# neither.
ENV KACHE_CONFIG=/opt/kache/config.toml

# The store's location, next to the config that records it. kache's environment
# wins over its files, so this is the effective value even if a host layer or a
# project file disagrees, and the wrapper and the daemon it starts resolve the
# same path without consulting anything else. It is *not* under $HOME on purpose:
# see the block above and kache-config.toml. The directory is left for kache to
# create, 0700 for the guest uid that builds first — pre-creating it as root
# would make every guest unable to write it.
ENV KACHE_CACHE_DIR=/var/tmp/kache
