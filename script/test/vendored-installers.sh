#!/usr/bin/env bash
# SP7 black-box execution tier for the vendored shipped installers (#227).
#
# The owning-hook suites (script/test/{codex,claude,opencode}-installer.sh) fake
# the vendored installer and record its inputs; that proves the envelope, not
# the installer. THIS suite executes the REAL patched vendored scripts
# (images/tools/{codex,opencode,claude}/vendor/install.sh) under the owning
# hooks through the shared run-install.sh envelope, with:
#
#   * a fake curl on PATH that serves exact, synthetic release metadata /
#     checksum manifests and genuinely-shaped synthetic archives whose sha256
#     sums are real, and DENIES every moving-channel (`latest`/dist-tag) URL;
#   * a fake uname that drives each Linux architecture branch;
#   * fixture "native" commands that succeed, fail, exit 75 or hang.
#
# It covers, for both Linux architecture branches where applicable: a clean
# install (parsing + hash verification + extraction + linking + native run), a
# checksum mismatch BEFORE the command is published, malformed metadata, a
# denied `latest` URL (via a mutation), transport receipt propagation and
# clearing, and native nonzero / exit-75 / hang under soft input (all hard).
#
# No Docker, no network. Boot-free; wired into script/test/contracts.sh.

set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
CONTRACT="$REPO_ROOT/images/recipe-contract"
RUN_INSTALL="$CONTRACT/run-install.sh"
CODEX_VENDOR="$REPO_ROOT/images/tools/codex/vendor"
OPENCODE_VENDOR="$REPO_ROOT/images/tools/opencode/vendor"
CLAUDE_VENDOR="$REPO_ROOT/images/tools/claude/vendor"
WATCHDOG="$REPO_ROOT/script/test/host-watchdog.py"

# Every bound the vendored installers request is capped to this inside the fake
# timeout, so a hung native fixture costs seconds instead of minutes.
FAKE_TIMEOUT_CAP_SECONDS=3
# Mirrors run-report.sh's --kill-after: a TERM-ignoring probe may outlive the
# cap by this much.
PROBE_KILL_GRACE_SECONDS=5
# Capped probes the codex hang path may wait out, both in
# images/tools/codex/vendor/install.sh: `version_from_binary bin/codex`, then
# its `codex` symlink fallback. A third probe fails the case below rather than
# passing silently behind a spare slot.
HANG_PROBE_ALLOWANCE=2
HANG_WATCHDOG_HEADROOM=2
# A hung native fixture sleeps this long; only an UNBOUNDED probe waits it out.
FIXTURE_HANG_SECONDS=300
# External backstop for the codex hang case. 8 s left under 1 s over two capped
# probes and fired on a loaded host (#37). It is an empirical margin, not a
# wall-clock guarantee; the guards below pin it between the capped-probe
# allowance and the fixture hang. The guards read the argv actually executed.
HANG_WATCHDOG_SECONDS=60
HANG_WATCHDOG=(python3 "$WATCHDOG" "$HANG_WATCHDOG_SECONDS")

TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/vendored-installers.XXXXXX")"
chmod 0755 "$TEST_ROOT"
trap 'rm -rf "$TEST_ROOT"' EXIT

REAL_TIMEOUT="$(command -v timeout || true)"
[ -n "$REAL_TIMEOUT" ] || fail "timeout is required (macOS: brew install coreutils)"
REAL_TAR="$(command -v tar || true)"
[ -n "$REAL_TAR" ] || fail "tar is required"
PYTHON=$(command -v python3 || true)
[ -n "$PYTHON" ] || fail "python3 is required"

CASE=""
CODEX_TARGET=""
OPENCODE_ARCH=""
RUN_OUT=""
RUN_RC=0
CURL_TRANSPORT_CODE=""
SOFT=""

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

note() { echo "  $*"; }

hang_wait_allowance=$((HANG_PROBE_ALLOWANCE * (FAKE_TIMEOUT_CAP_SECONDS + PROBE_KILL_GRACE_SECONDS)))
[ "${HANG_WATCHDOG[2]}" -ge $((HANG_WATCHDOG_HEADROOM * hang_wait_allowance)) ] ||
    fail "hang watchdog ${HANG_WATCHDOG[2]}s is under ${HANG_WATCHDOG_HEADROOM}x the ${hang_wait_allowance}s capped-probe allowance"
[ "${HANG_WATCHDOG[2]}" -lt "$FIXTURE_HANG_SECONDS" ] ||
    fail "hang watchdog ${HANG_WATCHDOG[2]}s must be below the ${FIXTURE_HANG_SECONDS}s fixture hang, or an unbounded probe cannot trip it"

sha256_of() { # host-side hash, the same value the fake sha256sum must produce
    if command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        openssl dgst -sha256 "$1" | awk '{print $NF}'
    fi
}

# --- shared fake executables -------------------------------------------------
SHARED="$TEST_ROOT/shared"
mkdir -p "$SHARED"

# Fake curl. Parses `-o OUTPUT` and the URL, logs every request, denies any
# moving-channel URL and any URL not explicitly routed, and otherwise copies a
# rooted fixture file to OUTPUT. A transport code (FAKE_CURL_TRANSPORT_CODE) is
# returned for every request to drive the classified 75 path.
cat >"$SHARED/curl" <<'SH'
#!/bin/sh
# The request stream (`FAKE_CURL_LOG`) records every attempted URL exactly once,
# in order, whatever the outcome. Denials go to their own stream
# (`FAKE_CURL_DENY_LOG`) so an assertion on the ordered URL list can never be
# satisfied or defeated by a diagnostic line.
#
# Routing is by basename (the historical shape) or, for codex, by the ENTIRE
# canonical URL (`FAKE_CURL_ROUTE_MODE=full-url`), which requires exact string
# equality and has no basename fallback.
out=""
url=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) out="${2:-}"; shift 2 ;;
        -*) shift ;;
        *) url="$1"; shift ;;
    esac
done
[ -n "${FAKE_CURL_LOG:-}" ] && printf '%s\n' "$url" >>"$FAKE_CURL_LOG"
deny() {
    [ -n "${FAKE_CURL_DENY_LOG:-}" ] && printf 'DENIED %s\n' "$url" >>"$FAKE_CURL_DENY_LOG"
    exit 22
}
# Any moving channel is an HTTP error. The vendored installers must select an
# exact version; a `latest`/dist-tag request is never an acceptable build step.
case "$url" in
    *latest* | */dist-tags/*) deny ;;
esac
if [ -n "${FAKE_CURL_TRANSPORT_CODE:-}" ]; then
    exit "$FAKE_CURL_TRANSPORT_CODE"
fi
case "${FAKE_CURL_ROUTE_MODE:-basename}" in
    full-url) key="$url" ;;
    basename) key="${url##*/}" ;;
    *)
        echo "fake curl: unknown routing mode ${FAKE_CURL_ROUTE_MODE}" >&2
        exit 90
        ;;
esac
file="$(awk -F '\t' -v b="$key" '$1 == b { print $2; exit }' "${FAKE_CURL_ROUTES:-/dev/null}")"
if [ -z "$file" ] || [ ! -f "$file" ]; then
    deny
fi
cp "$file" "$out"
SH

# Fake sha256sum: GNU-style "<hex>  <path>" regardless of host tooling, so the
# vendored scripts' own hash checks and this suite's expected hashes agree.
cat >"$SHARED/sha256sum" <<'SH'
#!/bin/sh
if [ -x /usr/bin/sha256sum ]; then exec /usr/bin/sha256sum "$@"; fi
for file in "$@"; do
    if command -v shasum >/dev/null 2>&1; then
        printf '%s  %s\n' "$(shasum -a 256 "$file" | awk '{print $1}')" "$file"
    else
        printf '%s  %s\n' "$(openssl dgst -sha256 "$file" | awk '{print $NF}')" "$file"
    fi
done
SH

# Fake timeout: records the bound the vendored script asked for, then enforces a
# small test cap so a hung native is bounded quickly. The requested bound is
# the script's own ("300"); the cap is this suite's. The shipped callers pass a
# leading --kill-after= option (TERM then SIGKILL); keep it, but the recorded
# value the suite asserts on is the duration.
cat >"$SHARED/timeout" <<'SH'
#!/bin/sh
kill_after=
case "${1:-}" in
    --kill-after=*) kill_after=$1; shift ;;
esac
bound="${1:-0}"
# Provenance marker: the bound this fake timeout was asked for, captured before
# the cap below, exported so the exec'd native inherits it (through this exec
# and REAL_TIMEOUT). A native the installer calls without a bound never sees it,
# which is what the hang case reads to prove an unbounded probe.
FAKE_TIMEOUT_BOUND="$bound"
export FAKE_TIMEOUT_BOUND
if [ -n "${FAKE_TIMEOUT_LOG:-}" ]; then
    printf '%s\n' "$bound" >>"$FAKE_TIMEOUT_LOG"
fi
shift
cap="${FAKE_TIMEOUT_CAP:?fake timeout needs FAKE_TIMEOUT_CAP}"
case "$bound" in
    '' | *[!0-9]*) ;;
    *) [ "$bound" -gt "$cap" ] && bound="$cap" ;;
esac
if [ -n "$kill_after" ]; then
    exec "$REAL_TIMEOUT" "$kill_after" "$bound" "$@"
fi
exec "$REAL_TIMEOUT" "$bound" "$@"
SH

# Fake tar: records only extraction (the step a pin/hash/preflight refusal must
# never reach) and otherwise defers to the real one.
cat >"$SHARED/tar" <<'SH'
#!/bin/sh
case "${1:-}" in
    -x* | --extract*) [ -n "${FAKE_TAR_LOG:-}" ] && printf 'EXTRACT %s\n' "$*" >>"$FAKE_TAR_LOG" ;;
esac
exec "$REAL_TAR" "$@"
SH

chmod 0755 "$SHARED/curl" "$SHARED/sha256sum" "$SHARED/timeout" "$SHARED/tar"

route() { # $1 route key (basename or full URL), $2 fixture file
    printf '%s\t%s\n' "$1" "$2" >>"$CASE/routes"
}

new_case() { # $1 name, $2 uname arch
    CASE="$TEST_ROOT/$1"
    rm -rf "$CASE"
    mkdir -p "$CASE/bin" "$CASE/prefix" "$CASE/status" "$CASE/assets" "$CASE/link"
    chmod 0755 "$CASE" "$CASE/bin" "$CASE/prefix" "$CASE/status" "$CASE/assets" "$CASE/link"
    cp "$SHARED/curl" "$SHARED/sha256sum" "$SHARED/timeout" "$SHARED/tar" "$CASE/bin/"
    mkdir -p "$CASE/tmp" "$CASE/pybin"
    chmod 0755 "$CASE/tmp" "$CASE/pybin"
    # Only python3, so the fixture PATH cannot pick up the host's own tools.
    ln -sf "$PYTHON" "$CASE/pybin/python3"
    # shellcheck disable=SC2016  # the generated uname script must keep ${1:-} literal
    printf '#!/bin/sh\ncase "${1:-}" in\n  -s) echo Linux ;;\n  -m) echo %s ;;\n  *) echo Linux ;;\nesac\n' \
        "$2" >"$CASE/bin/uname"
    chmod 0755 "$CASE/bin/uname"
    : >"$CASE/curl.log"
    : >"$CASE/deny.log"
    : >"$CASE/routes"
    : >"$CASE/timeout.log"
    : >"$CASE/tar.log"
    : >"$CASE/native.log"
}

# --- fixture native commands -------------------------------------------------
render_codex_native() { # $1 out, $2 version, $3 exit, $4 hang
    local body
    body="$(cat <<'SH'
#!/bin/sh
printf 'codex-cli @VERSION@\n'
if [ -n "${AGENT_VM_TRANSPORT_RECEIPT:-}" ]; then
  printf 'transport download 6\n' >"$AGENT_VM_TRANSPORT_RECEIPT"
fi
if [ -n "${FIXTURE_PID_LOG:-}" ]; then
  printf '%s %s\n' "$$" "${FAKE_TIMEOUT_BOUND:-}" >>"$FIXTURE_PID_LOG"
fi
@HANG@exit @EXIT@
SH
)"
    body="${body//@VERSION@/$2}"
    body="${body//@EXIT@/$3}"
    if [ "$4" = 1 ]; then
        body="${body//@HANG@/exec sleep $FIXTURE_HANG_SECONDS$'\n'}"
    else
        body="${body//@HANG@/}"
    fi
    printf '%s\n' "$body" >"$1"
    chmod 0755 "$1"
}

render_claude_native() { # $1 out, $2 version, $3 exit, $4 hang
    local body
    body="$(cat <<'SH'
#!/bin/sh
printf 'claude-native-ran\n' >>"$HOME/native-ran"
# Record and enforce the EXACT native dispatch: the production installer must
# invoke `install <requested-version>`, and must strip AGENT_VM_TRANSPORT_RECEIPT
# from the native environment. A dropped target, a wrong target, or a leaked
# receipt env fails hard here (review F3).
{
  printf 'argc=%s\n' "$#"
  for a in "$@"; do printf 'arg=%s\n' "$a"; done
} >"$HOME/native-argv"
if [ "$#" -ne 2 ] || [ "$1" != install ] || [ "$2" != "@VERSION@" ]; then
  printf 'claude-native: unexpected argv (%s): %s\n' "$#" "$*" >&2
  exit 90
fi
if [ -n "${AGENT_VM_TRANSPORT_RECEIPT:-}" ]; then
  printf 'claude-native: AGENT_VM_TRANSPORT_RECEIPT must not reach the native\n' >&2
  exit 91
fi
@HANG@if [ "@EXIT@" = 0 ]; then
  mkdir -p "$HOME/.local/bin"
  printf '#!/bin/sh\nprintf "@VERSION@ (Claude Code)\\n"\n' >"$HOME/.local/bin/claude"
  chmod 0755 "$HOME/.local/bin/claude"
fi
exit @EXIT@
SH
)"
    body="${body//@VERSION@/$2}"
    body="${body//@EXIT@/$3}"
    if [ "$4" = 1 ]; then
        body="${body//@HANG@/sleep $FIXTURE_HANG_SECONDS$'\n'}"
    else
        body="${body//@HANG@/}"
    fi
    printf '%s\n' "$body" >"$1"
    chmod 0755 "$1"
}

# --- fixture archives / metadata ---------------------------------------------
build_codex_archive() { # $1 out, $2 version, $3 target, $4 exit, $5 hang
    local stage="$CASE/assets/stage-codex-$RANDOM"
    mkdir -p "$stage/bin" "$stage/codex-path" "$stage/codex-resources"
    render_codex_native "$stage/bin/codex" "$2" "$4" "$5"
    printf '#!/bin/sh\nexit 0\n' >"$stage/bin/codex-code-mode-host"
    printf '#!/bin/sh\nexit 0\n' >"$stage/codex-path/rg"
    printf '#!/bin/sh\nexit 0\n' >"$stage/codex-resources/bwrap"
    chmod 0755 "$stage/bin/codex-code-mode-host" "$stage/codex-path/rg" "$stage/codex-resources/bwrap"
    printf '{}\n' >"$stage/codex-package.json"
    (cd "$stage" && tar -czf "$1" .)
    rm -rf "$stage"
}

make_codex_case() { # $1 name, $2 arch, $3 version, $4 native exit, $5 hang, $6 corrupt
    new_case "$1" "$2"
    case "$2" in
        x86_64) CODEX_TARGET="x86_64-unknown-linux-musl" ;;
        aarch64) CODEX_TARGET="aarch64-unknown-linux-musl" ;;
        *) fail "unknown arch $2" ;;
    esac
    local asset="codex-package-$CODEX_TARGET.tar.gz"
    build_codex_archive "$CASE/assets/archive.tgz" "$3" "$CODEX_TARGET" "$4" "$5"
    # $6: 0 = intact, 1 = invalid bytes served, 2 = a VALID archive whose
    # manifest digest differs.
    local ahash served sum_hash
    ahash="$(sha256_of "$CASE/assets/archive.tgz")"
    sum_hash="$ahash"
    served="$CASE/assets/archive.tgz"
    case "$6" in
        1)
            printf 'this is not the archive\n' >"$CASE/assets/corrupt.tgz"
            served="$CASE/assets/corrupt.tgz" ;;
        2)
            sum_hash="0000000000000000000000000000000000000000000000000000000000000000" ;;
    esac
    printf '%s  %s\n' "$sum_hash" "$asset" >"$CASE/assets/codex-package_SHA256SUMS"
    local shash
    shash="$(sha256_of "$CASE/assets/codex-package_SHA256SUMS")"
    printf '{"tag_name":"rust-v%s","assets":[{"name":"%s","digest":"sha256:%s"},{"name":"codex-package_SHA256SUMS","digest":"sha256:%s"}]}\n' \
        "$3" "$asset" "$ahash" "$shash" >"$CASE/assets/meta.json"
    route "rust-v$3" "$CASE/assets/meta.json"
    route "$asset" "$served"
    route "codex-package_SHA256SUMS" "$CASE/assets/codex-package_SHA256SUMS"
}

make_opencode_case() { # $1 name, $2 arch, $3 version
    new_case "$1" "$2"
    case "$2" in
        x86_64) OPENCODE_ARCH=x64 ;;
        aarch64) OPENCODE_ARCH=arm64 ;;
        *) fail "unknown arch $2" ;;
    esac
    local stage="$CASE/assets/stage-oc-$RANDOM" body
    mkdir -p "$stage/package/bin"
    body="$(cat <<'SH'
#!/bin/sh
printf '%s\n' "opencode v@VERSION@"
SH
)"
    body="${body//@VERSION@/$3}"
    printf '%s\n' "$body" >"$stage/package/bin/opencode"
    chmod 0755 "$stage/package/bin/opencode"
    (cd "$stage" && tar -czf "$CASE/assets/opencode.tgz" package)
    rm -rf "$stage"
    # Route every filename the installer may compute on a glibc or musl host,
    # with or without the AVX2-baseline suffix, to the same synthetic archive.
    local variant
    for variant in \
        "cli-linux-$OPENCODE_ARCH" \
        "cli-linux-$OPENCODE_ARCH-baseline" \
        "cli-linux-$OPENCODE_ARCH-musl" \
        "cli-linux-$OPENCODE_ARCH-baseline-musl"; do
        route "$variant-$3.tgz" "$CASE/assets/opencode.tgz"
    done
}

make_claude_case() { # $1 name, $2 arch, $3 version, $4 native exit, $5 hang, $6 corrupt, $7 malformed
    new_case "$1" "$2"
    local platform hash
    case "$2" in
        x86_64) platform=linux-x64 ;;
        aarch64) platform=linux-arm64 ;;
        *) fail "unknown arch $2" ;;
    esac
    render_claude_native "$CASE/assets/claude-native" "$3" "$4" "$5"
    hash="$(sha256_of "$CASE/assets/claude-native")"
    served="$CASE/assets/claude-native"
    # $6: 0 = intact, 1 = valid asset with a mismatched manifest digest,
    # 2 = invalid bytes served with the real digest.
    case "$6" in
        1) hash="0000000000000000000000000000000000000000000000000000000000000000" ;;
        2)
            printf 'not a real binary\n' >"$CASE/assets/claude-corrupt"
            served="$CASE/assets/claude-corrupt" ;;
    esac
    if [ "$7" = 1 ]; then
        printf '{ this is not json\n' >"$CASE/assets/manifest.json"
    else
        printf '{"platforms":{"%s":{"checksum":"%s","size":1234}}}\n' "$platform" "$hash" \
            >"$CASE/assets/manifest.json"
    fi
    route "manifest.json" "$CASE/assets/manifest.json"
    route "claude" "$served"
}

# --- envelope runners --------------------------------------------------------
# All run the real run-install.sh envelope with the owning hook and the real
# vendored installer. The fake curl/sha256sum/uname/timeout live in $CASE/bin.
run_codex() { # $1 slot, $2 vendor dir
    local -a e=(
        "PATH=$CASE/bin:$CASE/pybin:/usr/bin:/bin"
        "HOME=$CASE"
        "TMPDIR=$CASE/tmp"
        "AGENT_VM_CONTRACT_DIR=$CONTRACT"
        "AGENT_VM_VENDOR_DIR=$2"
        "AGENT_VM_CODEX_SOURCE_DIR=$CASE/source"
        "AGENT_VM_CODEX_PREFIX=$CASE/prefix"
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status"
        "AGENT_VM_VERSION_CODEX=$1"
        "FAKE_CURL_LOG=$CASE/curl.log"
        "FAKE_CURL_DENY_LOG=$CASE/deny.log"
        "FAKE_CURL_ROUTES=$CASE/routes"
        "FAKE_CURL_ROUTE_MODE=full-url"
        "FAKE_TIMEOUT_LOG=$CASE/timeout.log"
        "FAKE_TAR_LOG=$CASE/tar.log"
        "REAL_TAR=$REAL_TAR"
        "FIXTURE_NATIVE_LOG=$CASE/native.log"
        "FIXTURE_PID_LOG=$CASE/hang.pids"
        "REAL_TIMEOUT=$REAL_TIMEOUT"
        "FAKE_TIMEOUT_CAP=$FAKE_TIMEOUT_CAP_SECONDS"
    )
    if [ -n "$CURL_TRANSPORT_CODE" ]; then e+=("FAKE_CURL_TRANSPORT_CODE=$CURL_TRANSPORT_CODE"); fi
    if [ -n "$SOFT" ]; then e+=("AGENT_INSTALL_SOFT_FAIL=$SOFT"); fi
    set +e
    RUN_OUT="$(env -i "${e[@]}" sh "$RUN_INSTALL" codex "$CASE/status/codex" sh "$REPO_ROOT/images/tools/codex/install-codex.sh" 2>&1)"
    RUN_RC=$?
    set -e
}

run_opencode() { # $1 slot, $2 vendor dir
    mkdir -p "$CASE/tmp"
    local -a e=(
        "PATH=$CASE/bin:/usr/bin:/bin"
        "HOME=$CASE"
        "TMPDIR=$CASE/tmp"
        "AGENT_VM_CONTRACT_DIR=$CONTRACT"
        "AGENT_VM_VENDOR_DIR=$2"
        "AGENT_VM_OPENCODE_PREFIX=$CASE/prefix"
        "AGENT_VM_OPENCODE_LINK=$CASE/link/opencode"
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status"
        "AGENT_VM_VERSION_OPENCODE=$1"
        "FAKE_CURL_LOG=$CASE/curl.log"
        "FAKE_CURL_DENY_LOG=$CASE/deny.log"
        "FAKE_CURL_ROUTES=$CASE/routes"
        "FAKE_TAR_LOG=$CASE/tar.log"
        "REAL_TAR=$REAL_TAR"
    )
    if [ -n "$CURL_TRANSPORT_CODE" ]; then e+=("FAKE_CURL_TRANSPORT_CODE=$CURL_TRANSPORT_CODE"); fi
    if [ -n "$SOFT" ]; then e+=("AGENT_INSTALL_SOFT_FAIL=$SOFT"); fi
    set +e
    RUN_OUT="$(env -i "${e[@]}" sh "$RUN_INSTALL" opencode "$CASE/status/opencode" sh "$REPO_ROOT/images/tools/opencode/install-opencode.sh" 2>&1)"
    RUN_RC=$?
    set -e
}

run_claude() { # $1 slot, $2 vendor dir
    local -a e=(
        "PATH=$CASE/bin:/usr/bin:/bin"
        "HOME=$CASE"
        "AGENT_VM_CONTRACT_DIR=$CONTRACT"
        "AGENT_VM_VENDOR_DIR=$2"
        "AGENT_VM_CLAUDE_PREFIX=$CASE/prefix"
        "AGENT_VM_FIXTURE_ROOT=$CASE"
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status"
        "AGENT_VM_VERSION_CLAUDE=$1"
        "FAKE_CURL_LOG=$CASE/curl.log"
        "FAKE_CURL_DENY_LOG=$CASE/deny.log"
        "FAKE_CURL_ROUTES=$CASE/routes"
        "FAKE_TIMEOUT_LOG=$CASE/timeout.log"
        "FAKE_TAR_LOG=$CASE/tar.log"
        "REAL_TAR=$REAL_TAR"
        "REAL_TIMEOUT=$REAL_TIMEOUT"
        "FAKE_TIMEOUT_CAP=$FAKE_TIMEOUT_CAP_SECONDS"
    )
    if [ -n "$CURL_TRANSPORT_CODE" ]; then e+=("FAKE_CURL_TRANSPORT_CODE=$CURL_TRANSPORT_CODE"); fi
    if [ -n "$SOFT" ]; then e+=("AGENT_INSTALL_SOFT_FAIL=$SOFT"); fi
    set +e
    RUN_OUT="$(env -i "${e[@]}" sh "$RUN_INSTALL" claude "$CASE/status/claude" sh "$REPO_ROOT/images/tools/claude/install-claude.sh" 2>&1)"
    RUN_RC=$?
    set -e
}

status_of() { # $1 tool
    cat "$CASE/status/$1" 2>/dev/null || echo MISSING
}

expect_ok() { [ "$RUN_RC" -eq 0 ] || fail "$1: expected success, got rc=$RUN_RC: $RUN_OUT"; }
expect_hard() { [ "$RUN_RC" -ne 0 ] || fail "$1: expected a hard failure, got success: $RUN_OUT"; }
expect_no_denied() {
    [ ! -s "$CASE/deny.log" ] ||
        fail "$1: a request was denied: $(cat "$CASE/deny.log")"
}
expect_denied() {
    [ -s "$CASE/deny.log" ] ||
        fail "$1: no denied URL was logged: $(cat "$CASE/curl.log")"
    grep -q 'latest' "$CASE/deny.log" ||
        fail "$1: the mutation never requested a latest URL: $(cat "$CASE/deny.log")"
}

mutate_vendor() { # $1 src dir, $2 out dir, $3 sed expr
    mkdir -p "$2"
    chmod 0755 "$2"
    sed "$3" "$1/install.sh" >"$2/install.sh"
    chmod 0644 "$2/install.sh"
}

# =============================================================================
# codex
# =============================================================================
# The vendored installer no longer resolves a release: the committed pin is the
# only selection authority, so every Codex assertion below is an exact ordered
# request stream whose URLs are written in this file (never taken from production
# code), plus an operation log proving whether extraction and native execution
# happened at all.
echo "== codex (real vendored install.sh) =="

codex_urls() { # $1 uname arch, $2 release -> CODEX_TARGET API_URL PAYLOAD_URL ...
    local v="${2#rust-v}"
    case "$1" in
        x86_64) CODEX_TARGET="x86_64-unknown-linux-musl" ;;
        aarch64) CODEX_TARGET="aarch64-unknown-linux-musl" ;;
        *) fail "unknown arch $1" ;;
    esac
    API_URL="https://api.github.com/repos/openai/codex/releases/tags/rust-v$v"
    PAYLOAD_URL="https://github.com/openai/codex/releases/download/rust-v$v/codex-package-$CODEX_TARGET.tar.gz"
    BUNDLE_URL="$PAYLOAD_URL.sigstore"
    SUMS_URL="https://github.com/openai/codex/releases/download/rust-v$v/codex-package_SHA256SUMS"
}

codex_native_body() { # $1 out, $2 version, $3 exit, $4 hang
    local body
    body="$(cat <<'SH'
printf 'codex-cli @VERSION@\n'
if [ -n "${FIXTURE_NATIVE_LOG:-}" ]; then echo "NATIVE $$" >>"$FIXTURE_NATIVE_LOG"; fi
if [ -n "${FIXTURE_PID_LOG:-}" ]; then printf '%s %s\n' "$$" "${FAKE_TIMEOUT_BOUND:-}" >>"$FIXTURE_PID_LOG"; fi
@HANG@exit @EXIT@
SH
)"
    body="${body//@VERSION@/$2}"
    body="${body//@EXIT@/$3}"
    if [ "$4" = 1 ]; then
        body="${body//@HANG@/exec sleep $FIXTURE_HANG_SECONDS$'\n'}"
    else
        body="${body//@HANG@/}"
    fi
    printf '%s\n' "$body" >"$1"
}

codex_fixture() { # $1 out dir, $2 release, $3 target, $4 native body, rest: extra
    local out=$1 release=$2 target=$3 body=$4
    shift 4
    python3 "$REPO_ROOT/script/test/codex-pins.py" --write-installer-fixture "$out" \
        --release "$release" --target "$target" --native-script "$body" "$@" >/dev/null
}

new_codex_case() { # $1 name, $2 uname arch, $3 release, $4 native exit, $5 hang
    new_case "$1" "$2"
    codex_urls "$2" "$3"
    codex_native_body "$CASE/native-body.sh" "${3#rust-v}" "$4" "$5"
    codex_fixture "$CASE/fx" "$3" "$CODEX_TARGET" "$CASE/native-body.sh"
    cp -R "$CASE/fx/source" "$CASE/source"
    route "$PAYLOAD_URL" "$CASE/fx/archive.tgz"
}

expect_stream() { # $1 label, rest: the exact attempted URLs, in order
    local label=$1
    shift
    local want="$CASE/want.log"
    : >"$want"
    local url
    for url in "$@"; do
        printf '%s\n' "$url" >>"$want"
    done
    cmp -s "$want" "$CASE/curl.log" ||
        fail "$label: request stream mismatch
  want: $(tr '\n' ' ' <"$want")
  got:  $(tr '\n' ' ' <"$CASE/curl.log" 2>/dev/null)"
}

expect_no_extraction() {
    [ ! -s "$CASE/tar.log" ] || fail "$1: the archive was extracted: $(cat "$CASE/tar.log")"
}

expect_no_native() {
    [ ! -s "$CASE/native.log" ] ||
        fail "$1: the native entrypoint ran: $(cat "$CASE/native.log")"
}

for arch in x86_64 aarch64; do
    V=0.159.3

    # --- clean install ------------------------------------------------------
    new_codex_case "codex-$arch-ok" "$arch" "rust-v$V" 0 0
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_ok "codex $arch clean install"
    expect_stream "codex $arch clean install" "$PAYLOAD_URL"
    [ "$(grep -c '^EXTRACT ' "$CASE/tar.log" || true)" = 1 ] ||
        fail "codex $arch: expected exactly one extraction: $(cat "$CASE/tar.log")"
    [ "$(grep -c '^NATIVE ' "$CASE/native.log" || true)" -ge 1 ] ||
        fail "codex $arch: the native entrypoint never ran"
    "$CASE/prefix/.local/bin/codex" --version >"$CASE/out" 2>&1 ||
        fail "codex $arch: published command failed"
    [ "$(sed -n '1p' "$CASE/out")" = "codex-cli $V" ] ||
        fail "codex $arch: report '$(sed -n '1p' "$CASE/out")' != 'codex-cli $V'"
    [ "$(status_of codex)" = pending ] || fail "codex $arch: status is $(status_of codex)"
    note "codex $arch: download -> hash -> preflight -> tar -> report ($CODEX_TARGET)"

    # --- a constructed environment proves no orphaned environment needs -----
    # `run_codex` is already `env -i` with only the required variables, so this
    # is the clean-environment control: the vendored installer keeps `set -u` on
    # and must complete with no inherited `checksum_*` variable at all.
    new_codex_case "codex-$arch-clean-environment" "$arch" "rust-v$V" 0 0
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_ok "codex $arch clean-environment install"
    expect_stream "codex $arch clean-environment install" "$PAYLOAD_URL"
    [ "$(grep -c '^NATIVE ' "$CASE/native.log" || true)" -ge 1 ] ||
        fail "codex $arch clean environment: the native entrypoint never ran"
    note "codex $arch: a constructed environment with no checksum_* inherits status 0"

    # --- identity failures happen BEFORE tar and before the native ----------
    for kind in corrupt valid-hash-mismatch wrong-version wrong-target; do
        new_codex_case "codex-$arch-$kind-before-tar" "$arch" "rust-v$V" 0 0
        : >"$CASE/routes"
        case "$kind" in
            corrupt)
                printf 'this is not the archive\n' >"$CASE/fx/corrupt.tgz"
                printf '%s\t%s\n' "$PAYLOAD_URL" "$CASE/fx/corrupt.tgz" >>"$CASE/routes"
                diagnostic='checksum did not match expected digest'
                ;;
            valid-hash-mismatch)
                # A VALID package whose bytes are not the pinned ones: a jointly
                # rewritten upstream, with its own corroborating metadata and
                # checksums served at their exact URLs. The build must never ask.
                codex_native_body "$CASE/other-body.sh" "$V" 0 0
                # A genuinely different but still valid package.
                printf '# rewritten upstream bytes\n' >>"$CASE/other-body.sh"
                codex_fixture "$CASE/fx-other" "rust-v$V" "$CODEX_TARGET" \
                    "$CASE/other-body.sh"
                route "$PAYLOAD_URL" "$CASE/fx-other/archive.tgz"
                route "$API_URL" "$CASE/fx-other/source/evidence/release.json"
                route "$SUMS_URL" "$CASE/fx-other/source/evidence/codex-package_SHA256SUMS"
                route "$BUNDLE_URL" \
                    "$CASE/fx-other/source/evidence/codex-package-$CODEX_TARGET.tar.gz.sigstore"
                diagnostic='checksum did not match expected digest'
                ;;
            wrong-version)
                # The pin hashes THIS archive, so the committed digest agrees and
                # only the package metadata can refuse: a synthetic archive that
                # lies about its own version.
                codex_native_body "$CASE/native-body.sh" "$V" 0 0
                codex_fixture "$CASE/fx" "rust-v$V" "$CODEX_TARGET" \
                    "$CASE/native-body.sh" --package-version 0.159.4
                rm -rf "$CASE/source"
                cp -R "$CASE/fx/source" "$CASE/source"
                route "$PAYLOAD_URL" "$CASE/fx/archive.tgz"
                diagnostic='version is not the pinned'
                ;;
            wrong-target)
                codex_native_body "$CASE/native-body.sh" "$V" 0 0
                if [ "$arch" = x86_64 ]; then other=aarch64-unknown-linux-musl
                else other=x86_64-unknown-linux-musl; fi
                codex_fixture "$CASE/fx" "rust-v$V" "$CODEX_TARGET" \
                    "$CASE/native-body.sh" --package-target "$other"
                rm -rf "$CASE/source"
                cp -R "$CASE/fx/source" "$CASE/source"
                route "$PAYLOAD_URL" "$CASE/fx/archive.tgz"
                diagnostic='target is not'
                ;;
        esac
        for soft in "" 1 0; do
            # A fresh prefix and fresh logs per run: the installer refuses a
            # pre-existing standalone root, so a failed run is not repeatable in
            # the same prefix by design.
            rm -rf "$CASE/prefix"
            mkdir -p "$CASE/prefix"
            chmod 0755 "$CASE/prefix"
            : >"$CASE/curl.log"
            : >"$CASE/tar.log"
            : >"$CASE/native.log"
            SOFT="$soft"
            run_codex "rust-v$V" "$CODEX_VENDOR"
            expect_hard "codex $arch $kind (soft='$soft')"
            expect_stream "codex $arch $kind (soft='$soft')" "$PAYLOAD_URL"
            expect_no_extraction "codex $arch $kind (soft='$soft')"
            expect_no_native "codex $arch $kind (soft='$soft')"
            [ ! -e "$CASE/prefix/.local/bin/codex" ] ||
                fail "codex $arch $kind (soft='$soft'): a command was published"
            case "$(status_of codex)" in
                absent-transport*)
                    fail "codex $arch $kind (soft='$soft'): softened to $(status_of codex)" ;;
            esac
            printf '%s' "$RUN_OUT" | grep -q "$diagnostic" ||
                fail "codex $arch $kind (soft='$soft'): expected '$diagnostic', got: $RUN_OUT"
            SOFT=""
        done
        note "codex $arch: $kind is HARD before tar/native under empty/1/0 soft input"
    done

    # --- missing asset: one attempt, no alternate, no fallback --------------
    new_codex_case "codex-$arch-missing-asset" "$arch" "rust-v$V" 0 0
    : >"$CASE/routes"
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_hard "codex $arch missing asset"
    expect_stream "codex $arch missing asset" "$PAYLOAD_URL"
    expect_no_extraction "codex $arch missing asset"
    expect_no_native "codex $arch missing asset"
    [ -s "$CASE/deny.log" ] || fail "codex $arch missing asset: no denial was recorded"
    note "codex $arch: a missing payload is one attempted URL and no fallback"

    # --- transport classification -------------------------------------------
    new_codex_case "codex-$arch-transport-soft" "$arch" "rust-v$V" 0 0
    CURL_TRANSPORT_CODE=7
    SOFT=1
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_ok "codex $arch transport-soft"
    expect_stream "codex $arch transport-soft" "$PAYLOAD_URL"
    [ "$(status_of codex)" = "absent-transport 7" ] ||
        fail "codex $arch: softened transport must record absence, got $(status_of codex)"
    [ ! -e "$CASE/prefix/.local/bin/codex" ] ||
        fail "codex $arch: a softened transport failure must not leave a command"
    CURL_TRANSPORT_CODE=""
    SOFT=""
    note "codex $arch: transport 7 propagates as a receipt and softens to an absence record"

    new_codex_case "codex-$arch-transport-hard" "$arch" "rust-v$V" 0 0
    CURL_TRANSPORT_CODE=7
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_hard "codex $arch transport without soft"
    expect_stream "codex $arch transport without soft" "$PAYLOAD_URL"
    [ "$(status_of codex)" != "absent-transport 7" ] ||
        fail "codex $arch: a transport failure without soft input must not record absence"
    CURL_TRANSPORT_CODE=""
    note "codex $arch: the same transport failure is hard without soft input"

    # --- a pre-existing standalone root is never probed ---------------------
    new_codex_case "codex-$arch-preexisting-release" "$arch" "rust-v$V" 0 0
    mkdir -p "$CASE/prefix/.codex/packages/standalone"
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_hard "codex $arch pre-existing standalone root"
    expect_stream "codex $arch pre-existing standalone root"
    expect_no_native "codex $arch pre-existing standalone root"
    printf '%s' "$RUN_OUT" | grep -q 'Fresh pinned Codex installation required' ||
        fail "codex $arch: expected a fresh-install diagnostic, got: $RUN_OUT"
    note "codex $arch: an existing standalone root is refused before any probe or fetch"

    # --- native failures ----------------------------------------------------
    new_codex_case "codex-$arch-native-nonzero" "$arch" "rust-v$V" 1 0
    SOFT=1
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_hard "codex $arch native nonzero under soft input"
    expect_stream "codex $arch native nonzero under soft input" "$PAYLOAD_URL"
    [ "$(status_of codex)" != installed ] || fail "codex $arch: native nonzero recorded installed"
    SOFT=""
    note "codex $arch: a native that exits nonzero is hard even with soft input"

    new_codex_case "codex-$arch-native-75" "$arch" "rust-v$V" 75 0
    SOFT=1
    run_codex "rust-v$V" "$CODEX_VENDOR"
    expect_hard "codex $arch native 75 under soft input"
    expect_stream "codex $arch native 75 under soft input" "$PAYLOAD_URL"
    case "$(status_of codex)" in
        absent-transport*)
            fail "codex $arch: a native exit 75 was misclassified as transport ($(status_of codex))" ;;
    esac
    note "codex $arch: a native exit 75 stays hard (the native never sees the receipt env)"
    SOFT=""

    # --- hung native --------------------------------------------------------
    echo "  (codex $arch hang)"
    new_codex_case "codex-$arch-hang" "$arch" "rust-v$V" 0 1
    SOFT=1
    local_env=(
        "PATH=$CASE/bin:$CASE/pybin:/usr/bin:/bin"
        "HOME=$CASE"
        "TMPDIR=$CASE/tmp"
        "AGENT_VM_CONTRACT_DIR=$CONTRACT"
        "AGENT_VM_VENDOR_DIR=$CODEX_VENDOR"
        "AGENT_VM_CODEX_SOURCE_DIR=$CASE/source"
        "AGENT_VM_CODEX_PREFIX=$CASE/prefix"
        "AGENT_VM_INSTALL_STATUS_DIR=$CASE/status"
        "AGENT_VM_VERSION_CODEX=rust-v$V"
        "AGENT_INSTALL_SOFT_FAIL=1"
        "FIXTURE_PID_LOG=$CASE/hang.pids"
        "FIXTURE_NATIVE_LOG=$CASE/native.log"
        "FAKE_CURL_LOG=$CASE/curl.log"
        "FAKE_CURL_DENY_LOG=$CASE/deny.log"
        "FAKE_CURL_ROUTES=$CASE/routes"
        "FAKE_CURL_ROUTE_MODE=full-url"
        "FAKE_TIMEOUT_LOG=$CASE/timeout.log"
        "FAKE_TAR_LOG=$CASE/tar.log"
        "REAL_TAR=$REAL_TAR"
        "REAL_TIMEOUT=$REAL_TIMEOUT"
        "FAKE_TIMEOUT_CAP=$FAKE_TIMEOUT_CAP_SECONDS"
    )
    set +e
    RUN_OUT="$("${HANG_WATCHDOG[@]}" env -i "${local_env[@]}" \
        sh "$RUN_INSTALL" codex "$CASE/status/codex" sh "$REPO_ROOT/images/tools/codex/install-codex.sh" 2>&1)"
    RUN_RC=$?
    set -e
    probes="$(grep -c '^60$' "$CASE/timeout.log" || true)"
    [ -s "$CASE/hang.pids" ] || fail "codex $arch: the hung native never started: $RUN_OUT"
    survivors=""
    unmarked=0
    while read -r pid marker; do
        [ -n "$marker" ] || unmarked=1
        [ "$(ps -o command= -p "$pid" 2>/dev/null || true)" = "sleep $FIXTURE_HANG_SECONDS" ] || continue
        survivors="$survivors $pid"
    done <"$CASE/hang.pids"
    if [ -n "$survivors" ]; then
        kill -9 $survivors 2>/dev/null || true
        fail "codex $arch: hung native(s)$survivors survived the bound"
    fi
    if [ "$RUN_RC" -eq 124 ]; then
        if [ "$probes" -ge 1 ] && [ "$unmarked" -eq 0 ]; then
            fail "codex $arch: the ${HANG_WATCHDOG[2]}s watchdog fired after $probes probes were routed through the ${FAKE_TIMEOUT_CAP_SECONDS}s cap; host too slow or fixture defect: $RUN_OUT"
        fi
        fail "codex $arch: the installer did not bound the hung native probe itself (the ${HANG_WATCHDOG[2]}s watchdog fired; each probe is capped at ${FAKE_TIMEOUT_CAP_SECONDS}s): $RUN_OUT"
    fi
    [ "$RUN_RC" -ne 0 ] || fail "codex $arch: a hung native must fail hard: $RUN_OUT"
    [ "$(status_of codex)" != installed ] || fail "codex $arch: a hung native recorded installed"
    [ "$probes" -ge 1 ] ||
        fail "codex $arch: the installer did not route the native probe through timeout 60"
    [ "$probes" -le "$HANG_PROBE_ALLOWANCE" ] ||
        fail "codex $arch: $probes capped probes exceed HANG_PROBE_ALLOWANCE ($HANG_PROBE_ALLOWANCE); re-derive the hang watchdog"
    note "codex $arch: the installer bounds its native probe (timeout 60) and fails hard before the watchdog"
    SOFT=""
done

# --- routing has no basename fallback ---------------------------------------
# A canonical URL must be the ONLY thing a route matches. The same archive is
# reachable under its basename, so a basename-fallback router would happily serve
# a wrong repository, a wrong version or a mirror.
echo "  (codex full-URL routing)"
new_case "codex-full-url-routing" x86_64
V=0.159.3
codex_urls x86_64 "rust-v$V"
codex_native_body "$CASE/native-body.sh" "$V" 0 0
codex_fixture "$CASE/fx" "rust-v$V" "$CODEX_TARGET" "$CASE/native-body.sh"
printf '%s\t%s\n' "codex-package-$CODEX_TARGET.tar.gz" "$CASE/fx/archive.tgz" >>"$CASE/routes"
route "$PAYLOAD_URL" "$CASE/fx/archive.tgz"
fake_curl() { # $1 url, $2 out
    env -i "PATH=/usr/bin:/bin" "FAKE_CURL_ROUTES=$CASE/routes" \
        "FAKE_CURL_ROUTE_MODE=full-url" "FAKE_CURL_LOG=$CASE/curl.log" \
        "FAKE_CURL_DENY_LOG=$CASE/deny.log" sh "$CASE/bin/curl" -fsSL -o "$2" "$1"
}
: >"$CASE/curl.log"
: >"$CASE/deny.log"
fake_curl "$PAYLOAD_URL" "$CASE/got.tgz" ||
    fail "codex full-url routing: the canonical URL must be routable"
cmp -s "$CASE/got.tgz" "$CASE/fx/archive.tgz" ||
    fail "codex full-url routing: the canonical route served the wrong bytes"
for wrong in \
    "https://github.com/openai/codexx/releases/download/rust-v$V/codex-package-$CODEX_TARGET.tar.gz" \
    "https://github.com/openai/codex/releases/download/rust-v0.159.2/codex-package-$CODEX_TARGET.tar.gz" \
    "https://mirror.example/openai/codex/releases/download/rust-v$V/codex-package-$CODEX_TARGET.tar.gz" \
    "$PAYLOAD_URL?token=REVIEW-TOKEN"; do
    rm -f "$CASE/wrong.tgz"
    if fake_curl "$wrong" "$CASE/wrong.tgz"; then
        fail "codex full-url routing: '$wrong' was routed by basename"
    fi
    [ ! -s "$CASE/wrong.tgz" ] || fail "codex full-url routing: '$wrong' produced bytes"
done
[ "$(grep -c '^DENIED ' "$CASE/deny.log" || true)" = 4 ] ||
    fail "codex full-url routing: expected four denials: $(cat "$CASE/deny.log")"
note "codex: wrong repo / adjacent version / mirror / query are all denied"

# --- an attempted latest request is denied and logged -----------------------
echo "  (codex latest mutant)"
new_codex_case "codex-latest-mutant" x86_64 "rust-v$V" 0 0
python3 "$REPO_ROOT/script/test/vendored-installers-mutate-vendor.py" \
    "$CODEX_VENDOR/install.sh" "$CASE/mutant/install.sh" latest-request
! cmp -s "$CODEX_VENDOR/install.sh" "$CASE/mutant/install.sh" ||
    fail "codex latest mutant: the mutation changed no bytes"
run_codex "rust-v$V" "$CASE/mutant"
expect_hard "codex latest mutant"
expect_stream "codex latest mutant" \
    "https://api.github.com/repos/openai/codex/releases/latest"
grep -q 'releases/latest' "$CASE/deny.log" ||
    fail "codex latest mutant: the latest request was not denied: $(cat "$CASE/deny.log")"
expect_no_extraction "codex latest mutant"
expect_no_native "codex latest mutant"
note "codex: an injected /releases/latest request is a real attempt, denied and fatal"

# --- the payload is substituted between the download and its digest check ----
# The same control the native strict-egress gate runs, exercised hermetically:
# the mutation lands INSIDE download_file_with_fallback, on the download side of
# verify_archive_digest, so the refusal is the shell checksum diagnostic and the
# archive never reaches tar or the native. A mutation made after the call site
# would instead be caught by the later Python preflight, which is a different
# oracle and the wrong side of the hash.
echo "  (codex substitution mutant)"
new_codex_case "codex-substitution-mutant" x86_64 "rust-v$V" 0 0
python3 "$REPO_ROOT/script/test/vendored-installers-mutate-vendor.py" \
    "$CODEX_VENDOR/install.sh" "$CASE/mutant/install.sh" substitute-before-digest
! cmp -s "$CODEX_VENDOR/install.sh" "$CASE/mutant/install.sh" ||
    fail "codex substitution mutant: the mutation changed no bytes"
run_codex "rust-v$V" "$CASE/mutant"
expect_hard "codex substitution mutant"
expect_stream "codex substitution mutant" "$PAYLOAD_URL"
printf '%s' "$RUN_OUT" | grep -q 'checksum did not match expected digest' ||
    fail "codex substitution mutant: the committed digest did not refuse it: $RUN_OUT"
expect_no_extraction "codex substitution mutant"
expect_no_native "codex substitution mutant"
[ ! -e "$CASE/prefix/.local/bin/codex" ] ||
    fail "codex substitution mutant: a command was published"
note "codex: a post-download substitution is refused by the committed digest, before tar"

# =============================================================================
# opencode
# =============================================================================
echo "== opencode (real vendored install.sh) =="
for arch in x86_64 aarch64; do
    V=2.0.29

    make_opencode_case "opencode-$arch-ok" "$arch" "$V"
    run_opencode "v$V" "$OPENCODE_VENDOR"
    expect_ok "opencode $arch clean install"
    expect_no_denied "opencode $arch clean install"
    [ -x "$CASE/prefix/.opencode/bin/opencode" ] ||
        fail "opencode $arch: the binary was not installed"
    [ -L "$CASE/link/opencode" ] || fail "opencode $arch: the PATH link was not published"
    [ "$("$CASE/link/opencode")" = "opencode v$V" ] || fail "opencode $arch: installed binary reports wrong version"
    [ "$("$CASE/prefix/.opencode/bin/opencode2" --version)" = "opencode v$V" ] ||
        fail "opencode2 must forward to the installed command"
    [ "$(status_of opencode)" = pending ] || fail "opencode $arch: status is $(status_of opencode)"
    note "opencode $arch: clean install extracted the exact asset and linked the command"

    make_opencode_case "opencode-$arch-latest" "$arch" "$V"
    mutate_vendor "$OPENCODE_VENDOR" "$CASE/mutant" 's#https://registry.npmjs.org/\$package_name/-/\$filename#https://opencode.ai/update/api/latest/cli/npm#'
    ! cmp -s "$OPENCODE_VENDOR/install.sh" "$CASE/mutant/install.sh" || fail "latest mutation changed no bytes"
    run_opencode "v$V" "$CASE/mutant"
    expect_hard "opencode $arch latest mutation"
    expect_denied "opencode $arch latest mutation"
    note "opencode $arch: a reintroduced /latest request is denied and fails"

    make_opencode_case "opencode-$arch-transport-soft" "$arch" "$V"
    CURL_TRANSPORT_CODE=6
    SOFT=1
    run_opencode "v$V" "$OPENCODE_VENDOR"
    expect_ok "opencode $arch transport-soft"
    [ "$(status_of opencode)" = "absent-transport 6" ] ||
        fail "opencode $arch: softened transport must record absence, got $(status_of opencode)"
    [ ! -e "$CASE/prefix/.opencode" ] || fail "opencode $arch: softened install left the install dir"
    [ ! -e "$CASE/link/opencode" ] || fail "opencode $arch: softened install left the link"
    # C5: the private download scratch must be removed on a softened transport
    # failure, not left for the degraded image to ship.
    [ -z "$(ls -A "$CASE/tmp")" ] ||
        fail "opencode $arch: softened transport left scratch: $(ls -A "$CASE/tmp")"
    CURL_TRANSPORT_CODE=""
    SOFT=""
    note "opencode $arch: transport 6 softens to an absence record and cleans partial artifacts"

    make_opencode_case "opencode-$arch-transport-hard" "$arch" "$V"
    CURL_TRANSPORT_CODE=6
    run_opencode "v$V" "$OPENCODE_VENDOR"
    expect_hard "opencode $arch transport without soft"
    CURL_TRANSPORT_CODE=""
    note "opencode $arch: the same transport failure is hard without soft input"
done

# =============================================================================
# claude
# =============================================================================
echo "== claude (real vendored install.sh) =="
for arch in x86_64 aarch64; do
    V=2.1.286

    make_claude_case "claude-$arch-ok" "$arch" "$V" 0 0 0 0
    run_claude "$V" "$CLAUDE_VENDOR"
    expect_ok "claude $arch clean install"
    expect_no_denied "claude $arch clean install"
    [ -f "$CASE/prefix/native-ran" ] || fail "claude $arch: the native installer never ran"
    [ "$(cat "$CASE/prefix/native-argv")" = "$(printf 'argc=2\narg=install\narg=%s' "$V")" ] ||
        fail "claude $arch: the native installer received unexpected argv: $(cat "$CASE/prefix/native-argv")"
    [ -x "$CASE/prefix/.local/bin/claude" ] || fail "claude $arch: native install did not publish the command"
    [ "$("$CASE/prefix/.local/bin/claude")" = "$V (Claude Code)" ] ||
        fail "claude $arch: native command reports the wrong version"
    [ "$(status_of claude)" = pending ] || fail "claude $arch: status is $(status_of claude)"
    note "claude $arch: clean install verified the exact asset then ran the native installer"

    # Checksum corruption BEFORE native publication, under every nonempty soft
    # input, for a valid asset with a mismatched manifest digest AND invalid
    # bytes.
    for soft in "" 1 0; do
        for mode in 1 2; do
            kind=valid-digest-mismatch
            [ "$mode" = 2 ] && kind=invalid-bytes
            make_claude_case "claude-$arch-checksum-$kind-soft-${soft:-none}" "$arch" "$V" 0 0 "$mode" 0
            SOFT="$soft"
            run_claude "$V" "$CLAUDE_VENDOR"
            expect_hard "claude $arch $kind checksum (soft='$soft')"
            case "$(status_of claude)" in
                absent-transport*) fail "claude $arch $kind (soft='$soft'): checksum softened to $(status_of claude)" ;;
            esac
            [ ! -e "$CASE/prefix/native-ran" ] ||
                fail "claude $arch $kind (soft='$soft'): a checksum mismatch must fail before the native installer runs"
            [ ! -e "$CASE/prefix/.local/bin/claude" ] ||
                fail "claude $arch $kind (soft='$soft'): a checksum mismatch must not publish a command"
            SOFT=""
        done
    done
    note "claude $arch: checksum corruption (digest mismatch + invalid bytes) is HARD under empty/1/0 soft input"

    for soft in "" 1 0; do
        make_claude_case "claude-$arch-malformed-soft-${soft:-none}" "$arch" "$V" 0 0 0 1
        SOFT="$soft"
        run_claude "$V" "$CLAUDE_VENDOR"
        expect_hard "claude $arch malformed manifest (soft='$soft')"
        case "$(status_of claude)" in
            absent-transport*) fail "claude $arch malformed (soft='$soft'): manifest failure softened to $(status_of claude)" ;;
        esac
        [ ! -e "$CASE/prefix/native-ran" ] ||
            fail "claude $arch malformed (soft='$soft'): a malformed manifest must not reach the native installer"
        SOFT=""
    done
    note "claude $arch: malformed manifest is HARD under empty/1/0 soft input"

    make_claude_case "claude-$arch-latest" "$arch" "$V" 0 0 0 0
    mutate_vendor "$CLAUDE_VENDOR" "$CASE/mutant" 's#/manifest.json#/latest/manifest.json#'
    run_claude "$V" "$CASE/mutant"
    expect_hard "claude $arch latest mutation"
    expect_denied "claude $arch latest mutation"
    note "claude $arch: a reintroduced /latest request is denied and fails"

    make_claude_case "claude-$arch-transport-soft" "$arch" "$V" 0 0 0 0
    CURL_TRANSPORT_CODE=28
    SOFT=1
    run_claude "$V" "$CLAUDE_VENDOR"
    expect_ok "claude $arch transport-soft"
    [ "$(status_of claude)" = "absent-transport 28" ] ||
        fail "claude $arch: softened transport must record absence, got $(status_of claude)"
    [ ! -e "$CASE/prefix/.local/bin/claude" ] ||
        fail "claude $arch: softened transport must not leave a command"
    CURL_TRANSPORT_CODE=""
    SOFT=""
    note "claude $arch: transport 28 propagates as a receipt and softens to an absence record"

    make_claude_case "claude-$arch-transport-hard" "$arch" "$V" 0 0 0 0
    CURL_TRANSPORT_CODE=28
    run_claude "$V" "$CLAUDE_VENDOR"
    expect_hard "claude $arch transport without soft"
    CURL_TRANSPORT_CODE=""
    note "claude $arch: the same transport failure is hard without soft input"

    make_claude_case "claude-$arch-native-nonzero" "$arch" "$V" 1 0 0 0
    SOFT=1
    run_claude "$V" "$CLAUDE_VENDOR"
    expect_hard "claude $arch native nonzero under soft input"
    [ "$(status_of claude)" != installed ] || fail "claude $arch: native nonzero recorded installed"
    SOFT=""
    note "claude $arch: a native non-zero install is hard even with soft input"

    make_claude_case "claude-$arch-native-75" "$arch" "$V" 75 0 0 0
    SOFT=1
    run_claude "$V" "$CLAUDE_VENDOR"
    expect_hard "claude $arch native 75 under soft input"
    case "$(status_of claude)" in
        absent-transport*)
            fail "claude $arch: a native exit 75 was misclassified as transport ($(status_of claude))" ;;
    esac
    SOFT=""
    note "claude $arch: a native exit 75 stays hard (the native never sees the receipt env)"

    make_claude_case "claude-$arch-hang" "$arch" "$V" 0 1 0 0
    SOFT=1
    run_claude "$V" "$CLAUDE_VENDOR"
    expect_hard "claude $arch native hang under soft input"
    [ "$(status_of claude)" != installed ] || fail "claude $arch: a hung native recorded installed"
    grep -q '^300$' "$CASE/timeout.log" ||
        fail "claude $arch: the vendored installer did not bound the native install with timeout 300"
    SOFT=""
    note "claude $arch: the native install is bounded by 'timeout 300' and a hang fails"
done

# =============================================================================
# transport receipt clearing (images/recipe-contract/download.sh)
# =============================================================================
echo "== transport receipt clearing =="
new_case "receipt-clearing" x86_64
receipt="$CASE/receipt"
download_url="https://example.invalid/asset"
route "asset" "$CASE/assets/payload"
printf 'payload\n' >"$CASE/assets/payload"

# A stale receipt must be cleared before a successful attempt, not carried.
printf 'transport download 6\n' >"$receipt"
CURL_TRANSPORT_CODE=""
set +e
RECEIPT_OUT="$(env -i \
    "PATH=$CASE/bin:/usr/bin:/bin" \
    "AGENT_VM_CONTRACT_DIR=$CONTRACT" \
    "AGENT_VM_TRANSPORT_RECEIPT=$receipt" \
    "FAKE_CURL_LOG=$CASE/curl.log" \
    "FAKE_CURL_ROUTES=$CASE/routes" \
    sh "$CONTRACT/download.sh" "$download_url" "$CASE/out.bin" 2>&1)"
RECEIPT_RC=$?
set -e
[ "$RECEIPT_RC" -eq 0 ] || fail "clearing: a routed download must succeed: $RECEIPT_OUT"
[ ! -s "$receipt" ] || fail "clearing: a successful attempt must clear the stale receipt (got '$(cat "$receipt")')"
note "clearing: a stale receipt is cleared before a successful download"

# A hard (non-transport) failure must not leave a stale receipt behind either,
# so an unrelated 75 can never consume an earlier attempt's classification.
printf 'transport download 6\n' >"$receipt"
CURL_TRANSPORT_CODE=""
set +e
RECEIPT_OUT="$(env -i \
    "PATH=$CASE/bin:/usr/bin:/bin" \
    "AGENT_VM_CONTRACT_DIR=$CONTRACT" \
    "AGENT_VM_TRANSPORT_RECEIPT=$receipt" \
    "FAKE_CURL_LOG=$CASE/curl.log" \
    "FAKE_CURL_ROUTES=$CASE/routes" \
    sh "$CONTRACT/download.sh" "https://example.invalid/not-routed" "$CASE/out.bin" 2>&1)"
RECEIPT_RC=$?
set -e
[ "$RECEIPT_RC" -ne 0 ] || fail "clearing: an unrouted (HTTP error) download must fail hard"
[ ! -s "$receipt" ] || fail "clearing: a hard failure must not leave the stale receipt"

# A classified transport failure writes exactly one receipt line and exits 75.
printf 'stale\n' >"$receipt"
CURL_TRANSPORT_CODE=7
set +e
RECEIPT_OUT="$(env -i \
    "PATH=$CASE/bin:/usr/bin:/bin" \
    "AGENT_VM_CONTRACT_DIR=$CONTRACT" \
    "AGENT_VM_TRANSPORT_RECEIPT=$receipt" \
    "FAKE_CURL_LOG=$CASE/curl.log" \
    "FAKE_CURL_ROUTES=$CASE/routes" \
    "FAKE_CURL_TRANSPORT_CODE=$CURL_TRANSPORT_CODE" \
    sh "$CONTRACT/download.sh" "$download_url" "$CASE/out.bin" 2>&1)"
RECEIPT_RC=$?
set -e
CURL_TRANSPORT_CODE=""
[ "$RECEIPT_RC" -eq 75 ] || fail "clearing: a transport failure must exit 75, got $RECEIPT_RC"
[ "$(cat "$receipt")" = "transport download 7" ] ||
    fail "clearing: the receipt must be exactly 'transport download 7', got '$(cat "$receipt")'"
note "clearing: a classified transport writes one exact receipt line and exits 75"

echo "vendored-installer black-box tests passed"
