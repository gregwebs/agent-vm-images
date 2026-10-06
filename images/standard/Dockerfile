# One maintained all-agent recipe. Build context: images/.
# Version declarations remain local to each block for Docker cache locality.
ARG BASE_IMAGE=agent-vm-base:local
FROM ${BASE_IMAGE}
LABEL org.opencontainers.image.title="agent-vm standard coding-agent image"
LABEL org.opencontainers.image.description="dsh, Pi, Codex, OpenCode, Claude and Copilot with reviewed exact selections."

# dsh
ARG AGENT_VERSION_DSH=
ARG AGENT_VERSION_PNPM=

LABEL org.agent-vm.version.dsh="${AGENT_VERSION_DSH:-0.1.5-rc.2}" \
      org.agent-vm.version.pnpm="${AGENT_VERSION_PNPM:-11.11.0}"

COPY tools/dsh/package.json tools/dsh/package-lock.json /opt/agent-vm/dsh/
RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/dsh/install-dsh.sh,target=/tmp/install-dsh.sh,ro \
    --mount=type=bind,source=tools/dsh/prepare-lock.sh,target=/tmp/prepare-lock.sh,ro \
    --mount=type=bind,source=tools/dsh/check-lock-update.js,target=/tmp/check-lock-update.js,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_PREPARE_LOCK=/tmp/prepare-lock.sh \
    AGENT_VM_VERSION_DSH="${AGENT_VERSION_DSH}" \
    AGENT_VM_VERSION_PNPM="${AGENT_VERSION_PNPM}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh dsh /opt/agent-vm/install-status/dsh sh /tmp/install-dsh.sh

RUN ln -s /opt/agent-vm/dsh/node_modules/.bin/dsh /usr/local/bin/dsh \
 && ln -s /opt/agent-vm/dsh/node_modules/.bin/pnpm /usr/local/bin/pnpm

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/dsh/verify-dsh.sh,target=/tmp/verify-dsh.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VERSION_DSH="${AGENT_VERSION_DSH}" \
    AGENT_VM_VERSION_PNPM="${AGENT_VERSION_PNPM}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/verify-dsh.sh

# pi
ARG AGENT_VERSION_PI=
ARG AGENT_VERSION_PI_CLAUDE_BRIDGE=

LABEL org.agent-vm.version.pi="${AGENT_VERSION_PI:-0.87.1}" \
      org.agent-vm.version.pi-claude-bridge="${AGENT_VERSION_PI_CLAUDE_BRIDGE:-0.8.0}"

COPY tools/pi/package.json tools/pi/package-lock.json /opt/agent-vm/pi/
RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/pi/install-pi.sh,target=/tmp/install-pi.sh,ro \
    --mount=type=bind,source=tools/pi/prepare-lock.sh,target=/tmp/prepare-pi-lock.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_PREPARE_LOCK=/tmp/prepare-pi-lock.sh \
    AGENT_VM_VERSION_PI="${AGENT_VERSION_PI}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh pi /opt/agent-vm/install-status/pi sh /tmp/install-pi.sh

COPY tools/pi/bridge/package.json tools/pi/bridge/package-lock.json /opt/agent-vm/pi-packages/
RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/pi/install-pi-packages.sh,target=/tmp/install-pi-packages.sh,ro \
    --mount=type=bind,source=tools/pi/bridge/prepare-lock.sh,target=/tmp/prepare-bridge-lock.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_PREPARE_LOCK=/tmp/prepare-bridge-lock.sh \
    AGENT_VM_VERSION_PI_CLAUDE_BRIDGE="${AGENT_VERSION_PI_CLAUDE_BRIDGE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh pi-claude-bridge /opt/agent-vm/install-status/pi-claude-bridge sh /tmp/install-pi-packages.sh

COPY --chmod=0755 tools/pi/seed-claude-bridge-config.sh /opt/agent-vm/seed.d/20-pi-claude-bridge

COPY tools/pi/extensions/guest-credential-warning.js /opt/agent-vm/pi-extensions/
RUN chmod -R a+rX /opt/agent-vm/pi-extensions

COPY --chmod=0755 tools/pi/pi.sh /usr/local/bin/pi

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/pi/verify-pi.sh,target=/tmp/verify-pi.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VERSION_PI="${AGENT_VERSION_PI}" \
    AGENT_VM_VERSION_PI_CLAUDE_BRIDGE="${AGENT_VERSION_PI_CLAUDE_BRIDGE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/verify-pi.sh

# codex
ARG AGENT_VERSION_CODEX=rust-v0.159.3

LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"

ENV PATH=/opt/agent/.local/bin:${PATH}

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/codex/install-codex.sh,target=/tmp/install-codex.sh,ro \
    --mount=type=bind,source=tools/codex/vendor,target=/opt/agent-vm/recipe-vendor,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VENDOR_DIR=/opt/agent-vm/recipe-vendor \
    AGENT_VM_VERSION_CODEX="${AGENT_VERSION_CODEX}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh codex /opt/agent-vm/install-status/codex sh /tmp/install-codex.sh

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/codex/verify-codex.sh,target=/tmp/verify-codex.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VERSION_CODEX="${AGENT_VERSION_CODEX}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/verify-codex.sh

# opencode
ARG AGENT_VERSION_OPENCODE=v1.18.34

LABEL org.agent-vm.version.opencode="${AGENT_VERSION_OPENCODE}"

ENV PATH=/opt/agent/.opencode/bin:${PATH}

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/opencode/install-opencode.sh,target=/tmp/install-opencode.sh,ro \
    --mount=type=bind,source=tools/opencode/vendor,target=/opt/agent-vm/recipe-vendor,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VENDOR_DIR=/opt/agent-vm/recipe-vendor \
    AGENT_VM_VERSION_OPENCODE="${AGENT_VERSION_OPENCODE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh opencode /opt/agent-vm/install-status/opencode sh /tmp/install-opencode.sh

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/opencode/verify-opencode.sh,target=/tmp/verify-opencode.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VERSION_OPENCODE="${AGENT_VERSION_OPENCODE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/verify-opencode.sh

# claude
ARG AGENT_VERSION_CLAUDE=2.1.286

LABEL org.agent-vm.version.claude="${AGENT_VERSION_CLAUDE}"

ENV PATH=/opt/agent/.local/bin:/opt/agent/.claude/local/bin:${PATH}

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/claude/install-claude.sh,target=/tmp/install-claude.sh,ro \
    --mount=type=bind,source=tools/claude/vendor,target=/opt/agent-vm/recipe-vendor,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VENDOR_DIR=/opt/agent-vm/recipe-vendor \
    AGENT_VM_VERSION_CLAUDE="${AGENT_VERSION_CLAUDE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh claude /opt/agent-vm/install-status/claude sh /tmp/install-claude.sh

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/claude/verify-claude.sh,target=/tmp/verify-claude.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VERSION_CLAUDE="${AGENT_VERSION_CLAUDE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/verify-claude.sh

# Optional extras are bounded and visibly best-effort; required CLI gates above
# and final certification below remain hard even when every plugin fails.
RUN { HOME=/opt/agent timeout --kill-after=10s 180s /opt/agent/.local/bin/claude plugin marketplace add anthropics/claude-plugins-official \
        || { status=$?; echo "==> optional marketplace add skipped (status $status)" >&2; }; } \
 && for lsp in clangd-lsp pyright-lsp typescript-lsp gopls-lsp; do \
        HOME=/opt/agent timeout --kill-after=10s 120s /opt/agent/.local/bin/claude plugin install "${lsp}@claude-plugins-official" \
        || { status=$?; echo "==> optional plugin $lsp skipped (status $status)" >&2; }; \
    done \
 && echo "==> LSP plugins installed at build time:" \
 && { HOME=/opt/agent timeout --kill-after=10s 60s /opt/agent/.local/bin/claude plugin list \
        || { status=$?; echo "==> optional plugin list failed (status $status)" >&2; }; } \
 && chmod -R a+rX /opt/agent

RUN mkdir -p /opt/agent-vm/claude-seed \
 && if [ -d /opt/agent/.claude/plugins ]; then \
        cp -a /opt/agent/.claude/plugins /opt/agent-vm/claude-seed/plugins; \
        [ -f /opt/agent/.claude/settings.json ] \
            && cp -a /opt/agent/.claude/settings.json /opt/agent-vm/claude-seed/settings.json; \
        echo "==> stashed claude LSP plugins for first-boot seeding"; \
    else \
        echo "==> no /opt/agent/.claude/plugins to stash (LSP install skipped); seed is a no-op"; \
    fi \
 && chmod -R a+rX /opt/agent-vm/claude-seed

COPY --chmod=0755 tools/claude/seed-claude-plugins.sh /opt/agent-vm/seed.d/10-claude-plugins

# copilot
ARG AGENT_VERSION_COPILOT=1.0.90

LABEL org.agent-vm.version.copilot="${AGENT_VERSION_COPILOT}"

RUN --mount=type=bind,source=tools/copilot/install-copilot.sh,target=/tmp/install-copilot.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_VERSION_COPILOT="${AGENT_VERSION_COPILOT}" \
    sh /tmp/install-copilot.sh

RUN --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/copilot/verify-copilot.sh,target=/tmp/verify-copilot.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_VERSION_COPILOT="${AGENT_VERSION_COPILOT}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    sh /tmp/verify-copilot.sh

# Certification of the finished filesystem and its unmodified final PATH.
RUN --mount=type=bind,source=standard/verify-standard.sh,target=/tmp/verify-standard.sh,ro \
    --mount=type=bind,source=tools,target=/tmp/standard-tools,ro \
    --mount=type=bind,source=recipe-contract,target=/tmp/recipe-contract,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_TOOL_SOURCE_DIR=/tmp/standard-tools \
    AGENT_VM_VERSION_DSH="${AGENT_VERSION_DSH}" \
    AGENT_VM_VERSION_PNPM="${AGENT_VERSION_PNPM}" \
    AGENT_VM_VERSION_PI="${AGENT_VERSION_PI}" \
    AGENT_VM_VERSION_PI_CLAUDE_BRIDGE="${AGENT_VERSION_PI_CLAUDE_BRIDGE}" \
    AGENT_VM_VERSION_CODEX="${AGENT_VERSION_CODEX}" \
    AGENT_VM_VERSION_OPENCODE="${AGENT_VERSION_OPENCODE}" \
    AGENT_VM_VERSION_CLAUDE="${AGENT_VERSION_CLAUDE}" \
    AGENT_VM_VERSION_COPILOT="${AGENT_VERSION_COPILOT}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/verify-standard.sh
