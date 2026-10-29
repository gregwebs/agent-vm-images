#!/usr/bin/env bash
# Black-box contract tests for the shared recipe helpers
# (images/recipe-contract/{run-install,download,run-report}.sh). The retired
# `run-npm.sh` section is now a deletion/negative control.
#
# Hermetic: no Docker, no network, no real npm/curl. Every helper is driven as a
# subprocess with exact argv/env and its status, receipt, result record and
# operation log are asserted -- the helpers are never sourced and their internal
# functions are never called. This is the seam that decides which install
# failures may be softened under AGENT_INSTALL_SOFT_FAIL, so the "must be hard"
# cases (checksum/EINTEGRITY/unknown/arbitrary 75) are the point of the suite.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
CONTRACT="$REPO_ROOT/images/recipe-contract"

TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/installer-contracts.XXXXXX")"
trap 'rm -rf "$TEST_ROOT"' EXIT
CASE=""

REAL_JQ="$(command -v jq || true)"
[[ -n "$REAL_JQ" ]] || { echo "FAIL: jq is required" >&2; exit 1; }
JQ_DIR="$(dirname "$REAL_JQ")"
TIMEOUT_BIN="$(command -v timeout || true)"
[[ -n "$TIMEOUT_BIN" ]] || { echo "FAIL: timeout is required" >&2; exit 1; }
TIMEOUT_DIR="$(dirname "$TIMEOUT_BIN")"
REAL_MKTEMP="$(command -v mktemp || true)"
[[ -n "$REAL_MKTEMP" ]] || { echo "FAIL: mktemp is required" >&2; exit 1; }

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_eq() {
    [[ "$1" == "$2" ]] || fail "$3 (expected '$2', got '$1')"
}

assert_contains() {
    [[ "$1" == *"$2"* ]] || fail "expected output to contain '$2' (got: $1)"
}

# new_case <name>; creates $CASE/bin and a receipt path.
new_case() {
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/bin"
    RECEIPT="$CASE/receipt"
    : >"$RECEIPT"
    EXTRA_ENV=()
}

# make_exec <name> <<SH ... SH
make_exec() {
    local name="$1"
    cat >"$CASE/bin/$name"
    chmod +x "$CASE/bin/$name"
}

run_helper() {
    # run_helper <script> [args...]; env: RUN_ENV (array of KEY=VAL) optional.
    local script="$1"
    shift
    set +e
    RUN_OUTPUT="$(
        env -i \
            "PATH=$CASE/bin:$TIMEOUT_DIR:$JQ_DIR:/usr/bin:/bin" \
            "AGENT_VM_TRANSPORT_RECEIPT=$RECEIPT" \
            "${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"}" \
            sh "$CONTRACT/$script" "$@" 2>&1
    )"
    RUN_STATUS=$?
    set -e
}

# ============================ run-install.sh ================================

new_case install-success
make_exec install-ok <<'SH'
#!/bin/sh
printf 'installed ok\n'
exit 0
SH
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-ok"
assert_eq "$RUN_STATUS" 0 "a successful installer must return 0"
assert_eq "$(cat "$CASE/status")" pending "the status record starts pending"

new_case install-status-cleared
make_exec install-ok <<'SH'
#!/bin/sh
exit 0
SH
printf 'installed\n' >"$CASE/status"
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-ok"
assert_eq "$(cat "$CASE/status")" pending "an inherited installed status must be cleared"

new_case install-argv
make_exec install-argv <<'SH'
#!/bin/sh
printf '%s\n' "$@" >"$RECORD"
SH
RECORD="$CASE/argv.log"
EXTRA_ENV=("RECORD=$CASE/argv.log")
# shellcheck disable=SC2016  # the literal '$(date)' is deliberately not expanded
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-argv" 'a b' '$(date)' 'x;y'
assert_eq "$RUN_STATUS" 0 "argv forwarding must succeed"
assert_eq "$(sed -n '1p' "$RECORD")" 'a b' "first forwarded arg is literal"
# shellcheck disable=SC2016
assert_eq "$(sed -n '2p' "$RECORD")" '$(date)' "shell syntax is forwarded literally, not evaluated"
assert_eq "$(sed -n '3p' "$RECORD")" 'x;y' "metacharacters are forwarded literally"

new_case install-unknown-nonzero
make_exec install-bad <<'SH'
#!/bin/sh
exit 7
SH
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-bad"
assert_eq "$RUN_STATUS" 1 "an unknown nonzero installer exit is hard"

new_case install-arbitrary-75
make_exec install-75 <<'SH'
#!/bin/sh
exit 75
SH
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-75"
assert_eq "$RUN_STATUS" 1 "an arbitrary 75 with no receipt is hard"

new_case install-classified-75
make_exec install-classified <<'SH'
#!/bin/sh
printf 'transport download 6\n' >"$AGENT_VM_TRANSPORT_RECEIPT"
exit 75
SH
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-classified"
assert_eq "$RUN_STATUS" 1 "a classified 75 the owning hook did not soften is hard"
assert_contains "$RUN_OUTPUT" "classified transport failure"

# ============================== download.sh =================================

new_case download-success
make_exec curl <<'SH'
#!/bin/sh
echo "curl $*" >>"$FAKE_LOG"
out=
while [ $# -gt 0 ]; do
    case "$1" in -o) out=$2; shift 2 ;; *) shift ;; esac
done
printf 'payload\n' >"$out"
exit 0
SH
FAKE_LOG="$CASE/curl.log"
EXTRA_ENV=("FAKE_LOG=$CASE/curl.log")
run_helper download.sh https://example.test/asset "$CASE/out"
assert_eq "$RUN_STATUS" 0 "a successful download returns 0"
assert_eq "$(cat "$CASE/out")" payload "the payload is written"

new_case download-transport
make_exec curl <<'SH'
#!/bin/sh
echo "curl $*" >>"$FAKE_LOG"
exit 6
SH
FAKE_LOG="$CASE/curl.log"
EXTRA_ENV=("FAKE_LOG=$CASE/curl.log")
run_helper download.sh https://example.test/asset "$CASE/out"
assert_eq "$RUN_STATUS" 75 "an allowlisted curl transport failure is classified"
assert_eq "$(cat "$RECEIPT")" "transport download 6" "the receipt names the transport"

new_case download-http-error
make_exec curl <<'SH'
#!/bin/sh
exit 22
SH
run_helper download.sh https://example.test/asset "$CASE/out"
assert_eq "$RUN_STATUS" 1 "an HTTP error (curl 22) is hard"
assert_eq "$(cat "$RECEIPT")" "" "a hard failure writes no receipt"

new_case download-unsafe-url
make_exec curl <<'SH'
#!/bin/sh
echo "curl $*" >>"$FAKE_LOG"
exit 0
SH
FAKE_LOG="$CASE/curl.log"
EXTRA_ENV=("FAKE_LOG=$CASE/curl.log")
run_helper download.sh "https://example.test/a b" "$CASE/out"
assert_eq "$RUN_STATUS" 1 "a URL with whitespace is rejected before curl"
[[ ! -s "$FAKE_LOG" ]] || fail "curl must not be invoked for an unsafe URL"

new_case download-embedded-newline
make_exec curl <<'SH'
#!/bin/sh
echo "curl $*" >>"$FAKE_LOG"
exit 0
SH
FAKE_LOG="$CASE/curl.log"
EXTRA_ENV=("FAKE_LOG=$CASE/curl.log")
run_helper download.sh "$(printf 'https://example.test/a\nb')" "$CASE/out"
assert_eq "$RUN_STATUS" 1 "a URL with an embedded newline is rejected before curl"
[[ ! -s "$FAKE_LOG" ]] || fail "curl must not be invoked for a URL with an embedded newline"

new_case download-no-receipt
: # AGENT_VM_TRANSPORT_RECEIPT is always set by run_helper; call env directly.
set +e
RUN_OUTPUT="$(env -i "PATH=$CASE/bin:/usr/bin:/bin" sh "$CONTRACT/download.sh" https://example.test/x "$CASE/out" 2>&1)"
RUN_STATUS=$?
set -e
assert_eq "$RUN_STATUS" 1 "without a receipt path the download cannot be classified"

# ========================= retired npm adapter ==============================
# `run-npm.sh` is deleted with the Pi/bridge pnpm migration; nothing produces an
# npm transport receipt any more. The surviving controls that the old case block
# covered live where their helper still exists: the timeout bound and the
# --kill-after escalation in script/test/run-pnpm.sh (plus report-term-ignoring
# below), signal exits in the run-pnpm status matrix, and the JSON report capture
# in run-pnpm's result-file assertions.
[[ ! -e "$CONTRACT/run-npm.sh" ]] || fail "run-npm.sh must stay deleted"

# A `transport npm CODE` receipt is retired vocabulary. It must not classify a
# 75, so an installer that still wrote one cannot soften a failure.
new_case install-retired-npm-receipt
make_exec install-npm-receipt <<'SH'
#!/bin/sh
printf 'transport npm EAI_AGAIN\n' >"$AGENT_VM_TRANSPORT_RECEIPT"
exit 75
SH
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-npm-receipt"
assert_eq "$RUN_STATUS" 1 "a retired npm receipt must not classify a 75"
[[ "$RUN_OUTPUT" == *"no classified receipt"* ]] ||
    fail "a retired npm receipt must be reported as unclassified (got: $RUN_OUTPUT)"

# The same receipt shape with the download operation is still classified, so the
# negative above is about the retired operation, not about receipts in general.
new_case install-download-receipt
make_exec install-download-receipt <<'SH'
#!/bin/sh
printf 'transport download 56\n' >"$AGENT_VM_TRANSPORT_RECEIPT"
exit 75
SH
run_helper run-install.sh demo "$CASE/status" sh "$CASE/bin/install-download-receipt"
assert_eq "$RUN_STATUS" 1 "a classified 75 the owning hook did not soften is hard"
assert_contains "$RUN_OUTPUT" "classified transport failure"

# ============================== run-report.sh ===============================

new_case report-success
make_exec mytool <<'SH'
#!/bin/sh
printf 'the version\n'
printf 'a warning\n' >&2
exit 0
SH
run_helper run-report.sh 30 "$CASE/out" "$CASE/err" "$CASE/bin/mytool" --version
assert_eq "$RUN_STATUS" 0 "a successful bounded run returns 0"
assert_eq "$(cat "$CASE/out")" "the version" "stdout is captured"
assert_eq "$(cat "$CASE/err")" "a warning" "stderr is captured"

new_case report-nonzero
make_exec mytool <<'SH'
#!/bin/sh
printf 'the version\n'
exit 3
SH
run_helper run-report.sh 30 "$CASE/out" "$CASE/err" "$CASE/bin/mytool" --version
assert_eq "$RUN_STATUS" 3 "a nonzero run preserves its status"
assert_eq "$(cat "$CASE/out")" "the version" "diagnostics are preserved"

new_case report-arbitrary-75
make_exec mytool <<'SH'
#!/bin/sh
printf 'the version\n'
exit 75
SH
run_helper run-report.sh 30 "$CASE/out" "$CASE/err" "$CASE/bin/mytool" --version
assert_eq "$RUN_STATUS" 1 "an arbitrary 75 is remapped to a hard 1"

new_case report-missing
run_helper run-report.sh 30 "$CASE/out" "$CASE/err" "$CASE/does-not-exist"
assert_eq "$RUN_STATUS" 1 "a missing executable is hard"

new_case report-dangling
ln -s "$CASE/nowhere" "$CASE/bin/dangle"
run_helper run-report.sh 30 "$CASE/out" "$CASE/err" "$CASE/bin/dangle"
assert_eq "$RUN_STATUS" 1 "a dangling symlink is hard, never absent"

# A4/B3: a TERM-ignoring tool must be bounded by the SIGKILL escalation, not
# hang the build. The 1s bound plus a short --kill-after completes the case.
new_case report-term-ignoring
make_exec mytool <<'SH'
#!/bin/sh
trap '' TERM
sleep 30
exit 0
SH
_start="$(date +%s)"
run_helper run-report.sh 1 "$CASE/out" "$CASE/err" "$CASE/bin/mytool"
_elapsed=$(( $(date +%s) - _start ))
[[ $RUN_STATUS -ne 0 ]] || fail "a TERM-ignoring report child must not pass"
[[ $_elapsed -lt 15 ]] || fail "run-report must SIGKILL a TERM-ignoring child (took ${_elapsed}s)"

# ============================== cleanup safety ==============================
# A7: the scratch cleanup trap must not re-parse a TMPDIR that contains shell
# syntax. The generated copies used `trap "rm -rf '$scratch'"`, so a single
# quote in TMPDIR escaped the quoting and injected a command into the exit trap.
# BSD mktemp ignores TMPDIR, so a shim reproduces GNU mktemp's honoring of it.
new_case trap-quote-tmpdir
make_exec mytool <<'SH'
#!/bin/sh
printf 'ok\n'
SH
make_exec mktemp <<'SH'
#!/bin/sh
case "${1:-}" in
    -d)
        shift
        exec "$REAL_MKTEMP" -d "${TMPDIR:-/tmp}/tmp.XXXXXXXXXX"
        ;;
    *) exec "$REAL_MKTEMP" "$@" ;;
esac
SH
evil="$CASE/tmp'; touch $CASE/MARKER; #"
mkdir -p "$evil"
run_with_evil_tmpdir() {
    local script="$1"
    shift
    set +e
    RUN_OUTPUT="$(env -i \
        "PATH=$CASE/bin:$TIMEOUT_DIR:$JQ_DIR:/usr/bin:/bin" \
        "TMPDIR=$evil" \
        "REAL_MKTEMP=$REAL_MKTEMP" \
        "AGENT_VM_TRANSPORT_RECEIPT=$RECEIPT" \
        sh "$CONTRACT/$script" "$@" 2>&1)"
    RUN_STATUS=$?
    set -e
}
run_with_evil_tmpdir run-report.sh 5 "$CASE/out" "$CASE/err" "$CASE/bin/mytool"
assert_eq "$RUN_STATUS" 0 "run-report must succeed with a quote in TMPDIR"
# run-pnpm.sh owns the other scratch/trap pair; its suite carries the same
# control, so this file only keeps the surviving helper it can drive here.
[[ ! -e "$CASE/MARKER" ]] || fail "a quote in TMPDIR injected into the cleanup trap"

echo "shipped-installer-contracts black-box tests passed"
