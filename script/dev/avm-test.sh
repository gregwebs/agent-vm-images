#!/usr/bin/env bash
# The entry point for the shared fixture builders (ADR 0017: a host binary is
# reached through a `.sh` wrapper at a stable path). See pin-artifact.sh for why
# this compiles instead of using `go run`, and why the binary lives in the checkout.
#
# The bumper suite (script/test/agent-versions.sh) is the caller: it needs the same
# synthetic release bytes each pin suite builds for itself, so that the closure it
# serves to a writer is the closure the suites check against.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH='' cd -- "$here/../.." && pwd)
build="$root/target/avm-test-bin"
mkdir -p "$build"
cd "$root"
go build -o "$build/avm-test" ./cmd/avm-test
exec "$build/avm-test" "$@"
