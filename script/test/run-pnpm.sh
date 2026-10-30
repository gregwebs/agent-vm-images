#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cat >"$work/manager" <<'SH'
#!/bin/sh
[ -z "${AGENT_VM_TRANSPORT_RECEIPT:-}" ] || exit 90
printf '%s\n' "$*" >"$ARGV"
printf '%s\n' "${OUTPUT:-{\"name\":\"pnpm\"}}"
exit "${STATUS:-0}"
SH
chmod +x "$work/manager"
export ARGV="$work/argv"
export AGENT_VM_TRANSPORT_RECEIPT="$work/receipt"
printf stale >"$work/receipt"
sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/result" -- "$work/manager" install --frozen-lockfile
[ "$(cat "$work/argv")" = 'install --frozen-lockfile --reporter=ndjson' ]
[ "$(cat "$work/receipt")" = stale ]
for status in 1 75 124 130 143; do
    for output in 'ERR_PNPM_META_FETCH_FAIL' 'ERR_PNPM_FETCH_500' 'garbage'; do
        if STATUS="$status" OUTPUT="$output" sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/result" -- "$work/manager" >"$work/log" 2>&1; then
            echo "unexpected runner success: $status $output" >&2
            exit 1
        fi
        [ "$(cat "$work/receipt")" = stale ]
    done
done
if sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/missing/result" -- "$work/manager" >"$work/log" 2>&1; then
    echo 'capture failure passed' >&2
    exit 1
fi
OUTPUT='https://user:secret@registry.invalid/ Bearer secret _authToken=secret' \
    sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/result" -- "$work/manager"
! grep -q secret "$work/result"
# A manager writes multi-megabyte files (package tarballs, store content). The
# runner must not impose RLIMIT_FSIZE: doing so killed a correct install with
# SIGXFSZ (status 153), which reads as an unclassifiable signal failure.
cat >"$work/writer" <<'SH'
#!/bin/sh
dd if=/dev/zero of="$BIG" bs=1024 count=4096 2>/dev/null
printf '%s\n' '{"name":"pnpm"}'
SH
chmod +x "$work/writer"
export BIG="$work/big-file"
sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/writer-result" -- "$work/writer" >/dev/null 2>&1
[ "$(wc -c <"$work/big-file" | tr -d ' ')" = 4194304 ]
# Published diagnostics are bounded after the child exits, so a chatty manager
# neither fails nor leaves an unbounded artifact.
cat >"$work/loud" <<'SH'
#!/bin/sh
i=0
while [ "$i" -lt 20000 ]; do printf '{"level":"debug","n":%s}\n' "$i"; i=$((i + 1)); done
SH
chmod +x "$work/loud"
sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/loud-result" -- "$work/loud" >/dev/null 2>&1
[ "$(wc -c <"$work/loud-result" | tr -d ' ')" -le 262144 ]
# Signal the invocation-owned runner, not a fake status: its timeout child must
# forward termination to the monitored process group before scratch disappears.
cat >"$work/signal-manager" <<'SH'
#!/bin/sh
printf '%s\n' "$$" >"$READY"
sleep 30
SH
chmod +x "$work/signal-manager"
export READY="$work/ready"
sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/signal-result" -- "$work/signal-manager" >"$work/signal-log" 2>&1 &
runner=$!
for ((i=0; i<100; i++)); do
    [ ! -f "$READY" ] || break
    sleep 0.05
done
[ -f "$READY" ]
kill -TERM "$runner"
signal_status=0
wait "$runner" || signal_status=$?
[ "$signal_status" -ne 0 ]
! kill -0 "$(cat "$READY")" 2>/dev/null
[ "$(cat "$work/receipt")" = stale ]
# Keep timeout tests hermetic while asserting both production bound arguments.
mkdir "$work/bin"
cat >"$work/bin/timeout" <<'SH'
#!/bin/sh
[ "$1" = --kill-after=5 ] && [ "$2" = 600 ] || exit 91
exit 124
SH
chmod +x "$work/bin/timeout"
if PATH="$work/bin:$PATH" sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/result" -- "$work/manager" >"$work/log" 2>&1; then
    echo 'timeout passed' >&2
    exit 1
fi
echo 'run-pnpm: hard failures, receipt isolation, capture failure, redaction, file writes and output bounds passed'
# The production bound is 600s with a 5s SIGKILL escalation. The hermetic case
# above asserts the arguments; this one proves the escalation actually terminates
# a manager that ignores TERM, so a hung pnpm cannot hang a build. The fake
# `timeout` keeps the helper's own arguments and shortens the durations.
#
# The real binaries are resolved before the PATH prefix takes effect: a
# `PATH=$shim:$PATH REAL=$(command -v x)` assignment expands `command -v` with
# the shim already in PATH, so the shim would exec itself forever. Every case is
# additionally bounded so such a mistake fails instead of hanging.
REAL_TIMEOUT=$(command -v timeout)
REAL_MKTEMP=$(command -v mktemp)
mkdir -p "$work/bin-kill"
cat >"$work/bin-kill/timeout" <<'SH'
#!/bin/sh
[ "$1" = --kill-after=5 ] && [ "$2" = 600 ] || exit 91
shift 2
exec "$REAL_TIMEOUT" --kill-after=1 1 "$@"
SH
chmod +x "$work/bin-kill/timeout"
cat >"$work/term-ignoring" <<'SH'
#!/bin/sh
trap '' TERM
sleep 30
SH
chmod +x "$work/term-ignoring"
start=$(date +%s)
kill_status=0
PATH="$work/bin-kill:$PATH" REAL_TIMEOUT="$REAL_TIMEOUT" timeout 20 \
    sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/kill-result" -- "$work/term-ignoring" \
    >"$work/kill-log" 2>&1 || kill_status=$?
elapsed=$(( $(date +%s) - start ))
[ "$kill_status" -ne 0 ] || { echo 'a TERM-ignoring manager must not pass' >&2; exit 1; }
[ "$kill_status" -ne 124 ] || { echo 'the escalation never terminated the manager' >&2; exit 1; }
[ "$elapsed" -lt 15 ] || { echo "the manager bound must SIGKILL (took ${elapsed}s)" >&2; exit 1; }
# A8/A7: the scratch cleanup trap must not re-parse a TMPDIR that contains shell
# syntax. BSD mktemp ignores TMPDIR, so a shim reproduces GNU mktemp's honouring
# of it; the marker proves no injected command ran.
mkdir -p "$work/bin-tmpdir"
cat >"$work/bin-tmpdir/mktemp" <<'SH'
#!/bin/sh
case "${1:-}" in
    -d) shift; exec "$REAL_MKTEMP" -d "${TMPDIR:-/tmp}/tmp.XXXXXXXXXX" ;;
    *) exec "$REAL_MKTEMP" "$@" ;;
esac
SH
chmod +x "$work/bin-tmpdir/mktemp"
evil="$work/tmp'; touch $work/MARKER; #"
mkdir -p "$evil"
tmpdir_status=0
PATH="$work/bin-tmpdir:$PATH" TMPDIR="$evil" REAL_MKTEMP="$REAL_MKTEMP" timeout 20 \
    sh "$ROOT/images/recipe-contract/run-pnpm.sh" "$work/tmpdir-result" -- "$work/manager" \
    >"$work/tmpdir-log" 2>&1 || tmpdir_status=$?
[ "$tmpdir_status" -eq 0 ] || { echo "a quote in TMPDIR must not fail the runner (status $tmpdir_status)" >&2; exit 1; }
[ ! -e "$work/MARKER" ] || { echo 'a quote in TMPDIR injected into the cleanup trap' >&2; exit 1; }
echo 'run-pnpm: TERM-ignoring manager is killed by the escalation; quote TMPDIR is inert'
