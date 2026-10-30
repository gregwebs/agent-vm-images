#!/usr/bin/env bash
# Black-box tests for the agy layer's install hook and build gate
# (images/tools/agy/{install-agy.sh,verify-agy.sh}).
#
# No Docker and no network: `install-artifact.sh` is a recorder in a fixture
# ARTIFACT_SOURCE_DIR and every agy binary is a fake on PATH. The real
# `read-pin-version.py` and `artifact_pin.py` run against the committed agy pin,
# so an exact-but-unpinned version is proved to be refused before anything is
# fetched, and the real run-report.sh / `recipe-contract tool-access` are used
# because status handling and the access audit are part of what is under test.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
INSTALL="$REPO_ROOT/images/tools/agy/install-agy.sh"
VERIFY="$REPO_ROOT/images/tools/agy/verify-agy.sh"
COMMITTED="$REPO_ROOT/images/tools/agy"
# The verifier executes `recipe-contract` from the contract directory, so build
# the native composed directory once before any restricted env -i replaces the
# tooling.
CONTRACT="$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)/out"

TEST_ROOT="$(mktemp -d /tmp/agy-installer.XXXXXX)"
chmod 0755 "$TEST_ROOT"
trap 'rm -rf "$TEST_ROOT"' EXIT

PYTHON_DIR="$(dirname "$(command -v python3)")"
TIMEOUT_DIR="$(dirname "$(command -v timeout)")"
CASE=""

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# --- 1. install-agy.sh black-box --------------------------------------------

# A real artifact source directory, with the installer replaced by a recorder.
# The loader, the pin reader and the committed pin are the real ones.
make_artifact_source() {
    local directory=$1
    mkdir -p "$directory/artifacts"
    chmod 0755 "$directory" "$directory/artifacts"
    cp "$REPO_ROOT"/images/artifacts/*.py "$directory/artifacts/"
    cat >"$directory/install-artifact.sh" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >>"$FAKE_OPLOG"
exit "${FAKE_ARTIFACT_STATUS:-0}"
SH
    chmod 0755 "$directory/install-artifact.sh"
}
SHARED_ARTIFACTS="$TEST_ROOT/shared-artifacts"
make_artifact_source "$SHARED_ARTIFACTS"

new_hook_case() {
    CASE="$TEST_ROOT/hook-$1"
    # The pin's directory names its artifact, so the fixture keeps production's
    # `images/tools/agy/` spelling.
    mkdir -p "$CASE/agy" "$CASE/artifacts/artifacts"
    chmod 0755 "$CASE" "$CASE/agy" "$CASE/artifacts" "$CASE/artifacts/artifacts"
    cp "$COMMITTED/pin.json" "$CASE/agy/pin.json"
    cp -R "$COMMITTED/evidence" "$CASE/agy/evidence"
    cp "$SHARED_ARTIFACTS"/artifacts/*.py "$CASE/artifacts/artifacts/"
    cp "$SHARED_ARTIFACTS/install-artifact.sh" "$CASE/artifacts/"
    OPLOG="$CASE/invocations.log"
}

# run_hook [VAR=value ...]; AGENT_VM_VERSION_AGY and the other required names are
# supplied unless the caller overrides them.
run_hook() {
    set +e
    RUN_OUTPUT="$(
        env -i \
            "PATH=$PYTHON_DIR:/usr/bin:/bin" \
            "AGENT_VM_VERSION_AGY=${WANT-1.4.11}" \
            "AGENT_VM_AGY_SOURCE_DIR=$CASE/agy" \
            "ARTIFACT_SOURCE_DIR=$CASE/artifacts" \
            "AGENT_VM_AGY_ARCH=${ARCH-amd64}" \
            "FAKE_OPLOG=$OPLOG" \
            "$@" \
            sh "$INSTALL" 2>&1
    )"
    RUN_STATUS=$?
    set -e
}

new_hook_case pinned
run_hook
[[ $RUN_STATUS -eq 0 ]] || fail "the pinned version must reach the installer: $RUN_OUTPUT"
[[ -f "$OPLOG" ]] || fail 'the installer was never invoked'
[[ "$(cat "$OPLOG")" == "agy $CASE/agy/pin.json amd64" ]] ||
    fail "the installer must receive the pin and architecture: $(cat "$OPLOG")"

new_hook_case arm64
ARCH=arm64 run_hook
[[ "$(cat "$OPLOG")" == "agy $CASE/agy/pin.json arm64" ]] ||
    fail "the arm64 token must be passed through: $(cat "$OPLOG")"

for value in 'v1.4.11' '1.4' 'latest' '1.4.11
2.0.0' '1.4.11 ' '01.4.11'; do
    new_hook_case "version-$RANDOM"
    WANT="$value" run_hook
    [[ $RUN_STATUS -ne 0 ]] || fail "the hook must refuse the version '$value'"
    [[ ! -e "$OPLOG" ]] || fail "the hook must refuse '$value' before the installer runs"
    [[ "$RUN_OUTPUT" == *"not an exact agy version"* ]] ||
        fail "the refusal must name the version grammar: $RUN_OUTPUT"
done

# An empty slot is refused by the required-variable guard, before the grammar.
new_hook_case empty
WANT='' run_hook
[[ $RUN_STATUS -ne 0 ]] || fail 'an empty version must be refused'
[[ ! -e "$OPLOG" ]] || fail 'an empty version must not reach the installer'

# An exact version the committed pin does not select: refused before any fetch.
new_hook_case unpinned
WANT=1.4.10 run_hook
[[ $RUN_STATUS -ne 0 ]] || fail 'an exact-but-unpinned version must be refused'
[[ "$RUN_OUTPUT" == *"the committed agy pin selects 1.4.11, not 1.4.10"* ]] ||
    fail "the refusal must name both versions: $RUN_OUTPUT"
[[ ! -e "$OPLOG" ]] || fail 'an unpinned version must not reach the installer'

new_hook_case missing-pin
rm -f "$CASE/agy/pin.json"
run_hook
[[ $RUN_STATUS -ne 0 ]] || fail 'a missing pin must be refused'
[[ ! -e "$OPLOG" ]] || fail 'a missing pin must not reach the installer'

new_hook_case bad-arch
ARCH=x86_64 run_hook
[[ $RUN_STATUS -ne 0 ]] || fail 'an unexpected architecture must be refused'
[[ "$RUN_OUTPUT" == *"unsupported architecture 'x86_64'"* ]] ||
    fail "the refusal must name the architecture: $RUN_OUTPUT"
[[ ! -e "$OPLOG" ]] || fail 'an unexpected architecture must not reach the installer'

new_hook_case installer-failure
run_hook FAKE_ARTIFACT_STATUS=1
[[ $RUN_STATUS -ne 0 ]] || fail 'an installer failure must propagate'

new_hook_case no-soft-fail
run_hook FAKE_ARTIFACT_STATUS=1 AGENT_INSTALL_SOFT_FAIL=1
[[ $RUN_STATUS -ne 0 ]] || fail 'agy is required: soft-fail must not soften a failure'
[[ ! -e "$CASE/status" ]] || fail 'a softened failure must not record a status'

echo "agy-installer: install hook cases passed"

# --- 2. verify-agy.sh black-box ---------------------------------------------

new_gate_case() {
    CASE="$TEST_ROOT/gate-$1"
    mkdir -p "$CASE/bin" "$CASE/status"
    chmod 0755 "$CASE" "$CASE/bin" "$CASE/status"
    GATE_BIN="$CASE/bin/agy"
    WANT=1.4.11
    GATE_ENV_VAR="AGY_CLI_DISABLE_AUTO_UPDATE=1"
    # The gate requires a root-owned 0755 binary and a black-box fixture cannot
    # create a root-owned file, so the expectation is the fixture binary's own
    # uid:gid (read in make_agy). GATE_OWNER_OMIT leaves the variable unset, which
    # is how the production default (0:0) is proved.
    GATE_OWNER=""
    GATE_OWNER_OMIT=""
}

# The same two `stat` dialects the gate reads, so the fixture's expectation is the
# file's real ownership rather than an assumption about `id -g`.
file_owner() { # $1 = path
    stat -c '%u:%g' "$1" 2>/dev/null || stat -f '%u:%g' "$1"
}

# Every accepting fake carries the auto-update variable, because the gate requires
# the release to still read it.
make_agy() {
    cat >"$GATE_BIN"
    chmod 0755 "$GATE_BIN"
    [ -n "$GATE_OWNER" ] || GATE_OWNER="$(file_owner "$GATE_BIN")"
}

run_gate() {
    local -a environment=()
    if [ -n "${GATE_ENV_VAR:-}" ]; then environment+=("$GATE_ENV_VAR"); fi
    if [ -z "$GATE_OWNER_OMIT" ] && [ -n "$GATE_OWNER" ]; then
        environment+=("AGENT_VM_AGY_EXPECTED_OWNER=$GATE_OWNER")
    fi
    set +e
    RUN_OUTPUT="$(
        env -i \
            "PATH=$CASE/bin:$TIMEOUT_DIR:$PYTHON_DIR:/usr/bin:/bin" \
            "AGENT_VM_CONTRACT_DIR=$CONTRACT" \
            "AGENT_VM_AGY_BIN=$GATE_BIN" \
            "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status" \
            "AGENT_VM_VERSION_AGY=${WANT-1.4.11}" \
            "${environment[@]+"${environment[@]}"}" \
            sh "$VERIFY" 2>&1
    )"
    RUN_STATUS=$?
    set -e
}

# The variable the gate insists the binary still names.
AUTOUPDATE='# AGY_CLI_DISABLE_AUTO_UPDATE'

new_gate_case ok
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "the exact report must pass: $RUN_OUTPUT"
[[ "$(cat "$CASE/status/agy")" == "installed" ]] || fail 'a passing gate must record installed'

new_gate_case mismatch
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.10\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a wrong installed version must fail'
[[ ! -e "$CASE/status/agy" ]] || fail 'a failed gate must not record installed'

new_gate_case v-prefix
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf 'v1.4.11\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a v-prefixed report must fail an exact pin'

new_gate_case missing-newline
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a report with no final newline must fail the byte contract'
[[ ! -e "$CASE/status/agy" ]] || fail 'a newline-less report must not record installed'

new_gate_case extra-line
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
printf 'update available\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a second report line must fail'

new_gate_case stderr
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
printf 'warning: telemetry disabled\n' >&2
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'any stderr output must fail the gate'

new_gate_case nonzero
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
exit 3
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a nonzero exit after the right report must fail'

new_gate_case hang
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
sleep 120
SH
started=$SECONDS
run_gate
elapsed=$((SECONDS - started))
[[ $RUN_STATUS -ne 0 ]] || fail 'a hanging --version must fail'
[[ $elapsed -lt 100 ]] || fail "the report must be bounded, took ${elapsed}s"

new_gate_case no-variable
make_agy <<'SH'
#!/bin/sh
printf '1.4.11\n'
SH
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a binary that does not name the switch must fail'
[[ "$RUN_OUTPUT" == *"no longer names AGY_CLI_DISABLE_AUTO_UPDATE"* ]] ||
    fail "the refusal must explain the self-update switch: $RUN_OUTPUT"

new_gate_case missing-binary
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a missing binary must fail'
[[ ! -e "$CASE/status/agy" ]] || fail 'a missing binary must not record installed'

new_gate_case private
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
chmod 0700 "$CASE/bin"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'an inaccessible command must fail the access audit'

new_gate_case empty-want
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
WANT='' run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'an empty expected version must fail'

new_gate_case switch-off
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
GATE_ENV_VAR="AGY_CLI_DISABLE_AUTO_UPDATE=0"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'the switch must be set to 1, not merely set'

new_gate_case switch-unset
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
GATE_ENV_VAR=""
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'an unset switch must fail the gate'

new_gate_case mode-775
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
chmod 0775 "$GATE_BIN"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a group-writable binary must fail the gate'
[[ "$RUN_OUTPUT" == *"want '755 "* ]] ||
    fail "the refusal must name the required mode and owner: $RUN_OUTPUT"
[[ "$RUN_OUTPUT" == *"is '775 "* ]] ||
    fail "the refusal must name the installed mode: $RUN_OUTPUT"
[[ ! -e "$CASE/status/agy" ]] || fail 'a 0775 binary must not record installed'

new_gate_case mode-777
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
chmod 0777 "$GATE_BIN"
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a world-writable binary must fail the gate'
[[ "$RUN_OUTPUT" == *"is '777 "* ]] ||
    fail "the refusal must name the world-writable mode: $RUN_OUTPUT"
[[ ! -e "$CASE/status/agy" ]] || fail 'a 0777 binary must not record installed'

new_gate_case owner-default
# With the expectation unset the gate demands the production owner, root:root.
make_agy <<SH
#!/bin/sh
$AUTOUPDATE
printf '1.4.11\n'
SH
GATE_OWNER_OMIT=1
run_gate
[[ $RUN_STATUS -ne 0 ]] || fail 'a non-root-owned binary must fail the gate'
[[ "$RUN_OUTPUT" == *"want '755 0:0'"* ]] ||
    fail "the default owner expectation must be 0:0: $RUN_OUTPUT"
[[ ! -e "$CASE/status/agy" ]] || fail 'a non-root-owned binary must not record installed'

echo "agy-installer black-box tests passed"
