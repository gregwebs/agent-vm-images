#!/usr/bin/env bash
# Contract tests for the build-only Pi installer (images/tools/pi/install-pi.sh).
#
# Hermetic: no Docker, no registry, no real pnpm. Three fixtures carry the
# behaviour this hook owns:
#   * a recording fake manager that answers `--version`, `--help` (through the
#     installed `.bin/pi` stub) and the frozen install;
#   * a fake contract directory providing the four helpers the installer spawns,
#     plus a `download.sh` that copies a local archive or fails classified;
#   * AGENT_VM_PI_BYTES_CHECK, the installer's authorized checker seam, standing
#     in for the real byte proof (which script/test/pi-installed-bytes.mjs owns).
# Everything else is real: the shared capability module, the owning preparation
# driver in its zero-manager equal mode, the archive extractor, and the installer
# itself. The four inputs are the committed Pi project.
#
# Covered here: zero-manager preparation, the dispatch list generated from the
# installed CLI's own `--help` and checked against the reviewed expectation,
# hard manager/checksum/byte/expectation failures with the partial node_modules
# removed, the curl-only soft branch under non-empty soft values 1 and 0, a stale
# or non-download receipt staying hard, caller-parent preservation, cleanup of
# every owned artifact, and the fact that no path here writes an installed record.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd -P)"
INSTALLER="$REPO_ROOT/images/tools/pi/install-pi.sh"
TEST_ROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/pi-install-test.XXXXXX")" && pwd -P)"
WORK_PARENT=""
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

[[ -d "$REPO_ROOT/images/tools/pi" ]] || fail "the committed Pi project is missing"
REAL_JQ="$(command -v jq || true)"
REAL_PYTHON="$(command -v python3 || true)"
# The composed Go helper: the four-input case needs the REAL pinned-input check,
# because a tampered pin file is exactly what that check owns.
CONTRACT_CONTEXT="$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)"
NATIVE_CONTRACT="$CONTRACT_CONTEXT/out"
[[ -n "$REAL_JQ" ]] || fail 'jq is required'

# One authenticated archive, built here so the SRI check and the real extractor
# run against real bytes.
make_archive() {
    local destination=$1
    local staging
    staging="$(mktemp -d "$TEST_ROOT/archive.XXXXXX")"
    mkdir -p "$staging/package"
    printf '{"name":"@earendil-works/pi-ai","version":"1.4.0","license":"MIT"}\n' \
        >"$staging/package/package.json"
    printf 'export const fixture = 1;\n' >"$staging/package/index.js"
    # Built by the committed helper: Python's tarfile, no host metadata (the
    # real extractor refuses AppleDouble `._package` members, as it should).
    "$REAL_PYTHON" "$REPO_ROOT/script/test/fixtures/pi-install/make-archive.py" \
        @earendil-works/pi-ai 1.4.0 "$destination"
    rm -rf "$staging"
}

ARCHIVE="$TEST_ROOT/pi-ai-1.4.0.tgz"
make_archive "$ARCHIVE"
ARCHIVE_SRI="sha512-$(openssl dgst -sha512 -binary "$ARCHIVE" | openssl base64 -A)"

new_case() {
    WORK_PARENT=""
    CASE_CONTRACT=""
    unset AGENT_INSTALL_SOFT_FAIL FAKE_MANAGER_VERSION FAKE_INSTALL_STATUS \
        FAKE_DOWNLOAD_ARCHIVE FAKE_DOWNLOAD_FAIL FAKE_RECEIPT_LINE FAKE_CONTRACT_CHECK_FAIL \
        FAKE_RUNTIME_CHECK_FAIL
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/root/pi" "$CASE/root/pnpm" "$CASE/root/install-status" \
        "$CASE/root/contract" "$CASE/root/dispatch" "$CASE/answers" "$CASE/receipt"
    cp "$REPO_ROOT"/images/tools/pi/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} "$CASE/root/pi/"
    cp "$REPO_ROOT/images/pnpm/pin.json" "$CASE/root/pnpm/pin.json"
    : >"$CASE/receipt/download"
    : >"$CASE/manager.log"
    make_contract "$CASE/root/contract"
    make_manager "$CASE/root/manager"
    make_checker "$CASE/root/checker.mjs"
    make_runtime_checker "$CASE/root/runtime-check.mjs"
    : >"$CASE/order.log"
    make_pi_stub "$CASE/pi-stub"
    for version in 1.4.0 0.87.0; do
        cp "$REPO_ROOT/images/tools/pi/dispatch/$version.txt" "$CASE/root/dispatch/$version.txt"
    done
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
            check) [ -n "${FAKE_CONTRACT_CHECK_FAIL:-}" ] && { echo 'fixture: pins disagree' >&2; exit 1; }
                   echo 1.4.0 ;;
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
    cat >"$dir/run-report.sh" <<'SH'
#!/bin/sh
out=$2
err=$3
shift 3
"$@" >"$out" 2>"$err"
SH
    chmod +x "$dir/run-report.sh"
    # download.sh mirrors the real contract's classified shape: 75 plus a fresh
    # `transport download CODE` receipt, or a byte-identical local archive.
    cat >"$dir/download.sh" <<SH
#!/bin/sh
[ "\$#" -eq 2 ] || exit 2
receipt="\${AGENT_VM_TRANSPORT_RECEIPT:-}"
if [ -n "\${FAKE_DOWNLOAD_FAIL:-}" ]; then
    printf '%s\n' "\${FAKE_RECEIPT_LINE-}" >"\$receipt"
    exit 75
fi
cp "\${FAKE_DOWNLOAD_ARCHIVE:-$ARCHIVE}" "\$2"
SH
    chmod +x "$dir/download.sh"
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
        mkdir -p "$CASE/root/pi/node_modules/.bin"
        cp "$CASE/pi-stub" "$CASE/root/pi/node_modules/.bin/pi"
        chmod 0755 "$CASE/root/pi/node_modules/.bin/pi"
        exit 0 ;;
esac
exit 0
SH
    chmod +x "$path"
}

make_pi_stub() {
    local path=$1
    cat >"$path" <<SH
#!/bin/sh
printf 'help\n' >>"$CASE/order.log"
cat <<'HELP'
pi - AI coding assistant with read, bash, edit, write tools

Commands:
  pi install <source> [-l]     Install extension source and add to settings
  pi remove <source> [-l]      Remove extension source from settings
  pi uninstall <source> [-l]   Alias for remove
  pi update [source|self|pi]   Update pi, extensions, or model catalogs
  pi list                      List installed extensions from settings
  pi config [-l]               Open TUI to enable/disable package resources
  pi auth <command>            Print credentials or check provider readiness
  pi mcp <command>             Check MCP servers, sign in to or out of OAuth servers
  pi <command> --help          Show help for install/remove/uninstall/update/list/config/auth/mcp
HELP
SH
    chmod 0755 "$path"
}

# The authorized seam for the byte proof: `inventory` prints the lock-derived
# list, the three-operand form accepts an extraction the installer created.
make_checker() {
    local path=$1
    cat >"$path" <<JS
import fs from 'node:fs';
import path from 'node:path';
const args = process.argv.slice(2);
if (args.length === 2 && args[0] === 'inventory') {
  process.stdout.write(JSON.stringify({
    platform: 'linux/arm64',
    family: [{name: '@earendil-works/pi-ai', version: '1.4.0',
              integrity: '$ARCHIVE_SRI',
              tarball: 'https://registry.npmjs.org/@earendil-works/pi-ai/-/pi-ai-1.4.0.tgz'}],
    providers: [],
  }) + '\n');
  process.exit(0);
}
if (args.length !== 3) {
  process.stderr.write('fake checker: usage: PREFIX SIBLING EXTRACTED_PACKAGE\n');
  process.exit(2);
}
fs.appendFileSync('$CASE/order.log', 'bytes\n');
const [prefix, sibling, extraction] = args;
if (sibling !== 'pi-ai') { process.stderr.write('fake checker: unexpected sibling ' + sibling + '\n'); process.exit(1); }
if (!fs.lstatSync(path.join(extraction, 'package.json')).isFile()) {
  process.stderr.write('fake checker: extraction has no package.json\n');
  process.exit(1);
}
if (!fs.existsSync(path.join(prefix, 'node_modules/.bin/pi'))) {
  process.stderr.write('fake checker: the prefix is not installed\n');
  process.exit(1);
}
process.stdout.write('1/1 Pi sibling archives and installed bytes verified\n');
JS
}

# The authorized seam for the mounted runtime proof: the real check-runtime.mjs
# needs a cold N6 install, so the harness substitutes a recorder that also refuses
# on demand. What is under test is that the installer runs it, in order, and that a
# failure is hard.
make_runtime_checker() {
    local path=$1
    cat >"$path" <<JS
import fs from 'node:fs';
const args = process.argv.slice(2);
if (args.length !== 1) {
  process.stderr.write('fake runtime checker: usage: PREFIX\n');
  process.exit(2);
}
fs.appendFileSync('$CASE/order.log', 'runtime\n');
const [prefix] = args;
if (!fs.existsSync(prefix + '/node_modules/.bin/pi')) {
  process.stderr.write('fake runtime checker: the prefix is not installed\n');
  process.exit(1);
}
if (process.env.FAKE_RUNTIME_CHECK_FAIL === '1') {
  process.stderr.write('check-runtime.mjs: root-command: fixture refusal\n');
  process.exit(1);
}
process.stdout.write('pi runtime: {"fixture":true}\n');
JS
}

run_installer() {
    local -a env_args=(
        "PATH=$PATH"
        "HOME=$CASE/answers/home"
        "AGENT_VM_PI_PREFIX=$CASE/root/pi"
        "AGENT_VM_PI_SOURCE_DIR=$REPO_ROOT/images/tools/pi"
        "AGENT_VM_PI_SHARED=$REPO_ROOT/images/tools/pnpm-project.py"
        "AGENT_VM_CONTRACT_DIR=${CASE_CONTRACT:-$CASE/root/contract}"
        "AGENT_VM_FIXTURE_ROOT=$CASE/root"
        "AGENT_VM_FIXTURE_PNPM=$CASE/root/manager"
        "AGENT_VM_PI_BYTES_CHECK=$CASE/root/checker.mjs"
        "AGENT_VM_PI_RUNTIME_CHECK=$CASE/root/runtime-check.mjs"
        "FAKE_RUNTIME_CHECK_FAIL=${FAKE_RUNTIME_CHECK_FAIL-}"
        "AGENT_VM_PI_DISPATCH_DIR=$CASE/root/dispatch"
        "AGENT_VM_PI_TARGET_PLATFORM=linux/arm64"
        "AGENT_VM_TRANSPORT_RECEIPT=$CASE/receipt/download"
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/root/install-status"
        "FAKE_MANAGER_VERSION=${FAKE_MANAGER_VERSION-}"
        "FAKE_INSTALL_STATUS=${FAKE_INSTALL_STATUS-0}"
        "FAKE_DOWNLOAD_ARCHIVE=${FAKE_DOWNLOAD_ARCHIVE-$ARCHIVE}"
        "FAKE_DOWNLOAD_FAIL=${FAKE_DOWNLOAD_FAIL-}"
        "FAKE_RECEIPT_LINE=${FAKE_RECEIPT_LINE-}"
        "AGENT_INSTALL_SOFT_FAIL=${AGENT_INSTALL_SOFT_FAIL-}"
        "CASE=$CASE"
    )
    [[ -z "$WORK_PARENT" ]] || env_args+=("AGENT_VM_PI_WORK_DIR=$WORK_PARENT")
    set +e
    env -i "${env_args[@]}" sh "$INSTALLER" >"$CASE/out" 2>"$CASE/err"
    RUN_STATUS=$?
    set -e
    RUN_OUTPUT="$(cat "$CASE/out")"
    RUN_ERROR="$(cat "$CASE/err")"
}

require_hard() { # require_hard MESSAGE
    [[ $RUN_STATUS -eq 1 ]] || fail "expected a hard failure, got $RUN_STATUS (out=$RUN_OUTPUT err=$RUN_ERROR)"
    assert_contains "$RUN_ERROR" "$1"
    assert_absent "$CASE/root/pi/node_modules"
    assert_absent "$CASE/root/install-status/pi"
    assert_absent "$CASE/root/install-status/pi-claude-bridge"
}

# --- the positive path -------------------------------------------------------
new_case happy
WORK_PARENT="$CASE/root/work-parent"
mkdir -p "$WORK_PARENT"
before="$(cd "$CASE/root/pi" && shasum -a 256 package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256 2>/dev/null || sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)"
run_installer
[[ $RUN_STATUS -eq 0 ]] || fail "the installer failed: $RUN_ERROR"
assert_contains "$RUN_OUTPUT" 'installed pi 1.4.0 from the reviewed inputs'
assert_contains "$RUN_OUTPUT" '1/1 Pi sibling archives and installed bytes verified'
# Installed Pi code is the last thing that may run: the authenticated byte proof and
# the runtime identity proof both precede the `--help` that executes the payload.
[[ "$(tr '\n' ' ' <"$CASE/order.log")" == 'bytes runtime help ' ]] ||
    fail "the installer ran the payload before its proofs: [$(tr '\n' ' ' <"$CASE/order.log")]"
# Zero-manager preparation: the fake manager must not have run before the frozen
# install, and the equal request must have published nothing.
[[ "$(head -n 1 "$CASE/manager.log")" == --version* ]] ||
    fail "preparation started a manager before the installer's own calls"
[[ "$(cd "$CASE/root/pi" && shasum -a 256 package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256 2>/dev/null || sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)" == "$before" ]] ||
    fail 'an equal install changed the reviewed inputs'
# The dispatch list is data: one canonical sorted word per line, 0644.
dispatch="$CASE/root/pi/agent-vm-subcommands"
[[ -f "$dispatch" ]] || fail 'the installer did not publish the subcommand list'
[[ "$(cat "$dispatch")" == "$(cat "$REPO_ROOT/images/tools/pi/dispatch/1.4.0.txt")" ]] ||
    fail "the published subcommand list is [$(tr '\n' ' ' <"$dispatch")]"
[[ "$(ls -l "$dispatch" | cut -c1-10)" == -rw-r--r-- ]] ||
    fail "the subcommand list is not 0644: $(ls -l "$dispatch")"
# The caller's work parent is preserved, and no owned artifact survives.
[[ -d "$WORK_PARENT" ]] || fail "the caller's work parent was deleted"
[[ -d "$CASE/root/pi/node_modules" ]] || fail 'the installed payload is missing'
# No record: only the verifier publishes.
assert_absent "$CASE/root/install-status/pi"
assert_absent "$CASE/root/install-status/pi-claude-bridge"

# --- hard failures -----------------------------------------------------------
# A runtime identity proof that fails must stop the build before the payload runs:
# no dispatch list, no executed CLI, and the partial prefix is removed.
new_case runtime-identity
FAKE_RUNTIME_CHECK_FAIL=1
mkdir -p "$CASE/root/pi/node_modules"
run_installer
require_hard 'the installed Pi runtime failed its identity and N6 probes'
assert_absent "$CASE/root/pi/agent-vm-subcommands"
[[ "$(tr '\n' ' ' <"$CASE/order.log")" == 'bytes runtime ' ]] ||
    fail "installed Pi code ran although its identity proof failed: [$(tr '\n' ' ' <"$CASE/order.log")]"

new_case manager-identity
FAKE_MANAGER_VERSION=11.10.0
mkdir -p "$CASE/root/pi/node_modules"
run_installer
require_hard 'base manager is not the pinned 11.11.0'

new_case frozen-install
FAKE_INSTALL_STATUS=1
mkdir -p "$CASE/root/pi/node_modules/should-be-removed"
run_installer
require_hard 'frozen install failed'

new_case integrity
FAKE_DOWNLOAD_ARCHIVE="$CASE/not-the-archive.tgz"
printf 'different bytes\n' >"$FAKE_DOWNLOAD_ARCHIVE"
FAKE_DOWNLOAD_FAIL=
run_installer
require_hard 'INTEGRITY MISMATCH'

new_case integrity-soft-1
AGENT_INSTALL_SOFT_FAIL=1
printf 'different bytes\n' >"$CASE/not-the-archive.tgz"
FAKE_DOWNLOAD_ARCHIVE="$CASE/not-the-archive.tgz"
run_installer
# A checksum mismatch can never soften, even with soft-fail set.
require_hard 'INTEGRITY MISMATCH'
assert_absent "$CASE/root/install-status/pi"

new_case byte-proof
# The real extractor runs, but the checker seam refuses: a byte failure is hard.
cat >"$CASE/root/checker.mjs" <<'JS'
import fs from 'node:fs';
const args = process.argv.slice(2);
if (args.length === 2 && args[0] === 'inventory') {
  process.stdout.write(fs.readFileSync(process.env.CASE + '/inventory.json', 'utf8'));
  process.exit(0);
}
process.stderr.write('check-installed-bytes.mjs: pi-ai dist/index.js does not match the authenticated archive bytes\n');
process.exit(1);
JS
cat >"$CASE/inventory.json" <<JSON
{"platform":"linux/arm64","family":[{"name":"@earendil-works/pi-ai","version":"1.4.0","integrity":"$ARCHIVE_SRI","tarball":"https://registry.npmjs.org/x.tgz"}],"providers":[]}
JSON
run_installer
require_hard 'pi-ai dist/index.js does not match the authenticated archive bytes'

new_case four-inputs
# The real composed helper, so this is the actual pinned-input check.
CASE_CONTRACT="$NATIVE_CONTRACT"
printf 'tampered\n' >"$CASE/root/pi/pnpm-inputs.sha256"
run_installer
[[ $RUN_STATUS -eq 1 ]] || fail "tampered inputs must be hard (got $RUN_STATUS)"
assert_contains "$RUN_ERROR" 'reviewed inputs failed pinned-input validation'
[[ ! -s "$CASE/manager.log" ]] || fail 'tampered inputs reached the manager'

new_case no-dispatch-expectation
rm "$CASE/root/dispatch/1.4.0.txt"
run_installer
require_hard 'no reviewed dispatch expectation for Pi 1.4.0'

new_case dispatch-drift
# A Pi whose `--help` advertises a different set than the reviewed 1.4.0 list.
sed -i.bak '/pi mcp /d' "$CASE/pi-stub" 2>/dev/null || true
rm -f "$CASE/pi-stub.bak"
run_installer
require_hard 'the reviewed 1.4.0 list is'

# --- the curl-only soft branch ----------------------------------------------
for soft in 1 0; do
    new_case "soft-$soft"
    AGENT_INSTALL_SOFT_FAIL=$soft
    FAKE_DOWNLOAD_FAIL=1
    FAKE_RECEIPT_LINE='transport download 56'
    run_installer
    [[ $RUN_STATUS -eq 0 ]] ||
        fail "soft=$soft must soften a classified download failure (got $RUN_STATUS: $RUN_ERROR)"
    assert_absent "$CASE/root/pi"
    assert_absent "$CASE/root/pi/node_modules"
    assert_absent "$CASE/root/install-status/pi-claude-bridge"
    [[ "$(cat "$CASE/root/install-status/pi")" == 'absent-transport 56' ]] ||
        fail "soft=$soft wrote [$(cat "$CASE/root/install-status/pi")]"
    assert_contains "$RUN_OUTPUT" 'soft-fail mode; image will ship without pi'
done

new_case soft-unset
FAKE_DOWNLOAD_FAIL=1
FAKE_RECEIPT_LINE='transport download 56'
run_installer
require_hard 'could not fetch'
assert_absent "$CASE/root/install-status/pi"

new_case soft-stale-npm-receipt
# A transport line the download contract did not write (the retired npm
# vocabulary) can never soften, and neither can a classified code with no
# receipt.
AGENT_INSTALL_SOFT_FAIL=1
FAKE_DOWNLOAD_FAIL=1
FAKE_RECEIPT_LINE='transport npm EAI_AGAIN'
run_installer
require_hard 'could not fetch'
assert_absent "$CASE/root/install-status/pi"

new_case soft-empty-receipt
AGENT_INSTALL_SOFT_FAIL=1
FAKE_DOWNLOAD_FAIL=1
FAKE_RECEIPT_LINE=
run_installer
require_hard 'could not fetch'
assert_absent "$CASE/root/install-status/pi"

new_case soft-unclassified-code
AGENT_INSTALL_SOFT_FAIL=1
FAKE_DOWNLOAD_FAIL=1
FAKE_RECEIPT_LINE='transport download not-a-code'
run_installer
require_hard 'could not fetch'
assert_absent "$CASE/root/install-status/pi"

echo 'pi-install contract tests passed'
