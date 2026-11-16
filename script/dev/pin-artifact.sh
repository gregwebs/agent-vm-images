#!/usr/bin/env bash
# The maintainer entry point for the artifact pin writer (ADR 0017: a host binary is
# reached through a `.sh` wrapper at a stable path).
#
# The wrapper compiles rather than using `go run`: `go run` rewrites a child's exit
# status to 1, and this tool's statuses are its contract (2 usage, 3 undecided
# recovery, 128+N after a signal). `go build` is incremental, so a warm cache costs
# a few hundred milliseconds.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH='' cd -- "$here/../.." && pwd)
build="${TMPDIR:-/tmp}/avm-pin-bin"
mkdir -p "$build"
cd "$root"
go build -o "$build/avm-pin" ./cmd/avm-pin
exec "$build/avm-pin" artifact "$@"
