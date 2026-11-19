#!/bin/sh
# Preparation entry point for the dsh layer.
#
# Usage: prepare-lock.sh PREFIX DSH_VERSION PNPM_VERSION [--refresh-lock]
#
# The policy is the `dsh-prepare-lock` subcommand of the composed helper: the exact
# selection grammar, the base-pin and manifest assertions, the staged manifest, the
# unseeded resolution, the structural and peer validation and the four-input
# transaction all live there, because one rule has to serve the image build, the CI
# changed leg and every developer upgrade.
#
# This file only resolves the helper and passes the caller's argv through unchanged, so
# the option words stay the driver's own grammar. The helper is located through the
# shared project module, whose `contract-dir` authorizes the directory this invocation
# may execute before anything from it runs.
set -eu

usage() {
    echo 'usage: prepare-lock.sh PREFIX DSH_VERSION PNPM_VERSION [--refresh-lock]' >&2
}

# The arity guard exists only so `$1` can be named before the helper is resolved. It
# is not the grammar: the driver checks the package-manager environment first, then the
# slots, then the options, and it is the only place a duplicate or unknown option is
# refused.
if [ "$#" -lt 1 ]; then
    usage
    exit 2
fi
PREFIX=$1
self_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
SOURCE=${AGENT_VM_DSH_SOURCE_DIR:-$self_dir}
PROJECT=$SOURCE/project.py
[ -f "$PROJECT" ] || {
    echo "prepare-lock.sh: dsh project helper is missing at $PROJECT" >&2
    exit 1
}
CONTRACT=$(python3 "$PROJECT" contract-dir "$PREFIX") || exit 1

exec "$CONTRACT/recipe-contract" dsh-prepare-lock \
    --source "$SOURCE" --contract "$CONTRACT" "$@"
