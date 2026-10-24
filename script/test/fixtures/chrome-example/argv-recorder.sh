#!/bin/sh
# Stand-in for the installed MCP binary in the argv image (R5, #41).
#
# The wrapper's accepted rows are asserted on the argv that actually reached the
# installed program, not on the wrapper's own text. Truncating first makes the
# file mean "this invocation", so a stale file cannot satisfy an accepted row
# and a refused row is detected by the file being absent altogether.
set -eu
: > /tmp/argv-out
for arg in "$@"; do
    printf '%s\n' "$arg" >> /tmp/argv-out
done
