#!/usr/bin/env bash
# Restricted-egress smoke for one image-declared network request (ADR 0014).
#
# It reads the request label off a BUILT image, turns it into the addon config the
# installer gate already uses, and runs the layer's own fixture command in a
# container whose only route out is that proxy. The claim proved is narrow and
# executable: every TLS destination the command reached was declared, and
# dropping one that was used makes the command fail. It does NOT prove that every
# declared host is used.
#
# The proxy runs the OPT-IN `enforce_tls_destination` addon mode, so a CONNECT
# authority, an SNI or a Host that disagree are refused before any upstream
# connection, and `UPSTREAM` lines in the log are the proof of what was reached.
#
# Usage: network-requests-egress.sh --image IMAGE --layer NAME --platform P [--evidence-dir DIR]
#        network-requests-egress.sh --self-test
#        network-requests-egress.sh --print-budget
#        network-requests-egress.sh --print-owned-files
# Status: 0 every step passed, evidence was written and cleanup succeeded; 1 any
#         step, evidence write or cleanup failed (a step's own timeout included);
#         2 usage; 128+N on an inherited signal.
set -euo pipefail

ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
cd "$ROOT"
DOCKER=${DOCKER:-docker}
WATCHDOG="$ROOT/script/test/host-watchdog.py"
FIXTURES="$ROOT/script/test/fixtures/network-requests"
REFERENCE="$ROOT/script/test/fixtures/installer-egress/mitmproxy-image.ref"
ADDON="$ROOT/script/test/fixtures/installer-egress/addon.py"

# --- time budget --------------------------------------------------------------
# One deadline for the whole run: every step budget is a command bound plus the
# two 1-second escalation windows its watchdog can take, and the steps sum to the
# active deadline. The finalizer gets a fresh reserve, so a run that spends its
# whole budget and then fails can still publish and clean up.
INSPECT_SECONDS=32             # docker image inspect (30) + windows
CONFIG_SECONDS=12              # egress-config (10) + windows
PULL_SECONDS=182               # docker pull (180) + windows
NETWORKS_SECONDS=44            # network create --internal (20), network create (20), windows
NETWORK_CREATE_SECONDS=20      # each create; the step's two of them plus 4 windows is 44
PROXY_SECONDS=116              # run -d (30) + network connect (20) + readiness (60), windows
DESTINATIONS_SECONDS=62        # the destination probe container (60) + windows
STRICT_SECONDS=122             # the smoke container (120) + windows
RESTART_SECONDS=138            # rm -f (20) + run -d (30) + connect (20) + readiness (60), windows
DENIAL_SECONDS=122             # the smoke container (120) + windows
ACTIVE_SECONDS=$((INSPECT_SECONDS + CONFIG_SECONDS + PULL_SECONDS + NETWORKS_SECONDS \
    + PROXY_SECONDS + DESTINATIONS_SECONDS + STRICT_SECONDS + RESTART_SECONDS + DENIAL_SECONDS))
FINALIZE_SECONDS=90            # logs, evidence copies, removals, scratch, on its own deadline
MAX_SECONDS=$((ACTIVE_SECONDS + FINALIZE_SECONDS + 10))

STEPS='SETUP INSPECT CONFIG PULL NETWORKS PROXY DESTINATIONS STRICT RESTART DENIAL'
PROXY_READY_SECONDS=60
READINESS_CLIENT_SECONDS=20
POLL_SECONDS=1                 # between readiness polls; the self-test shrinks it
# `evidence-failure.txt` is first: allocation clears and creates it before it
# touches anything else, so a failure recorded during allocation is not erased by
# a later line of that same loop.
RUNNER_OWNED_FILES='evidence-failure.txt steps.txt step-status.log summary.txt
cleanup.txt durations.txt setup.err egress-destinations.log egress-strict.log
egress-denial.log egress-strict.out egress-denial.out egress-full.log proxy.log
proxy-start-1.log proxy-start-2.log transcript.log scratch.txt destinations.txt
image-inspect.json'

usage() {
    echo 'usage: network-requests-egress.sh --image IMAGE --layer NAME --platform linux/amd64|linux/arm64 [--evidence-dir DIR]' >&2
    echo '       network-requests-egress.sh --self-test | --print-budget | --print-owned-files' >&2
    exit 2
}
fail() { echo "network-requests-egress: $*" >&2; exit 1; }

# The digest-pinned mitmproxy reference is one data file, shared with the
# installer gate. This reader is COPIED from
# `script/test/shipped-installer-network.sh`, deliberately and not shared: a
# helper file would be one more input every CI route has to know about, and each
# copy lives in a file that is already routed. A change here is reviewed against
# the other copy (ADR 0014).
read_proxy_image() { # FILE
    local line
    [ -f "$1" ] || return 1
    line=$(head -n 1 "$1") || return 1
    [ "$(wc -l <"$1")" -eq 1 ] || return 1
    [[ "$line" =~ ^mitmproxy/mitmproxy@sha256:[0-9a-f]{64}$ ]] || return 1
    printf '%s\n' "$line"
}

# --- self-test seams ----------------------------------------------------------
# The scale, the ref override and the fixture override exist only inside
# --self-test, so the timeout and signal cases do not wait minutes and the SETUP
# cases can point at defective inputs. Production runs read none of them.
SELF_TEST=0
SCALE=1
[ -n "${NETWORK_REQUESTS_EGRESS_SELF_TEST:-}" ] && SELF_TEST=1
if [ "$SELF_TEST" = 1 ]; then
    SCALE=${NETWORK_REQUESTS_EGRESS_TEST_SCALE:-1}
    [ "$SCALE" -ge 1 ] || SCALE=1
    REFERENCE=${NETWORK_REQUESTS_EGRESS_TEST_REF:-$REFERENCE}
    FIXTURES=${NETWORK_REQUESTS_EGRESS_TEST_FIXTURES:-$FIXTURES}
    # The scaled readiness bound is a couple of seconds, so the poll interval has
    # to shrink with it or a single attempt would use the whole budget.
    POLL_SECONDS=0.1
fi

scale_seconds() { # SECONDS -> the bound this run may actually spend on one command
    local seconds=$1 scaled
    if [ "$SCALE" -le 1 ]; then
        printf '%s\n' "$seconds"
        return 0
    fi
    scaled=$(( (seconds + SCALE - 1) / SCALE ))
    [ "$scaled" -ge 1 ] || scaled=1
    printf '%s\n' "$scaled"
}

# --- log segments -------------------------------------------------------------
# The cumulative egress log is never truncated: markers scope each step's segment,
# and finalize copies a partial segment so a failed step keeps its evidence.
mark_log() { wc -l <"$TMP/logs/egress.log" | tr -d ' '; }
since_marker() { # MARKER
    tail -n "+$(( $1 + 1 ))" "$TMP/logs/egress.log" 2>/dev/null || true
}

# --- bounded commands ---------------------------------------------------------
# No Docker command runs unbounded, including the removals, `logs`, `ps` and
# `inspect` in the finalizer. Every one is clamped to what is left of the current
# deadline (active, or the finalizer's own), so a slow child cannot borrow the
# next phase's budget.
record_command() { # LABEL COMMAND...
    local label=$1
    [ -n "$TMP" ] || return 0
    shift
    { printf '+ %s' "$label"; printf ' %q' "$@"; printf '\n'; } >>"$TMP/transcript.log" ||
        evidence_failure 'cannot write transcript.log'
}

bounded_into() { # DESTINATION LABEL SECONDS COMMAND...
    local destination=$1 label=$2 seconds status=0 start
    local -a pipe_status=()
    seconds=$(bounded_seconds "$3")
    shift 3
    if [ "$seconds" -le 0 ]; then
        # 124 for the same reason the watchdog uses it: there was no time left, and
        # a caller classifying the outcome reads that as a timeout rather than as
        # a command that ran and failed. The durations line says which it was.
        duration "$label" 0 skipped-deadline
        return 124
    fi
    record_command "$label" "$@"
    start=$SECONDS
    if [ "$destination" = '-' ]; then
        python3 "$WATCHDOG" --grace-seconds 1 "$seconds" "$@" 2>&1 |
            tee -a "$TMP/transcript.log" >&2
        pipe_status=("${PIPESTATUS[@]}")
        status=${pipe_status[0]}
        [ "${pipe_status[1]}" -eq 0 ] ||
            evidence_failure 'cannot append to transcript.log'
    else
        if python3 "$WATCHDOG" --grace-seconds 1 "$seconds" "$@" \
            >"$destination" 2>>"$TMP/transcript.log"; then
            status=0
        else
            status=$?
        fi
    fi
    duration "$label" "$((SECONDS - start))" "$status"
    return "$status"
}

# The seconds a child may actually use: its own bound, or what is left of the
# deadline in force if that is less. A child given more than this can outlive the
# budget this run publishes, which is how a step allocation stops meaning
# anything. `LIMIT_DEADLINE` is the whole run's; `READY_DEADLINE` is the nested
# readiness wait, which is a bound inside the PROXY step and not an exemption
# from it.
bounded_seconds() { # WANTED
    local wanted remaining
    wanted=$(scale_seconds "$1")
    remaining=$((LIMIT_DEADLINE - SECONDS - 2))
    if [ -n "$READY_DEADLINE" ] && [ "$((READY_DEADLINE - SECONDS))" -lt "$remaining" ]; then
        remaining=$((READY_DEADLINE - SECONDS))
    fi
    if [ "$wanted" -lt "$remaining" ]; then
        printf '%s\n' "$wanted"
    elif [ "$remaining" -gt 0 ]; then
        printf '%s\n' "$remaining"
    else
        printf '%s\n' 0
    fi
}

bounded() { # LABEL SECONDS COMMAND...  (output goes to the transcript)
    bounded_into - "$@"
}

bounded_capture() { # FILE LABEL SECONDS COMMAND...
    local file=$1
    shift
    bounded_into "$file" "$@"
}

# --- step status --------------------------------------------------------------
# Evidence is a product of this run (R10b): a lost write is a failed run, so every
# append this runner owns is checked and the flag it sets is what the final
# verdict reads. `evidence_failure` is the one place that records the loss; it
# cannot report its own failure, so a read-only evidence directory still fails
# loudly on stderr and in the exit status.
EVIDENCE_FAILED=0

evidence_failure() {
    EVIDENCE_FAILED=1
    printf '%s\n' "$1" >>"$EVID/evidence-failure.txt" 2>/dev/null || true
    echo "network-requests-egress: evidence: $1" >&2
}

# A duration line is the only record of how a step spent its budget.
duration() { # LABEL SECONDS STATUS
    printf '%s seconds=%s status=%s\n' "$1" "$2" "$3" >>"$EVID/durations.txt" ||
        evidence_failure "cannot record $1 in durations.txt"
}

record_step() {
    printf '%s %s\n' "$1" "$2" >>"$EVID/step-status.log" ||
        evidence_failure "cannot record the $1 step"
}

last_status() { # STEP
    local step=$1 found=NOT-RUN line
    if [ -f "$EVID/step-status.log" ]; then
        while IFS= read -r line; do
            case "$line" in
                "$step "*) found=${line#* } ;;
            esac
        done <"$EVID/step-status.log"
    fi
    printf '%s\n' "$found"
}

write_steps() {
    local step status=0
    : >"$EVID/steps.txt" || status=1
    if [ "$status" -eq 0 ]; then
        for step in $STEPS; do
            printf '%s %s\n' "$step" "$(last_status "$step")" >>"$EVID/steps.txt" || status=1
        done
    fi
    [ "$status" -eq 0 ] || evidence_failure 'cannot write steps.txt'
    return "$status"
}

# Every destination this runner owns is cleared and created before the first
# Docker command. A directory left at one of those names, or an unwritable
# evidence tree, then fails here instead of swallowing a write later and leaving
# a passing run with no evidence of it. One blocked name does not stop the loop:
# returning at the first failure leaves every later name holding the earlier
# run's verdict, which is the `strict=PASS` beside this run's `SETUP FAIL` this
# allocation exists to prevent.
allocate_evidence() {
    local name failed=0
    for name in $RUNNER_OWNED_FILES; do
        if ! rm -f "$EVID/$name"; then
            evidence_failure "cannot clear $EVID/$name"
            failed=1
            continue
        fi
        if ! : >"$EVID/$name"; then
            evidence_failure "cannot create $EVID/$name"
            failed=1
        fi
    done
    return "$failed"
}

copy_segment() { # START END NAME  (0 as END means "to the end of the log")
    local start=$1 end=$2 name=$3
    case "$start" in
        '' | *[!0-9]*) start=0 ;;
    esac
    case "$end" in
        '' | *[!0-9]*) end=0 ;;
    esac
    if [ -d "$EVID/$name" ]; then
        evidence_failure "$name is a directory"
        return 1
    fi
    if [ "$end" -gt "$start" ]; then
        if ! sed -n "$((start + 1)),${end}p" "$TMP/logs/egress.log" >"$EVID/$name" \
            2>>"$EVID/evidence-failure.txt"; then
            evidence_failure "cannot write $name"
            return 1
        fi
        return 0
    fi
    if ! since_marker "$start" >"$EVID/$name" 2>>"$EVID/evidence-failure.txt"; then
        evidence_failure "cannot write $name"
        return 1
    fi
}
# `cp file DIRECTORY` nests the file instead of failing, so a destination that
# stopped being a regular file would publish nothing under the promised name and
# still exit 0. The destination is removed first, which fails on a directory.
publish_file() { # SOURCE DESTINATION NAME
    if ! { rm -f "$2" && cp "$1" "$2"; }; then
        evidence_failure "cannot write $3"
        return 1
    fi
}

# --- stream assertions --------------------------------------------------------
# Pure functions over an egress-log segment and the declared host list, so the
# hermetic self-test exercises exactly what a real run judges.
declared_contains() { # HOST
    case ",$DECLARED," in
        *",$1,"*) return 0 ;;
    esac
    return 1
}

allowed_hosts() { # SEGMENT
    sed -nE 's#^ALLOW https://([^/:]+)/.*#\1#p' "$1" | sort -u
}

used_hosts() { # SEGMENT
    sed -nE 's#^UPSTREAM https://([^/:]+)/.*#\1#p' "$1" | sort -u
}

assert_strict_log() { # SEGMENT
    local segment=$1 problems='' host
    if ! grep -q '^ALLOW https://' "$segment"; then
        problems="$problems no-allow"
    fi
    if grep -q '^DENY ' "$segment"; then
        problems="$problems deny"
    fi
    if grep -qE '^(ALLOW-LEARNED|LEARN) ' "$segment"; then
        problems="$problems learned"
    fi
    for host in $(allowed_hosts "$segment") $(used_hosts "$segment"); do
        declared_contains "$host" || problems="$problems undeclared:$host"
    done
    if [ -n "$problems" ]; then
        echo "the strict egress segment is not a pass:$problems" >&2
        return 1
    fi
}

assert_denial_log() { # SEGMENT HOST
    local segment=$1 host=$2 problems=''
    if ! grep -qxF "DENY connect-unlisted https://$host/" "$segment"; then
        problems="$problems no-connect-denial"
    fi
    if grep -qxF "UPSTREAM https://$host/" "$segment"; then
        problems="$problems upstream-contact"
    fi
    if [ -n "$problems" ]; then
        echo "the denial segment does not prove the gate is load-bearing:$problems" >&2
        return 1
    fi
}

assert_destinations() { # PROBE_OUTPUT SEGMENT
    local output=$1 segment=$2 problems=''
    case_line() { grep -q "^case=$1 .*$2" "$output"; }
    case_line connect-unlisted 'connect_status=403' || problems="$problems connect-unlisted-status"
    grep -qxF 'DENY connect-unlisted https://example.com/' "$segment" ||
        problems="$problems connect-unlisted-log"
    if ! case_line sni-unlisted 'blocked_by_proxy=yes' && ! case_line sni-unlisted 'tls=failed'; then
        problems="$problems sni-unlisted"
    fi
    if ! grep -qE "^DENY (sni-mismatch https://example\.com/|server-unlisted https://$DECLARED_FIRST/)$" \
        "$segment"; then
        problems="$problems sni-unlisted-log"
    fi
    case_line host-unlisted 'blocked_by_proxy=yes' || problems="$problems host-unlisted"
    grep -qxF 'DENY authority-mismatch https://example.com/' "$segment" ||
        problems="$problems host-unlisted-log"
    case_line positive 'tls=ok' || problems="$problems positive-tls"
    case_line positive 'blocked_by_proxy=no' || problems="$problems positive-allowed"
    grep -qxF "ALLOW https://$DECLARED_FIRST/" "$segment" || problems="$problems positive-allow"
    grep -qxF "UPSTREAM https://$DECLARED_FIRST/" "$segment" || problems="$problems positive-upstream"
    # The unlisted destination must never have been contacted: that line is the
    # proof an upstream connection happened.
    if grep -qxF 'UPSTREAM https://example.com/' "$segment"; then
        problems="$problems unlisted-was-contacted"
    fi
    if [ -n "$problems" ]; then
        echo "the destination controls did not hold:$problems" >&2
        return 1
    fi
}

# --- proxy lifecycle ----------------------------------------------------------
start_proxy() { # CONFIG
    local config=$1
    bounded_capture "$TMP/proxy-run.out" proxy-run 30 "$DOCKER" run -d \
        --name "$RUN_ID-proxy" --network "$RUN_ID-external" \
        --cap-drop ALL --security-opt no-new-privileges --user "$(id -u):$(id -g)" \
        -v "$TMP/certs:/certs" -v "$config:/addon/config.json:ro" \
        -v "$ADDON:/addon/addon.py:ro" -v "$TMP/logs:/logs" \
        -e EGRESS_CONFIG=/addon/config.json -e EGRESS_LOG=/logs/egress.log \
        -e EGRESS_MODE=strict \
        --entrypoint mitmdump "$MITM_IMAGE" \
        --set confdir=/certs --listen-host 0.0.0.0 --listen-port 8080 \
        --set connection_strategy=lazy -s /addon/addon.py \
        --set console_eventlog_verbosity=error || return 1
    PROXY_CID=$(head -n 1 "$TMP/proxy-run.out")
    [ -n "$PROXY_CID" ] || { echo 'the proxy container printed no ID' >&2; return 1; }
    bounded proxy-connect 20 "$DOCKER" network connect "$RUN_ID-internal" "$RUN_ID-proxy"
}

# Readiness is not "the CA file exists": the CA persists across a restart, so its
# presence says nothing about the current process. The current container must be
# running, a client on the internal network must reach it, and the same container
# must still be running afterwards.
#
# The wait is a bound of its own INSIDE the PROXY step, so it is clamped to the
# active deadline and every call it makes is clamped to it as well: otherwise a
# wait entered near the active deadline could poll for its whole sixty seconds
# after it, and a lenient per-call bound could return success long after the
# readiness budget was gone. `bounded_seconds` is what applies the clamp, and the
# success path re-checks the deadline it was given.
wait_proxy_ready() { # N
    local n=$1 deadline state='' reason=timeout status=0
    deadline=$((SECONDS + $(scale_seconds "$PROXY_READY_SECONDS")))
    [ "$deadline" -gt "$LIMIT_DEADLINE" ] && deadline=$LIMIT_DEADLINE
    READY_DEADLINE=$deadline
    while [ "$SECONDS" -lt "$deadline" ]; do
        if [ -s "$TMP/certs/mitmproxy-ca-cert.pem" ]; then
            bounded_capture "$TMP/proxy-inspect.out" "proxy-inspect-$n" 10 \
                "$DOCKER" inspect -f '{{.Id}} {{.State.Running}}' "$RUN_ID-proxy" || true
            state=$(head -n 1 "$TMP/proxy-inspect.out" 2>/dev/null || true)
            if [ "$state" = "$PROXY_CID true" ]; then
                READY_SEQ=$((READY_SEQ + 1))
                status=0
                bounded_capture "$TMP/ready-$READY_SEQ.out" "ready-$READY_SEQ" \
                    "$READINESS_CLIENT_SECONDS" "$DOCKER" run --rm \
                    --name "$RUN_ID-ready-$READY_SEQ" --platform "$PLATFORM" \
                    --network "$RUN_ID-internal" --user 12345:23456 --cap-drop ALL \
                    --security-opt no-new-privileges \
                    --entrypoint /bin/bash "$IMAGE" -c \
                    'for i in $(seq 1 40); do (exec 3<>/dev/tcp/"$1"/8080) 2>/dev/null && exit 0; sleep 0.5; done; exit 1' \
                    _ "$RUN_ID-proxy" || status=$?
                if [ "$status" -eq 0 ]; then
                    bounded_capture "$TMP/proxy-inspect.out" "proxy-inspect-after-$n" 10 \
                        "$DOCKER" inspect -f '{{.Id}} {{.State.Running}}' "$RUN_ID-proxy" || true
                    state=$(head -n 1 "$TMP/proxy-inspect.out" 2>/dev/null || true)
                    if [ "$state" = "$PROXY_CID true" ]; then
                        if [ "$SECONDS" -lt "$deadline" ]; then
                            READY_DEADLINE=''
                            return 0
                        fi
                        # The cycle that looks healthy finished after the budget
                        # this wait published, so it is a timeout and not a pass.
                        reason=timeout
                        break
                    fi
                    reason=exited
                elif [ "$status" -eq 124 ]; then
                    reason=timeout
                else
                    reason=refused
                fi
            elif [ -n "$state" ]; then
                reason=exited
            fi
        fi
        sleep "$POLL_SECONDS"
    done
    READY_DEADLINE=''
    bounded_capture "$EVID/proxy-start-$n.log" "proxy-logs-$n" 10 "$DOCKER" logs "$RUN_ID-proxy" || true
    bounded proxy-inspect-failure "10" "$DOCKER" inspect "$RUN_ID-proxy" || true
    echo "proxy_ready_$n=$reason" >&2
    return 1
}

# --- steps --------------------------------------------------------------------
step_setup() {
    local fixture
    if [ -z "$LAYER" ] || [ -z "$IMAGE" ] || [ -z "$PLATFORM" ]; then
        echo 'missing --image, --layer or --platform' >&2
        return 1
    fi
    fixture="$FIXTURES/$LAYER"
    if [ -L "$fixture" ] || [ ! -d "$fixture" ]; then
        echo "fixture $fixture is not a real directory" >&2
        return 1
    fi
    if [ ! -f "$fixture/smoke.sh" ]; then
        echo "fixture $fixture has no smoke.sh" >&2
        return 1
    fi
    if ! MITM_IMAGE=$(read_proxy_image "$REFERENCE"); then
        echo "$REFERENCE must hold one digest-pinned mitmproxy reference" >&2
        return 1
    fi
    mkdir -p "$ROOT/target" || return 1
    TMP=$(mktemp -d "$ROOT/target/network-requests.XXXXXX") || return 1
    # Recorded in the evidence, not just printed, so a reviewer of a failed run can
    # find what cleanup kept and the self-test can check the directory THIS run
    # created instead of every directory some other run may have left.
    printf '%s\n' "$TMP" >"$EVID/scratch.txt" || return 1
    mkdir -p "$TMP/certs" "$TMP/logs" || return 1
    : >"$TMP/logs/egress.log" || return 1
    : >"$TMP/transcript.log" || return 1
}

step_inspect() {
    bounded_capture "$EVID/image-inspect.json" inspect "$INSPECT_SECONDS" \
        "$DOCKER" image inspect "$IMAGE"
}

step_config() {
    bounded_capture "$TMP/strict.json" config "$CONFIG_SECONDS" \
        python3 "$ROOT/script/test/network_requests.py" egress-config \
        --inspect "$EVID/image-inspect.json" --key "org.agent-vm.network-requests.image.$LAYER" ||
        return 1
    DECLARED=$(jq -r '.allow[].host' "$TMP/strict.json" | sort -u | paste -sd, -)
    DECLARED_FIRST=$(jq -r '.allow[0].host' "$TMP/strict.json")
    [ -n "$DECLARED_FIRST" ] || { echo 'the label declares no host' >&2; return 1; }
}

step_pull() {
    bounded pull "$PULL_SECONDS" "$DOCKER" pull --platform "$PLATFORM" "$MITM_IMAGE"
}

step_networks() {
    # Each create gets its own 20 seconds, not the step's 44: a slow first create
    # must not be able to spend the budget the second one needs.
    bounded network-internal "$NETWORK_CREATE_SECONDS" "$DOCKER" network create --internal \
        "$RUN_ID-internal" &&
        bounded network-external "$NETWORK_CREATE_SECONDS" "$DOCKER" network create "$RUN_ID-external"
}

step_proxy() {
    start_proxy "$TMP/strict.json" && wait_proxy_ready 1
}

step_destinations() {
    MARK_DESTINATIONS=$(mark_log)
    bounded_capture "$EVID/destinations.txt" destinations "$DESTINATIONS_SECONDS" \
        "$DOCKER" run --rm --name "$RUN_ID-destinations" --platform "$PLATFORM" \
        --network "$RUN_ID-internal" --user 12345:23456 --cap-drop ALL \
        --security-opt no-new-privileges -e HOME=/tmp/home \
        -v "$TMP/certs/mitmproxy-ca-cert.pem:/egress/ca.pem:ro" \
        -v "$FIXTURES/$LAYER:/egress/fixture:ro" \
        -v "$FIXTURES/run-in-image.sh:/egress/run-in-image.sh:ro" \
        -v "$FIXTURES/destination-probe.py:/egress/destination-probe.py:ro" \
        --entrypoint /bin/bash "$IMAGE" /egress/run-in-image.sh destinations \
        --proxy "$RUN_ID-proxy:8080" \
        --case connect-unlisted example.com example.com "$DECLARED_FIRST" \
        --case sni-unlisted "$DECLARED_FIRST" example.com "$DECLARED_FIRST" \
        --case host-unlisted "$DECLARED_FIRST" "$DECLARED_FIRST" example.com \
        --case positive "$DECLARED_FIRST" "$DECLARED_FIRST" "$DECLARED_FIRST" || return 1
    since_marker "$MARK_DESTINATIONS" >"$TMP/logs/destinations.segment"
    assert_destinations "$EVID/destinations.txt" "$TMP/logs/destinations.segment"
}

smoke_run() { # LABEL
    local label=$1
    bounded_capture "$TMP/$label.out" "$label" "$STRICT_SECONDS" \
        "$DOCKER" run --rm --name "$RUN_ID-smoke" --platform "$PLATFORM" \
        --network "$RUN_ID-internal" --user 12345:23456 --cap-drop ALL \
        --security-opt no-new-privileges -e HOME=/tmp/home \
        -e "HTTPS_PROXY=http://$RUN_ID-proxy:8080" -e "https_proxy=http://$RUN_ID-proxy:8080" \
        -e "HTTP_PROXY=http://$RUN_ID-proxy:8080" -e "http_proxy=http://$RUN_ID-proxy:8080" \
        -e NO_PROXY= -e no_proxy= \
        -v "$TMP/certs/mitmproxy-ca-cert.pem:/egress/ca.pem:ro" \
        -v "$FIXTURES/$LAYER:/egress/fixture:ro" \
        -v "$FIXTURES/run-in-image.sh:/egress/run-in-image.sh:ro" \
        -v "$FIXTURES/destination-probe.py:/egress/destination-probe.py:ro" \
        --entrypoint /bin/bash "$IMAGE" /egress/run-in-image.sh smoke
}

step_strict() {
    MARK_STRICT=$(mark_log)
    smoke_run strict || return 1
    since_marker "$MARK_STRICT" >"$TMP/logs/strict.segment"
    assert_strict_log "$TMP/logs/strict.segment" || return 1
    USED=$(used_hosts "$TMP/logs/strict.segment" | paste -sd, -)
    [ -n "$USED" ] || { echo 'the command contacted no declared host' >&2; return 1; }
}

step_restart() {
    DENIAL_FIRST=${USED%%,*}
    jq --arg host "$DENIAL_FIRST" \
        '{allow: [.allow[] | select(.host != $host)], https_only: .https_only,
          learn_redirects: .learn_redirects,
          enforce_tls_destination: .enforce_tls_destination}' \
        "$TMP/strict.json" >"$TMP/denial.json" || return 1
    bounded proxy-rm 20 "$DOCKER" rm -f "$RUN_ID-proxy" || return 1
    start_proxy "$TMP/denial.json" || return 1
    wait_proxy_ready 2
}

step_denial() {
    MARK_DENIAL=$(mark_log)
    if smoke_run denial; then
        echo "the command succeeded without its used host $DENIAL_FIRST" >&2
        return 1
    fi
    since_marker "$MARK_DENIAL" >"$TMP/logs/denial.segment"
    assert_denial_log "$TMP/logs/denial.segment" "$DENIAL_FIRST"
}

# --- finalizer ----------------------------------------------------------------
remove_run_containers() {
    local -a ids=()
    local id
    # Nothing was created before SETUP opened the scratch directory, so there is
    # no run-scoped container to look for (and no place to record the look).
    [ -n "$TMP" ] || return 0
    if ! bounded_capture "$TMP/ps.out" ps 10 "$DOCKER" ps -aq --filter "name=^$RUN_ID-"; then
        return 1
    fi
    while IFS= read -r id; do
        [ -n "$id" ] && ids+=("$id")
    done <"$TMP/ps.out"
    if [ "${#ids[@]}" -eq 0 ]; then
        return 0
    fi
    bounded rm 20 "$DOCKER" rm -f "${ids[@]}"
}

remove_network() { # NAME
    [ -n "$TMP" ] || return 0
    bounded network-rm 10 "$DOCKER" network rm "$1"
}

remove_scratch() {
    case "$TMP" in
        "$ROOT"/target/network-requests.?*) rm -rf "$TMP" ;;
        '') : ;;
        *) evidence_failure "refusing to remove scratch $TMP"; return 1 ;;
    esac
}

image_id() {
    # `docker image inspect` was never run, or wrote nothing this run could read:
    # an empty file makes `jq` print nothing and exit 0, which would leave a blank
    # field claiming an image nobody identified.
    local id
    id=$(jq -r '.[0].Id // empty' "$EVID/image-inspect.json" 2>/dev/null) || true
    printf '%s\n' "${id:-unknown}"
}

write_summary() { # EVIDENCE_STATUS
    local evidence=$1 text
    # Built first and written once: a redirect that fails halfway would leave a
    # summary claiming half a run, and appending stderr into the evidence tree
    # must not be able to break the write itself.
    text=$(printf 'layer=%s\nplatform=%s\nimage_id=%s\nmitm_image=%s\ndeclared=%s\nused=%s\nsetup=%s\ndestinations=%s\nstrict=%s\ndenial=%s\nevidence=%s\n' \
        "$LAYER" "$PLATFORM" "$(image_id)" \
        "${MITM_IMAGE:-unknown}" "${DECLARED:-unknown}" "${USED:-unknown}" \
        "$([ "$(last_status SETUP)" = PASS ] && echo PASS || echo "FAIL: ${SETUP_FAILURE:-unknown}")" \
        "$(last_status DESTINATIONS)" "$(last_status STRICT)" "$(last_status DENIAL)" \
        "$evidence")
    if ! printf '%s\n' "$text" >"$EVID/summary.txt"; then
        evidence_failure 'cannot write summary.txt'
        return 1
    fi
}

finalize() {
    local status=$? copy_status=0 cleanup_status=0 evidence=ok label
    trap - EXIT
    trap '' INT TERM HUP
    LIMIT_DEADLINE=$((SECONDS + FINALIZE_SECONDS))
    READY_DEADLINE=''
    if [ "$status" -ne 0 ] && [ -n "$CURRENT" ]; then
        if [ "$(last_status "$CURRENT")" != PASS ]; then
            record_step "$CURRENT" FAIL
        fi
    fi
    if ! write_steps; then
        echo 'network-requests-egress: cannot write steps.txt' >&2
        status=1
    fi
    if [ -n "${TMP:-}" ] && [ -d "$TMP" ]; then
        copy_segment "${MARK_DESTINATIONS:-0}" "${MARK_STRICT:-0}" egress-destinations.log || copy_status=1
        copy_segment "${MARK_STRICT:-0}" "${MARK_DENIAL:-0}" egress-strict.log || copy_status=1
        copy_segment "${MARK_DENIAL:-0}" 0 egress-denial.log || copy_status=1
        publish_file "$TMP/logs/egress.log" "$EVID/egress-full.log" egress-full.log ||
            copy_status=1
        publish_file "$TMP/transcript.log" "$EVID/transcript.log" transcript.log || copy_status=1
        # The fixture's own stdout is the diagnostic a failed run needs, and it is
        # whatever the container wrote before it exited, timed out or was
        # signalled: copied here, before the scratch directory goes, so a partial
        # output survives too.
        for label in strict denial; do
            [ -f "$TMP/$label.out" ] || continue
            publish_file "$TMP/$label.out" "$EVID/egress-$label.out" "egress-$label.out" ||
                copy_status=1
        done
    fi
    if [ -n "${PROXY_CID:-}" ]; then
        bounded_capture "$EVID/proxy.log" proxy-logs 10 "$DOCKER" logs "$PROXY_CID" ||
            { evidence_failure 'cannot capture the proxy log'; copy_status=1; }
    fi
    [ "$EVIDENCE_FAILED" -eq 0 ] || copy_status=1
    [ "$copy_status" -eq 0 ] || evidence=failed
    write_summary "$evidence" || copy_status=1
    if [ "$copy_status" -ne 0 ]; then
        evidence=failed
        status=1
    fi
    remove_run_containers || cleanup_status=1
    remove_network "$RUN_ID-internal" || cleanup_status=1
    remove_network "$RUN_ID-external" || cleanup_status=1
    if ! printf 'completed status=%s\n' "$cleanup_status" >"$EVID/cleanup.txt"; then
        evidence_failure 'cannot write cleanup.txt'
        cleanup_status=1
    fi
    if ! printf 'cleanup=%s\n' "$([ "$cleanup_status" -eq 0 ] && echo ok || echo failed)" \
        >>"$EVID/summary.txt"; then
        evidence_failure 'cannot append the cleanup verdict to summary.txt'
        cleanup_status=1
    fi
    if [ "$cleanup_status" -eq 0 ] && [ "$copy_status" -eq 0 ]; then
        remove_scratch || cleanup_status=1
    else
        printf 'network-requests-egress: scratch kept: %s\n' "${TMP:-none}" >&2
    fi
    if [ "$cleanup_status" -ne 0 ]; then
        echo 'network-requests-egress: cleanup failed' >&2
        status=1
    fi
    exit "$status"
}

# --- hermetic self-test -------------------------------------------------------
write_fake_docker() { # DESTINATION
    cat >"$1" <<'FAKE'
#!/usr/bin/env bash
# A fake docker for the runner's self-test: it records argv, models run-scoped
# containers and networks, and writes the egress-log lines its mode names into
# the log directory the proxy was given. It never claims a daemon.
set -uo pipefail
mode=${FAKE_EGRESS_MODE:-healthy}
state=${FAKE_DOCKER_STATE:?FAKE_DOCKER_STATE must be set}
declared=${FAKE_DECLARED_FIRST:-crates.io}
denied=${FAKE_DENIED_HOST:-index.crates.io}
mkdir -p "$state"
printf '%s\n' "$*" >>"$state/argv.log"
arg_after() { # FLAG ARGS...
    local flag=$1 previous='' arg
    shift
    for arg in "$@"; do
        if [ "$previous" = "$flag" ]; then printf '%s\n' "$arg"; return 0; fi
        previous=$arg
    done
}
last_arg() { local arg; for arg in "$@"; do :; done; printf '%s\n' "$arg"; }
ident() { printf 'sha256:%064d\n' "$1"; }
logdir() { cat "$state/logdir" 2>/dev/null || true; }
case "$1" in
    --version) echo 'Docker version 99.9.9, build fake' ;;
    image)
        case "$2" in
            inspect)
                # Allocation proves every destination is writable at the start. A
                # mode that takes one back mid-run proves the write itself is
                # checked instead of silently lost: the first docker command of
                # the run is this inspect.
                case "$mode" in
                    sabotage-durations)
                        rm -f "$FAKE_EVIDENCE_DIR/durations.txt"
                        mkdir "$FAKE_EVIDENCE_DIR/durations.txt" ;;
                    sabotage-transcript)
                        rm -f "$FAKE_EVIDENCE_DIR/transcript.log"
                        mkdir "$FAKE_EVIDENCE_DIR/transcript.log" ;;
                    sabotage-live-transcript)
                        tmp=$(cat "$FAKE_EVIDENCE_DIR/scratch.txt")
                        rm -f "$tmp/transcript.log"
                        mkdir "$tmp/transcript.log" ;;
                esac
                printf '[{"Id":"%s","Config":{"Labels":{"org.agent-vm.network-requests.image.rust-dev":"crates.io,index.crates.io,static.crates.io"}}}]\n' \
                    "$(ident 1)" ;;
            *) : ;;
        esac ;;
    inspect) # docker inspect -f '{{.Id}} {{.State.Running}}' NAME
        target=$(last_arg "$@")
        cid=$(cat "$state/cid.$target" 2>/dev/null || printf 'sha256:unknown')
        running=true
        runs=$(cat "$state/proxy-runs" 2>/dev/null || echo 0)
        case "$mode" in
            readiness-slow)
                sleep 0.9 ;;
            stale-ca-restart)
                # A fresh container is not yet running for its first two polls.
                # Only a not-running answer counts as a poll, so the post-readiness
                # re-inspect does not consume one.
                if [ "$runs" -ge 2 ] && [ ! -f "$state/late-ok" ]; then
                    polls=$(cat "$state/polls" 2>/dev/null || echo 0)
                    if [ "$polls" -ge 2 ]; then
                        : >"$state/late-ok"
                    else
                        polls=$((polls + 1))
                        echo "$polls" >"$state/polls"
                        running=false
                    fi
                fi ;;
            restart-cid-changed)
                [ "$runs" -lt 2 ] || cid=sha256:different ;;
        esac
        printf '%s %s\n' "$cid" "$running" ;;
    pull) echo 'fake: pulled' ;;
    network)
        case "$2" in
            create)
                [ "$mode" = network-create-slow ] && sleep 1.2
                printf '%s\n' "$(last_arg "$@")" >>"$state/networks" ;;
            connect) : ;;
            rm)
                [ "$mode" = cleanup-fails ] && exit 1
                if [ "$mode" = readiness-client-orphaned ] && [ -s "$state/running" ]; then
                    exit 1
                fi
                : ;;
            *) : ;;
        esac ;;
    run)
        shift
        detached=0 name='' logs='' certs=''
        while [ "$#" -gt 0 ]; do
            case "$1" in
                -d) detached=1; shift ;;
                --name) name=$2; shift 2 ;;
                -v)
                    case "${2##*:}" in
                        /logs) logs=${2%%:*} ;;
                        /certs) certs=${2%%:*} ;;
                    esac
                    shift 2 ;;
                --rm) shift ;;
                -e | -u | --user | --network | --cap-drop | --security-opt | --entrypoint | --platform)
                    shift 2 ;;
                *) break ;;
            esac
        done
        image=$1
        shift
        if [ "$detached" = 1 ]; then
            runs=$(cat "$state/proxy-runs" 2>/dev/null || echo 0)
            runs=$((runs + 1))
            echo "$runs" >"$state/proxy-runs"
            case "$mode" in
                proxy-start-fails) echo 'fake: cannot start the proxy' >&2; exit 125 ;;
            esac
            ident "$(printf '%s' "$name" | cksum | cut -d' ' -f1)" >"$state/cid.$name"
            printf '%s\n' "$logs" >"$state/logdir"
            # The real sidecar writes its CA into the certs mount; readiness must
            # key on the container, and this file is only what makes trust work.
            if [ -n "$certs" ]; then printf 'fake CA\n' >"$certs/mitmproxy-ca-cert.pem"; fi
            cat "$state/cid.$name"
            exit 0
        fi
        case "$name" in
            *-ready-*)
                printf 'x' >>"$state/ready-attempts"
                case "$mode" in
                    proxy-never-ready) exit 1 ;;
                    readiness-client-orphaned)
                        printf '%s\n' "$name" >>"$state/running"
                        sleep 30
                        exit 1 ;;
                    proxy-late)
                        [ "$(wc -c <"$state/ready-attempts")" -le 3 ] && exit 1
                        : ;;
                    readiness-slow)
                        # Healthy, but each poll cycle costs most of a scaled
                        # readiness budget: the wait cannot finish inside it.
                        sleep 0.9 ;;
                esac
                exit 0 ;;
            *-destinations)
                logs=$(logdir)
                printf 'case=connect-unlisted connect_status=403 tls=failed http_status=none blocked_by_proxy=yes\n'
                printf 'case=sni-unlisted connect_status=200 tls=ok http_status=403 blocked_by_proxy=yes\n'
                printf 'case=host-unlisted connect_status=200 tls=ok http_status=403 blocked_by_proxy=yes\n'
                printf 'case=positive connect_status=200 tls=ok http_status=200 blocked_by_proxy=no\n'
                {
                    if [ "$mode" != destination-gate-open ]; then
                        printf 'DENY connect-unlisted https://example.com/\n'
                    fi
                    printf 'DENY sni-mismatch https://example.com/\n'
                    printf 'DENY authority-mismatch https://example.com/\n'
                    printf 'ALLOW https://%s/\n' "$declared"
                    printf 'UPSTREAM https://%s/\n' "$declared"
                    if [ "$mode" = destination-gate-open ]; then
                        printf 'UPSTREAM https://example.com/\n'
                    fi
                } >>"$(logdir)/egress.log"
                exit 0 ;;
            *-smoke)
                logs=$(logdir)
                runs=$(cat "$state/smoke-runs" 2>/dev/null || echo 0)
                runs=$((runs + 1))
                echo "$runs" >"$state/smoke-runs"
                case "$mode" in
                    strict-deny)
                        printf 'DENY unlisted https://other.example/\n' >>"$(logdir)/egress.log"
                        exit 0 ;;
                    strict-fetch-fails)
                        printf 'REVIEW-STDOUT-ONLY-FAILURE\n'
                        exit 101 ;;
                    denial-not-load-bearing)
                        [ "$runs" -ge 2 ] && exit 0 ;;
                    timeout) printf 'FIXTURE-PARTIAL-STDOUT\n'; sleep 30; exit 0 ;;
                    signal) sleep 30; exit 0 ;;
                esac
                if [ "$runs" -eq 1 ]; then
                    {
                        printf 'ALLOW https://index.crates.io/\n'
                        printf 'ALLOW https://static.crates.io/crates/itoa/1.0.15/download\n'
                        printf 'UPSTREAM https://index.crates.io/\n'
                        printf 'UPSTREAM https://static.crates.io/\n'
                    } >>"$(logdir)/egress.log"
                    exit 0
                fi
                printf 'DENY connect-unlisted https://%s/\n' "$denied" >>"$(logdir)/egress.log"
                exit 1 ;;
        esac
        exit 0 ;;
    rm)
        shift
        [ "${1:-}" = -f ] && shift
        for name in "$@"; do
            rm -f "$state/cid.$name"
            if [ -f "$state/running" ]; then
                if grep -vx "$name" "$state/running" >"$state/running.new"; then
                    mv "$state/running.new" "$state/running"
                else
                    : >"$state/running"
                    rm -f "$state/running.new"
                fi
            fi
        done
        [ "$mode" = cleanup-fails ] && exit 1
        : ;;
    logs) printf 'fake proxy log for %s\n' "${2:-}" ;;
    ps) if [ -s "$state/running" ]; then cat "$state/running"; fi ;;
    *) : ;;
esac
FAKE
}

SELF_TEST_FAILURES=0

check_ok() { # DESCRIPTION COMMAND...
    local description=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo "  ok: $description"
    else
        echo "self-test: FAIL $description" >&2
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    fi
}

check_fail() { # DESCRIPTION COMMAND...
    local description=$1
    shift
    if "$@" >/dev/null 2>&1; then
        echo "self-test: FAIL $description (it passed)" >&2
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    else
        echo "  ok: $description"
    fi
}

check_equal() { # DESCRIPTION ACTUAL EXPECTED
    if [ "$2" = "$3" ]; then
        echo "  ok: $1"
    else
        echo "self-test: FAIL $1: expected '$3', got '$2'" >&2
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    fi
}

check_grep() { # PATTERN FILE DESCRIPTION
    if grep -qF -- "$1" "$2" 2>/dev/null; then
        echo "  ok: $3"
    else
        echo "self-test: FAIL $3: '$1' missing from $2" >&2
        sed -n '1,20p' "$2" >&2 || true
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    fi
}

check_absent() { # PATTERN FILE DESCRIPTION
    if grep -qF -- "$1" "$2" 2>/dev/null; then
        echo "self-test: FAIL $3: '$1' is still in $2" >&2
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    else
        echo "  ok: $3"
    fi
}

check_scratch_removed() { # DESCRIPTION EVIDENCE_DIR
    # The directory THIS run created, read from its own evidence, not every
    # `target/network-requests.*` on the host: a concurrent run's live scratch
    # directory used to fail this check.
    local kept
    kept=$(cat "$2/scratch.txt" 2>/dev/null || true)
    if [ -z "$kept" ]; then
        echo "self-test: FAIL $1: the run recorded no scratch directory" >&2
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    elif [ -e "$kept" ]; then
        echo "self-test: FAIL $1: $kept is still there" >&2
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    else
        echo "  ok: $1"
    fi
}

self_test_destinations() {
    local units=$ROUND_SCRATCH/units probe log
    DECLARED='crates.io,index.crates.io,static.crates.io'
    DECLARED_FIRST='crates.io'
    probe=$units/destinations.txt
    log=$units/destinations.log
    printf '%s\n' \
        'case=connect-unlisted connect_status=403 tls=failed http_status=none blocked_by_proxy=yes' \
        'case=sni-unlisted connect_status=200 tls=ok http_status=403 blocked_by_proxy=yes' \
        'case=host-unlisted connect_status=200 tls=ok http_status=403 blocked_by_proxy=yes' \
        'case=positive connect_status=200 tls=ok http_status=200 blocked_by_proxy=no' >"$probe"
    printf '%s\n' \
        'DENY connect-unlisted https://example.com/' \
        'DENY sni-mismatch https://example.com/' \
        'DENY authority-mismatch https://example.com/' \
        "ALLOW https://$DECLARED_FIRST/" \
        "UPSTREAM https://$DECLARED_FIRST/" >"$log"
    check_ok 'the four destination controls pass' assert_destinations "$probe" "$log"
    local pattern
    for pattern in 'case=connect-unlisted' 'case=sni-unlisted' 'case=host-unlisted' 'case=positive'; do
        grep -vF -- "$pattern" "$probe" >"$units/dropped.txt" || true
        check_fail "a destination case missing $pattern fails" \
            assert_destinations "$units/dropped.txt" "$log"
    done
    for pattern in 'DENY connect-unlisted https://example.com/' \
        'DENY authority-mismatch https://example.com/' \
        "UPSTREAM https://$DECLARED_FIRST/"; do
        grep -vF -- "$pattern" "$log" >"$units/dropped.log" || true
        check_fail "a destination log missing '$pattern' fails" \
            assert_destinations "$probe" "$units/dropped.log"
    done
    { cat "$log"; printf 'UPSTREAM https://example.com/\n'; } >"$units/contacted.log"
    check_fail 'an upstream contact with the unlisted destination fails' \
        assert_destinations "$probe" "$units/contacted.log"
}

# --- whole-runner cases -------------------------------------------------------
case_env() { # MODE NAME [ENV ASSIGNMENT...]
    local mode=$1 name=$2
    shift 2
    mkdir -p "$ROUND_SCRATCH/cases" "$ROUND_SCRATCH/state/$name"
    env PATH="$ROUND_SCRATCH/bin:$PATH" DOCKER="$ROUND_SCRATCH/bin/docker" \
        NETWORK_REQUESTS_EGRESS_SELF_TEST=1 \
        NETWORK_REQUESTS_EGRESS_TEST_SCALE=$SELF_TEST_SCALE \
        FAKE_EGRESS_MODE=$mode FAKE_DOCKER_STATE="$ROUND_SCRATCH/state/$name" \
        FAKE_EVIDENCE_DIR="$ROUND_SCRATCH/cases/$name" \
        FAKE_DECLARED_FIRST=crates.io "$@" \
        bash "${BASH_SOURCE[0]}" --image agent-vm-fake:test --layer rust-dev \
        --platform "$SELF_TEST_PLATFORM" --evidence-dir "$ROUND_SCRATCH/cases/$name"
}

case_run() { # MODE NAME [ENV ASSIGNMENT...]
    local mode=$1 name=$2
    shift 2
    mkdir -p "$ROUND_SCRATCH/cases"
    RUN_STATUS=0
    case_env "$mode" "$name" ${@+"$@"} >"$ROUND_SCRATCH/cases/$name.log" 2>&1 ||
        RUN_STATUS=$?
    return 0
}

case_evidence() { # NAME
    printf '%s/cases/%s' "$ROUND_SCRATCH" "$1"
}

preseed_evidence() { # NAME
    local evid
    evid=$(case_evidence "$1")
    mkdir -p "$evid"
    printf 'layer=rust-dev\nstrict=PASS\ndenial=PASS\n' >"$evid/summary.txt"
    printf 'ALLOW https://stale.example/\n' >"$evid/egress-strict.log"
    printf 'keep me\n' >"$evid/keep.txt"
}

self_test_cases() { # SCRATCH PLATFORM
    ROUND_SCRATCH=$1
    SELF_TEST_PLATFORM=$2
    SELF_TEST_SCALE=30
    local scratch=$1 evid name

    echo 'self-test: a healthy run'
    case_run healthy healthy
    evid=$(case_evidence healthy)
    check_equal 'a healthy run exits 0' "$RUN_STATUS" 0
    check_equal 'every step passed' "$(grep -c ' PASS$' "$evid/steps.txt" || true)" 10
    check_equal 'cleanup succeeded' "$(cat "$evid/cleanup.txt")" 'completed status=0'
    check_grep 'declared=crates.io,index.crates.io,static.crates.io' "$evid/summary.txt" \
        'the summary records the declared hosts'
    check_grep 'used=index.crates.io,static.crates.io' "$evid/summary.txt" \
        'the summary records the used hosts'
    check_grep 'evidence=ok' "$evid/summary.txt" 'the evidence is recorded as written'
    check_grep 'cleanup=ok' "$evid/summary.txt" 'the cleanup is recorded'
    check_scratch_removed 'the scratch directory was removed' "$evid"
    check_grep 'network rm' "$scratch/state/healthy/argv.log" 'the networks were removed'
    check_absent 'outside-scope' "$scratch/state/healthy/argv.log" 'nothing outside the run was touched'

    echo 'self-test: a strict DENY is a failure with evidence'
    case_run strict-deny strict-deny
    evid=$(case_evidence strict-deny)
    check_equal 'a denied strict run exits 1' "$RUN_STATUS" 1
    check_grep 'STRICT FAIL' "$evid/steps.txt" 'the strict step failed'
    check_grep 'RESTART NOT-RUN' "$evid/steps.txt" 'the restart never ran'
    check_grep 'DENY unlisted https://other.example/' "$evid/egress-strict.log" \
        'the refusal is published'
    check_scratch_removed 'the scratch directory was removed' "$evid"

    echo 'self-test: a failing fixture command is a failure with evidence'
    case_run strict-fetch-fails strict-fetch-fails
    evid=$(case_evidence strict-fetch-fails)
    check_equal 'a failed fetch exits 1' "$RUN_STATUS" 1
    check_grep 'STRICT FAIL' "$evid/steps.txt" 'the strict step failed'
    check_grep 'strict seconds=' "$evid/durations.txt" 'the strict command is recorded'
    check_grep '+ strict ' "$evid/transcript.log" 'the command transcript is published'
    check_grep 'REVIEW-STDOUT-ONLY-FAILURE' "$evid/egress-strict.out" \
        'the fixture stdout survives a failure'

    echo 'self-test: each network create has its own 20-second bound'
    # The create takes longer than its own bound but is shorter than the step's
    # whole 44-second allocation, so only a per-create bound fails here.
    case_run network-create-slow network-create-slow
    evid=$(case_evidence network-create-slow)
    check_equal 'a slow network create exits 1' "$RUN_STATUS" 1
    check_grep 'NETWORKS FAIL' "$evid/steps.txt" 'the networks step failed'
    check_grep 'network-internal seconds=' "$evid/durations.txt" 'the bound is recorded'
    check_ok 'the create was killed by its own bound' \
        grep -qE '^network-internal seconds=[0-9]+ status=124$' "$evid/durations.txt"

    echo 'self-test: a slow readiness cycle cannot pass late'
    case_run readiness-slow readiness-slow
    evid=$(case_evidence readiness-slow)
    check_equal 'a readiness cycle that outlives its budget exits 1' "$RUN_STATUS" 1
    check_grep 'PROXY FAIL' "$evid/steps.txt" 'the proxy step failed'
    check_grep 'STRICT NOT-RUN' "$evid/steps.txt" 'the smoke never ran'
    check_grep 'proxy_ready_1=timeout' "$scratch/cases/readiness-slow.log" \
        'the reason is a readiness timeout'

    echo 'self-test: a proxy that never becomes ready'
    case_run proxy-never-ready proxy-never-ready
    evid=$(case_evidence proxy-never-ready)
    check_equal 'a proxy that never listens exits 1' "$RUN_STATUS" 1
    check_grep 'PROXY FAIL' "$evid/steps.txt" 'the proxy step failed'
    check_grep 'DESTINATIONS NOT-RUN' "$evid/steps.txt" 'the destination controls never ran'
    check_grep 'fake proxy log for' "$evid/proxy-start-1.log" 'the proxy log is published'

    echo 'self-test: a proxy that starts late is waited for'
    case_run proxy-late proxy-late
    check_equal 'a late proxy still passes' "$RUN_STATUS" 0
    check_ok 'readiness was retried' \
        test "$(wc -c <"$scratch/state/proxy-late/ready-attempts" | tr -d ' ')" -ge 4
    echo 'self-test: a stale CA is not readiness'
    case_run stale-ca-restart stale-ca-restart
    check_equal 'a restart with a stale CA passes' "$RUN_STATUS" 0
    check_equal 'the not-running container was polled twice' \
        "$(cat "$scratch/state/stale-ca-restart/polls")" 2

    echo 'self-test: a changed container ID is not readiness'
    case_run restart-cid-changed restart-cid-changed
    evid=$(case_evidence restart-cid-changed)
    check_equal 'a changed container ID fails the restart' "$RUN_STATUS" 1
    check_grep 'RESTART FAIL' "$evid/steps.txt" 'the restart failed'
    check_grep 'proxy_ready_2=exited' "$scratch/cases/restart-cid-changed.log" \
        'the reason names the exited container'

    echo 'self-test: a proxy that fails to start'
    case_run proxy-start-fails proxy-start-fails
    evid=$(case_evidence proxy-start-fails)
    check_equal 'a proxy start failure exits 1' "$RUN_STATUS" 1
    check_grep 'PROXY FAIL' "$evid/steps.txt" 'the proxy step failed'
    check_grep 'STRICT NOT-RUN' "$evid/steps.txt" 'the strict step never ran'

    echo 'self-test: a command that overruns its bound'
    case_run timeout timeout
    evid=$(case_evidence timeout)
    check_equal 'a timeout exits 1' "$RUN_STATUS" 1
    check_grep 'status=124' "$evid/durations.txt" 'the bound is recorded as 124'
    check_equal 'cleanup still succeeded' "$(cat "$evid/cleanup.txt")" 'completed status=0'
    check_grep 'FIXTURE-PARTIAL-STDOUT' "$evid/egress-strict.out" \
        'the partial stdout of a bounded command is published'

    echo 'self-test: an inherited signal'
    # The runner replaces the background subshell (`exec`) so the test's PID is the
    # runner's own: a signal must reach its handler and its EXIT trap, which a
    # TERM sent to a wrapper shell would never do.
    (
        export PATH="$scratch/bin:$PATH" DOCKER="$scratch/bin/docker"
        export NETWORK_REQUESTS_EGRESS_SELF_TEST=1
        export NETWORK_REQUESTS_EGRESS_TEST_SCALE=$SELF_TEST_SCALE
        export FAKE_EGRESS_MODE=signal FAKE_DOCKER_STATE="$scratch/state/signal"
        export FAKE_DECLARED_FIRST=crates.io
        exec bash "${BASH_SOURCE[0]}" --image agent-vm-fake:test --layer rust-dev \
            --platform "$SELF_TEST_PLATFORM" --evidence-dir "$scratch/cases/signal"
    ) >"$scratch/cases/signal.log" 2>&1 &
    local signal_pid=$! waited=0
    while [ "$waited" -lt 100 ]; do
        if grep -q -- '-smoke' "$scratch/state/signal/argv.log" 2>/dev/null; then
            break
        fi
        sleep 0.1
        waited=$((waited + 1))
    done
    kill -TERM "$signal_pid" 2>/dev/null || true
    RUN_STATUS=0
    wait "$signal_pid" || RUN_STATUS=$?
    evid=$(case_evidence signal)
    check_equal 'a terminated run exits 143' "$RUN_STATUS" 143
    check_grep 'STRICT FAIL' "$evid/steps.txt" 'the interrupted step is a failure'
    check_equal 'the containers were removed' \
        "$([ -s "$scratch/state/signal/running" ] && echo left || echo none)" none
    check_equal 'cleanup succeeded after the signal' "$(cat "$evid/cleanup.txt")" \
        'completed status=0'

    echo 'self-test: a reused evidence directory cannot present a PASS'
    evid=$(case_evidence reused-evidence-dir)
    preseed_evidence reused-evidence-dir
    case_run proxy-start-fails reused-evidence-dir
    check_equal 'the reused directory run exits 1' "$RUN_STATUS" 1
    check_absent 'strict=PASS' "$evid/summary.txt" 'the stale PASS is gone'
    check_absent 'stale.example' "$evid/egress-strict.log" 'the stale log is gone'
    check_equal 'an unrelated file is untouched' "$(cat "$evid/keep.txt")" 'keep me'

    echo 'self-test: a directory at any required evidence path fails the run'
    # One row per destination this runner must be able to write. A directory at
    # that name used to swallow the write (`cp` nests into it, an append fails
    # silently) and the run still reported evidence=ok. Allocation catches every
    # one of them before the first Docker command.
    for name in $RUNNER_OWNED_FILES; do
        evid=$(case_evidence "evidence-path-$name")
        mkdir -p "$evid/$name"
        case_run healthy "evidence-path-$name"
        check_equal "a directory at $name exits 1" "$RUN_STATUS" 1
        check_absent 'pull' "$scratch/state/evidence-path-$name/argv.log" \
            "$name reaches no Docker command"
        # The row whose own path is blocked cannot carry the verdict that says so:
        # steps.txt or summary.txt is then the destination that was lost, and the
        # failure is visible in the other one and in the exit status.
        case "$name" in
            steps.txt)
                check_grep 'evidence=failed' "$evid/summary.txt" \
                    'a lost steps.txt is recorded as failed evidence' ;;
            step-status.log)
                # The lost file is the one that records a step, so the failure is
                # in the summary line that names the reason instead.
                check_grep 'setup=FAIL' "$evid/summary.txt" \
                    'a lost step-status.log is recorded in the summary' ;;
            summary.txt)
                check_grep 'SETUP FAIL' "$evid/steps.txt" \
                    'a lost summary.txt still records the setup failure' ;;
            *)
                check_grep 'SETUP FAIL' "$evid/steps.txt" "$name is a setup failure"
                check_grep 'INSPECT NOT-RUN' "$evid/steps.txt" "$name runs no step"
                check_grep 'evidence=failed' "$evid/summary.txt" \
                    "$name is a lost-evidence failure" ;;
        esac
    done

    echo 'self-test: a blocked evidence name does not keep the earlier run verdicts'
    # Allocation used to return at the first name it could not clear, so every
    # later name kept the earlier run's content: `last_status` then read the stale
    # step-status.log and the summary of a run that never reached INSPECT reported
    # the previous run's strict=PASS. One name that cannot be cleared must not save
    # the rest.
    evid=$(case_evidence stale-after-blocked-name)
    mkdir -p "$evid" "$evid/steps.txt"
    for name in $RUNNER_OWNED_FILES; do
        [ "$name" = steps.txt ] && continue
        printf 'STALE %s\n' "$name" >"$evid/$name"
    done
    printf 'SETUP PASS\nINSPECT PASS\nCONFIG PASS\nPULL PASS\nNETWORKS PASS\nPROXY PASS\nDESTINATIONS PASS\nSTRICT PASS\nRESTART PASS\nDENIAL PASS\n' \
        >"$evid/step-status.log"
    printf 'ALLOW https://stale.example/\n' >"$evid/egress-strict.log"
    printf 'keep me\n' >"$evid/keep.txt"
    case_run healthy stale-after-blocked-name
    check_equal 'a blocked evidence name exits 1' "$RUN_STATUS" 1
    check_grep 'setup=FAIL' "$evid/summary.txt" 'the blocked name is a setup failure'
    check_absent 'strict=PASS' "$evid/summary.txt" 'the earlier run strict verdict is gone'
    check_absent 'denial=PASS' "$evid/summary.txt" 'the earlier run denial verdict is gone'
    check_absent 'ALLOW' "$evid/egress-strict.log" 'the earlier run egress log is gone'
    check_absent 'STALE' "$evid/transcript.log" 'the earlier run transcript is gone'
    check_absent 'STALE' "$evid/durations.txt" 'the earlier run durations are gone'
    check_absent 'PASS' "$evid/step-status.log" 'the earlier run step statuses are gone'
    check_equal 'an unrelated file is untouched' "$(cat "$evid/keep.txt")" 'keep me'

    echo 'self-test: an evidence append lost after allocation is still a failure'
    # `sabotage-durations` is the one that isolates an append check: nothing else
    # reads that file, so only the checked `duration` write can notice. The
    # transcript rows break a file that is appended to AND read back, so they prove
    # the mid-run loss fails the run without claiming which of the two checks saw it.
    for mode in sabotage-durations sabotage-transcript sabotage-live-transcript; do
        evid=$(case_evidence "$mode")
        case_run "$mode" "$mode"
        check_equal "$mode exits 1" "$RUN_STATUS" 1
        check_grep 'evidence=failed' "$evid/summary.txt" "$mode is a lost-evidence failure"
        check_grep 'scratch kept' "$scratch/cases/$mode.log" "$mode keeps the scratch"
        check_grep 'image inspect' "$scratch/state/$mode/argv.log" \
            "$mode reaches Docker before the loss"
    done

    echo 'self-test: a cleanup failure fails a passing run'
    case_run cleanup-fails cleanup-fails
    check_equal 'a cleanup failure exits 1' "$RUN_STATUS" 1
    check_grep 'cleanup=failed' "$(case_evidence cleanup-fails)/summary.txt" \
        'the summary records failed cleanup'

    echo 'self-test: the denial control must be load-bearing'
    case_run denial-not-load-bearing denial-not-load-bearing
    evid=$(case_evidence denial-not-load-bearing)
    check_equal 'a denial run that succeeds exits 1' "$RUN_STATUS" 1
    check_grep 'DENIAL FAIL' "$evid/steps.txt" 'the denial step failed'

    echo 'self-test: an open destination gate is caught before the smoke'
    case_run destination-gate-open destination-gate-open
    evid=$(case_evidence destination-gate-open)
    check_equal 'an open destination gate exits 1' "$RUN_STATUS" 1
    check_grep 'DESTINATIONS FAIL' "$evid/steps.txt" 'the destination controls failed'
    check_grep 'STRICT NOT-RUN' "$evid/steps.txt" 'the smoke never ran'

    echo 'self-test: SETUP failures clear earlier evidence'
    mkdir -p "$scratch/fixtures/rust-dev"
    for defect in setup-malformed-ref setup-missing-smoke; do
        evid=$(case_evidence "$defect")
        preseed_evidence "$defect"
        if [ "$defect" = setup-malformed-ref ]; then
            case_run healthy "$defect" NETWORK_REQUESTS_EGRESS_TEST_REF="$scratch/tag.ref"
        else
            case_run healthy "$defect" NETWORK_REQUESTS_EGRESS_TEST_FIXTURES="$scratch/fixtures"
        fi
        check_equal "$defect exits 1" "$RUN_STATUS" 1
        check_grep 'SETUP FAIL' "$evid/steps.txt" "$defect records the setup failure"
        check_grep 'INSPECT NOT-RUN' "$evid/steps.txt" "$defect leaves later steps NOT-RUN"
        check_absent 'stale.example' "$evid/egress-strict.log" "$defect clears the stale log"
        check_equal "$defect keeps an unrelated file" "$(cat "$evid/keep.txt")" 'keep me'
        check_grep 'image_id=unknown' "$evid/summary.txt" \
            "$defect names the image it never inspected"
        check_absent 'pull' "$scratch/state/$defect/argv.log" "$defect ran no docker command"
    done
    check_grep 'smoke.sh' "$(case_evidence setup-missing-smoke)/summary.txt" \
        'the missing smoke.sh is named'

    echo 'self-test: a readiness client outliving its killed CLI'
    case_run readiness-client-orphaned readiness-client-orphaned
    evid=$(case_evidence readiness-client-orphaned)
    check_equal 'an orphaned readiness client exits 1' "$RUN_STATUS" 1
    check_grep 'PROXY FAIL' "$evid/steps.txt" 'the proxy step failed'
    check_grep 'proxy_ready_1=timeout' "$scratch/cases/readiness-client-orphaned.log" \
        'the reason is a readiness timeout'
    check_grep '-ready-1' "$scratch/state/readiness-client-orphaned/argv.log" \
        'the readiness client was run-scoped'
    check_equal 'the orphan was removed' \
        "$([ -s "$scratch/state/readiness-client-orphaned/running" ] && echo left || echo none)" none
    check_equal 'cleanup succeeded' "$(cat "$evid/cleanup.txt")" 'completed status=0'
}

run_self_test() {
    local scratch
    scratch=$(mktemp -d "${TMPDIR:-/tmp}/network-requests-egress-selftest.XXXXXX")
    ROUND_SCRATCH=$scratch
    trap 'rm -rf "$ROUND_SCRATCH"' EXIT
    local host_platform=linux/arm64
    case "$(uname -m)" in
        x86_64 | amd64) host_platform=linux/amd64 ;;
    esac
    mkdir -p "$scratch/bin" "$scratch/state" "$scratch/units" "$scratch/cases"
    write_fake_docker "$scratch/bin/docker"
    chmod 0755 "$scratch/bin/docker"

    echo 'self-test: stream assertions'
    local units=$scratch/units
    DECLARED='crates.io,index.crates.io,static.crates.io'
    printf 'ALLOW https://index.crates.io/\nALLOW https://static.crates.io/crates/itoa/1.0.15/download\nUPSTREAM https://index.crates.io/\nUPSTREAM https://static.crates.io/\n' >"$units/good"
    check_ok 'a correct strict stream passes' assert_strict_log "$units/good"
    printf 'ALLOW https://index.crates.io/\nDENY unlisted https://other.example/\n' >"$units/deny"
    check_fail 'a strict stream with a DENY fails' assert_strict_log "$units/deny"
    : >"$units/empty"
    check_fail 'an empty strict stream fails' assert_strict_log "$units/empty"
    printf 'ALLOW-LEARNED https://cdn.example/\n' >"$units/learned"
    check_fail 'a learned target fails' assert_strict_log "$units/learned"
    printf 'ALLOW https://undeclared.example/\n' >"$units/undeclared-allow"
    check_fail 'an ALLOW to an undeclared host fails' assert_strict_log "$units/undeclared-allow"
    printf 'ALLOW https://index.crates.io/\nUPSTREAM https://undeclared.example/\n' >"$units/undeclared-upstream"
    check_fail 'an UPSTREAM to an undeclared host fails' assert_strict_log "$units/undeclared-upstream"
    printf 'ALLOW https://proxy.golang.org/\nUPSTREAM https://proxy.golang.org/\n' >"$units/go"
    DECLARED='proxy.golang.org,sum.golang.org'
    check_ok 'a Go stream without sum.golang.org passes' assert_strict_log "$units/go"
    DECLARED='crates.io,index.crates.io,static.crates.io'
    printf 'DENY connect-unlisted https://crates.io/\n' >"$units/denial-good"
    check_ok 'a denial stream passes' assert_denial_log "$units/denial-good" crates.io
    printf 'DENY connect-unlisted https://crates.io/\nUPSTREAM https://crates.io/\n' >"$units/denial-upstream"
    check_fail 'a denial stream that reached the host fails' \
        assert_denial_log "$units/denial-upstream" crates.io
    printf 'DENY connect-unlisted https://other.example/\n' >"$units/denial-other"
    check_fail 'a denial stream naming another host fails' \
        assert_denial_log "$units/denial-other" crates.io
    : >"$units/denial-empty"
    check_fail 'an empty denial stream fails' assert_denial_log "$units/denial-empty" crates.io
    self_test_destinations

    echo 'self-test: the proxy pin reader'
    if read_proxy_image "$REFERENCE" >/dev/null; then
        echo '  ok: the committed reference is accepted'
    else
        echo 'self-test: FAIL the committed reference was refused' >&2
        SELF_TEST_FAILURES=$((SELF_TEST_FAILURES + 1))
    fi
    printf 'mitmproxy/mitmproxy:12\n' >"$scratch/tag.ref"
    printf '%s\n%s\n' "$(cat "$REFERENCE")" "$(cat "$REFERENCE")" >"$scratch/duplicate.ref"
    : >"$scratch/empty.ref"
    for defect in tag duplicate empty; do
        check_fail "a $defect reference is refused" read_proxy_image "$scratch/$defect.ref"
    done

    echo 'self-test: budget invariants'
    check_equal 'the runner maximum covers the active and finalizer budgets' \
        "$((MAX_SECONDS >= ACTIVE_SECONDS + FINALIZE_SECONDS))" 1
    check_equal 'the active budget is the sum of the step budgets' "$ACTIVE_SECONDS" 830
    check_equal 'the NETWORKS allocation is two bounded creates plus their windows' \
        "$NETWORKS_SECONDS" "$((2 * (NETWORK_CREATE_SECONDS + 2)))"

    echo 'self-test: every command bound is clamped to the deadline in force'
    # A step allocation only means something if the child cannot borrow the next
    # step's share: the clamp is arithmetic, so it is checked as arithmetic. Bash
    # lets SECONDS be assigned, which is what makes a deadline edge testable
    # without waiting for it.
    local saved_seconds=$SECONDS saved_limit=${LIMIT_DEADLINE:-} saved_ready=${READY_DEADLINE:-}
    SECONDS=0
    LIMIT_DEADLINE=5
    READY_DEADLINE=''
    check_equal 'a command inside its bound keeps it' "$(bounded_seconds 2)" 2
    check_equal 'a command past the active deadline gets only the remainder' \
        "$(bounded_seconds 20)" 3
    READY_DEADLINE=3
    check_equal 'a nested readiness deadline wins over the active one' \
        "$(bounded_seconds 20)" 3
    READY_DEADLINE=1
    check_equal 'a command near the readiness deadline gets the remainder' \
        "$(bounded_seconds 20)" 1
    SECONDS=10
    READY_DEADLINE=''
    check_equal 'a command entered after the active deadline gets none' \
        "$(bounded_seconds 20)" 0
    SECONDS=$saved_seconds
    LIMIT_DEADLINE=$saved_limit
    READY_DEADLINE=$saved_ready

    echo 'self-test: a readiness wait stops at the active deadline'
    # `wait_proxy_ready` gives itself the whole readiness budget. Entered with one
    # second of active time left and a proxy that never becomes ready, it has to
    # stop at the deadline it was given: dropping the clamp polls for the full
    # readiness budget and is the original unbounded wait.
    local ready_status=0 elapsed start
    saved_seconds=$SECONDS
    saved_limit=${LIMIT_DEADLINE:-}
    saved_ready=${READY_DEADLINE:-}
    saved_tmp=${TMP:-}
    saved_evid=${EVID:-}
    saved_proxy_ready=$PROXY_READY_SECONDS
    saved_run_id=${RUN_ID:-}
    SECONDS=0
    LIMIT_DEADLINE=1
    READY_DEADLINE=''
    PROXY_READY_SECONDS=5
    RUN_ID=never-ready
    TMP=$scratch/units/never-ready
    EVID=$scratch/units/never-ready-evidence
    mkdir -p "$TMP/certs" "$EVID"
    start=$SECONDS
    wait_proxy_ready 1 2>"$scratch/units/never-ready.log" || ready_status=$?
    elapsed=$((SECONDS - start))
    check_equal 'a wait against a never-ready proxy fails' "$ready_status" 1
    check_grep 'proxy_ready_1=timeout' "$scratch/units/never-ready.log" \
        'the reason is a readiness timeout'
    check_equal 'the wait returns within the active remainder plus one grace' \
        "$([ "$elapsed" -le 2 ] && echo bounded || echo "$elapsed")" bounded
    SECONDS=$saved_seconds
    LIMIT_DEADLINE=$saved_limit
    READY_DEADLINE=$saved_ready
    TMP=$saved_tmp
    EVID=$saved_evid
    PROXY_READY_SECONDS=$saved_proxy_ready
    RUN_ID=$saved_run_id

    echo 'self-test: whole-runner cases'
    self_test_cases "$scratch" "$host_platform"

    if [ "$SELF_TEST_FAILURES" -ne 0 ]; then
        echo "network-requests-egress --self-test: $SELF_TEST_FAILURES failure(s)" >&2
        return 1
    fi
    echo 'network-requests-egress --self-test: OK'
}

# --- dispatch -----------------------------------------------------------------
case "${1:-}" in
    --print-budget)
        [ "$#" -eq 1 ] || usage
        printf 'ACTIVE_SECONDS=%s\nFINALIZE_SECONDS=%s\nMAX_SECONDS=%s\n' \
            "$ACTIVE_SECONDS" "$FINALIZE_SECONDS" "$MAX_SECONDS"
        exit 0 ;;
    --print-owned-files)
        # The runner is the one owner of this list: a caller that reuses the
        # evidence directory has to clear exactly these files and nothing else.
        [ "$#" -eq 1 ] || usage
        for name in $RUNNER_OWNED_FILES; do printf '%s\n' "$name"; done
        exit 0 ;;
    --self-test)
        [ "$#" -eq 1 ] || usage
        if run_self_test; then exit 0; fi
        exit 1 ;;
esac

IMAGE=''
LAYER=''
PLATFORM=''
EVID=''
while [ "$#" -gt 0 ]; do
    case "$1" in
        --image | --layer | --platform | --evidence-dir)
            [ "$#" -ge 2 ] && [ -n "$2" ] || usage
            case "$1" in
                --image) IMAGE=$2 ;;
                --layer) LAYER=$2 ;;
                --platform) PLATFORM=$2 ;;
                --evidence-dir) EVID=$2 ;;
            esac
            shift 2 ;;
        *) usage ;;
    esac
done
# Syntactic validation only: the fixture and the proxy pin are operational inputs,
# read in SETUP after the evidence directory has been cleared. A reused directory
# must not be able to present an earlier run's PASS.
[ -n "$IMAGE" ] || usage
case "$LAYER" in
    '' | *[!a-z0-9-]*) usage ;;
esac
case "$PLATFORM" in
    linux/amd64 | linux/arm64) ;;
    *) usage ;;
esac

RUN_ID="network-requests-$$-$(date +%s)"
EVID=${EVID:-$ROOT/target/ci-logs/network-requests-$RUN_ID}
mkdir -p "$EVID" || fail "cannot create the evidence directory $EVID"
EVID=$(cd "$EVID" && pwd)
CURRENT=''
TMP=''
MITM_IMAGE=''
PROXY_CID=''
READY_SEQ=0
READY_DEADLINE=''
MARK_DESTINATIONS=0
MARK_STRICT=0
MARK_DENIAL=0
DECLARED=''
DECLARED_FIRST=''
USED=''
DENIAL_FIRST=''
SETUP_FAILURE=''

# The traps go in first and the allocation immediately after them, so a lost
# evidence destination records itself and still publishes what it can. Only names
# this runner creates and reads back are touched: an unrelated file in a reused
# directory stays, but no earlier verdict can survive.
LIMIT_DEADLINE=$((SECONDS + ACTIVE_SECONDS))
trap finalize EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

CURRENT=SETUP
# SETUP must run in this shell, not in a command substitution: it creates the
# scratch directory the rest of the run uses.
if ! allocate_evidence; then
    SETUP_FAILURE="the evidence directory $EVID cannot hold this run"
    record_step SETUP FAIL
    echo "network-requests-egress: SETUP FAIL: $SETUP_FAILURE" >&2
elif step_setup 2>"$EVID/setup.err"; then
    record_step SETUP PASS
else
    SETUP_FAILURE=$(cat "$EVID/setup.err")
    record_step SETUP FAIL
    echo "network-requests-egress: SETUP FAIL: ${SETUP_FAILURE:-unknown}" >&2
fi
write_steps
if [ "$(last_status SETUP)" != PASS ]; then
    exit 1
fi

run_step() { # NAME FUNCTION
    CURRENT=$1
    if "$2"; then
        record_step "$1" PASS
    else
        record_step "$1" FAIL
    fi
    write_steps
    [ "$(last_status "$1")" = PASS ]
}

run_step INSPECT step_inspect || exit 1
run_step CONFIG step_config || exit 1
run_step PULL step_pull || exit 1
run_step NETWORKS step_networks || exit 1
run_step PROXY step_proxy || exit 1
run_step DESTINATIONS step_destinations || exit 1
run_step STRICT step_strict || exit 1
run_step RESTART step_restart || exit 1
run_step DENIAL step_denial || exit 1
CURRENT=''
