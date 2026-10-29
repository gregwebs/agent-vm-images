#!/usr/bin/env bash
# Contract tests for the bridge installer (images/tools/pi/install-pi-packages.sh).
#
# The bridge has no byte-defence branch and, after the acknowledged #42/ADR-0002
# narrowing, no absence producer at all: every failure is hard in every soft
# mode, and the only bridge absence this image can have is the derivative of a
# genuine Pi curl absence, which the verifier's gate entry owns. This suite pins
# exactly that, plus the reviewed hoisted layout (SDK JavaScript kept, platform
# payload omitted, no second Pi, no .pnpm), the partial node_modules removal, the
# cleanup of every owned artifact and the fact that no path here writes a record.
#
# Hermetic: a recording fake manager and a fake contract directory. The four
# inputs are the committed bridge project.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd -P)"
INSTALLER="$REPO_ROOT/images/tools/pi/install-pi-packages.sh"
TEST_ROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/pi-bridge-install-test.XXXXXX")" && pwd -P)"
CASE=""
RUN_STATUS=0
RUN_OUTPUT=""
RUN_ERROR=""
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_contains() {
    [[ "$1" == *"$2"* ]] || fail "expected [$2] in output, got [$1]"
}

assert_absent() {
    [[ ! -e "$1" && ! -L "$1" ]] || fail "expected $1 to be absent"
}

[[ -d "$REPO_ROOT/images/tools/pi/bridge" ]] || fail "the committed bridge project is missing"

new_case() {
    unset FAKE_MANAGER_VERSION FAKE_INSTALL_STATUS FAKE_LAYOUT
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/root/bridge" "$CASE/root/pnpm" "$CASE/root/install-status" \
        "$CASE/root/contract" "$CASE/receipt"
    cp "$REPO_ROOT"/images/tools/pi/bridge/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} "$CASE/root/bridge/"
    cp "$REPO_ROOT/images/pnpm/pin.json" "$CASE/root/pnpm/pin.json"
    : >"$CASE/manager.log"
    make_contract "$CASE/root/contract"
    make_manager "$CASE/root/manager"
    RUN_STATUS=0
    RUN_OUTPUT=""
    RUN_ERROR=""
}

make_contract() {
    local dir=$1
    cat >"$dir/recipe-contract" <<'SH'
#!/bin/sh
case "$1" in
    pnpm-environment) exit 0 ;;
    pnpm-lock)
        shift
        case "$1" in
            check) echo 0.8.0 ;;
            graph) echo '{}' ;;
        esac
        exit 0 ;;
esac
exit 0
SH
    chmod +x "$dir/recipe-contract"
    cat >"$dir/run-pnpm.sh" <<'SH'
#!/bin/sh
[ "$#" -ge 3 ] && [ "$2" = -- ] || exit 2
shift 2
exec "$@"
SH
    chmod +x "$dir/run-pnpm.sh"
}

make_manager() {
    local path=$1
    cat >"$path" <<SH
#!/bin/sh
printf '%s\n' "\$*" >>"$CASE/manager.log"
case "\$1" in
    --version) echo "\${FAKE_MANAGER_VERSION:-11.11.0}"; exit 0 ;;
    install)
        [ "\${FAKE_INSTALL_STATUS:-0}" -eq 0 ] || exit "\${FAKE_INSTALL_STATUS}"
        layout="\${FAKE_LAYOUT:-hoisted}"
        mkdir -p "$CASE/root/bridge/node_modules/@anthropic-ai" \\
                 "$CASE/root/bridge/node_modules/@anthropic-ai/claude-agent-sdk" \\
                 "$CASE/root/bridge/node_modules/pi-claude-bridge/src"
        case "\$layout" in
            hoisted) ;;
            isolated) mkdir -p "$CASE/root/bridge/node_modules/.pnpm/x/node_modules" ;;
            sdk) rm -rf "$CASE/root/bridge/node_modules/@anthropic-ai/claude-agent-sdk" ;;
            platform) mkdir -p "$CASE/root/bridge/node_modules/@anthropic-ai/claude-agent-sdk-linux-arm64" ;;
            alias) mkdir -p "$CASE/root/bridge/node_modules/@earendil-works" ;;
            nosrc) rm -rf "$CASE/root/bridge/node_modules/pi-claude-bridge/src" ;;
        esac
        printf '{"name":"@anthropic-ai/claude-agent-sdk","version":"0.3.281"}\n' \\
            >"$CASE/root/bridge/node_modules/@anthropic-ai/claude-agent-sdk/package.json" 2>/dev/null || :
        : >"$CASE/root/bridge/node_modules/pi-claude-bridge/src/index.ts" 2>/dev/null || :
        exit 0 ;;
esac
exit 0
SH
    chmod +x "$path"
}

run_installer() {
    local -a env_args=(
        "PATH=$PATH"
        "HOME=$CASE/home"
        "AGENT_VM_PI_PACKAGES_PREFIX=$CASE/root/bridge"
        "AGENT_VM_PI_SOURCE_DIR=$REPO_ROOT/images/tools/pi"
        "AGENT_VM_PI_SHARED=$REPO_ROOT/images/tools/pnpm-project.py"
        "AGENT_VM_CONTRACT_DIR=$CASE/root/contract"
        "AGENT_VM_FIXTURE_ROOT=$CASE/root"
        "AGENT_VM_FIXTURE_PNPM=$CASE/root/manager"
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/root/install-status"
        "AGENT_VM_TRANSPORT_RECEIPT=$CASE/receipt/download"
        "FAKE_MANAGER_VERSION=${FAKE_MANAGER_VERSION-}"
        "FAKE_INSTALL_STATUS=${FAKE_INSTALL_STATUS-0}"
        "FAKE_LAYOUT=${FAKE_LAYOUT-}"
        "AGENT_INSTALL_SOFT_FAIL=${AGENT_INSTALL_SOFT_FAIL-}"
        "CASE=$CASE"
    )
    set +e
    env -i "${env_args[@]}" sh "$INSTALLER" >"$CASE/out" 2>"$CASE/err"
    RUN_STATUS=$?
    set -e
    RUN_OUTPUT="$(cat "$CASE/out")"
    RUN_ERROR="$(cat "$CASE/err")"
}

require_hard() { # require_hard DIAGNOSTIC
    [[ $RUN_STATUS -eq 1 ]] || fail "expected a hard failure, got $RUN_STATUS ($RUN_ERROR)"
    assert_contains "$RUN_ERROR" "$1"
    assert_absent "$CASE/root/install-status/pi"
    assert_absent "$CASE/root/install-status/pi-claude-bridge"
}

# --- the positive path -------------------------------------------------------
new_case happy
run_installer
[[ $RUN_STATUS -eq 0 ]] || fail "the bridge installer failed: $RUN_ERROR"
assert_contains "$RUN_OUTPUT" 'installed pi-claude-bridge 0.8.0 from the reviewed inputs'
[[ -d "$CASE/root/bridge/node_modules" ]] || fail 'the bridge payload is missing'
# No record and no absence: publishing belongs to the verifier alone.
assert_absent "$CASE/root/install-status/pi"
assert_absent "$CASE/root/install-status/pi-claude-bridge"
# The manager was called with the reviewed frozen selection.
grep -q '^install --prod --frozen-lockfile ' "$CASE/manager.log" ||
    fail "the frozen install argv is wrong: $(cat "$CASE/manager.log")"

# --- hard failures, in every soft mode --------------------------------------
new_case manager-identity
FAKE_MANAGER_VERSION=11.9.0
run_installer
require_hard 'base manager is not the pinned 11.11.0'

new_case frozen-install
FAKE_INSTALL_STATUS=1
mkdir -p "$CASE/root/bridge/node_modules/half-written"
run_installer
require_hard 'frozen install failed'
assert_absent "$CASE/root/bridge/node_modules"

for soft in '' 1 0; do
    new_case "no-absence-producer-soft-${soft:-unset}"
    AGENT_INSTALL_SOFT_FAIL=$soft
    FAKE_INSTALL_STATUS=1
    mkdir -p "$CASE/root/bridge/node_modules/half-written"
    # Even a fresh classified curl receipt cannot soften a bridge failure.
    printf 'transport download 56\n' >"$CASE/receipt/download"
    run_installer
    require_hard 'frozen install failed'
    assert_absent "$CASE/root/bridge/node_modules"
    assert_absent "$CASE/root/install-status/pi-claude-bridge"
done

new_case isolated-layout
FAKE_LAYOUT=isolated
run_installer
require_hard 'private-store package payload'

new_case sdk-missing
FAKE_LAYOUT=sdk
run_installer
require_hard 'the Claude Agent SDK JavaScript is missing'

new_case platform-native
FAKE_LAYOUT=platform
run_installer
require_hard 'ships the omitted SDK platform package'

new_case second-pi
FAKE_LAYOUT='alias'
run_installer
require_hard 'installs @earendil-works'

new_case missing-source
FAKE_LAYOUT=nosrc
run_installer
require_hard 'no loadable TypeScript entry point'

new_case unauthorized-prefix
# A prefix outside the fixture root must be refused before anything is mutated.
mv "$CASE/root/bridge" "$CASE/outside-bridge"
set +e
env -i "PATH=$PATH" "HOME=$CASE/home" \
    "AGENT_VM_PI_PACKAGES_PREFIX=$CASE/outside-bridge" \
    "AGENT_VM_PI_SOURCE_DIR=$REPO_ROOT/images/tools/pi" \
    "AGENT_VM_PI_SHARED=$REPO_ROOT/images/tools/pnpm-project.py" \
    "AGENT_VM_CONTRACT_DIR=$CASE/root/contract" \
    "AGENT_VM_FIXTURE_ROOT=$CASE/root" \
    "AGENT_VM_FIXTURE_PNPM=$CASE/root/manager" \
    sh "$INSTALLER" >"$CASE/out" 2>"$CASE/err"
    RUN_STATUS=$?
set -e
RUN_ERROR="$(cat "$CASE/err")"
[[ $RUN_STATUS -eq 1 ]] || fail 'an unauthorized prefix must be a hard failure'
assert_contains "$RUN_ERROR" 'pnpm-project:'

echo 'pi-bridge-install contract tests passed'
