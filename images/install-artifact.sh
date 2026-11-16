#!/usr/bin/env bash
# Install one committed release-artifact pin (#96): `install-artifact.sh NAME PIN ARCH`
# with the optional `--root DIR` and `--events FILE` the recipe-contract
# subcommand documents. The pin is the only input; the reviewable policy lives in
# artifacts/ next to it.
#
# The installer is a recipe-contract subcommand, so the helper has to be supplied
# by the host compile: the build mounts the composed directory and names it here.
set -euo pipefail
helper=${RECIPE_CONTRACT_HELPER:-/tmp/recipe-contract/recipe-contract}
if [ ! -x "$helper" ]; then
    echo "install-artifact.sh: no recipe-contract helper at $helper" >&2
    exit 1
fi
exec "$helper" install-artifact "$@"
