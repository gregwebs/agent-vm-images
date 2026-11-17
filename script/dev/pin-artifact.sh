#!/usr/bin/env bash
# The maintainer entry point for the artifact pin writer (ADR 0017: a host binary is
# reached through a `.sh` wrapper at a stable path).
#
# The wrapper compiles rather than using `go run`: `go run` rewrites a child's exit
# status to 1, and this tool's statuses are its contract (2 usage, 3 undecided
# recovery, 128+N after a signal). `go build` is incremental, so a warm cache costs
# a few hundred milliseconds.
#
# The binary lives under this checkout's own ignored scratch directory rather than a
# shared temp path: a predictable path outside the checkout can be pre-created or
# left behind by a different worktree, and either one runs the wrong code.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH='' cd -- "$here/../.." && pwd)
build="$root/target/avm-pin-bin"
mkdir -p "$build"
cd "$root"
go build -o "$build/avm-pin" ./cmd/avm-pin
exec "$build/avm-pin" artifact "$@"
