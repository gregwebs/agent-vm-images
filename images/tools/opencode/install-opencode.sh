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
# the driver applies. A directory the caller names is used as it stands: the build's
# mount is what the recipe passes, and a value that holds no helper is refused rather
# than quietly replaced by this checkout's build. With nothing named, the fixed build
# mount is the other base entry points' default, and a host checkout has no mount at
# all -- so the composed directory its own adapter publishes is named last. Nothing is
# built here: a checkout without that build refuses.
set -eu

self_dir="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)"
contract="${AGENT_VM_CONTRACT_DIR:-}"
if [ -z "$contract" ]; then
    contract=/tmp/recipe-contract
    if [ ! -x "$contract/recipe-contract" ]; then
        case "$(uname -m)" in
            x86_64 | amd64) arch=amd64 ;;
            arm64 | aarch64) arch=arm64 ;;
            *) arch=unsupported ;;
        esac
        os=$(uname -s | tr '[:upper:]' '[:lower:]')
        contract="$self_dir/../../../target/recipe-contract/$os-$arch/out"
    fi
fi
helper="$contract/recipe-contract"
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
