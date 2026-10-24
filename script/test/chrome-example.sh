#!/usr/bin/env bash
# Chrome example: identity, NSS, browser, pinned-MCP and policy checks (#41).
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
cd "$ROOT"
# Trusted publisher identity, used by phase A to re-verify the committed bundle.
# shellcheck source=/dev/null
. "$ROOT/script/build/chrome-mcp-provenance.sh"
MCP_HELPER=examples/layers/chrome-devtools/chrome-devtools-mcp.sh
MCP_INPUTS=examples/layers/chrome-devtools/chrome-devtools-mcp
MCP_BUNDLE=examples/layers/chrome-devtools/chrome-devtools-mcp.sigstore.json
MCP_PREFIX=/opt/agent-vm/chrome-devtools-mcp
RUN_ID=$(printf '%s' "${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$-$(date +%s)" | tr 'A-Z' 'a-z')
PLATFORM=''
PARENT=$(awk 'NR == 1 && sub(/^ARG BASE_IMAGE=/, "") { print; exit }' examples/layers/chrome-devtools/Dockerfile)
EVID="$ROOT/target/chrome-example/$RUN_ID"
usage() { echo 'usage: chrome-example.sh [--platform linux/ARCH] [--base-image REF] [--evidence-dir DIR] [--skip-mutations]' >&2; exit 2; }
SKIP_MUTATIONS=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --skip-mutations) SKIP_MUTATIONS=true; shift; continue ;;
    esac
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
CLEANUP_SECONDS=90          # total deadline, not a per-command allowance
CLEANUP_COMMAND_SECONDS=5
CLEANUP_ESCALATION_SECONDS=1 # two 0.5 s supervisor polling windows
CLEANUP_RESERVE_SECONDS=2   # evidence/scheduling, including SECONDS truncation
# Two listings plus twelve clients: 2 * (5 + 1) + 12 * (5 + 1) = 84 < 90. The
# suite now derives images (armed, argv, sentinel) and runs many short-lived
# containers, so the budget covers twelve leftovers rather than six.
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
PHASES='P0 P B S I N W D V R Y M A E'
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
    # A derived image cannot be removed before its children (armed, argv and
    # sentinel are children of the built image), and the id order is alphabetical,
    # not the dependency order. A failed removal is therefore retried after the
    # rest of the pass. Three passes cover the chains this suite creates: at most
    # twelve clients (72 s) plus two listings (10 s) stay inside the budget.
    if cleanup_timed cleanup-images docker image ls -q --filter "label=$LABEL"; then
        sort -u "$EVID/cleanup-images.log" > "$EVID/cleanup-image-ids.txt"
        cp "$EVID/cleanup-image-ids.txt" "$EVID/cleanup-image-remaining.txt"
        cleanup_pass=0
        while [ -s "$EVID/cleanup-image-remaining.txt" ] && [ "$cleanup_pass" -lt 3 ]; do
            cleanup_pass=$((cleanup_pass + 1))
            : > "$EVID/cleanup-image-retry-$cleanup_pass.txt"
            while IFS= read -r id; do
                [ -z "$id" ] && continue
                cleanup_timed "cleanup-image-${id#sha256:}-p$cleanup_pass" docker image rm -f "$id" >/dev/null ||
                    printf '%s\n' "$id" >> "$EVID/cleanup-image-retry-$cleanup_pass.txt"
            done < "$EVID/cleanup-image-remaining.txt"
            mv "$EVID/cleanup-image-retry-$cleanup_pass.txt" "$EVID/cleanup-image-remaining.txt"
        done
        [ ! -s "$EVID/cleanup-image-remaining.txt" ] || cleanup_status=1
    else cleanup_status=1; fi
    printf 'completed status=%s\n' "$cleanup_status" > "$EVID/cleanup.txt"
    [ "$cleanup_status" -eq 0 ] || { echo 'owned-resource cleanup failed' >&2; status=1; }
    if [ "$status" -eq 0 ]; then
        if [ "$SKIP_MUTATIONS" = true ]; then
            echo "chrome-example: PARTIAL (mutations skipped) platform=$PLATFORM parent=$PARENT"
        else
            echo "chrome-example: PASS ($PHASE_COUNT phases) platform=$PLATFORM parent=$PARENT"
        fi
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
    local label=$1 image=$2 ca='' cid status=0 spec n=0
    local opts=() copies=()
    shift 2
    while [ "$1" != -- ]; do
        case "$1" in
            --test-ca) ca=yes; shift ;;
            --copy) copies+=("$2"); shift 2 ;;
            *) opts+=("$1"); shift ;;
        esac
    done
    shift
    timed "$label-create" "$CONTAINER_CHECK_SECONDS" docker create --label "$LABEL" --security-opt seccomp=unconfined \
        ${opts[@]+"${opts[@]}"} "$image" "$@" >/dev/null || fail "$label infrastructure failure"
    cid=$(cat "$EVID/$label-create.log")
    timed "$label-fixtures" "$CONTAINER_CHECK_SECONDS" docker cp "$ROOT/script/test/fixtures/chrome-example/." "$cid:/agent-vm-test" >/dev/null || fail "$label infrastructure failure"
    for spec in ${copies[@]+"${copies[@]}"}; do
        n=$((n + 1))
        timed "$label-copy-$n" "$CONTAINER_CHECK_SECONDS" docker cp "${spec%%:*}" "$cid:${spec#*:}" >/dev/null || fail "$label infrastructure failure"
    done
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
run_check structure "$IMAGE" --copy "$ROOT/$MCP_HELPER:/agent-vm-test/chrome-devtools-mcp.sh" -- \
    sh /agent-vm-test/checks.sh structure
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
    run_check "$detector" "$image" --copy "$ROOT/$MCP_HELPER:/agent-vm-test/chrome-devtools-mcp.sh" -- \
        sh /agent-vm-test/checks.sh "$check" || status=$?
    [ "$status" -ne 0 ] || fail "detector $detector passed vacuously"
    [ "$status" -ne 124 ] && [ "$status" -ne 137 ] || fail "detector $detector timed out"
done
pass
CURRENT=V
# The installed-tree invariant (B.6). Each case mutates the real pnpm tree and
# then runs the helper's verify; mutate-tree fails if the mutation did not apply,
# so a no-op cannot read as a rejection.
for case in V0 V1 V2 V3 V4 V5 V6 V7 V8 V9 V10 V11; do
    case "$case" in
        V0) expect='' ;;
        V1 | V2 | V3 | V10) expect='prefix entry not root-owned read-only' ;;
        V4 | V5 | V6) expect='prefix link escapes or dangles' ;;
        V7) expect='ancestor not root-owned and write-protected: /opt/agent-vm' ;;
        V8) expect='wrapper must be root-owned 0755' ;;
        V9) expect='unsupported entry in prefix' ;;
        V11) expect='prefix link not root-owned' ;;
    esac
    status=0
    run_check "tree-$case" "$IMAGE" --network none \
        --copy "$ROOT/$MCP_HELPER:/agent-vm-test/chrome-devtools-mcp.sh" -- \
        sh -c "sh /agent-vm-test/checks.sh mutate-tree $case && sh /agent-vm-test/chrome-devtools-mcp.sh verify" || status=$?
    if [ -z "$expect" ]; then
        [ "$status" -eq 0 ] || fail "$case rejected the reviewed tree"
    else
        [ "$status" -ne 0 ] || fail "$case accepted a mutated tree"
        grep -qF "$expect" "$EVID/tree-$case.log" || fail "$case lacks the expected rejection: $expect"
    fi
done
pass
CURRENT=R
# Derived images carry the recorders; labels are inherited, so cleanup covers
# them. Every run is offline: first use must not need the registry.
R_PATHS=/agent-vm-test/managers:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
derive_image() { # derive_image TAG MODE(armed|argv)
    local tag=$1 mode=$2 cid
    timed "derive-$mode-create" "$DISCOVERY_SECONDS" docker create --label "$LABEL" "$IMAGE" \
        sh /agent-vm-test/arm-managers.sh "$mode" >/dev/null || fail 'cannot create the rig container'
    cid=$(cat "$EVID/derive-$mode-create.log")
    timed "derive-$mode-fixtures" "$CONTAINER_CHECK_SECONDS" docker cp "$ROOT/script/test/fixtures/chrome-example/." "$cid:/agent-vm-test" >/dev/null \
        || fail 'cannot copy the rig fixtures'
    timed "derive-$mode-arm" "$CONTAINER_CHECK_SECONDS" docker start -a "$cid" || fail 'cannot arm the derived image'
    timed "derive-$mode-commit" "$DISCOVERY_SECONDS" docker commit "$cid" "$tag" >/dev/null || fail 'cannot commit the derived image'
    timed "derive-$mode-remove" "$DISCOVERY_SECONDS" docker rm -f "$cid" >/dev/null || fail 'cannot remove the rig container'
}
ARMED="agent-vm-chrome-check:$RUN_ID-armed"
ARGV_IMAGE="agent-vm-chrome-check:$RUN_ID-argv"
derive_image "$ARMED" armed
derive_image "$ARGV_IMAGE" argv
run_check mcp-root "$ARMED" --network none -e HOME=/root \
    -e CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 -e "PATH=$R_PATHS" -- sh /agent-vm-test/checks.sh mcp wrapper
run_check mcp-numeric "$ARMED" --network none --user 12345:23456 -e HOME=/tmp \
    -e CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 -e "PATH=$R_PATHS" -- sh /agent-vm-test/checks.sh mcp wrapper
run_check mcp-overmount "$ARMED" --network none \
    --tmpfs /home/chrome:uid=9999,gid=9999,mode=0755 \
    -e CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 -e "PATH=$R_PATHS" -- sh /agent-vm-test/checks.sh mcp wrapper
run_check argv-root "$ARGV_IMAGE" --network none \
    -e PATH="$R_PATHS" -- sh /agent-vm-test/checks.sh argv-matrix root
run_check argv-numeric "$ARGV_IMAGE" --network none --user 12345:12345 -e HOME=/tmp \
    -e PATH="$R_PATHS" -- sh /agent-vm-test/checks.sh argv-matrix numeric
run_check mcp-manager "$ARMED" --network none \
    -e CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 -e PATH="$R_PATHS" -- sh /agent-vm-test/checks.sh mcp manager
# `mcp cold` is a positive control for a negative path: it exits 0 when the
# unarmed image really fails to reach the registry, so the status is the control
# verdict and the failure text is asserted from the log.
run_check mcp-cold "$IMAGE" --network none \
    --tmpfs /home/chrome:uid=9999,gid=9999,mode=0755 \
    -e CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1 -- sh /agent-vm-test/checks.sh mcp cold
grep -qF 'PROBE FAIL server-exit' "$EVID/mcp-cold.log" || fail 'R6b: expected a server exit, not a probe timeout'
grep -qF 'registry.npmjs.org' "$EVID/mcp-cold.log" || fail 'R6b: npm did not try the registry'
record mcp_cold_failure 'server-exit with registry.npmjs.org'
pass
CURRENT=Y
# Policy controls (B.11.2). The sentinel image removes the installed prefix, so
# every case starts from inputs only, and records manager calls so "the helper
# refused" is distinguishable from "the reviewed manager refused".
SENTINEL="agent-vm-chrome-check:$RUN_ID-sentinel"
timed sentinel-build "$BUILD_SECONDS" docker build --builder "$BUILDER" --platform "$PLATFORM" --progress=plain \
    --label "$LABEL" --build-arg "EXAMPLE=$IMAGE" -t "$SENTINEL" \
    -f "$ROOT/script/test/fixtures/chrome-example/sentinel.Dockerfile" "$ROOT/script/test/fixtures/chrome-example" \
    || fail 'sentinel image build failed'
mkdir -p "$EVID/y/modes"
for mode in pass fail version-12 age-tighten hooks hooks-enabled hooks-no-flag weaken; do
    printf '%s\n' "$mode" > "$EVID/y/modes/$mode"
done
y_inputs() { # y_inputs CASE -> path to a private copy of the committed inputs
    rm -rf "$EVID/y/$1/inputs"
    mkdir -p "$EVID/y/$1"
    cp -R "$MCP_INPUTS" "$EVID/y/$1/inputs"
    printf '%s\n' "$EVID/y/$1/inputs"
}
y_repin() { ( cd "$1" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml > pnpm-inputs.sha256 ); }
y_calls() { if [ -f "$Y_CALLS" ]; then wc -l < "$Y_CALLS" | tr -d ' '; else echo 0; fi; }
y_settings() { jq -se 'all(.[];
    .env.PNPM_CONFIG_MINIMUM_RELEASE_AGE == "1440"
    and .env.PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT == "true"
    and .env.PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME == "false"
    and .env.PNPM_CONFIG_TRUST_POLICY == "no-downgrade"
    and .env.PNPM_CONFIG_IGNORE_SCRIPTS == "true"
    and (.env.HOME | startswith("/tmp/tmp."))
    and .env.PATH == "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")' "$Y_CALLS" >/dev/null; }
y_expect_refusal() { # y_expect_refusal LABEL MESSAGE
    [ "$Y_STATUS" -ne 0 ] || fail "$1: the helper accepted refused inputs"
    grep -qF "$2" "$EVID/$1.log" || fail "$1: missing diagnostic: $2"
    [ "$(y_calls)" -eq 0 ] || fail "$1: the helper called a manager"
    [ "$Y_MODULES" = no ] || fail "$1: an install happened"
}
run_sentinel() { # run_sentinel LABEL MODE INPUTS_DIR [docker opts...] --
    local label=$1 mode=$2 inputs=$3 cid
    local opts=()
    Y_STATUS=0
    Y_MODULES=no
    Y_CALLS=$EVID/y/$label-sentinel/calls.jsonl
    shift 3
    while [ "$1" != -- ]; do
        opts+=("$1")
        shift
    done
    shift
    timed "$label-create" "$CONTAINER_CHECK_SECONDS" docker create --label "$LABEL" \
        --security-opt seccomp=unconfined ${opts[@]+"${opts[@]}"} \
        "$SENTINEL" sh /tmp/chrome-devtools-mcp.sh install >/dev/null || fail "$label infrastructure failure"
    cid=$(cat "$EVID/$label-create.log")
    timed "$label-inputs" "$CONTAINER_CHECK_SECONDS" docker cp "$inputs" "$cid:$MCP_PREFIX" >/dev/null || fail "$label infrastructure failure"
    timed "$label-helper" "$CONTAINER_CHECK_SECONDS" docker cp "$ROOT/$MCP_HELPER" "$cid:/tmp/chrome-devtools-mcp.sh" >/dev/null || fail "$label infrastructure failure"
    timed "$label-mode" "$CONTAINER_CHECK_SECONDS" docker cp "$EVID/y/modes/$mode" "$cid:/agent-vm-sentinel/mode" >/dev/null || fail "$label infrastructure failure"
    timed "$label" "$CONTAINER_CHECK_SECONDS" docker start -a "$cid" || Y_STATUS=$?
    timed "$label-state" "$DISCOVERY_SECONDS" docker inspect "$cid" >/dev/null || fail "$label inspection failure"
    if timed "$label-collect" "$DISCOVERY_SECONDS" docker cp "$cid:/agent-vm-sentinel/." "$EVID/y/$label-sentinel" >/dev/null 2>&1; then :; fi
    if timed "$label-modules" "$DISCOVERY_SECONDS" docker cp "$cid:$MCP_PREFIX/node_modules/.modules.yaml" "$EVID/y/$label.modules" >/dev/null 2>&1; then
        Y_MODULES=yes
    fi
    timed "$label-remove" "$DISCOVERY_SECONDS" docker rm -f "$cid" >/dev/null || fail "$label infrastructure failure"
    return "$Y_STATUS"
}
# Y0 positive control: ordinary inputs install, and pnpm's own text is accepted.
Y_STATUS=0
run_sentinel y0 pass "$(y_inputs y0)" --network bridge -e NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt --
[ "$Y_STATUS" -eq 0 ] || { cat "$EVID/y0.log" >&2; fail 'Y0: the reviewed inputs failed to install'; }
[ "$(y_calls)" -eq 2 ] || fail "Y0: expected two manager calls, got $(y_calls)"
y_settings || fail 'Y0: a manager call lacked the five reviewed settings or the private environment'
jq -se '.[0].argv == ["--version", "--pm-on-fail=error", "--config.update-notifier=false"]' "$Y_CALLS" >/dev/null \
    || fail 'Y0: unexpected --version argv'
jq -se '.[1].argv[0] == "install"
    and (. [1].argv | index("--frozen-lockfile") != null and index("--prod") != null
        and index("--no-optional") != null and index("--ignore-scripts") != null
        and index("--ignore-pnpmfile") != null and index("--pm-on-fail=error") != null
        and index("--package-import-method=copy") != null
        and (any(.[]; startswith("--store-dir"))))' "$Y_CALLS" >/dev/null \
    || fail 'Y0: the install argv is not the reviewed one'
grep -qF 'resolution step is skipped' "$EVID/y0.log" || fail 'Y0: pnpm did not use the committed lock'
[ "$Y_MODULES" = yes ] || fail 'Y0: no installed tree'
record y0_calls "$(y_calls)"
# Y1-Y8: reviewed-input refusals, before any manager call.
inputs=$(y_inputs y1); printf ' ' >> "$inputs/pnpm-lock.yaml"
run_sentinel y1 pass "$inputs" --network none -- || true
y_expect_refusal y1 'pnpm-lock.yaml: bytes differ from the reviewed pin'
inputs=$(y_inputs y2); rm "$inputs/pnpm-lock.yaml"
run_sentinel y2 pass "$inputs" --network none -- || true
y_expect_refusal y2 'missing input: pnpm-lock.yaml'
inputs=$(y_inputs y3); awk '$2 != "pnpm-lock.yaml"' "$inputs/pnpm-inputs.sha256" > "$EVID/y/y3.pins"
cat "$EVID/y/y3.pins" > "$inputs/pnpm-inputs.sha256"
run_sentinel y3 pass "$inputs" --network none -- || true
y_expect_refusal y3 'pin file must name exactly'
inputs=$(y_inputs y4); : > "$inputs/.npmrc"
run_sentinel y4 pass "$inputs" --network none -- || true
y_expect_refusal y4 'unreviewed input: .npmrc'
inputs=$(y_inputs y5); : > "$inputs/.pnpmfile.cjs"
run_sentinel y5 pass "$inputs" --network none -- || true
y_expect_refusal y5 'unreviewed input: .pnpmfile.cjs'
inputs=$(y_inputs y6)
jq '.dependencies["chrome-devtools-mcp"] = "^1.0.1"' "$inputs/package.json" > "$EVID/y/y6.manifest"
cat "$EVID/y/y6.manifest" > "$inputs/package.json"
y_repin "$inputs"
run_sentinel y6 pass "$inputs" --network none -- || true
y_expect_refusal y6 'selection must be one canonical exact version'
inputs=$(y_inputs y7); echo 'ignoreScripts: false' >> "$inputs/pnpm-workspace.yaml"; y_repin "$inputs"
run_sentinel y7 pass "$inputs" --network none -- || true
y_expect_refusal y7 'policy escape in pnpm-workspace.yaml: ignoreScripts'
inputs=$(y_inputs y8)
awk '/^snapshots:/ && !done { print "  other-pkg@1.0.0:"; done = 1 } { print }' "$inputs/pnpm-lock.yaml" > "$EVID/y/y8.lock"
cat "$EVID/y/y8.lock" > "$inputs/pnpm-lock.yaml"
y_repin "$inputs"
run_sentinel y8 pass "$inputs" --network none -- || true
y_expect_refusal y8 'closure shape changed; review required'
# Y9: the manager version gate is the only manager call.
run_sentinel y9 version-12 "$(y_inputs y9)" --network none -- || true
[ "$Y_STATUS" -ne 0 ] || fail 'Y9: an unexpected manager version was accepted'
grep -qF 'pnpm 11.11.0 is required (packageManager); parent provides 12.9.1' "$EVID/y9.log" \
    || fail 'Y9: missing the version diagnostic'
[ "$(y_calls)" -eq 1 ] || fail "Y9: expected exactly one manager call, got $(y_calls)"
[ "$Y_MODULES" = no ] || fail 'Y9: an install happened'
# Y10-Y18: ambient configuration is refused before any manager call.
ambient() { # ambient LABEL MESSAGE ENVVAR=VALUE...
    local label=$1 message=$2
    local opts=()
    shift 2
    for assignment in "$@"; do opts+=(-e "$assignment"); done
    run_sentinel "$label" pass "$(y_inputs "$label")" --network none "${opts[@]}" -- || true
    y_expect_refusal "$label" "$message"
}
ambient y10 'ambient package-manager configuration is unsupported: npm_config_ignore_scripts' npm_config_ignore_scripts=false
ambient y11 'ambient package-manager configuration is unsupported: NPM_CONFIG_REGISTRY' NPM_CONFIG_REGISTRY=http://127.0.0.1:9/
ambient y12 'ambient package-manager configuration is unsupported: Pnpm_Config_Minimum_Release_Age' Pnpm_Config_Minimum_Release_Age=0
ambient y13 'ambient package-manager configuration is unsupported: PNPM_CONFIG_MINIMUM_RELEASE_AGE' PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440
ambient y14 'ambient package-manager configuration is unsupported: COREPACK_ENABLE_STRICT' COREPACK_ENABLE_STRICT=0
ambient y15 'ambient package-manager configuration is unsupported: PNPM_HOME' PNPM_HOME=/tmp/p
ambient y16 'manager runtime overrides are unsupported: NODE_OPTIONS' NODE_OPTIONS=--require=/tmp/x.js
ambient y17 'manager runtime overrides are unsupported: NODE_PATH' NODE_PATH=/tmp
ambient y18 'TLS verification cannot be disabled' NODE_TLS_REJECT_UNAUTHORIZED=0
# Y19-Y23: the reviewed manager itself enforces policy (needs the registry).
run_sentinel y19 age-tighten "$(y_inputs y19)" --network bridge -- || true
[ "$Y_STATUS" -ne 0 ] || fail 'Y19: the age policy did not reject the committed lock'
grep -qF 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION' "$EVID/y19.log" || fail 'Y19: missing the age rejection'
grep -qF 'chrome-devtools-mcp@1.0.1' "$EVID/y19.log" || fail 'Y19: the rejection does not name the package'
[ "$Y_MODULES" = no ] || fail 'Y19: the failed install left a tree'
y_hooks() { # y_hooks LABEL EXPECTED(present|absent)
    local label=$1
    if [ "$2" = present ]; then
        for name in preinstall postinstall prepare; do
            [ -f "$EVID/y/$label-sentinel/hook-$name" ] || fail "$label: lifecycle script $name did not run"
        done
    else
        for name in preinstall postinstall prepare; do
            [ ! -f "$EVID/y/$label-sentinel/hook-$name" ] || fail "$label: lifecycle script $name ran"
        done
    fi
}
# The hook modes patch root scripts into package.json, so pnpm records
# pendingBuilds ["."] and the helper's policy-record check ends the install with
# a failure after the lifecycle step ran. The claim under test is whether the
# scripts ran, so the status is recorded rather than demanded.
run_sentinel y20 hooks "$(y_inputs y20)" --network bridge -- || true
y_hooks y20 absent
grep -qF 'pnpm did not record the reviewed policy' "$EVID/y20.log" \
    || fail 'Y20: the install did not stop at the policy-record check'
record y20_status "$Y_STATUS"
run_sentinel y21 hooks-enabled "$(y_inputs y21)" --network bridge -- || true
y_hooks y21 present
record y21_status "$Y_STATUS"
run_sentinel y22 hooks-no-flag "$(y_inputs y22)" --network bridge -- || true
y_hooks y22 absent
record y22_status "$Y_STATUS"
run_sentinel y23 weaken "$(y_inputs y23)" --network bridge -- || true
[ "$Y_STATUS" -ne 0 ] || fail 'Y23: a weakened policy was accepted'
grep -qF 'pnpm did not record the reviewed policy' "$EVID/y23.log" || fail 'Y23: missing the policy-record rejection'
pass
CURRENT=M
# Each mutation is a fresh, never-reused tag; the default build (phase B) is its
# positive control, so no English-word oracle is needed beyond these diagnostics.
if [ "$SKIP_MUTATIONS" = true ]; then
    echo 'M NOT-RUN' >> "$EVID/verdicts.txt"
    record mutations skipped
else
    for case in M1 M2 M3 M4; do
        ctx=$EVID/mutate/$case/context
        mcp=$ctx/chrome-devtools-mcp
        mkdir -p "$EVID/mutate/$case"
        cp -R examples/layers/chrome-devtools "$ctx"
        case "$case" in
            M1) printf ' ' >> "$mcp/pnpm-lock.yaml" ;;
            M2) rm "$mcp/pnpm-lock.yaml" ;;
            M3)
                sed 's/integrity: sha512-\(.*\)xg==}/integrity: sha512-\1xw==}/' "$mcp/pnpm-lock.yaml" > "$EVID/mutate/$case/rewrite"
                cat "$EVID/mutate/$case/rewrite" > "$mcp/pnpm-lock.yaml"
                ;;
            M4)
                jq '.dependencies["chrome-devtools-mcp"] = "0.0.0-agent-vm-missing"' "$mcp/package.json" > "$EVID/mutate/$case/rewrite"
                cat "$EVID/mutate/$case/rewrite" > "$mcp/package.json"
                sed 's/1\.0\.1/0.0.0-agent-vm-missing/g' "$mcp/pnpm-lock.yaml" > "$EVID/mutate/$case/rewrite"
                cat "$EVID/mutate/$case/rewrite" > "$mcp/pnpm-lock.yaml"
                ;;
        esac
        case "$case" in
            M1) expect='pnpm-lock.yaml: bytes differ from the reviewed pin' ;;
            M2) expect='missing input: pnpm-lock.yaml' ;;
            M3) expect='ERR_PNPM_TARBALL_INTEGRITY' ;;
            M4) expect='ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION' ;;
        esac
        case "$case" in
            M3 | M4) (
                cd "$mcp" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml > pnpm-inputs.sha256
            ) ;;
        esac
        status=0
        timed "mutate-$case" "$BUILD_SECONDS" docker build --builder "$BUILDER" --platform "$PLATFORM" \
            --progress=plain --label "$LABEL" -t "agent-vm-chrome-check:$RUN_ID-$case" "$ctx" || status=$?
        [ "$status" -ne 0 ] || fail "$case built despite the mutation"
        grep -qF "$expect" "$EVID/mutate-$case.log" || fail "$case lacks the expected diagnostic: $expect"
        if timed "mutate-$case-absent" "$DISCOVERY_SECONDS" docker image inspect "agent-vm-chrome-check:$RUN_ID-$case"; then
            fail "$case left a failed image behind"
        fi
    done
    record mutations 'M1 lock bytes, M2 missing lock, M3 integrity mismatch, M4 policy rejection'
    pass
fi
CURRENT=A
# Phase A: the committed bundle must verify cryptographically, not just bind
# statically (the static binding is contracts case 35-46).
timed gh-version "$DISCOVERY_SECONDS" gh --version
record gh_version "$(head -n 1 "$EVID/gh-version.log")"
MCP_SELECTED=$(sh "$MCP_HELPER" check "$MCP_INPUTS") || fail 'committed MCP inputs do not pass check'
record mcp_version "$MCP_SELECTED"
MCP_HEX=$(chrome_mcp_lock_integrity_hex "$MCP_INPUTS/pnpm-lock.yaml") || fail 'cannot read the locked tarball digest'
record mcp_tarball_sha512 "$MCP_HEX"
mkdir -p "$EVID/provenance"
chrome_mcp_fetch_tarball "$MCP_SELECTED" "$MCP_HEX" "$EVID/provenance/pkg.tgz" || fail 'published tarball does not match the lock'
cp "$MCP_BUNDLE" "$EVID/provenance/sigstore.json"
chrome_mcp_verify_bundle "$EVID/provenance/pkg.tgz" "$EVID/provenance/sigstore.json" "$MCP_SELECTED" "$MCP_HEX" "$EVID/provenance" \
    || fail 'committed provenance does not verify under the trusted identity'
record provenance verified
pass
CURRENT=$FINAL_PHASE
for phase in $PHASES; do
    if [ "$phase" != "$FINAL_PHASE" ]; then
        if [ "$phase" = M ] && [ "$SKIP_MUTATIONS" = true ]; then continue; fi
        grep -qx "$phase PASS" "$EVID/verdicts.txt" || fail "missing passing phase $phase"
    fi
done
pass
