# Build-only helper compiler. Docker stages do not cross Dockerfiles, so both
# products declare this stage; the substantive recipe is recipe-contract/build.sh
# and the version/digest pair is guarded by script/test/image-sources. The binary
# reaches consumers only through a read-only composed-directory mount, so no
# compiler or helper persists in a final layer.
FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS recipe-contract-bin
ARG TARGETARCH
WORKDIR /src/recipe-contract
COPY recipe-contract/ ./
# The pinned image compiler must equal go.mod's toolchain before any cross-target
# variable is set, so auto-download inside the build cannot mask a stale image.
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOTOOLCHAIN=local GOWORK=off GOFLAGS= GOENV=off \
    go test -tags recipe_contract_image_compiler -run '^TestImageCompiler$' ./internal/buildcheck
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=linux GOARCH=$TARGETARCH CGO_ENABLED=0 bash ./build.sh /out

# Coding-agent-free source building block; install agents with standard/Dockerfile
# or extend this base with an ordinary user-owned Dockerfile.
FROM debian:13-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad

LABEL org.opencontainers.image.title="agent-vm coding-agent-free base"
LABEL org.opencontainers.image.description="Debian 13 building block with Node 22, pnpm, Python, Docker prerequisites and diagnostic utilities."
LABEL org.agent-vm.version.pnpm="11.11.0"
# The two pinned third-party artifacts (#96). The gate cross-checks each value
# against images/artifacts/<name>.json, so a pin refresh that misses the label
# fails instead of shipping an image that misreports what it contains.
LABEL org.agent-vm.version.node="22.23.4"
LABEL org.agent-vm.version.gh="2.103.2"
LABEL org.opencontainers.image.source="https://github.com/gregwebs/agent-vm-images"
LABEL org.opencontainers.image.licenses="Apache-2.0"

# C.UTF-8 is present without locale generation and preserves non-ASCII paths.
ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/root \
    PATH=/usr/local/bin:/usr/bin:/usr/sbin:/bin \
    LANG=C.UTF-8

# Verify the parent's shipped apt baseline before anything is installed. No
# instruction below adds an apt source or key any more (#96), so the check
# immediately before the identity stamp re-runs this and the built image's
# final source set is the same baseline.
RUN --mount=type=bind,source=apt-policy,target=/tmp/apt-policy,ro \
    bash /tmp/apt-policy/check-sources.sh --policy /tmp/apt-policy

# An explicitly supplied host CA persists as trust material, so the caller must
# name its SHA-256: the hash is the cache key and the reviewed identity, while
# secret bytes alone are not. The CA_SHIM_CACHEBUST declaration is a tombstone
# that rejects the removed interface; Docker silently ignores an undeclared
# build argument, so no legacy value is accepted. The label records the reviewed
# hash and the public release refuses a nonempty one.
ARG BUILD_CA_SHA256=
ARG CA_SHIM_CACHEBUST=__removed_use_BUILD_CA_SHA256__
LABEL org.agent-vm.build-ca.sha256="${BUILD_CA_SHA256}"
RUN --mount=type=secret,id=hostca,target=/tmp/hostca.crt,required=false \
    --mount=type=bind,source=install-build-ca.sh,target=/tmp/install-build-ca.sh,ro \
    BUILD_CA_SHA256="${BUILD_CA_SHA256}" \
    REMOVED_CA_SHIM_CACHEBUST="${CA_SHIM_CACHEBUST}" \
    bash /tmp/install-build-ca.sh /tmp/hostca.crt

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl wget \
        git \
        jq \
        bash \
        python3 python3-pip \
        ripgrep fd-find \
        gnupg \
        iputils-ping iputils-tracepath traceroute mtr-tiny \
        net-tools iproute2 \
        openssh-client sshpass \
        dnsutils netcat-openbsd tcpdump \
        less vim-tiny file unzip zip xz-utils rsync \
        htop lsof strace tmux \
 && rm -rf /var/lib/apt/lists/*

# Node and the GitHub CLI come from committed release archives, not from a live
# setup script or a third-party apt repository: images/artifacts/<name>.json pins
# the exact per-architecture URL and SHA-256, and images/install-artifact.sh
# refuses a changed byte, a changed signed manifest, a hostile archive member and
# an attestation bundle the installed verifier cannot tie to the archive. There
# is no apt, npm, mirror or `latest` fallback, so a build without network fails
# instead of resolving something unreviewed. TARGETARCH is the platform token
# every pin is keyed by; xz-utils above is what unpacks Node's `.tar.xz`.
ARG TARGETARCH
RUN --mount=type=bind,source=install-artifact.sh,target=/tmp/artifact-source/install-artifact.sh,ro \
    --mount=type=bind,source=artifacts,target=/tmp/artifact-source/artifacts,ro \
    ARTIFACT_SOURCE_DIR=/tmp/artifact-source \
    bash /tmp/artifact-source/install-artifact.sh node \
        /tmp/artifact-source/artifacts/node.json "$TARGETARCH" \
 && bash /tmp/artifact-source/install-artifact.sh gh \
        /tmp/artifact-source/artifacts/gh.json "$TARGETARCH"

# pnpm's reviewed pin owns the inherited selection; npm/npx remain Node's commands.
RUN --mount=type=bind,source=install-pnpm.sh,target=/tmp/install-pnpm.sh,ro \
    --mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro \
    --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    AGENT_INSTALL_SOFT_FAIL= bash /tmp/install-pnpm.sh \
 && sh /tmp/pnpm-source/verify-pnpm.sh

# fuse-overlayfs accommodates downstream overlay-root guests. Docker is not
# auto-started: runtime callers choose whether they need a daemon.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        docker.io docker-cli \
        containerd runc \
        fuse-overlayfs \
        iptables \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /etc/docker \
 && printf '%s\n' '{' '  "storage-driver": "fuse-overlayfs"' '}' \
        > /etc/docker/daemon.json

# Reviewed archive digests, not checksums fetched from the download origin.
ARG ZELLIJ_VERSION=0.45.0
ARG ZELLIJ_SHA256_AMD64=ab8b2494d80c20c07da4361041a25b96b93c73df992d2d54143e70fb9b1a1063
ARG ZELLIJ_SHA256_ARM64=d8b311d0f9ec7a03f3341563331aac12c9f52990ecc96a36ed44b1add9dfa035
RUN --mount=type=bind,source=install-zellij.sh,target=/tmp/install-zellij.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= bash /tmp/install-zellij.sh

# Shared installation prefix outside HOME, traversable by arbitrary numeric UIDs.
RUN mkdir -p /opt/agent && chmod 755 /opt/agent

RUN mkdir -p /opt/agent-vm/seed.d && chmod -R a+rX /opt/agent-vm

RUN set -e \
 && echo "== HARD requirements ==" \
 && dockerd --version >/dev/null \
 && docker --version >/dev/null \
 && containerd --version >/dev/null \
 && runc --version >/dev/null \
 && ssh -V >/dev/null 2>&1 \
 && sshpass -V >/dev/null 2>&1 \
 && ping -V >/dev/null 2>&1 \
 && bash --version >/dev/null \
 && node --version >/dev/null \
 && gh --version >/dev/null \
 && python3 --version >/dev/null \
 && zellij --version >/dev/null \
 && echo "== sanity OK =="

# The final apt source set, checked again after every package step: with no
# third-party repository left (#96) this must still be the committed baseline.
# It is a separate RUN, not part of the stamp below, so a failure names the apt
# source set rather than the source identity.
RUN --mount=type=bind,source=apt-policy,target=/tmp/apt-policy,ro \
    bash /tmp/apt-policy/check-sources.sh --policy /tmp/apt-policy

# Nothing above proves which sources this base consumed: another checkout with
# the same pinned parents installs the same packages and still looks like this
# image. Re-hashing the committed manifest stamps the source identity that
# standard and a copied extension compare against, and it runs last so it covers
# everything the recipe produced. The mounts are read-only bind mounts, not
# COPYs, so no source bytes persist in a layer.
RUN --mount=type=bind,source=base-inputs.sha256,target=/tmp/base-source/base-inputs.sha256,ro \
    --mount=type=bind,source=.dockerignore,target=/tmp/base-source/.dockerignore,ro \
    --mount=type=bind,source=Dockerfile,target=/tmp/base-source/Dockerfile,ro \
    --mount=type=bind,source=apt-policy,target=/tmp/base-source/apt-policy,ro \
    --mount=type=bind,source=artifacts,target=/tmp/base-source/artifacts,ro \
    --mount=type=bind,source=install-artifact.sh,target=/tmp/base-source/install-artifact.sh,ro \
    --mount=type=bind,source=install-build-ca.sh,target=/tmp/base-source/install-build-ca.sh,ro \
    --mount=type=bind,source=install-pnpm.sh,target=/tmp/base-source/install-pnpm.sh,ro \
    --mount=type=bind,source=install-zellij.sh,target=/tmp/base-source/install-zellij.sh,ro \
    --mount=type=bind,source=pnpm,target=/tmp/base-source/pnpm,ro \
    --mount=type=bind,source=recipe-contract,target=/tmp/base-source/recipe-contract,ro \
    --mount=type=bind,source=verify-base-inputs.sh,target=/tmp/base-source/verify-base-inputs.sh,ro \
    sh /tmp/base-source/verify-base-inputs.sh /tmp/base-source stamp /opt/agent-vm/base/source-identity

CMD ["/bin/bash"]
