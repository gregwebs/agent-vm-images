#!/bin/sh
# pnpm diagnostics are NDJSON, not npm's single-object error protocol. Until
# terminal transport failures can be proved, every failure remains hard.
set -eu
if [ "$#" -lt 3 ] || [ "$2" != -- ]; then
    echo 'usage: run-pnpm.sh RESULT_FILE -- COMMAND ARG...' >&2
    exit 2
fi
result=$1
shift 2
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
child=
interrupt() {
    if [ -n "$child" ]; then
        kill -TERM "$child" 2>/dev/null || true
        wait "$child" 2>/dev/null || true
    fi
    exit "$1"
}
trap 'interrupt 130' INT
trap 'interrupt 143' TERM
trap 'interrupt 129' HUP
unset AGENT_VM_TRANSPORT_RECEIPT
status=0
(
    exec timeout --kill-after=5 600 "$@" --reporter=ndjson
) >"$scratch/out" 2>"$scratch/err" &
child=$!
wait "$child" || status=$?
child=
# Redact registry credentials before publishing diagnostics. The bounded raw
# capture is invocation-owned and never becomes a transport receipt.
#
# The child gets no RLIMIT_FSIZE. A manager legitimately writes multi-megabyte
# files -- package tarballs and store content -- so a file-size limit kills a
# correct install with SIGXFSZ (status 153), which is indistinguishable from a
# real signal failure. Bound the published artifact instead, after the child
# exits, where truncation cannot affect it.
redact() {
    LC_ALL=C sed -E \
        -e 's#(https?://)[^ /"<>]*@#\1[redacted]@#g' \
        -e 's/([Bb][Ee][Aa][Rr][Ee][Rr]|[Bb][Aa][Ss][Ii][Cc])[[:space:]]+[A-Za-z0-9+\/=._-]+/\1 [redacted]/g' \
        -e 's/([Tt][Oo][Kk][Ee][Nn]|[Pp][Aa][Ss][Ss][Ww][Oo][Rr][Dd]|_auth)["[:space:]:=]+[^ ,"}]+/\1=[redacted]/g' "$1"
}
if ! redact "$scratch/out" | tail -c 262144 >"$result" || ! redact "$scratch/err" >"$scratch/redacted-err"; then
    echo 'run-pnpm.sh: output capture failed' >&2
    exit 1
fi
tail -c 8192 "$scratch/redacted-err" >&2

if [ "$status" -ne 0 ]; then
    echo "run-pnpm.sh: hard failure (status $status); no transport classification" >&2
    tail -c 8192 "$result" >&2
    exit 1
fi
