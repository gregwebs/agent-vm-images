#!/usr/bin/env bash
# Exercise the real harness's nested bounds and EXIT evidence with a native
# daemon. Only build/slow cleanup clients are shimmed; no browser flags change.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
cd "$ROOT"
BASE_IMAGE=${BASE_IMAGE:-agent-vm-standard:local}
WATCHDOG_SECONDS=${WATCHDOG_SECONDS:-60}
WATCHDOG_GRACE_SECONDS=75
export CHROME_EXAMPLE_WATCHDOG_GRACE_SECONDS=$WATCHDOG_GRACE_SECONDS
# Never let a previous invocation's completion files satisfy this run.
EVID=${EVIDENCE_DIR:-"$ROOT/target/chrome-example/watchdog-positive"}/run-$(date +%s)-$$
mkdir -p "$EVID"
EVID=$(cd "$EVID" && pwd)
WATCHDOG="$ROOT/script/test/host-watchdog.py"
QUERY_SECONDS=10
QUERY_GRACE_SECONDS=0.5
shim=$(mktemp -d)
real_docker=$(command -v docker)
fail() { echo "watchdog control: $*" >&2; exit 1; }
bounded() { python3 "$WATCHDOG" --grace-seconds "$QUERY_GRACE_SECONDS" "$QUERY_SECONDS" "$@"; }
cleanup() {
    local status=$?
    trap - EXIT
    rm -rf "$shim"
    exit "$status"
}
trap cleanup EXIT
platform="linux/$(bounded docker version -f '{{.Server.Arch}}')"

for mode in ignored-term slow-cleanup inner-expiry; do
    case_dir="$EVID/$mode"
    mkdir -p "$case_dir"
    export CONTROL_DIR="$case_dir" CONTROL_MODE="$mode" CONTROL_BASE="$BASE_IMAGE" REAL_DOCKER="$real_docker"
    cat >"$shim/docker" <<'SHIM'
#!/bin/sh
set -eu
if [ "${1:-}" = build ]; then
    touch "$CONTROL_DIR/build-started"
    if [ "$CONTROL_MODE" = ignored-term ]; then
        while [ "$1" != --label ]; do shift; done
        "$REAL_DOCKER" create --label "$2" "$CONTROL_BASE" sleep 600 > "$CONTROL_DIR/seed-container.txt"
    fi
    # A descendant that ignores TERM must be killed on BOTH outer and inner expiry.
    trap '' TERM
    sleep 600 &
    echo $! > "$CONTROL_DIR/descendant.pid"
    wait
    exit 0
fi
if [ "$CONTROL_MODE" = slow-cleanup ] && [ -f "$CONTROL_DIR/build-started" ]; then
    case "${1:-}" in ps|image) trap '' TERM; exec sleep 600 ;; esac
fi
exec "$REAL_DOCKER" "$@"
SHIM
    chmod +x "$shim/docker"
    harness="$ROOT/script/test/chrome-example.sh"
    if [ "$mode" = inner-expiry ]; then
        # Exercise the actual timed/cleanup code with a short inner build budget,
        # leaving ample outer budget. The source file itself is never mutated.
        harness="$shim/inner.sh"
        sed -e "s|^ROOT=.*|ROOT='$ROOT'|" -e 's/^BUILD_SECONDS=1500/BUILD_SECONDS=0.5/' \
            script/test/chrome-example.sh > "$harness"
    fi
    if [ "$mode" = slow-cleanup ]; then
        # Deliberate daemon outage, not ordinary listing latency: shorten only
        # this scratch harness's total budget so one 5 s timeout exhausts it.
        harness="$shim/slow.sh"
        sed -e "s|^ROOT=.*|ROOT='$ROOT'|" -e 's/^CLEANUP_SECONDS=60 /CLEANUP_SECONDS=9 /' \
            script/test/chrome-example.sh > "$harness"
    fi
    status=0
    PATH="$shim:$PATH" python3 "$WATCHDOG" --grace-seconds "$WATCHDOG_GRACE_SECONDS" "$WATCHDOG_SECONDS" bash "$harness" \
        --platform "$platform" --base-image "$BASE_IMAGE" --evidence-dir "$case_dir" \
        > "$case_dir/transcript.log" 2>&1 || status=$?
    printf 'supervisor_status=%s\n' "$status" > "$case_dir/control.txt"
    if [ "$mode" = inner-expiry ]; then
        [ "$status" -eq 124 ] || fail "inner bound returned $status"
        grep -Eqx 'build seconds=[0-9]+ status=124' "$case_dir/durations.txt" || fail 'inner expiry not recorded'
        ! grep -Fq "command timed out after ${WATCHDOG_SECONDS}s" "$case_dir/transcript.log" || fail 'outer bound fired in inner control'
    else
        [ "$status" -eq 124 ] || fail "outer watchdog returned $status"
        grep -Eqx 'build seconds=[0-9]+ status=interrupted' "$case_dir/durations.txt" || fail 'interrupted timing missing'
    fi
    roster=$(awk -F= '$1 == "phases" {print $2}' "$case_dir/evidence.txt")
    [ -n "$roster" ] || fail 'missing roster'
    expected=$(printf '%s\n' $roster | wc -l | tr -d ' ')
    actual=$(wc -l < "$case_dir/verdicts.txt" | tr -d ' ')
    [ "$expected" -eq "$actual" ] || fail "$mode: $actual verdicts for $expected phases"
    for phase in $roster; do
        [ "$(grep -c "^$phase " "$case_dir/verdicts.txt")" -eq 1 ] || fail "duplicate/missing $phase"
        if [ "$phase" = B ]; then
            grep -qx 'B FAIL' "$case_dir/verdicts.txt" || fail 'build not FAIL'
        else
            grep -Eqx "$phase (PASS|NOT-RUN)" "$case_dir/verdicts.txt" || fail "unexpected $phase verdict"
        fi
    done
    [ -f "$case_dir/cleanup.txt" ] || fail 'cleanup evidence incomplete'
    if [ "$mode" = slow-cleanup ]; then
        grep -Eqx 'cleanup-containers seconds=[0-9]+ status=124' "$case_dir/durations.txt" || fail 'slow listing did not expire'
        grep -qx 'cleanup-images seconds=0 status=skipped-deadline' "$case_dir/durations.txt" || fail 'deadline admission not exercised'
        grep -qx 'completed status=1' "$case_dir/cleanup.txt" || fail 'cleanup failure not recorded'
    else
        grep -qx 'completed status=0' "$case_dir/cleanup.txt" || fail 'cleanup failed'
    fi
    pid=$(cat "$case_dir/descendant.pid")
    if kill -0 "$pid" 2>/dev/null; then
        kill -9 "$pid" 2>/dev/null || true
        fail "descendant $pid survived $mode"
    fi
    run_id=$(awk -F= '$1 == "run_id" {print $2}' "$case_dir/evidence.txt")
    [ -n "$run_id" ] || fail 'missing run ID'
    label="org.agent-vm.test.chrome-example=$run_id"
    ids=$(bounded docker ps -aq --filter "label=$label") || fail 'container query failed'
    [ -z "$ids" ] || fail 'owned containers remain'
    ids=$(bounded docker image ls -q --filter "label=$label") || fail 'image query failed'
    [ -z "$ids" ] || fail 'owned images remain'
    if [ "$mode" = ignored-term ]; then
        [ -s "$case_dir/seed-container.txt" ] || fail 'no owned container seeded'
        grep -q '^cleanup-container-.* status=0$' "$case_dir/durations.txt" || fail 'seed deletion not recorded'
    fi
    echo "watchdog control: $mode passed; $actual verdicts; run_id=$run_id" | tee -a "$case_dir/control.txt"
done
