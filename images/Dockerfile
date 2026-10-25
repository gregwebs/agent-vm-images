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
LABEL org.opencontainers.image.source="https://github.com/gregwebs/agent-vm-images"
LABEL org.opencontainers.image.licenses="Apache-2.0"

# C.UTF-8 is present without locale generation and preserves non-ASCII paths.
ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/root \
    PATH=/usr/local/bin:/usr/bin:/usr/sbin:/bin \
    LANG=C.UTF-8

# Verify the parent Debian baseline, not the final source set (#96 remains open).
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
        less vim-tiny file unzip zip rsync \
        htop lsof strace tmux \
 && rm -rf /var/lib/apt/lists/*

RUN install -dm 755 /etc/apt/keyrings \
 && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        > /etc/apt/keyrings/githubcli-archive-keyring.gpg \
 && chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

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
 && python3 --version >/dev/null \
 && zellij --version >/dev/null \
 && echo "== sanity OK =="

CMD ["/bin/bash"]
