#!/bin/sh
set -eu
: "${PNPM_CONFIG_MINIMUM_RELEASE_AGE:=1440}"
: "${PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT:=true}"
: "${PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME:=false}"
: "${PNPM_CONFIG_TRUST_POLICY:=no-downgrade}"
: "${PNPM_CONFIG_IGNORE_SCRIPTS:=true}"
export PNPM_CONFIG_MINIMUM_RELEASE_AGE PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME PNPM_CONFIG_TRUST_POLICY
export PNPM_CONFIG_IGNORE_SCRIPTS
case "${0##*/}" in
    pnpm) exec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpm.mjs "$@" ;;
    pnx|pnpx) exec /usr/bin/node /opt/agent-vm/pnpm/bin/pnpx.mjs "$@" ;;
    *) echo 'pnpm launcher: unsupported invocation name' >&2; exit 1 ;;
esac
