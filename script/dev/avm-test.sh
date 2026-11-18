#!/usr/bin/env bash
# The entry point for the shared fixture builders (ADR 0017: a host binary is
# reached through a `.sh` wrapper at a stable path). See pin-artifact.sh for why
# this compiles instead of using `go run`, and why the binary lives in the checkout.
#
# Two kinds of caller, one builder: the bumper suite
# (script/test/agent-versions.sh) needs the same synthetic release bytes each pin
# suite builds for itself, so the closure it serves to a writer is the closure the
# suites check against, and the Claude hook and vendored-installer suites need the
# signer, ELF blob and closure primitives their per-case fixtures are made of.
#
# Every path argument is refused unless it is absolute, because this wrapper chdirs
# before exec: a relative path would name a different tree here than it did for the
# caller.
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=$(CDPATH='' cd -- "$here/../.." && pwd)
build="$root/target/avm-test-bin"
mkdir -p "$build"
cd "$root"
go build -o "$build/avm-test" ./cmd/avm-test
exec "$build/avm-test" "$@"
