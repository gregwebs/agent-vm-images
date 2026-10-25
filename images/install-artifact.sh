#!/usr/bin/env bash
# Install one committed release-artifact pin (#96): `install-artifact.sh NAME PIN ARCH`.
# The pin is the only input; the reviewable policy lives in artifacts/ next to it.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
source_dir=${ARTIFACT_SOURCE_DIR:-/tmp/artifact-source}
exec python3 "$source_dir/artifacts/install_artifact.py" "$@"
