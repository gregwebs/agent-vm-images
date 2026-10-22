#!/bin/bash
# Fake host compiler used by build-entrypoint.py's spaced checkout.
set -euo pipefail
printf '%s\n' "$*" >> "$HOST_LOG"
[ "${HOST_STATUS:-0}" = 0 ] || exit "${HOST_STATUS}"
printf '%s\n' "$HOST_CONTEXT"
