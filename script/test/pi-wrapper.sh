#!/usr/bin/env bash
# Black-box contract tests for the stable `pi` wrapper (images/tools/pi/pi.sh).
#
# No Docker and no real Pi: a fake entry point on AGENT_VM_PI_ENTRY records its
# argv (one <token> per argument), its environment, and its stdin, so the
# wrapper's decisions -- the PI_SKIP_VERSION_CHECK enforcement, subcommand
# dispatch, "inject the mandatory --extension on the non-subcommand path", and
# "inject the image-owned bridge --extension only when it exists and is not
# opted out", and existence-checked Codex activation -- are pinned without a
# 150 MiB install or a microVM.
#
# agent-vm only intervenes where agent-vm introduced the condition, so the
# wrapper injects NO --approve default and NO PI_TELEMETRY default: those, and
# --no-approve and --extension, are forwarded untouched. The cases below pin
# that forwarding exactly.

set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
WRAPPER="$REPO_ROOT/images/tools/pi/pi.sh"
MANDATORY_EXTENSION=/opt/agent-vm/pi-extensions/guest-credential-warning.js
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/pi-wrapper-test.XXXXXX")"
trap 'rm -rf "$TEST_ROOT"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

CASE=""
CASE_EXTRA_ENV=""
CASE_BRIDGE_EXTENSION=""

new_case() {
    CASE="$TEST_ROOT/$1"
    CASE_EXTRA_ENV=""
    CASE_SUBCOMMANDS="$CASE/no-such-subcommands"
    CASE_CODEX_EXTENSION="$CASE/no-such-codex.js"
    # Default fixture: the bridge extension does not exist, so the wrapper must
    # skip it. The bridge cases below plant a readable file via
    # CASE_BRIDGE_EXTENSION and repoint this.
    CASE_BRIDGE_EXTENSION="$CASE/no-such-bridge.ts"
    mkdir -p "$CASE"
    : >"$CASE/log"
    : >"$CASE/stdin"
    : >"$CASE/env"
    FAKE_EXIT_STATUS=""
    cat >"$CASE/fake-pi" <<'SH'
#!/usr/bin/env bash
{
    printf 'pi'
    printf ' <%s>' "$@"
    printf '\n'
} >>"$FAKE_LOG"
printf 'PI_SKIP_VERSION_CHECK=%s\nPI_TELEMETRY=%s\n' \
    "${PI_SKIP_VERSION_CHECK-<unset>}" "${PI_TELEMETRY-<unset>}" >"$CASE_ENV"
cat >>"$CASE_STDIN"
if [[ -n "${FAKE_EXIT_STATUS:-}" ]]; then
    exit "$FAKE_EXIT_STATUS"
fi
SH
    chmod +x "$CASE/fake-pi"
}

# Run the wrapper through `sh` (it is committed 0644; the execute bit comes from
# the layer's `COPY --chmod=0755`, which this host test deliberately does not
# reproduce), with a clean environment, and capture its status.
run_wrapper() {
    local -a env_args=(
        "PATH=/usr/bin:/bin"
        "AGENT_VM_PI_ENTRY=$CASE/fake-pi"
        "AGENT_VM_PI_BRIDGE_EXTENSION=$CASE_BRIDGE_EXTENSION"
        "AGENT_VM_PI_CODEX_EXTENSION=$CASE_CODEX_EXTENSION"
        "AGENT_VM_PI_SUBCOMMANDS=$CASE_SUBCOMMANDS"
        "FAKE_LOG=$CASE/log"
        "CASE_STDIN=$CASE/stdin"
        "CASE_ENV=$CASE/env"
        "FAKE_EXIT_STATUS=${FAKE_EXIT_STATUS-}"
    )
    if [[ -n "$CASE_EXTRA_ENV" ]]; then
        env_args+=("$CASE_EXTRA_ENV")
    fi
    set +e
    env -i "${env_args[@]}" sh "$WRAPPER" "$@" </dev/null 2>"$CASE/stderr"
    RUN_STATUS=$?
    set -e
}

# The exact single log line the wrapper's argv must produce.
assert_argv_is() {
    local expected="pi"
    local token
    for token in "$@"; do
        expected+=" <$token>"
    done
    local actual
    actual="$(cat "$CASE/log")"
    [[ "$actual" == "$expected" ]] \
        || fail "argv mismatch: expected [$expected] got [$actual]"
}

assert_env_line() {
    grep -Fqx "$1" "$CASE/env" \
        || fail "expected env line [$1]; got [$(tr '\n' ' ' <"$CASE/env")]"
}

# --- the extension is injected, and only when it should be -------------------

new_case bare
run_wrapper
[[ $RUN_STATUS -eq 0 ]] || fail "bare invocation failed: $RUN_STATUS"
# The wrapper injects only the mandatory extension: no --approve, no telemetry.
assert_argv_is --extension "$MANDATORY_EXTENSION"
assert_env_line 'PI_SKIP_VERSION_CHECK=1'
# Pi's telemetry policy is Pi's own, so an unset PI_TELEMETRY stays unset.
assert_env_line 'PI_TELEMETRY=<unset>'

new_case prompt
run_wrapper "fix the tests"
assert_argv_is --extension "$MANDATORY_EXTENSION" "fix the tests"

new_case flags-preserved
run_wrapper -ne -p msg
assert_argv_is --extension "$MANDATORY_EXTENSION" -ne -p msg

# A subcommand is only a subcommand as argv[1]; forwarded verbatim (no -e).
for subcommand in auth config install list remove uninstall update; do
    new_case "subcommand-$subcommand"
    run_wrapper "$subcommand"
    assert_argv_is "$subcommand"
done

new_case subcommand-args-verbatim
run_wrapper auth check --provider x
assert_argv_is auth check --provider x

# The env enforcement is exported before the dispatch, so a subcommand
# invocation gets it too (and still no --extension / --approve).
new_case subcommand-env
run_wrapper list
assert_argv_is list
assert_env_line 'PI_SKIP_VERSION_CHECK=1'
assert_env_line 'PI_TELEMETRY=<unset>'

# The other side of the boundary: the word is a prompt/argument, not a
# subcommand, so the extension IS injected.
new_case leading-flag-then-subcommand-word
run_wrapper -p list
assert_argv_is --extension "$MANDATORY_EXTENSION" -p list

new_case subcommand-word-as-prompt
run_wrapper "list the files"
assert_argv_is --extension "$MANDATORY_EXTENSION" "list the files"

# Exact match, not a prefix match.
new_case lookalike-listen
run_wrapper listen
assert_argv_is --extension "$MANDATORY_EXTENSION" listen

new_case lookalike-auth-x
run_wrapper auth-x
assert_argv_is --extension "$MANDATORY_EXTENSION" auth-x

# --- the image-owned bridge extension: existence-checked, and opt-out --------
#
# The mandatory extension is injected unconditionally (Pi fails closed on it);
# the bridge is a convenience, so an absent one must cost the bridge, not pi.
# Both states are explicit fixtures rather than relying on the host's
# /opt/agent-vm/pi-packages being absent.

new_case bridge-absent
run_wrapper
[[ -e "$CASE_BRIDGE_EXTENSION" ]] && fail "the bridge-absent fixture must not exist"
assert_argv_is --extension "$MANDATORY_EXTENSION"

new_case bridge-present
CASE_BRIDGE_EXTENSION="$CASE/fake-bridge.ts"
: >"$CASE_BRIDGE_EXTENSION"
run_wrapper
# Order matters: the mandatory extension first, then the bridge.
assert_argv_is --extension "$MANDATORY_EXTENSION" --extension "$CASE_BRIDGE_EXTENSION"

new_case bridge-present-prompt
CASE_BRIDGE_EXTENSION="$CASE/fake-bridge.ts"
: >"$CASE_BRIDGE_EXTENSION"
run_wrapper "fix the tests"
assert_argv_is --extension "$MANDATORY_EXTENSION" --extension "$CASE_BRIDGE_EXTENSION" "fix the tests"

# A subcommand is forwarded verbatim: the bridge flag must NOT be injected on
# that path either (it would displace argv[1]).
new_case bridge-present-subcommand
CASE_BRIDGE_EXTENSION="$CASE/fake-bridge.ts"
: >"$CASE_BRIDGE_EXTENSION"
run_wrapper install foo
assert_argv_is install foo

# The opt-out (ADR-0023): a bridge that throws, or a user-installed second copy,
# must be escapable without knowing the internal entry point.
new_case bridge-opt-out
CASE_BRIDGE_EXTENSION="$CASE/fake-bridge.ts"
: >"$CASE_BRIDGE_EXTENSION"
CASE_EXTRA_ENV="AGENT_VM_PI_NO_BRIDGE=1"
run_wrapper
assert_argv_is --extension "$MANDATORY_EXTENSION"

# Any non-empty value opts out; an empty one is not an opt-out.
new_case bridge-opt-out-empty-is-not-an-opt-out
CASE_BRIDGE_EXTENSION="$CASE/fake-bridge.ts"
: >"$CASE_BRIDGE_EXTENSION"
CASE_EXTRA_ENV="AGENT_VM_PI_NO_BRIDGE="
run_wrapper
assert_argv_is --extension "$MANDATORY_EXTENSION" --extension "$CASE_BRIDGE_EXTENSION"

# A subcommand still bypasses the opt-out entirely (its own forward-verbatim
# path runs first, so neither extension flag is added).
new_case bridge-opt-out-subcommand
CASE_EXTRA_ENV="AGENT_VM_PI_NO_BRIDGE=1"
run_wrapper list
assert_argv_is list

# --- argv fidelity: an unquoted `$@` would break every one of these ----------

new_case argv-fidelity
run_wrapper -- "-a b" "*" ""
assert_argv_is --extension "$MANDATORY_EXTENSION" -- "-a b" "*" ""

# --- no trust default: an explicit approve flag is forwarded verbatim --------

# The wrapper injects no --approve/--no-approve of its own, so an explicit one
# reaches Pi exactly once -- never duplicated, never dropped. `assert_argv_is`
# is an exact whole-line match, so a duplicate would fail here too.
new_case approve-explicit
run_wrapper --approve
assert_argv_is --extension "$MANDATORY_EXTENSION" --approve

new_case no-approve-explicit
run_wrapper --no-approve
assert_argv_is --extension "$MANDATORY_EXTENSION" --no-approve

new_case approve-short-explicit
run_wrapper -a
assert_argv_is --extension "$MANDATORY_EXTENSION" -a

new_case no-approve-short-explicit
run_wrapper -na
assert_argv_is --extension "$MANDATORY_EXTENSION" -na

# An approve flag among other args survives exactly once, in place.
new_case approve-among-args
run_wrapper --approve --mode rpc --no-session
assert_argv_is --extension "$MANDATORY_EXTENSION" --approve --mode rpc --no-session

# The exact line verify-pi.sh and pi-runtime.sh run: the explicit
# --no-approve is forwarded verbatim and never duplicated.
new_case rpc-no-approve
run_wrapper --mode rpc --no-session --no-approve
assert_argv_is --extension "$MANDATORY_EXTENSION" --mode rpc --no-session --no-approve

# `--` still terminates option forwarding unchanged: after it, `--no-approve`
# is a message passed through to Pi, not a wrapper concern.
new_case delimiter-terminates-forwarding
run_wrapper -- --no-approve
assert_argv_is --extension "$MANDATORY_EXTENSION" -- --no-approve

# The --extension injection happens after the subcommand check, so it never
# displaces argv[1]: a subcommand keeps its position, and an approve flag after
# it is just an argument.
new_case subcommand-argv-not-displaced
run_wrapper list --approve
assert_argv_is list --approve

# --- the wrapper's env decisions (#96) --------------------------------------

# An explicit PI_TELEMETRY passes through untouched; PI_SKIP_VERSION_CHECK stays
# enforced.
new_case telemetry-explicit
CASE_EXTRA_ENV="PI_TELEMETRY=1"
run_wrapper
assert_env_line 'PI_TELEMETRY=1'
assert_env_line 'PI_SKIP_VERSION_CHECK=1'

# An empty PI_TELEMETRY is also left alone (the wrapper no longer coerces it).
new_case telemetry-empty-passthrough
CASE_EXTRA_ENV="PI_TELEMETRY="
run_wrapper
assert_env_line 'PI_TELEMETRY='
assert_env_line 'PI_SKIP_VERSION_CHECK=1'

# PI_SKIP_VERSION_CHECK is enforced, not defaulted -- an empty value is
# overwritten.
new_case skip-version-check-empty
CASE_EXTRA_ENV="PI_SKIP_VERSION_CHECK="
run_wrapper
assert_env_line 'PI_SKIP_VERSION_CHECK=1'

# --- exec, not a subshell: the entry's status is the wrapper's ---------------

new_case exit-status
FAKE_EXIT_STATUS=42
run_wrapper --version
[[ $RUN_STATUS -eq 42 ]] || fail "exit status was $RUN_STATUS, expected 42 (exec, not subshell?)"

# --- stdin reaches the entry point (RPC mode depends on it) ------------------

new_case stdin-passthrough
printf 'hello from stdin\n' | env -i \
    "PATH=/usr/bin:/bin" \
    "AGENT_VM_PI_ENTRY=$CASE/fake-pi" \
    "AGENT_VM_PI_BRIDGE_EXTENSION=$CASE_BRIDGE_EXTENSION" \
    "AGENT_VM_PI_CODEX_EXTENSION=$CASE_CODEX_EXTENSION" \
    "FAKE_LOG=$CASE/log" \
    "CASE_STDIN=$CASE/stdin" \
    "CASE_ENV=$CASE/env" \
    "FAKE_EXIT_STATUS=" \
    sh "$WRAPPER" --mode rpc
[[ "$(cat "$CASE/stdin")" == 'hello from stdin' ]] \
    || fail "piped stdin did not reach the entry point"

# --- the installer-generated dispatch list ----------------------------------
# The selected subcommand set belongs to the installed Pi (0.87.0 has no `mcp`),
# so it arrives as data. The wrapper accepts only a canonical, closed-vocabulary
# list, dispatches exactly the words it selects, and keeps the mandatory-warning
# path for a word the selected Pi does not have.
new_case dispatch-legacy-list
CASE_SUBCOMMANDS="$CASE/subcommands"
printf 'auth\nconfig\ninstall\nlist\nremove\nuninstall\nupdate\n' >"$CASE_SUBCOMMANDS"
run_wrapper list
assert_argv_is list
# `mcp` is NOT in this Pi's list, so it is a prompt: the mandatory extension is
# injected and no subcommand path is taken.
: >"$CASE/log"
run_wrapper mcp
assert_argv_is --extension "$MANDATORY_EXTENSION" mcp
: >"$CASE/log"
run_wrapper "mcp list"
assert_argv_is --extension "$MANDATORY_EXTENSION" "mcp list"

new_case dispatch-default-list
CASE_SUBCOMMANDS="$CASE/subcommands"
printf 'auth\nconfig\ninstall\nlist\nmcp\nremove\nuninstall\nupdate\n' >"$CASE_SUBCOMMANDS"
run_wrapper mcp server
assert_argv_is mcp server
: >"$CASE/log"
run_wrapper mcp
assert_argv_is mcp

new_case dispatch-absent-falls-back
# No data file: the literal list (this build's own Pi) still dispatches.
run_wrapper update
assert_argv_is update
: >"$CASE/log"
run_wrapper mcp
assert_argv_is mcp

for malformed in unsorted duplicate unknown-word empty no-final-newline trailing-space; do
    new_case "dispatch-malformed-$malformed"
    CASE_SUBCOMMANDS="$CASE/subcommands"
    case "$malformed" in
        unsorted) printf 'list\nauth\n' >"$CASE_SUBCOMMANDS" ;;
        duplicate) printf 'auth\nauth\n' >"$CASE_SUBCOMMANDS" ;;
        unknown-word) printf 'auth\nevil\n' >"$CASE_SUBCOMMANDS" ;;
        empty) : >"$CASE_SUBCOMMANDS" ;;
        no-final-newline) printf 'auth\nlist' >"$CASE_SUBCOMMANDS" ;;
        trailing-space) printf 'auth \nlist\n' >"$CASE_SUBCOMMANDS" ;;
    esac
    run_wrapper list
    [[ $RUN_STATUS -eq 2 ]] || fail "$malformed list was not refused with status 2 (got $RUN_STATUS)"
    [[ ! -s "$CASE/log" ]] || fail "$malformed list still reached the entry point"
    if [[ $malformed == empty ]]; then
        expected_diagnostic="pi: refusing the empty subcommand list at $CASE_SUBCOMMANDS"
    else
        expected_diagnostic="pi: refusing the malformed subcommand list at $CASE_SUBCOMMANDS"
    fi
    grep -Fq "$expected_diagnostic" "$CASE/stderr" \
        || fail "the $malformed list did not report [$expected_diagnostic]"
done

new_case dispatch-symlinked
CASE_SUBCOMMANDS="$CASE/subcommands"
printf 'auth\n' >"$CASE/real-subcommands"
ln -s "$CASE/real-subcommands" "$CASE_SUBCOMMANDS"
run_wrapper auth
[[ $RUN_STATUS -eq 2 ]] || fail 'a symlinked subcommand list was not refused'
grep -Fq "pi: refusing the subcommand list at $CASE_SUBCOMMANDS (not a readable regular file)" "$CASE/stderr" \
    || fail 'a symlinked subcommand list did not report its exact diagnostic'

new_case dispatch-directory
CASE_SUBCOMMANDS="$CASE/subcommands"
mkdir "$CASE_SUBCOMMANDS"
run_wrapper auth
[[ $RUN_STATUS -eq 2 ]] || fail 'a directory subcommand list was not refused'

# --- the test seam cannot become the production default ----------------------

grep -Fq 'AGENT_VM_PI_ENTRY:-/opt/agent-vm/pi/node_modules/.bin/pi}"' "$WRAPPER" \
    || fail "production default entry point is missing"
grep -Fq "MANDATORY_EXTENSION=$MANDATORY_EXTENSION" "$WRAPPER" \
    || fail "production mandatory-extension path is missing"
grep -Fq 'AGENT_VM_PI_BRIDGE_EXTENSION:-/opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src/index.ts}"' "$WRAPPER" \
    || fail "production bridge-extension path is missing"
grep -Fq 'PI_SUBCOMMANDS="auth config install list mcp remove uninstall update"' "$WRAPPER" \
    || fail "the subcommand vocabulary literal changed (the image build checks drift against pi --help)"
grep -Fq 'PI_SUBCOMMAND_VOCABULARY=$PI_SUBCOMMANDS' "$WRAPPER" \
    || fail "the wrapper no longer restricts the data file to its closed vocabulary"
grep -Fq 'AGENT_VM_PI_SUBCOMMANDS:-/opt/agent-vm/pi/agent-vm-subcommands}' "$WRAPPER" \
    || fail "the production subcommand data path is missing"
# The wrapper must not reintroduce a trust or telemetry default. Match the code
# (not the prose above it, which deliberately names the flags it forwards).
if grep -Fq 'PI_TELEMETRY:=' "$WRAPPER" || grep -Fq 'export PI_TELEMETRY' "$WRAPPER"; then
    fail "the wrapper must not default or export PI_TELEMETRY (no telemetry default)"
fi
if grep -Fq 'approve=' "$WRAPPER" || grep -Fq 'for argument in' "$WRAPPER"; then
    fail "the wrapper must not inject or scan for approve flags (no trust default)"
fi

for bridge in absent present opted-out; do
    for codex in absent present; do
        new_case "codex-$codex-bridge-$bridge"
        expected=(--extension "$MANDATORY_EXTENSION")
        if [[ $bridge != absent ]]; then
            CASE_BRIDGE_EXTENSION="$CASE/bridge.ts"
            : >"$CASE_BRIDGE_EXTENSION"
            if [[ $bridge == opted-out ]]; then CASE_EXTRA_ENV=AGENT_VM_PI_NO_BRIDGE=1;
            else expected+=(--extension "$CASE_BRIDGE_EXTENSION"); fi
        fi
        if [[ $codex == present ]]; then
            CASE_CODEX_EXTENSION="$CASE/codex.js"
            : >"$CASE_CODEX_EXTENSION"
            expected+=(--extension "$CASE_CODEX_EXTENSION")
        fi
        run_wrapper
        [[ $RUN_STATUS -eq 0 ]] || fail 'codex matrix invocation failed'
        assert_argv_is "${expected[@]}"
    done
done

for variant in prompt subcommand argv-fidelity unreadable; do
    new_case "codex-present-$variant"
    CASE_CODEX_EXTENSION="$CASE/codex.js"
    : >"$CASE_CODEX_EXTENSION"
    case "$variant" in
        prompt|subcommand)
            CASE_BRIDGE_EXTENSION="$CASE/bridge.ts"
            : >"$CASE_BRIDGE_EXTENSION"
            if [[ $variant == prompt ]]; then
                run_wrapper 'fix the tests'
                assert_argv_is --extension "$MANDATORY_EXTENSION" --extension "$CASE_BRIDGE_EXTENSION" --extension "$CASE_CODEX_EXTENSION" 'fix the tests'
            else run_wrapper install foo; assert_argv_is install foo; fi ;;
        argv-fidelity)
            run_wrapper -- '-a b' '*' ''
            assert_argv_is --extension "$MANDATORY_EXTENSION" --extension "$CASE_CODEX_EXTENSION" -- '-a b' '*' '' ;;
        unreadable)
            if [[ $(id -u) -ne 0 ]]; then
                chmod 0000 "$CASE_CODEX_EXTENSION"
                run_wrapper
                assert_argv_is --extension "$MANDATORY_EXTENSION"
            fi ;;
    esac
done
grep -Fq 'AGENT_VM_PI_CODEX_EXTENSION:-/opt/agent-vm/pi-extensions/agent-vm-codex.js}"' "$WRAPPER" || fail 'codex default missing'

echo 'pi-wrapper black-box tests passed'
