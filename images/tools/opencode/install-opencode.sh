#!/bin/sh
# Owning install hook for the opencode layer, bind-mounted and never COPYed so it
# stays out of the shipped image.
#
# The policy is the `opencode-install` subcommand of the composed helper (ADR 0017,
# #219): the `v` slot and its major-2 gate, the vendored installer run as a child, the
# published link, and the one softening a classified transport failure may get all
# live there. This file resolves the helper and names the container's environment as
# flags.
#
# Resolving the helper is the entry point's own step, so it happens before the guards
# the driver applies. The build mounts the composed directory and names it in
# AGENT_VM_CONTRACT_DIR; the fixed mount is the default the other base entry points
# use. There is deliberately no third step: a directory the caller names is used as it
# stands, so a value that holds no helper is refused rather than replaced by a guess,
# and a host checkout names the directory its own adapter published.
set -eu

helper="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}/recipe-contract"
[ -x "$helper" ] || {
    echo "install-opencode.sh: no recipe-contract helper at $helper" >&2
    exit 1
}

# The slot is required, and an unset or empty one is the shell's own status 2 with
# the predecessor's text; the driver renames nothing.
exec "$helper" opencode-install \
    --vendor "${AGENT_VM_VENDOR_DIR:-/opt/agent-vm/recipe-vendor}" \
    --prefix "${AGENT_VM_OPENCODE_PREFIX:-/opt/agent}" \
    --link "${AGENT_VM_OPENCODE_LINK:-/usr/local/bin/opencode}" \
    --opencode-version "${AGENT_VM_VERSION_OPENCODE:?install-opencode.sh needs AGENT_VM_VERSION_OPENCODE}" \
    --receipt "${AGENT_VM_TRANSPORT_RECEIPT:-}" \
    --soft-fail "${AGENT_INSTALL_SOFT_FAIL:-}" \
    --status-dir "${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"
