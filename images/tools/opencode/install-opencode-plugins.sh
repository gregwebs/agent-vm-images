#!/bin/sh
# Owning install hook for the reviewed OpenCode plugin inputs. This pnpm consumer is
# hard-failure-only: certification owns installed.
#
# The policy is the `opencode-install-plugins` subcommand of the composed helper (ADR
# 0017, #219): the prefix authorization, the preload rejection, the removed developer
# seams, the shared environment guard, preparation, the manager identity check, the
# frozen install through the composed envelope and the published permissions all live
# there. This file resolves the helper and names the container's environment as flags.
#
# `--` is not needed: this driver takes flags only, and the positional words belong to
# `prepare-lock.sh`, which the driver runs as a child.
set -eu

CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
[ -x "$CONTRACT/recipe-contract" ] || {
    echo "install-opencode-plugins.sh: no recipe-contract helper at $CONTRACT/recipe-contract" >&2
    exit 1
}

exec "$CONTRACT/recipe-contract" opencode-install-plugins \
    --contract "$CONTRACT" \
    --source "${AGENT_VM_OPENCODE_SOURCE_DIR:-/tmp/opencode-source}" \
    --prefix "${AGENT_VM_OPENCODE_PLUGINS_PREFIX:-/opt/agent-vm/opencode-plugins}" \
    --fixture-root "${AGENT_VM_FIXTURE_ROOT:-}" \
    --fixture-pnpm "${AGENT_VM_FIXTURE_PNPM:-}" \
    --plugin-version "${AGENT_VM_VERSION_OPENCODE_CLAUDE:-}"
