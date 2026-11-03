#!/usr/bin/env bash
# Contract tests for the Antigravity provider installer
# (images/tools/pi/install-pi-antigravity.sh).
#
# The provider has no byte-defence branch and no absence producer at all: every
# failure is hard in every soft mode, and the only provider absence this image
# can have is the derivative of a genuine Pi curl absence, which the verifier's
# gate entry owns. This suite pins exactly that, plus the reviewed hoisted layout
# (the provider's one runtime dependency kept, no second Pi, no .pnpm), the
# partial node_modules removal, the cleanup of every owned artifact and the fact
# that no path here writes a record. It also pins the one reviewed exception the
# workspace carries: `trustPolicyExclude: ['pi-antigravity@0.11.0']` (ADR 0016).
#
# Hermetic: a recording fake manager and a fake contract directory. The four
# inputs are the committed provider project.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd -P)"
INSTALLER="$REPO_ROOT/images/tools/pi/install-pi-antigravity.sh"
TEST_ROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/pi-antigravity-install-test.XXXXXX")" && pwd -P)"
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

[[ -d "$REPO_ROOT/images/tools/pi/antigravity" ]] || fail "the committed antigravity project is missing"

new_case() {
    unset FAKE_MANAGER_VERSION FAKE_INSTALL_STATUS FAKE_LAYOUT
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/root/antigravity" "$CASE/root/pnpm" "$CASE/root/install-status" \
        "$CASE/root/contract" "$CASE/receipt"
    cp "$REPO_ROOT"/images/tools/pi/antigravity/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} "$CASE/root/antigravity/"
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
            check) echo 0.11.0 ;;
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
        mkdir -p "$CASE/root/antigravity/node_modules/undici" \\
                 "$CASE/root/antigravity/node_modules/pi-antigravity/src"
        case "\$layout" in
            hoisted) ;;
            isolated) mkdir -p "$CASE/root/antigravity/node_modules/.pnpm/x/node_modules" ;;
            undici) rm -rf "$CASE/root/antigravity/node_modules/undici" ;;
            alias) mkdir -p "$CASE/root/antigravity/node_modules/@earendil-works" ;;
            nosrc) rm -rf "$CASE/root/antigravity/node_modules/pi-antigravity/src" ;;
        esac
        # A layout may have removed the directory a payload file goes into, and a
        # redirect into a missing directory is fatal in dash: a redirection error on
        # the null command exits a non-interactive shell, so a trailing
        # double-pipe-colon guard never runs. Guard the writes instead.
        undici_dir="$CASE/root/antigravity/node_modules/undici"
        if [ -d "\$undici_dir" ]; then
            printf '{"name":"undici","version":"8.12.3"}\n' \\
                >"\$undici_dir/package.json"
        fi
        src_dir="$CASE/root/antigravity/node_modules/pi-antigravity/src"
        if [ -d "\$src_dir" ]; then
            : >"\$src_dir/index.ts"
        fi
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
        "AGENT_VM_PI_ANTIGRAVITY_PREFIX=$CASE/root/antigravity"
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
    assert_absent "$CASE/root/install-status/pi-antigravity"
}

# --- the positive path -------------------------------------------------------
new_case happy
run_installer
[[ $RUN_STATUS -eq 0 ]] || fail "the provider installer failed: $RUN_ERROR"
assert_contains "$RUN_OUTPUT" 'installed pi-antigravity 0.11.0 from the reviewed inputs'
[[ -d "$CASE/root/antigravity/node_modules" ]] || fail 'the provider payload is missing'
# No record and no absence: publishing belongs to the verifier alone.
assert_absent "$CASE/root/install-status/pi"
assert_absent "$CASE/root/install-status/pi-antigravity"
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
mkdir -p "$CASE/root/antigravity/node_modules/half-written"
run_installer
require_hard 'frozen install failed'
assert_absent "$CASE/root/antigravity/node_modules"

for soft in '' 1 0; do
    new_case "no-absence-producer-soft-${soft:-unset}"
    AGENT_INSTALL_SOFT_FAIL=$soft
    FAKE_INSTALL_STATUS=1
    mkdir -p "$CASE/root/antigravity/node_modules/half-written"
    # Even a fresh classified curl receipt cannot soften a provider failure.
    printf 'transport download 56\n' >"$CASE/receipt/download"
    run_installer
    require_hard 'frozen install failed'
    assert_absent "$CASE/root/antigravity/node_modules"
    assert_absent "$CASE/root/install-status/pi-antigravity"
done

new_case isolated-layout
FAKE_LAYOUT=isolated
run_installer
require_hard 'private-store package payload'

new_case undici-missing
FAKE_LAYOUT=undici
run_installer
require_hard 'the provider runtime dependency undici is missing'

new_case second-pi
FAKE_LAYOUT='alias'
run_installer
require_hard 'installs @earendil-works'

new_case missing-source
FAKE_LAYOUT=nosrc
run_installer
require_hard 'no loadable TypeScript entry point'

new_case workspace-policy
# The reviewed exception is one exact version, not a workspace relaxation: the
# committed workspace text is what the Go policy compares byte for byte.
grep -Fq "trustPolicyExclude: ['pi-antigravity@0.11.0']" \
    "$REPO_ROOT/images/tools/pi/antigravity/pnpm-workspace.yaml" ||
    fail 'the reviewed trust exception is missing from the provider workspace'
[[ "$(grep -c 'trustPolicyExclude' "$REPO_ROOT/images/tools/pi/antigravity/pnpm-workspace.yaml")" -eq 1 ]] ||
    fail 'the provider workspace names more than one trust exception'
grep -Fq '"pi-antigravity": "0.11.0"' "$REPO_ROOT/images/tools/pi/antigravity/package.json" ||
    fail 'the committed provider manifest does not pin 0.11.0'

new_case unauthorized-prefix
# A prefix outside the fixture root must be refused before anything is mutated.
mv "$CASE/root/antigravity" "$CASE/outside-antigravity"
set +e
env -i "PATH=$PATH" "HOME=$CASE/home" \
    "AGENT_VM_PI_ANTIGRAVITY_PREFIX=$CASE/outside-antigravity" \
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
# A prefix outside the authorized root is refused by name, not silently relocated.
assert_contains "$RUN_ERROR" 'pnpm-project: '
assert_absent "$CASE/outside-antigravity/node_modules"

echo 'pi-antigravity-install contract tests passed'
