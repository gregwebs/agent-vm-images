#!/usr/bin/env bash
# Black-box contract tests for the dsh build gate (images/tools/dsh/verify-dsh.sh)
# and for the bounded web readiness driver (images/tools/dsh/probe-dsh.py).
#
# No Docker, no npm, no real dsh: the target is a fixture prefix that mirrors the
# physical pnpm isolated layout with stub packages, and the web server is a
# native fixture (script/test/fixtures/dsh/web.py). That controls the gate's
# control flow only -- it never claims the real harness works. Every negative the
# gate must reject is exercised here: missing commands, the exit-0-with-no-output
# trap an old Node produces, print-then-fail, an extra line, a disagreeing slot,
# a same-version shadow (dsh and pnpm), a wrong bin mapping, a private pnpm, an
# escaping sandbox, every runtime probe failing in isolation, every adversarial
# web startup shape, stale status reset, soft-mode invariance, idempotent reruns
# and caller-HOME isolation.

set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
GATE="$REPO_ROOT/images/tools/dsh/verify-dsh.sh"
SOURCE_DIR="$REPO_ROOT/images/tools/dsh"
# The gate executes its contract helpers, so the native composed directory is
# built once and copied into each case's own scratch checkout: that keeps the
# explicit and unset contract-directory shapes authorized by the fixture root and
# gives both a real binary instead of a source-only tree.
CONTRACT_CONTEXT="$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)"
CONTRACT=""
FIXTURES="$REPO_ROOT/script/test/fixtures/dsh"
PNPM_FIXTURES="$REPO_ROOT/script/test/fixtures/pnpm"
TEST_ROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/dsh-verify-test.XXXXXX")" && pwd -P)"

REAL_JQ="$(command -v jq || true)"
REAL_TIMEOUT="$(command -v timeout || true)"
REAL_PYTHON="$(command -v python3 || true)"
REAL_NODE="$(command -v node || true)"
for tool in REAL_JQ REAL_TIMEOUT REAL_PYTHON REAL_NODE; do
    [[ -n "${!tool}" ]] || { echo "FAIL: ${tool#REAL_} is required" >&2; exit 1; }
done

# The probe's ownership proof must fail closed when procfs cannot be read; that is
# a unit property of the driver, so it is checked before the fixture black box.
"$REAL_PYTHON" "$SOURCE_DIR/probe-dsh.py" --self-test

cleanup_test_root() {
    # A detached descendant fixture deliberately escapes the probe's process
    # group; it records its pid so the suite can still leave nothing behind.
    local pidfile
    for pidfile in "$TEST_ROOT"/*/leak.pid "$TEST_ROOT"/*/*/leak.pid; do
        [ -f "$pidfile" ] || continue
        kill "$(cat "$pidfile")" 2>/dev/null || true
    done
    rm -rf "$TEST_ROOT"
    # Only the decoy this run created is removed, never a real build mount.
    if [ "${CONTRACT_DECOY_CREATED:-}" = 1 ]; then rm -rf "$CONTRACT_DECOY"; fi
}
trap cleanup_test_root EXIT

chmod 0755 "$TEST_ROOT"
mkdir -p "$TEST_ROOT/prerequisites" "$TEST_ROOT/tmp"
chmod 0755 "$TEST_ROOT/prerequisites" "$TEST_ROOT/tmp"
# Do not expose unrelated host-installed agents through the gate's PATH.
for tool in "$REAL_JQ" "$REAL_TIMEOUT" "$REAL_PYTHON" "$REAL_NODE"; do
    ln -s "$tool" "$TEST_ROOT/prerequisites/${tool##*/}"
done

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_contains() {
    [[ "$1" == *"$2"* ]] || fail "expected output to contain: $2 (got: $1)"
}

DSH_VERSION=0.0.0-fixture
PNPM_VERSION=11.7.0
CASE=""
RUN_OUTPUT=""
RUN_STATUS=0
RUN_GATE_CONTRACT="default"
PROBE_OUTPUT=""
PROBE_STATUS=0
AGENT_VM_VERSION_DSH=""
AGENT_VM_VERSION_PNPM=""
AGENT_INSTALL_SOFT_FAIL=""
DSH_FIXTURE_BEHAVIOUR=""
DSH_FIXTURE_VERSION=""
DSH_WEB_FIXTURE_CASE=""
PNPM_FIXTURE_VERSION_OVERRIDE=""
PATH_PREFIX=""

new_case() {
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/bin" "$CASE/status" "$CASE/home/scratch" "$CASE/caller-home" "$CASE/exec-temp"
    chmod 0755 "$CASE" "$CASE/bin" "$CASE/status" "$CASE/home" "$CASE/home/scratch" "$CASE/caller-home" "$CASE/exec-temp"
    printf 'caller marker\n' >"$CASE/caller-home/marker"
    RUN_OUTPUT=""
    RUN_STATUS=0
    AGENT_VM_VERSION_DSH=""
    AGENT_VM_VERSION_PNPM=""
    AGENT_INSTALL_SOFT_FAIL=""
    DSH_FIXTURE_BEHAVIOUR=""
    DSH_FIXTURE_VERSION=""
    DSH_WEB_FIXTURE_CASE="valid"
    PNPM_FIXTURE_VERSION_OVERRIDE=""
    PATH_PREFIX=""
    RUN_GATE_CONTRACT="default"
}

stage_prefix() {
    local mode="${1:-healthy}"
    python3 "$FIXTURES/install.py" "$CASE/prefix" --version "$DSH_VERSION" \
        --pnpm "$PNPM_VERSION" --mode "$mode"
    ln -s "$CASE/prefix/node_modules/@deepseek-ai/dsh/lib/bin.js" "$CASE/bin/dsh"
    # project.py only needs a confined base-pin record; the gate cases replace
    # this with the authenticated verifier fixture pin below.
    mkdir -p "$CASE/pnpm"
    printf '{"version":"%s"}\n' "$PNPM_VERSION" >"$CASE/pnpm/pin.json"
    chmod 0644 "$CASE/pnpm/pin.json"
}

stage_pnpm() {
    # The fixture makes this body the published launcher, so it must answer
    # `--version` from PNPM_FIXTURE_VERSION like the reviewed launcher stub.
    printf '#!/bin/sh\necho "${PNPM_FIXTURE_VERSION}"\n' >"$CASE/bin/pnpm"
    chmod 0755 "$CASE/bin/pnpm"
    mkdir -p "$CASE/fixture"
    python3 "$PNPM_FIXTURES/verifier-fixture.py" "$CASE/fixture" "$PNPM_VERSION" "$CASE/bin/pnpm"
    for alias in pnx pnpx; do ln -s pnpm "$CASE/bin/$alias"; done
    mkdir -p "$CASE/pnpm"
    cp "$CASE/fixture/opt/agent-vm/pnpm/pin.json" "$CASE/pnpm/pin.json"
}

stage() {
    stage_prefix "${1:-healthy}"
    stage_pnpm
    # A copied checkout owns its own contract directory: the gate's module derives
    # it from its own location, so the copy must sit under this case's root.
    mkdir -m 0755 "$CASE/checkout"
    mkdir -m 0755 "$CASE/checkout/tools"
    cp -R "$SOURCE_DIR" "$CASE/checkout/tools/dsh"
    cp -R "$CONTRACT_CONTEXT/out" "$CASE/checkout/recipe-contract"
    chmod -R a+rX "$CASE/checkout"
    CASE_SOURCE_DIR="$CASE/checkout/tools/dsh"
    CONTRACT="$CASE/checkout/recipe-contract"
}

run_gate() {
    # The contract directory is the only caller-selected input that executes code,
    # so one case runs the gate with it unset: the gate must then derive the
    # authorized directory itself instead of falling back to a fixed mount path.
    local -a contract_env=("AGENT_VM_CONTRACT_DIR=$CONTRACT")
    if [ "$RUN_GATE_CONTRACT" = "unset" ]; then contract_env=(); fi
    set +e
    RUN_OUTPUT="$(env -i \
        "PATH=${PATH_PREFIX:+$PATH_PREFIX:}$CASE/bin:$TEST_ROOT/prerequisites:/usr/bin:/bin" \
        "HOME=$CASE/caller-home" \
        "TMPDIR=$TEST_ROOT/tmp" \
        "AGENT_VM_FIXTURE_ROOT=$CASE" \
        "AGENT_VM_DSH_PREFIX=$CASE/prefix" \
        "AGENT_VM_DSH_SOURCE_DIR=${CASE_SOURCE_DIR:-$SOURCE_DIR}" \
        "AGENT_VM_DSH_COMMAND=$CASE/bin/dsh" \
        ${contract_env[@]+"${contract_env[@]}"} \
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status" \
        "AGENT_VM_VERSION_DSH=${AGENT_VM_VERSION_DSH-}" \
        "AGENT_VM_VERSION_PNPM=${AGENT_VM_VERSION_PNPM-}" \
        "AGENT_INSTALL_SOFT_FAIL=${AGENT_INSTALL_SOFT_FAIL-}" \
        "PNPM_FIXTURE_DESTINATION=$CASE/fixture" \
        "PNPM_SOURCE_DIR=$CASE/fixture/pnpm-source" \
        "PNPM_FIXTURE_VERSION=${PNPM_FIXTURE_VERSION_OVERRIDE:-$PNPM_VERSION}" \
        "DSH_FIXTURE_BEHAVIOUR=${DSH_FIXTURE_BEHAVIOUR-}" \
        "DSH_FIXTURE_VERSION=${DSH_FIXTURE_VERSION-}" \
        "DSH_WEB_FIXTURE_CASE=${DSH_WEB_FIXTURE_CASE:-valid}" \
        "DSH_WEB_FIXTURE_SCRIPT=$FIXTURES/web.py" \
        "DSH_WEB_FIXTURE_PYTHON=$REAL_PYTHON" \
        sh "$GATE" 2>&1)"
    RUN_STATUS=$?
    set -e
}

status_record() {
    cat "$CASE/status/dsh" 2>/dev/null || true
}

probe_argv() {
    local readiness="$1" escalation="$2"
    PROBE_ARGS=(--prefix "$CASE/prefix"
        --command "$CASE/bin/dsh"
        --scratch-root "$CASE/home/scratch" --readiness-seconds "$readiness")
    if [ -n "$escalation" ]; then
        PROBE_ARGS+=(--escalation-seconds "$escalation")
    fi
}

probe_env() {
    local web_case="$1"
    PROBE_ENV=(
        "PATH=$TEST_ROOT/prerequisites:/usr/bin:/bin"
        "HOME=$CASE/home"
        "AGENT_VM_FIXTURE_ROOT=$CASE"
        "AGENT_VM_FIXTURE_EXEC_TMPDIR=$CASE/exec-temp"
        "DSH_WEB_FIXTURE_CASE=$web_case"
        "DSH_WEB_FIXTURE_TMP_RECORD=$CASE/exec-temp-path"
        "DSH_WEB_FIXTURE_SCRIPT=$FIXTURES/web.py"
        "DSH_WEB_FIXTURE_PYTHON=$REAL_PYTHON"
        "DSH_WEB_FIXTURE_PIDFILE=$CASE/leak.pid"
        "DSH_WEB_FIXTURE_DELAY=${DSH_WEB_FIXTURE_DELAY-}"
    )
}

run_probe() {
    local web_case="$1" readiness="$2" escalation="${3:-}"
    probe_argv "$readiness" "$escalation"
    probe_env "$web_case"
    set +e
    PROBE_OUTPUT="$(env -i "${PROBE_ENV[@]}" \
        python3 "$SOURCE_DIR/probe-dsh.py" "${PROBE_ARGS[@]}" 2>&1)"
    PROBE_STATUS=$?
    set -e
    [[ -z "$(find "$CASE/exec-temp" -mindepth 1 -print -quit)" ]] || fail 'exec TMPDIR survived success/failure'
}

# --- the first case: a same-version raw pnpm is not the reviewed launcher ----

new_case raw-same-version-shadow
stage
rm "$CASE/bin/pnpm"
printf '#!/bin/sh\necho 11.7.0\n' >"$CASE/bin/pnpm"
chmod 0755 "$CASE/bin/pnpm"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a same-version raw pnpm must fail the identity audit'
assert_contains "$RUN_OUTPUT" 'unexpected first PATH candidate'

# --- missing commands are hard failures, soft mode cannot change that --------

new_case missing-dsh
stage_prefix
rm "$CASE/bin/dsh"
stage_pnpm
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a missing dsh must be a hard failure'
assert_contains "$RUN_OUTPUT" 'dsh: MISSING'

new_case missing-dsh-soft
stage_prefix
rm "$CASE/bin/dsh"
stage_pnpm
AGENT_INSTALL_SOFT_FAIL=1
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'dsh is never soft-failable'
assert_contains "$RUN_OUTPUT" 'dsh: MISSING'

new_case missing-pnpm
stage_prefix
stage_pnpm
rm "$CASE/bin/pnpm" "$CASE/bin/pnx" "$CASE/bin/pnpx"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a missing pnpm must be a hard failure'
assert_contains "$RUN_OUTPUT" 'pnpm: MISSING'

# --- the old-Node trap: exit 0, no output -----------------------------------

new_case empty-version
stage
DSH_FIXTURE_BEHAVIOUR=empty
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'an empty --version must be a hard failure'
assert_contains "$RUN_OUTPUT" 'empty --version output'

new_case empty-version-soft
stage
DSH_FIXTURE_BEHAVIOUR=empty
AGENT_INSTALL_SOFT_FAIL=1
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'an empty --version must fail even under soft-fail'

# --- version mismatch (dsh from the manifest, pnpm from the base pin) --------

new_case wrong-dsh
stage
DSH_FIXTURE_VERSION=9.9.9
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a dsh version differing from the pin must fail'
assert_contains "$RUN_OUTPUT" 'installed 9.9.9 but the manifest pins 0.0.0-fixture'

new_case wrong-pnpm
stage
PNPM_FIXTURE_VERSION_OVERRIDE=9.9.9
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a pnpm version differing from the base pin must fail'
assert_contains "$RUN_OUTPUT" 'installed 9.9.9 but the base pin is 11.7.0'

# --- status is checked BEFORE parsing ---------------------------------------

new_case nonzero-after-print
stage
DSH_FIXTURE_BEHAVIOUR=print-then-fail
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'printing the pin then exiting nonzero must fail'
assert_contains "$RUN_OUTPUT" '--version exited 1'

new_case extra-line
stage
DSH_FIXTURE_BEHAVIOUR=extra-line
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'extra --version output must fail'
assert_contains "$RUN_OUTPUT" 'unexpected --version output'

# --- supplied slots must agree with the effective selection -----------------

new_case supplied-mismatch
stage
AGENT_VM_VERSION_DSH=9.9.9
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a supplied dsh slot disagreeing with the manifest must fail'
assert_contains "$RUN_OUTPUT" 'layer asked for 9.9.9 but the manifest pins 0.0.0-fixture'

new_case supplied-pnpm-mismatch
stage
AGENT_VM_VERSION_PNPM=11.6.0
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a supplied pnpm slot disagreeing with the base pin must fail'
assert_contains "$RUN_OUTPUT" 'layer asked for 11.6.0 but the base pin is 11.7.0'

new_case package-manager-mismatch
stage_prefix
python3 "$REPO_ROOT/script/test/dsh-verify-mutate-manager.py" "$CASE/prefix/package.json"
stage_pnpm
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a manifest packageManager disagreeing with the base pin must fail'
assert_contains "$RUN_OUTPUT" 'disagrees with base pin'

# --- a same-version different file for dsh is not the installed command -----

new_case dsh-shadow
stage
mkdir "$CASE/shadow"
chmod 0755 "$CASE/shadow"
printf '#!/bin/sh\necho 0.0.0-fixture\n' >"$CASE/shadow/dsh"
chmod 0755 "$CASE/shadow/dsh"
PATH_PREFIX="$CASE/shadow"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a same-version dsh shadow must fail the identity audit'
assert_contains "$RUN_OUTPUT" 'first PATH candidate'

# --- a symlink to the same file from another directory is normal layout -----

new_case samefile-symlink
stage
mkdir "$CASE/shadow"
chmod 0755 "$CASE/shadow"
ln -s "$CASE/prefix/node_modules/@deepseek-ai/dsh/lib/bin.js" "$CASE/shadow/dsh"
PATH_PREFIX="$CASE/shadow"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "a symlink chain to the same dsh file must pass: $RUN_OUTPUT"
[[ "$(status_record)" == "installed" ]] || fail 'a successful samefile run must record installed'

# --- packaging invariants are enforced by native code -----------------------

new_case wrong-bin-mapping
stage bin-wrong
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a wrong bin mapping must fail the layout check'
assert_contains "$RUN_OUTPUT" 'bin layout'

new_case private-pnpm
stage private-pnpm
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a private pnpm payload must fail'
assert_contains "$RUN_OUTPUT" 'second package manager'

new_case escaping-sandbox
stage escaping-sandbox
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a sandbox resolving outside the prefix must fail'
assert_contains "$RUN_OUTPUT" 'escapes the installed prefix'

# --- happy path: record installed -------------------------------------------

new_case happy
stage
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "the happy path must succeed: $RUN_OUTPUT"
assert_contains "$RUN_OUTPUT" 'dsh: 0.0.0-fixture (pnpm 11.7.0)'
assert_contains "$RUN_OUTPUT" 'tool-access: ok'
[[ "$(status_record)" == "installed" ]] || fail 'the gate must record installed'
[[ -f "$CASE/caller-home/marker" ]] || fail 'the caller HOME must not be touched'
[[ "$(cat "$CASE/caller-home/marker")" == 'caller marker' ]] || fail 'the caller HOME marker changed'

# --- the happy path leaves no scratch, listener or process behind -----------

new_case idempotent
stage
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "the first run must succeed: $RUN_OUTPUT"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "the second run must succeed: $RUN_OUTPUT"
[[ "$(status_record)" == "installed" ]] || fail 'the rerun must record installed'
leftover=$(find "$CASE" -maxdepth 1 -name 'dsh-*' | sed -n '1p')
[[ -z "$leftover" ]] || fail "a scratch directory leaked: $leftover"
leftover=$(find "$CASE/home/scratch" -mindepth 1 | sed -n '1p')
[[ -z "$leftover" ]] || fail "a runtime scratch directory leaked: $leftover"

# --- the executed contract helpers come from the authorized directory -------

# With AGENT_VM_CONTRACT_DIR unset the gate must resolve the directory the guard
# authorized (this checkout's copy) instead of a fixed build-mount path.
new_case contract-unset
stage
RUN_GATE_CONTRACT="unset"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "an unset contract directory must resolve to the authorized copy: $RUN_OUTPUT"
assert_contains "$RUN_OUTPUT" 'tool-access: ok'
[[ "$(status_record)" == "installed" ]] || fail 'the unset-env run must record installed'

# A host checkout authorizes a *source-only* contract directory, which holds no
# binary. Exec would then fail with 127 and no reason; the gate must name the
# missing helper instead and keep its own status.
new_case contract-source-only
stage
rm -f "$CASE/checkout/recipe-contract/recipe-contract"
RUN_GATE_CONTRACT="unset"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail "a source-only contract directory must fail with status 1: $RUN_STATUS $RUN_OUTPUT"
assert_contains "$RUN_OUTPUT" 'no executable recipe-contract'
assert_contains "$RUN_OUTPUT" "$CASE/checkout/recipe-contract"

# `/tmp/recipe-contract` is a build mount, not a directory this gate may execute
# from. A decoy there must never run: the helpers come from contract-dir, so an
# unset environment still resolves the authorized copy. Skipped only if a real
# mount already occupies the path.
CONTRACT_DECOY=/tmp/recipe-contract
if [ -e "$CONTRACT_DECOY" ]; then
    echo "note: $CONTRACT_DECOY exists; the contract decoy case is skipped" >&2
else
    new_case contract-decoy
    stage
    mkdir -m 0755 "$CONTRACT_DECOY"
    CONTRACT_DECOY_CREATED=1
    for helper in run-report.sh recipe-contract; do
        printf '#!/bin/sh\nprintf executed >%s/contract-decoy-executed\n' "$CASE" >"$CONTRACT_DECOY/$helper"
        chmod 0755 "$CONTRACT_DECOY/$helper"
    done
    RUN_GATE_CONTRACT="unset"
    run_gate
    [[ ! -e "$CASE/contract-decoy-executed" ]] || fail 'the gate executed a helper from the fixed build-mount path'
    [[ $RUN_STATUS -eq 0 ]] || fail "a decoy contract must not change the outcome: $RUN_OUTPUT"
    [[ "$(status_record)" == "installed" ]] || fail 'the decoy run must record installed'
fi

# --- a stale installed record is reset before the gate measures -------------

new_case stale-installed
stage missing-sandbox
printf 'installed\n' >"$CASE/status/dsh"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a missing sandbox must fail'
[[ "$(status_record)" == "pending" ]] || fail 'a failed rerun must reset the record to pending'

# --- soft mode cannot change any hard failure -------------------------------

for soft in unset 1 0; do
    new_case "soft-$soft"
    stage missing-sandbox
    if [ "$soft" = unset ]; then
        unset AGENT_INSTALL_SOFT_FAIL
    else
        AGENT_INSTALL_SOFT_FAIL="$soft"
    fi
    run_gate
    AGENT_INSTALL_SOFT_FAIL=""
    [[ $RUN_STATUS -ne 0 ]] || fail "soft mode $soft must not change a hard runtime failure"
    [[ "$(status_record)" != "installed" ]] || fail "soft mode $soft must not record installed"
done

# --- each runtime probe failing in isolation blocks installed ---------------

for mode in missing-sandbox missing-node-pty missing-koffi missing-pi-ai genai-missing \
    genai-wrong-export pty-no-marker pty-exit-nonzero koffi-wrong-pid \
    protobuf-bad-roundtrip subprocess-empty-output subprocess-nonzero subprocess-wrong-cordis; do
    new_case "probe-$mode"
    stage "$mode"
    run_gate
    [[ $RUN_STATUS -ne 0 ]] || fail "probe fixture $mode must fail the gate"
    [[ "$(status_record)" != "installed" ]] || fail "probe fixture $mode must not record installed"
done

# --- a web startup failure blocks installed even when reports are exact -----

for web in hmr-error sandbox-error early-exit bare-200 wrong-body; do
    new_case "web-$web"
    stage
    DSH_WEB_FIXTURE_CASE="$web"
    run_gate
    [[ $RUN_STATUS -ne 0 ]] || fail "web fixture $web must fail the gate"
    [[ "$(status_record)" != "installed" ]] || fail "web fixture $web must not record installed"
done

# --- the bounded web driver itself ------------------------------------------

new_case probe-valid
stage_prefix
run_probe valid 10
[[ $PROBE_STATUS -eq 0 ]] || fail "the web driver must accept a valid server: $PROBE_OUTPUT"
assert_contains "$PROBE_OUTPUT" '"status": 200'
assert_contains "$PROBE_OUTPUT" '"redirects": 1'
assert_contains "$PROBE_OUTPUT" '"portReleased": true'
[[ "$PROBE_OUTPUT" != *'token='* ]] || fail 'the driver must never print the session token'

new_case probe-exec-temp
stage_prefix
run_probe exec-temp 10
[[ $PROBE_STATUS -eq 0 ]] || fail "private exec TMPDIR failed: $PROBE_OUTPUT"
[[ -f "$CASE/exec-temp-path" ]] || fail 'child did not inspect private temp'
[[ ! -e "$(cat "$CASE/exec-temp-path")" ]] || fail 'private exec TMPDIR leaked'
assert_contains "$PROBE_OUTPUT" '"execTmpRemoved": true'

new_case probe-early-exit
stage_prefix
run_probe early-exit 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'an early exit must fail readiness'
assert_contains "$PROBE_OUTPUT" 'web exited before readiness'

new_case probe-hmr-error
stage_prefix
run_probe hmr-error 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'an HMR failure must fail readiness'
assert_contains "$PROBE_OUTPUT" 'Cordis HMR service'

new_case probe-sandbox-error
stage_prefix
run_probe sandbox-error 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'a sandbox resolution failure must fail readiness'
assert_contains "$PROBE_OUTPUT" 'dsh-sandbox-local'

new_case probe-delayed-valid
stage_prefix
DSH_WEB_FIXTURE_DELAY=0.4
run_probe delayed-valid 10
[[ $PROBE_STATUS -eq 0 ]] || fail "a delayed valid server must still be ready: $PROBE_OUTPUT"

new_case probe-wrong-body
stage_prefix
run_probe wrong-body 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'a wrong body must not count as ready'
assert_contains "$PROBE_OUTPUT" 'not the shipped dsh frontend'

new_case probe-wrong-status
stage_prefix
run_probe wrong-status 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'a wrong status must not count as ready'
assert_contains "$PROBE_OUTPUT" 'HTTP 500'

new_case probe-bare-200
stage_prefix
run_probe bare-200 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'a valid page that rejects our token must not count as ready'
assert_contains "$PROBE_OUTPUT" 'HTTP 404'

new_case probe-changed-origin
stage_prefix
run_probe changed-origin 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'a cross-origin redirect must not count as ready'
assert_contains "$PROBE_OUTPUT" 'left the probed origin'

new_case probe-double-redirect
stage_prefix
run_probe double-redirect 10
[[ $PROBE_STATUS -eq 1 ]] || fail 'more than one redirect must not count as ready'
assert_contains "$PROBE_OUTPUT" 'HTTP 302'

new_case probe-hang
stage_prefix
run_probe hang 0.5 0.3
[[ $PROBE_STATUS -eq 1 ]] || fail 'a child that never becomes ready must fail'
assert_contains "$PROBE_OUTPUT" 'readiness deadline'

new_case probe-ignore-term
stage_prefix
run_probe ignore-term 10 0.3
[[ $PROBE_STATUS -eq 0 ]] || fail "an ignored TERM must still be escalated and cleaned: $PROBE_OUTPUT"
assert_contains "$PROBE_OUTPUT" 'SIGKILL'
assert_contains "$PROBE_OUTPUT" '"survivors": 0'

new_case probe-descendant
stage_prefix
run_probe descendant 10 0.5
[[ $PROBE_STATUS -eq 0 ]] || fail "a same-group descendant must be cleaned with the group: $PROBE_OUTPUT"
assert_contains "$PROBE_OUTPUT" '"portReleased": true'

new_case probe-escaping-descendant
stage_prefix
run_probe escaping-descendant 10 0.5
[[ $PROBE_STATUS -eq 1 ]] || fail 'a survivor outside the group holding the port must fail cleanup'
if [[ "$PROBE_OUTPUT" != *'still has a listener'* && "$PROBE_OUTPUT" != *'is not owned by the probed process group'* ]]; then
    fail "a survivor holding the reserved port must fail cleanup: $PROBE_OUTPUT"
fi
if [ -f "$CASE/leak.pid" ]; then
    kill "$(cat "$CASE/leak.pid")" 2>/dev/null || true
fi

new_case probe-interrupt
stage_prefix
probe_argv 10 ""
probe_env hang
set +e
env -i "${PROBE_ENV[@]}" python3 "$SOURCE_DIR/probe-dsh.py" "${PROBE_ARGS[@]}" \
    >"$CASE/probe.log" 2>&1 &
probe_pid=$!
sleep 0.5
kill -INT "$probe_pid" 2>/dev/null || true
wait "$probe_pid"
PROBE_STATUS=$?
set -e
[[ $PROBE_STATUS -eq 130 ]] || fail "an interrupted probe must exit 130 (got $PROBE_STATUS: $(cat "$CASE/probe.log"))"
leftover=$(find "$CASE/home/scratch" -mindepth 1 | sed -n '1p')
[[ -z "$leftover" ]] || fail "an interrupted probe leaked scratch: $leftover"

[[ -z "$(find "$CASE/exec-temp" -mindepth 1 -print -quit)" ]] || fail 'interrupt leaked private exec TMPDIR'

new_case probe-temp-escape
stage_prefix
probe_argv 10 ''
probe_env valid
PROBE_ENV+=("AGENT_VM_FIXTURE_EXEC_TMPDIR=$TEST_ROOT/tmp")
set +e
PROBE_OUTPUT="$(env -i "${PROBE_ENV[@]}" python3 "$SOURCE_DIR/probe-dsh.py" "${PROBE_ARGS[@]}" 2>&1)"
PROBE_STATUS=$?
set -e
[[ $PROBE_STATUS -eq 1 ]] || fail 'exec temp override must remain in fixture root'

# --- the test seam cannot become the production default ---------------------

# shellcheck disable=SC2016  # single quotes keep the literal grep pattern
grep -Fq 'AGENT_VM_DSH_PREFIX:-/opt/agent-vm/dsh}' "$GATE" \
    || fail 'production default prefix is missing'
grep -Fq 'AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}' "$GATE" \
    || fail 'production default status dir is missing'
grep -Fq 'AGENT_VM_DSH_COMMAND:-/usr/local/bin/dsh}' "$GATE" \
    || fail 'production default command is missing'
# The gate executes run-report.sh and recipe-contract, so it must take their
# directory from the authorized verb, never from a fixed build-mount path.
# shellcheck disable=SC2016  # single quotes keep the literal grep pattern
grep -Fq 'contract-dir "$PREFIX"' "$GATE" \
    || fail 'the gate does not obtain the authorized contract directory'
! grep -Fq '/tmp/recipe-contract' "$GATE" \
    || fail 'the gate names the build-mount contract path'

echo 'dsh-verify black-box tests passed'
