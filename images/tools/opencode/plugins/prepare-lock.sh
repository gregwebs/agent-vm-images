#!/bin/sh
# Developer preparation entry point for the OpenCode plugin inputs.
#
# The policy is the `opencode-prepare-lock` subcommand of the composed helper: the
# exact-semver grammar, the prefix authorization, the preload and environment
# guards, the manager identity check, the staged resolution and the three-input
# publication all live there (ADR 0017, #218), because one rule has to serve the
# image build, the CI leg and every developer upgrade.
#
# This file resolves the helper and names the environment the container exports as
# flags; the positional words stay the caller's own grammar, so an option this
# driver does not have reaches the driver and is refused there.
#
# `--` ends the flags before the caller's words. Without it a caller's
# `PREFIX 1.4.1 --check other.mjs` would be read as this program's own checker, and
# `--refresh-lock` itself would be refused as an unknown option.
#
# Locating the helper is the entry point's own step, so it happens before the
# guards the driver applies: an absent composed directory reports 127 instead of a
# usage refusal. Nothing in a build can reach the re-pin path -- the install hook
# passes no --refresh-lock, and the checker it runs is the one this driver runs.
set -eu

self_dir="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)"
CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"

exec "$CONTRACT/recipe-contract" opencode-prepare-lock \
    --contract "$CONTRACT" \
    --check "${AGENT_VM_OPENCODE_LOCK_CHECK:-$self_dir/check-lock.mjs}" \
    --pin "${AGENT_VM_OPENCODE_LOCK_PIN:-$self_dir/pin-pnpm-inputs.mjs}" \
    --fixture-root "${AGENT_VM_FIXTURE_ROOT:-}" \
    -- "$@"
