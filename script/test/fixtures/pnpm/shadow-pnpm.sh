#!/bin/sh
# A same-version raw pnpm earlier on PATH is not the reviewed launcher. The
# harness supplies the version so this program body stays native.
echo "${PNPM_SHADOW_VERSION}"
