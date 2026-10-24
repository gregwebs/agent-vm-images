#!/bin/sh
# Package-manager sentinel for the R-series runtime checks (#41).
#
# The armed test image replaces every manager found on PATH with this script, so
# a first use that reaches a manager both fails loudly and leaves a record the
# same-container checks read: a probe that "succeeds" through a manager is
# caught instead of silently fetching.
set -eu
printf '%s %s\n' "$0" "$*" >> /tmp/agent-vm-manager-calls
exit 97
