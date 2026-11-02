#!/usr/bin/env bash
# Black-box tests for the claude layer's install hook and build gate
# (images/tools/claude/{install-claude.sh,verify-claude.sh}) and for the
# vendored installer patch reproduction.
#
# No Docker, no network: the vendored installer is faked on a temp prefix and
# the real run-install/report/download/check-tool-access helpers are used where
# they are part of what is under test. The committed closure is a per-run fixture
# key and per-case closure (`script/test/fixtures/claude/claude_fixture.py`), so
# the hook's pin refusal and the gate's payload/layout/state checks run against
# real signatures. This exercises the exact-version validation, the pin refusal,
# the classified transport softening (and every way it must NOT soften), and the
# report/T5 gate.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
VENDOR_SRC="$REPO_ROOT/images/tools/claude/vendor"
INSTALL="$REPO_ROOT/images/tools/claude/install-claude.sh"
VERIFY="$REPO_ROOT/images/tools/claude/verify-claude.sh"
FIXTURE="$REPO_ROOT/script/test/fixtures/claude/claude_fixture.py"
# The converted verifiers execute `recipe-contract` from the contract directory,
# so build the native composed directory once before any restricted env -i or
# fixture PATH replaces the tooling.
CONTRACT="$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)/out"

TEST_ROOT="$(mktemp -d /tmp/claude-installer.XXXXXX)"
chmod 0755 "$TEST_ROOT"
trap 'rm -rf "$TEST_ROOT"' EXIT

PYTHON_DIR="$(dirname "$(command -v python3)")"
TIMEOUT_DIR="$(dirname "$(command -v timeout)")"

# gpg and gpgconf only: the pin verifier allocates its own private GNUPGHOME, and
# its cleanup must find gpgconf or it prints a warning into an exact-output case.
GPGBIN="$TEST_ROOT/gpgbin"
mkdir -p "$GPGBIN"
ln -s "$(command -v gpg)" "$GPGBIN/gpg"
ln -s "$(command -v gpgconf)" "$GPGBIN/gpgconf"

# One throwaway OpenPGP key for the whole run; every closure below is signed by it.
SIGNER="$TEST_ROOT/signer"
python3 "$FIXTURE" keygen "$SIGNER" >/dev/null

CASE=""

# make_closure <version> <dir> <amd64 file> <arm64 file>
make_closure() {
    rm -rf "$2"
    python3 "$FIXTURE" closure "$2" --signer "$SIGNER" --version "$1" \
        --amd64 "$3" --arm64 "$4" >/dev/null
}

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_contains() { [[ "$1" == *"$2"* ]] || fail "expected '$2' in: $1"; }

# The native layout check compares the payload's owner with the invoking uid/gid.
# macOS gives every file created under /tmp the *directory's* group (wheel), not
# the caller's, so a case tree has to carry the invoking group before anything is
# written into it -- otherwise the check refuses a payload the suite created.
own_case() {
    chgrp -R "$(id -g)" "$CASE"
}

# --- 1. the patch reproduces the committed runnable installer ----------------

patch_case="$TEST_ROOT/patch"
mkdir -p "$patch_case"
chmod 0755 "$patch_case"
cp "$VENDOR_SRC/install.upstream.sh" "$patch_case/install.sh"
(
    cd "$patch_case"
    git apply -p1 <"$VENDOR_SRC/install.patch"
)
cmp "$patch_case/install.sh" "$VENDOR_SRC/install.sh" ||
    fail "install.patch does not reproduce vendor/install.sh byte-for-byte"

# --- 2. install-claude.sh black-box ------------------------------------------

new_case() {
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/vendor" "$CASE/prefix" "$CASE/status"
    chmod 0755 "$CASE" "$CASE/prefix" "$CASE/status"
    own_case

    # Fake vendored installer. It honours FAKE_EXIT/FAKE_RECEIPT, records that it
    # ran and records the pin directory it was handed, so exact-version and pin
    # rejections can prove the installer was never invoked and a valid run can
    # prove the seam reached it. It reads the receipt path the hook exports, like
    # the real patched installer's download.sh does.
    cat >"$CASE/vendor/install.sh" <<'SH'
#!/bin/sh
if [ -n "${FAKE_SENTINEL:-}" ]; then printf 'ran\n' >>"$FAKE_SENTINEL"; fi
if [ -n "${FAKE_PIN_LOG:-}" ]; then printf '%s\n' "${CLAUDE_PIN_DIR:-}" >>"$FAKE_PIN_LOG"; fi
if [ -n "${FAKE_RECEIPT:-}" ] && [ -n "${AGENT_VM_TRANSPORT_RECEIPT:-}" ]; then
    printf '%s\n' "$FAKE_RECEIPT" >"$AGENT_VM_TRANSPORT_RECEIPT"
fi
exit "${FAKE_EXIT:-0}"
SH
    chmod 0755 "$CASE/vendor/install.sh"
}

# make_source <version> [<dir>]: a fixture closure for the hook to verify.
make_source() {
    local version="$1" directory="${2:-$CASE/source}"
    printf 'amd64 payload for %s\n' "$version" >"$CASE/source-amd64"
    printf 'arm64 payload for %s\n' "$version" >"$CASE/source-arm64"
    make_closure "$version" "$directory" "$CASE/source-amd64" "$CASE/source-arm64"
}

# run_install <version>; env: FAKE_EXIT, FAKE_RECEIPT, SOFT, NO_RECEIPT,
# SOURCE_DIR (default: the current case's fixture closure)
run_install() {
    local version="$1"
    local receipt_val="$CASE/receipt"
    local source_dir="${SOURCE_DIR:-$CASE/source}"
    [[ "${NO_RECEIPT:-}" == 1 ]] && receipt_val=""
    set +e
    RUN_OUTPUT="$(
        env -i \
            "PATH=$GPGBIN:$PYTHON_DIR:/usr/bin:/bin" \
            "AGENT_VM_VENDOR_DIR=$CASE/vendor" \
            "AGENT_VM_CLAUDE_PREFIX=$CASE/prefix" \
            "AGENT_VM_FIXTURE_ROOT=$CASE" \
            "AGENT_VM_CLAUDE_SOURCE_DIR=$source_dir" \
            "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status" \
            "AGENT_VM_VERSION_CLAUDE=$version" \
            "FAKE_SENTINEL=$CASE/ran" \
            "FAKE_PIN_LOG=$CASE/pin-dir.log" \
            "FAKE_EXIT=${FAKE_EXIT:-0}" \
            "FAKE_RECEIPT=${FAKE_RECEIPT:-}" \
            "AGENT_VM_TRANSPORT_RECEIPT=$receipt_val" \
            ${SOFT:+"AGENT_INSTALL_SOFT_FAIL=$SOFT"} \
            sh "$INSTALL" 2>&1
    )"
    RUN_STATUS=$?
    set -e
}

new_case success
SOFT=
make_source 2.1.286
run_install 2.1.286
[[ $RUN_STATUS -eq 0 ]] || fail "a clean install must pass: $RUN_OUTPUT"
[[ -f "$CASE/ran" ]] || fail "the vendored installer must run on a valid version"
[[ "$(cat "$CASE/pin-dir.log")" == "$CASE/source" ]] ||
    fail "the vendor must receive the committed pin directory, got $(cat "$CASE/pin-dir.log")"
[[ ! -e "$CASE/status/claude" ]] || fail "the hook must not write installed itself"

# floating / non-exact versions are rejected BEFORE the installer runs
for bad in latest stable '^1.0.0' '1.0' '1.0.0 2.0.0' 'https://x/2.1.286' '2.1.286 ' \
    '01.2.3' '1.02.3' '1.2.03' '1.2.3-01' '1.2.3-alpha.01'; do
    new_case "reject-$(printf '%s' "$bad" | tr -c 'A-Za-z0-9' '_')"
    SOFT=
    # The closure covers the pinned version, never the rejected spelling.
    make_source 2.1.286
    run_install "$bad"
    [[ $RUN_STATUS -ne 0 ]] || fail "version '$bad' must be rejected"
    [[ ! -f "$CASE/ran" ]] || fail "version '$bad' must be rejected before the installer runs"
done

# Canonical prerelease/build metadata the pinned installer supports is accepted,
# but only for a version the committed pin actually covers.
for good in 1.2.3-rc.1 1.2.3-alpha; do
    new_case "accept-$(printf '%s' "$good" | tr -c 'A-Za-z0-9' '_')"
    SOFT=
    make_source "$good"
    run_install "$good"
    [[ $RUN_STATUS -eq 0 ]] || fail "canonical version '$good' must be accepted: $RUN_OUTPUT"
    [[ -f "$CASE/ran" ]] || fail "canonical version '$good' must reach the vendor"
done

# ...and an exact but unpinned version is refused before the vendor runs.
for unpinned in 1.2.3-rc.1 2.1.285; do
    new_case "unpinned-$(printf '%s' "$unpinned" | tr -c 'A-Za-z0-9' '_')"
    SOFT=
    make_source 2.1.286
    run_install "$unpinned"
    [[ $RUN_STATUS -ne 0 ]] || fail "unpinned version '$unpinned' must be refused"
    [[ ! -f "$CASE/ran" ]] || fail "unpinned version '$unpinned' must be refused before the vendor"
    assert_contains "$RUN_OUTPUT" "does not cover"
done

new_case empty
SOFT=
make_source 2.1.286
run_install ""
[[ $RUN_STATUS -ne 0 ]] || fail "an empty version must be rejected"

# A missing pin directory is refused before the vendor runs.
new_case no-pin
SOFT=
SOURCE_DIR="$CASE/absent"
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "a missing pin directory must be refused"
[[ ! -f "$CASE/ran" ]] || fail "a missing pin directory must be refused before the vendor"
assert_contains "$RUN_OUTPUT" "no committed Claude pin"
unset SOURCE_DIR

# Tampered committed evidence is refused before the vendor runs.
new_case tampered-evidence
SOFT=
make_source 2.1.286
printf 'tampered\n' >>"$CASE/source/evidence/manifest.json"
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "tampered evidence must be refused"
[[ ! -f "$CASE/ran" ]] || fail "tampered evidence must be refused before the vendor"
assert_contains "$RUN_OUTPUT" "does not match the pin digest"

# a classified transport 75 with a fresh receipt softens when asked
new_case transport-soft
make_source 2.1.286
FAKE_EXIT=75
FAKE_RECEIPT='transport download 6'
SOFT=1
mkdir -p "$CASE/prefix/.claude/downloads" "$CASE/prefix/.local/bin"
touch "$CASE/prefix/.claude/downloads/partial" "$CASE/prefix/.local/bin/claude"
run_install 2.1.286
[[ $RUN_STATUS -eq 0 ]] || fail "a classified transport failure must soften: $RUN_OUTPUT"
[[ "$(cat "$CASE/status/claude")" == "absent-transport 6" ]] ||
    fail "softened install must write an absence record"
[[ ! -e "$CASE/prefix/.claude/downloads" ]] || fail "the download cache must be cleaned"
[[ ! -e "$CASE/prefix/.local/bin/claude" ]] || fail "the half-published command must be cleaned"
unset FAKE_EXIT FAKE_RECEIPT SOFT

# B6: an env-overridden prefix that is a symlink escaping the fixture root must
# not authorise deletion of another real tree. The marker outside must survive.
new_case transport-soft-symlink-escape
make_source 2.1.286
FAKE_EXIT=75
FAKE_RECEIPT='transport download 6'
SOFT=1
outside="$TEST_ROOT/other-tree-$RANDOM"
mkdir -p "$outside/.claude/downloads" "$outside/.local/bin"
printf 'do not delete\n' >"$outside/.claude/downloads/marker"
printf 'cmd\n' >"$outside/.local/bin/claude"
rm -rf "$CASE/prefix"
ln -s "$outside" "$CASE/prefix"
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "a symlinked escaped prefix must be refused"
[[ -f "$outside/.claude/downloads/marker" ]] || fail "an unauthorised tree was deleted"
[[ -f "$outside/.local/bin/claude" ]] || fail "an unauthorised command was deleted"
rm -f "$CASE/prefix"
unset FAKE_EXIT FAKE_RECEIPT SOFT

# ...but not without AGENT_INSTALL_SOFT_FAIL
new_case transport-no-soft
make_source 2.1.286
FAKE_EXIT=75
FAKE_RECEIPT='transport download 6'
SOFT=
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "a transport failure without soft-fail must be hard"
[[ ! -e "$CASE/status/claude" ]] || fail "a hard failure must not leave an absence record"
unset FAKE_EXIT FAKE_RECEIPT SOFT

# ...and only for a receipt with the right operation. This is deliberately not a
# valid download line: `transport npm 6` and `transport download 6` are both
# *right-shaped* receipts for a different producer, so the control names an
# operation that no helper produces at all.
new_case transport-wrong-op
make_source 2.1.286
FAKE_EXIT=75
FAKE_RECEIPT='transport wrong-operation 6'
SOFT=1
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "a receipt for another operation must not soften a native install"
unset FAKE_EXIT FAKE_RECEIPT SOFT

# a bare 75 with no receipt is never classified
new_case bare-75
make_source 2.1.286
FAKE_EXIT=75
FAKE_RECEIPT=''
SOFT=1
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "an arbitrary 75 must be hard"
unset FAKE_EXIT FAKE_RECEIPT SOFT

# an unknown installer failure is hard even with soft input
new_case unknown-failure
make_source 2.1.286
FAKE_EXIT=3
FAKE_RECEIPT=''
SOFT=1
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "an unknown installer exit must be hard"
unset FAKE_EXIT FAKE_RECEIPT SOFT

# no receipt environment at all: a 75 cannot be classified
new_case no-receipt-env
make_source 2.1.286
FAKE_EXIT=75
FAKE_RECEIPT='transport download 6'
SOFT=1
NO_RECEIPT=1
run_install 2.1.286
[[ $RUN_STATUS -ne 0 ]] || fail "a 75 without a receipt path must be hard"
unset FAKE_EXIT FAKE_RECEIPT SOFT NO_RECEIPT

# --- 3. verify-claude.sh black-box -------------------------------------------

new_gate_case() {
    CASE="$TEST_ROOT/gate-$1"
    mkdir -p "$CASE/prefix/.local/bin" "$CASE/prefix/.local/share/claude/versions" \
        "$CASE/status"
    chmod 0755 "$CASE" "$CASE/prefix" "$CASE/prefix/.local/bin" \
        "$CASE/prefix/.local/share" "$CASE/prefix/.local/share/claude" \
        "$CASE/prefix/.local/share/claude/versions" "$CASE/status"
    own_case
    GATE_BIN="$CASE/prefix/.local/share/claude/versions/${WANT-}"
    GATE_LAUNCHER="$CASE/prefix/.local/bin/claude"
    # A placeholder closure so the absence cases reach their own decision: the gate
    # requires a readable pin directory before it looks at the command.
    printf '#!/bin/sh\nexit 1\n' >"$CASE/dummy"
    make_closure "${WANT-}" "$CASE/source" "$CASE/dummy" "$CASE/dummy"
}

# Seal the layout around whatever payload bytes are now at GATE_BIN: the native
# shape the offline setup step leaves (absolute launcher, mode 0755, state file),
# and a closure whose two platform digests are those bytes.
seal_gate() {
    make_closure "${WANT-}" "$CASE/source" "$GATE_BIN" "$GATE_BIN"
    ln -sfn "$GATE_BIN" "$GATE_LAUNCHER"
    printf '{"installMethod": "native", "autoUpdates": false}\n' \
        >"$CASE/prefix/.claude.json"
}

# The marker is inserted after the shebang so a case that must refuse before
# executing can prove nothing ran, whatever the script itself does.
make_claude() {
    cat >"$GATE_BIN.body"
    {
        head -n 1 "$GATE_BIN.body"
        printf 'printf "ran\\n" >>%q\n' "$CASE/executed"
        tail -n +2 "$GATE_BIN.body"
    } >"$GATE_BIN"
    rm -f "$GATE_BIN.body"
    chmod 0755 "$GATE_BIN"
    seal_gate
}

# A payload written without the execution marker (for mode/T5 cases).
write_payload() {
    cat >"$GATE_BIN"
    chmod 0755 "$GATE_BIN"
    seal_gate
}

run_gate() {
    set +e
    RUN_OUTPUT="$(
        env -i \
            "PATH=$GPGBIN:$CASE/prefix/.local/bin:$TIMEOUT_DIR:$PYTHON_DIR:/usr/bin:/bin" \
            "AGENT_VM_CONTRACT_DIR=$CONTRACT" \
            "AGENT_VM_CLAUDE_PREFIX=$CASE/prefix" \
            "AGENT_VM_CLAUDE_SOURCE_DIR=$CASE/source" \
            "AGENT_VM_CLAUDE_EXPECTED_OWNER=$(id -u):$(id -g)" \
            "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status" \
            "AGENT_VM_VERSION_CLAUDE=${WANT-}" \
            sh "$VERIFY" 2>&1
    )"
    RUN_STATUS=$?
    set -e
}

WANT=2.1.286
new_gate_case ok
make_claude <<'SH'
#!/bin/sh
[ "$1" = --version ] || exit 2
printf '2.1.286 (Claude Code)\n'
SH
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "the pinned version must pass: $RUN_OUTPUT"
[[ "$(cat "$CASE/status/claude")" == "installed" ]] ||
    fail "a passing gate must record installed"

WANT=2.1.286
new_gate_case mismatch
make_claude <<'SH'
#!/bin/sh
printf '2.1.285 (Claude Code)\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a wrong installed version must fail"
[[ ! -e "$CASE/status/claude" ]] || fail "a failed gate must not record installed"

WANT=2.1.286
new_gate_case prefix
make_claude <<'SH'
#!/bin/sh
printf '2.1.2860 (Claude Code)\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "2.1.2860 must not satisfy a 2.1.286 pin"

WANT=2.1.286
new_gate_case extra
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
printf 'warning: 2.1.286\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a warning on a later line must not be accepted"

# B1: a contradictory version banner on the captured stderr must not certify the
# slot; the gate must reject it rather than discard it on exit 0.
WANT=2.1.286
new_gate_case stderr-contradictory
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
printf '9.9.9 (Claude Code)\n' >&2
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a contradictory banner on stderr must fail"

# C2: a NUL byte inside the report is exact-byte corruption, not a report the
# shell may canonicalize by erasing the NUL during command substitution.
WANT=2.1.286
new_gate_case nul-in-report
make_claude <<'SH'
#!/bin/sh
printf '2.1.286\000 (Claude Code)\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a NUL inside the report must fail"

# SP5: an unterminated trailing report line (no final newline) must still fail;
# a bare `while read` drops it.
WANT=2.1.286
new_gate_case unterminated-trailing
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\nUNRECOGNIZED TRAILING REPORT'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "an unterminated trailing report line must be rejected"

WANT=2.1.286
new_gate_case leading-warning
make_claude <<'SH'
#!/bin/sh
printf 'warning: expected 2.1.286\n'
printf '2.1.286 (Claude Code)\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a leading warning line must not be accepted"

WANT=2.1.286
new_gate_case nonzero
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
exit 1
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "printing the expected version then exiting nonzero must fail"
assert_contains "$RUN_OUTPUT" "exited 1"

# The committed payload, the native layout and the state file are checked BEFORE
# the binary is executed. Each case below would pass on its report alone.
WANT=2.1.286
new_gate_case payload-bytes-changed
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
printf 'tampered after sealing\n' >>"$GATE_BIN"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a payload changed after sealing must fail"
[[ ! -e "$CASE/executed" ]] || fail "a changed payload must not be executed"
assert_contains "$RUN_OUTPUT" "does not match the pinned"

WANT=2.1.286
new_gate_case launcher-relative
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
ln -sfn ".local/share/claude/versions/2.1.286" "$GATE_LAUNCHER"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a relative launcher must fail"
[[ ! -e "$CASE/executed" ]] || fail "a relative launcher must fail before execution"

WANT=2.1.286
new_gate_case extra-version
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
: >"$CASE/prefix/.local/share/claude/versions/9.9.9"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "an extra versions entry must fail"

WANT=2.1.286
new_gate_case payload-mode
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
chmod 0775 "$GATE_BIN"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a 0775 payload must fail"

WANT=2.1.286
new_gate_case auto-updates
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
printf '{"installMethod": "native", "autoUpdates": true}\n' >"$CASE/prefix/.claude.json"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "autoUpdates true must fail"

WANT=2.1.286
new_gate_case install-method
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
printf '{"autoUpdates": false}\n' >"$CASE/prefix/.claude.json"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a missing installMethod must fail"

# absence with the exact record is accepted (raw developer build only)
WANT=2.1.286
new_gate_case absent
printf 'absent-transport 6\n' >"$CASE/status/claude"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "a recorded absence must be accepted: $RUN_OUTPUT"

# SP2: a dangling symlink is PRESENT (lstat succeeds), so an absence record must
# not waive it.
WANT=2.1.286
new_gate_case dangling
printf 'absent-transport 6\n' >"$CASE/status/claude"
ln -s nowhere "$GATE_LAUNCHER"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a dangling command symlink must fail despite the absence record"

# SP2/B2: a dangling ancestor symlink is a present, broken path, not an absence.
WANT=2.1.286
new_gate_case broken-ancestor-dangling
printf 'absent-transport 6\n' >"$CASE/status/claude"
rm -rf "$CASE/prefix/.local"
ln -s nowhere "$CASE/prefix/.local"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a dangling ancestor symlink must not authorise absence"

# SP2/B2: a non-directory ancestor (ENOTDIR) is the same kind of broken path.
WANT=2.1.286
new_gate_case broken-ancestor-notdir
printf 'absent-transport 6\n' >"$CASE/status/claude"
rm -rf "$CASE/prefix/.local"
printf 'not a directory\n' >"$CASE/prefix/.local"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a non-directory ancestor must not authorise absence"

# SP2: a present-but-unusable file is a contract violation, not an absence.
WANT=2.1.286
new_gate_case present-unusable
printf 'absent-transport 6\n' >"$CASE/status/claude"
printf '#!/bin/sh\nexit 0\n' >"$GATE_LAUNCHER"
chmod 0644 "$GATE_LAUNCHER"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a present 0644 command must fail despite the absence record"
[[ -e "$GATE_LAUNCHER" ]] || fail "the present command must not be deleted"

# SP2: an absence line inside a malformed multi-line record is not valid.
WANT=2.1.286
new_gate_case malformed-record
printf 'absent-transport 6\njunk\n' >"$CASE/status/claude"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a multi-line absence record must not be accepted"

# a missing command with no record is hard
WANT=2.1.286
new_gate_case missing
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a missing command with no record must fail"

# a pending/inherited record is not an absence
WANT=2.1.286
new_gate_case pending
printf 'pending\n' >"$CASE/status/claude"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a pending record must not be accepted as absence"

# T5: a private directory on the command's path fails the audit
WANT=2.1.286
new_gate_case private-path
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
chmod 0700 "$CASE/prefix/.local/bin"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "a 0700 directory on the command's path must fail T5"

# T5: an unreadable script target fails the audit
WANT=2.1.286
new_gate_case unreadable-target
make_claude <<'SH'
#!/bin/sh
printf '2.1.286 (Claude Code)\n'
SH
chmod 0111 "$GATE_BIN"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail "an unreadable script target must fail T5"

echo "claude-installer black-box tests passed"
