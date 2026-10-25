#!/usr/bin/env bash
# Offline evidence for the pinned release-artifact installer (#96).
# The fixture tree, its signing key and every mutation are Python sources
# beside this wrapper; see script/test/artifact-pins.py.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
exec python3 "${BASH_SOURCE[0]%/*}/artifact-pins.py" "$@"
