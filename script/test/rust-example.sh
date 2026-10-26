#!/usr/bin/env bash
# Native build/runtime evidence for the rust-dev example (#102).
#
# This is the single entrypoint for a native host and for a one-off PR workflow:
# all evidence logic lives here, never in YAML. It builds the real recipe (the
# committed default parent unless --base-image is given as a diagnostic),
# inspects the image, delivers the same verifier contract the Dockerfile uses
# over a read-only bundle, and then runs the project/toolchain/negative/canary
# legs with real tools on the host's native architecture.
#
# Prerequisites: a native Linux daemon of the requested platform, a
# daemon-backed builder, python3, git and host-watchdog.py. The amd64 row must
# come from a native amd64 host; QEMU is not acceptance.
#
# Usage: bash script/test/rust-example.sh [--platform linux/amd64|linux/arm64]
#                                         [--base-image EXACT_REF] [--no-cache]
#        bash script/test/rust-example.sh --self-test
#
# `--no-cache` builds the example from scratch. Use it for acceptance evidence:
# a cached toolchain-install stage proves nothing about the installer, so the
# suite records `build-toolchain-install cached=0|1` for every run and notes a
# cached stage in the log.
# Status: 0 all phases passed, 1 a phase or the cleanup failed, 2 usage,
#         124 the suite watchdog fired, 128+N on an inherited signal.
#
# Budgets: every host-side command runs under host-watchdog.py, the whole suite
# re-executes itself under one supervisor, and cleanup has its own deadline. The
# per-command numbers are floors: this harness has not been measured on native
# hardware yet, so they are raised only with a measurement, never to make a slow
# run pass.
#
# Cleanup is state-aware: a confirmed removal clears ownership, each resource kind
# is swept once, and a resource that is already gone is not a failure -- while a
# genuine daemon, permission or in-use error still is.
set -euo pipefail

ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
EXAMPLE=examples/layers/rust-dev
FIXTURES=script/test/fixtures/rust-toolchain
DOCKER=${DOCKER:-docker}
WATCHDOG="$ROOT/script/test/host-watchdog.py"

SUITE_SECONDS=2700          # 45 minutes, whole run
SUPERVISOR_GRACE_SECONDS=90 # > cleanup budget (60 s) + escalation + trap entry
DAEMON_SECONDS=60
PULL_SECONDS=900
BUILD_SECONDS=1800
RUN_SECONDS=300
CLEANUP_SECONDS=60          # total deadline for owned-resource removal
CLEANUP_COMMAND_SECONDS=10
CLEANUP_GRACE_SECONDS=1
CLEANUP_RESERVE_SECONDS=2

PLATFORM=linux/arm64
BASE_IMAGE=''
SELF_TEST=0
COLD_BUILD=0
# Kept for the supervising re-exec below, which happens after the arguments are
# parsed.
ORIGINAL_ARGUMENTS=("$@")
while [ "$#" -gt 0 ]; do
    case "$1" in
        --platform) [ "$#" -ge 2 ] || exit 2; PLATFORM=$2; shift 2 ;;
        --base-image) [ "$#" -ge 2 ] || exit 2; BASE_IMAGE=$2; shift 2 ;;
        --no-cache) COLD_BUILD=1; shift ;;
        --self-test) [ "$#" -eq 1 ] || exit 2; SELF_TEST=1; shift ;;
        *) echo "usage: rust-example.sh [--platform linux/amd64|linux/arm64] [--base-image REF]" >&2
           echo "                         [--no-cache]" >&2
           echo "       rust-example.sh --self-test" >&2
           exit 2 ;;
    esac
done
if [ "$SELF_TEST" -eq 1 ] && [ "$COLD_BUILD" -eq 1 ]; then
    echo 'rust-example: --no-cache is a native-run option' >&2
    exit 2
fi
if [ "$SELF_TEST" -eq 0 ]; then
    case "$PLATFORM" in
        linux/amd64) ARCH=amd64; MUSL=x86_64-unknown-linux-musl ;;
        linux/arm64) ARCH=arm64; MUSL=aarch64-unknown-linux-musl ;;
        *) echo 'rust-example: unsupported platform' >&2; exit 2 ;;
    esac
fi

# --- first-use canary leg table --------------------------------------------
# One line per leg: LABEL|PERSISTED_SETTING|EXPECTED_STATUS|EXPECTED_REQUESTS|SETUP|COMMAND
# `!` means "any nonzero status" and `any` means "either". The command strings are
# real shell programs; the guest fixture runs each as its own `bash -c` and
# reports a parse or setup failure separately from the tool's own status. Both the
# native phase and the daemon-free self-test read this one table, so the controls
# exercise the legs that actually run.
canary_legs() {
    cat <<'LEGS'
selected-tools|disable|0|none||cargo build --offline && cargo clippy --offline -- -D warnings && cargo fmt -- --check && rustc --version
absent-channel|disable|!|none||RUSTUP_AUTO_INSTALL=0 cargo +0.0.0 --version
persisted-defense|disable|!|none||env -u RUSTUP_AUTO_INSTALL cargo +0.0.0 --version
weakened-persisted|enable|!|at-least-1||env -u RUSTUP_AUTO_INSTALL cargo +0.0.0 --version
extra-declaration|disable|any|none|cp "$TOOLCHAIN/extra-toolchain.toml" rust-toolchain.toml|RUSTUP_AUTO_INSTALL=0 cargo build --offline
extra-declaration-persisted|disable|any|none|cp "$TOOLCHAIN/extra-toolchain.toml" rust-toolchain.toml|env -u RUSTUP_AUTO_INSTALL cargo build --offline
extra-declaration-target|disable|!|none|cp "$TOOLCHAIN/extra-toolchain.toml" rust-toolchain.toml|RUSTUP_AUTO_INSTALL=0 cargo build --offline --target wasm32-unknown-unknown
extra-declaration-positive|enable|!|at-least-1|cp "$TOOLCHAIN/extra-toolchain.toml" rust-toolchain.toml|env -u RUSTUP_AUTO_INSTALL cargo build --offline
LEGS
}

# The guest program's status contract, in one place: 98/99 are harness faults
# (the leg never reached the policy under test) and, for a leg that expects a
# tool failure, 126/127 mean the command could not be launched at all.
canary_verdict() { # EXPECTED_STATUS STATUS
    local expected=$1 status=$2
    if [ "$status" -eq 98 ]; then echo harness-parse; return 0; fi
    if [ "$status" -eq 99 ]; then echo harness-setup; return 0; fi
    if [ "$expected" = '!' ] && { [ "$status" -eq 126 ] || [ "$status" -eq 127 ]; }; then
        echo harness-launch; return 0
    fi
    if [ "$expected" = 'any' ]; then echo matched; return 0; fi
    if [ "$expected" = '!' ] && [ "$status" -ne 0 ]; then echo matched; return 0; fi
    if [ "$expected" != '!' ] && [ "$status" -eq "$expected" ]; then echo matched; return 0; fi
    echo mismatched
}

# --- self-test: the harness's own controls, no daemon -----------------------
# Three controls prove the harness fails closed and reports honestly, and that
# the inspected parent is the one both builds are given. They never claim a
# passing acceptance: the fake docker fails or is interrupted, the harness must
# say so.
SELFTEST_SCRATCH=''
self_test() {
    local scratch rc=0 out
    # Global, not local: the EXIT trap runs after this function returns.
    SELFTEST_SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/rust-example-selftest.XXXXXX")
    scratch=$SELFTEST_SCRATCH
    trap 'rm -rf "$SELFTEST_SCRATCH"' EXIT
    mkdir -p "$scratch/bin"
    write_fake_docker "$scratch/bin/docker"

    echo 'self-test: unavailable daemon'
    out=$scratch/no-daemon.log
    rc=0
    FAKE_DOCKER_MODE=no-daemon PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || rc=$?
    selftest_expect "$rc" 1 'daemon unavailable exits 1' "$out"
    selftest_expect_grep 'BLOCKED' "$out" 'daemon unavailable is reported as blocked'
    selftest_expect_grep 'cleanup completed status=0' "$out" 'owned-resource cleanup ran and reported'

    echo 'self-test: interrupted run'
    out=$scratch/interrupted.log
    FAKE_DOCKER_MODE=hang-build PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 &
    local child=$!
    sleep 4
    kill -TERM "$child" 2>/dev/null || true
    rc=0
    wait "$child" || rc=$?
    selftest_expect "$rc" 143 'an interrupted run reports 128+15' "$out"
    selftest_expect_grep 'cleanup completed status=' "$out" \
        'an interrupted run still records cleanup'

    echo 'self-test: both builds get the inspected parent'
    out=$scratch/build-args.log
    local diagnostic zeroes
    zeroes=$(printf 'a%.0s' $(seq 1 64))
    diagnostic="registry.example/diagnostic-parent@sha256:$zeroes"
    FAKE_DOCKER_MODE=record FAKE_DOCKER_LOG="$scratch/docker.log" PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 --base-image "$diagnostic" \
        >"$out" 2>&1 || true
    local builds with_parent pulls
    builds=$(grep -c '^build ' "$scratch/docker.log" || true)
    with_parent=$(grep '^build ' "$scratch/docker.log" |
        grep -c -- "--build-arg BASE_IMAGE=$diagnostic" || true)
    pulls=$(grep -c "^pull .*$diagnostic" "$scratch/docker.log" || true)
    selftest_expect "$pulls" 1 'the diagnostic parent is what gets pulled' "$out"
    # Three builds: the example, the wrong-Verus-digest control and the mutated
    # manifest negative. Every one must be given the inspected parent.
    selftest_expect "$builds" 3 'every build runs' "$out"
    selftest_expect "$with_parent" "$builds" \
        'every build carries --build-arg BASE_IMAGE=<inspected ref>' "$out"
    selftest_expect \
        "$(grep '^build ' "$scratch/docker.log" | head -1 | grep -c -- '--no-cache' || true)" 0 \
        'a default run does not ask for a cold example build' "$out"

    echo 'self-test: a cold build is requested and named'
    out=$scratch/cold-build.log
    FAKE_DOCKER_MODE=record FAKE_DOCKER_LOG="$scratch/cold-docker.log" PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 --no-cache >"$out" 2>&1 || true
    selftest_expect \
        "$(grep '^build ' "$scratch/cold-docker.log" | head -1 | grep -c -- '--no-cache' || true)" 1 \
        '--no-cache reaches the example build' "$out"
    selftest_expect "$(grep -c 'cold=yes' "$out" || true)" 1 \
        'the cold build is named in the harness log' "$out"

    echo 'self-test: canary guest program'
    canary_guest_controls "$scratch" "$scratch/canary-guest.log"

    echo 'self-test: bad Verus digest extracts nothing'
    verus_digest_controls "$scratch" "$scratch/verus-digest.log"

    echo 'self-test: a tag Docker would refuse is not swallowed'
    out=$scratch/tag-reference.log
    rc=0
    FAKE_DOCKER_STATE="$scratch/tag-state" FAKE_DOCKER_MODE=record \
        "$scratch/bin/docker" build -t 'Rust-Example-run-mutant' . >"$out" 2>&1 || rc=$?
    selftest_expect "$rc" 1 'the fake daemon refuses an invalid repository name' "$out"
    selftest_expect_grep 'repository name must be lowercase' "$out" \
        'the refusal is Docker'\''s own error'

    echo 'self-test: healthy native lifecycle'
    out=$scratch/healthy.log
    mkdir -p "$scratch/healthy-state"
    rc=0
    FAKE_DOCKER_MODE=healthy FAKE_DOCKER_STATE="$scratch/healthy-state" \
        FAKE_DOCKER_LOG="$scratch/healthy-docker.log" PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || rc=$?
    selftest_expect "$rc" 0 'a healthy run exits 0' "$out"
    selftest_expect_grep 'cleanup completed status=0' "$out" \
        'a healthy run cleans up cleanly'
    local log_dir
    log_dir=$(sed -n 's/^.*(logs: \(.*\))$/\1/p' "$out" | tail -1)
    selftest_expect_grep_e '^=== [0-9]+ checks, 0 failed' "$out" \
        'the healthy run reports no failed check'
    selftest_expect "$(grep -c 'canary' "$log_dir/cleanup-containers-list.log" 2>/dev/null || true)" 1 \
        'the labelled sweep removed the canary container once' "$out"
    selftest_expect "$(wc -l <"$log_dir/cleanup-images-list.log" 2>/dev/null | tr -d ' ' || true)" 1 \
        'the labelled sweep removed the one owned image' "$out"
    if [ -e "$log_dir/cleanup-container-verifier.log" ]; then
        selftest_expect 1 0 'the removed verifier container is not removed a second time' "$out"
    else
        selftest_expect 0 0 'the removed verifier container is not removed a second time' "$out"
    fi
    selftest_expect "$(grep -c 'already-absent' "$log_dir/durations.txt" 2>/dev/null || true)" 0 \
        'no cleanup removal had to be excused as already absent' "$out"
    selftest_expect "$(grep -c 'completed status=0' "$log_dir/cleanup.txt" 2>/dev/null || true)" 1 \
        'the cleanup log records success' "$out"

    echo 'self-test: a failed or malformed mandatory inspection is not acceptance'
    local mode phase
    for mode in parent-inspect-fails image-inspect-fails image-bad-id; do
        phase=parent
        [ "$mode" = image-inspect-fails ] && phase=image
        [ "$mode" = image-bad-id ] && phase=image
        out=$scratch/$mode.log
        mkdir -p "$scratch/$mode-state"
        rc=0
        FAKE_DOCKER_MODE=$mode FAKE_DOCKER_STATE="$scratch/$mode-state" \
            FAKE_DOCKER_LOG="$scratch/$mode-docker.log" PATH="$scratch/bin:$PATH" \
            bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || rc=$?
        selftest_expect "$rc" 1 "an inspection that fails or reports a non-identity fails the run ($mode)" "$out"
        selftest_expect_grep "FAIL [$phase]: " "$out" \
            "the failed inspection is named as a $phase-phase failure"
        selftest_expect "$(grep -c 'id=unknown' "$out" || true)" 0 \
            "no unknown identity is recorded as evidence ($mode)"
        selftest_expect "$(grep -cE '^image (garbage|unknown) ' "$out" || true)" 0 \
            "the malformed identity is never logged as the built image ($mode)" "$out"
        selftest_expect "$(grep -c 'checks, 0 failed' "$out" || true)" 0 \
            "the run never reports a clean summary ($mode)"
    done

    echo 'self-test: a genuine cleanup failure is still a failure'
    out=$scratch/cleanup-failed.log
    rc=0
    FAKE_DOCKER_MODE=cleanup-fails FAKE_DOCKER_STATE="$scratch/failed-state" \
        FAKE_DOCKER_LOG="$scratch/failed-docker.log" PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || rc=$?
    selftest_expect "$rc" 1 'a genuine cleanup failure exits 1' "$out"
    selftest_expect_grep 'cleanup completed status=1' "$out" \
        'the cleanup result is recorded as failed'
    selftest_expect_grep 'FAIL [cleanup]: owned-resource cleanup failed' "$out" \
        'the failure is named, not masked'
    echo 'self-test: passed (59 controls)'
}

# The canary legs are shell programs handed to a guest container. Nothing in the
# three fake-docker controls above parses or runs them, so this control does both
# on this host: every leg's setup and command must be valid Bash, and the guest
# fixture must execute a positive, a negative, a syntax-broken, a setup-broken and
# an unlaunchable leg with the statuses the harness classifies separately. Each
# required setup operation gets its own failing control: a masked setup failure
# would let a leg claim a persisted policy it never applied.
canary_guest_controls() { # SCRATCH LOG
    local scratch=$1 out=$2 root leg status verdict legs=0 parsed=0
    root=$scratch/canary-guest
    mkdir -p "$root/opt/rustup" "$root/opt/cargo/bin" "$root/toolchain" "$root/work/src"
    printf '[toolchain]\nchannel = "0.0.0"\n' >"$root/toolchain/extra-toolchain.toml"
    printf '[package]\nname = "fixture"\nversion = "0.0.0"\n' >"$root/work/Cargo.toml"
    printf 'fn main() {}\n' >"$root/work/src/main.rs"
    cat >"$root/opt/cargo/bin/rustup" <<'STUB'
#!/usr/bin/env bash
if [ "${1:-}" = set ] && [ "${2:-}" = auto-install ]; then
    printf '%s\n' "${3:-}" >"$RUSTUP_HOME/auto-install"
fi
exit 0
STUB
    cat >"$root/opt/cargo/bin/cargo" <<'STUB'
#!/usr/bin/env bash
case " $* " in
    *" --target "*) printf 'error: target not installed\n' >&2; exit 1 ;;
esac
case "${1:-}" in
    +*) printf 'error: toolchain not installed\n' >&2; exit 1 ;;
    *) exit 0 ;;
esac
STUB
    printf '#!/usr/bin/env bash\nprintf "rustc 0.0.0\\n"\n' >"$root/opt/cargo/bin/rustc"
    chmod 0755 "$root/opt/cargo/bin/rustup" "$root/opt/cargo/bin/cargo" \
        "$root/opt/cargo/bin/rustc"

    run_guest() { # PERSISTED SETUP COMMAND
        # GUEST_WORK and GUEST_PROJECT are the controls' levers over the two
        # setup paths an ordinary scratch tree cannot break.
        CANARY_ROOT="$root" CANARY_IMAGE_RUSTUP="$root/opt/rustup" \
        CANARY_IMAGE_CARGO="$root/opt/cargo" \
        CANARY_IMAGE_WORK="${GUEST_WORK:-$root/work}" \
        CANARY_IMAGE_TOOLCHAIN="$root/toolchain" \
        CANARY_PROJECT="${GUEST_PROJECT:-}" \
        env -u RUSTUP_HOME -u CARGO_HOME -u RUSTUP_AUTO_INSTALL -u RUSTUP_TOOLCHAIN \
            bash "$ROOT/script/test/fixtures/rust-toolchain/canary-guest.sh" "$1" "$2" "$3"
    }

    # 1. Every leg in the shared table is valid Bash, exactly as handed to the guest.
    while IFS='|' read -r leg _persisted _expected _requests setup command; do
        [ -n "$leg" ] || continue
        legs=$((legs + 1))
        if [ -n "$setup" ] && ! bash -n -c "$setup"; then
            selftest_expect 1 0 "canary leg $leg setup parses" "$out"
        fi
        if ! bash -n -c "$command"; then
            selftest_expect 1 0 "canary leg $leg command parses" "$out"
        fi
        parsed=$((parsed + 1))
    done <<EOF
$(canary_legs)
EOF
    selftest_expect "$parsed" "$legs" 'every canary leg parses as Bash' "$out"
    selftest_expect "$legs" 8 'the shared leg table has every planned leg' "$out"

    # 2. The guest program really runs a leg: positive status and persisted setting.
    rm -f "$root/tmp/rustup/auto-install"
    status=0
    run_guest disable '' 'cargo build --offline && cargo clippy --offline -- -D warnings && cargo fmt -- --check && rustc --version' \
        >>"$out" 2>&1 || status=$?
    selftest_expect "$status" 0 'the selected-tools leg runs to success' "$out"
    selftest_expect_text "$(cat "$root/tmp/rustup/auto-install" 2>/dev/null || echo missing)" \
        disable 'the guest applied the leg persisted setting' "$out"

    # 3. A tool's expected nonzero, then the three harness-fault statuses.
    status=0
    run_guest disable '' 'RUSTUP_AUTO_INSTALL=0 cargo +0.0.0 --version' >>"$out" 2>&1 || status=$?
    verdict=$(canary_verdict '!' "$status")
    selftest_expect_text "$verdict" matched 'a negative leg is a matched observation' "$out"
    status=0
    run_guest disable '' 'cargo build --offline --target wasm32-unknown-unknown' \
        >>"$out" 2>&1 || status=$?
    verdict=$(canary_verdict '!' "$status")
    selftest_expect_text "$verdict" matched 'an uninstalled target is a matched observation' "$out"
    status=0
    run_guest disable '' 'if then' >>"$out" 2>&1 || status=$?
    selftest_expect_text "$(canary_verdict '!' "$status")" harness-parse \
        'a syntactically broken leg is a harness fault, not a refusal' "$out"
    status=0
    run_guest disable 'false' 'cargo build --offline' >>"$out" 2>&1 || status=$?
    selftest_expect_text "$(canary_verdict '!' "$status")" harness-setup \
        'a failed leg setup is a harness fault' "$out"
    status=0
    run_guest disable '' 'canary-leg-missing-command-0.0.0' >>"$out" 2>&1 || status=$?
    selftest_expect_text "$(canary_verdict '!' "$status")" harness-launch \
        'an unlaunchable command is not counted as a policy refusal' "$out"

    # 4. Every required setup operation is checked on its own. A masking failure
    #    here is the defect this leg set exists to prevent: an early failed
    #    `rustup set` used to be overwritten by the later successful copy and cd.
    local masked=$scratch/canary-setup-masked.log
    # A fresh clone per control: a leftover scratch tree would let the previous
    # run's healthy rustup stand in for the failing one.
    rm -rf "$root/tmp"
    mv "$root/opt/cargo/bin/rustup" "$root/opt/cargo/bin/rustup.stub"
    printf '#!/usr/bin/env bash\nprintf "SETUP-FAIL\\n" >&2\nexit 17\n' \
        >"$root/opt/cargo/bin/rustup"
    chmod 0755 "$root/opt/cargo/bin/rustup"
    status=0
    run_guest disable '' 'echo COMMAND-RAN' >"$masked" 2>&1 || status=$?
    mv "$root/opt/cargo/bin/rustup.stub" "$root/opt/cargo/bin/rustup"
    selftest_expect_text "$(canary_verdict '!' "$status")" harness-setup \
        'a failed persisted-policy setup is a harness fault' "$masked"
    selftest_expect_text "$(grep -c 'COMMAND-RAN' "$masked" || true)" 0 \
        'a leg whose persisted setting failed never runs its command' "$masked"
    status=0
    rm -rf "$root/tmp"
    GUEST_WORK="$root/absent-work" run_guest disable '' 'echo COMMAND-RAN' \
        >"$masked" 2>&1 || status=$?
    selftest_expect_text "$(canary_verdict '!' "$status")" harness-setup \
        'a failed project copy is a harness fault' "$masked"
    status=0
    rm -rf "$root/tmp"
    GUEST_PROJECT="$root/tmp/project/absent" run_guest disable '' 'echo COMMAND-RAN' \
        >"$masked" 2>&1 || status=$?
    selftest_expect_text "$(canary_verdict '!' "$status")" harness-setup \
        'a failed project cd is a harness fault' "$masked"
}

# The bad-Verus-digest leg claims the installer fails *before* it extracts. The
# installer's `unzip -q` is silent, so a grep over ordinary build output can never
# establish that: this control executes the real installer with an instrumented
# scratch PATH and counts actual extraction calls, then proves the same count
# catches a mutant that moves the extraction before the checksum.
verus_digest_controls() { # SCRATCH LOG
    local scratch=$1 out=$2 shims root trace mutant unzip_line status=0
    shims=$scratch/verus-shims
    root=$scratch/verus-install
    trace=$scratch/verus-trace
    mkdir -p "$shims" "$root/verus"
    cat >"$shims/dpkg" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
echo amd64
SHIM
    cat >"$shims/curl" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\n' "$*" >>"$VERUS_CONTROL_TRACE"
target=''
while [ "$#" -gt 0 ]; do
    case "$1" in -o) shift; target=$1 ;; esac
    shift
done
if [ -n "$target" ]; then printf 'not the pinned asset' >"$target"; fi
exit 0
SHIM
    cat >"$shims/unzip" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
printf 'unzip %s\n' "$*" >>"$VERUS_CONTROL_TRACE"
exit 0
SHIM
    cat >"$shims/sha256sum" <<'SHIM'
#!/usr/bin/env bash
set -euo pipefail
# Only `-c -` is used here: delegate to the host's own digest checker, never
# compute a digest inside the shell.
for candidate in /usr/bin/sha256sum /bin/sha256sum; do
    if [ -x "$candidate" ]; then exec "$candidate" "$@"; fi
done
for candidate in /usr/bin/shasum /bin/shasum; do
    if [ -x "$candidate" ]; then exec "$candidate" -a 256 -c -; fi
done
echo 'sha256sum: no host digest tool' >&2
exit 3
SHIM
    chmod 0755 "$shims/dpkg" "$shims/curl" "$shims/unzip" "$shims/sha256sum"

    run_installer() { # SCRIPT LOG
        : >"$trace"
        env PATH="$shims:$PATH" VERUS_CONTROL_TRACE="$trace" \
            VERUS_RELEASE=0.0.0-control \
            VERUS_SHA256=0000000000000000000000000000000000000000000000000000000000000000 \
            VERUS_DIR="$root/verus" bash "$1" >"$2" 2>&1
    }

    status=0
    run_installer "$ROOT/$EXAMPLE/install-verus.sh" "$root/bad-digest.log" || status=$?
    selftest_expect_text "$([ "$status" -ne 0 ] && echo refused || echo accepted)" refused \
        'a wrong Verus digest fails the installer' "$out"
    selftest_expect_grep 'FAILED' "$root/bad-digest.log" \
        'the refusal names the failed digest check'
    selftest_expect "$(grep -c '^unzip ' "$trace" || true)" 0 \
        'the refused install performed zero extraction calls' "$out"
    selftest_expect_grep 'curl ' "$trace" 'the refused install fetched the asset first'

    # The discrimination control: the same installer with an extraction call
    # added before the checksum must show one call, so a zero count is evidence.
    mutant=$root/install-verus-early-unzip.sh
    awk '/sha256sum -c -/ && !placed { print "unzip -q \"$archive\" -d \"$VERUS_DIR\""; placed = 1 } { print }' \
        "$ROOT/$EXAMPLE/install-verus.sh" >"$mutant"
    unzip_line=$(grep -c '^unzip -q ' "$mutant" || true)
    selftest_expect "$unzip_line" 2 'the early-extraction mutant applied' "$out"
    status=0
    run_installer "$mutant" "$root/early-unzip.log" || status=$?
    selftest_expect_text "$([ "$status" -ne 0 ] && echo refused || echo accepted)" refused \
        'the early-extraction mutant still fails on the digest' "$out"
    selftest_expect "$(grep -c '^unzip ' "$trace" || true)" 1 \
        'the early-extraction mutant is caught by the extraction count' "$out"
}

selftest_expect() { # ACTUAL EXPECTED NAME LOG
    if [ "$1" -ne "$2" ]; then
        echo "self-test: $3: expected status $2, got $1" >&2
        cat "$4" >&2
        exit 1
    fi
    echo "self-test: ok   $3"
}

selftest_expect_text() { # ACTUAL EXPECTED NAME LOG
    if [ "$1" != "$2" ]; then
        echo "self-test: $3: expected '$2', got '$1'" >&2
        cat "$4" >&2
        exit 1
    fi
    echo "self-test: ok   $3"
}

selftest_expect_grep_e() { # EXTENDED_PATTERN LOG NAME
    if ! grep -qE -- "$1" "$2"; then
        echo "self-test: $3: $1 not found" >&2
        cat "$2" >&2
        exit 1
    fi
    echo "self-test: ok   $3"
}

selftest_expect_grep() { # PATTERN LOG NAME
    if ! grep -qF -- "$1" "$2"; then
        echo "self-test: $3: $1 not found" >&2
        cat "$2" >&2
        exit 1
    fi
    echo "self-test: ok   $3"
}

write_fake_docker() { # DESTINATION
    cat >"$1" <<'FAKE'
#!/usr/bin/env bash
# A deliberately unhelpful docker: it records argv and fails or hangs where the
# self-test says so. It never pretends an acceptance run succeeded.
#
# With FAKE_DOCKER_STATE set it also models resource existence, so the harness's
# cleanup can be exercised for real: removing the same container, network or
# image twice fails the way Docker fails, which is exactly the state a successful
# run used to turn into a cleanup error.
mode=${FAKE_DOCKER_MODE:-record}
state=${FAKE_DOCKER_STATE:-}
if [ -n "${FAKE_DOCKER_LOG:-}" ]; then printf '%s\n' "$*" >>"$FAKE_DOCKER_LOG"; fi

arg_after() { # FLAG ARGS... -> the value following FLAG
    local flag=$1 previous='' arg
    shift
    for arg in "$@"; do
        if [ "$previous" = "$flag" ]; then printf '%s\n' "$arg"; return 0; fi
        previous=$arg
    done
    return 0
}

# Docker refuses a reference whose repository name (the part before the final
# ':') is missing or has uppercase letters. A fake that accepted any string hid a
# real harness defect: the mutant builds were refused with this very error, and
# the negative controls counted that refusal as their expected failure.
require_valid_tag() { # REFERENCE
    local ref=$1 repo=$1
    case "${ref##*:}" in
        */*) : ;;
        *) repo=${ref%:*} ;;
    esac
    case "$repo" in
        '' | *[A-Z]*)
            printf 'ERROR: failed to build: invalid tag "%s": repository name must be lowercase\n' \
                "$ref" >&2
            exit 1 ;;
    esac
    return 0
}

banner() { printf 'sha256:'; printf 'b%.0s' $(seq 1 64); printf ' %s\n' "$*"; }

legacy() {
    case "$1 ${2:-}" in
        'version --format') [ "$mode" = no-daemon ] && exit 1; echo "${FAKE_SERVER_ARCH:-linux/amd64}" ;;
        'context show') echo fake-builder ;;
        'buildx inspect') echo 'Driver: docker' ;;
        'info --format') echo 'linux amd64 overlay2' ;;
        'version ') [ "$mode" = no-daemon ] && exit 1; echo 'fake client' ;;
        'pull '*) exit 0 ;;
        'build '*)
            previous=''
            for arg in "$@"; do
                [ "$previous" = '-t' ] && require_valid_tag "$arg"
                previous=$arg
            done
            [ "$mode" = hang-build ] && sleep 3600; exit 0 ;;
        # The shape follows the requested format: the harness now validates both
        # fields, and an unhelpful fake must still model what it is asked for.
        'image inspect')
            case "${4:-}" in
                *Size*) echo 'sha256:'$(printf 'b%.0s' $(seq 1 64))' 12345' ;;
                *) echo 'sha256:'$(printf 'b%.0s' $(seq 1 64))' fake@sha256:'$(printf 'b%.0s' $(seq 1 64))' linux/amd64' ;;
            esac ;;
        'image ls'*) exit 0 ;;
        'image rm'*) exit 0 ;;
        'network create') exit 0 ;;
        'network rm') exit 0 ;;
        'rm '*) exit 0 ;;
        'ps '*) exit 0 ;;
        'logs '*) exit 0 ;;
        'create '*) exit 1 ;;
        'start '*) exit 1 ;;
        'cp '*) exit 1 ;;
        'run -d'*) echo 'sha256:'$(printf 'c%.0s' $(seq 1 64)) ;;
        'run '*) exit 1 ;;
        *) exit 0 ;;
    esac
}

create_container() { # ARGS... (expects --name and --label)
    local name label
    name=$(arg_after --name "$@")
    label=$(arg_after --label "$@")
    [ -n "$name" ] || { printf 'fake docker: no --name\n' >&2; exit 2; }
    printf '%s\n' "$label" >"$state/container.$name"
}

remove_container() { # ARGS... (rm -f NAME)
    local name
    case "$2" in -f) name=$3 ;; *) name=$2 ;; esac
    if [ ! -f "$state/container.$name" ]; then
        printf 'Error response from daemon: No such container: %s\n' "$name" >&2
        exit 1
    fi
    if [ "$mode" = cleanup-fails ]; then
        printf 'Error response from daemon: cannot remove container %s: permission denied\n' \
            "$name" >&2
        exit 1
    fi
    rm -f "$state/container.$name"
}

run_docker() { # ARGS...
    local name label arg last='' script='' persisted='' command=''
    if [ "${2:-}" = '-d' ]; then
        name=$(arg_after --name "$@")
        label=$(arg_after --label "$@")
        printf '%s\n' "$label" >"$state/container.$name"
        printf '%s\n' "$name" >"$state/canary"
        : >"$state/requests.$name"
        banner 'fake container'
        exit 0
    fi
    for arg in "$@"; do
        [ "$arg" = '/fixtures/canary-guest.sh' ] && script=canary
        last=$arg
    done
    if [ "$script" = canary ]; then
        # The guest argv is PERSISTED SETUP COMMAND, so the command is last and
        # the persisted setting is the third from the end.
        command=$last
        persisted=$(arg_after /fixtures/canary-guest.sh "$@")
    fi
    case "$last" in
        *'env | sort'*)
            printf 'RUSTUP_AUTO_INSTALL=0\n'
            printf '755 root:root /opt/cargo/bin/rustup\n'
            printf '644 root:root /opt/rustup/settings.toml\n'
            printf 'ls: cannot access /tmp/rust_prepare: No such file or directory\n'
            exit 0 ;;
    esac
    if [ "$script" = canary ]; then
        case "$command" in
            *'--target wasm32'*) exit 1 ;;
            *'+0.0.0'*)
                [ "$persisted" = enable ] &&
                    printf 'request /dist/channel-rust-0.0.0.toml\n' \
                        >>"$state/requests.$(cat "$state/canary")"
                exit 1 ;;
            *'env -u RUSTUP_AUTO_INSTALL cargo build'*)
                [ "$persisted" = enable ] &&
                    printf 'request /dist/channel-rust-1.98.1.toml\n' \
                        >>"$state/requests.$(cat "$state/canary")"
                exit 1 ;;
            *) exit 0 ;;
        esac
    fi
    case "$last" in
        *'cargo +0.0.0'*|*'--target wasm32'*|*'cargo miri'*) exit 1 ;;
        *'verus --crate-type=lib'*)
            printf 'verification results:: 12 verified, 0 errors\n'; exit 0 ;;
        *'test -d /opt/verus'*) exit 1 ;;
    esac
    exit 0
}

if [ -z "$state" ]; then
    legacy "$@"
    exit 0
fi

mkdir -p "$state"
case "$1" in
    version)
        [ "$mode" = no-daemon ] && exit 1
        case " $* " in
            *' --format '*) printf '%s\n' "${FAKE_SERVER_ARCH:-linux/amd64}" ;;
            *) echo 'fake client' ;;
        esac ;;
    context) echo fake-builder ;;
    buildx) echo 'Driver: docker' ;;
    info) echo 'linux amd64 overlay2' ;;
    pull) exit 0 ;;
    build)
        # A tag is checked before any injected failure: the mutant builds are
        # expected to fail, so a bad tag that failed them for the wrong reason
        # would otherwise look exactly like the negative the controls want.
        tagprev=''
        for tagarg in "$@"; do
            [ "$tagprev" = '-t' ] && require_valid_tag "$tagarg"
            tagprev=$tagarg
        done
        case " $* " in
            *' VERUS_SHA256=0000000'*)
                printf 'sha256sum: FAILED\n' >&2; exit 1 ;;
            *' --no-cache '*)
                printf 'bytes differ from the committed digest\n' >&2; exit 1 ;;
        esac
        local id='' label='' previous='' arg
        for arg in "$@"; do
            if [ "$previous" = '-t' ]; then
                # The harness validates the ID as Docker's own format, so the fake
                # has to emit a real-shaped one: a shorter or word-like ID is not
                # an image identity.
                id="sha256:$(printf '%064x' "$(printf '%s' "$arg" | cksum | cut -d' ' -f1)")"
                printf '%s\n' "$arg" >"$state/tag.$arg"
                printf '%s\n' "$id" >"$state/tag-id.$arg"
                printf '%s\n' "$label" >"$state/image.$id"
            fi
            [ "$previous" = '--label' ] && label=$arg
            previous=$arg
        done
        [ "$mode" = hang-build ] && sleep 3600
        exit 0 ;;
    image)
        case "$2" in
            inspect)
                fmt=$4
                for arg in "$@"; do last=$arg; done
                case "$fmt" in
                    *RepoDigests*)
                        if [ "$mode" = parent-inspect-fails ]; then
                            printf 'Error response from daemon: parent inspect failed\n' >&2
                            exit 17
                        fi
                        banner 'fake@sha256:'"$(printf 'b%.0s' $(seq 1 64))"' linux/amd64' ;;
                    *)
                        if [ "$mode" = image-inspect-fails ]; then
                            printf 'Error response from daemon: image inspect failed\n' >&2
                            exit 17
                        fi
                        if [ "$mode" = image-bad-id ]; then
                            # A plausible size next to a non-identity: the shape the
                            # harness used to accept as evidence.
                            printf 'garbage 12345\n'
                            exit 0
                        fi
                        if [ -f "$state/tag.$last" ]; then
                            id=$(cat "$state/tag-id.$last")
                            printf '%s 12345\n' "$id"
                        else
                            printf 'Error: No such image: %s\n' "$last" >&2; exit 1
                        fi ;;
                esac ;;
            ls)
                wanted=''
                for arg in "$@"; do
                    case "$arg" in label=*) wanted=${arg#label=} ;; esac
                done
                for image in "$state"/image.*; do
                    [ -f "$image" ] || continue
                    if [ -n "$wanted" ] && [ "$(cat "$image")" = "$wanted" ]; then
                        printf '%s\n' "${image##*/image.}"
                    fi
                done ;;
            rm)
                case "$3" in -f) id=$4 ;; *) id=$3 ;; esac
                if [ -f "$state/image.$id" ]; then
                    rm -f "$state/image.$id"
                    for idf in "$state"/tag-id.*; do
                        [ -f "$idf" ] || continue
                        if [ "$(cat "$idf")" = "$id" ]; then
                            rm -f "$idf" "${idf/tag-id./tag.}"
                        fi
                    done
                    exit 0
                fi
                printf 'Error: No such image: %s\n' "$id" >&2; exit 1 ;;
        esac ;;
    rmi)
        case "$2" in -f) name=$3 ;; *) name=$2 ;; esac
        if [ -f "$state/tag.$name" ]; then
            id=$(cat "$state/tag-id.$name")
            rm -f "$state/tag.$name" "$state/tag-id.$name" "$state/image.$id"
            exit 0
        fi
        printf 'Error: No such image: %s\n' "$name" >&2; exit 1 ;;
    network)
        case "$2" in
            create)
                for arg in "$@"; do last=$arg; done
                : >"$state/network.$last"; exit 0 ;;
            rm)
                if [ -f "$state/network.$3" ]; then
                    rm -f "$state/network.$3"; exit 0
                fi
                printf 'Error: No such network: %s\n' "$3" >&2; exit 1 ;;
        esac ;;
    ps)
        wanted=''
        for arg in "$@"; do
            case "$arg" in label=*) wanted=${arg#label=} ;; esac
        done
        for container in "$state"/container.*; do
            [ -f "$container" ] || continue
            if [ -n "$wanted" ] && [ "$(cat "$container")" = "$wanted" ]; then
                printf '%s\n' "${container##*/container.}"
            fi
        done ;;
    logs)
        [ -f "$state/requests.$2" ] && cat "$state/requests.$2"
        exit 0 ;;
    create) create_container "$@" ;;
    start|cp) exit 0 ;;
    rm) remove_container "$@" ;;
    run) run_docker "$@" ;;
esac
exit 0
FAKE
    chmod 0755 "$1"
}

if [ "$SELF_TEST" -eq 1 ]; then
    self_test
    exit 0
fi

# --- suite supervision ------------------------------------------------------
if [ "${RUST_EXAMPLE_SUPERVISED:-0}" != 1 ]; then
    export RUST_EXAMPLE_SUPERVISED=1
    exec python3 "$WATCHDOG" --grace-seconds "$SUPERVISOR_GRACE_SECONDS" \
        "$SUITE_SECONDS" bash "${BASH_SOURCE[0]}" ${ORIGINAL_ARGUMENTS[@]+"${ORIGINAL_ARGUMENTS[@]}"}
fi

RUN_ID="rust-example-$(date -u +%Y%m%dT%H%M%SZ)-$$"
LOG_DIR="$ROOT/target/rust-example/$RUN_ID"
mkdir -p "$LOG_DIR"
LABEL="org.agent-vm.test.rust-example=$RUN_ID"
NETWORK_NAME="$RUN_ID-net"
CANARY_NAME="$RUN_ID-canary"
CONTAINER="$RUN_ID-verify"
TAG="agent-vm-rust-dev:$RUN_ID"
# A tag needs a lowercase repository name before the ':'; the run id alone is not
# a valid repository (it carries 'T' and 'Z'), so a bare "$RUN_ID-mutant" made
# every mutant build fail with an invalid-tag error that the negative controls
# then read as the negative they were looking for.
MUTANT_TAG="agent-vm-rust-dev-mutant:$RUN_ID"
BUNDLE="$LOG_DIR/verifier-bundle"
TESTS=0
FAILED=0
PHASE=start
CLEANUP_DEADLINE=0
# Every owned resource is registered *before* the command that creates it, so an
# interrupted creation is still cleaned up and nothing the run did not create is
# ever removed.
OWNED_NETWORK=0
OWNED_CANARY=0
OWNED_CONTAINER=0
OWNED_TAG=0
OWNED_MUTANT_TAG=0

log() { printf '%s\n' "$*" | tee -a "$LOG_DIR/harness.log"; }
record() { printf '%s\n' "$*" >>"$LOG_DIR/durations.txt"; }
pass() { TESTS=$((TESTS + 1)); log "PASS [$PHASE]: $*"; }
fail() { TESTS=$((TESTS + 1)); FAILED=$((FAILED + 1)); log "FAIL [$PHASE]: $*"; }

# Every host-side command is bounded and its own session: no hung client, build
# or runtime container can outlive the suite, and its status is decided
# separately so a nonzero exit is never swallowed.
run_bounded() { # LABEL SECONDS OUTPUT
    local label=$1 seconds=$2 out=$3 started status=0
    shift 3
    started=$SECONDS
    python3 "$WATCHDOG" --grace-seconds "$CLEANUP_GRACE_SECONDS" "$seconds" "$@" \
        >"$out" 2>&1 || status=$?
    record "$label status=$status seconds=$((SECONDS - started))"
    return "$status"
}

# --- cleanup ----------------------------------------------------------------
cleanup_timed() { # LABEL COMMAND...
    local label=$1 remaining
    shift
    remaining=$((CLEANUP_DEADLINE - SECONDS - CLEANUP_RESERVE_SECONDS))
    if [ "$remaining" -lt "$((CLEANUP_COMMAND_SECONDS + CLEANUP_GRACE_SECONDS))" ]; then
        record "cleanup-$label seconds=0 status=skipped-deadline"
        return 1
    fi
    run_bounded "cleanup-$label" "$CLEANUP_COMMAND_SECONDS" "$LOG_DIR/cleanup-$label.log" "$@"
}

# A removal whose only complaint is that the resource is already absent is a
# successful cleanup: this run may have removed it at the point that owned it, or
# the labelled sweep may have removed it first. A real daemon, permission or
# in-use error keeps its status, so genuine cleanup failures are never masked.
cleanup_remove() { # LABEL COMMAND...
    local label=$1 status=0 log="$LOG_DIR/cleanup-$1.log"
    shift
    cleanup_timed "$label" "$@" || status=$?
    [ "$status" -eq 0 ] && return 0
    if [ -f "$log" ] && grep -qiE 'no such (container|network|image)|no such object|not found' "$log"; then
        record "cleanup-$label already-absent"
        return 0
    fi
    return "$status"
}

# Only resources this invocation labelled or named are removed; nothing shared is
# pruned and no image is removed that the run did not create. Each kind is swept
# once: when the labelled sweep completes, the named removals are skipped because
# the sweep already covered every resource this run labelled -- removing them
# again would report a spurious failure for an absent container, network or tag.
cleanup() {
    local status=$? id cleanup_status=0 containers_gone=0 images_gone=0
    trap '' INT TERM HUP
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_SECONDS))
    if cleanup_timed containers-list "$DOCKER" ps -aq --filter "label=$LABEL"; then
        containers_gone=1
        while IFS= read -r id; do
            [ -n "$id" ] || continue
            cleanup_remove "container-$id" "$DOCKER" rm -f "$id" || { containers_gone=0; cleanup_status=1; }
        done <"$LOG_DIR/cleanup-containers-list.log"
    else
        cleanup_status=1
    fi
    if [ "$containers_gone" -eq 1 ]; then
        OWNED_CONTAINER=0
        OWNED_CANARY=0
    fi
    if [ "$OWNED_CONTAINER" -eq 1 ]; then
        cleanup_remove container-verifier "$DOCKER" rm -f "$CONTAINER" || cleanup_status=1
    fi
    if [ "$OWNED_CANARY" -eq 1 ]; then
        cleanup_remove container-canary "$DOCKER" rm -f "$CANARY_NAME" || cleanup_status=1
    fi
    if [ "$OWNED_NETWORK" -eq 1 ]; then
        cleanup_remove network "$DOCKER" network rm "$NETWORK_NAME" || cleanup_status=1
    fi
    if cleanup_timed images-list "$DOCKER" image ls -q --filter "label=$LABEL"; then
        images_gone=1
        while IFS= read -r id; do
            [ -n "$id" ] || continue
            cleanup_remove "image-$id" "$DOCKER" image rm -f "$id" || { images_gone=0; cleanup_status=1; }
        done <"$LOG_DIR/cleanup-images-list.log"
    else
        cleanup_status=1
    fi
    if [ "$images_gone" -eq 1 ]; then
        # Removing the image ids above removed their tags with them.
        OWNED_TAG=0
        OWNED_MUTANT_TAG=0
    fi
    if [ "$OWNED_TAG" -eq 1 ]; then
        cleanup_remove image-tag "$DOCKER" rmi -f "$TAG" || cleanup_status=1
    fi
    if [ "$OWNED_MUTANT_TAG" -eq 1 ]; then
        cleanup_remove image-mutant-tag "$DOCKER" rmi -f "$MUTANT_TAG" || cleanup_status=1
    fi
    printf 'completed status=%s seconds=%s\n' "$cleanup_status" \
        "$((SECONDS - (CLEANUP_DEADLINE - CLEANUP_SECONDS)))" >"$LOG_DIR/cleanup.txt"
    log "cleanup completed status=$cleanup_status (logs: $LOG_DIR)"
    [ "$cleanup_status" -eq 0 ] || { log 'FAIL [cleanup]: owned-resource cleanup failed'; status=1; }
    exit "$status"
}
trap cleanup EXIT

log "=== rust-example $RUN_ID platform=$PLATFORM arch=$ARCH ==="
log "source $(git -C "$ROOT" rev-parse HEAD) dirty=$(git -C "$ROOT" status --porcelain | grep -cv '^?? ' || true)"
run_bounded daemon "$DAEMON_SECONDS" "$LOG_DIR/daemon.log" "$DOCKER" version || true
run_bounded context "$DAEMON_SECONDS" "$LOG_DIR/context.log" "$DOCKER" context show || true
run_bounded builder "$DAEMON_SECONDS" "$LOG_DIR/builder.log" \
    "$DOCKER" buildx inspect "$(cat "$LOG_DIR/context.log" 2>/dev/null || echo default)" || true
runner=$(cat "$LOG_DIR/context.log" 2>/dev/null || true)
[ -n "$runner" ] || runner=default
run_bounded server-arch "$DAEMON_SECONDS" "$LOG_DIR/server-arch.log" \
    "$DOCKER" version --format '{{.Server.Os}}/{{.Server.Arch}}' || true
SERVER_ARCH=$(cat "$LOG_DIR/server-arch.log" 2>/dev/null || true)
[ -n "$SERVER_ARCH" ] || SERVER_ARCH=unknown
log "docker server $SERVER_ARCH builder $runner"
PHASE=prerequisite
if [ "$SERVER_ARCH" != "linux/$ARCH" ]; then
    fail "BLOCKED: the daemon is $SERVER_ARCH, not native linux/$ARCH; QEMU is not acceptance"
    record 'native-prerequisite FAILED'
    exit 1
fi
record "native-prerequisite OK ($SERVER_ARCH)"

# --- phase 1: parent and build ----------------------------------------------
PINNED_PARENT=$(sed -nE 's/^ARG BASE_IMAGE=(.*)$/\1/p' "$ROOT/$EXAMPLE/Dockerfile")
if [ -n "$BASE_IMAGE" ]; then
    PARENT_REF=$BASE_IMAGE
    log "diagnostic parent override: $PARENT_REF (not acceptance)"
else
    PARENT_REF=$PINNED_PARENT
fi
PHASE=parent
if ! run_bounded parent-pull "$PULL_SECONDS" "$LOG_DIR/parent-pull.log" \
        "$DOCKER" pull --platform "$PLATFORM" "$PARENT_REF"; then
    fail "parent image unavailable or not pullable: $PARENT_REF"
    exit 1
fi
# Resolving the parent is part of the phase, not logging: a build against an
# unresolvable or misreported parent is not the evidence this run claims.
if ! run_bounded parent-inspect "$RUN_SECONDS" "$LOG_DIR/parent-inspect.log" \
        "$DOCKER" image inspect --format \
        '{{.Id}} {{index .RepoDigests 0}} {{.Os}}/{{.Architecture}}' "$PARENT_REF"; then
    fail "the parent image could not be inspected: $PARENT_REF"
    exit 1
fi
PARENT_ID=''; PARENT_DIGEST=''; PARENT_PLATFORM=''
read -r PARENT_ID PARENT_DIGEST PARENT_PLATFORM <"$LOG_DIR/parent-inspect.log" || true
if ! printf '%s' "$PARENT_ID" | grep -qE '^sha256:[0-9a-f]{64}$'; then
    fail "the parent inspection reported no image ID: '$PARENT_ID'"
    exit 1
fi
if ! printf '%s' "$PARENT_DIGEST" | grep -qE '@sha256:[0-9a-f]{64}$'; then
    fail "the parent inspection reported no repository digest: '$PARENT_DIGEST'"
    exit 1
fi
if [ "$PARENT_PLATFORM" != "$PLATFORM" ]; then
    fail "the parent inspection reported $PARENT_PLATFORM, not the requested $PLATFORM"
    exit 1
fi
log "parent $PARENT_REF -> $PARENT_ID $PARENT_DIGEST $PARENT_PLATFORM"
pass 'parent image available, resolved and of the requested platform'

PHASE=build
BUILD_MODE=''
if [ "$COLD_BUILD" -eq 1 ]; then BUILD_MODE=' cold=yes'; fi
log "building the example with --build-arg BASE_IMAGE=$PARENT_REF$BUILD_MODE"
OWNED_TAG=1
BUILD_FLAGS=()
if [ "$COLD_BUILD" -eq 1 ]; then BUILD_FLAGS=(--no-cache); fi
if run_bounded build "$BUILD_SECONDS" "$LOG_DIR/build.log" \
        "$DOCKER" build --builder "$runner" --platform "$PLATFORM" --progress plain \
        ${BUILD_FLAGS[@]+"${BUILD_FLAGS[@]}"} \
        --label "$LABEL" --build-arg "BASE_IMAGE=$PARENT_REF" -t "$TAG" "$ROOT/$EXAMPLE"; then
    pass 'example image built'
else
    fail 'example image build failed'
    exit 1
fi
# A cached build proves nothing about the installer, so whether the toolchain
# install stage actually ran is part of the record rather than an invisible
# property of the local cache.
install_step=$(awk '/^#[0-9]+ \[/ && /install-rust\.sh/ { sub(/^#/, "", $1); print $1; exit }' \
    "$LOG_DIR/build.log")
if [ -z "$install_step" ]; then
    record 'build-toolchain-install step=unknown'
elif grep -qxF "#$install_step CACHED" "$LOG_DIR/build.log"; then
    record 'build-toolchain-install cached=1'
    log 'NOTE [build]: the toolchain install stage came from the build cache; pass --no-cache for a cold build'
else
    record 'build-toolchain-install cached=0'
fi
PHASE=image
# The identity and size of the built image are required evidence, so a failed or
# unparsable inspection fails the phase: an `unknown` ID next to a PASS is how a
# broken run used to be recorded as a successful one.
if ! run_bounded image-inspect "$RUN_SECONDS" "$LOG_DIR/image-inspect.log" \
        "$DOCKER" image inspect --format '{{.Id}} {{.Size}}' "$TAG"; then
    fail "the built image could not be inspected: $TAG"
    exit 1
fi
IMAGE_ID=''; IMAGE_SIZE=''
read -r IMAGE_ID IMAGE_SIZE <"$LOG_DIR/image-inspect.log" || true
# Docker reports an image identity as `sha256:<64 hex>`, the same shape the parent
# inspection is held to. Anything else -- a word, a short or mixed-case digest, a
# second field that happens to be a number -- is not an image identity and cannot
# be recorded as evidence of what was built.
if ! printf '%s' "$IMAGE_ID" | grep -qE '^sha256:[0-9a-f]{64}$'; then
    fail "the image inspection reported no image ID: '$IMAGE_ID'"
    exit 1
fi
if ! printf '%s' "$IMAGE_SIZE" | grep -qE '^[1-9][0-9]*$'; then
    fail "the image inspection reported no image size: '$IMAGE_SIZE'"
    exit 1
fi
log "image $IMAGE_ID size=$IMAGE_SIZE parent=$PARENT_REF id=$PARENT_ID"
record "image id=$IMAGE_ID size=$IMAGE_SIZE parent=$PARENT_REF parent-id=$PARENT_ID"

PHASE=inspect
run_bounded inspect-env "$RUN_SECONDS" "$LOG_DIR/inspect.log" \
    "$DOCKER" run --rm --network none --label "$LABEL" --entrypoint /bin/bash "$TAG" -c \
    'env | sort; stat -c "%a %U:%G %n" /opt /opt/rustup /opt/cargo /opt/cargo/bin /opt/cargo/bin/rustup /opt/rustup/settings.toml 2>&1; ls -d /run/rust-verify 2>&1 || true; ls /tmp/rust_toolchain.py /tmp/install-rust.sh 2>&1 || true; ls -d /tmp/rust-prepare-* /tmp/rust-install-* /tmp/rust-verify-* 2>&1 || true' \
    || true
if grep -qx 'RUSTUP_AUTO_INSTALL=0' "$LOG_DIR/inspect.log" &&
    ! grep -q '^CARGO_HOME=' "$LOG_DIR/inspect.log" &&
    ! grep -q '^RUSTUP_TOOLCHAIN=' "$LOG_DIR/inspect.log"; then
    pass 'image ENV: RUSTUP_AUTO_INSTALL=0, no CARGO_HOME and no forced toolchain'
else
    fail 'image ENV contract'
fi
if grep -q '^755 root:root /opt/cargo/bin/rustup$' "$LOG_DIR/inspect.log" &&
    grep -q '^644 root:root /opt/rustup/settings.toml$' "$LOG_DIR/inspect.log" &&
    ! grep -qE '^(7[0-9]7|77[0-9]|[0-7]7[0-9]|[0-7]77) ' "$LOG_DIR/inspect.log" &&
    grep -q 'No such file' "$LOG_DIR/inspect.log"; then
    pass 'shared /opt is world-readable and no build helper/key remains in the image'
else
    fail 'shared /opt ownership/modes or leftover build material'
fi
if grep -qE '^/tmp/(rust-prepare|rust-install|rust-verify)-' "$LOG_DIR/inspect.log"; then
    fail 'leftover toolchain scratch in the image'
else
    pass 'no toolchain scratch, downloads or bootstrap remains in the image'
fi

# --- phase 2: verifier bundle, no network ------------------------------------
PHASE=verifier
mkdir -p "$BUNDLE"
cp "$ROOT/$EXAMPLE/verify-toolchain.sh" "$ROOT/$EXAMPLE/rust_toolchain.py" "$BUNDLE/"
cp -R "$ROOT/$EXAMPLE/rust-toolchain" "$BUNDLE/rust-toolchain"
if ! run_bounded pin-check "$RUN_SECONDS" "$LOG_DIR/pin-check.log" \
        env PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/$EXAMPLE/rust_toolchain.py" \
        check "$ROOT/$EXAMPLE/rust-toolchain"; then
    fail 'the committed pin data does not verify; no native phase is meaningful'
    exit 1
fi
channel=$(cat "$LOG_DIR/pin-check.log")
# The container is created before it is started so a failure between the two
# still leaves a named, labelled, removable resource.
OWNED_CONTAINER=1
if run_bounded verifier-create "$RUN_SECONDS" "$LOG_DIR/verifier-create.log" \
        "$DOCKER" create --name "$CONTAINER" --label "$LABEL" --network none \
        -e "RUST_TOOLCHAIN=$channel" -e VERUS_DIR=/opt/verus -e PYTHONDONTWRITEBYTECODE=1 \
        --entrypoint /bin/bash "$TAG" /run/rust-verify/verify-toolchain.sh; then
    if run_bounded verifier-copy "$RUN_SECONDS" "$LOG_DIR/verifier-copy.log" \
            "$DOCKER" cp "$BUNDLE" "$CONTAINER:/run/rust-verify"; then
        if run_bounded verifier-run "$RUN_SECONDS" "$LOG_DIR/verifier-run.log" \
                "$DOCKER" start -a "$CONTAINER"; then
            pass 'network-none installed-state verifier passed'
        else
            fail 'network-none installed-state verifier failed'
        fi
    else
        fail 'could not deliver the verifier bundle'
    fi
else
    fail 'could not create the verifier container'
fi
if run_bounded verifier-remove "$RUN_SECONDS" "$LOG_DIR/verifier-remove.log" \
        "$DOCKER" rm -f "$CONTAINER"; then
    # Confirmed removal clears ownership, so the EXIT cleanup does not try to
    # remove the same container again.
    OWNED_CONTAINER=0
else
    fail 'could not remove the verifier container'
fi

# --- phase 3: runtime legs ---------------------------------------------------
# An empty writable HOME (tmpfs) as root and as numeric uid/gid pairs, read-only
# /opt from the image, no network. cargo/rustc/fmt/clippy must work from
# preinstalled content and the musl std must compile.
PHASE=runtime
runtime_leg() { # LABEL UID:GID CMD
    local label=$1 identity=$2 command=$3
    run_bounded "runtime-$label" "$RUN_SECONDS" "$LOG_DIR/runtime-$label.log" \
        "$DOCKER" run --rm --network none --label "$LABEL" -u "$identity" \
        --tmpfs "/home/agent-$RUN_ID:rw,mode=1777" \
        -e "HOME=/home/agent-$RUN_ID" -e TMPDIR=/tmp \
        -v "$ROOT/$FIXTURES/project:/work:ro" \
        -v "$ROOT/$FIXTURES/musl.rs:/fixtures/musl.rs:ro" \
        -w /tmp --entrypoint /bin/bash "$TAG" -c "$command"
}
runtime_checks="cp -R /work/. /tmp/project && cd /tmp/project &&
    cargo build --offline && cargo test --offline &&
    cargo clippy --offline -- -D warnings && cargo fmt -- --check &&
    rustc --version && printf 'fn main() { println!(\"ok\"); }\n' > /tmp/main.rs &&
    rustc -o /tmp/sample /tmp/main.rs && /tmp/sample &&
    rustc --crate-type lib --target $MUSL -o /tmp/libmusl.rlib /fixtures/musl.rs"
for leg in 'root:0:0' 'one:12345:23456' 'two:9999:9999'; do
    label=${leg%%:*}
    rest=${leg#*:}
    if runtime_leg "$label" "${rest%%:*}:${rest##*:}" "$runtime_checks"; then
        pass "runtime leg $label ($rest): build/test/clippy/fmt/rustc/musl without network"
    else
        fail "runtime leg $label ($rest)"
    fi
done

PHASE=toolchain-file
sed -e "s|@CHANNEL@|$channel|" -e "s|@MUSL@|$MUSL|" \
    "$ROOT/$FIXTURES/project/rust-toolchain.toml.in" >"$LOG_DIR/rust-toolchain.toml"
sed -e "s|@CHANNEL@|$channel|" "$ROOT/$FIXTURES/project/extra-toolchain.toml.in" \
    >"$LOG_DIR/extra-toolchain.toml"
mkdir -p "$LOG_DIR/toolchain-files"
mv "$LOG_DIR/rust-toolchain.toml" "$LOG_DIR/extra-toolchain.toml" "$LOG_DIR/toolchain-files/"
if run_bounded toolchain-file "$RUN_SECONDS" "$LOG_DIR/toolchain-file.log" \
        "$DOCKER" run --rm --network none --label "$LABEL" -u 12345:23456 \
        -e HOME=/tmp/home -e RUSTUP_AUTO_INSTALL=0 \
        -v "$ROOT/$FIXTURES/project:/work:ro" -v "$LOG_DIR/toolchain-files:/toolchain:ro" \
        -w /tmp --entrypoint /bin/bash "$TAG" -c \
        'mkdir -p /tmp/home && cp -R /work/. /tmp/project && cd /tmp/project &&
         cp /toolchain/rust-toolchain.toml . &&
         cargo build --offline && cargo clippy --offline -- -D warnings &&
         cargo fmt -- --check && cargo test --offline'; then
    pass 'project rust-toolchain.toml with the selected channel, clippy/rustfmt and musl succeeds cold'
else
    fail 'project rust-toolchain.toml leg'
fi

# --- phase 4: first use without a distribution server ----------------------
PHASE=first-use
first_use() { # LABEL EXPECT_STATUS EXPECT_REQUESTS CMD
    local label=$1 expected=$2 requests=$3 command=$4 status=0 observed=0 matched=0
    run_bounded "first-use-$label" "$RUN_SECONDS" "$LOG_DIR/first-use-$label.log" \
        "$DOCKER" run --rm --network none --label "$LABEL" -u 12345:23456 \
        -e HOME=/tmp/home -e RUSTUP_AUTO_INSTALL=0 \
        -v "$ROOT/$FIXTURES/project:/work:ro" -v "$LOG_DIR/toolchain-files:/toolchain:ro" \
        -w /tmp --entrypoint /bin/bash "$TAG" -c "mkdir -p /tmp/home; $command" || status=$?
    if [ -s "$LOG_DIR/first-use-$label.log" ]; then
        observed=$(grep -c '/dist/' "$LOG_DIR/first-use-$label.log" || true)
    fi
    record "first-use-$label status=$status dist-requests=$observed"
    if [ "$expected" = '!' ] && [ "$status" -ne 0 ]; then matched=1; fi
    if [ "$expected" != '!' ] && [ "$status" -eq "$expected" ]; then matched=1; fi
    if [ "$matched" -eq 1 ] && [ "$observed" -eq "$requests" ]; then
        pass "$label: status $status, $observed distribution requests"
    else
        fail "$label: status $status, $observed distribution requests"
    fi
}
first_use absent-tool '!' 0 'cargo +0.0.0 --version'
first_use extra-target-declared 0 0 \
    'cp -R /work/. /tmp/p && cd /tmp/p && cp /toolchain/extra-toolchain.toml rust-toolchain.toml && cargo build --offline'
first_use extra-target-compile '!' 0 \
    'cp -R /work/. /tmp/p && cd /tmp/p && cp /toolchain/extra-toolchain.toml rust-toolchain.toml && cargo build --offline --target wasm32-unknown-unknown'
first_use missing-tool '!' 0 'cp -R /work/. /tmp/p && cd /tmp/p && cargo miri --version'

# --- phase 5: the first-use policy, observed via an internal canary ---------
# The canary network has no route off the daemon, so a request that reaches it is
# the only request that can be made. The guest program
# (`$FIXTURES/canary-guest.sh`) clones the shared homes into writable scratch
# copies, applies the leg's persisted setting, then runs the leg's setup and
# command as separate `bash -c` programs. It reports a parse failure (98) or a
# setup failure (99) distinctly, so a leg that never reached the policy under test
# can never be counted as either a refusal or an acceptance.
PHASE=canary
canary_leg() { # LABEL IDENTITY PERSISTED EXPECT_STATUS EXPECT_REQUESTS SETUP COMMAND
    local label=$1 identity=$2 persisted=$3 expected=$4 requests=$5 setup=$6 command=$7
    local status=0 before=0 after=0 observed=0 verdict log
    before=$(grep -c '^request' "$LOG_DIR/canary-server.log" 2>/dev/null || true)
    log=$LOG_DIR/canary-$label-$identity.log
    run_bounded "canary-$label-$identity" "$RUN_SECONDS" "$log" \
        "$DOCKER" run --rm --network "$NETWORK_NAME" --label "$LABEL" -u "$identity" \
        -e HOME=/tmp/home -e RUSTUP_DIST_SERVER=http://canary:8765 \
        -e RUSTUP_UPDATE_ROOT=http://canary:8765/update \
        -v "$ROOT/$FIXTURES:/fixtures:ro" -v "$ROOT/$FIXTURES/project:/work:ro" \
        -v "$LOG_DIR/toolchain-files:/toolchain:ro" \
        -w /tmp --entrypoint /bin/bash "$TAG" \
        /fixtures/canary-guest.sh "$persisted" "$setup" "$command" || status=$?
    run_bounded "canary-server-$label-$identity" "$RUN_SECONDS" \
        "$LOG_DIR/canary-server.log" "$DOCKER" logs "$CANARY_NAME" || true
    after=$(grep -c '^request' "$LOG_DIR/canary-server.log" 2>/dev/null || true)
    observed=$((after - before))
    cp "$LOG_DIR/canary-server.log" "$LOG_DIR/canary-requests-$label-$identity.log" || true
    record "canary-$label-$identity status=$status dist-requests=$observed"
    verdict=$(canary_verdict "$expected" "$status")
    case "$verdict" in
        harness-parse)
            fail "canary $label as $identity: the guest program failed to parse (status $status, $observed requests)"
            return 0 ;;
        harness-setup)
            fail "canary $label as $identity: the leg's own setup failed (status $status, $observed requests)"
            return 0 ;;
        harness-launch)
            fail "canary $label as $identity: the guest command could not be launched (status $status, $observed requests)"
            return 0 ;;
        matched) ;;
        *) ;;  # a mismatched status is reported below with its request count
    esac
    local requests_ok=0
    if [ "$requests" = 'at-least-1' ] && [ "$observed" -ge 1 ]; then requests_ok=1; fi
    if [ "$requests" = 'none' ] && [ "$observed" -eq 0 ]; then requests_ok=1; fi
    if [ "$verdict" = matched ] && [ "$requests_ok" -eq 1 ]; then
        pass "canary $label as $identity: status $status, $observed distribution requests"
    else
        fail "canary $label as $identity: status $status, $observed distribution requests"
    fi
}
OWNED_NETWORK=1
if run_bounded canary-network "$RUN_SECONDS" "$LOG_DIR/canary-network.log" \
        "$DOCKER" network create --internal --label "$LABEL" "$NETWORK_NAME"; then
    OWNED_CANARY=1
    if run_bounded canary-start "$RUN_SECONDS" "$LOG_DIR/canary-start.log" \
            "$DOCKER" run -d --name "$CANARY_NAME" --label "$LABEL" \
            --network "$NETWORK_NAME" --network-alias canary \
            -v "$ROOT/$FIXTURES/canary.py:/canary.py:ro" --entrypoint /usr/bin/python3 \
            "$TAG" /canary.py 8765; then
        sleep 2
        for identity in 0:0 12345:23456; do
            while IFS='|' read -r label persisted expected requests setup command; do
                [ -n "$label" ] || continue
                canary_leg "$label" "$identity" "$persisted" "$expected" "$requests" \
                    "$setup" "$command"
            done <<EOF
$(canary_legs)
EOF
        done
    else
        fail 'could not start the canary recorder'
    fi
else
    fail 'could not create the internal canary network'
fi

# --- phase 6: Verus ---------------------------------------------------------
PHASE=verus
if [ "$ARCH" = amd64 ]; then
    if run_bounded verus "$RUN_SECONDS" "$LOG_DIR/verus.log" \
            "$DOCKER" run --rm --network none --label "$LABEL" -u 12345:23456 -e HOME=/tmp/home \
            -v "$ROOT/$FIXTURES:/fixtures:ro" -w /tmp --entrypoint /bin/bash "$TAG" -c \
            'mkdir -p /tmp/home /tmp/v && cp /fixtures/verus-smoke.rs /tmp/v/smoke.rs &&
             cd /tmp/v && /opt/verus/verus-x86-linux/verus --crate-type=lib smoke.rs'; then
        if grep -Eq 'verification results:: [1-9][0-9]* verified, 0 errors' "$LOG_DIR/verus.log"; then
            pass 'independent native Verus oracle verified a real contract'
        else
            fail 'independent Verus oracle produced no positive verification count'
        fi
    else
        fail 'independent Verus oracle failed'
    fi
else
    if run_bounded verus-absent "$RUN_SECONDS" "$LOG_DIR/verus-absent.log" \
            "$DOCKER" run --rm --network none --label "$LABEL" --entrypoint /bin/bash "$TAG" \
            -c 'test -d /opt/verus'; then
        fail 'arm64 must not carry a Verus payload'
    else
        pass 'arm64: no Verus payload installed (documented no-op), nothing claimed'
    fi
fi

# The installer's digest refusal is a control of its own: a wrong digest must
# fail the amd64 build before anything is unzipped, and stay a clean no-op on
# arm64 where upstream publishes no asset.
PHASE=verus-digest
OWNED_MUTANT_TAG=1
WRONG_DIGEST=$(printf '0%.0s' $(seq 1 64))
digest_log=$LOG_DIR/verus-digest.log
if [ "$ARCH" = amd64 ]; then
    if run_bounded verus-digest "$BUILD_SECONDS" "$digest_log" \
            "$DOCKER" build --builder "$runner" --platform "$PLATFORM" --progress plain \
            --label "$LABEL" --build-arg "BASE_IMAGE=$PARENT_REF" \
            --build-arg "VERUS_SHA256=$WRONG_DIGEST" -t "$MUTANT_TAG" "$ROOT/$EXAMPLE"; then
        fail 'a wrong Verus digest must fail the build'
    elif grep -qE 'sha256sum|FAILED|checksum|digest' "$digest_log" &&
        ! grep -q 'install-verus: extracting' "$digest_log"; then
        # The marker is the installer's own named step; the count of *actual*
        # extraction calls is established by the daemon-free instrumented control
        # in --self-test, because `unzip -q` itself prints nothing.
        pass 'a wrong Verus digest fails the build before extraction'
    else
        fail 'the wrong-digest build failed without the digest refusal or after extracting'
    fi
else
    if run_bounded verus-digest "$BUILD_SECONDS" "$digest_log" \
            "$DOCKER" build --builder "$runner" --platform "$PLATFORM" --progress plain \
            --label "$LABEL" --build-arg "BASE_IMAGE=$PARENT_REF" \
            --build-arg "VERUS_SHA256=$WRONG_DIGEST" -t "$MUTANT_TAG" "$ROOT/$EXAMPLE"; then
        if ! grep -qE 'verus-lang|curl .*\.zip' "$digest_log"; then
            pass 'arm64: a wrong Verus digest stays a clean no-op with no fetch'
        else
            fail 'arm64 fetched a Verus asset'
        fi
    else
        fail 'arm64 must not fail the build on a Verus digest it never uses'
    fi
fi

# --- phase 7: hard build negative ------------------------------------------
PHASE=negative
MUTANT_ROOT="$LOG_DIR/mutant"
mkdir -p "$MUTANT_ROOT"
cp -R "$ROOT/$EXAMPLE/." "$MUTANT_ROOT/"
printf ' ' >>"$MUTANT_ROOT/rust-toolchain/channel.toml"
if run_bounded build-mutant "$BUILD_SECONDS" "$LOG_DIR/build-mutant.log" \
        "$DOCKER" build --no-cache --builder "$runner" --platform "$PLATFORM" --progress plain \
        --label "$LABEL" --build-arg "BASE_IMAGE=$PARENT_REF" -t "$MUTANT_TAG" "$MUTANT_ROOT"; then
    fail 'a mutated committed manifest byte must fail the build'
elif grep -qE 'bytes differ from the committed digest|rust_toolchain' "$LOG_DIR/build-mutant.log"; then
    pass 'changed manifest byte fails the build through the named pin error'
else
    fail 'build failed without the named pin error'
fi

PHASE=summary
record "summary tests=$TESTS failed=$FAILED platform=$PLATFORM image=$IMAGE_ID \
parent=$PARENT_REF parent-id=${PARENT_ID:-unknown}"
log "=== $TESTS checks, $FAILED failed (logs: $LOG_DIR) ==="
[ "$FAILED" -eq 0 ]
