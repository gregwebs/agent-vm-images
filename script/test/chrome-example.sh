#!/usr/bin/env bash
# Current-recipe browser checks; no MCP or package-fetching guarantee.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
cd "$ROOT"
RUN_ID=$(printf '%s' "${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$-$(date +%s)" | tr 'A-Z' 'a-z')
PLATFORM=''
PARENT=$(awk 'NR == 1 && sub(/^ARG BASE_IMAGE=/, "") { print; exit }' examples/layers/chrome-devtools/Dockerfile)
EVID="$ROOT/target/chrome-example/$RUN_ID"
usage() { echo 'usage: chrome-example.sh [--platform linux/ARCH] [--base-image REF] [--evidence-dir DIR]' >&2; exit 2; }
while [ "$#" -gt 0 ]; do
    [ "$#" -ge 2 ] && [ -n "$2" ] || usage
    case "$1" in
        --platform) PLATFORM=$2 ;;
        --base-image) PARENT=$2 ;;
        --evidence-dir) EVID=$2 ;;
        *) usage ;;
    esac
    shift 2
done
mkdir -p "$EVID"
EVID=$(cd "$EVID" && pwd)
# Client bounds, declared next to the commands that consume them.
DISCOVERY_SECONDS=30        # docker version/context/inspect/list
CONTAINER_CHECK_SECONDS=120 # one check container: create, copy fixtures, start
BUILDER_GUARD_SECONDS=120
PULL_SECONDS=900
BUILD_SECONDS=1500
KILL_GRACE_SECONDS=0.5
# Invariant: supervising watchdog grace must exceed CLEANUP_SECONDS plus margin
# (5 s for inner escalation/trap entry). Callers use --grace-seconds 75; the
# watchdog's default 10 s is insufficient. An optional declaration catches bad
# callers early; arbitrary parent watchdog settings cannot be discovered here.
CLEANUP_SECONDS=60          # total deadline, not a per-command allowance
CLEANUP_COMMAND_SECONDS=5
CLEANUP_ESCALATION_SECONDS=1 # two 0.5 s supervisor polling windows
CLEANUP_RESERVE_SECONDS=2   # evidence/scheduling, including SECONDS truncation
# Two listings + six containers + one image: 9 * (5 + 1) + 2 = 56 < 60.
# Normal checks remove containers immediately; six covers interrupted checks too.
if [ -n "${CHROME_EXAMPLE_WATCHDOG_GRACE_SECONDS:-}" ]; then
    case "$CHROME_EXAMPLE_WATCHDOG_GRACE_SECONDS" in
        *[!0-9]*) echo 'chrome-example: watchdog grace must be integer seconds' >&2; exit 2 ;;
    esac
    if [ "$CHROME_EXAMPLE_WATCHDOG_GRACE_SECONDS" -le "$((CLEANUP_SECONDS + 5))" ]; then
        echo 'chrome-example: supervising watchdog grace must exceed cleanup plus 5 s; use 75 s' >&2
        exit 2
    fi
fi
WATCHDOG="$ROOT/script/test/host-watchdog.py"
# Single roster: the final phase validates every other one and the success banner
# counts this list.
PHASES='P0 P B S I N W D E'
FINAL_PHASE=E
PHASE_COUNT=0
for phase in $PHASES; do PHASE_COUNT=$((PHASE_COUNT + 1)); done
: > "$EVID/verdicts.txt"
: > "$EVID/durations.txt"
: > "$EVID/evidence.txt"
# A reused --evidence-dir must not present a previous run's completion as this
# run's cleanup evidence. Only files the harness alone creates and reads back are
# removed here: a `*.log` sweep would delete a caller's redirect target (the
# watchdog control tees its transcript into this directory) and would be an
# unbounded `rm` in a user-supplied path.
rm -f "$EVID/cleanup.txt" "$EVID/cleanup-image-ids.txt"
LABEL="org.agent-vm.test.chrome-example=$RUN_ID"
CURRENT=P0
# Name of the `timed` command currently in flight, so cleanup can record it when a
# signal preempts that call.
ACTIVE=''
ACTIVE_START=0
# Each command has its own group. Inherited signals are forwarded and escalated
# before re-raising, so Bash can enter its trap within the outer grace.
timed() {
    local label=$1 seconds=$2 status=0
    shift 2
    ACTIVE=$label
    ACTIVE_START=$SECONDS
    python3 "$WATCHDOG" --grace-seconds "$KILL_GRACE_SECONDS" "$seconds" "$@" \
        > "$EVID/$label.log" 2>&1 || status=$?
    ACTIVE=''
    cat "$EVID/$label.log"
    printf '%s seconds=%s status=%s\n' "$label" "$((SECONDS-ACTIVE_START))" "$status" >> "$EVID/durations.txt"
    return "$status"
}
cleanup_timed() {
    local label=$1 remaining
    shift
    remaining=$((CLEANUP_DEADLINE - SECONDS - CLEANUP_RESERVE_SECONDS))
    # Reserve both TERM and post-KILL polling windows before starting any client.
    if [ "$remaining" -lt "$((CLEANUP_COMMAND_SECONDS + CLEANUP_ESCALATION_SECONDS))" ]; then
        printf '%s seconds=0 status=skipped-deadline\n' "$label" >> "$EVID/durations.txt"
        return 1
    fi
    timed "$label" "$CLEANUP_COMMAND_SECONDS" "$@"
}
cleanup() {
    local status=$? id phase cleanup_status=0
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_SECONDS))
    trap - EXIT
    # Bash ignores further signals, keeping the record and resource removal going.
    # An in-flight cleanup client can still be aborted by a nested supervisor
    # forwarding that signal, and the abort is recorded as a cleanup failure:
    # signal-ignoring never turns a failed removal into a success.
    trap '' INT TERM HUP
    if [ "$status" -ne 0 ]; then
        grep -q "^$CURRENT " "$EVID/verdicts.txt" || echo "$CURRENT FAIL" >> "$EVID/verdicts.txt"
    fi
    for phase in $PHASES; do
        grep -q "^$phase " "$EVID/verdicts.txt" || echo "$phase NOT-RUN" >> "$EVID/verdicts.txt"
    done
    # A signal preempts `timed` before it can record: the command is still ACTIVE.
    if [ -n "$ACTIVE" ]; then
        printf '%s seconds=%s status=interrupted\n' "$ACTIVE" "$((SECONDS-ACTIVE_START))" >> "$EVID/durations.txt"
        ACTIVE=''
    fi
    if cleanup_timed cleanup-containers docker ps -aq --filter "label=$LABEL"; then
        while IFS= read -r id; do
            [ -z "$id" ] || cleanup_timed "cleanup-container-$id" docker rm -f "$id" >/dev/null || cleanup_status=1
        done < "$EVID/cleanup-containers.log"
    else cleanup_status=1; fi
    if cleanup_timed cleanup-images docker image ls -q --filter "label=$LABEL"; then
        sort -u "$EVID/cleanup-images.log" > "$EVID/cleanup-image-ids.txt"
        while IFS= read -r id; do
            [ -z "$id" ] || cleanup_timed "cleanup-image-${id#sha256:}" docker image rm -f "$id" >/dev/null || cleanup_status=1
        done < "$EVID/cleanup-image-ids.txt"
    else cleanup_status=1; fi
    printf 'completed status=%s\n' "$cleanup_status" > "$EVID/cleanup.txt"
    [ "$cleanup_status" -eq 0 ] || { echo 'owned-resource cleanup failed' >&2; status=1; }
    if [ "$status" -eq 0 ]; then
        echo "chrome-example: PASS ($PHASE_COUNT phases) platform=$PLATFORM parent=$PARENT"
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
pass() { echo "$CURRENT PASS" >> "$EVID/verdicts.txt"; }
fail() { echo "chrome-example: $*" >&2; exit 1; }
record() { printf '%s=%s\n' "$1" "$2" >> "$EVID/evidence.txt"; }
run_check() {
    local label=$1 image=$2 ca='' cid status=0
    local opts=()
    shift 2
    while [ "$1" != -- ]; do
        case "$1" in --test-ca) ca=yes; shift ;; *) opts+=("$1"); shift ;; esac
    done
    shift
    timed "$label-create" "$CONTAINER_CHECK_SECONDS" docker create --label "$LABEL" --security-opt seccomp=unconfined \
        ${opts[@]+"${opts[@]}"} "$image" "$@" >/dev/null || fail "$label infrastructure failure"
    cid=$(cat "$EVID/$label-create.log")
    timed "$label-fixtures" "$CONTAINER_CHECK_SECONDS" docker cp "$ROOT/script/test/fixtures/chrome-example" "$cid:/agent-vm-test" >/dev/null || fail "$label infrastructure failure"
    if [ -n "$ca" ]; then
        timed "$label-ca" "$CONTAINER_CHECK_SECONDS" docker cp "$ROOT/script/test/fixtures/chrome-example/test-ca.pem" \
            "$cid:/usr/local/share/ca-certificates/microsandbox-ca.crt" >/dev/null || fail "$label infrastructure failure"
    fi
    timed "$label" "$CONTAINER_CHECK_SECONDS" docker start -a "$cid" || status=$?
    # A client/daemon failure is not evidence that a negative assertion fired.
    timed "$label-state" "$DISCOVERY_SECONDS" docker inspect "$cid" >/dev/null || fail "$label inspection failure"
    jq -e --argjson status "$status" '.[0].State | .Status == "exited" and .ExitCode == $status' \
        "$EVID/$label-state.log" >/dev/null || fail "$label did not finish with the reported status"
    timed "$label-remove" "$DISCOVERY_SECONDS" docker rm -f "$cid" >/dev/null || fail "$label infrastructure failure"
    return "$status"
}

command -v python3 >/dev/null 2>&1 || fail 'python3 required'
# Every host-side bound goes through host-watchdog.py, so no host `timeout` is
# required; `timeout` is only used inside the check containers.
timed daemon "$DISCOVERY_SECONDS" docker version -f '{{.Server.Arch}}'
DAEMON_ARCH=$(cat "$EVID/daemon.log")
PLATFORM=${PLATFORM:-linux/$DAEMON_ARCH}
record source_sha "$(git rev-parse HEAD)"
if [ -n "$(git status --porcelain)" ]; then record dirty yes; else record dirty no; fi
record platform "$PLATFORM"
record phases "$PHASES"
record run_id "$RUN_ID"
record daemon_arch "$DAEMON_ARCH"
record uname_m "$(uname -m)"
record parent_ref "$PARENT"
case "$PLATFORM" in linux/amd64 | linux/arm64) ;; *) fail 'platform must be linux/amd64 or linux/arm64' ;; esac
[ "$PLATFORM" = "linux/$DAEMON_ARCH" ] || fail "native daemon required: requested $PLATFORM, daemon linux/$DAEMON_ARCH; emulation refused"
if [ "${GITHUB_ACTIONS:-}" = true ]; then
    case "$PLATFORM:$(uname -m)" in linux/amd64:x86_64 | linux/arm64:aarch64) ;; *) fail 'runner is not requested native architecture' ;; esac
fi
timed context "$DISCOVERY_SECONDS" docker context show
BUILDER=$(cat "$EVID/context.log")
record builder "$BUILDER"
timed builder-guard "$BUILDER_GUARD_SECONDS" bash script/test/require-daemon-builder.sh
timed builder "$DISCOVERY_SECONDS" docker buildx inspect "$BUILDER"
record buildkit_version "$(awk '/BuildKit version:/ {print $NF}' "$EVID/builder.log")"
if [ "$(uname -s)" = Linux ]; then
    record apparmor_restrict_unprivileged_userns "$(sysctl -n kernel.apparmor_restrict_unprivileged_userns 2>/dev/null || echo absent)"
fi
case "$PARENT" in *@sha256:*) timed pull "$PULL_SECONDS" docker pull --platform "$PLATFORM" "$PARENT" ;; esac
timed parent-inspect "$DISCOVERY_SECONDS" docker image inspect "$PARENT"
record parent_digest "$(jq -c '.[0].RepoDigests' "$EVID/parent-inspect.log")"
PARENT_PLATFORM=$(jq -r '.[0] | .Os + "/" + .Architecture' "$EVID/parent-inspect.log")
record parent_platform "$PARENT_PLATFORM"
record parent_pnpm_label "$(jq -c '.[0].Config.Labels | with_entries(select(.key | test("pnpm"; "i")))' "$EVID/parent-inspect.log")"
[ "$PARENT_PLATFORM" = "$PLATFORM" ] || fail 'parent platform mismatch'
pass
CURRENT=P
run_check parent "$PARENT" -- sh /agent-vm-test/checks.sh parent
pass
CURRENT=B
IMAGE="agent-vm-chrome-check:$RUN_ID-default"
timed build "$BUILD_SECONDS" docker build --builder "$BUILDER" --platform "$PLATFORM" --progress=plain --label "$LABEL" \
    --build-arg "BASE_IMAGE=$PARENT" -t "$IMAGE" examples/layers/chrome-devtools
timed image-inspect "$DISCOVERY_SECONDS" docker image inspect "$IMAGE"
record image_id "$(jq -r '.[0].Id' "$EVID/image-inspect.log")"
IMAGE_PLATFORM=$(jq -r '.[0] | .Os + "/" + .Architecture' "$EVID/image-inspect.log")
record image_platform "$IMAGE_PLATFORM"
[ "$IMAGE_PLATFORM" = "$PLATFORM" ] || fail 'image platform mismatch'
pass
CURRENT=S
run_check structure "$IMAGE" -- sh /agent-vm-test/checks.sh structure
record chromium_version "$(head -n 1 "$EVID/structure.log")"
pass
CURRENT=I
run_check identity-root "$IMAGE" -e HOME=/root -e USER=root -e LOGNAME=root -- /usr/local/bin/agent-vm-chrome-mcp sh /agent-vm-test/checks.sh as-chrome
run_check identity-numeric "$IMAGE" --user 12345:12345 -e HOME=/tmp -- /usr/local/bin/agent-vm-chrome-mcp sh /agent-vm-test/checks.sh as-numeric
pass
CURRENT=N
run_check nss-root "$IMAGE" --test-ca --tmpfs /home/chrome:uid=9999,gid=9999,mode=0755 -- /usr/local/bin/agent-vm-chrome-mcp sh /agent-vm-test/checks.sh nss
! grep -q 'agent-vm-chrome-mcp: warning' "$EVID/nss-root.log" || fail 'root NSS warning'
run_check nss-numeric "$IMAGE" --test-ca --user 12345:12345 -e HOME=/tmp -- /usr/local/bin/agent-vm-chrome-mcp sh /agent-vm-test/checks.sh nss
! grep -q 'agent-vm-chrome-mcp: warning' "$EVID/nss-numeric.log" || fail 'numeric NSS warning'
pass
CURRENT=W
run_check browser-numeric "$IMAGE" --user 12345:12345 -e HOME=/tmp -- sh /agent-vm-test/checks.sh browser
run_check browser-root "$IMAGE" -- sh /agent-vm-test/checks.sh browser-root
pass
CURRENT=D
# Expected-negative checks must still run their shell with errexit enabled.
for detector in D1 D2 D3; do
    case "$detector" in
        D1) image=$IMAGE; check=parent ;;
        D2) image=$PARENT; check=structure ;;
        D3) image=$IMAGE; check=as-chrome ;;
    esac
    status=0
    run_check "$detector" "$image" -- sh /agent-vm-test/checks.sh "$check" || status=$?
    [ "$status" -ne 0 ] || fail "detector $detector passed vacuously"
    [ "$status" -ne 124 ] && [ "$status" -ne 137 ] || fail "detector $detector timed out"
done
pass
CURRENT=$FINAL_PHASE
for phase in $PHASES; do
    if [ "$phase" != "$FINAL_PHASE" ]; then
        grep -qx "$phase PASS" "$EVID/verdicts.txt" || fail "missing passing phase $phase"
    fi
done
pass
