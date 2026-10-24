# Derived image for the Y-series policy controls (#41), built from the example
# image so the helper's install sees the same chrome account, sudo and wrapper a
# real build sees. Removing the installed prefix resets every case to "inputs
# only", exactly like a build; the parent's own manager stays on disk under its
# unchanged basename (#40's launcher dispatches on ${0##*/}) and is put back on
# PATH by the sentinel, so the policy controls exercise the reviewed manager.
ARG EXAMPLE
FROM ${EXAMPLE}
RUN rm -rf /opt/agent-vm/chrome-devtools-mcp && mkdir -p /usr/local/lib/agent-vm-test/real /agent-vm-sentinel && mv /usr/local/bin/pnpm /usr/local/lib/agent-vm-test/real/pnpm && chmod 0777 /agent-vm-sentinel
COPY --chmod=0755 pnpm-sentinel.sh /usr/local/bin/pnpm
