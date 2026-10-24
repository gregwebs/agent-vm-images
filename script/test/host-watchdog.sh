#!/usr/bin/env bash
# Black-box tests for the host process-group watchdog
# (script/test/host-watchdog.py).
#
# The load-bearing property is that escalation does NOT depend on the group
# leader exiting: a grandchild that ignores SIGTERM must still be SIGKILLed, and
# an inherited SIGTERM must be forwarded to the child group. These are the two
# ways an orphaned `docker run`/buildx client could survive a harness timeout.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
WATCHDOG="$REPO_ROOT/script/test/host-watchdog.py"

command -v python3 >/dev/null 2>&1 || { echo "FAIL: python3 is required" >&2; exit 1; }

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/host-watchdog.XXXXXX")"
trap 'rm -rf "$ROOT"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

alive() {
    kill -0 "$1" 2>/dev/null
}

# --- 1. a SIGTERM-ignoring grandchild is SIGKILLed on timeout ----------------

pidfile="$ROOT/grandchild.pid"
set +e
python3 "$WATCHDOG" 1 python3 "$REPO_ROOT/script/test/host-watchdog-leader.py" \
    "$REPO_ROOT/script/test/host-watchdog-grandchild.py" "$pidfile"
status=$?
set -e
[[ $status -eq 124 ]] || fail "expected timeout exit 124, got $status"
[[ -f "$pidfile" ]] || fail "the grandchild never started"
gpid="$(cat "$pidfile")"
if alive "$gpid"; then
    kill -9 "$gpid" 2>/dev/null || true
    fail "SIGTERM-ignoring grandchild $gpid survived the watchdog timeout"
fi

# --- 2. an inherited SIGTERM is forwarded to the child group -----------------

pidfile="$ROOT/sleeper.pid"
python3 "$WATCHDOG" 300 python3 "$REPO_ROOT/script/test/host-watchdog-sleeper.py" "$pidfile" &
watchdog_pid=$!
for _ in $(seq 1 200); do
    [ -f "$pidfile" ] && break
    sleep 0.05
done
[[ -f "$pidfile" ]] || { kill -9 "$watchdog_pid" 2>/dev/null || true; fail "the sleeper never started"; }
spid="$(cat "$pidfile")"
kill -TERM "$watchdog_pid"
set +e
wait "$watchdog_pid"
set -e
if alive "$spid"; then
    kill -9 "$spid" 2>/dev/null || true
    fail "the child group survived a forwarded SIGTERM (sleeper $spid)"
fi

# --- 3. --grace-seconds shortens the post-TERM escalation window ------------
# Nested supervisors pass a sub-second grace so their own trap still runs inside
# the outer watchdog's grace. Escalation must still reach the grandchild.

pidfile="$ROOT/short-grace-grandchild.pid"
statusfile="$ROOT/short-grace.status"
supervisorfile="$ROOT/short-grace-supervisor.pid"
# The default 10 s grace spends about 11 s here; the shortened one must be done
# before this two-second deadline, so the assertion cannot pass by luck. The
# status file is written by the subshell itself, so the result does not depend on
# the parent noticing the exit, and the supervisor pid is recorded so a failure
# can still escalate through it instead of orphaning the child group.
( status=0
  python3 "$WATCHDOG" --grace-seconds 0.2 1 python3 "$REPO_ROOT/script/test/host-watchdog-leader.py" \
      "$REPO_ROOT/script/test/host-watchdog-grandchild.py" "$pidfile" &
  supervisor=$!
  echo "$supervisor" > "$supervisorfile"
  wait "$supervisor" || status=$?
  echo "$status" > "$statusfile" ) &
grace_pid=$!
sleep 2
if [ ! -f "$statusfile" ]; then
    kill -TERM "$(cat "$supervisorfile")" 2>/dev/null || true
    wait "$grace_pid" 2>/dev/null || true
    fail "--grace-seconds 0.2 did not finish within 2s"
fi
wait "$grace_pid"
status="$(cat "$statusfile")"
[[ $status -eq 124 ]] || fail "expected timeout exit 124 with --grace-seconds, got $status"
[[ -f "$pidfile" ]] || fail "the grandchild never started with --grace-seconds"
gpid="$(cat "$pidfile")"
if alive "$gpid"; then
    kill -9 "$gpid" 2>/dev/null || true
    fail "SIGTERM-ignoring grandchild $gpid survived the shortened grace"
fi

set +e
python3 "$WATCHDOG" --grace-seconds 0 1 true 2>/dev/null
status=$?
set -e
[[ $status -eq 2 ]] || fail "expected exit 2 for --grace-seconds 0, got $status"

echo "host-watchdog black-box tests passed"
