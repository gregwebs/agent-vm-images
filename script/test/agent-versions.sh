#!/usr/bin/env bash
# Black-box tests for script/build/agent-versions.sh, the EXPLICIT developer
# write tool that resolves each single-slot installer layer's current upstream
# release and stages the reviewed exact default in its Dockerfile.
#
# No network and no repository writes: a fake `curl` (GitHub, the claude channel
# and Google's two agy updater manifests) and a fake `npm` (OpenCode and Copilot
# dist-tags) answer from environment variables, and every run targets a
# `--repo-root` fixture tree of Dockerfiles with one `ARG AGENT_VERSION_*=<old>`
# line each. The real `jq` is used, since the JSON extraction is part of what is
# under test.
#
# Since #97 a write run also stages and publishes the Codex payload closure (pin,
# four evidence files, Dockerfile) as ONE transaction, and since #131 it publishes
# agy's closure (pin, three evidence files, Dockerfile) as its own transaction
# FIRST. The fixture repository therefore carries committed OLD closures for both
# and replaces `script/dev/pin-codex.py` / `script/dev/pin-agy.py` with
# `fixtures/agent-versions/{codex,agy}-writer.py`, which load the REAL writers and
# swap only their seams (streaming fetch, blob verification). Everything else --
# schema, evidence hashing, archive preflight, the fixed destination lists, the
# shared Python publisher and every status -- is production code.

set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
. "$REPO_ROOT/script/test/file-mode.sh"
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
# agy is the one tool whose release is resolved from a manifest rather than a
# tag; the committed fixture closure and the served one are one minor apart.
FIXTURE_OLD_AGY='1.4.10'
DEFAULT_AGY_RELEASE='1.4.11'

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
    */manifests/linux_amd64.json) printf '%s\n' "${STUB_AGY_AMD64-}" ;;
    */manifests/linux_arm64.json) printf '%s\n' "${STUB_AGY_ARM64-}" ;;
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

# One publisher run per release, built once by the owner suite and reused: the
# fixture repository checks in the OLD closure and the transport serves the
# published bytes of whichever release a case selects.
AGY_FIXTURES="$TEST_ROOT/agy-fixtures"
agy_fixture() { # $1 = version -> directory holding source/ (closure) and assets/
    local directory="$AGY_FIXTURES/$1"
    if [[ ! -d "$directory" ]]; then
        python3 "$REPO_ROOT/script/test/pin-agy-maintenance.py" \
            --write-fixture "$directory" --version "$1" >/dev/null ||
            fail "cannot build the agy $1 fixture"
    fi
    printf '%s\n' "$directory"
}

reset_fixture() {
    FIXTURE_ROOT="$CASE/repo"
    rm -rf "$FIXTURE_ROOT"
    mkdir -p "$FIXTURE_ROOT"
    write_dockerfile codex "ARG AGENT_VERSION_CODEX=${FIXTURE_OLD_CODEX}"
    write_dockerfile opencode "ARG AGENT_VERSION_OPENCODE=${FIXTURE_OLD_OPENCODE}"
    write_dockerfile claude "ARG AGENT_VERSION_CLAUDE=${FIXTURE_OLD_CLAUDE}"
    write_dockerfile copilot "ARG AGENT_VERSION_COPILOT=${FIXTURE_OLD_COPILOT}"
    write_dockerfile agy "ARG AGENT_VERSION_AGY=${FIXTURE_OLD_AGY}"
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
    # agy: one committed OLD closure, the real writer behind the fixture adapter,
    # and the shared modules both writers load from the repository under test.
    AGY_COMMITTED="$(agy_fixture "$FIXTURE_OLD_AGY")/source/images/tools/agy"
    mkdir -p "$FIXTURE_ROOT/images/tools/agy"
    cp "$AGY_COMMITTED/pin.json" "$FIXTURE_ROOT/images/tools/agy/"
    cp -R "$AGY_COMMITTED/evidence" "$FIXTURE_ROOT/images/tools/agy/"
    cp "$REPO_ROOT/script/test/fixtures/agent-versions/agy-writer.py" \
        "$FIXTURE_ROOT/script/dev/pin-agy.py"
    cp "$FIXTURE_ROOT/images/standard/Dockerfile" "$CASE/original"
    # shellcheck disable=SC2016 # literal Dockerfile interpolation
    printf 'LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"\n' \
        >>"$FIXTURE_ROOT/images/standard/Dockerfile"
    # shellcheck disable=SC2016 # literal Dockerfile interpolation
    printf 'LABEL org.agent-vm.version.agy="${AGENT_VERSION_AGY}"\n' \
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
        STUB_CODEX_RECORD_ENOSPC STUB_CODEX_VERIFY STUB_CODEX_FAIL_KIND STUB_CODEX_STAGE_SNAPSHOT
    unset STUB_AGY_INJECT STUB_AGY_INJECT_AT STUB_AGY_REFUSE_RESTORE STUB_AGY_RECORD_FETCHES \
        STUB_AGY_FAIL_URL
    STAGED_AGY_DEFAULT=""
    SERVED_AGY_RELEASE="$FIXTURE_OLD_AGY"
    reset_fixture
    write_codex_assets "$SERVED_CODEX_RELEASE"
    serve_agy "$SERVED_AGY_RELEASE"
    ARGS=(--write --repo-root "$FIXTURE_ROOT" --cosign "$STUB_BIN/cosign")
}

# The bytes the agy transport serves for the current case, copied per case so one
# case can rewrite or delete one of them without touching the cached fixture.
serve_agy() { # $1 = served version
    local fixture
    SERVED_AGY_RELEASE=$1
    fixture="$(agy_fixture "$1")"
    rm -rf "$CASE/agy-assets"
    cp -R "$fixture/assets" "$CASE/agy-assets"
    # The bumper reads the manifests through the stub curl; serving it the same
    # documents the writer will corroborate keeps the two in step.
    STUB_AGY_AMD64="$(cat "$CASE/agy-assets/linux_amd64.json")"
    STUB_AGY_ARM64="$(cat "$CASE/agy-assets/linux_arm64.json")"
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
        "STUB_AGY_AMD64=${STUB_AGY_AMD64-}" \
        "STUB_AGY_ARM64=${STUB_AGY_ARM64-}" \
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
        "FIXTURE_CODEX_STAGE_SNAPSHOT=${STUB_CODEX_STAGE_SNAPSHOT-}" \
        "FIXTURE_AGY_WRITER=$REPO_ROOT/script/dev/pin-agy.py" \
        "FIXTURE_AGY_ASSETS=$CASE/agy-assets" \
        "FIXTURE_AGY_INJECT=${STUB_AGY_INJECT:-none}" \
        "FIXTURE_AGY_INJECT_AT=${STUB_AGY_INJECT_AT-}" \
        "FIXTURE_AGY_REFUSE_RESTORE=${STUB_AGY_REFUSE_RESTORE-}" \
        "FIXTURE_AGY_RECORD_FETCHES=${STUB_AGY_RECORD_FETCHES-}" \
        "FIXTURE_AGY_FAIL_URL=${STUB_AGY_FAIL_URL-}" \
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

# --- happy path: five staged exact ARG edits and both closures published ----

new_case happy
GITHUB_OUTPUT="$CASE/github_output"
# Every case starts from the committed agy release; a bump case selects a newer
# one, because a publication that changes nothing prints `unchanged`.
serve_agy "$DEFAULT_AGY_RELEASE"
# The codex fixture records the Dockerfile it staged, so the ordering claim is
# checked on the bytes Codex actually read.
STUB_CODEX_STAGE_SNAPSHOT="$CASE/codex-staged-dockerfile"
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "the happy path must succeed: $RUN_STDERR"
assert_contains "$RUN_OUTPUT" "agy ${FIXTURE_OLD_AGY} -> ${DEFAULT_AGY_RELEASE}"
assert_contains "$RUN_OUTPUT" "codex ${FIXTURE_OLD_CODEX} -> rust-v0.30.0"
assert_contains "$RUN_OUTPUT" "opencode ${FIXTURE_OLD_OPENCODE} -> v2.2.3"
assert_contains "$RUN_OUTPUT" "claude ${FIXTURE_OLD_CLAUDE} -> 1.0.100"
assert_contains "$RUN_OUTPUT" "copilot ${FIXTURE_OLD_COPILOT} -> 0.1.30"
assert_contains "$RUN_OUTPUT" "images/standard/Dockerfile"
assert_contains "$RUN_OUTPUT" "images/tools/agy/"
# agy was published in its own transaction, so the report says so instead of
# calling it a staged ARG edit.
assert_contains "$RUN_OUTPUT" "Published closures"
# agy moved, so the human-edited egress selection is named.
assert_contains "$RUN_OUTPUT" "selections.tsv"
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
    assert_agy_unchanged
}

# The committed agy closure and its ARG default: a refused run must leave both
# exactly as they were. The agy transaction publishes first, so this is what
# proves a refusal happened before it rather than after.
assert_agy_unchanged() {
    local relative
    for relative in pin.json evidence/release.json evidence/manifest-linux-amd64.json \
        evidence/manifest-linux-arm64.json; do
        cmp -s "$AGY_COMMITTED/$relative" "$FIXTURE_ROOT/images/tools/agy/$relative" ||
            fail "a refused run changed images/tools/agy/$relative"
    done
    grep -q "^ARG AGENT_VERSION_AGY=${FIXTURE_OLD_AGY}$" "$FIXTURE_ROOT/images/standard/Dockerfile" \
        || fail "a refused run changed the agy ARG"
}

# The agy closure a published release must leave behind: the same bytes the owner
# fixture published, file for file.
assert_agy_published() { # $1 = released version
    local relative fixture
    fixture="$(agy_fixture "$1")/source/images/tools/agy"
    for relative in pin.json evidence/release.json evidence/manifest-linux-amd64.json \
        evidence/manifest-linux-arm64.json; do
        cmp -s "$fixture/$relative" "$FIXTURE_ROOT/images/tools/agy/$relative" ||
            fail "the published agy closure differs from the reviewed $1 bytes: $relative"
    done
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
assert_arg_rewritten agy "$DEFAULT_AGY_RELEASE"
assert_not_contains "$(cat "$FIXTURE_ROOT/images/standard/Dockerfile")" "$FIXTURE_OLD_CODEX"
assert_agy_published "$DEFAULT_AGY_RELEASE"
# Ordering: the Dockerfile Codex staged already carried the released agy ARG, so
# agy published before Codex read the file rather than alongside it.
[[ -f "$CASE/codex-staged-dockerfile" ]] || fail 'the codex staging input was not recorded'
grep -q "^ARG AGENT_VERSION_AGY=${DEFAULT_AGY_RELEASE}$" "$CASE/codex-staged-dockerfile" ||
    fail "the Dockerfile Codex staged did not carry the released agy ARG: $(cat "$CASE/codex-staged-dockerfile")"
grep -q "^ARG AGENT_VERSION_AGY=${FIXTURE_OLD_AGY}$" "$CASE/original" ||
    fail 'the fixture did not start from the older agy release'
echo '  ok: agy published before the Codex staging step read the Dockerfile'

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
assert_contains "$RUN_OUTPUT" "both closures were still verified"
# agy did not move, so it is not reported as a bump, its directory is not listed
# as changed, and the human-edited egress selection is not mentioned.
assert_not_contains "$RUN_OUTPUT" "agy ${FIXTURE_OLD_AGY} ->"
assert_not_contains "$RUN_OUTPUT" "Published closures"
assert_not_contains "$RUN_OUTPUT" "images/tools/agy/"
assert_not_contains "$RUN_OUTPUT" "selections.tsv"

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

# --- agy: the updater manifest is the only release feed ---------------------
#
# Google's per-architecture manifests are agy's whole version source. The happy
# path above already ran them; these cases pin down what the bumper does with a
# document it will not act on, and that nothing is written when it refuses.

# The same manifest document on both architectures.
agy_manifest_both() { # $1 = the raw JSON value of `version`
    STUB_AGY_AMD64="$(printf '{"version":%s}' "$1")"
    STUB_AGY_ARM64="$STUB_AGY_AMD64"
}

new_case agy-manifest-is-the-feed
serve_agy "$DEFAULT_AGY_RELEASE"
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "the agy happy path must succeed: $RUN_STDERR"
assert_contains "$(cat "$CASE/log")" '/manifests/linux_amd64.json'
assert_contains "$(cat "$CASE/log")" '/manifests/linux_arm64.json'
assert_contains "$(cat "$CASE/log")" '/releases/latest'

new_case agy-manifests-disagree
# The two documents are compared on their version only: that is the field they
# share, and a disagreement means there is no single release to pin.
STUB_AGY_ARM64="$(printf '{"version":"%s"}' "$DEFAULT_AGY_RELEASE")"
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "agy manifests disagree: amd64 names '${FIXTURE_OLD_AGY}', arm64 names '${DEFAULT_AGY_RELEASE}'"
assert_all_args_unchanged

new_case agy-manifest-garbage
STUB_AGY_AMD64='<html><body>error</body></html>'
STUB_AGY_ARM64='<html><body>error</body></html>'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "agy version lookup failed"
assert_all_args_unchanged

new_case agy-manifest-null
# Both documents must carry the same refused value: a disagreement is reported
# before anything reads the version itself.
agy_manifest_both null
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "agy version empty/null"
assert_all_args_unchanged

new_case agy-manifest-empty
STUB_AGY_AMD64=''
STUB_AGY_ARM64=''
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "agy version empty/null"
assert_all_args_unchanged

new_case agy-manifest-multiline
# Two JSON documents on the stream: jq prints both values, so the bumper sees a
# version with a newline in it rather than one value.
STUB_AGY_AMD64="$(printf '{"version":"1.4.11"}\n{"version":"1.4.12"}')"
STUB_AGY_ARM64="$STUB_AGY_AMD64"
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "agy version has unexpected characters"
assert_all_args_unchanged

new_case agy-manifest-v-prefix
# The pin and the installer accept `X.Y.Z` only (artifact_pin.VERSION), so the
# bumper must not stage a `v`-prefixed value the owning hook would refuse.
agy_manifest_both '"v1.4.11"'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "agy version is not exact semver: 'v1.4.11'"
assert_all_args_unchanged

new_case agy-manifest-prerelease
agy_manifest_both '"1.4.11-rc.1"'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" "agy version is not exact semver: '1.4.11-rc.1'"
assert_all_args_unchanged

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

    # Force the BSD command shape on every host so Linux also exercises the
    # fallback. The shim delegates the mode lookup to whichever native stat is
    # installed, preserving the real mode instead of returning a fixture value.
    fake="$dir/bin/stat"
    cat >"$fake" <<'SH'
#!/bin/sh
case "${1:-}" in
    -c|--format) exit 1 ;;
    -f)
        [ "${2:-}" = '%Lp' ] || exit 1
        if "$REAL_STAT" -c '%a' "$3" >/dev/null 2>&1; then
            exec "$REAL_STAT" -c '%a' "$3"
        fi
        exec "$REAL_STAT" -f '%Lp' "$3"
        ;;
esac
exec "$REAL_STAT" "$@"
SH
    chmod 0755 "$fake"
    mode="$(PATH="$dir/bin:$PATH" REAL_STAT="$real_stat" file_mode "$dir/probe")"
    [ "$mode" = 640 ] || fail "file_mode returned '$mode' through the BSD stat interface"
    echo '  ok: file_mode reads the BSD stat interface on every host'
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

# agy's closure: the five files one agy publication replaces, exactly as its owner
# scratch holds them (`proposal/` + the repository-relative destination, which is
# what `pin-agy.py` writes and what the retained proposal is inspected by).
AGY_PROPOSAL_FILES=(
    proposal/images/tools/agy/pin.json
    proposal/images/standard/Dockerfile
    proposal/images/tools/agy/evidence/release.json
    proposal/images/tools/agy/evidence/manifest-linux-amd64.json
    proposal/images/tools/agy/evidence/manifest-linux-arm64.json
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
    echo '  ok: the codex staged proposal list covers all six destinations'
}

# agy's proposal is one literal (its owner keeps repository-relative paths under
# `proposal/`), so what has to hold is that every entry names its own destination
# and its own snapshot: a copy-pasted duplicate would silently stop being checked.
agy_proposal_self_check() {
    local proposal destinations count
    [ "${#AGY_PROPOSAL_FILES[@]}" -eq 5 ] ||
        fail "AGY_PROPOSAL_FILES has ${#AGY_PROPOSAL_FILES[@]} entries, expected 5"
    destinations="$(for proposal in "${AGY_PROPOSAL_FILES[@]}"; do
        agy_destination "$proposal"
    done | sort -u | grep -c . || true)"
    count="$(for proposal in "${AGY_PROPOSAL_FILES[@]}"; do
        recovery_snapshot_name agy "$proposal"
    done | sort -u | grep -c . || true)"
    [ "$destinations" -eq 5 ] || fail "the agy proposal list names $destinations distinct destinations"
    [ "$count" -eq 5 ] || fail "the agy proposal list names $count distinct snapshots"
    echo '  ok: the agy staged proposal list covers all five destinations'
}

# What the staged Dockerfile must be: the fixture's committed Dockerfile with the
# four single-slot ARG defaults it was staged for. Computed, never hard-coded, so a
# changed fixture default cannot silently weaken the comparison.
#
# A partial-bump case sets STAGED_AGY_DEFAULT: agy publishes first, so the file the
# Codex transaction reads is the committed one with the agy default already moved,
# and a reference that ignored that would disagree with what the run staged.
STAGED_AGY_DEFAULT=""
staged_dockerfile_expected() { # $1 = out file
    local expressions=(
        -e "s|^ARG AGENT_VERSION_CODEX=.*\$|ARG AGENT_VERSION_CODEX=${DEFAULT_CODEX_RELEASE}|"
        -e "s|^ARG AGENT_VERSION_OPENCODE=.*\$|ARG AGENT_VERSION_OPENCODE=v${DEFAULT_OPENCODE}|"
        -e "s|^ARG AGENT_VERSION_CLAUDE=.*\$|ARG AGENT_VERSION_CLAUDE=${DEFAULT_CLAUDE}|"
        -e "s|^ARG AGENT_VERSION_COPILOT=.*\$|ARG AGENT_VERSION_COPILOT=${DEFAULT_COPILOT}|"
    )
    if [ -n "$STAGED_AGY_DEFAULT" ]; then
        expressions+=(-e "s|^ARG AGENT_VERSION_AGY=.*\$|ARG AGENT_VERSION_AGY=${STAGED_AGY_DEFAULT}|")
    fi
    sed "${expressions[@]}" "$CASE/original" >"$1"
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

# agy's owner keeps its proposal under repository-relative paths inside a
# `proposal/` subdirectory, so one destination is all a snapshot name needs.
agy_destination() { # $1 = proposal/<repository path> -> <repository path>
    printf '%s\n' "${1#proposal/}"
}

# agy's expected publication, from an independent writer run against a COPY of the
# same fixture repository: the run under test never gets to publish (it is the one
# that faults), so the bytes it would have written have to come from somewhere
# else, and they have to come from the production writer rather than from a
# hand-written expectation. The copy is why the original repository is untouched.
agy_stage_reference() { # $1 = target directory
    local target=$1 status=0
    rm -rf "$target"
    mkdir -p "$target"
    cp -R "$FIXTURE_ROOT" "$target/repo"
    # The fixture repository's `script/dev/pin-codex.py` is agy's transport too, so
    # the codex adapter's own required environment has to be present here as well.
    GITHUB_ACTIONS='' \
        FIXTURE_AGY_WRITER="$REPO_ROOT/script/dev/pin-agy.py" \
        FIXTURE_AGY_ASSETS="$CASE/agy-assets" \
        FIXTURE_CODEX_WRITER="$REPO_ROOT/script/dev/pin-codex.py" \
        TMPDIR="$CASE" \
        python3 "$FIXTURE_ROOT/script/dev/pin-agy.py" --version "$SERVED_AGY_RELEASE" \
        --repo-root "$target/repo" >"$target/stdout" 2>"$target/stderr" || status=$?
    [ "$status" -eq 0 ] || {
        cat "$target/stderr" >&2
        fail "the agy reference proposal could not be published (status $status)"
    }
}

# A status-3 run promises usable recovery material, not just a status code and a
# message. Both printed absolute directories must still exist, the owner scratch
# must still hold EVERY staged proposal file with exactly the bytes staged, and
# every numbered backup must still carry the pre-publication bytes AND mode of
# exactly one committed file, under a `.complete` marker. A cleanup regression that
# deleted either directory used to leave the status/text assertions green, and an
# incomplete proposal (the reviewer's missing x86_64 bundle) used to pass as well.
#
# A profile names what the check is against: the files the owner scratch holds
# (each owner chooses its own scratch layout), the expected bytes of each of them,
# and how many numbered backups a complete set has. bash 3.2 (macOS) has no
# `declare -n`, so a profile copies its list into the RECOVERY_* globals directly.
recovery_profile() { # $1 = profile
    case "$1" in
        codex)
            RECOVERY_PROPOSAL=("${PROPOSAL_FILES[@]}")
            RECOVERY_BACKUPS=6
            RECOVERY_DOCKERFILE="$CASE/staged-dockerfile"
            RECOVERY_EVIDENCE="$CASE/codex-assets/source/evidence"
            RECOVERY_PIN="$CASE/expected-proposal"
            ;;
        agy)
            RECOVERY_PROPOSAL=("${AGY_PROPOSAL_FILES[@]}")
            RECOVERY_BACKUPS=5
            RECOVERY_DOCKERFILE="$CASE/agy-reference/repo/images/standard/Dockerfile"
            RECOVERY_EVIDENCE="$CASE/agy-reference/repo/images/tools/agy/evidence"
            RECOVERY_PIN="$CASE/agy-reference/repo/images/tools/agy"
            ;;
        *) fail "unknown recovery profile: $1" ;;
    esac
}

# The snapshot name one owner-relative proposal entry is kept under: the closure's
# repository destination, flattened.
recovery_snapshot_name() { # $1 = profile, $2 = owner-relative proposal entry
    local path=$2
    case "$1" in
        codex) path="images/tools/codex/$path" ;;
        agy) path="$(agy_destination "$path")" ;;
        *) fail "unknown recovery profile: $1" ;;
    esac
    case "$path" in
        *Dockerfile) printf 'images_standard_Dockerfile\n' ;;
        *) printf '%s\n' "$(printf '%s' "$path" | tr '/' '_')" ;;
    esac
}

proposal_list_self_check
agy_proposal_self_check

verify_retained_recovery() { # $1 label, $2 profile, $3 snapshot dir, $4 owner override, $5 publisher override
    local label=$1 profile=$2 snapshot=$3
    local publisher owner entry name matched count numbered proposal expected
    recovery_profile "$profile"
    prepare_recovery_expectations "$profile"
    publisher="${5:-$(retained_publisher_dir)}"
    owner="${4:-$(retained_owner_dir)}"
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
    for proposal in "${RECOVERY_PROPOSAL[@]}"; do
        case "$proposal" in
            *Dockerfile) expected="$RECOVERY_DOCKERFILE" ;;
            *evidence/*) expected="$RECOVERY_EVIDENCE/${proposal##*evidence/}" ;;
            *) expected="$RECOVERY_PIN/${proposal##*/}" ;;
        esac
        [ -f "$expected" ] || fail "$label: the case has no expected $proposal: $expected"
        [ -f "$owner/$proposal" ] ||
            fail "$label: the owner scratch lost $proposal: $owner"
        cmp -s "$owner/$proposal" "$expected" ||
            fail "$label: the owner scratch holds different bytes for $proposal"
    done
    [ -f "$publisher/backup/.complete" ] ||
        fail "$label: the retained backup has no .complete marker: $publisher"
    numbered="$(find "$publisher/backup" -maxdepth 1 -type f ! -name '.complete' | sort)"
    count=0
    for entry in $numbered; do count=$((count + 1)); done
    [ "$count" -eq "$RECOVERY_BACKUPS" ] ||
        fail "$label: the retained backup has $count numbered copies, expected $RECOVERY_BACKUPS: $numbered"
    for proposal in "${RECOVERY_PROPOSAL[@]}"; do
        name="$(recovery_snapshot_name "$profile" "$proposal")"
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
    echo "  ok: $label: both retained directories hold the $RECOVERY_BACKUPS numbered backups (+ .complete) and all ${#RECOVERY_PROPOSAL[@]} staged proposal files"
}

# Where each profile's expected bytes come from. codex computes the staged
# Dockerfile from the fixture's committed one; agy needs a published reference
# proposal, which its case must take BEFORE the run under test (that run leaves
# the repository in the state the proposal was supposed to replace).
prepare_recovery_expectations() { # $1 = profile
    case "$1" in
        codex) staged_dockerfile_expected "$RECOVERY_DOCKERFILE" ;;
        agy)
            [ -d "$CASE/agy-reference" ] ||
                fail "the agy profile needs a reference proposal staged before the run"
            ;;
    esac
}

# The oracle's own control. A complete copy of the owner scratch passes; deleting
# ANY ONE of the staged files must fail it. The x86_64 bundle was the one the old
# check omitted, so a proposal missing it used to pass. Nothing here touches the
# directories the run actually retained.
verify_recovery_oracle_mutants() { # $1 label, $2 profile, $3 snapshot dir
    local label=$1 profile=$2 snapshot=$3 source copy status proposal
    recovery_profile "$profile"
    source="$(retained_owner_dir)"
    copy="$CASE/owner-mutants"
    rm -rf "$copy"
    cp -R "$source" "$copy"
    status=0
    ( verify_retained_recovery "$label control" "$profile" "$snapshot" "$copy" ) \
        >/dev/null 2>&1 || status=$?
    [ "$status" -eq 0 ] ||
        fail "$label: the recovery oracle rejects a complete copy of the owner scratch"
    for proposal in "${RECOVERY_PROPOSAL[@]}"; do
        rm -rf "$copy"
        cp -R "$source" "$copy"
        rm -f "$copy/$proposal"
        status=0
        ( verify_retained_recovery "$label missing $proposal" "$profile" "$snapshot" "$copy" ) \
            >/dev/null 2>&1 || status=$?
        [ "$status" -ne 0 ] ||
            fail "$label: the recovery oracle accepts a proposal with no $proposal"
    done
    echo "  ok: $label: deleting any one of the ${#RECOVERY_PROPOSAL[@]} staged proposal files fails the recovery oracle"
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
verify_retained_recovery 'incomplete rollback' codex "$CASE/snapshot"
# ...and the oracle itself must be able to fail: one deleted proposal file, the
# x86_64 bundle included, has to be detected.
verify_recovery_oracle_mutants 'incomplete rollback' codex "$CASE/snapshot"

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
verify_retained_recovery 'unwritable recovery record' codex "$CASE/snapshot"
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
verify_retained_recovery 'signal plus unwritable recovery record' codex "$CASE/snapshot"
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

# --- agy's own transaction: ordering, independence and recovery -----------

# One file per agy closure destination, with its bytes AND its mode: a rollback
# that restores bytes through a different mode has still changed the repository.
agy_snapshot() { # $1 = directory
    mkdir -p "$1"
    local proposal destination name
    for proposal in "${AGY_PROPOSAL_FILES[@]}"; do
        destination="$(agy_destination "$proposal")"
        name="$(recovery_snapshot_name agy "$proposal")"
        cp "$FIXTURE_ROOT/$destination" "$1/$name"
        chmod "$(file_mode "$FIXTURE_ROOT/$destination")" "$1/$name"
    done
}

agy_compare_snapshot() { # $1 directory, $2 label
    local proposal destination name
    for proposal in "${AGY_PROPOSAL_FILES[@]}"; do
        destination="$(agy_destination "$proposal")"
        name="$(recovery_snapshot_name agy "$proposal")"
        cmp -s "$1/$name" "$FIXTURE_ROOT/$destination" ||
            fail "$2: $destination does not match the pre-publication snapshot"
        [ "$(file_mode "$1/$name")" = "$(file_mode "$FIXTURE_ROOT/$destination")" ] ||
            fail "$2: $destination mode changed"
    done
}

# The Dockerfile the agy transaction alone must leave: the fixture's committed one
# with the agy default released and every other single-slot default untouched.
agy_dockerfile_expected() { # $1 = out file, $2 = released version
    sed "s|^ARG AGENT_VERSION_AGY=.*\$|ARG AGENT_VERSION_AGY=${2}|" "$CASE/original" >"$1"
}

new_case agy-published-then-codex-fails
# The two transactions are independent. agy published, so its closure and its ARG
# default stay; Codex rolled back, so the Codex closure and the Codex default are
# the committed ones and the run reports failure.
serve_agy "$DEFAULT_AGY_RELEASE"
agy_snapshot "$CASE/agy-snapshot"
STUB_CODEX_INJECT=partial
STUB_CODEX_INJECT_AT="$EVIDENCE_BUNDLE"
run_agent_versions
[[ $RUN_STATUS -eq 1 ]] || fail "a failed codex publication must exit 1, got $RUN_STATUS"
assert_contains "$RUN_STDERR" 'pin-codex'
# The agy half of the bump is already committed, so the failure names it and the
# human edit the strict-egress audit will demand.
assert_contains "$RUN_STDERR" "agy ${FIXTURE_OLD_AGY} -> ${DEFAULT_AGY_RELEASE} was already published (its own transaction); codex was not"
assert_contains "$RUN_STDERR" "selections.tsv"
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
assert_agy_published "$DEFAULT_AGY_RELEASE"
assert_codex_unchanged
agy_dockerfile_expected "$CASE/agy-dockerfile" "$DEFAULT_AGY_RELEASE"
cmp -s "$CASE/agy-dockerfile" "$FIXTURE_ROOT/images/standard/Dockerfile" ||
    fail "the Dockerfile is not the agy publication with the committed Codex default: $(cat "$FIXTURE_ROOT/images/standard/Dockerfile")"
echo '  ok: a failed Codex publication leaves the already-published agy closure in place'

new_case agy-published-then-codex-rollback-fails
# The third half of the same contract, and the one an operator reads while
# recovering: agy published, then the Codex publication failed AND its rollback was
# refused, so the Codex destinations are undecided. The report must not claim Codex
# was not published — that contradicts the recovery warning printed above it — and
# it must still name the landed agy half and the egress edit.
serve_agy "$DEFAULT_AGY_RELEASE"
agy_snapshot "$CASE/agy-snapshot"
codex_snapshot "$CASE/snapshot"
# agy publishes first, so the Dockerfile the Codex transaction replaces is the
# committed one with the released agy default: the snapshot of the pre-publication
# destination bytes and the staged-proposal reference both have to carry it.
STAGED_AGY_DEFAULT="$DEFAULT_AGY_RELEASE"
agy_dockerfile_expected "$CASE/snapshot/images_standard_Dockerfile" "$DEFAULT_AGY_RELEASE"
stage_reference_proposal "$CASE/expected-proposal"
STUB_CODEX_INJECT=partial
STUB_CODEX_INJECT_AT="$EVIDENCE_BUNDLE"
STUB_CODEX_REFUSE_RESTORE=1
run_agent_versions
[[ $RUN_STATUS -eq 3 ]] || fail "an incomplete Codex rollback must exit 3, got $RUN_STATUS"
assert_contains "$RUN_STDERR" 'pin-artifact: restore incomplete'
assert_contains "$RUN_STDERR" "agy ${FIXTURE_OLD_AGY} -> ${DEFAULT_AGY_RELEASE} was already published (its own transaction)"
assert_contains "$RUN_STDERR" 'codex is undecided'
assert_contains "$RUN_STDERR" 'stop and recover before further edits, builds or commits'
# The undecided wording replaces the clean-failure claim rather than joining it.
assert_not_contains "$RUN_COMBINED" 'codex was not'
assert_contains "$RUN_STDERR" 'selections.tsv'
assert_contains "$RUN_STDERR" 'recovery material retained at /'
assert_contains "$RUN_STDERR" 'pin-artifact: owner scratch retained at /'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
assert_agy_published "$DEFAULT_AGY_RELEASE"
verify_retained_recovery 'agy published, Codex rollback incomplete' codex "$CASE/snapshot"
echo '  ok: an incomplete Codex rollback after an agy bump reports Codex as undecided and keeps the recovery material'

new_case agy-published-then-codex-staging-fails
# The other half of the same contract: Codex staging can fail (a refusing
# signature verifier) after agy has published, and the exit must still name the
# agy bump and the selections.tsv edit.
serve_agy "$DEFAULT_AGY_RELEASE"
agy_snapshot "$CASE/agy-snapshot"
STUB_CODEX_VERIFY=fail
run_agent_versions
assert_status_fail
assert_contains "$RUN_STDERR" 'cosign refused'
assert_contains "$RUN_STDERR" "agy ${FIXTURE_OLD_AGY} -> ${DEFAULT_AGY_RELEASE} was already published (its own transaction); codex was not"
assert_contains "$RUN_STDERR" "selections.tsv"
assert_agy_published "$DEFAULT_AGY_RELEASE"
assert_codex_unchanged
echo '  ok: a failed Codex staging names the already-published agy bump and the egress edit'

new_case agy-publication-undecided
# Status 3 at the owner level, driven through the bumper: the Codex transaction
# must not even start, and both directories the agy writer printed must survive the
# bumper's own EXIT trap with the complete proposal, backups and modes.
serve_agy "$DEFAULT_AGY_RELEASE"
agy_snapshot "$CASE/agy-snapshot"
agy_stage_reference "$CASE/agy-reference"
STUB_CODEX_STAGE_SNAPSHOT="$CASE/codex-staged-dockerfile"
STUB_AGY_INJECT=partial
STUB_AGY_INJECT_AT=manifest-linux-arm64.json
STUB_AGY_REFUSE_RESTORE=1
run_agent_versions
[[ $RUN_STATUS -eq 3 ]] || fail "an undecided agy publication must exit 3, got $RUN_STATUS"
assert_contains "$RUN_STDERR" 'agy publication is undecided'
assert_contains "$RUN_STDERR" 'pin-agy: owner scratch retained at /'
assert_contains "$RUN_STDERR" 'recovery material retained at /'
assert_not_contains "$RUN_COMBINED" 'Traceback'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
[[ ! -e "$CASE/codex-staged-dockerfile" ]] ||
    fail 'the Codex transaction started after an undecided agy publication'
verify_retained_recovery 'agy publication undecided' agy "$CASE/agy-snapshot"
verify_recovery_oracle_mutants 'agy publication undecided' agy "$CASE/agy-snapshot"
echo '  ok: an undecided agy publication retains both directories and stops before Codex'

new_case agy-writer-failure-writes-nothing
# The agy transaction is the first thing that can write, and its failure leaves the
# whole fixture repository untouched: no closure byte, no ARG, no staging.
serve_agy "$DEFAULT_AGY_RELEASE"
agy_snapshot "$CASE/agy-snapshot"
STUB_CODEX_STAGE_SNAPSHOT="$CASE/codex-staged-dockerfile"
STUB_AGY_FAIL_URL='https://api.github.com/repos/google-antigravity/antigravity-cli/releases/tags/1.4.11'
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'cannot fetch'
assert_not_contains "$RUN_OUTPUT" 'Staged version bumps:'
[[ ! -e "$CASE/codex-staged-dockerfile" ]] || fail 'Codex was staged after an agy refusal'
agy_compare_snapshot "$CASE/agy-snapshot" 'agy fetch failure'
assert_codex_unchanged
echo '  ok: an agy fetch failure writes nothing and never reaches the Codex stage'

# --- offline --check ------------------------------------------------------

new_case check-offline
ARGS=(--check --repo-root "$FIXTURE_ROOT")
: >"$CASE/log"
run_agent_versions
[[ $RUN_STATUS -eq 0 ]] || fail "--check must succeed: $RUN_STDERR"
assert_contains "$RUN_OUTPUT" 'Codex and agy scope only'
assert_contains "$RUN_OUTPUT" 'offline committed-input check'
assert_contains "$RUN_OUTPUT" 'not a fresh corroboration'
assert_contains "$RUN_OUTPUT" 'not a signature check'
assert_log_empty
cmp "$FIXTURE_ROOT/images/standard/Dockerfile" "$CASE/original"
echo '  ok: --check is offline, covers both closures, writes nothing and states its scope'

new_case check-refuses-mutated-agy-evidence
# The agy check must actually run: a mutated evidence byte is its own refusal,
# reported before the codex check ever starts.
printf '{\n' >"$FIXTURE_ROOT/images/tools/agy/evidence/manifest-linux-arm64.json"
ARGS=(--check --repo-root "$FIXTURE_ROOT")
run_agent_versions
assert_status_fail
assert_contains "$RUN_COMBINED" 'manifest-linux-arm64.json is '
assert_contains "$RUN_COMBINED" 'the pin says'
assert_not_contains "$RUN_COMBINED" 'signature check'
assert_log_empty

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
