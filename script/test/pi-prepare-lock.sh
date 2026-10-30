#!/usr/bin/env bash
# Contract tests for the public preparation interfaces
# images/tools/pi/prepare-lock.sh and images/tools/pi/bridge/prepare-lock.sh.
#
# Two layers are checked, and the split matters:
#
#   * the interface layer (a fake driver) pins the argument grammar every caller
#     depends on -- PREFIX VERSION [--refresh-lock], a repeated flag is usage 2,
#     unknown words are usage 2, and the driver's transaction statuses (0, 3,
#     128+N) reach the caller unchanged while every other failure is 1;
#
#   * the real-driver layer runs the actual images/tools/pi/prepare-project.py
#     against the real composed recipe-contract helper with a recording fake
#     manager, because "how many manager processes does this request start" is
#     the property the issue cares about: an equal/no-refresh request must start
#     ZERO, and an explicit `--refresh-lock` must deliberately start one, even for
#     the equal selection. A failure there must leave the four inputs byte
#     identical (no partial publication, no invented pin).
#
# The four inputs used by the real-driver layer are the committed projects, so
# the suite proves the interface and the transaction on the published selections.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd -P)"
PI_PREPARE="$REPO_ROOT/images/tools/pi/prepare-lock.sh"
BRIDGE_PREPARE="$REPO_ROOT/images/tools/pi/bridge/prepare-lock.sh"
TEST_ROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/pi-prepare-lock-test.XXXXXX")" && pwd -P)"
trap 'rm -rf "$TEST_ROOT"' EXIT

REAL_PYTHON="$(command -v python3 || true)"
[[ -n "$REAL_PYTHON" ]] || { echo 'FAIL: python3 is required' >&2; exit 1; }
CONTRACT_CONTEXT="$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)"
CONTRACT="$CONTRACT_CONTEXT/out"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_contains() {
    [[ "$1" == *"$2"* ]] || fail "expected [$2] in output, got [$1]"
}

CASE=""
RUN_STATUS=0
RUN_OUTPUT=""
RUN_ERROR=""

run() { # run INTERPRETER SCRIPT ENV... -- ARGS...
    local interpreter=$1 script=$2
    shift 2
    local -a env_args=()
    while [[ "$1" != -- ]]; do env_args+=("$1"); shift; done
    shift
    set +e
    env -i PATH="$PATH" HOME="$CASE" "${env_args[@]}" "$interpreter" "$script" "$@" \
        >"$CASE/out" 2>"$CASE/err"
    RUN_STATUS=$?
    set -e
    RUN_OUTPUT="$(cat "$CASE/out")"
    RUN_ERROR="$(cat "$CASE/err")"
}

DRIVER_LOG=""
new_case() {
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE"
    : >"$CASE/log"
    [ -z "$DRIVER_LOG" ] || : >"$DRIVER_LOG"
    RUN_STATUS=0
    RUN_OUTPUT=""
    RUN_ERROR=""
}

# --- the interface layer -----------------------------------------------------
# A fake driver records the argv it received and answers with a chosen status.
new_case grammar
DRIVER_DIR="$TEST_ROOT/grammar-driver"
mkdir -p "$DRIVER_DIR"
DRIVER_LOG="$DRIVER_DIR/driver.log"
: >"$DRIVER_LOG"
cp "$REPO_ROOT/script/test/fixtures/pi-prepare/driver.py" "$DRIVER_DIR/driver"
chmod +x "$DRIVER_DIR/driver"

for shell in pi bridge; do
    prepare=$PI_PREPARE
    [[ $shell == bridge ]] && prepare=$BRIDGE_PREPARE

    new_case "$shell-usage-none"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" --
    [[ $RUN_STATUS -eq 2 ]] || fail "$shell: no arguments must be usage 2 (got $RUN_STATUS)"
    assert_contains "$RUN_ERROR" 'usage: prepare-lock.sh PREFIX'

    new_case "$shell-usage-one"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" -- /some/prefix
    [[ $RUN_STATUS -eq 2 ]] || fail "$shell: one argument must be usage 2 (got $RUN_STATUS)"

    new_case "$shell-usage-unknown"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" -- \
        /prefix '' --frobnicate
    [[ $RUN_STATUS -eq 2 ]] || fail "$shell: an unknown flag must be usage 2 (got $RUN_STATUS)"

    new_case "$shell-usage-duplicate-refresh"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" -- \
        /prefix '' --refresh-lock --refresh-lock
    [[ $RUN_STATUS -eq 2 ]] || fail "$shell: a repeated --refresh-lock must be usage 2 (got $RUN_STATUS)"
    assert_contains "$RUN_ERROR" 'usage: prepare-lock.sh PREFIX'
    [[ ! -s "$DRIVER_LOG" ]] ||
        fail "$shell: a usage error still started the driver"

    new_case "$shell-driver-argv"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" -- /a/prefix 1.2.3
    [[ $RUN_STATUS -eq 0 ]] || fail "$shell: a normal request must succeed (got $RUN_STATUS)"
    assert_contains "$(cat "$DRIVER_LOG")" \
        "--consumer $shell prepare /a/prefix 1.2.3"
    : >"$DRIVER_LOG"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" -- /a/prefix '' --refresh-lock
    [[ $RUN_STATUS -eq 0 ]] || fail "$shell: a refresh request must succeed (got $RUN_STATUS)"
    assert_contains "$(cat "$DRIVER_LOG")" \
        "--consumer $shell prepare /a/prefix  --refresh-lock"

    for status in 0 1 3 130 143 129; do
        new_case "$shell-status-$status"
        expected=$status
        # Everything that is not a defined transaction status collapses to 1.
        case $status in 0 | 1 | 3 | 130 | 143 | 129) ;; esac
        run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
            FAKE_DRIVER_LOG="$DRIVER_LOG" FAKE_DRIVER_STATUS="$status" -- /a/prefix 1.2.3
        [[ $RUN_STATUS -eq $expected ]] ||
            fail "$shell: driver status $status must reach the caller as $expected (got $RUN_STATUS)"
    done
    new_case "$shell-status-other"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$DRIVER_DIR/driver" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" FAKE_DRIVER_STATUS=7 -- /a/prefix 1.2.3
    [[ $RUN_STATUS -eq 1 ]] || fail "$shell: an undefined driver status must be 1 (got $RUN_STATUS)"

    new_case "$shell-driver-missing"
    run sh "$prepare" env AGENT_VM_PI_PREPARE_PROJECT="$CASE/no-such-driver.py" \
        FAKE_DRIVER_LOG="$DRIVER_LOG" -- /a/prefix 1.2.3
    [[ $RUN_STATUS -eq 1 ]] || fail "$shell: a missing driver must be a hard failure"
    assert_contains "$RUN_ERROR" 'the owning preparation driver is missing at'
done

# --- the real driver and the reviewed transaction ---------------------------
# The four inputs are the committed ones; the manager is a recording fake that
# never resolves anything, so this layer can only prove *whether* a manager was
# started and that nothing was published.
[[ -d "$REPO_ROOT/images/tools/pi" ]] || fail "the committed Pi project is missing"

real_case() { # real_case NAME CONSUMER
    local input_dir
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/root/pnpm" "$CASE/root/$2"
    case "$2" in
        pi) input_dir="$REPO_ROOT/images/tools/pi" ;;
        bridge) input_dir="$REPO_ROOT/images/tools/pi/bridge" ;;
    esac
    cp "$input_dir"/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} \
        "$CASE/root/$2/"
    cp "$REPO_ROOT/images/pnpm/pin.json" "$CASE/root/pnpm/pin.json"
    # The fake manager must live inside the authorized fixture root: a manager
    # path is executable policy, so it is authorized like the contract directory.
    cat >"$CASE/root/fake-pnpm" <<SH
#!/bin/sh
printf '%s\n' "\$*" >>"$CASE/manager.log"
case "\$1" in
    --version) echo 11.11.0; exit 0 ;;
esac
exit 1
SH
    chmod +x "$CASE/root/fake-pnpm"
    : >"$CASE/manager.log"
}

# The reviewed hard runner needs the image's coreutils (`timeout`), which this
# host does not necessarily have on the driver's fixed PATH. A fake contract lets
# the *manager-argv* layer still prove which call was attempted and that a failure
# publishes nothing; the equal and invalid-selection cases above use the real
# composed helper.
fake_contract() { # fake_contract DIR
    mkdir -p "$1"
    cat >"$1/recipe-contract" <<'SH'
#!/bin/sh
case "$1" in
    pnpm-environment) exit 0 ;;
    pnpm-lock) shift; case "$1" in check | graph) echo '{}' ;; esac; exit 0 ;;
esac
exit 0
SH
    chmod +x "$1/recipe-contract"
    cat >"$1/run-pnpm.sh" <<'SH'
#!/bin/sh
# Stand-in for the reviewed hard runner: argv is RESULT_FILE -- COMMAND ARG...
[ "$#" -ge 3 ] && [ "$2" = -- ] || exit 2
shift 2
exec "$@"
SH
    chmod +x "$1/run-pnpm.sh"
}

digest_inputs() { # digest_inputs DIR
    (cd "$1" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256) \
        2>/dev/null || (cd "$1" && shasum -a 256 package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)
}

for consumer in pi bridge; do
    if [[ $consumer == pi ]]; then prepare=$PI_PREPARE; else prepare=$BRIDGE_PREPARE; fi
    case "$consumer" in
        pi) input_dir="$REPO_ROOT/images/tools/pi" ;;
        bridge) input_dir="$REPO_ROOT/images/tools/pi/bridge" ;;
    esac
    [[ -d "$input_dir" ]] || fail "the committed $consumer project is missing"

    # An equal, unrefreshed request: zero manager processes, byte-identical inputs.
    real_case "$consumer-equal-zero-manager" "$consumer"
    prefix="$CASE/root/$consumer"
    before="$(digest_inputs "$prefix")"
    run sh "$prepare" env AGENT_VM_FIXTURE_ROOT="$CASE/root" \
        AGENT_VM_FIXTURE_PNPM="$CASE/root/fake-pnpm" AGENT_VM_CONTRACT_DIR="$CONTRACT" -- \
        "$prefix" ''
    [[ $RUN_STATUS -eq 0 ]] || fail "$consumer: an equal request failed: $RUN_ERROR"
    assert_contains "$RUN_OUTPUT" '"managerCalls": 0'
    [[ ! -s "$CASE/manager.log" ]] ||
        fail "$consumer: an equal request started a package manager: $(cat "$CASE/manager.log")"
    [[ "$(digest_inputs "$prefix")" == "$before" ]] ||
        fail "$consumer: an equal request changed the reviewed inputs"

    # An explicit refresh of the equal selection MUST start the manager (that is
    # the developer maintenance path), and its failure must publish nothing.
    real_case "$consumer-refresh-seeds" "$consumer"
    fake_contract "$CASE/root/contract"
    fake_contract_contract="$CASE/root/contract"
    prefix="$CASE/root/$consumer"
    before="$(digest_inputs "$prefix")"
    run sh "$prepare" env AGENT_VM_FIXTURE_ROOT="$CASE/root" \
        AGENT_VM_FIXTURE_PNPM="$CASE/root/fake-pnpm" \
        AGENT_VM_CONTRACT_DIR="$fake_contract_contract" -- \
        "$prefix" '' --refresh-lock
    [[ $RUN_STATUS -eq 1 ]] || fail "$consumer: the failing refresh must be a hard failure (got $RUN_STATUS): $RUN_ERROR"
    grep -q '^--version ' "$CASE/manager.log" ||
        fail "$consumer: an explicit refresh did not even check the manager identity"
    if [[ $consumer == pi ]]; then
        # Pi's refresh is not an incremental range resolve: the route is decided by
        # the *authenticated* selected archive, so the first call after the manager
        # identity is the exact metadata query. The fake manager refuses it, which
        # is the failure this case proves publishes nothing.
        grep -q '^view @earendil-works/pi-coding-agent@' "$CASE/manager.log" ||
            fail "pi: an explicit refresh did not authenticate the selected archive: $(cat "$CASE/manager.log")"
        ! grep -q '^install --lockfile-only' "$CASE/manager.log" ||
            fail 'pi: the selected archive was resolved by range without authenticating it'
    else
        grep -q '^install --lockfile-only' "$CASE/manager.log" ||
            fail "$consumer: an explicit refresh did not seed a resolve: $(cat "$CASE/manager.log")"
    fi
    [[ "$(digest_inputs "$prefix")" == "$before" ]] ||
        fail "$consumer: a failed refresh changed the reviewed inputs"
    (cd "$prefix" && AGENT_VM_CONTRACT_DIR="$CONTRACT" "$CONTRACT/recipe-contract" \
        pnpm-lock check "$consumer" "$prefix") >/dev/null ||
        fail "$consumer: the reviewed inputs no longer pass the pinned-input check"
    [[ "$(digest_inputs "$prefix")" == "$before" ]] ||
        fail "$consumer: the pins changed after a failed refresh"

    # An invalid selection is refused before any manager is started.
    real_case "$consumer-invalid-version" "$consumer"
    prefix="$CASE/root/$consumer"
    run sh "$prepare" env AGENT_VM_FIXTURE_ROOT="$CASE/root" \
        AGENT_VM_FIXTURE_PNPM="$CASE/root/fake-pnpm" AGENT_VM_CONTRACT_DIR="$CONTRACT" -- \
        "$prefix" 'latest'
    [[ $RUN_STATUS -eq 1 ]] || fail "$consumer: a range selection must be hard (got $RUN_STATUS)"
    assert_contains "$RUN_ERROR" "not an exact npm version: 'latest'"
    [[ ! -s "$CASE/manager.log" ]] ||
        fail "$consumer: an invalid selection started a manager: $(cat "$CASE/manager.log")"
done

echo 'pi-prepare-lock contract tests passed'
