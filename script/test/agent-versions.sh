#!/usr/bin/env bash
# Black-box tests for script/build/agent-versions.sh, the EXPLICIT developer
# write tool that resolves each single-slot installer layer's current upstream
# release and stages the reviewed exact default in its Dockerfile.
#
# No network and no repository writes: a fake `curl` (GitHub + the claude
# channel) and a fake `npm` (OpenCode and Copilot dist-tags) answer from environment
# variables, and every run targets a `--repo-root` fixture tree of four
# Dockerfiles with one `ARG AGENT_VERSION_*=<old>` line each. The real `jq` is
# used, since the JSON extraction is part of what is under test.
#
# Since #97 a write run also stages and publishes the Codex payload closure (pin,
# four evidence files, Dockerfile) as ONE transaction. The fixture repository
# therefore carries a committed OLD closure and the suite replaces
# `script/dev/pin-codex.py` with `fixtures/agent-versions/codex-writer.py`, which
# loads the REAL writer and swaps only its two seams (streaming fetch and blob
# verification). Everything else -- schema, evidence hashing, archive preflight,
# the fixed destination list, the shared Python publisher and every status -- is
# production code.

set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
SCRIPT="$REPO_ROOT/script/build/agent-versions.sh"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/agent-versions-test.XXXXXX")"
TEST_ROOT="$(cd "$TEST_ROOT" && pwd -P)"
trap 'rm -rf "$TEST_ROOT"' EXIT

REAL_JQ="$(command -v jq || true)"
[[ -n "$REAL_JQ" ]] || { echo "FAIL: jq is required" >&2; exit 1; }
JQ_DIR="$(dirname "$REAL_JQ")"
REAL_CP="$(command -v cp || true)"
[[ -n "$REAL_CP" ]] || { echo "FAIL: mv is required" >&2; exit 1; }
BASH_BIN="${BASH:-/bin/bash}"

# Upstream responses the fake curl/npm serve by default. The fixture Dockerfile
# defaults below are different, so the happy path rewrites all four.
DEFAULT_CODEX='{"tag_name":"rust-v0.30.0"}'
DEFAULT_OPENCODE='2.2.3'
DEFAULT_CLAUDE='1.0.100'
DEFAULT_COPILOT='0.1.30'

FIXTURE_OLD_CODEX='rust-v0.29.9'
DEFAULT_CODEX_RELEASE='rust-v0.30.0'
FIXTURE_OLD_OPENCODE='v2.2.2'
FIXTURE_OLD_CLAUDE='1.0.99'
FIXTURE_OLD_COPILOT='0.1.29'

STUB_BIN="$TEST_ROOT/bin"
mkdir -p "$STUB_BIN"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

assert_contains() {
    [[ "$1" == *"$2"* ]] || fail "expected output to contain: $2 (got: $1)"
}

assert_not_contains() {
    [[ "$1" != *"$2"* ]] || fail "expected output NOT to contain: $2 (got: $1)"
}

assert_status_fail() {
    [[ $RUN_STATUS -ne 0 ]] || fail "expected failure, got success: $RUN_OUTPUT"
}

assert_log_empty() {
    [[ ! -s "$CASE/log" ]] || fail "expected no network query, got: $(cat "$CASE/log")"
}

# The exact line count of PATTERN in FILE. grep -c prints 0 on no match; the
# `|| true` keeps set -e from aborting before we can read the count.
count_lines() {
    grep -c "$1" "$2" || true
}

cat >"$STUB_BIN/curl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
url="${@: -1}"
printf '%s\n' "$*" >>"${STUB_LOG:-/dev/null}"
[[ "${STUB_CURL_FAIL:-}" == 1 ]] && exit 1
case "$url" in
    *openai/codex*) printf '%s' "${STUB_CODEX-}" ;;
    *claude-code-releases/latest*) printf '%s' "${STUB_CLAUDE-}" ;;
    *) echo "stub curl: unexpected url: $url" >&2; exit 22 ;;
esac
SH
cat >"$STUB_BIN/npm" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${STUB_LOG:-/dev/null}"
[[ "${STUB_NPM_FAIL:-}" == 1 ]] && exit 1
case "$*" in
    'view @opencode/cli dist-tags.latest') printf '%s\n' "${STUB_OPENCODE-}" ;;
    'view @github/copilot dist-tags.latest') printf '%s\n' "${STUB_COPILOT-}" ;;
    *) exit 1 ;;
esac
SH
# Partial-write injection distinguishes publication from backup restoration.
cat >"$STUB_BIN/cp" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
src=${@: -2:1}; target=${@: -1}
if [[ "$target" == "${STUB_CP_FAIL_DEST:-}" ]]; then
    if [[ "$src" == */publish-backup/* ]]; then
        [[ "${STUB_ROLLBACK_FAIL:-}" == 1 ]] && exit 9
    else
        printf partial >"$target"
        exit 8
    fi
fi
exec "$REAL_CP" "$@"
SH
# The reviewed verifier's exact-version report. The owner writer only asks for the
# version (the verification seam is injected), so this is the whole stub.
cat >"$STUB_BIN/cosign" <<'SH'
#!/bin/sh
if [ "${1:-}" = version ]; then printf '{"gitVersion":"v2.4.1"}\n'; exit 0; fi
exit 0
SH
chmod +x "$STUB_BIN/curl" "$STUB_BIN/npm" "$STUB_BIN/cp" "$STUB_BIN/cosign"

CASE=""
FIXTURE_ROOT=""

codex_native_body_file() { # $1 release, $2 out
    printf 'printf "codex-cli %s\\n"\n' "${1#rust-v}" >"$2"
}

write_dockerfile() { # $1 = tool, remaining args = lines
    local tool="$1" dir line
    shift
    dir="$FIXTURE_ROOT/images/standard"
    mkdir -p "$dir"
    local arg
    arg="AGENT_VERSION_$(printf '%s' "$tool" | tr '[:lower:]' '[:upper:]')"
    if [ -f "$dir/Dockerfile" ]; then
        grep -v "^ARG ${arg}=" "$dir/Dockerfile" >"$dir/next" || true
        mv "$dir/next" "$dir/Dockerfile"
    fi
    for line in "$@"; do printf '%s\n' "$line" >>"$dir/Dockerfile"; done
}

reset_fixture() {
    FIXTURE_ROOT="$CASE/repo"
    rm -rf "$FIXTURE_ROOT"
    mkdir -p "$FIXTURE_ROOT"
    write_dockerfile codex "ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}"
    write_dockerfile opencode "ARG AGENT_VERSION_OPENCODE=${FIXTURE_OLD_OPENCODE}"
    write_dockerfile claude "ARG AGENT_VERSION_CLAUDE=${FIXTURE_OLD_CLAUDE}"
    write_dockerfile copilot "ARG AGENT_VERSION_COPILOT=${FIXTURE_OLD_COPILOT}"
    # shellcheck disable=SC2016 # literal Dockerfile interpolation
    printf 'LABEL org.agent-vm.version.pi="${AGENT_VERSION_PI:-0.87.1}"\n' >>"$FIXTURE_ROOT/images/standard/Dockerfile"
    mkdir -p "$FIXTURE_ROOT/images/pnpm" "$FIXTURE_ROOT/images/tools/dsh"
    cp "$REPO_ROOT/images/pnpm/pin.json" "$FIXTURE_ROOT/images/pnpm/pin.json"
    cp "$REPO_ROOT"/images/tools/dsh/package.json "$REPO_ROOT"/images/tools/dsh/pnpm-lock.yaml \
        "$REPO_ROOT"/images/tools/dsh/pnpm-workspace.yaml "$REPO_ROOT"/images/tools/dsh/pnpm-inputs.sha256 \
        "$FIXTURE_ROOT/images/tools/dsh/"
    # The committed OLD closure plus the real helper modules the writer imports.
    mkdir -p "$FIXTURE_ROOT/images/tools/codex" "$FIXTURE_ROOT/script/dev"
    codex_native_body_file "$FIXTURE_OLD_CODEX" "$CASE/native-committed.sh"
    python3 "$REPO_ROOT/script/test/codex-pins.py" --write-installer-fixture \
        "$CASE/codex-committed" --release "$FIXTURE_OLD_CODEX" \
        --target x86_64-unknown-linux-musl \
        --native-script "$CASE/native-committed.sh" >/dev/null
    cp "$CASE/codex-committed/source/pin.json" "$FIXTURE_ROOT/images/tools/codex/"
    cp -R "$CASE/codex-committed/source/evidence" "$FIXTURE_ROOT/images/tools/codex/"
    cp "$REPO_ROOT/script/dev/pin-artifact.py" "$FIXTURE_ROOT/script/dev/"
    # The shared publisher imports the reviewed artifact model, so the fixture
    # needs the real `images/artifacts` package (unused, but present).
    cp -R "$REPO_ROOT/images/artifacts" "$FIXTURE_ROOT/images/artifacts"
    cp "$REPO_ROOT/script/test/fixtures/agent-versions/codex-writer.py" \
        "$FIXTURE_ROOT/script/dev/pin-codex.py"
    cp "$FIXTURE_ROOT/images/standard/Dockerfile" "$CASE/original"
    # shellcheck disable=SC2016 # literal Dockerfile interpolation
    printf 'LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"\n' \
        >>"$FIXTURE_ROOT/images/standard/Dockerfile"
    cp "$FIXTURE_ROOT/images/standard/Dockerfile" "$CASE/original"
}

# What the fake upstream and the writer's injected fetch will serve. A bump case
# serves a NEWER release; the "already current" case serves the committed one.
write_codex_assets() {
    local release=$1 dir="$CASE/codex-assets"
    rm -rf "$dir"
    codex_native_body_file "$release" "$CASE/codex-native.sh"
    python3 "$REPO_ROOT/script/test/codex-pins.py" --write-installer-fixture "$dir" \
        --release "$release" --target x86_64-unknown-linux-musl \
        --native-script "$CASE/codex-native.sh" >/dev/null
    # The fixture's entrypoint is a recording shell script, so the fixture source
    # directory carries the same test-only inspector adapter the vendored suite
    # uses. Production code is unchanged.
    cp "$dir/source/codex_pin.py" "$FIXTURE_ROOT/images/tools/codex/codex_pin.py"
    # The fixture loader's checked-in sidecar names the real module it adapts.
    cp "$dir/source/codex-pin-adapter.json" \
        "$FIXTURE_ROOT/images/tools/codex/codex-pin-adapter.json"
}

new_case() {
    CASE="$TEST_ROOT/$1"
    mkdir -p "$CASE/home"
    STUB_CODEX="$DEFAULT_CODEX"
    STUB_OPENCODE="$DEFAULT_OPENCODE"
    STUB_CLAUDE="$DEFAULT_CLAUDE"
    STUB_COPILOT="$DEFAULT_COPILOT"
    unset STUB_ROLLBACK_FAIL STUB_CURL_FAIL STUB_NPM_FAIL GH_TOKEN GITHUB_ACTIONS GITHUB_OUTPUT STUB_CP_FAIL_DEST
    : >"$CASE/log"
    : >"$CASE/stderr"
    SERVED_CODEX_RELEASE="$DEFAULT_CODEX_RELEASE"
    unset STUB_CODEX_INJECT STUB_CODEX_INJECT_AT STUB_CODEX_REFUSE_RESTORE \
        STUB_CODEX_RECORD_ENOSPC STUB_CODEX_VERIFY STUB_CODEX_FAIL_KIND
    reset_fixture
    write_codex_assets "$SERVED_CODEX_RELEASE"
    ARGS=(--write --repo-root "$FIXTURE_ROOT" --cosign "$STUB_BIN/cosign")
}

ARGS=()
run_agent_versions() {
    set +e
    RUN_OUTPUT="$(env -i \
        "PATH=$STUB_BIN:$JQ_DIR:/usr/bin:/bin" \
        "HOME=$CASE/home" \
        "TMPDIR=$CASE" \
        "STUB_ROLLBACK_FAIL=${STUB_ROLLBACK_FAIL:-}" \
        "REAL_CP=$REAL_CP" \
        "STUB_LOG=$CASE/log" \
        "STUB_CODEX=${STUB_CODEX-}" \
        "STUB_OPENCODE=${STUB_OPENCODE-}" \
        "STUB_CLAUDE=${STUB_CLAUDE-}" \
        "STUB_COPILOT=${STUB_COPILOT-}" \
        "STUB_CURL_FAIL=${STUB_CURL_FAIL-}" \
        "STUB_NPM_FAIL=${STUB_NPM_FAIL-}" \
        "STUB_CP_FAIL_DEST=${STUB_CP_FAIL_DEST-}" \
        "FIXTURE_CODEX_WRITER=$REPO_ROOT/script/dev/pin-codex.py" \
        "FIXTURE_CODEX_ASSETS=$CASE/codex-assets/assets" \
        "FIXTURE_CODEX_VERIFY=${STUB_CODEX_VERIFY-}" \
        "FIXTURE_CODEX_INJECT=${STUB_CODEX_INJECT:-none}" \
        "FIXTURE_CODEX_INJECT_AT=${STUB_CODEX_INJECT_AT-}" \
        "FIXTURE_CODEX_REFUSE_RESTORE=${STUB_CODEX_REFUSE_RESTORE-}" \
        "FIXTURE_CODEX_RECORD_ENOSPC=${STUB_CODEX_RECORD_ENOSPC-}" \
        "FIXTURE_CODEX_FAIL_URL=${STUB_CODEX_FAIL_URL-}" \
        "GH_TOKEN=${GH_TOKEN-}" \
        "GITHUB_ACTIONS=${GITHUB_ACTIONS-}" \
        "GITHUB_OUTPUT=${GITHUB_OUTPUT-}" \
        "$BASH_BIN" "$SCRIPT" ${ARGS[@]+"${ARGS[@]}"} 2>"$CASE/stderr")"
    RUN_STATUS=$?
    # The dsh layer is one reviewed pnpm project, so all four inputs plus the
    # base pin are frozen against a bump owned by other layers.
    for unchanged in images/pnpm/pin.json images/tools/dsh/package.json \
        images/tools/dsh/pnpm-lock.yaml images/tools/dsh/pnpm-workspace.yaml \
        images/tools/dsh/pnpm-inputs.sha256; do
        cmp -s "$REPO_ROOT/$unchanged" "$FIXTURE_ROOT/$unchanged" || fail "agent bump changed $unchanged"
    done
    RUN_STDERR="$(cat "$CASE/stderr")"
    RUN_COMBINED="$RUN_OUTPUT
$RUN_STDERR"
    set -e
}

# --- usage / argument handling: no network, no writes -----------------------

new_case help
ARGS=(--help)
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "--help must succeed: $RUN_STDERR"
assert_contains "$RUN_OUTPUT" "Usage:"
assert_log_empty
grep -q "^ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
    || fail "--help must not touch the Dockerfiles"

new_case no-args
ARGS=()
run_agent_versions
[[ $RUN_STATUS -eq 2 ]] || fail "no args must exit 2, got $RUN_STATUS"
assert_contains "$RUN_STDERR" "Usage:"
assert_log_empty

new_case unknown-arg
ARGS=(--bogus)
run_agent_versions
[[ $RUN_STATUS -eq 2 ]] || fail "an unknown arg must exit 2, got $RUN_STATUS"
assert_contains "$RUN_STDERR" "unknown argument: --bogus"
assert_log_empty

new_case unknown-after-write
ARGS=(--write --bogus)
run_agent_versions
[[ $RUN_STATUS -eq 2 ]] || fail "an unknown arg after --write must exit 2, got $RUN_STATUS"
assert_contains "$RUN_STDERR" "unknown argument: --bogus"
assert_log_empty

new_case repo-root-without-write
ARGS=(--repo-root "$FIXTURE_ROOT")
run_agent_versions
[[ $RUN_STATUS -eq 2 ]] || fail "--repo-root without --write must exit 2, got $RUN_STATUS"
assert_contains "$RUN_STDERR" "Usage:"
assert_log_empty

new_case missing-repo-root-dir
ARGS=(--write --repo-root "$CASE/nope")
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "not a directory"
assert_log_empty

# --- GITHUB_ACTIONS refuses before any query or write -----------------------

new_case github-actions
GITHUB_ACTIONS=true
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "refusing to run under GitHub Actions"
assert_log_empty
grep -q "^ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
    || fail "a refused CI run must not touch the Dockerfiles"

# --- happy path: four staged exact ARG edits --------------------------------

new_case happy
GITHUB_OUTPUT="$CASE/github_output"
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "the happy path must succeed: $RUN_STDERR"
assert_contains "$RUN_OUTPUT" "codex ${FIXTURE_OLD_CODEX} -> rust-v0.30.0"
assert_contains "$RUN_OUTPUT" "opencode ${FIXTURE_OLD_OPENCODE} -> v2.2.3"
assert_contains "$RUN_OUTPUT" "claude ${FIXTURE_OLD_CLAUDE} -> 1.0.100"
assert_contains "$RUN_OUTPUT" "copilot ${FIXTURE_OLD_COPILOT} -> 0.1.30"
assert_contains "$RUN_OUTPUT" "images/standard/Dockerfile"
assert_contains "$RUN_OUTPUT" "images/standard/Dockerfile"
assert_contains "$RUN_OUTPUT" "images/standard/Dockerfile"
assert_contains "$RUN_OUTPUT" "images/standard/Dockerfile"
# The old interface's machine-adoptable `tool=version` lines are gone.
assert_not_contains "$RUN_OUTPUT" "codex="
assert_not_contains "$RUN_OUTPUT" "opencode="
# The write tool never emits GITHUB_OUTPUT.
[[ ! -e "$GITHUB_OUTPUT" ]] || fail "GITHUB_OUTPUT must not be written: $(cat "$GITHUB_OUTPUT")"

assert_arg_rewritten() { # $1 = tool, $2 = new value
    local file="$FIXTURE_ROOT/images/standard/Dockerfile" count
    count="$(count_lines "^ARG AGENT_VERSION_$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]')=$2\$" "$file")"
    [[ "$count" == 1 ]] || fail "$1: expected exactly one exact ARG line, found $count in $(cat "$file")"
}

# All four fixture Dockerfiles must still carry their original ARG default: a
# rejected fetched response must never stage a partial write.
assert_codex_unchanged() { # the committed closure plus the Dockerfile
    local relative
    for relative in images/tools/codex/pin.json \
        images/tools/codex/evidence/release.json \
        images/tools/codex/evidence/codex-package_SHA256SUMS \
        images/tools/codex/evidence/codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore \
        images/tools/codex/evidence/codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore; do
        cmp -s "$CASE/codex-committed/source/${relative#images/tools/codex/}" \
            "$FIXTURE_ROOT/$relative" ||
            fail "a refused run changed $relative"
    done
}

assert_all_args_unchanged() {
    grep -q "^ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
        || fail "codex Dockerfile changed on rejection: $(cat "$FIXTURE_ROOT/images/standard/Dockerfile")"
    grep -q "^ARG AGENT_VERSION_OPENCODE=${FIXTURE_OLD_OPENCODE}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
        || fail "opencode Dockerfile changed on rejection"
    grep -q "^ARG AGENT_VERSION_CLAUDE=${FIXTURE_OLD_CLAUDE}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
        || fail "claude Dockerfile changed on rejection"
    grep -q "^ARG AGENT_VERSION_COPILOT=${FIXTURE_OLD_COPILOT}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
        || fail "copilot Dockerfile changed on rejection"
}
assert_arg_rewritten codex rust-v0.30.0
# One transaction published the pin, its four evidence files and the Dockerfile.
python3 "$REPO_ROOT/script/test/fixtures/installer-egress/codex-pin-release.py" \
    --digests "$CASE/codex-assets/source/pin.json" >"$CASE/pin-expected"
python3 "$REPO_ROOT/script/test/fixtures/installer-egress/codex-pin-release.py" \
    --digests "$FIXTURE_ROOT/images/tools/codex/pin.json" >"$CASE/pin-published"
cmp -s "$CASE/pin-expected" "$CASE/pin-published" ||
    fail 'the published codex pin does not carry the staged identities'
for evidence in release.json codex-package_SHA256SUMS \
    codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore \
    codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore; do
    cmp -s "$CASE/codex-assets/source/evidence/$evidence" \
        "$FIXTURE_ROOT/images/tools/codex/evidence/$evidence" ||
        fail "the codex evidence $evidence was not published"
done
assert_arg_rewritten opencode v2.2.3
assert_arg_rewritten claude 1.0.100
assert_arg_rewritten copilot 0.1.30
assert_not_contains "$(cat "$FIXTURE_ROOT/images/standard/Dockerfile")" "$FIXTURE_OLD_CODEX"

# --- the authentication token never appears in diagnostics ------------------

new_case token-not-printed
GH_TOKEN=secret-token
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "the authenticated path must succeed: $RUN_STDERR"
assert_not_contains "$RUN_COMBINED" "secret-token"
assert_contains "$(cat "$CASE/log")" "Authorization: Bearer secret-token"

# --- already-current defaults are a clean no-op -----------------------------

new_case already-current
STUB_CODEX='{"tag_name":"rust-v0.29.9"}'
STUB_OPENCODE='2.2.2'
STUB_CLAUDE='1.0.99'
STUB_COPILOT='0.1.29'
SERVED_CODEX_RELEASE="$FIXTURE_OLD_CODEX"
write_codex_assets "$SERVED_CODEX_RELEASE"
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "a no-op run must succeed: $RUN_STDERR"
assert_contains "$RUN_OUTPUT" "already current"
assert_contains "$RUN_OUTPUT" "codex closure was still verified"
cmp "$FIXTURE_ROOT/images/standard/Dockerfile" "$CASE/original"
cmp "$FIXTURE_ROOT/images/tools/codex/pin.json" "$CASE/codex-committed/source/pin.json" ||
    fail 'a repeat run must leave the committed pin byte-identical'

grep -q "^ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
    || fail "a no-op run must not touch the Dockerfiles"

# --- HTTP-200-but-garbage / malformed values, per tool ----------------------

new_case codex-null
STUB_CODEX='{"tag_name":null}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version empty/null"

new_case codex-empty-string
STUB_CODEX='{"tag_name":""}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version empty/null"

new_case codex-html
STUB_CODEX='<html><body>error</body></html>'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version lookup failed"

new_case codex-multiline
STUB_CODEX='{"tag_name":"rust-v1.0.0\nrust-v1.0.1"}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version has unexpected characters"

new_case codex-metachar
STUB_CODEX='{"tag_name":"v1.2.3; rm -rf /"}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version has unexpected characters"

new_case codex-no-prefix
STUB_CODEX='{"tag_name":"1.2.3"}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex tag is not rust-v<semver>: '1.2.3'"

new_case codex-bad-semver
STUB_CODEX='{"tag_name":"rust-v1.2"}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version is not exact semver: 'rust-v1.2'"

new_case opencode-null
STUB_OPENCODE='null'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "opencode version empty/null"

new_case opencode-empty
STUB_OPENCODE=''
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "opencode version empty/null"

new_case opencode-no-prefix
STUB_OPENCODE='v2.2.3'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "opencode version is not exact semver"

for bad in '1.2.3' '3.0.0' '2.2' '^2.2.3' '2.2.3 ' '2.2.3
2.2.4'; do
    new_case "opencode-bad-$(printf '%s' "$bad" | tr -c 'A-Za-z0-9' '_')"
    STUB_OPENCODE="$bad"
    run_agent_versions
    assert_status_fail
    assert_all_args_unchanged
done

new_case claude-empty
STUB_CLAUDE=''
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version empty/null"

new_case claude-null
STUB_CLAUDE='null'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version empty/null"

new_case claude-html
STUB_CLAUDE='<html>'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version has unexpected characters"

new_case claude-whitespace
STUB_CLAUDE='1.2.3 '
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version has unexpected characters"

new_case claude-leading-zero
STUB_CLAUDE='01.2.3'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version is not exact semver: '01.2.3'"

new_case claude-bare-major-minor
STUB_CLAUDE='1.2'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version is not exact semver: '1.2'"

new_case copilot-empty
STUB_COPILOT=''
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "copilot version empty/null"

new_case copilot-garbage
STUB_COPILOT='not-a-version'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "copilot version is not exact semver: 'not-a-version'"

new_case copilot-metachar
# shellcheck disable=SC2016  # the literal `$(...)` is exactly the metachar input under test
STUB_COPILOT='1.2.3$(reboot)'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "copilot version has unexpected characters"

# --- canonical grammar: empty and leading-zero pre-release/build ids --------
# Each response below was accepted by the old `[0-9A-Za-z.-]+` body and would
# have been staged into the Dockerfile, where the owning installer rejects it.
# The binder must refuse it before any staging (review E1).

new_case claude-prerelease-leading-zero
STUB_CLAUDE='1.2.3-01'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version is not exact semver: '1.2.3-01'"
assert_all_args_unchanged

new_case claude-prerelease-empty-id
STUB_CLAUDE='1.2.3-a..b'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version is not exact semver: '1.2.3-a..b'"
assert_all_args_unchanged

new_case claude-prerelease-trailing-dot
STUB_CLAUDE='1.2.3-a.'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version is not exact semver: '1.2.3-a.'"
assert_all_args_unchanged

new_case claude-build-not-supported
# The claude owning hook has no `+build` alternative; the bumper must be no
# more permissive than the hook, so an empty or any build id is refused here.
STUB_CLAUDE='1.2.3+..'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "claude version is not exact semver: '1.2.3+..'"
assert_all_args_unchanged

new_case copilot-build-empty-id
STUB_COPILOT='1.2.3+..'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "copilot version is not exact semver: '1.2.3+..'"
assert_all_args_unchanged

new_case copilot-build-leading-dot
STUB_COPILOT='1.2.3+.b'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "copilot version is not exact semver: '1.2.3+.b'"
assert_all_args_unchanged

new_case opencode-prerelease-leading-zero
STUB_OPENCODE='2.2.3-01'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "opencode version is not exact semver: '2.2.3-01'"
assert_all_args_unchanged

new_case codex-prerelease-leading-zero
STUB_CODEX='{"tag_name":"rust-v1.2.3-alpha.01"}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version is not exact semver: 'rust-v1.2.3-alpha.01'"
assert_all_args_unchanged

new_case codex-unsupported-prerelease
# codex's owning hook accepts only its alpha/beta subset, so a generic
# pre-release spelling must be refused at the bumper instead of staged.
STUB_CODEX='{"tag_name":"rust-v1.2.3-rc.1"}'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version is not exact semver: 'rust-v1.2.3-rc.1'"
assert_all_args_unchanged

# --- canonical grammar: accepted ids are still staged (controls) ------------

new_case canonical-prerelease-build-accepted
STUB_CODEX='{"tag_name":"rust-v1.2.3-alpha.1.2"}'
STUB_OPENCODE='2.2.3-rc.1+build.01'
STUB_CLAUDE='1.2.3-rc.1'
STUB_COPILOT='1.2.3+build.01'
SERVED_CODEX_RELEASE='rust-v1.2.3-alpha.1.2'
write_codex_assets "$SERVED_CODEX_RELEASE"
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "canonical version ids must be accepted: $RUN_STDERR"
assert_arg_rewritten codex rust-v1.2.3-alpha.1.2
assert_arg_rewritten opencode v2.2.3-rc.1+build.01
assert_arg_rewritten claude 1.2.3-rc.1
assert_arg_rewritten copilot 1.2.3+build.01

# --- lookup failures are hard and never partial-write -----------------------

new_case curl-fail
STUB_CURL_FAIL=1
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "codex version lookup failed"
grep -q "^ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
    || fail "a failed lookup must not touch the Dockerfiles"

new_case npm-fail
STUB_NPM_FAIL=1
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "opencode version lookup failed"

# --- Dockerfile guards: missing, duplicate and unwritable -------------------

new_case duplicate-arg
write_dockerfile codex "ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}" "ARG AGENT_VERSION_CODEX=rust-v0.1.0"
run_agent_versions
assert_status_fail
# shellcheck disable=SC2016  # the backticks are the message text under test
assert_contains "$RUN_COMBINED" 'exactly one `ARG AGENT_VERSION_CODEX=`'
assert_contains "$RUN_COMBINED" "found 2"

new_case missing-arg
write_dockerfile codex "LABEL org.example=1"
run_agent_versions
assert_status_fail
# shellcheck disable=SC2016  # the backticks are the message text under test
assert_contains "$RUN_COMBINED" 'exactly one `ARG AGENT_VERSION_CODEX=`'
assert_contains "$RUN_COMBINED" "found 0"

new_case missing-dockerfile
rm -f "$FIXTURE_ROOT/images/standard/Dockerfile"
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "missing Dockerfile"

# --- closure transaction: faults, rollback and retained recovery -----------
#
# Every case below drives the REAL shared Python publisher through the owner
# script. The injected fault targets an EVIDENCE destination (not just the
# Dockerfile), which is what proves publication is no longer a Dockerfile-only
# transaction.

EVIDENCE_BUNDLE='codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore'

# The permission mode of one path, as `chmod` accepts it. Each `stat` dialect is
# captured and status-checked SEPARATELY: GNU `stat -f` prints filesystem data
# before it fails, so `stat -f ... || stat -c ...` inside ONE command substitution
# produced a multi-line, invalid mode and took the whole suite down at
# partial-publication. Only a successful capture is ever printed.
file_mode() { # $1 = path
    local mode
    if mode="$(stat -c '%a' "$1" 2>/dev/null)" && [ -n "$mode" ]; then
        printf '%s\n' "$mode"
        return 0
    fi
    if mode="$(stat -f '%Lp' "$1" 2>/dev/null)" && [ -n "$mode" ]; then
        printf '%s\n' "$mode"
        return 0
    fi
    fail "cannot read the mode of $1 with stat -c or stat -f"
}

# The mode helper's own control, including the failure mode that broke the suite:
# a GNU `stat` that prints a filesystem block for the format argument and THEN
# fails must contribute nothing to the mode.
file_mode_self_check() {
    local dir="$TEST_ROOT/file-mode" fake real_stat mode
    rm -rf "$dir"
    mkdir -p "$dir/bin"
    real_stat="$(command -v stat)"
    : >"$dir/probe"
    chmod 0640 "$dir/probe"

    mode="$(file_mode "$dir/probe")"
    [ "$mode" = 640 ] || fail "file_mode returned '$mode', expected 640"
    [ "$(printf '%s\n' "$mode" | grep -c .)" = 1 ] ||
        fail "file_mode returned a multi-line mode: '$mode'"
    cp "$dir/probe" "$dir/copy"
    chmod "$mode" "$dir/copy" || fail "file_mode returned a mode chmod rejects: '$mode'"

    # The reported regression: a stat whose FIRST dialect fails after printing.
    # Only a real GNU stat has that behaviour, so the control runs where it can.
    if stat -c '%a' "$dir/probe" >/dev/null 2>&1; then
        fake="$dir/bin/stat"
        # shellcheck disable=SC2016  # these printf arguments are fixture shell source
        {
            printf '#!/bin/sh\n'
            printf 'case "$1" in\n'
            printf '  --format|-c) exec "%s" "$@" ;;\n' "$real_stat"
            printf '  -f|--file-system)\n'
            printf '    echo "  File: \"%%s\"" "$2"\n'
            printf '    echo "    ID: 0000 Namelen: 255 Type: fixture"\n'
            printf '    exit 1 ;;\n'
            printf 'esac\n'
            printf 'exec "%s" "$@"\n' "$real_stat"
        } >"$fake"
        chmod 0755 "$fake"
        mode="$(PATH="$dir/bin:$PATH" file_mode "$dir/probe")"
        case "$mode" in
            [0-7][0-7][0-7] | [0-7][0-7][0-7][0-7]) ;;
            *) fail "file_mode returned a contaminated mode: '$mode'" ;;
        esac
        [ "$mode" = 640 ] || fail "file_mode returned '$mode' under a failing stat -f"
        echo '  ok: file_mode ignores a stat dialect that prints before it fails'
    else
        echo '  ok: file_mode reads the BSD stat dialect (no GNU stat on PATH here)'
    fi
    echo '  ok: file_mode returns one octal mode that chmod accepts'
}

file_mode_self_check

# The six published destinations (five closure files plus the Dockerfile), as
# snapshot file names. One list, so the snapshot, the comparison and the retained
# backup check cannot drift apart.
SNAPSHOT_FILES=(
    images_standard_Dockerfile
    images_tools_codex_pin.json
    images_tools_codex_evidence_release.json
    images_tools_codex_evidence_codex-package_SHA256SUMS
    "images_tools_codex_evidence_$EVIDENCE_BUNDLE"
    images_tools_codex_evidence_codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore
    images_tools_codex_evidence_codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore
)

codex_snapshot() { # $1 = directory
    mkdir -p "$1"
    local relative
    for relative in images/standard/Dockerfile images/tools/codex/pin.json \
        images/tools/codex/evidence/release.json \
        images/tools/codex/evidence/codex-package_SHA256SUMS \
        "images/tools/codex/evidence/$EVIDENCE_BUNDLE" \
        images/tools/codex/evidence/codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore \
        images/tools/codex/evidence/codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore; do
        cp "$FIXTURE_ROOT/$relative" "$1/$(printf '%s' "$relative" | tr '/' '_')"
        chmod "$(file_mode "$FIXTURE_ROOT/$relative")" \
            "$1/$(printf '%s' "$relative" | tr '/' '_')"
    done
}

# Bytes AND modes: a rollback that restores the bytes through a different mode has
# changed the repository.
codex_compare_snapshot() { # $1 directory, $2 label
    local relative name
    for relative in images/standard/Dockerfile images/tools/codex/pin.json \
        images/tools/codex/evidence/release.json \
        images/tools/codex/evidence/codex-package_SHA256SUMS \
        "images/tools/codex/evidence/$EVIDENCE_BUNDLE" \
        images/tools/codex/evidence/codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore \
        images/tools/codex/evidence/codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore; do
        name="$(printf '%s' "$relative" | tr '/' '_')"
        cmp -s "$1/$name" "$FIXTURE_ROOT/$relative" ||
            fail "$2: $relative does not match the pre-publication snapshot"
        [ "$(file_mode "$1/$name")" = "$(file_mode "$FIXTURE_ROOT/$relative")" ] ||
            fail "$2: $relative mode changed ($(file_mode "$1/$name") vs $(file_mode "$FIXTURE_ROOT/$relative"))"
    done
}

retained_publisher_dir() {
    printf '%s\n' "$RUN_STDERR" |
        sed -n 's/.*recovery material retained at \([^ ]*\)$/\1/p' | head -n 1
}

retained_owner_dir() {
    printf '%s\n' "$RUN_STDERR" |
        sed -n 's/.*owner scratch retained at \(.*\)$/\1/p' | head -n 1
}

# The six files of the staged proposal, which is the half of the recovery material
# the owner scratch holds. One list, so the inspection, the byte comparison and the
# deletion mutants below cannot drift apart.
PROPOSAL_FILES=(
    pin.json
    Dockerfile
    evidence/release.json
    evidence/codex-package_SHA256SUMS
    evidence/codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore
    "evidence/$EVIDENCE_BUNDLE"
)

# The proposal list above and the snapshot list below are two independent literals.
# The inspection and its deletion mutants are both derived from the proposal list,
# so an entry dropped from it would silently stop being checked by *either* -- the
# exact shape of the missing-x86_64-bundle finding. This keeps them in step.
proposal_list_self_check() {
    local name relative entry matched
    [ "${#PROPOSAL_FILES[@]}" -eq 6 ] ||
        fail "PROPOSAL_FILES has ${#PROPOSAL_FILES[@]} entries, expected 6"
    for name in "${SNAPSHOT_FILES[@]}"; do
        case "$name" in
            images_standard_Dockerfile) relative=Dockerfile ;;
            images_tools_codex_pin.json) relative=pin.json ;;
            images_tools_codex_evidence_*) relative="evidence/${name#images_tools_codex_evidence_}" ;;
            *) fail "the snapshot list names an unknown file: $name" ;;
        esac
        matched=0
        for entry in "${PROPOSAL_FILES[@]}"; do
            [ "$entry" = "$relative" ] && matched=1
        done
        [ "$matched" -eq 1 ] ||
            fail "the proposal list does not cover $relative"
    done
    echo '  ok: the staged proposal list covers all six destinations'
}

# What the staged Dockerfile must be: the fixture's committed Dockerfile with the
# four single-slot ARG defaults it was staged for. Computed, never hard-coded, so a
# changed fixture default cannot silently weaken the comparison.
staged_dockerfile_expected() { # $1 = out file
    sed -e "s|^ARG AGENT_VERSION_CODEX=.*\$|ARG AGENT_VERSION_CODEX=${DEFAULT_CODEX_RELEASE}|" \
        -e "s|^ARG AGENT_VERSION_OPENCODE=.*\$|ARG AGENT_VERSION_OPENCODE=v${DEFAULT_OPENCODE}|" \
        -e "s|^ARG AGENT_VERSION_CLAUDE=.*\$|ARG AGENT_VERSION_CLAUDE=${DEFAULT_CLAUDE}|" \
        -e "s|^ARG AGENT_VERSION_COPILOT=.*\$|ARG AGENT_VERSION_COPILOT=${DEFAULT_COPILOT}|" \
        "$CASE/original" >"$1"
}

# The proposal this case's staging step must produce, from an independent staging
# run of the same fixture writer against the same committed repository. Only the
# pin is compared against it: the four evidence files are compared against the
# bytes upstream served and the Dockerfile against one computed here. A staging run
# writes nothing in the repository.
stage_reference_proposal() { # $1 = target directory
    local out=$1 status=0
    rm -rf "$out"
    GITHUB_ACTIONS='' \
        FIXTURE_CODEX_WRITER="$REPO_ROOT/script/dev/pin-codex.py" \
        FIXTURE_CODEX_ASSETS="$CASE/codex-assets/assets" \
        TMPDIR="$CASE" \
        python3 "$FIXTURE_ROOT/script/dev/pin-codex.py" \
        --version "$DEFAULT_CODEX_RELEASE" --stage-dir "$out" \
        --repo-root "$FIXTURE_ROOT" --cosign "$STUB_BIN/cosign" \
        >/dev/null 2>&1 || status=$?
    [ "$status" -eq 0 ] ||
        fail "the reference proposal could not be staged (status $status)"
}

# A status-3 run promises usable recovery material, not just a status code and a
# message. Both printed absolute directories must still exist, the owner scratch
# must still hold ALL SIX staged proposal files with exactly the bytes that were
# staged, and every numbered backup must still carry the pre-publication bytes AND
# mode of exactly one committed file, under a `.complete` marker. A cleanup
# regression that deleted either directory used to leave the status/text
# assertions green, and an incomplete proposal (the reviewer's missing x86_64
# bundle) used to pass as well.
proposal_list_self_check

verify_retained_recovery() { # $1 label, $2 snapshot dir, $3 owner override, $4 publisher override
    local label=$1 snapshot=$2 publisher owner entry name matched count numbered relative \
        expected
    publisher="${4:-$(retained_publisher_dir)}"
    owner="${3:-$(retained_owner_dir)}"
    [ -n "$publisher" ] || fail "$label: no retained publisher directory in: $RUN_STDERR"
    [ -n "$owner" ] || fail "$label: no retained owner scratch in: $RUN_STDERR"
    case "$publisher" in
        /*) ;;
        *) fail "$label: the retained publisher path is not absolute: $publisher" ;;
    esac
    case "$owner" in
        /*) ;;
        *) fail "$label: the owner scratch path is not absolute: $owner" ;;
    esac
    [ -d "$publisher" ] || fail "$label: the retained publisher directory is gone: $publisher"
    [ -d "$owner" ] || fail "$label: the retained owner scratch is gone: $owner"
    staged_dockerfile_expected "$CASE/staged-dockerfile"
    for relative in "${PROPOSAL_FILES[@]}"; do
        case "$relative" in
            Dockerfile) expected="$CASE/staged-dockerfile" ;;
            evidence/*) expected="$CASE/codex-assets/source/$relative" ;;
            *) expected="$CASE/expected-proposal/$relative" ;;
        esac
        [ -f "$expected" ] || fail "$label: the case has no expected $relative: $expected"
        [ -f "$owner/$relative" ] ||
            fail "$label: the owner scratch lost $relative: $owner"
        cmp -s "$owner/$relative" "$expected" ||
            fail "$label: the owner scratch holds different bytes for $relative"
    done
    [ -f "$publisher/backup/.complete" ] ||
        fail "$label: the retained backup has no .complete marker: $publisher"
    numbered="$(find "$publisher/backup" -maxdepth 1 -type f ! -name '.complete' | sort)"
    count=0
    for entry in $numbered; do count=$((count + 1)); done
    [ "$count" -eq 6 ] ||
        fail "$label: the retained backup has $count numbered copies, expected 6: $numbered"
    for name in "${SNAPSHOT_FILES[@]}"; do
        matched=0
        [ -f "$snapshot/$name" ] ||
            fail "$label: the snapshot is missing $name: $snapshot"
        for entry in $numbered; do
            if cmp -s "$entry" "$snapshot/$name" &&
                [ "$(file_mode "$entry")" = "$(file_mode "$snapshot/$name")" ]; then
                matched=$((matched + 1))
            fi
        done
        [ "$matched" -eq 1 ] ||
            fail "$label: $name has $matched retained backups matching its bytes and mode"
    done
    echo "  ok: $label: both retained directories hold the six numbered backups (+ .complete) and all six staged proposal files"
}

# The oracle's own control. A complete copy of the owner scratch passes; deleting
# ANY ONE of the six staged files must fail it. The x86_64 bundle was the one the
# old check omitted, so a proposal missing it used to pass. Nothing here touches
# the directories the run actually retained.
verify_recovery_oracle_mutants() { # $1 label, $2 snapshot dir
    local label=$1 snapshot=$2 source copy status relative
    source="$(retained_owner_dir)"
    copy="$CASE/owner-mutants"
    rm -rf "$copy"
    cp -R "$source" "$copy"
    status=0
    ( verify_retained_recovery "$label control" "$snapshot" "$copy" ) >/dev/null 2>&1 ||
        status=$?
    [ "$status" -eq 0 ] ||
        fail "$label: the recovery oracle rejects a complete copy of the owner scratch"
    for relative in "${PROPOSAL_FILES[@]}"; do
        rm -rf "$copy"
        cp -R "$source" "$copy"
        rm -f "$copy/$relative"
        status=0
        ( verify_retained_recovery "$label missing $relative" "$snapshot" "$copy" ) \
            >/dev/null 2>&1 || status=$?
        [ "$status" -ne 0 ] ||
            fail "$label: the recovery oracle accepts a proposal with no $relative"
    done
    echo "  ok: $label: deleting any one of the six staged proposal files fails the recovery oracle"
}

new_case partial-publication
codex_snapshot "$CASE/snapshot"
STUB_CODEX_INJECT=partial
STUB_CODEX_INJECT_AT="$EVIDENCE_BUNDLE"
run_agent_versions
[[ $RUN_STATUS -eq 1 ]] || fail "a failed publication must exit 1, got $RUN_STATUS"
codex_compare_snapshot "$CASE/snapshot" 'inclusive rollback'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
echo '  ok: a failed publication restores the Dockerfile and every evidence file'

new_case publication-signal
codex_snapshot "$CASE/snapshot"
STUB_CODEX_INJECT=signal
STUB_CODEX_INJECT_AT="$EVIDENCE_BUNDLE"
run_agent_versions
[[ $RUN_STATUS -eq 130 ]] || fail "a SIGINT during publication must exit 130, got $RUN_STATUS"
codex_compare_snapshot "$CASE/snapshot" 'signal rollback'
echo '  ok: a SIGINT during publication restores every destination'

new_case rollback-failure-recovers-nothing
codex_snapshot "$CASE/snapshot"
stage_reference_proposal "$CASE/expected-proposal"
STUB_CODEX_INJECT=partial
STUB_CODEX_INJECT_AT="$EVIDENCE_BUNDLE"
STUB_CODEX_REFUSE_RESTORE=1
run_agent_versions
[[ $RUN_STATUS -eq 3 ]] || fail "an incomplete rollback must exit 3, got $RUN_STATUS"
assert_contains "$RUN_STDERR" 'recovery material retained at /'
assert_contains "$RUN_STDERR" 'pin-artifact: owner scratch retained at /'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
verify_retained_recovery 'incomplete rollback' "$CASE/snapshot"
# ...and the oracle itself must be able to fail: one deleted proposal file, the
# x86_64 bundle included, has to be detected.
verify_recovery_oracle_mutants 'incomplete rollback' "$CASE/snapshot"

new_case rollback-failure-record-enospc
codex_snapshot "$CASE/snapshot"
stage_reference_proposal "$CASE/expected-proposal"
STUB_CODEX_INJECT=partial
STUB_CODEX_INJECT_AT="$EVIDENCE_BUNDLE"
STUB_CODEX_REFUSE_RESTORE=1
STUB_CODEX_RECORD_ENOSPC=1
run_agent_versions
[[ $RUN_STATUS -eq 3 ]] ||
    fail "a recovery record that cannot be written must still exit 3, got $RUN_STATUS"
assert_contains "$RUN_STDERR" 'no space left on device'
assert_contains "$RUN_STDERR" 'could not write the recovery record'
assert_contains "$RUN_STDERR" 'pin-artifact: owner scratch retained at /'
assert_not_contains "$RUN_COMBINED" 'Traceback'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
verify_retained_recovery 'unwritable recovery record' "$CASE/snapshot"
echo '  ok: an unwritable recovery record keeps status 3 and the recovery material'

new_case signal-rollback-failure-record-enospc
codex_snapshot "$CASE/snapshot"
stage_reference_proposal "$CASE/expected-proposal"
STUB_CODEX_INJECT=signal
STUB_CODEX_INJECT_AT="$EVIDENCE_BUNDLE"
STUB_CODEX_REFUSE_RESTORE=1
STUB_CODEX_RECORD_ENOSPC=1
run_agent_versions
[[ $RUN_STATUS -eq 3 ]] ||
    fail "a signal plus an unwritable record must exit 3 (not 128+N), got $RUN_STATUS"
assert_contains "$RUN_STDERR" 'no space left on device'
# The publisher exits through os._exit(3) on this path, so the owner directory it
# names has to have been rendered and written from the handler itself.
assert_contains "$RUN_STDERR" 'pin-artifact: owner scratch retained at /'
assert_contains "$RUN_STDERR" 'pin-artifact: recovery material retained at /'
assert_not_contains "$RUN_COMBINED" 'Traceback'
verify_retained_recovery 'signal plus unwritable recovery record' "$CASE/snapshot"
echo '  ok: a signal plus an unwritable recovery record exits 3 with its recovery material'

# --- pin-side refusals never begin a publication --------------------------

new_case second-arch-fails-no-writes
codex_snapshot "$CASE/snapshot"
# Refuse the second architecture's payload: the staging step fails before any
# publication, so every committed byte survives.
rm -f "$CASE/codex-assets/assets/codex-package-aarch64-unknown-linux-musl.tar.gz"
run_agent_versions
assert_status_fail
codex_compare_snapshot "$CASE/snapshot" 'second architecture failure'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
echo '  ok: a failing second architecture writes nothing'

new_case signature-fails-no-writes
codex_snapshot "$CASE/snapshot"
STUB_CODEX_VERIFY=fail
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'cosign refused'
codex_compare_snapshot "$CASE/snapshot" 'signature failure'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
echo '  ok: a refusing signature verifier writes nothing'

new_case same-version-evidence-checked
# The upstream tag resolves to the version already pinned, but the served bytes
# are a DIFFERENT valid package: the closure must be validated and the rewrite
# refused, never re-pinned.
codex_snapshot "$CASE/snapshot"
STUB_CODEX='{"tag_name":"rust-v0.29.9"}'
printf '\n# rewritten upstream\n' >>"$CASE/codex-native.sh"
python3 "$REPO_ROOT/script/test/codex-pins.py" --write-installer-fixture \
    "$CASE/codex-assets" --release "$FIXTURE_OLD_CODEX" \
    --target x86_64-unknown-linux-musl --native-script "$CASE/codex-native.sh" >/dev/null
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'changed for the already-pinned'
codex_compare_snapshot "$CASE/snapshot" 'same-release rewrite'
echo '  ok: a same-release rewrite is refused and never re-pinned'

# --- offline --check ------------------------------------------------------

new_case check-offline
ARGS=(--check --repo-root "$FIXTURE_ROOT")
: >"$CASE/log"
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "--check must succeed: $RUN_STDERR"
assert_contains "$RUN_OUTPUT" 'Codex scope only'
assert_contains "$RUN_OUTPUT" 'offline committed-input check'
assert_contains "$RUN_OUTPUT" 'not a signature check'
assert_log_empty
cmp "$FIXTURE_ROOT/images/standard/Dockerfile" "$CASE/original"
echo '  ok: --check is offline, writes nothing and states its scope'

new_case check-refuses-mutated-evidence
printf '{\n' >"$FIXTURE_ROOT/images/tools/codex/evidence/codex-package_SHA256SUMS"
ARGS=(--check --repo-root "$FIXTURE_ROOT")
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'do not match the pin digest'
assert_log_empty

new_case check-rejects-cosign
ARGS=(--check --repo-root "$FIXTURE_ROOT" --cosign "$STUB_BIN/cosign")
run_agent_versions
[[ $RUN_STATUS -ne 0 ]] || fail '--check must reject --cosign'
assert_contains "$RUN_COMBINED" 'belongs to --write'
assert_log_empty

new_case write-rejects-missing-cosign
# The reviewed verifier is a write-mode prerequisite: an unusable path is refused
# before any upstream discovery, with no publication.
codex_snapshot "$CASE/snapshot"
ARGS=(--write --repo-root "$FIXTURE_ROOT" --cosign "$CASE/no-such-cosign")
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'is not an executable file'
assert_log_empty
codex_compare_snapshot "$CASE/snapshot" 'missing cosign'

new_case write-rejects-relative-cosign
ARGS=(--write --repo-root "$FIXTURE_ROOT" --cosign relative/cosign)
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'must be an absolute path'
assert_log_empty

new_case write-rejects-other-version-before-discovery
# The exact-version check is the writer's own and runs BEFORE any lookup: an
# incompatible verifier must stop the run without resolving a single upstream
# tag. The stub reports v2.2.0 from a sidecar beside the executable.
codex_snapshot "$CASE/snapshot"
mkdir -p "$CASE/other-cosign"
# The stub reports the reviewed version verbatim, so the incompatible verifier is
# that same executable with only the reported version changed.
sed 's/v2\.4\.1/v2.2.0/' "$STUB_BIN/cosign" >"$CASE/other-cosign/cosign"
chmod 0755 "$CASE/other-cosign/cosign"
ARGS=(--write --repo-root "$FIXTURE_ROOT" --cosign "$CASE/other-cosign/cosign")
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'reviewed verifier is v2.4.1'
assert_log_empty
codex_compare_snapshot "$CASE/snapshot" 'incompatible cosign'
echo '  ok: an incompatible verifier is refused before any upstream discovery'

# --- symlinked destinations are refused -----------------------------------

new_case symlink-evidence-dir
codex_snapshot "$CASE/snapshot"
mv "$FIXTURE_ROOT/images/tools/codex/evidence" "$CASE/evidence-moved"
ln -s "$CASE/evidence-moved" "$FIXTURE_ROOT/images/tools/codex/evidence"
run_agent_versions
[[ $RUN_STATUS -eq 1 ]] || fail "a symlinked evidence directory must exit 1, got $RUN_STATUS"
assert_contains "$RUN_COMBINED" 'symlink'
[[ ! -s "$CASE/evidence-moved/release.json.rewritten" ]] || fail 'symlink was written through'
rm -f "$FIXTURE_ROOT/images/tools/codex/evidence"
mv "$CASE/evidence-moved" "$FIXTURE_ROOT/images/tools/codex/evidence"
echo '  ok: a destination reached through a symlink is refused'

echo 'agent-versions black-box tests passed'
