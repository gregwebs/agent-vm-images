# Tool-free source building block; install coding agents with standard/Dockerfile
# or extend this base with an ordinary user-owned Dockerfile.
FROM debian:13-slim

LABEL org.opencontainers.image.title="agent-vm tool-free base"
LABEL org.opencontainers.image.description="Tool-free Debian 13 building block with Node 22, Python, Docker prerequisites and diagnostic utilities."
LABEL org.opencontainers.image.source="https://github.com/gregwebs/agent-vm-images"
LABEL org.opencontainers.image.licenses="Apache-2.0"

ENV DEBIAN_FRONTEND=noninteractive \
    HOME=/root \
    PATH=/usr/local/bin:/usr/bin:/usr/sbin:/bin \
    LANG=C.UTF-8

# Optional explicitly supplied trust material persists in the image. Secret contents
# do not invalidate cache: supply the certificate SHA-256 as CA_SHIM_CACHEBUST.
ARG CA_SHIM_CACHEBUST=
RUN --mount=type=secret,id=hostca,target=/tmp/hostca.crt,required=false \
    : "${CA_SHIM_CACHEBUST}" \
 && if [ -s /tmp/hostca.crt ]; then \
        apt-get update \
        && apt-get install -y --no-install-recommends ca-certificates \
        && install -d /usr/local/share/ca-certificates \
        && install -m 0644 /tmp/hostca.crt /usr/local/share/ca-certificates/agent-vm-build.crt \
        && update-ca-certificates \
        && rm -rf /var/lib/apt/lists/* \
        && echo "==> host CA trusted for this build"; \
    fi

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
