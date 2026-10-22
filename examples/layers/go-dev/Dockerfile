# examples/layers/go-dev — Go development toolchain custom boot image.
#
# Build it, import it, and select it to build, test, lint and navigate a Go
# code base inside the microVM (there is no launcher-side layering, #259):
#
#   agent-vm build --builder native-oci \
#     --build-arg BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de \
#     --tag agent-vm-go-dev:dev examples/layers/go-dev
#   agent-vm claude --image agent-vm-go-dev:dev
#
# It provides the pinned Go toolchain (go, gofmt, go vet, ...) plus the two
# tools a Go project's edit/CI loop expects beyond it: golangci-lint and the
# gopls language server. See examples/layers/README.md and agent-vm's
# docs/adr/0035-consume-user-owned-boot-images.md.
#
# The four steps that need real shell live as standalone, independently
# runnable scripts beside this file and are bind-mounted in at build time:
# install-go.sh, install-golangci-lint.sh, install-gopls.sh and
# verify-toolchain.sh. This file only wires the pins and locations into them.
#
# The `ARG BASE_IMAGE`/`FROM ${BASE_IMAGE}` pair lets `--build-arg BASE_IMAGE=…`
# retarget the build, and every `ENV` keeps `PATH` additive
# (`PATH=<new>:${PATH}`) so the base's directories are never dropped.

ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de
FROM ${BASE_IMAGE}

# A C toolchain, for the two things the Go distribution itself defers to the
# host: cgo (a project that imports "C", or a dependency that does) and
# `go test -race`, which the Go command refuses to run without cgo. With no
# gcc present Go silently defaults CGO_ENABLED=0, and the failure only
# surfaces deep into a test run as "-race requires cgo; enable cgo by setting
# CGO_ENABLED=1". rust-dev installs the same package for its native build
# dependencies.
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential \
 && rm -rf /var/lib/apt/lists/*

# Pins. The lint version mirrors images/recipe-contract/golangci-lint.version
# (guarded by image-sources); the standalone example context cannot read that
# repository file during a build. The pin and its verification stay together: install-go.sh and
# install-golangci-lint.sh each check their prebuilt tarball against the digest
# declared here, so a bumped version left beside a stale digest fails the build
# instead of shipping a different tool. gopls has no tarball to digest;
# install-gopls.sh pins its module version and the go command verifies it
# against the checksum database. Bump a version and its digests together.
ARG GO_VERSION=1.27.3
ARG GO_AMD64_SHA256=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_ARM64_SHA256=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
ARG GOLANGCI_LINT_VERSION=2.14.0
ARG GOLANGCI_LINT_AMD64_SHA256=ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab
ARG GOLANGCI_LINT_ARM64_SHA256=ee7ec5f3453d15ddf106fae5a4d6c71737712348a979d1fe9cd52ec7ea299bae
ARG GOPLS_VERSION=v0.23.0

# Where the toolchain and the two extra tools live. Both are under /opt so the
# arbitrary-uid guest (ADR-0001) can read them. GOROOT is deliberately *not*
# exported: /opt/go/bin/go locates its own root, and leaving the variable
# unset keeps a project-level `toolchain` directive from colliding with it.
ARG GO_PREFIX=/opt/go
ARG GO_TOOLS_BIN=/opt/go-tools/bin

RUN --mount=type=bind,source=install-go.sh,target=/tmp/install-go.sh,ro \
    bash /tmp/install-go.sh

RUN --mount=type=bind,source=install-golangci-lint.sh,target=/tmp/install-golangci-lint.sh,ro \
    bash /tmp/install-golangci-lint.sh

RUN --mount=type=bind,source=install-gopls.sh,target=/tmp/install-gopls.sh,ro \
    bash /tmp/install-gopls.sh

# PATH stays additive: the base's entries are preserved via
# ${PATH}. GOTOOLCHAIN=local makes the pin the toolchain that actually
# runs: by default `go` would silently download a *different* toolchain
# through the module proxy whenever a go.mod asks for a newer one, which both
# defeats the pin and fails confusingly under the guest's network allow list.
# The error for an out-of-range go.mod names both versions, so the fix (bump
# GO_VERSION here and rebuild the image) is discoverable. See the README.
ENV PATH=${GO_PREFIX}/bin:${GO_TOOLS_BIN}:${PATH}
ENV GOTOOLCHAIN=local

# Post-install sanity: every tool is invoked directly, and the Go, lint and
# language-server checks analyse a real module built from scratch, so a
# present-but-broken binary fails the build here instead of surfacing as
# command-not-found in the guest.
RUN --mount=type=bind,source=verify-toolchain.sh,target=/tmp/verify-toolchain.sh,ro \
    bash /tmp/verify-toolchain.sh
