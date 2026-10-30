#!/usr/bin/env bash
# Black-box contract tests for the Pi build gate (images/tools/pi/verify-pi.sh),
# including the one absence decision it now owns through
# `pnpm-project.py pi-gate-entry`.
#
# No Docker, no real Pi, no real pnpm: a fixture prefix mirrors the installed
# layout with a stub entry point and a stub wrapper, the REAL composed
# recipe-contract helper supplies `install-status`/`tool-access`, and a stub
# wrapper answers `--version`, `--help` and the two RPC probes. Everything the
# gate decides for itself -- absence recognition, retirement inventory, the
# reviewed dispatch list, per-tool aggregation and the two independent records --
# is decided by the real script.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd -P)"
GATE="$REPO_ROOT/images/tools/pi/verify-pi.sh"
PROJECT="$REPO_ROOT/images/tools/pnpm-project.py"
CONTRACT_CONTEXT="$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)"
CONTRACT="$CONTRACT_CONTEXT/out"
TEST_ROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/pi-verify-test.XXXXXX")" && pwd -P)"
# The gate audits reachability for an arbitrary uid, ancestors included, so the
# harness's own roots must be traversable exactly like an image layer.
chmod 0755 "$TEST_ROOT"
CASE=""
RUN_STATUS=0
RUN_OUTPUT=""
RUN_ERROR=""
trap 'chmod -R u+rwX "$TEST_ROOT" 2>/dev/null || true; rm -rf "$TEST_ROOT"' EXIT

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
[[ -d "$REPO_ROOT/images/tools/pi/bridge" ]] || fail "the committed bridge project is missing"

# --- fixtures ----------------------------------------------------------------
make_wrapper() { # make_wrapper PATH
    cat >"$1" <<'SH'
#!/usr/bin/env bash
set -u
# Every invocation is recorded when a case asks for it, so an ordering case can
# assert that the wrapper never ran instead of inferring it from the output.
if [ -n "${FAKE_WRAPPER_LOG-}" ]; then
    printf '%s\n' "$*" >>"$FAKE_WRAPPER_LOG"
fi
for argument in "$@"; do
    case "$argument" in
        --version)
            echo "${FAKE_VERSION:-1.4.0}"
            [ -z "${FAKE_VERSION_TRAILER-}" ] || echo "${FAKE_VERSION_TRAILER}"
            exit "${FAKE_VERSION_STATUS-0}" ;;
        --help)
            sed -n '/^Commands:/,/^$/p' "${FAKE_HELP_FILE}" || exit 1
            exit "${FAKE_HELP_STATUS-0}" ;;
        *codex-probe.js)
            printf '%s\n' "${FAKE_CODEX_LINE:-AGENT-VM-CODEX-REGISTERED models=3}" || exit 1
            exit "${FAKE_CODEX_STATUS-0}" ;;
        *probe.js)
            printf '%s\n' "${FAKE_BRIDGE_LINE:-AGENT-VM-BRIDGE-REGISTERED models=12}" || exit 1
            exit "${FAKE_BRIDGE_STATUS-0}" ;;
    esac
done
cat <<'OUT'
agent-vm: signing in here
Credentials imported from your host stay on the host and never appear in guest files.
OUT
exit "${FAKE_WARNING_STATUS-0}"
SH
    chmod 0755 "$1"
}

new_case() {
    unset SKIP_BRIDGE FAKE_VERSION FAKE_VERSION_TRAILER FAKE_VERSION_STATUS FAKE_HELP_STATUS \
        FAKE_WARNING_STATUS FAKE_CODEX_LINE FAKE_CODEX_STATUS FAKE_BRIDGE_LINE \
        FAKE_BRIDGE_STATUS FAKE_WRAPPER_LOG FAKE_HELP_FILE NODE_OPTIONS NODE_PATH BUN_OPTIONS \
        FAKE_RUNTIME_CHECK_FAIL TARGET_PLATFORM CASE_PATH_PREFIX CODEX_STATE_SEAM \
        INHERITED_FIXTURE_SRC
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/tmp" \
        "$CASE/root/pi" "$CASE/root/pnpm" "$CASE/root/install-status" \
        "$CASE/root/pi-extensions" "$CASE/root/seed.d" "$CASE/root/pi-gate" \
        "$CASE/root/pi-packages/node_modules/pi-claude-bridge/src" \
        "$CASE/root/pi-packages/node_modules/@anthropic-ai/claude-agent-sdk" \
        "$CASE/root/pi/node_modules/.bin"
    cp "$REPO_ROOT"/images/tools/pi/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} "$CASE/root/pi/"
    cp "$REPO_ROOT"/images/tools/pi/bridge/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} "$CASE/root/pi-packages/"
    cp "$REPO_ROOT/images/pnpm/pin.json" "$CASE/root/pnpm/pin.json"
    cp "$REPO_ROOT/images/tools/pi/dispatch/1.4.0.txt" "$CASE/root/pi/agent-vm-subcommands"
    : >"$CASE/root/pi-extensions/guest-credential-warning.js"
    : >"$CASE/root/pi-extensions/agent-vm-codex.js"
    cat >"$CASE/root/seed.d/20-pi-claude-bridge" <<'SH'
#!/bin/sh
# pathToClaudeCodeExecutable
CLAUDE=/opt/agent/.local/bin/claude
echo "$CLAUDE"
SH
    chmod 0755 "$CASE/root/seed.d/20-pi-claude-bridge"
    printf '#!/bin/sh\nexit 0\n' >"$CASE/root/pi/node_modules/.bin/pi"
    chmod 0755 "$CASE/root/pi/node_modules/.bin/pi"
    printf '{"name":"pi-claude-bridge","version":"0.8.0"}\n' \
        >"$CASE/root/pi-packages/node_modules/pi-claude-bridge/package.json"
    : >"$CASE/root/pi-packages/node_modules/pi-claude-bridge/src/index.ts"
    printf '{"name":"@anthropic-ai/claude-agent-sdk","version":"0.3.281"}\n' \
        >"$CASE/root/pi-packages/node_modules/@anthropic-ai/claude-agent-sdk/package.json"
    make_wrapper "$CASE/root/wrapper"
    make_runtime_checker "$CASE/root/runtime-check.mjs"
    cat >"$CASE/help.txt" <<'HELP'
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
    # Keep the fixture world-readable a+rX: the gate proves exactly that.
    chmod -R a+rX "$CASE/root"
    RUN_STATUS=0
    RUN_OUTPUT=""
    RUN_ERROR=""
}

# Two seams in the gate are environment-overridable so the black-box tests can point
# them at fixtures: AGENT_VM_PI_ENTRY (inside the shipped wrapper) and
# AGENT_VM_PI_RUNTIME_CHECK. Both were left to their defaults by the image's RUN
# lines, which made them inherited-environment holes -- an image ENV could have
# pointed the gate at a different binary or a different checker. The standard
# Dockerfile now pins both explicitly, so a build cannot inherit either, and this
# guard fails if a future edit drops a pin.
#
# Pinning two names was the narrow half of the same hole: the gate executes
# AGENT_VM_PI_WRAPPER, AGENT_VM_PI_GATE_SKIP_BRIDGE decides whether its bridge gates
# run at all, and pi.sh loads AGENT_VM_PI_BRIDGE_EXTENSION/AGENT_VM_PI_CODEX_EXTENSION,
# to name only the worst. Both RUN lines therefore remove the whole family first, and
# this guard owns three things about that statement: both windows carry it, nothing
# AGENT_VM_PI_* is assigned before it (it would be removed with the rest), and -- read
# out of the Dockerfile, not restated here -- it really removes the family in a shell
# while leaving everything else alone.
dockerfile="$REPO_ROOT/images/standard/Dockerfile"
pin_window() { # pin_window COMMAND -> the whole RUN instruction that calls COMMAND
    local first last
    # `|| true`: the diagnostic below is the report, not a silent exit under
    # `set -e` with `pipefail` when the command is gone.
    first=$(grep -n -F "$1" "$dockerfile" | head -n 1 | cut -d: -f1) || true
    [[ -n "$first" ]] || fail "the Dockerfile has no RUN line calling $1"
    # The instruction, not a fixed number of lines above the command: a pin or an
    # unset that falls one line outside a guessed window is exactly the hole this
    # guard exists to refuse.
    while [ "$first" -gt 1 ] && ! sed -n "$((first - 1))p" "$dockerfile" | grep -q '^RUN '; do
        first=$((first - 1))
    done
    last=$first
    while sed -n "${last}p" "$dockerfile" | grep -q '\\$'; do
        last=$((last + 1))
    done
    sed -n "${first},${last}p" "$dockerfile"
}
verify_window=$(pin_window 'sh /tmp/standard-tools/pi/verify-pi.sh')
for seam in AGENT_VM_PI_ENTRY= AGENT_VM_PI_RUNTIME_CHECK=; do
    [[ "$verify_window" == *"$seam"* ]] ||
        fail "the verify RUN line does not pin $seam, so the gate inherited it"
done
install_window=$(pin_window 'sh /tmp/recipe-contract/run-install.sh pi /opt/agent-vm/install-status/pi ')
[[ "$install_window" == *AGENT_VM_PI_RUNTIME_CHECK=* ]] ||
    fail 'the install RUN line does not pin AGENT_VM_PI_RUNTIME_CHECK=, so the installer inherited it'

pi_unset_line=$(grep -n '^ *unset \$(env ' "$dockerfile" | head -n 1 | cut -d: -f1) || true
[[ -n "$pi_unset_line" ]] ||
    fail 'the Dockerfile has no AGENT_VM_PI_* family unset, so both Pi RUN lines inherit it'
pi_unset=$(sed -n "${pi_unset_line}p" "$dockerfile" | sed -e 's/ *&& *\\$//' -e 's/^ *//')
assert_family_unset() { # assert_family_unset WINDOW
    local window=$1
    [[ "$window" == *"$pi_unset"* ]] ||
        fail 'a Pi RUN line does not unset the inherited AGENT_VM_PI_* family'
    [[ "${window%%"$pi_unset"*}" != *AGENT_VM_PI_* ]] ||
        fail 'a Pi RUN line assigns AGENT_VM_PI_* before the family unset, which removes it'
}
assert_family_unset "$verify_window"
assert_family_unset "$install_window"
# Executed, not matched: the statement has to remove every seam name in this `/bin/sh`
# and leave the rest of the environment to the scripts.
unset_witness=$(env -i PATH="$PATH" KEEP=1 \
    AGENT_VM_PI_WRAPPER=/evil/pi AGENT_VM_PI_GATE_SKIP_BRIDGE=1 \
    AGENT_VM_PI_BRIDGE_EXTENSION=/evil/bridge.js AGENT_VM_PI_CODEX_EXTENSION=/evil/codex.js \
    AGENT_VM_PI_SUBCOMMANDS=/evil/list AGENT_VM_PI_DISPATCH_DIR=/evil \
    AGENT_VM_PI_PREFIX=/evil AGENT_VM_PI_GATE_WORK_DIR=/evil \
    sh -c "$pi_unset; env" | LC_ALL=C sort)
leftover_seams=$(printf '%s\n' "$unset_witness" |
    sed -n 's/^\(AGENT_VM_PI_[0-9A-Za-z_]*\)=.*/\1/p' | tr '\n' ' ') || true
leftover_seams=${leftover_seams% }
[[ -z "$leftover_seams" ]] ||
    fail "the Dockerfile's family unset leaves $leftover_seams in the environment"
[[ "$unset_witness" == *KEEP=1* ]] || fail 'the Dockerfile family unset removes more than the AGENT_VM_PI_* family'

# The mounted runtime proof is a build-only helper that needs a cold N6 install, so
# the harness substitutes a recorder: what the gate owns is that it runs, that its
# status is checked before any output is parsed, and that its failure withholds the
# record.
make_runtime_checker() {
    local path=$1
    cat >"$path" <<JS
import fs from 'node:fs';
const args = process.argv.slice(2);
if (args.length !== 1) {
  process.stderr.write('fake runtime checker: usage: PREFIX\n');
  process.exit(2);
}
fs.appendFileSync('$CASE/runtime.log', 'runtime\n');
if (process.env.FAKE_RUNTIME_CHECK_FAIL === '1') {
  process.stderr.write('check-runtime.mjs: root-command: fixture refusal\n');
  process.exit(1);
}
process.stdout.write('pi runtime: {"fixture":true}\n');
JS
}

run_gate() {
    local -a env_args=(
        "PATH=${CASE_PATH_PREFIX-}$PATH"
        # The gate's own scratch is a plain `mktemp -d`; pin TMPDIR so a leaked
        # invocation scratch is visible inside the case.
        "TMPDIR=$CASE/tmp"
        "HOME=$CASE/root/pi-gate"
        "AGENT_VM_CONTRACT_DIR=$CONTRACT"
        "AGENT_VM_PI_SHARED=$PROJECT"
        "AGENT_VM_PI_PREFIX=$CASE/root/pi"
        "AGENT_VM_PI_WRAPPER=$CASE/root/wrapper"
        "AGENT_VM_PI_PACKAGES_PREFIX=$CASE/root/pi-packages"
        "AGENT_VM_PI_EXTENSION_DIR=$CASE/root/pi-extensions"
        "AGENT_VM_PI_SEED_HOOK=$CASE/root/seed.d/20-pi-claude-bridge"
        "AGENT_VM_PI_CODEX_STATE_DIR=${CODEX_STATE_SEAM-$CASE/root/codex-state}"
        # Empty unless a case injects one: the gate's cleanup must compare against the
        # inode it published, never against a path the caller named.
        "fixture_src=${INHERITED_FIXTURE_SRC-}"
        "AGENT_VM_PI_GATE_WORK_DIR=$CASE/root/pi-gate"
        "AGENT_VM_PI_SUBCOMMANDS=$CASE/root/pi/agent-vm-subcommands"
        "AGENT_VM_PI_DISPATCH_DIR=$REPO_ROOT/images/tools/pi/dispatch"
        "AGENT_VM_PI_RUNTIME_CHECK=$CASE/root/runtime-check.mjs"
        "AGENT_VM_PI_TARGET_PLATFORM=${TARGET_PLATFORM-linux/arm64}"
        "FAKE_RUNTIME_CHECK_FAIL=${FAKE_RUNTIME_CHECK_FAIL-}"
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/root/install-status"
        "AGENT_VM_FIXTURE_ROOT=$CASE/root"
        "FAKE_HELP_FILE=${FAKE_HELP_FILE-$CASE/help.txt}"
        "AGENT_VM_PI_GATE_SKIP_BRIDGE=${SKIP_BRIDGE-}"
        "FAKE_VERSION=${FAKE_VERSION-}"
        "FAKE_VERSION_TRAILER=${FAKE_VERSION_TRAILER-}"
        "FAKE_VERSION_STATUS=${FAKE_VERSION_STATUS-0}"
        "FAKE_HELP_STATUS=${FAKE_HELP_STATUS-0}"
        "FAKE_WARNING_STATUS=${FAKE_WARNING_STATUS-0}"
        "FAKE_CODEX_LINE=${FAKE_CODEX_LINE-}"
        "FAKE_CODEX_STATUS=${FAKE_CODEX_STATUS-0}"
        "FAKE_BRIDGE_LINE=${FAKE_BRIDGE_LINE-}"
        "FAKE_BRIDGE_STATUS=${FAKE_BRIDGE_STATUS-0}"
        "FAKE_WRAPPER_LOG=${FAKE_WRAPPER_LOG-}"
        # Empty unless a case injects one: the gate must refuse them before it
        # decides anything, including before the absence decision.
        "NODE_OPTIONS=${NODE_OPTIONS-}"
        "NODE_PATH=${NODE_PATH-}"
        "BUN_OPTIONS=${BUN_OPTIONS-}"
        "PRELOAD_WITNESS=$CASE/preload-witness"
    )
    set +e
    env -i "${env_args[@]}" sh "$GATE" >"$CASE/out" 2>"$CASE/err"
    RUN_STATUS=$?
    set -e
    RUN_OUTPUT="$(cat "$CASE/out")"
    RUN_ERROR="$(cat "$CASE/err")"
}

require_failure() { # require_failure DIAGNOSTIC
    [[ $RUN_STATUS -eq 1 ]] || fail "expected a hard failure, got $RUN_STATUS ($RUN_ERROR)"
    assert_contains "$RUN_ERROR" "$1"
}

# --- present and healthy -----------------------------------------------------
new_case healthy
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "the healthy gate failed: $RUN_ERROR"
assert_contains "$RUN_OUTPUT" 'subcommand list matches the installed CLI and the reviewed 1.4.0 expectation'
[[ "$(cat "$CASE/root/install-status/pi")" == installed ]] || fail 'no pi record'
[[ "$(cat "$CASE/root/install-status/pi-claude-bridge")" == installed ]] || fail 'no bridge record'
assert_contains "$RUN_OUTPUT" 'readable and executable by any uid'
assert_contains "$RUN_OUTPUT" 'installed runtime and N6 probes passed'
# The gate leaves no Codex fixture and no gate scratch behind.
assert_absent "$CASE/root/codex-state"
assert_absent "$CASE/root/pi-gate"

# N4: a Node preload or an injected module path would let the caller decide what
# every later probe reports, so the gate refuses it before any Node process. The
# preload records its own execution, so a gate that ran it cannot pass, and no
# record may be published from a run whose environment was rejected.
new_case runtime-injection
printf 'require("fs").appendFileSync(process.env.PRELOAD_WITNESS, "executed\\n");\n' \
    >"$CASE/preload.js"
for injection in NODE_OPTIONS NODE_PATH BUN_OPTIONS; do
    rm -f "$CASE/preload-witness"
    case "$injection" in
        NODE_OPTIONS) NODE_OPTIONS="--require=$CASE/preload.js" run_gate ;;
        NODE_PATH) NODE_PATH="$CASE" run_gate ;;
        BUN_OPTIONS) BUN_OPTIONS="--preload=$CASE/preload.js" run_gate ;;
    esac
    [[ $RUN_STATUS -eq 1 ]] || fail "$injection: expected a hard failure, got $RUN_STATUS"
    assert_contains "$RUN_ERROR" "$injection is not supported"
    assert_absent "$CASE/preload-witness"
    assert_absent "$CASE/root/install-status/pi"
    assert_absent "$CASE/root/install-status/pi-claude-bridge"
done

# The refusal precedes the presence decision: an absent-but-retirable Pi is not a
# reason to accept an injected environment either.
new_case runtime-injection-absent
printf 'require("fs").appendFileSync(process.env.PRELOAD_WITNESS, "executed\\n");\n' \
    >"$CASE/preload.js"
rm -rf "$CASE/root/pi"
printf 'absent-transport 56\n' >"$CASE/root/install-status/pi"
NODE_OPTIONS="--require=$CASE/preload.js" run_gate
require_failure 'NODE_OPTIONS is not supported'
assert_absent "$CASE/preload-witness"
[[ -f "$CASE/root/wrapper" ]] || fail 'the runtime was retired before the environment was rejected'
[[ -f "$CASE/root/seed.d/20-pi-claude-bridge" ]] || fail 'the seed hook was retired before rejection'

# The runtime identity proof gates publication: a tree whose root command is not the
# locked bin target (or whose N6 packages do not provide their shipped runtime) must
# publish neither record, even though every other gate passes.
new_case runtime-identity
FAKE_RUNTIME_CHECK_FAIL=1
# One log for both events, so the assertion is about order rather than presence: the
# checker records itself before it refuses, and every wrapper invocation records its
# arguments. A shim that rewrote `.bin/pi` on first run would pass a gate that proved
# the tree only after `--version`/`--help` had already executed it -- and the refusal
# has to stop every run, not just withhold the records, or the installed CLI still ran
# four times after its identity proof failed.
FAKE_WRAPPER_LOG="$CASE/runtime.log"
run_gate
require_failure 'the installed Pi runtime failed its identity and N6 probes'
[[ "$(tr '\n' '|' <"$CASE/runtime.log")" == 'runtime|' ]] ||
    fail "the installed CLI ran after its identity proof failed: $(tr '\n' '|' <"$CASE/runtime.log")"
assert_absent "$CASE/root/install-status/pi"
assert_absent "$CASE/root/install-status/pi-claude-bridge"

# The platform the proof runs against is an input, never the host: without it the
# gate fails instead of guessing.
new_case runtime-platform-missing
TARGET_PLATFORM=
run_gate
require_failure 'AGENT_VM_PI_TARGET_PLATFORM must name the image platform'
assert_absent "$CASE/root/install-status/pi"

# ... and every RUN in the image that reaches this gate -- directly, or through
# `verify-standard.sh` -- has to pass that input. The certification RUN shipped
# without it once, which fails the whole image at its last stage, so the
# Dockerfile is parsed here: a match in a comment cannot satisfy the check.
DOCKERFILE="$REPO_ROOT/images/standard/Dockerfile"
[[ -f "$DOCKERFILE" ]] || fail "the standard image Dockerfile is missing"

# Print every RUN paragraph that reaches the gate without naming the platform.
unplatformed_run() {
    awk '
        function check() {
            if (block ~ /sh \/tmp\/(standard-tools\/pi\/verify-pi|verify-standard)\.sh/ &&
                block !~ /AGENT_VM_PI_TARGET_PLATFORM=linux\//) {
                print block "---"
            }
        }
        /^RUN / { if (inrun) check(); block = $0 "\n"; inrun = 1; next }
        inrun { block = block $0 "\n"; if ($0 !~ /\\[[:space:]]*$/) { check(); inrun = 0 } }
        END { if (inrun) check() }
    ' "$1"
}
[[ -z "$(unplatformed_run "$DOCKERFILE")" ]] ||
    fail "a RUN that reaches verify-pi.sh does not name AGENT_VM_PI_TARGET_PLATFORM: $(unplatformed_run "$DOCKERFILE")"

# The guard has to be able to fail: the same parse over a copy with every platform
# line removed must name the certification RUN.
sabotaged=$(mktemp "${TMPDIR:-/tmp}/dockerfile.XXXXXX")
sed '/AGENT_VM_PI_TARGET_PLATFORM=linux\//d' "$DOCKERFILE" >"$sabotaged"
[[ "$(unplatformed_run "$sabotaged")" == *'verify-standard.sh'* ]] ||
    fail 'the Dockerfile platform check cannot detect a RUN that omits the platform'
rm -f "$sabotaged"

# A stale absence record cannot override a present command.
new_case present-with-stale-record
printf 'absent-transport 56\n' >"$CASE/root/install-status/pi"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "a stale record blocked a healthy Pi: $RUN_ERROR"
[[ "$(cat "$CASE/root/install-status/pi")" == installed ]] || fail 'the stale record survived'

# Pi-only gates: a healthy Pi is distinguishable from a bridge failure.
new_case pi-only
SKIP_BRIDGE=1
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "the Pi-only gate failed: $RUN_ERROR"
[[ "$(cat "$CASE/root/install-status/pi")" == installed ]] || fail 'Pi was not recorded'
assert_absent "$CASE/root/install-status/pi-claude-bridge"

# --- the single absence decision --------------------------------------------
new_case absent-genuine
rm -rf "$CASE/root/pi"
printf 'absent-transport 56\n' >"$CASE/root/install-status/pi"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "a genuine curl absence must be accepted: $RUN_ERROR"
assert_contains "$RUN_OUTPUT" 'ABSENT (absent-transport 56)'
[[ "$(cat "$CASE/root/install-status/pi")" == 'absent-transport 56' ]] ||
    fail 'the Pi curl record was not preserved'
[[ "$(cat "$CASE/root/install-status/pi-claude-bridge")" == 'absent-pi' ]] ||
    fail 'the derivative bridge record is missing'
assert_absent "$CASE/root/wrapper"
assert_absent "$CASE/root/seed.d/20-pi-claude-bridge"
assert_absent "$CASE/root/pi-extensions"
assert_absent "$CASE/root/pi-packages"

new_case absent-no-record
rm -rf "$CASE/root/pi"
run_gate
require_failure 'the Pi prefix is missing without a valid curl absence record'
# Nothing was retired: the wrapper and the seed hook are still there.
[[ -f "$CASE/root/wrapper" ]] || fail 'the wrapper was deleted without an absence record'

new_case absent-npm-record
rm -rf "$CASE/root/pi"
printf 'absent-transport EAI_AGAIN\n' >"$CASE/root/install-status/pi"
run_gate
require_failure 'the Pi prefix is missing without a valid curl absence record'

new_case absent-multiline-record
rm -rf "$CASE/root/pi"
printf 'absent-transport 56\nextra\n' >"$CASE/root/install-status/pi"
run_gate
require_failure 'the Pi prefix is missing without a valid curl absence record'

new_case absent-symlinked-record
rm -rf "$CASE/root/pi"
printf 'absent-transport 56\n' >"$CASE/absent-record"
ln -s "$CASE/absent-record" "$CASE/root/install-status/pi"
run_gate
require_failure 'the Pi prefix is missing without a valid curl absence record'

new_case surviving-prefix
rm "$CASE/root/pi/node_modules/.bin/pi"
printf 'absent-transport 56\n' >"$CASE/root/install-status/pi"
run_gate
require_failure 'the Pi prefix is not absent'
[[ -d "$CASE/root/pi" ]] || fail 'a surviving prefix was deleted'
[[ -f "$CASE/root/wrapper" ]] || fail 'the wrapper was deleted for a surviving prefix'

new_case absent-symlinked-root
rm -rf "$CASE/root/pi"
mkdir -p "$CASE/sentinel"
ln -s "$CASE/sentinel" "$CASE/root/pi-extensions-link"
mv "$CASE/root/pi-extensions" "$CASE/root/pi-extensions-real"
ln -s "$CASE/sentinel" "$CASE/root/pi-extensions"
printf 'absent-transport 56\n' >"$CASE/root/install-status/pi"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'a symlinked retirement root must be refused'
[[ -d "$CASE/sentinel" ]] || fail 'a sentinel outside the feature was reached'

# --- Pi gates ----------------------------------------------------------------
new_case version-mismatch
FAKE_VERSION=1.3.9
run_gate
require_failure "the wrapper --version reported '1.3.9'"
assert_absent "$CASE/root/install-status/pi"

new_case version-trailer
FAKE_VERSION_TRAILER='secret extra line'
run_gate
require_failure 'unexpected --version output on line 2'

new_case version-nonzero
FAKE_VERSION_STATUS=1
run_gate
require_failure '--version exited 1'

new_case dispatch-missing
rm "$CASE/root/pi/agent-vm-subcommands"
run_gate
require_failure 'the image-owned subcommand list is missing or unreadable'

new_case dispatch-not-reviewed
cp "$REPO_ROOT/images/tools/pi/dispatch/0.87.0.txt" "$CASE/root/pi/agent-vm-subcommands"
run_gate
require_failure 'is not the reviewed 1.4.0 list'

new_case dispatch-help-drift
sed -i.bak '/pi mcp /d' "$CASE/help.txt"
rm -f "$CASE/help.txt.bak"
run_gate
require_failure 'the installed CLI advertises'

new_case help-nonzero
FAKE_HELP_STATUS=3
run_gate
require_failure '--help exited 3'

new_case mandatory-extension-missing
rm "$CASE/root/pi-extensions/guest-credential-warning.js"
run_gate
require_failure 'the mandatory extension is missing'

new_case warning-missing
FAKE_WARNING_STATUS=0
# A wrapper whose real invocation does not print the warning.
grep -v 'agent-vm: signing in here' "$CASE/root/wrapper" >"$CASE/root/wrapper.nowarn"
mv "$CASE/root/wrapper.nowarn" "$CASE/root/wrapper"
chmod 0755 "$CASE/root/wrapper"
run_gate
require_failure 'does not load the mandatory extension'

new_case codex-empty-catalog
FAKE_CODEX_LINE='AGENT-VM-CODEX-REGISTERED models=0'
run_gate
require_failure 'agent-vm-codex registered but its model catalog is empty'

new_case codex-duplicate
FAKE_CODEX_LINE='AGENT-VM-CODEX-REGISTERED models=3
AGENT-VM-CODEX-MISSING'
run_gate
require_failure 'agent-vm-codex emitted 2 conflicting registration results'

# --- Codex fixture publication (adversarial) --------------------------------
# The gate plants a placeholder credential in `$AGENT_VM_PI_CODEX_STATE_DIR` so the
# image-owned extension registers during a real invocation. That write is the one
# destructive seam in the gate, so the controls below are the ones that make it
# safe: it never overwrites existing state, never writes through a symlink, gives
# the directories back on every exit path, and never trades an exit status for a
# cleanup result.

assert_no_record() {
    assert_absent "$CASE/root/install-status/pi"
    assert_absent "$CASE/root/install-status/pi-claude-bridge"
}

assert_scratch_removed() {
    [[ -z "$(ls -A "$CASE/tmp" 2>/dev/null)" ]] ||
        fail "the gate leaked invocation scratch: $(ls -A "$CASE/tmp")"
    assert_absent "$CASE/root/pi-gate"
}

# Existing state is never overwritten: a file already at auth.json is a hard
# failure, the bytes survive byte-for-byte, and the ancestor directory the gate did
# not create is left in place.
new_case codex-preexisting-auth
mkdir -p "$CASE/root/codex-alt/codex-state"
printf 'keep me\n' >"$CASE/root/codex-alt/codex-state/auth.json"
cp "$CASE/root/codex-alt/codex-state/auth.json" "$CASE/keep-expected"
CODEX_STATE_SEAM=$CASE/root/codex-alt/codex-state
run_gate
require_failure "refusing to overwrite existing $CASE/root/codex-alt/codex-state/auth.json"
assert_no_record
cmp -s "$CASE/keep-expected" "$CASE/root/codex-alt/codex-state/auth.json" ||
    fail 'the gate changed existing Codex state'
[[ -d "$CASE/root/codex-alt" ]] || fail 'the gate removed a directory it did not create'
assert_scratch_removed

# Ownership flags precede creation, so an earlier failure cannot let the EXIT trap
# delete state the gate never made.
new_case codex-preexisting-auth-early-failure
mkdir -p "$CASE/root/codex-state"
printf 'keep me\n' >"$CASE/root/codex-state/auth.json"
cp "$CASE/root/codex-state/auth.json" "$CASE/keep-expected"
FAKE_VERSION=1.3.9
run_gate
require_failure 'the wrapper --version reported'

# The ownership test is inode identity with the file the gate published, so it cannot
# be decided by a variable: an inherited `fixture_src` naming the credential the gate
# refused to overwrite made this failure path delete it.
new_case codex-inherited-fixture-src
mkdir -p "$CASE/root/codex-alt/codex-state"
printf 'precious\n' >"$CASE/root/codex-alt/codex-state/auth.json"
cp "$CASE/root/codex-alt/codex-state/auth.json" "$CASE/keep-expected"
CODEX_STATE_SEAM=$CASE/root/codex-alt/codex-state
INHERITED_FIXTURE_SRC=$CASE/root/codex-alt/codex-state/auth.json
run_gate
require_failure "refusing to overwrite existing $CASE/root/codex-alt/codex-state/auth.json"
assert_no_record
cmp -s "$CASE/keep-expected" "$CASE/root/codex-alt/codex-state/auth.json" ||
    fail 'an inherited fixture_src made the gate delete the credential it refused to overwrite'

# A pre-existing directory without a credential is reused and restored, not removed.
new_case codex-preexisting-dir
mkdir -p "$CASE/root/codex-state"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "a pre-existing state directory failed the gate: $RUN_ERROR"
[[ -d "$CASE/root/codex-state" ]] || fail 'the gate removed a pre-existing state directory'
assert_absent "$CASE/root/codex-state/auth.json"
assert_scratch_removed

# A symlinked ancestor would publish the placeholder outside the state tree, so it
# is refused before anything is created and nothing is written through the link.
new_case codex-symlinked-ancestry
mkdir -p "$CASE/target"
ln -s "$CASE/target" "$CASE/root/codex-link"
CODEX_STATE_SEAM=$CASE/root/codex-link/codex-state
run_gate
require_failure 'refusing symlinked Codex fixture ancestry'
assert_no_record
assert_absent "$CASE/target/codex-state"
assert_scratch_removed

# The state directory itself being a link is the same escape by one hop.
new_case codex-symlinked-state-dir
mkdir -p "$CASE/target"
ln -s "$CASE/target" "$CASE/root/codex-state"
run_gate
require_failure 'refusing symlinked Codex fixture ancestry'
assert_no_record
assert_absent "$CASE/target/auth.json"
assert_scratch_removed

# A symlinked credential file is a link even though `-e` follows it: refuse it and
# leave the target alone.
new_case codex-symlinked-auth-file
mkdir -p "$CASE/root/codex-state"
printf 'keep me\n' >"$CASE/target-auth.json"
ln -s "$CASE/target-auth.json" "$CASE/root/codex-state/auth.json"
run_gate
require_failure 'refusing to overwrite existing'
assert_no_record
[[ "$(cat "$CASE/target-auth.json")" == 'keep me' ]] || fail 'the gate wrote through a credential symlink'
assert_scratch_removed

# A *dangling* symlink is the case `-e` cannot see: `[ -e ]` follows the link, so the
# guard that refuses an existing credential needs its own `-L` half. Without it the
# gate reaches `ln`, fails with the publisher's diagnostic instead of this refusal,
# and the link it never created is treated as its own to remove.
new_case codex-dangling-auth-file
mkdir -p "$CASE/root/codex-state"
ln -s "$CASE/root/nowhere" "$CASE/root/codex-state/auth.json"
run_gate
require_failure 'refusing to overwrite existing'
assert_no_record
[[ -L "$CASE/root/codex-state/auth.json" ]] || fail 'the gate removed a credential link it did not create'
assert_absent "$CASE/root/nowhere"
assert_scratch_removed

# Directories the gate had to create are given back at exit, all the way up.
new_case codex-parent-restored
CODEX_STATE_SEAM=$CASE/root/codex-new/codex-state
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "a two-level state seam failed the healthy gate: $RUN_ERROR"
[[ -d "$CASE/root" ]] || fail 'the gate removed the root it did not create'
assert_absent "$CASE/root/codex-new"
assert_absent "$CASE/root/codex-state"
assert_scratch_removed

# A non-directory in the way is refused without clobbering it.
new_case codex-state-is-a-file
printf 'keep me\n' >"$CASE/root/codex-state"
run_gate
require_failure 'cannot plant the Codex fixture credential'
assert_no_record
[[ "$(cat "$CASE/root/codex-state")" == 'keep me' ]] || fail 'the gate clobbered a file at the state path'
assert_scratch_removed

# Publication is signal-safe: a TERM that lands while the exclusive link is being
# made must not leave the placeholder behind, must not certify anything, and must
# keep its own status. Both timings are covered because a successful `ln` and a
# killed parent are independent events, and a third case covers the fact that the
# cleanup's ownership test is inode identity: a credential another writer replaced
# after publication is not the gate's to delete.
assert_interrupted() { # assert_interrupted LABEL
    [[ $RUN_STATUS -eq 143 ]] || fail "$1: an interrupted gate exited $RUN_STATUS, not 143"
    for record in pi pi-claude-bridge; do
        assert_absent "$CASE/root/install-status/$record"
    done
    [[ -z "$(ls -A "$CASE/tmp" 2>/dev/null)" ]] ||
        fail "$1: leaked invocation scratch $(ls -A "$CASE/tmp")"
}

new_case codex-term-before-publication
mkdir "$CASE/bin"
printf '#!/bin/sh\nkill -TERM "$PPID"\nexec /bin/ln "$@"\n' >"$CASE/bin/ln"
chmod 0755 "$CASE/bin/ln"
CASE_PATH_PREFIX="$CASE/bin:"
run_gate
assert_interrupted 'TERM before publication'
assert_absent "$CASE/root/codex-state/auth.json"

new_case codex-term-after-publication
mkdir "$CASE/bin"
printf '#!/bin/sh\n/bin/ln "$@" || exit $?\nkill -TERM "$PPID"\n' >"$CASE/bin/ln"
chmod 0755 "$CASE/bin/ln"
CASE_PATH_PREFIX="$CASE/bin:"
run_gate
assert_interrupted 'TERM after publication'
assert_absent "$CASE/root/codex-state/auth.json"

new_case codex-term-after-replacement
mkdir "$CASE/bin"
# The shim publishes, a competing writer replaces the credential with its own
# inode (unlink + create, not a truncating write), and only then does the TERM
# arrive: `-ef` is what tells the two apart.
printf '#!/bin/sh\n/bin/ln "$@" || exit $?\nrm -f "$2"\nprintf "someone else\\n" >"$2"\nkill -TERM "$PPID"\n' \
    >"$CASE/bin/ln"
chmod 0755 "$CASE/bin/ln"
CASE_PATH_PREFIX="$CASE/bin:"
run_gate
assert_interrupted 'TERM after a competing write'
[[ "$(cat "$CASE/root/codex-state/auth.json" 2>/dev/null)" == 'someone else' ]] ||
    fail 'the gate deleted a credential that was not the one it published'

# The same competing write, but *after* publication: the gate has already published
# the link and moved on to the wrapper invocation when the credential is replaced, so
# it is the ownership test -- not the timing of the signal -- that decides. The
# replaced inode is preserved (it is not the gate's to delete) and the gate refuses to
# certify a state directory it cannot vouch for.
new_case codex-replacement-after-publication
mkdir -p "$CASE/root/codex-state"
mv "$CASE/root/wrapper" "$CASE/root/wrapper.real"
# After publication, exactly once: the wrapper runs before the fixture is planted too,
# so the shim waits for the credential to exist; and a shim that replaced it on every
# invocation would put it back after the cleanup, hiding whether the cleanup took it.
cat >"$CASE/root/wrapper" <<SH
#!/bin/sh
if [ ! -e "$CASE/replaced" ] && [ -e "$CASE/root/codex-state/auth.json" ]; then
    rm -f "$CASE/root/codex-state/auth.json"
    printf 'someone else\\n' >"$CASE/root/codex-state/auth.json"
    : >"$CASE/replaced"
fi
exec "$CASE/root/wrapper.real" "\$@"
SH
chmod 0755 "$CASE/root/wrapper"
run_gate
require_failure 'a credential replaced the agent-vm-codex fixture during the gate'
assert_no_record
[[ "$(cat "$CASE/root/codex-state/auth.json" 2>/dev/null)" == 'someone else' ]] ||
    fail 'the gate deleted a credential that was not the one it published'
assert_scratch_removed

# A failed publication keeps the publisher's own status.
new_case codex-publication-fails
mkdir "$CASE/bin"
printf '#!/bin/sh\nexit 37\n' >"$CASE/bin/ln"
chmod 0755 "$CASE/bin/ln"
CASE_PATH_PREFIX="$CASE/bin:"
run_gate
require_failure 'cannot plant the Codex fixture credential'
assert_no_record
assert_scratch_removed

# A cleanup failure is a gate failure with its own diagnostic, and it does not
# replace the status the gate already had.
new_case codex-cleanup-failure
mkdir "$CASE/bin"
cat >"$CASE/bin/rm" <<'SH'
#!/bin/sh
case "$*" in *codex-state/auth.json*) exit 37 ;; esac
exec /bin/rm "$@"
SH
chmod 0755 "$CASE/bin/rm"
CASE_PATH_PREFIX="$CASE/bin:"
run_gate
require_failure 'the agent-vm-codex fixture could not be removed'
assert_contains "$RUN_ERROR" 'the agent-vm-codex fixture survived the gate'
assert_no_record

new_case codex-cleanup-failure-preserves-status
mkdir "$CASE/bin"
cat >"$CASE/bin/rm" <<'SH'
#!/bin/sh
case "$*" in *codex-state/auth.json*) exit 37 ;; esac
exec /bin/rm "$@"
SH
chmod 0755 "$CASE/bin/rm"
CASE_PATH_PREFIX="$CASE/bin:"
FAKE_CODEX_STATUS=1
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail "cleanup replaced the probe failure status: $RUN_STATUS"
assert_contains "$RUN_ERROR" 'the agent-vm-codex probe exited 1'
assert_contains "$RUN_ERROR" 'the agent-vm-codex fixture could not be removed'
assert_no_record

# The seam has a production default, so a build that forgets to set it plants its
# placeholder next to the guest's real state directory rather than in the harness.
# shellcheck disable=SC2016  # single quotes keep the literal grep pattern
grep -Fq 'AGENT_VM_PI_CODEX_STATE_DIR:-/agent-vm-state/codex}' "$GATE" ||
    fail 'the production default Codex state path is missing'

# The extension the placeholder makes readable is node code with a bounded reader;
# its short-read, growth and trailing-garbage behaviour is proved by executing the
# committed factory, not by inspecting it here.
command -v node >/dev/null 2>&1 || fail 'node is required'
node "$REPO_ROOT/script/test/pi-codex-read.mjs"

# --- bridge gates ------------------------------------------------------------
new_case bridge-missing
rm -rf "$CASE/root/pi-packages/node_modules/pi-claude-bridge"
run_gate
require_failure 'no bridge absence producer'
# The slots are published independently: a healthy Pi is still recorded.
[[ "$(cat "$CASE/root/install-status/pi")" == installed ]] ||
    fail 'a missing bridge erased a healthy Pi record'
assert_absent "$CASE/root/install-status/pi-claude-bridge"

new_case bridge-absent-with-derivative-record
rm -rf "$CASE/root/pi-packages/node_modules/pi-claude-bridge"
printf 'absent-pi\n' >"$CASE/root/install-status/pi-claude-bridge"
run_gate
require_failure 'no bridge absence producer'

new_case bridge-absent-with-curl-record
rm -rf "$CASE/root/pi-packages/node_modules/pi-claude-bridge"
printf 'absent-transport 75\n' >"$CASE/root/install-status/pi-claude-bridge"
run_gate
require_failure 'no bridge absence producer'

new_case bridge-version-mismatch
printf '{"name":"pi-claude-bridge","version":"0.9.0"}\n' \
    >"$CASE/root/pi-packages/node_modules/pi-claude-bridge/package.json"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'a bridge version mismatch must be hard'
assert_contains "$RUN_ERROR" "pi-claude-bridge installed '0.9.0' but the manifest pins '0.8.0'"
# The bridge slot is not published, but the healthy Pi slot is: separate records.
[[ "$(cat "$CASE/root/install-status/pi")" == installed ]] ||
    fail 'a bridge-only failure must still record a healthy Pi'
assert_absent "$CASE/root/install-status/pi-claude-bridge"

new_case bridge-sdk-missing
rm "$CASE/root/pi-packages/node_modules/@anthropic-ai/claude-agent-sdk/package.json"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'a missing SDK JavaScript must be hard'
assert_contains "$RUN_ERROR" 'the Claude Agent SDK JavaScript is missing'

new_case bridge-platform-native
mkdir -p "$CASE/root/pi-packages/node_modules/@anthropic-ai/claude-agent-sdk-linux-arm64"
chmod -R a+rX "$CASE/root/pi-packages"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'an SDK platform payload must be hard'
assert_contains "$RUN_ERROR" 'ships the omitted SDK platform payload'

new_case bridge-second-pi
mkdir -p "$CASE/root/pi-packages/node_modules/@earendil-works"
chmod -R a+rX "$CASE/root/pi-packages"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'a second Pi under the bridge must be hard'
assert_contains "$RUN_ERROR" 'installs @earendil-works'

new_case bridge-metadata-only-store
# pnpm 11.11.0 keeps `node_modules/.pnpm/lock.yaml` in a hoisted install (the
# focused harness observed it on the real payload), so a store *file* is not an
# isolated tree. Only private-store package payload is.
mkdir -p "$CASE/root/pi-packages/node_modules/.pnpm"
printf 'lockfileVersion: 9.0\n' >"$CASE/root/pi-packages/node_modules/.pnpm/lock.yaml"
chmod -R a+rX "$CASE/root/pi-packages"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail 'a hoisted tree with a metadata-only store must pass'
assert_contains "$RUN_OUTPUT" 'installed'

new_case bridge-isolated-layout
mkdir -p "$CASE/root/pi-packages/node_modules/.pnpm/x/node_modules"
chmod -R a+rX "$CASE/root/pi-packages"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'an isolated bridge tree must be hard'
assert_contains "$RUN_ERROR" 'private-store package payload'

new_case bridge-print-marker-then-nonzero
FAKE_BRIDGE_STATUS=5
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'print-marker-then-nonzero must be hard'
assert_contains "$RUN_ERROR" 'the bridge probe exited 5; refusing to scan its output'
[[ "$(cat "$CASE/root/install-status/pi")" == installed ]] ||
    fail 'a bridge failure must not erase a healthy Pi record'
assert_absent "$CASE/root/install-status/pi-claude-bridge"

new_case bridge-empty-catalog
FAKE_BRIDGE_LINE='AGENT-VM-BRIDGE-REGISTERED models=00'
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'an empty model catalog must be hard'
assert_contains "$RUN_ERROR" 'pi-claude-bridge registered but its model catalog is empty'

new_case bridge-malformed-notification
FAKE_BRIDGE_LINE='{"message": "AGENT-VM-BRIDGE-MISSING", "truncated":'
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'a truncated registration notification must be hard'
assert_contains "$RUN_ERROR" 'malformed registration line'

# --- content and seed gates -------------------------------------------------
new_case content-only-failure
chmod 0600 "$CASE/root/pi-extensions/agent-vm-codex.js"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'unreadable required content must be hard'
assert_absent "$CASE/root/install-status/pi"

# The seed hook seeds the *bridge*, so its failures withhold the bridge record only:
# they used to count as Pi gates, which threw away a Pi that had passed every Pi
# proof. Each case below therefore asserts both halves.
require_bridge_only_failure() {
    [[ $RUN_STATUS -eq 1 ]] || fail "a bridge-only gate must still fail the layer (got $RUN_STATUS)"
    [[ "$(cat "$CASE/root/install-status/pi" 2>/dev/null)" == installed ]] ||
        fail 'a bridge-only failure cost the independently installed Pi record'
    assert_absent "$CASE/root/install-status/pi-claude-bridge"
    assert_contains "$RUN_ERROR" 'bridge gate(s) failed; only Pi is recorded'
}

new_case seed-missing
rm "$CASE/root/seed.d/20-pi-claude-bridge"
run_gate
assert_contains "$RUN_ERROR" 'the bridge seed hook is missing or not executable'
require_bridge_only_failure

new_case seed-not-executable
chmod 0644 "$CASE/root/seed.d/20-pi-claude-bridge"
run_gate
assert_contains "$RUN_ERROR" 'the bridge seed hook is missing or not executable'
require_bridge_only_failure

new_case seed-content-drift
sed -i.bak 's/pathToClaudeCodeExecutable/anotherOption/' "$CASE/root/seed.d/20-pi-claude-bridge"
rm -f "$CASE/root/seed.d/20-pi-claude-bridge.bak"
run_gate
assert_contains "$RUN_ERROR" 'no longer seeds the SDK executable path'
require_bridge_only_failure

new_case private-payload
chmod 0700 "$CASE/root/pi/node_modules/.bin/pi"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'a payload entry a guest cannot read must be hard'

# The command the gate is about to run must be the payload's own: `authorize-runtime`
# walks the command's link chain and refuses a waypoint that leaves the prefix, so a
# seam pointing the entry point through an alternative directory cannot be probed or
# certified. A link that stays inside the payload is real pnpm output and passes, so
# the positive control comes first: a refusal of every symlinked command fails here
# while a missing waypoint check does not.
new_case command-through-payload-link
mkdir -p "$CASE/root/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle"
printf '#!/bin/sh\nexit 0\n' >"$CASE/root/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"
chmod 0755 "$CASE/root/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"
rm "$CASE/root/pi/node_modules/.bin/pi"
ln -s ../@earendil-works/pi-coding-agent/dist/bundle/cli.js \
    "$CASE/root/pi/node_modules/.bin/pi"
run_gate
[[ $RUN_STATUS -eq 0 ]] || fail "a command link inside the payload is real pnpm output: $RUN_ERROR"
[[ "$(cat "$CASE/root/install-status/pi")" == installed ]] || fail 'no pi record'

new_case command-escapes-payload
mkdir -p "$CASE/root/alternative"
mkdir -p "$CASE/root/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle"
printf '#!/bin/sh\nexit 0\n' >"$CASE/root/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"
chmod 0755 "$CASE/root/pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js"
rm "$CASE/root/pi/node_modules/.bin/pi"
# The final target is inside the payload; only the walk passes through
# `root/alternative`, which is what the waypoint check exists to refuse.
ln -s ../../../alternative/../pi/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js \
    "$CASE/root/pi/node_modules/.bin/pi"
run_gate
[[ $RUN_STATUS -eq 1 ]] || fail 'a command link whose walk leaves the payload must be hard'
assert_contains "$RUN_ERROR" 'waypoint escapes payload'
assert_contains "$RUN_ERROR" 'the installed Pi payload failed runtime authorization'
assert_no_record

echo 'pi-verify contract tests passed'
