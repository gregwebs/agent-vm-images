# One maintained all-agent recipe. Build context: images/.
# Version declarations remain local to each block for Docker cache locality.
ARG BASE_IMAGE=agent-vm-base:local
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

FROM ${BASE_IMAGE}
LABEL org.opencontainers.image.title="agent-vm standard coding-agent image"
LABEL org.opencontainers.image.description="dsh, Pi, Codex, OpenCode, Claude and Copilot with reviewed exact selections."

# The inherited stamp proves which sources the base was built from; comparing it
# with this checkout's manifest is the first product RUN so a base from other
# sources fails before any agent installation, and the mount list matches the
# base's exactly so one manifest never describes two different input sets.
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
    sh /tmp/base-source/verify-base-inputs.sh /tmp/base-source verify /opt/agent-vm/base/source-identity

# dsh
ARG AGENT_VERSION_DSH=
# Compatibility assertion only; base owns the pnpm selection and launcher.
ARG AGENT_VERSION_PNPM=
LABEL org.agent-vm.version.dsh="${AGENT_VERSION_DSH:-0.1.5-rc.2}"

COPY tools/dsh/package.json tools/dsh/pnpm-lock.yaml tools/dsh/pnpm-workspace.yaml tools/dsh/pnpm-inputs.sha256 /opt/agent-vm/dsh/
RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/dsh,target=/tmp/dsh-source,ro \
    --mount=type=bind,source=tools/pnpm-project.py,target=/tmp/pnpm-project.py,ro \
    --mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_DSH_SOURCE_DIR=/tmp/dsh-source \
    PNPM_SOURCE_DIR=/tmp/pnpm-source \
    AGENT_VM_VERSION_DSH="${AGENT_VERSION_DSH}" \
    AGENT_VM_VERSION_PNPM="${AGENT_VERSION_PNPM}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh dsh /opt/agent-vm/install-status/dsh sh /tmp/dsh-source/install-dsh.sh

RUN test ! -e /usr/local/bin/dsh && test ! -L /usr/local/bin/dsh && \
    ln -s /opt/agent-vm/dsh/node_modules/@deepseek-ai/dsh/lib/bin.js /usr/local/bin/dsh

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/dsh,target=/tmp/dsh-source,ro \
    --mount=type=bind,source=tools/pnpm-project.py,target=/tmp/pnpm-project.py,ro \
    --mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_DSH_SOURCE_DIR=/tmp/dsh-source \
    PNPM_SOURCE_DIR=/tmp/pnpm-source \
    AGENT_VM_VERSION_DSH="${AGENT_VERSION_DSH}" \
    AGENT_VM_VERSION_PNPM="${AGENT_VERSION_PNPM}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/dsh-source/verify-dsh.sh

# pi
# Pi and the bridge are reviewed pnpm projects, so the block mirrors dsh: four
# published inputs per prefix, and every build-only helper arrives through a
# read-only mount and never through COPY. The repository shape is reproduced
# under /tmp/standard-tools so the preparation driver resolves its shared module
# as a sibling both here and in the finished gate, and the target platform is
# passed explicitly because a guest image may be built on another architecture.
#
# Both RUN lines that execute Pi code begin by removing every inherited
# AGENT_VM_PI_*: an image ENV, or a build reusing another stage's environment,
# would otherwise choose the wrapper the gate executes, switch its bridge gates
# off, or pick the extension files its registration probes load. The removal is a
# family statement rather than a list, so a seam added later is covered, and it
# precedes the assignments below -- anything assigned before it goes with the
# rest. `script/test/pi-verify.sh` executes the statement and fails if either RUN
# line drops it.
ARG TARGETARCH
ARG AGENT_VERSION_PI=
ARG AGENT_VERSION_PI_CLAUDE_BRIDGE=

LABEL org.agent-vm.version.pi="${AGENT_VERSION_PI:-1.4.0}" \
      org.agent-vm.version.pi-claude-bridge="${AGENT_VERSION_PI_CLAUDE_BRIDGE:-0.8.0}"

COPY tools/pi/package.json tools/pi/pnpm-lock.yaml tools/pi/pnpm-workspace.yaml tools/pi/pnpm-inputs.sha256 /opt/agent-vm/pi/
RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/pi,target=/tmp/standard-tools/pi,ro \
    --mount=type=bind,source=tools/pnpm-project.py,target=/tmp/standard-tools/pnpm-project.py,ro \
    --mount=type=bind,source=tools/pnpm-host-manager.py,target=/tmp/standard-tools/pnpm-host-manager.py,ro \
    --mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro \
    unset $(env | sed -n 's/^\(AGENT_VM_PI_[0-9A-Za-z_]*\)=.*/\1/p') && \
    mkdir -p /tmp/pi-verify && \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_PI_SOURCE_DIR=/tmp/standard-tools/pi \
    AGENT_VM_PI_SHARED=/tmp/standard-tools/pnpm-project.py \
    AGENT_VM_PI_BYTES_CHECK=/tmp/standard-tools/pi/check-installed-bytes.mjs \
    AGENT_VM_PI_RUNTIME_CHECK=/tmp/standard-tools/pi/check-runtime.mjs \
    AGENT_VM_PI_WORK_DIR=/tmp/pi-verify \
    AGENT_VM_PI_TARGET_PLATFORM=linux/${TARGETARCH} \
    AGENT_VM_VERSION_PI="${AGENT_VERSION_PI}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    PNPM_SOURCE_DIR=/tmp/pnpm-source \
    sh /tmp/recipe-contract/run-install.sh pi /opt/agent-vm/install-status/pi sh /tmp/standard-tools/pi/install-pi.sh; \
    status=$?; rmdir /tmp/pi-verify || { echo "pi: invocation scratch parent was not cleaned" >&2; status=1; }; exit $status

COPY tools/pi/bridge/package.json tools/pi/bridge/pnpm-lock.yaml tools/pi/bridge/pnpm-workspace.yaml tools/pi/bridge/pnpm-inputs.sha256 /opt/agent-vm/pi-packages/
RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/pi,target=/tmp/standard-tools/pi,ro \
    --mount=type=bind,source=tools/pnpm-project.py,target=/tmp/standard-tools/pnpm-project.py,ro \
    --mount=type=bind,source=tools/pnpm-host-manager.py,target=/tmp/standard-tools/pnpm-host-manager.py,ro \
    --mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_PI_SOURCE_DIR=/tmp/standard-tools/pi \
    AGENT_VM_PI_SHARED=/tmp/standard-tools/pnpm-project.py \
    AGENT_VM_VERSION_PI_CLAUDE_BRIDGE="${AGENT_VERSION_PI_CLAUDE_BRIDGE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    PNPM_SOURCE_DIR=/tmp/pnpm-source \
    sh /tmp/recipe-contract/run-install.sh pi-claude-bridge /opt/agent-vm/install-status/pi-claude-bridge sh /tmp/standard-tools/pi/install-pi-packages.sh

COPY --chmod=0755 tools/pi/seed-claude-bridge-config.sh /opt/agent-vm/seed.d/20-pi-claude-bridge

COPY tools/pi/extensions/guest-credential-warning.js tools/pi/extensions/agent-vm-codex.js /opt/agent-vm/pi-extensions/
RUN chmod -R a+rX /opt/agent-vm/pi-extensions

COPY --chmod=0755 tools/pi/pi.sh /usr/local/bin/pi

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/pi,target=/tmp/standard-tools/pi,ro \
    --mount=type=bind,source=tools/pnpm-project.py,target=/tmp/standard-tools/pnpm-project.py,ro \
    --mount=type=bind,source=tools/pnpm-host-manager.py,target=/tmp/standard-tools/pnpm-host-manager.py,ro \
    --mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro \
    unset $(env | sed -n 's/^\(AGENT_VM_PI_[0-9A-Za-z_]*\)=.*/\1/p') && \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_PI_SOURCE_DIR=/tmp/standard-tools/pi \
    AGENT_VM_PI_SHARED=/tmp/standard-tools/pnpm-project.py \
    AGENT_VM_VERSION_PI="${AGENT_VERSION_PI}" \
    AGENT_VM_VERSION_PI_CLAUDE_BRIDGE="${AGENT_VERSION_PI_CLAUDE_BRIDGE}" \
    AGENT_VM_PI_TARGET_PLATFORM=linux/${TARGETARCH} \
    AGENT_VM_PI_RUNTIME_CHECK=/tmp/standard-tools/pi/check-runtime.mjs \
    AGENT_VM_PI_ENTRY=/opt/agent-vm/pi/node_modules/.bin/pi \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    PNPM_SOURCE_DIR=/tmp/pnpm-source \
    sh /tmp/standard-tools/pi/verify-pi.sh

# codex
ARG AGENT_VERSION_CODEX=rust-v0.159.3

LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"

ENV PATH=/opt/agent/.local/bin:${PATH}

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/codex,target=/tmp/codex-source,ro \
    --mount=type=bind,source=tools/codex/install-codex.sh,target=/tmp/install-codex.sh,ro \
    --mount=type=bind,source=tools/codex/vendor,target=/opt/agent-vm/recipe-vendor,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_CODEX_SOURCE_DIR=/tmp/codex-source \
    AGENT_VM_VENDOR_DIR=/opt/agent-vm/recipe-vendor \
    AGENT_VM_VERSION_CODEX="${AGENT_VERSION_CODEX}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh codex /opt/agent-vm/install-status/codex sh /tmp/install-codex.sh

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/codex/verify-codex.sh,target=/tmp/verify-codex.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VERSION_CODEX="${AGENT_VERSION_CODEX}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/verify-codex.sh

# opencode
ARG AGENT_VERSION_OPENCODE=v2.0.29

LABEL org.agent-vm.version.opencode="${AGENT_VERSION_OPENCODE}"

ENV PATH=/opt/agent/.opencode/bin:${PATH}

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/opencode/install-opencode.sh,target=/tmp/install-opencode.sh,ro \
    --mount=type=bind,source=tools/opencode/vendor,target=/opt/agent-vm/recipe-vendor,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VENDOR_DIR=/opt/agent-vm/recipe-vendor \
    AGENT_VM_VERSION_OPENCODE="${AGENT_VERSION_OPENCODE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh opencode /opt/agent-vm/install-status/opencode sh /tmp/install-opencode.sh

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
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

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/claude/install-claude.sh,target=/tmp/install-claude.sh,ro \
    --mount=type=bind,source=tools/claude/vendor,target=/opt/agent-vm/recipe-vendor,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_VENDOR_DIR=/opt/agent-vm/recipe-vendor \
    AGENT_VM_VERSION_CLAUDE="${AGENT_VERSION_CLAUDE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    sh /tmp/recipe-contract/run-install.sh claude /opt/agent-vm/install-status/claude sh /tmp/install-claude.sh

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
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

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/copilot/verify-copilot.sh,target=/tmp/verify-copilot.sh,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_VERSION_COPILOT="${AGENT_VERSION_COPILOT}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    sh /tmp/verify-copilot.sh

# Locked OpenCode Claude integration; runtime owns no package-manager state.
ARG AGENT_VERSION_OPENCODE_CLAUDE=
LABEL org.agent-vm.version.opencode-claude="${AGENT_VERSION_OPENCODE_CLAUDE:-1.4.1}"
COPY tools/opencode/plugins/package.json /opt/agent-vm/opencode-plugins/package.json
COPY tools/opencode/plugins/pnpm-lock.yaml /opt/agent-vm/opencode-plugins/pnpm-lock.yaml
COPY tools/opencode/plugins/pnpm-workspace.yaml /opt/agent-vm/opencode-plugins/pnpm-workspace.yaml
COPY tools/opencode/plugins/pnpm-inputs.sha256 /opt/agent-vm/opencode-plugins/pnpm-inputs.sha256
COPY tools/opencode/plugins/index.js /opt/agent-vm/opencode-plugins/index.js
COPY tools/opencode/opencode.json /opt/agent-vm/opencode/opencode.json
ENV OPENCODE_CONFIG=/opt/agent-vm/opencode/opencode.json

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/opencode,target=/tmp/opencode-source,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_OPENCODE_SOURCE_DIR=/tmp/opencode-source \
    AGENT_VM_VERSION_OPENCODE_CLAUDE="${AGENT_VERSION_OPENCODE_CLAUDE}" \
    sh /tmp/recipe-contract/run-install.sh opencode-claude /opt/agent-vm/install-status/opencode-claude sh /tmp/opencode-source/install-opencode-plugins.sh

RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
    --mount=type=bind,source=tools/opencode,target=/tmp/opencode-source,ro \
    AGENT_INSTALL_SOFT_FAIL= \
    AGENT_VM_CONTRACT_DIR=/tmp/recipe-contract \
    AGENT_VM_OPENCODE_SOURCE_DIR=/tmp/opencode-source \
    AGENT_VM_VERSION_OPENCODE_CLAUDE="${AGENT_VERSION_OPENCODE_CLAUDE}" \
    sh /tmp/opencode-source/verify-opencode-plugins.sh

# Certification of the finished filesystem and its unmodified final PATH.
# The PATH-identity helper is a sibling of the script under /tmp: bind only that
# file, never the directory, so nothing is COPYed into the runtime image.
RUN --mount=type=bind,source=standard/verify-standard.sh,target=/tmp/verify-standard.sh,ro \
    --mount=type=bind,source=standard/verify-standard-path-identity.py,target=/tmp/verify-standard-path-identity.py,ro \
    --mount=type=bind,source=tools,target=/tmp/standard-tools,ro \
    --mount=type=bind,source=pnpm,target=/tmp/pnpm-source,ro \
    --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \
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
    AGENT_VM_VERSION_OPENCODE_CLAUDE="${AGENT_VERSION_OPENCODE_CLAUDE}" \
    AGENT_VM_INSTALL_STATUS_DIR=/opt/agent-vm/install-status \
    AGENT_VM_PI_TARGET_PLATFORM=linux/${TARGETARCH} \
    sh /tmp/verify-standard.sh
