# examples/layers/go-dev — Go development toolchain custom boot image.
#
# Build it, import it, and select it to build, test, lint and navigate a Go
# code base inside the microVM (there is no launcher-side layering, #259):
#
#   agent-vm image build --builder NAME \
#     --build-arg BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de \
#     --tag agent-vm-go-dev:dev examples/layers/go-dev
#   agent-vm claude --image agent-vm-go-dev:dev
#
# NAME is any host daemon-backed buildx builder that supports multi-stage
# Dockerfiles, `RUN --network=none` and an OCI export: the first stage compiles the
# layer's own helper module. This repository selects no builder, so the name is the
# caller's.
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
# Two further paths are build-only inputs, mounted read-only for the steps that
# need them and never copied into the image: `gopls_closure.py` (until #251 the
# program that installs the gopls closure) and `data/gopls-closure/` (the reviewed
# module selection, its sums, its licences and the record that binds them). See
# docs/adr/0008-go-example-gopls-closure.md. The layer's own `go-dev-helper` reads
# the same five files offline: `check` proves them (#249) and `verify-installed`
# proves the installed binary against them (#250). Python is the installer and the
# live oracle; it is no longer on the verification path.
#
# The `ARG BASE_IMAGE`/`FROM ${BASE_IMAGE}` pair lets `--build-arg BASE_IMAGE=…`
# retarget the build, and every `ENV` keeps `PATH` additive
# (`PATH=<new>:${PATH}`) so the base's directories are never dropped.

ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de

# go-dev-helper: this layer's own program (ADR 0017, "Example layers stay
# self-contained"). An example build has no `--build-context`, so the module under
# helper/ is compiled by this stage and mounted read-only into the one RUN that
# needs it; nothing is copied into the image. The stage is the first FROM and the
# image's own is the last, which `script/test/example-layers.sh` holds, and
# `images/parent-pins.json` registers both stages as parents.
#
# The binding to the repository's Go toolchain is a chain, one link per gate:
#
#   1. here: the compiler in the image must be the `toolchain` line of
#      helper/go.mod, so a builder whose patch release drifted cannot compile;
#   2. script/test/example-layers.sh: helper/go.mod's `go` and `toolchain` lines
#      must equal the root go.mod's, so the two modules cannot disagree;
#   3. script/test/example-layers.sh: the FROM below must be exactly
#      `golang:<root toolchain>-trixie` at the digest images/parent-pins.json
#      records for `.pins["golang-builder"]`, so the image is the reviewed bytes;
#   4. script/test/image-sources: the FROM must also be the reference the parent
#      inventory binds to this stage.
#
# The chain binds a compiler version and this recipe/inventory relationship. It
# does not bind source bytes, and neither does anything else here: a comment-only
# edit under helper/ changes nothing any gate sees.
#
# The host check in `script/test/example-layers.sh` (the same module, built with the
# same flags, over the same data the image mounts) and the parity suite
# (`script/test/gopls-closure-parity.py`, this reader against the Python oracle over
# the committed bytes and a mutant corpus) bind the reader's *behaviour*, not its
# source: they fail when the rules or their verdicts change, and they stay green for
# an edit that changes neither. Binding source bytes for a product image is #233's
# question, and it is a different one.
#
# This stage is a closed instruction set: exactly the FROM below and the RUN below,
# and `script/test/example-layers.sh` refuses every other instruction class. That
# allowlist, not the wording of the RUN, is what the guarantees rest on: `SHELL`
# can rewrite the command while the RUN stays byte-identical, `ADD` can fetch over
# the network in a stage whose RUN has `--network=none`, and `ENV`/`ARG` can reach
# the compiler through the environment.
#
# The committed vendor tree is the build's dependency closure. The `-mod=vendor` in
# the RUN is policy, not the enforcement: Go selects vendor mode by default when
# `vendor/modules.txt` is present, so removing the flag still builds. What the
# bytes are worth is a different gate — `script/test/example-helper-vendor.sh
# examples/layers/go-dev/helper` regenerates the tree and diffs it, and that gate
# runs in the default contracts mode only.
#
# There is no `--platform=$BUILDPLATFORM`: the stage compiles for the target
# platform natively, so the build does not depend on the builder resolving
# BUILDPLATFORM.
FROM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS go-dev-helper
RUN --network=none --mount=type=bind,source=helper,target=/src,ro \
    cd /src \
 && { [ "$(GOTOOLCHAIN=local go env GOVERSION)" = "$(awk '$1 == "toolchain" { print $2 }' go.mod)" ] \
      || { echo "go-dev-helper: the image compiler is not go.mod's toolchain" >&2; exit 1; }; } \
 && GOTOOLCHAIN=local GOWORK=off GOENV=off GOFLAGS=-mod=vendor GOPROXY=off CGO_ENABLED=0 \
    go build -trimpath -buildvcs=false -o /out/go-dev-helper .

FROM ${BASE_IMAGE}

# Runtime network requests (ADR 0014): the GOPROXY/GOSUMDB defaults.
# GOTOOLCHAIN=local below means no toolchain download; GOPROXY's `direct`
# fallback reaches project-chosen hosts and is the user's decision.
LABEL org.agent-vm.network-requests.image.go-dev="proxy.golang.org,sum.golang.org"

# A C toolchain, for the two things the Go distribution itself defers to the
# host: cgo (a project that imports "C", or a dependency that does) and
# `go test -race`, which the Go command refuses to run without cgo. With no
# gcc present Go silently defaults CGO_ENABLED=0, and the failure only
# surfaces deep into a test run as "-race requires cgo; enable cgo by setting
# CGO_ENABLED=1". rust-dev installs the same package for its native build
# dependencies.
#
# python3 runs the reviewed gopls closure helper below. It comes from the
# official Debian apt repository like the rest of this layer's distro packages
# (the only inputs allowed to float), because the released parent is not
# assumed to carry it; rust-dev installs it the same way.
RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential python3 \
 && rm -rf /var/lib/apt/lists/*

# Pins. The lint version mirrors the repository root golangci-lint.version
# (guarded by image-sources); the standalone example context cannot read that
# repository file during a build. The pin and its verification stay together:
# install-go.sh and install-golangci-lint.sh each check their prebuilt tarball
# against the digest declared here, so a bumped version left beside a stale
# digest fails the build instead of shipping a different tool.
#
# gopls has no tarball and no live `go install`: it is compiled from the
# reviewed module closure committed under gopls-closure/, which records the
# selected modules, both checksum kinds for each and the record that binds them
# (gopls-closure/pin.json). GOPLS_VERSION stays here as the exact assertion the
# closure must match, written only by script/dev/upgrade-gopls.sh -- a version
# bump without a regenerated closure fails the build. The build accepts only
# committed sums; it never resolves a newer version or refreshes a checksum.
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

# gopls is built from the reviewed closure by gopls_closure.py in an environment
# it constructs from scratch (no inherited GOFLAGS/GOPROXY/GONOSUMDB/GOENV), with
# GOTOOLCHAIN=local, CGO_ENABLED=0 and GOOS/GOARCH forced to this image's Linux
# target. The helper proves the result from the binary's own build info and only
# then publishes gopls and the third-party licence file. The three inputs are
# mounted read-only for this step and are not part of the image.
RUN --mount=type=bind,source=install-gopls.sh,target=/tmp/install-gopls.sh,ro \
    --mount=type=bind,source=gopls_closure.py,target=/tmp/gopls_closure.py,ro \
    --mount=type=bind,source=data/gopls-closure,target=/tmp/gopls-closure,ro \
    bash /tmp/install-gopls.sh

# The same five committed files, read offline by the layer's own Go reader. This is
# the one `check`: no Go command runs, nothing is downloaded, and the two asserted
# versions are the recipe's ARGs, so a bumped pin without a regenerated closure
# fails the build here as well as in the Python install above. The binary is
# mounted from the builder stage and the data from the build context; neither is
# copied into the image. `script/test/example-layers.sh` holds this line's exact
# shape, and `script/test/gopls-closure-parity.py` holds its verdicts against the
# Python reader.
RUN --network=none \
    --mount=type=bind,from=go-dev-helper,source=/out/go-dev-helper,target=/tmp/go-dev-helper,ro \
    --mount=type=bind,source=data/gopls-closure,target=/tmp/gopls-closure,ro \
    /tmp/go-dev-helper check /tmp/gopls-closure --expect-gopls "$GOPLS_VERSION" --expect-go "$GO_VERSION"

# PATH stays additive: the base's entries are preserved via
# ${PATH}. GOTOOLCHAIN=local makes the pin the toolchain that actually
# runs: by default `go` would silently download a *different* toolchain
# through the module proxy whenever a go.mod asks for a newer one, which both
# defeats the pin and fails confusingly under the guest's network allow list.
# The error for an out-of-range go.mod names both versions, so the fix (bump
# GO_VERSION here and rebuild the image) is discoverable. See the README.
# No other GO* policy is exported: a project's own `go get`/module download is
# a user action outside the build guarantee, so the guest keeps Go's defaults
# for everything except the toolchain switch.
ENV PATH=${GO_PREFIX}/bin:${GO_TOOLS_BIN}:${PATH}
ENV GOTOOLCHAIN=local

# Post-install sanity with no network at all: every tool is invoked directly,
# and the Go, lint and language-server checks analyse a real module built from
# scratch, so a present-but-broken binary fails the build here instead of
# surfacing as command-not-found in the guest. The first thing it does is check
# the *installed* gopls against the committed closure (build info, module/sum
# identity, licence bytes) with the proxy off, through the layer's own
# `go-dev-helper verify-installed`: the candidate is an argument to
# `go version -m -json`, never executed. The guest PATH smoke that follows is
# separate and stays in shell — it asks whether each tool resolves and analyses
# code as the guest's PATH finds it, which is not what an absolute-path
# verification answers. The verifier bundle is mounted read-only for this step and
# nothing is copied into the image.
RUN --network=none \
    --mount=type=bind,source=verify-toolchain.sh,target=/run/go-verify/verify-toolchain.sh,ro \
    --mount=type=bind,from=go-dev-helper,source=/out/go-dev-helper,target=/run/go-verify/go-dev-helper,ro \
    --mount=type=bind,source=data/gopls-closure,target=/run/go-verify/gopls-closure,ro \
    /bin/bash /run/go-verify/verify-toolchain.sh
