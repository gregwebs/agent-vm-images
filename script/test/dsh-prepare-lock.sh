#!/usr/bin/env bash
# Black-box owning tests for images/tools/dsh/prepare-lock.sh and install-dsh.sh.
#
# Hermetic: no registry, no Docker, no host package manager. The real
# project.py authorization/publication code and the real recipe-contract graph and
# pin code run; only the manager is a
# fixture-confined fake that records argv, environment and store, and simulates
# --version, lockfile-only resolution and the frozen install separately. A PATH
# `npm`/`pnpm` sentinel fails the suite if any code path looks a manager up by
# PATH or falls back to npm.
#
# Publication fault injection lives in script/test/dsh-project.py, which drives
# every four-file position and every rollback position in-process with a failing
# os.replace. This suite proves the shell end of the same contract: success,
# a restored failure (status 1) and an incomplete rollback (status 3, real code
# path: a read-only prefix directory makes both the forward write and the
# restore fail).
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd -P)"
SOURCE="$REPO_ROOT/images/tools/dsh"
PREPARE="$SOURCE/prepare-lock.sh"
INSTALL="$SOURCE/install-dsh.sh"
FIXTURES="$REPO_ROOT/script/test/fixtures/pnpm-lock"
TEST_ROOT="$(cd "$(mktemp -d "${TMPDIR:-/tmp}/dsh-prepare-lock-test.XXXXXX")" && pwd -P)"
trap 'chmod -R u+w "$TEST_ROOT" 2>/dev/null || true; rm -rf "$TEST_ROOT"' EXIT
chmod 0755 "$TEST_ROOT"

for tool in jq node python3 timeout sha256sum; do
    command -v "$tool" >/dev/null || { echo "FAIL: $tool is required" >&2; exit 1; }
done

fail() {
    echo "FAIL${LAST_CASE:+ [$LAST_CASE]}: $*" >&2
    exit 1
}
LAST_CASE=''

assert_success() {
    [[ $STATUS -eq 0 ]] || fail "expected success, got $STATUS: $OUTPUT"
}

assert_failure() {
    [[ $STATUS -ne 0 ]] || fail "expected failure, got success: $OUTPUT"
}

assert_contains() {
    [[ $OUTPUT == *"$1"* ]] || fail "expected output to contain: $1 (got: $OUTPUT)"
}

# --- fixture layout ---------------------------------------------------------
# Every case shares one fixture root so the base pin, the fake manager, the
# recipe-contract copy and the install-status directory are authorized once.
ROOT="$TEST_ROOT/root"
mkdir -p "$ROOT/pnpm" "$ROOT/install-status" "$TEST_ROOT/bin" "$TEST_ROOT/outside"
contract_context=$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --native)
cp -R "$contract_context/out" "$ROOT/recipe-contract"
printf '{"version":"11.11.0"}\n' >"$ROOT/pnpm/pin.json"
printf 'outside the payload\n' >"$TEST_ROOT/outside/sentinel"

# A second root carries an unsupported base pin.
ROOT12="$TEST_ROOT/root12"
mkdir -p "$ROOT12/pnpm" "$ROOT12/install-status"
cp -R "$contract_context/out" "$ROOT12/recipe-contract"
printf '{"version":"12.9.1"}\n' >"$ROOT12/pnpm/pin.json"

cat >"$ROOT/pnpm-fake" <<'FAKE'
#!/usr/bin/env bash
set -u
printf '%s\n' "$*" >>"${STUB_LOG:-/dev/null}"
{
    printf 'cwd=%s\n' "$PWD"
    printf 'HOME=%s\n' "${HOME-}"
    printf 'XDG_CONFIG_HOME=%s\n' "${XDG_CONFIG_HOME-}"
    printf 'XDG_CACHE_HOME=%s\n' "${XDG_CACHE_HOME-}"
    printf 'XDG_DATA_HOME=%s\n' "${XDG_DATA_HOME-}"
    printf 'XDG_STATE_HOME=%s\n' "${XDG_STATE_HOME-}"
    printf 'TMPDIR=%s\n' "${TMPDIR-}"
    for key in PNPM_CONFIG_MINIMUM_RELEASE_AGE PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT \
        PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME PNPM_CONFIG_TRUST_POLICY \
        PNPM_CONFIG_IGNORE_SCRIPTS; do
        printf '%s=%s\n' "$key" "${!key-}"
    done
} >>"${STUB_ENV_LOG:-/dev/null}"
verb=other
for arg in "$@"; do
    case $arg in
        --version) verb=version ;;
        --lockfile-only) verb=lock ;;
        --prod) verb=install ;;
    esac
done
case $verb in
    version)
        if [ "${STUB_FAIL_VERSION:-0}" = 1 ]; then
            echo 'stub pnpm: version failed' >&2
            exit 7
        fi
        printf '%s\n' "${STUB_VERSION:-11.11.0}"
        ;;
    lock)
        if [ "${STUB_FAIL_LOCK:-0}" = 1 ]; then
            echo 'stub pnpm: resolution failed' >&2
            exit 7
        fi
        [ -f pnpm-lock.yaml ] || { echo 'stub pnpm: the committed lock was not staged' >&2; exit 6; }
        staged=$(jq -r '.importers["."].dependencies["@deepseek-ai/dsh"].version // empty' pnpm-lock.yaml)
        printf 'staged-lock-version=%s\n' "$staged" >>"${STUB_STAGE_LOG:-/dev/null}"
        [ -n "${STUB_RESULT:-}" ] || { echo 'stub pnpm: STUB_RESULT unset' >&2; exit 4; }
        cp "$STUB_RESULT/package.json" "$STUB_RESULT/pnpm-lock.yaml" .
        if [ "${STUB_MUTATE_PREFIX:-0}" = 1 ]; then
            ln -s "$STUB_OUTSIDE" "$STUB_PREFIX/node_modules"
        fi
        ;;
    install)
        selection=$(jq -r '.dependencies["@deepseek-ai/dsh"]' package.json)
        selection=${STUB_INSTALLED_VERSION:-$selection}
                dir="node_modules/.pnpm/@deepseek-ai+dsh@$selection/node_modules/@deepseek-ai/dsh"
        mkdir -p "$dir/lib" "node_modules/@deepseek-ai" node_modules/.pnpm
        manifest() {
            printf '{"name":"@deepseek-ai/dsh","version":"%s","bin":%s}\n' "$selection" "$1" >"$dir/package.json"
        }
        case ${STUB_LAYOUT:-ok} in
            ok | private)
                manifest '{"dsh":"lib/bin.js"}'
                printf '#!/usr/bin/env node\nconsole.log("stub dsh")\n' >"$dir/lib/bin.js"
                ;;
            badmap)
                manifest '{"dsh":"lib/other.js"}'
                printf '#!/usr/bin/env node\n' >"$dir/lib/other.js"
                ;;
            missing)
                manifest '{"dsh":"lib/bin.js"}'
                ;;
            escape)
                manifest '{"dsh":"lib/bin.js"}'
                ln -s "$STUB_OUTSIDE/sentinel" "$dir/lib/bin.js"
                ;;
            *) echo "stub pnpm: unknown layout ${STUB_LAYOUT}" >&2; exit 5 ;;
        esac
        ln -s "$PWD/$dir" "node_modules/@deepseek-ai/dsh"
        printf 'hoistPattern:\n' >node_modules/.modules.yaml
        if [ "${STUB_LAYOUT:-ok}" = private ]; then
            mkdir -p node_modules/pnpm
            printf '{"name":"pnpm","version":"11.11.0"}\n' >node_modules/pnpm/package.json
        fi
        if [ "${STUB_FAIL_INSTALL:-0}" = 1 ]; then
            mkdir -p node_modules/partial-payload
            echo 'stub pnpm: install failed' >&2
            exit 7
        fi
        if [ -n "${STUB_SLEEP:-}" ]; then
            sleep "$STUB_SLEEP"
            echo 'stub pnpm: install failed after tree creation' >&2
            exit 7
        fi
        ;;
    *)
        echo "stub pnpm: unhandled invocation: $*" >&2
        exit 9
        ;;
esac
FAKE
chmod 0755 "$ROOT/pnpm-fake"

# PATH sentinels: any npm invocation and any PATH-selected pnpm is a defect.
for stub in npm pnpm npx; do
    cat >"$TEST_ROOT/bin/$stub" <<'SENTINEL'
#!/usr/bin/env bash
printf 'forbidden PATH manager: %s %s\n' "${0##*/}" "$*" >>"${STUB_LOG:-/dev/null}"
echo "sentinel: PATH manager invoked: ${0##*/}" >&2
exit 9
SENTINEL
    chmod 0755 "$TEST_ROOT/bin/$stub"
done

# --- case helpers -----------------------------------------------------------
CASE=''
CALLER_HOME=''
EXTRA_ENV=()

new_case() {
    LAST_CASE=$1
    CASE="$TEST_ROOT/cases/$1"
    CALLER_HOME="$CASE/home"
    mkdir -p "$CASE" "$CALLER_HOME/.config/pnpm" "$CASE/result"
    : >"$CASE/manager.log"
    : >"$CASE/manager-env.log"
    # A caller HOME that carries its own npm/pnpm configuration must be accepted:
    # the manager always runs with a private HOME/XDG.
    printf 'registry=https://caller.invalid\n' >"$CALLER_HOME/.npmrc"
    printf 'registry=https://caller.invalid\n' >"$CALLER_HOME/.config/pnpm/rc"
    mkdir -p "$CALLER_HOME/.npm"
    printf 'caller npm cache bytes\n' >"$CALLER_HOME/.npm/sentinel"
    PREFIX="$ROOT/p-$1"
    mkdir -p "$PREFIX"
    cp -R "$FIXTURES/tiny-dsh/." "$PREFIX/"
    EXTRA_ENV=()
    STUB_RESULT=''
    STUB_LAYOUT=ok
    STUB_FAIL_LOCK=''
    STUB_FAIL_INSTALL=''
    STUB_FAIL_VERSION=''
    STUB_VERSION=''
    STUB_MUTATE_PREFIX=''
    STUB_SLEEP=''
    STUB_INSTALLED_VERSION=''
}

hash_prefix() {
    (cd "$PREFIX" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)
}

assert_prefix_unchanged() {
    local before=$1 now
    now=$(hash_prefix)
    [[ "$now" == "$before" ]] || fail "committed inputs changed:
--- before
$before
--- after
$now"
}

manager_log() {
    cat "$CASE/manager.log"
}

assert_manager_calls() {
    local expected=$1 actual
    actual=$(manager_log)
    [[ "$actual" == "$expected" ]] || fail "unexpected manager calls:
--- expected
$expected
--- actual
$actual"
}

# The scratch directory name is random, so resolution is checked by its flags.
assert_resolution_flags() {
    local line
    line=$(manager_log | sed -n 2p)
    for flag in --lockfile-only --no-frozen-lockfile --config.minimum-release-age=1440 \
        --config.minimum-release-age-strict=true --config.minimum-release-age-ignore-missing-time=false \
        --config.trust-policy=no-downgrade --ignore-scripts --ignore-pnpmfile \
        --pm-on-fail=error --config.update-notifier=false \
        --registry=https://registry.npmjs.org/ --package-import-method=copy --reporter=ndjson; do
        [[ "$line" == *" $flag"* ]] || fail "manager call missing $flag: $line"
    done
    [[ "$line" == "--version "* ]] && fail "second manager call must be the resolution: $line"
    [[ "$line" =~ --store-dir\ $ROOT/dsh-.*/store\  ]] || fail "manager call has no private store: $line"
    [[ "$line" == *" --prod "* || "$line" == *" --lockfile-only "* ]] || fail "manager call has no mode: $line"
}

assert_frozen_install_flags() {
    local line
    line=$(manager_log | sed -n 2p)
    [[ "$line" == "install --prod --frozen-lockfile "* ]] || fail "not a frozen production install: $line"
    for flag in --ignore-scripts --ignore-pnpmfile --pm-on-fail=error \
        --registry=https://registry.npmjs.org/ --package-import-method=copy --reporter=ndjson; do
        [[ "$line" == *" $flag"* ]] || fail "install call missing $flag: $line"
    done
    [[ "$line" == *"--no-optional"* ]] && fail "install must retain optionals: $line"
    return 0
}

# Every manager call owns an invocation scratch HOME/XDG/store, so caller
# configuration cannot be discovered and caller caches cannot be written.
assert_private_manager_state() {
    local key value
    for key in HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME TMPDIR; do
        value=$(grep "^$key=" "$CASE/manager-env.log" | head -1 | cut -d= -f2-)
        [[ -n "$value" ]] || fail "manager environment recorded no $key: $(cat "$CASE/manager-env.log")"
        [[ "$value" == "$ROOT"/dsh-* ]] || fail "manager did not get an invocation-private $key: $value"
    done
}

assert_no_scratch() {
    local leftovers
    leftovers=$(find "$ROOT" -maxdepth 1 -name 'dsh-*' -print)
    [[ -z "$leftovers" ]] || fail "scratch was not cleaned up: $leftovers"
}

run_prepare() {
    local root=${FIXTURE_ROOT-$ROOT}
    local env_args=(
        "PATH=$TEST_ROOT/bin:$PATH"
        "HOME=$CALLER_HOME"
        "TMPDIR=$CASE/tmp"
        "AGENT_VM_FIXTURE_ROOT=$root"
        "AGENT_VM_FIXTURE_PNPM=${FIXTURE_PNPM-$ROOT/pnpm-fake}"
        "AGENT_VM_CONTRACT_DIR=${CONTRACT_DIR-$ROOT/recipe-contract}"
        "AGENT_VM_FIXTURE_BASE_PNPM="
        "STUB_LOG=$CASE/manager.log"
        "STUB_ENV_LOG=$CASE/manager-env.log"
        "STUB_STAGE_LOG=$CASE/stage.log"
        "STUB_RESULT=$STUB_RESULT"
        "STUB_LAYOUT=$STUB_LAYOUT"
        "STUB_FAIL_LOCK=$STUB_FAIL_LOCK"
        "STUB_FAIL_INSTALL=$STUB_FAIL_INSTALL"
        "STUB_FAIL_VERSION=$STUB_FAIL_VERSION"
        "STUB_VERSION=$STUB_VERSION"
        "STUB_MUTATE_PREFIX=$STUB_MUTATE_PREFIX"
        "STUB_SLEEP=$STUB_SLEEP"
        "STUB_INSTALLED_VERSION=$STUB_INSTALLED_VERSION"
        "STUB_PREFIX=$PREFIX"
        "STUB_OUTSIDE=$TEST_ROOT/outside"
    )
    mkdir -p "$CASE/tmp"
    set +e
    OUTPUT=$(env -i "${env_args[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} sh "$PREPARE" "$PREFIX" "$@" 2>&1)
    STATUS=$?
    set -e
}

run_install() {
    local root=${FIXTURE_ROOT-$ROOT}
    local env_args=(
        "PATH=$TEST_ROOT/bin:$PATH"
        "HOME=$CALLER_HOME"
        "TMPDIR=$CASE/tmp"
        "AGENT_VM_FIXTURE_ROOT=$root"
        "AGENT_VM_FIXTURE_PNPM=${FIXTURE_PNPM-$ROOT/pnpm-fake}"
        "AGENT_VM_CONTRACT_DIR=${CONTRACT_DIR-$ROOT/recipe-contract}"
        "AGENT_VM_DSH_SOURCE_DIR=$SOURCE"
        "AGENT_VM_DSH_PREFIX=$PREFIX"
        "AGENT_VM_DSH_COMMAND=$PREFIX.dsh"
        "AGENT_VM_VERSION_DSH=${RUN_DSH_VERSION:-}"
        "AGENT_VM_VERSION_PNPM=${RUN_PNPM_VERSION:-}"
        "AGENT_INSTALL_SOFT_FAIL=${SOFT_FAIL:-}"
        "STUB_LOG=$CASE/manager.log"
        "STUB_ENV_LOG=$CASE/manager-env.log"
        "STUB_STAGE_LOG=$CASE/stage.log"
        "STUB_RESULT=$STUB_RESULT"
        "STUB_LAYOUT=$STUB_LAYOUT"
        "STUB_FAIL_LOCK=$STUB_FAIL_LOCK"
        "STUB_FAIL_INSTALL=$STUB_FAIL_INSTALL"
        "STUB_FAIL_VERSION=$STUB_FAIL_VERSION"
        "STUB_VERSION=$STUB_VERSION"
        "STUB_SLEEP=$STUB_SLEEP"
        "STUB_INSTALLED_VERSION=$STUB_INSTALLED_VERSION"
        "STUB_PREFIX=$PREFIX"
        "STUB_OUTSIDE=$TEST_ROOT/outside"
    )
    mkdir -p "$CASE/tmp"
    set +e
    OUTPUT=$(env -i "${env_args[@]}" ${EXTRA_ENV[@]+"${EXTRA_ENV[@]}"} sh "$INSTALL" 2>&1)
    STATUS=$?
    set -e
}

write_result() { # $1 source fixture, $2 (optional) jq filter on the lock
    local source=$1 filter=${2:-.}
    cp "$FIXTURES/$source/package.json" "$CASE/result/package.json"
    jq "$filter" "$FIXTURES/$source/pnpm-lock.yaml" >"$CASE/result/pnpm-lock.yaml"
    STUB_RESULT="$CASE/result"
}

# --- empty and equal selections: zero manager, four bytes ------------------
new_case equal
before=$(hash_prefix)
run_prepare '' ''
assert_success
assert_contains 'unchanged; reviewed inputs reused'
assert_manager_calls ''
assert_prefix_unchanged "$before"
assert_no_scratch

new_case equal-explicit
before=$(hash_prefix)
run_prepare 0.1.5-rc.2 11.11.0
assert_success
assert_manager_calls ''
assert_prefix_unchanged "$before"

# A caller HOME with .npmrc and pnpm/rc is accepted, and the manager subprocess
# still proves a private HOME/XDG once a changed selection runs it.
new_case caller-config
write_result tiny-dsh-previous
[[ -f "$CALLER_HOME/.npmrc" && -f "$CALLER_HOME/.config/pnpm/rc" ]] || fail 'caller config fixture missing'
run_prepare 0.1.5-rc.1 ''
assert_success
assert_private_manager_state
grep -q "^HOME=$CALLER_HOME$" "$CASE/manager-env.log" && fail 'manager saw the caller HOME'
grep -q "^XDG_CONFIG_HOME=$CALLER_HOME" "$CASE/manager-env.log" && fail 'manager saw the caller XDG_CONFIG_HOME'
[[ "$(cat "$CALLER_HOME/.npm/sentinel")" == 'caller npm cache bytes' ]] || fail 'caller npm cache was modified'

# --- equal with broken committed bytes -------------------------------------
new_case equal-broken-pin
printf 'drift\n' >>"$PREFIX/pnpm-workspace.yaml"
before=$(hash_prefix)
run_prepare '' ''
assert_failure
assert_contains 'bytes differ from reviewed pin'
assert_manager_calls ''
assert_prefix_unchanged "$before"

new_case equal-missing-pin
rm "$PREFIX/pnpm-inputs.sha256"
run_prepare '' ''
assert_failure
assert_manager_calls ''

# --- usage and selection grammar -------------------------------------------
new_case usage
run_prepare
assert_failure
[[ $STATUS -eq 2 ]] || fail "arity failure must exit 2, got $STATUS"
run_prepare '' '' --bogus
[[ $STATUS -eq 2 ]] || fail "unknown flag must exit 2, got $STATUS"
run_prepare '' '' --refresh-lock --refresh-lock
[[ $STATUS -eq 2 ]] || fail "duplicate flag must exit 2, got $STATUS"
assert_manager_calls ''

new_case bad-version
for bad in latest next '^1.2.3' '1.2' 'https://x' '1.2.3 beta' \
    "$(printf '1.2.4\n||\n>=1.2.3')" '01.2.3' '1.02.3' '1.2.03' '1.2.3-01'; do
    run_prepare "$bad" ''
    assert_failure
    assert_contains 'not an exact version'
    assert_manager_calls ''
done

# --- base ownership assertions ---------------------------------------------
new_case pnpm-mismatch
before=$(hash_prefix)
run_prepare '' 11.12.0
assert_failure
assert_contains 'disagrees with base pin'
assert_manager_calls ''
assert_prefix_unchanged "$before"

new_case manager12
# The unsupported pin lives in its own fixture root, so the prefix moves with it.
PREFIX="$ROOT12/p-manager12"
mkdir -p "$PREFIX"
cp -R "$FIXTURES/tiny-dsh/." "$PREFIX/"
FIXTURE_ROOT=$ROOT12
CONTRACT_DIR=$ROOT12/recipe-contract
run_prepare '' ''
FIXTURE_ROOT=$ROOT
unset CONTRACT_DIR
assert_failure
assert_contains 'unsupported base manager selection: 12.9.1'
assert_manager_calls ''

new_case base-pin-missing
mv "$ROOT/pnpm/pin.json" "$ROOT/pnpm/pin.json.hold"
run_prepare '' ''
mv "$ROOT/pnpm/pin.json.hold" "$ROOT/pnpm/pin.json"
assert_failure
assert_manager_calls ''

new_case manifest-manager
jq '.packageManager = "pnpm@12.9.1"' "$FIXTURES/tiny-dsh/package.json" >"$PREFIX/package.json"
before=$(hash_prefix)
run_prepare '' ''
assert_failure
assert_contains 'disagrees with base pin 11.11.0'
assert_manager_calls ''
assert_prefix_unchanged "$before"

# --- authorization: fixture root and prefix escapes -------------------------
new_case no-fixture-root
unset FIXTURE_ROOT
FIXTURE_ROOT=''
run_prepare '' ''
FIXTURE_ROOT=$ROOT
assert_failure
assert_contains 'AGENT_VM_FIXTURE_ROOT or AGENT_VM_SCRATCH_ROOT required'
assert_manager_calls ''

new_case prefix-escape
mkdir -p "$TEST_ROOT/outside/escape"
cp -R "$FIXTURES/tiny-dsh/." "$TEST_ROOT/outside/escape/"
before=$(cd "$TEST_ROOT/outside/escape" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)
ln -s "$TEST_ROOT/outside/escape" "$ROOT/p-prefix-escape-link"
PREFIX="$ROOT/p-prefix-escape-link"
run_prepare '' ''
rm "$PREFIX"
PREFIX="$ROOT/p-prefix-escape"
assert_failure
assert_contains 'symlink waypoint'
assert_manager_calls ''
[[ "$(cd "$TEST_ROOT/outside/escape" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256)" == "$before" ]] ||
    fail 'a symlinked prefix was written through'

# --- hostile environment is rejected before any manager --------------------
new_case hostile-env
SENTINEL_FILE="$CASE/preload-sentinel"
printf '#!/bin/sh\ntouch %s\n' "$SENTINEL_FILE" >"$CASE/preload.js"
for injection in \
    "NODE_OPTIONS=--require=$CASE/preload.js" \
    "NODE_PATH=$CASE" \
    "BUN_OPTIONS=--x" \
    "NODE_EXTRA_CA_CERTS=$CASE/ca.pem" \
    "SSL_CERT_FILE=$CASE/ca.pem" \
    "SSL_CERT_DIR=$CASE" \
    "NODE_TLS_REJECT_UNAUTHORIZED=0" \
    "npm_config_registry=https://evil.invalid" \
    "PNPM_HOME=$CASE/pnpm-home" \
    "corepack_home=$CASE/cache"; do
    EXTRA_ENV=("$injection")
    run_prepare '' ''
    assert_failure
    assert_manager_calls ''
done
EXTRA_ENV=()
[[ ! -f "$SENTINEL_FILE" ]] || fail 'a Node preload executed'
for hook in .npmrc pnpmfile.cjs .pnpmfile.mjs; do
    printf 'registry=https://evil.invalid\n' >"$PREFIX/$hook"
    before=$(hash_prefix)
    run_prepare '' ''
    assert_failure
    assert_contains 'unreviewed package-manager input'
    assert_manager_calls ''
    rm "$PREFIX/$hook"
done

# --- changed selection: exactly one version and one resolution --------------
new_case changed
write_result tiny-dsh-previous
before=$(hash_prefix)
run_prepare 0.1.5-rc.1 ''
assert_success
assert_contains 'published four dsh inputs for 0.1.5-rc.1'
[[ "$(manager_log | sed -n 1p)" == '--version --pm-on-fail=error --config.update-notifier=false --ignore-pnpmfile --registry=https://registry.npmjs.org/' ]] ||
    fail "the first manager call must be the identity check: $(manager_log)"
[[ "$(manager_log | wc -l | tr -d ' ')" -eq 2 ]] || fail "exactly two manager calls expected: $(manager_log)"
assert_resolution_flags
[[ "$(jq -r '.dependencies["@deepseek-ai/dsh"]' "$PREFIX/package.json")" == '0.1.5-rc.1' ]] || fail 'manifest selection did not change'
[[ "$(cat "$PREFIX/pnpm-inputs.sha256")" == "$(cd "$PREFIX" && sha256sum package.json pnpm-lock.yaml pnpm-workspace.yaml)" ]] ||
    fail 'published pins do not describe the published bytes'
[[ "$(cd "$PREFIX" && sha256sum pnpm-lock.yaml)" == "$(cd "$FIXTURES/tiny-dsh-previous" && sha256sum pnpm-lock.yaml)" ]] ||
    fail 'the resolved lock was not published'
grep -q '^PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440$' "$CASE/manager-env.log" || fail 'policy default missing in the manager environment'
grep -q "^PNPM_CONFIG_TRUST_POLICY=no-downgrade$" "$CASE/manager-env.log" || fail 'trust policy missing'
[[ ! -e "$PREFIX/node_modules" ]] || fail 'lock-only preparation created node_modules'
assert_no_scratch

# A changed selection whose result violates structure, peers, integrity or the
# closure rules must never publish.
reject_result() { # $1 description, $2 jq filter, $3 expected diagnostic
    local name=$1 filter=$2 diagnostic=$3
    new_case "reject-$name"
    write_result tiny-dsh-previous "$filter"
    before=$(hash_prefix)
    run_prepare 0.1.5-rc.1 ''
    assert_failure
    assert_contains "$diagnostic"
    assert_prefix_unchanged "$before"
    assert_no_scratch
}
reject_result bad-sri '.packages["@deepseek-ai/cordis@4.0.2"].resolution.integrity = "sha512-SHORT"' 'full sha512 required'
reject_result orphan '.packages["orphan@1.0.0"] = {"resolution":{"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}}' 'orphan'
reject_result no-sandbox 'del(.packages["@deepseek-ai/dsh-sandbox-local@1.0.0"]) | del(.snapshots["@deepseek-ai/dsh-sandbox-local@1.0.0"]) | del(.snapshots["@deepseek-ai/dsh@0.1.5-rc.1"].dependencies["@deepseek-ai/dsh-sandbox-local"])' 'sandbox'
reject_result peer-wrong '.snapshots["@deepseek-ai/cordis-plugin-loader@1.0.3(@deepseek-ai/cordis@4.0.2)"].dependencies["@deepseek-ai/cordis"] = "9.9.9"' 'dangling snapshot edge'
reject_result private-pnpm '.packages["pnpm@11.11.0"] = {"resolution":{"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}} | .snapshots["pnpm@11.11.0"] = {} | .snapshots["@deepseek-ai/dsh@0.1.5-rc.1"].dependencies.pnpm = "11.11.0"' 'cannot own pnpm'

# A manager failure publishes nothing.
new_case manager-failure
write_result tiny-dsh-previous
STUB_FAIL_LOCK=1
before=$(hash_prefix)
run_prepare 0.1.5-rc.1 ''
assert_failure
assert_contains 'staged resolution failed'
assert_prefix_unchanged "$before"
assert_no_scratch

new_case manager-version-mismatch
write_result tiny-dsh-previous
STUB_VERSION=12.9.1
before=$(hash_prefix)
run_prepare 0.1.5-rc.1 ''
assert_failure
assert_contains 'manager is not the base pin 11.11.0'
assert_prefix_unchanged "$before"

# --- --refresh-lock ---------------------------------------------------------
# Ordinary equal selection is a no-op; an explicit developer refresh re-resolves
# transitives at the same selection and publishes the reviewed delta.
new_case refresh-noop
before=$(hash_prefix)
run_prepare '' ''
assert_success
assert_manager_calls ''
assert_prefix_unchanged "$before"

new_case refresh
write_result tiny-dsh '.packages["leaf@1.0.0"] = {"resolution":{"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}}
    | .snapshots["leaf@1.0.0"] = {}
    | .snapshots["@deepseek-ai/dsh@0.1.5-rc.2"].dependencies.leaf = "1.0.0"'
run_prepare '' '' --refresh-lock
assert_success
assert_contains 'refresh 0.1.5-rc.2 -> 0.1.5-rc.2'
[[ -n "$(jq -r '.packages["leaf@1.0.0"] // empty' "$PREFIX/pnpm-lock.yaml")" ]] || fail 'refresh did not publish the re-resolved graph'

# --- N2: a developer upgrade of a host checkout ----------------------------
# A checkout is not a fixture. The contract directory is the checkout's own
# tracked copy (derived from project.py's location, never from an environment
# value), the caller names only the scratch root that authorizes its staged
# prefix, and AGENT_VM_FIXTURE_ROOT is unset. An unchanged map must still reach
# publication: this is the ordinary developer path, not a fixture exception.
run_prepare_host() {
    local env_args=(
        "PATH=$TEST_ROOT/bin:$PATH"
        "HOME=$CALLER_HOME"
        "TMPDIR=$CASE/tmp"
        "AGENT_VM_SCRATCH_ROOT=$ROOT"
        # A developer upgrade names the composed helper it built: a source
        # checkout authorizes as a path but carries no binary to execute.
        "AGENT_VM_CONTRACT_DIR=$ROOT/recipe-contract"
        "AGENT_VM_FIXTURE_PNPM=$ROOT/pnpm-fake"
        "AGENT_VM_FIXTURE_BASE_PNPM="
        "STUB_LOG=$CASE/manager.log"
        "STUB_ENV_LOG=$CASE/manager-env.log"
        "STUB_STAGE_LOG=$CASE/stage.log"
        "STUB_RESULT=$STUB_RESULT"
        "STUB_LAYOUT=$STUB_LAYOUT"
        "STUB_FAIL_LOCK=$STUB_FAIL_LOCK"
        "STUB_FAIL_INSTALL=$STUB_FAIL_INSTALL"
        "STUB_FAIL_VERSION=$STUB_FAIL_VERSION"
        "STUB_VERSION=$STUB_VERSION"
        "STUB_MUTATE_PREFIX=$STUB_MUTATE_PREFIX"
        "STUB_SLEEP=$STUB_SLEEP"
        "STUB_INSTALLED_VERSION=$STUB_INSTALLED_VERSION"
        "STUB_PREFIX=$PREFIX"
        "STUB_OUTSIDE=$TEST_ROOT/outside"
    )
    case "${env_args[*]}" in
        *AGENT_VM_FIXTURE_ROOT*) fail 'the host-checkout case must not export a fixture root' ;;
    esac
    mkdir -p "$CASE/tmp"
    set +e
    OUTPUT=$(env -i "${env_args[@]}" sh "$PREPARE" "$PREFIX" "$@" 2>&1)
    STATUS=$?
    set -e
}

new_case host-checkout
write_result tiny-dsh
run_prepare_host '' '' --refresh-lock
assert_success
assert_contains 'refresh 0.1.5-rc.2 -> 0.1.5-rc.2'
[[ -n "$(manager_log)" ]] || fail 'the host-checkout refresh started no manager'

new_case refresh-orphan
write_result tiny-dsh '.packages["orphan@1.0.0"] = {"resolution":{"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="}}'
before=$(hash_prefix)
run_prepare '' '' --refresh-lock
assert_failure
assert_contains 'orphan archive'
assert_prefix_unchanged "$before"

# A committed input whose bytes no longer match its own pins is never re-pinned by
# a build: the old project is validated before any publication.
new_case tampered-workspace
write_result tiny-dsh-previous
printf '\n' >>"$PREFIX/pnpm-workspace.yaml"
before=$(hash_prefix)
run_prepare 0.1.5-rc.1 ''
assert_failure
assert_contains 'bytes differ from reviewed pin'
assert_prefix_unchanged "$before"
assert_no_scratch

# --- publication outcomes ---------------------------------------------------
# Status 3 through the real code path: a read-only prefix makes the forward write
# fail and then the restore fail too, so the scratch and recovery must survive.
new_case publication-rollback
write_result tiny-dsh-previous
before=$(hash_prefix)
chmod 0555 "$PREFIX"
run_prepare 0.1.5-rc.1 ''
chmod 0755 "$PREFIX"
assert_failure
[[ $STATUS -eq 3 ]] || fail "incomplete rollback must exit 3, got $STATUS: $OUTPUT"
assert_contains 'rollback is incomplete'
assert_prefix_unchanged "$before"
leftovers=$(find "$ROOT" -maxdepth 1 -name 'dsh-*' -print)
[[ -n "$leftovers" ]] || fail 'status 3 must retain the staged scratch'
staged='' inventory=''
while read -r directory; do
    [ -d "$directory/prefix" ] && staged=$directory
    [ -f "$directory/inventory.json" ] && inventory=$directory
done <<<"$leftovers"
[[ -n "$staged" ]] || fail 'status 3 must retain the staged project'
[[ -n "$inventory" ]] || fail 'status 3 must retain the publication recovery inventory'
while read -r directory; do chmod -R u+w "$directory"; done <<<"$leftovers"
find "$ROOT" -maxdepth 1 -name 'dsh-*' -exec rm -rf {} +

# Status 1: the prefix is mutated after authorization, so publication refuses and
# restores; the staged graph is discarded and the committed bytes stay.
new_case publication-restored
write_result tiny-dsh-previous
STUB_MUTATE_PREFIX=1
before=$(hash_prefix)
run_prepare 0.1.5-rc.1 ''
assert_failure
[[ $STATUS -eq 1 ]] || fail "restored publication failure must exit 1, got $STATUS: $OUTPUT"
assert_contains 'publication failed'
rm -f "$PREFIX/node_modules"
assert_prefix_unchanged "$before"
assert_no_scratch

# --- owning install ---------------------------------------------------------
new_case install
before_home=$(find "$CALLER_HOME" -type f -exec sha256sum {} + | sort)
before=$(hash_prefix)
run_install
assert_success
assert_contains 'installed dsh 0.1.5-rc.2'
[[ "$(manager_log | sed -n 1p)" == '--version --pm-on-fail=error --config.update-notifier=false --ignore-pnpmfile --registry=https://registry.npmjs.org/' ]] ||
    fail "the first manager call must be the identity check: $(manager_log)"
[[ "$(manager_log | wc -l | tr -d ' ')" -eq 2 ]] || fail "exactly two manager calls expected: $(manager_log)"
assert_frozen_install_flags
[[ -x "$PREFIX/node_modules/@deepseek-ai/dsh/lib/bin.js" ]] || fail 'direct bin target is not executable'
[[ -d "$PREFIX/node_modules/.pnpm" ]] || fail 'runtime payload .pnpm was removed'
assert_prefix_unchanged "$before"
[[ "$(find "$CALLER_HOME" -type f -exec sha256sum {} + | sort)" == "$before_home" ]] || fail 'caller HOME was modified'
assert_no_scratch
assert_private_manager_state

new_case install-changed
write_result tiny-dsh-previous
RUN_DSH_VERSION=0.1.5-rc.1
run_install
RUN_DSH_VERSION=''
assert_success
[[ "$(jq -r '.dependencies["@deepseek-ai/dsh"]' "$PREFIX/package.json")" == '0.1.5-rc.1' ]] || fail 'changed selection was not prepared'
[[ "$(jq -r '.version' "$PREFIX/node_modules/@deepseek-ai/dsh/package.json")" == '0.1.5-rc.1' ]] || fail 'changed selection was not installed'

new_case install-pnpm-mismatch
RUN_PNPM_VERSION=11.12.0
before=$(hash_prefix)
run_install
RUN_PNPM_VERSION=''
assert_failure
assert_contains 'compatibility slot must equal base pin'
assert_manager_calls ''
assert_prefix_unchanged "$before"

# Every layout defect removes the partial tree and reports hard, whatever the
# soft-fail input says.
for layout in badmap missing escape private; do
    new_case "install-$layout"
    STUB_LAYOUT=$layout
    before=$(hash_prefix)
    run_install
    assert_failure
    [[ ! -e "$PREFIX/node_modules" ]] || fail "layout $layout left a partial node_modules"
    assert_prefix_unchanged "$before"
    assert_no_scratch
done

new_case install-version-mismatch
STUB_INSTALLED_VERSION=0.1.5-rc.3
run_install
assert_failure
assert_contains 'installed dsh version differs'
[[ ! -e "$PREFIX/node_modules" ]] || fail 'version mismatch left a partial node_modules'

new_case install-failure
STUB_FAIL_INSTALL=1
before_home=$(find "$CALLER_HOME" -type f -exec sha256sum {} + | sort)
before=$(hash_prefix)
run_install
assert_failure
assert_contains 'frozen install failed'
[[ ! -e "$PREFIX/node_modules" ]] || fail 'failed install left a partial node_modules'
assert_prefix_unchanged "$before"
assert_no_scratch
[[ "$(find "$CALLER_HOME" -type f -exec sha256sum {} + | sort)" == "$before_home" ]] || fail 'caller HOME was modified'

for soft in 1 0; do
    new_case "install-soft-$soft"
    STUB_FAIL_INSTALL=1
    SOFT_FAIL=$soft
    run_install
    SOFT_FAIL=''
    assert_failure
    [[ ! -e "$PREFIX/node_modules" ]] || fail 'soft input changed the hard outcome'
done

new_case install-missing-contract
STUB_FAIL_INSTALL=1
EXTRA_ENV=("AGENT_VM_CONTRACT_DIR=$ROOT/absent-contract")
run_install
EXTRA_ENV=()
assert_failure
assert_contains 'no executable recipe-contract'

# A signal must exit 143 and still remove the partial tree created before it.
new_case install-signal
STUB_SLEEP=3
before=$(hash_prefix)
mkdir -p "$CASE/tmp"
env -i "PATH=$TEST_ROOT/bin:$PATH" "HOME=$CALLER_HOME" "TMPDIR=$CASE/tmp" \
    "AGENT_VM_FIXTURE_ROOT=$ROOT" "AGENT_VM_FIXTURE_PNPM=$ROOT/pnpm-fake" \
    "AGENT_VM_CONTRACT_DIR=$ROOT/recipe-contract" "AGENT_VM_DSH_SOURCE_DIR=$SOURCE" \
    "AGENT_VM_DSH_PREFIX=$PREFIX" "AGENT_VM_DSH_COMMAND=$PREFIX.dsh" \
    "STUB_LOG=$CASE/manager.log" "STUB_ENV_LOG=$CASE/manager-env.log" \
    "STUB_SLEEP=$STUB_SLEEP" "STUB_PREFIX=$PREFIX" \
    sh "$INSTALL" >"$CASE/signal.log" 2>&1 &
pid=$!
for _ in $(seq 1 50); do
    [ -e "$PREFIX/node_modules" ] && break
    sleep 0.1
done
[ -e "$PREFIX/node_modules" ] || fail 'signal case never reached the install'
kill -TERM "$pid"
set +e
wait "$pid"
STATUS=$?
set -e
[[ $STATUS -eq 143 ]] || fail "signal must exit 143, got $STATUS"
[[ ! -e "$PREFIX/node_modules" ]] || fail 'signal left a partial node_modules'
assert_prefix_unchanged "$before"
sleep 3.5
[[ ! -e "$PREFIX/node_modules" ]] || fail 'the interrupted manager recreated node_modules'
STUB_SLEEP=''
assert_no_scratch

new_case install-no-source
EXTRA_ENV=("AGENT_VM_DSH_SOURCE_DIR=$CASE/absent-source")
run_install
EXTRA_ENV=()
assert_failure
assert_contains 'dsh project helper is missing'
assert_manager_calls ''

echo 'dsh prepare-lock and install black-box tests passed'
