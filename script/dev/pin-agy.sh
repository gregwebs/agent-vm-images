#!/usr/bin/env bash
# The maintainer entry point for the agy pin writer (ADR 0017: a host binary
# is reached through a `.sh` wrapper at a stable path). See pin-artifact.sh for why
# this compiles instead of using `go run`, and why the binary lives in the checkout.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH='' cd -- "$here/../.." && pwd)
build="$root/target/avm-pin-bin"
mkdir -p "$build"
cd "$root"
go build -o "$build/avm-pin" ./cmd/avm-pin
exec "$build/avm-pin" agy "$@"
