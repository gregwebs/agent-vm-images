#!/bin/sh
# A copied base extension carries the source identity of the checkout it was
# copied from (#95).
#
# The default `AGENT_VM_BASE_IDENTITY` is generated from this repository's
# committed base-input manifest, so building the example against a base from
# different sources fails here instead of silently extending it. This is source
# identity, not authentication: the stamp is inherited from whatever image the
# caller named, and an explicit `--build-arg` is an acceptance of that base, not
# a proof about it.
set -eu

usage() {
    echo 'usage: check-base-identity.sh EXPECTED' >&2
    exit 2
}

[ "$#" -eq 1 ] || usage
expected=$1
stamp=${AGENT_VM_BASE_STAMP:-/opt/agent-vm/base/source-identity}

fail() {
    echo "base-inputs: $*" >&2
    exit 1
}

printf '%s' "$expected" | grep -Eqx 'sha256:[0-9a-f]{64}' ||
    fail 'AGENT_VM_BASE_IDENTITY must be sha256:<64 lowercase hex>; pass --build-arg AGENT_VM_BASE_IDENTITY=<the base identity>'
[ -f "$stamp" ] || fail 'BASE_IMAGE has no repository base identity; rebuild the base from this checkout'
declared=$(cat "$stamp")
if [ "$(wc -l <"$stamp")" -ne 1 ] || ! printf '%s' "$declared" | grep -Eqx 'sha256:[0-9a-f]{64}'; then
    fail 'BASE_IMAGE declares a malformed repository base identity; rebuild the base from this checkout'
fi
[ "$declared" = "$expected" ] ||
    fail "BASE_IMAGE declares $declared; this extension expects $expected; use --build-arg AGENT_VM_BASE_IDENTITY=$declared only if you intend that base"
echo "base-inputs: verified $stamp ($expected)"
