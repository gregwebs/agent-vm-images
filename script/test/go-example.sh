#!/usr/bin/env bash
# Native build/runtime evidence for the go-dev example and its gopls closure (#103).
#
# This is the single entrypoint for a native host and for the one-off PR workflow:
# all evidence logic lives here, never in YAML. It builds the real recipe (the
# committed default parent unless --base-image is given as a diagnostic), inspects
# the image, delivers the same verifier contract the Dockerfile uses over a
# read-only bundle, and then runs the Go loop, a real editor-facing LSP session,
# the first-use canary legs and the hard content-replacement build negative with
# real tools on the host's native architecture.
#
# Prerequisites: a native Linux daemon of the requested platform, a builder that is
# daemon-backed and bound to that same daemon (driver/endpoint are inspected, not
# assumed), python3, git and host-watchdog.py. The amd64 row must come from a native
# amd64 host; QEMU is not acceptance.
#
# Usage: bash script/test/go-example.sh [--platform linux/amd64|linux/arm64]
#                                      [--base-image EXACT_REF] [--no-cache]
#        bash script/test/go-example.sh --self-test
#
# `--no-cache` builds the example from scratch. Use it for acceptance evidence: a
# cached gopls-install stage proves nothing about the installer, so the suite
# records `build-gopls-install cached=0|1` for every run and refuses a cached stage
# in a cold run.
# Status: 0 all phases passed, 1 a phase or the cleanup failed, 2 usage,
#         124 the suite watchdog fired, 128+N on an inherited signal.
#
# Budgets: every host-side command runs under host-watchdog.py, the whole suite
# re-executes itself under one supervisor, and cleanup has its own deadline. The
# per-command numbers are provisional floors (this harness is accepted by
# measurement, not by relaxing a bound), so a slow machine fails loudly.
#
# Cleanup is state-aware: only a resource-specific absence is a successful removal,
# a genuine daemon, permission or in-use error stays a failure, and only the fixture
# tags this invocation registered are removed -- a label proves provenance, not
# ownership of every reference, so an image that keeps someone else's tag is
# retained and named rather than force-removed by id.
set -euo pipefail

ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
EXAMPLE=examples/layers/go-dev
FIXTURES=script/test/fixtures/go-example
DOCKER=${DOCKER:-docker}
WATCHDOG="$ROOT/script/test/host-watchdog.py"

SUITE_SECONDS=7200          # two builds plus runtime, LSP and canary phases
SUPERVISOR_GRACE_SECONDS=90 # > cleanup budget (60 s) + escalation + trap entry
DAEMON_SECONDS=60
PULL_SECONDS=900
BUILD_SECONDS=2400          # each build, positive and negative
RUN_SECONDS=300             # one runtime/verifier/canary container
LSP_SECONDS=300
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
        *) echo "usage: go-example.sh [--platform linux/amd64|linux/arm64] [--base-image REF]" >&2
           echo "                       [--no-cache]" >&2
           echo "       go-example.sh --self-test" >&2
           exit 2 ;;
    esac
done
if [ "$SELF_TEST" -eq 1 ] && [ "$COLD_BUILD" -eq 1 ]; then
    echo 'go-example: --no-cache is a native-run option' >&2
    exit 2
fi
if [ "$SELF_TEST" -eq 0 ]; then
    case "$PLATFORM" in
        linux/amd64) ARCH=amd64 ;;
        linux/arm64) ARCH=arm64 ;;
        *) echo 'go-example: unsupported platform' >&2; exit 2 ;;
    esac
fi

# --- first-use canary leg table --------------------------------------------
# One line per leg: LABEL|EXPECTED_STATUS|EXPECTED_REQUESTS|EXPECTED_OUTPUT|COMMAND
# `!` means "any nonzero status", `any` means "either", `-` means no output
# pattern. The request column is not a count but the fetch identity that must be
# observed: `none` means the recorder saw no request of any path, and a module
# path means at least one request for that path -- a request for something else
# says nothing about a toolchain fetch. The command runs in a guest whose GOPROXY
# points at the canary recorder, so a fetch is observable; the module in /work
# asks for a toolchain this image does not carry. The output patterns are the
# tools' own words: `go` names the pinned-toolchain refusal and the download it
# would make if the policy were weakened.
canary_legs() {
    cat <<'LEGS'
toolchain-refused|!|none|GOTOOLCHAIN=local|cp -R /work/. /tmp/p && cd /tmp/p && go build ./...
gopls-refused|any|none|GOTOOLCHAIN=local|cp -R /work/. /tmp/p && cd /tmp/p && gopls check main.go
toolchain-opt-out|!|golang.org/toolchain|go: downloading go1.99.0|cp -R /work/. /tmp/p && cd /tmp/p && GOTOOLCHAIN=auto go build ./...
LEGS
}

# The guest program's status contract in one place: 126/127 mean the command could
# not be launched at all, so a leg that expects a failure cannot pass that way. A
# watchdog timeout (124) or a signal-derived status (128+N) is the harness's own
# answer, never the guest program's, so it is named separately instead of being
# accepted as "some failure".
canary_verdict() { # EXPECTED_STATUS STATUS
    local expected=$1 status=$2
    if [ "$status" -eq 124 ]; then echo timeout; return 0; fi
    if [ "$status" -ge 129 ]; then echo signal; return 0; fi
    if [ "$expected" = '!' ] && { [ "$status" -eq 126 ] || [ "$status" -eq 127 ]; }; then
        echo harness-launch; return 0
    fi
    if [ "$expected" = 'any' ]; then echo matched; return 0; fi
    if [ "$expected" = '!' ] && [ "$status" -ne 0 ]; then echo matched; return 0; fi
    if [ "$expected" != '!' ] && [ "$status" -eq "$expected" ]; then echo matched; return 0; fi
    echo mismatched
}

# The recorder prints one `request <path>` line per fetch it answers. Counting is
# only meaningful for a log that was really read; the caller owns that decision and
# an unreadable log must never look like "no requests". A named module path is the
# request identity, so it is anchored to the start of the request path: a request
# for `/notgolang.org/toolchain/...` is not a fetch of `golang.org/toolchain`.
canary_request_count() { # LOG [PATH_PATTERN]
    local log=$1 pattern=${2:-} count=0
    [ -f "$log" ] || { printf '0\n'; return 0; }
    if [ -n "$pattern" ]; then
        count=$(grep -cE -- "^request /$pattern" "$log" || true)
    else
        count=$(grep -cE '^request ' "$log" || true)
    fi
    printf '%s\n' "${count:-0}"
}

# One first-use verdict, split out so every wrong layer can be exercised without a
# daemon. A leg passes only when the recorder's log was really read (an error from
# `docker logs` is not zero requests), the status contract matched, and the observed
# fetches satisfy the expected identity: no request at all, or at least one request
# for the named module path.
first_use_verdict() { # EXPECTED_STATUS STATUS LOG_STATUS EXPECT_REQUESTS LOG
    local expected=$1 status=$2 log_status=$3 want=$4 log=$5 verdict total named
    if [ "$log_status" -ne 0 ]; then
        printf 'the canary recorder log could not be read (status %s), so no request count is evidence\n' \
            "$log_status"
        return 0
    fi
    verdict=$(canary_verdict "$expected" "$status")
    case "$verdict" in
        timeout) printf 'the leg timed out (status 124), which is not a refusal\n'; return 0 ;;
        signal) printf 'the leg died from a signal (status %s)\n' "$status"; return 0 ;;
        harness-launch) printf 'the command could not be launched (status %s)\n' "$status"; return 0 ;;
        mismatched) printf 'status %s does not satisfy the expected %s\n' "$status" "$expected"; return 0 ;;
    esac
    total=$(canary_request_count "$log")
    case "$want" in
        none)
            [ "$total" -eq 0 ] || { printf '%s proxy requests, not zero\n' "$total"; return 0; } ;;
        *)
            named=$(canary_request_count "$log" "$want")
            [ "$named" -ge 1 ] || {
                # The pattern may be an ERE; the message names the path it means.
                printf '%s proxy requests, none for %s\n' "$total" "${want//\\/}"; return 0; }
            ;;
    esac
    printf 'ok\n'
}

# A cold build of the mutated closure is evidence only for the failure it names. A
# timeout, a signal, a daemon or permission failure, a retained-archive digest
# failure and any generic error fail the build for some other reason, so none of
# them may stand in for Go's checksum refusal.
mutant_build_verdict() { # STATUS LOG
    local status=$1 log=$2
    if [ "$status" -eq 0 ]; then
        printf 'the mutated closure built and published\n'; return 0
    fi
    if [ "$status" -eq 124 ]; then printf 'the mutant build timed out (status 124)\n'; return 0; fi
    if [ "$status" -ge 129 ]; then printf 'the mutant build died from a signal (status %s)\n' "$status"; return 0; fi
    if grep -qF 'verifying github.com/fatih/camelcase@v1.0.0: checksum mismatch' "$log" &&
        grep -qF 'SECURITY ERROR' "$log"; then
        printf 'ok\n'; return 0
    fi
    if grep -qiE 'cannot connect to the docker daemon|permission denied|unauthorized|context .* not found' "$log"; then
        printf 'the mutant build failed at the daemon or client (status %s), which is not evidence\n' "$status"
        return 0
    fi
    if grep -qiE 'computed checksum did not match|sha256sum: WARNING|archive digest' "$log"; then
        printf 'the mutant build failed at a retained archive digest (status %s), which is not evidence\n' "$status"
        return 0
    fi
    printf 'the mutant build failed without the named camelcase checksum mismatch and SECURITY ERROR (status %s)\n' \
        "$status"
}

# "Published no image" needs Docker's own absence answer. A daemon, context or
# permission error leaves publication unresolved, so it is reported as unresolved
# rather than as confirmed absence, and the owned tag stays registered for cleanup.
mutant_absent_verdict() { # STATUS LOG
    local status=$1 log=$2
    if [ "$status" -eq 0 ]; then
        printf 'the failed mutant build left an image behind\n'; return 0
    fi
    if [ "$status" -eq 124 ]; then
        printf 'the mutant tag inspection timed out (status 124), so absence is not established\n'
        return 0
    fi
    if [ "$status" -ge 129 ]; then
        printf 'the mutant tag inspection died from a signal (status %s)\n' "$status"; return 0
    fi
    if grep -qiE 'no such image|no such object|image [^ ]+ not found' "$log"; then
        printf 'ok\n'; return 0
    fi
    printf 'the mutant tag inspection failed (status %s) without confirming absence: %s\n' \
        "$status" "$(head -1 "$log" 2>/dev/null)"
}

# One deploy argument from the Dockerfile: exactly one line, or the run is not
# evidence of anything (a second ARG would make "the" pin ambiguous). Define it
# before the self-test dispatch, which uses the committed pins as its fixture.
deploy_version() { # NAME
    local values
    values=$(sed -nE "s/^ARG $1=(.*)$/\\1/p" "$ROOT/$EXAMPLE/Dockerfile")
    [ -n "$values" ] || { echo "go-example: $ROOT/$EXAMPLE/Dockerfile lacks ARG $1" >&2; exit 2; }
    [ "$(printf '%s\n' "$values" | grep -c .)" -eq 1 ] ||
        { echo "go-example: ARG $1 is not defined exactly once" >&2; exit 2; }
    printf '%s\n' "$values"
}

selftest_expect() { # ACTUAL EXPECTED NAME LOG
    if [ "$1" -ne "$2" ]; then
        echo "self-test: $3: expected $2, got $1" >&2
        cat "$4" >&2
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

selftest_expect_no_grep() { # PATTERN LOG NAME
    if grep -qF -- "$1" "$2"; then
        echo "self-test: $3: $1 must not appear" >&2
        cat "$2" >&2
        exit 1
    fi
    echo "self-test: ok   $3"
}

selftest_expect_text() { # NAME ACTUAL EXPECTED
    if [ "$2" != "$3" ]; then
        echo "self-test: FAIL $1: expected '$3', got '$2'" >&2
        exit 1
    fi
    echo "self-test: ok   $1"
}

selftest_expect_contains() { # HAYSTACK NEEDLE NAME
    case "$1" in
        *"$2"*) echo "self-test: ok   $3" ;;
        *) echo "self-test: FAIL $3: expected '$2' in: $1" >&2; exit 1 ;;
    esac
}

# --- self-test: the harness's own controls, no daemon -----------------------
# Three families. The fake docker proves the harness fails closed and reports
# honestly: an unavailable daemon, a non-native server, an unavailable or remote or
# misbound builder, an interrupted run, the inspected parent handed to every build
# with the verifier's own env values, an unreadable or wrong canary observation, the
# negative build's five wrong layers, and a retagged image the cleanup must not
# delete. The fake LSP server proves the editor leg discriminates a working load from
# every named defect (including a destructive formatting edit). The pure verdict
# controls exercise the first-use, negative-build and cleanup rules directly.
# None of them claims a passing acceptance.
SELFTEST_SCRATCH=''
self_test() {
    local scratch rc=0 out diagnostic zeroes
    SELFTEST_SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/go-example-selftest.XXXXXX")
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

    echo 'self-test: a non-native daemon is refused'
    out=$scratch/foreign-arch.log
    rc=0
    FAKE_DOCKER_MODE=record FAKE_SERVER_ARCH=linux/amd64 \
        FAKE_DOCKER_LOG="$scratch/foreign-arch-docker.log" PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/arm64 >"$out" 2>&1 || rc=$?
    selftest_expect "$rc" 1 'a foreign-architecture daemon exits 1' "$out"
    selftest_expect_grep 'QEMU is not acceptance' "$out" 'the refusal names the QEMU rule'
    selftest_expect "$(grep -c '^build ' "$scratch/foreign-arch-docker.log" 2>/dev/null || true)" 0 \
        'no build is attempted on a non-native daemon' "$out"

    # The builder is where BuildKit compiles, so a native daemon plus a remote
    # builder is still not native acceptance. Each control must stop the run before
    # any build and name the builder, not the architecture. `two-node-remote` is the
    # bypass this rule has to close: the first node is the inspected daemon and the
    # second compiles elsewhere.
    echo 'self-test: the builder must be daemon-backed and bound to the inspected daemon'
    local builder_mode
    for builder_mode in remote-builder misbound-builder no-builder two-node-remote no-endpoint; do
        out=$scratch/builder-$builder_mode.log
        rc=0
        FAKE_DOCKER_MODE=$builder_mode FAKE_DOCKER_LOG="$scratch/builder-$builder_mode.log.docker" \
            PATH="$scratch/bin:$PATH" \
            bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || rc=$?
        selftest_expect "$rc" 1 "the $builder_mode builder exits 1" "$out"
        selftest_expect_grep 'BLOCKED' "$out" "the $builder_mode builder is reported as blocked"
        selftest_expect_grep 'builder' "$out" "the $builder_mode refusal names the builder"
        selftest_expect "$(grep -c '^build ' "$scratch/builder-$builder_mode.log.docker" 2>/dev/null || true)" 0 \
            "no build is attempted with the $builder_mode builder" "$out"
    done
    selftest_expect_grep "uses the 'remote' driver" "$scratch/builder-remote-builder.log" \
        'a remote driver is refused for not being daemon-backed'
    selftest_expect_grep "endpoint is 'other-daemon'" "$scratch/builder-misbound-builder.log" \
        'a builder bound to another endpoint is refused'
    selftest_expect_grep 'buildx cannot inspect' "$scratch/builder-no-builder.log" \
        'a builder that cannot be inspected is refused'
    selftest_expect_grep 'has 2 nodes' "$scratch/builder-two-node-remote.log" \
        'a two-node builder whose second node is remote is refused'
    selftest_expect_grep "endpoint 'ssh://different-host' is not 'fake-builder'" \
        "$scratch/builder-two-node-remote.log" \
        'the refusal names the remote node endpoint'
    selftest_expect_grep 'named no endpoint' "$scratch/builder-no-endpoint.log" \
        'an inspection without an endpoint is refused as unknown binding'

    echo 'self-test: a builder whose every node is the inspected daemon is accepted'
    out=$scratch/builder-two-node-local.log
    FAKE_DOCKER_MODE=two-node-local FAKE_DOCKER_LOG="$scratch/builder-two-node-local.log.docker" \
        PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect_grep 'driver=docker-container endpoint=fake-builder nodes=2' "$out" \
        'a two-node builder bound to the inspected daemon is accepted with its node count'
    selftest_expect "$(grep -c '^build ' "$scratch/builder-two-node-local.log.docker" || true)" 2 \
        'both builds run on an accepted two-node builder' "$out"
    selftest_expect "$(grep '^build ' "$scratch/builder-two-node-local.log.docker" |
        grep -c -- '--builder fake-builder' || true)" 2 \
        'every build on the accepted two-node builder names it' "$out"

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

    echo 'self-test: both builds get the inspected parent and the verifier its env'
    out=$scratch/build-args.log
    zeroes=$(printf 'a%.0s' $(seq 1 64))
    diagnostic="registry.example/diagnostic-parent@sha256:$zeroes"
    FAKE_DOCKER_MODE=record FAKE_DOCKER_LOG="$scratch/docker.log" PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 --base-image "$diagnostic" \
        >"$out" 2>&1 || true
    local builds with_parent pulls
    pulls=$(grep -c "^pull .*$diagnostic" "$scratch/docker.log" || true)
    selftest_expect "$pulls" 1 'the diagnostic parent is what gets pulled' "$out"
    builds=$(grep -c '^build ' "$scratch/docker.log" || true)
    with_parent=$(grep '^build ' "$scratch/docker.log" |
        grep -c -- "--build-arg BASE_IMAGE=$diagnostic" || true)
    # Two builds: the example itself and the mutated-closure negative.
    selftest_expect "$builds" 2 'every build runs' "$out"
    selftest_expect "$with_parent" "$builds" \
        'every build carries --build-arg BASE_IMAGE=<inspected ref>' "$out"
    selftest_expect_grep 'driver=docker endpoint=fake-builder' "$out" \
        'the accepted builder is recorded with its driver and endpoint'
    selftest_expect "$(grep '^build ' "$scratch/docker.log" |
        grep -c -- '--builder fake-builder' || true)" "$builds" \
        'every build names the inspected builder' "$out"
    selftest_expect "$(grep '^build ' "$scratch/docker.log" | tail -1 |
        grep -c -- '--no-cache' || true)" 1 \
        'the negative build is always cold' "$out"
    selftest_expect "$(grep -c -- '-e GO_VERSION=' "$scratch/docker.log" || true)" 1 \
        'the verifier receives GO_VERSION as an explicit env value' "$out"
    selftest_expect "$(grep -c -- '-e GOLANGCI_LINT_VERSION=' "$scratch/docker.log" || true)" 1 \
        'the verifier receives GOLANGCI_LINT_VERSION as an explicit env value' "$out"
    selftest_expect "$(grep -c -- '-e GOPLS_VERSION=' "$scratch/docker.log" || true)" 1 \
        'the verifier receives GOPLS_VERSION as an explicit env value' "$out"
    selftest_expect "$(grep -F '/run/go-verify/verify-toolchain.sh' "$scratch/docker.log" |
        grep -c -- '--network none' || true)" 1 \
        'the verifier container runs with no network' "$out"
    # A build that published the mutant is not a passing negative, and neither is an
    # image inspection that reported one.
    selftest_expect_grep 'FAIL [negative]: the mutant build is not evidence: the mutated closure built and published' \
        "$out" 'a successful mutant build fails the negative phase'
    selftest_expect_grep 'FAIL [negative]: mutant publication is unresolved: the failed mutant build left an image behind' \
        "$out" 'an image left behind fails the publication assertion'

    echo 'self-test: a cold build is requested and named'
    out=$scratch/cold-build.log
    FAKE_DOCKER_MODE=record FAKE_DOCKER_LOG="$scratch/cold-docker.log" PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 --no-cache >"$out" 2>&1 || true
    selftest_expect "$(grep '^build ' "$scratch/cold-docker.log" | head -1 |
        grep -c -- '--no-cache' || true)" 1 \
        '--no-cache reaches the example build' "$out"
    selftest_expect "$(grep -c 'cold=yes' "$out" || true)" 1 \
        'the cold build is named in the harness log' "$out"

    echo 'self-test: the negative build passes only for the named checksum failure'
    out=$scratch/genuine-negative.log
    FAKE_DOCKER_MODE=genuine-negative PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect_grep 'PASS [negative]: the mutated content hash fails the build with the named Go checksum error' \
        "$out" 'the genuine checksum refusal is accepted'
    selftest_expect_grep 'PASS [negative]: the failed mutant build published no image (confirmed absent)' \
        "$out" 'a confirmed absence is accepted'
    out=$scratch/fault-negative.log
    FAKE_DOCKER_MODE=fault-negative PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect_grep 'FAIL [negative]: the mutant build is not evidence: the mutant build failed at the daemon or client' \
        "$out" 'a daemon fault is not evidence of the checksum refusal'
    selftest_expect_grep 'FAIL [negative]: mutant publication is unresolved' \
        "$out" 'a daemon fault is not confirmed absence'
    selftest_expect_no_grep 'published no image (confirmed absent)' "$out" \
        'no daemon fault is reported as a published-nothing pass'

    echo 'self-test: a maintainer tag on the labelled image is never deleted'
    out=$scratch/retagged.log
    FAKE_DOCKER_MODE=retagged FAKE_DOCKER_LOG="$scratch/retagged-docker.log" \
        PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect "$(grep -c '^image rm -f' "$scratch/retagged-docker.log" || true)" 0 \
        'no image is force-removed by id' "$out"
    selftest_expect "$(grep -c '^image rm agent-vm-go-dev:go-example-' "$scratch/retagged-docker.log" || true)" 1 \
        'the registered fixture tag is removed by name' "$out"
    selftest_expect "$(grep -c 'maintainer/keep:mine' "$scratch/retagged-docker.log" || true)" 0 \
        'a reference this invocation does not own is never removed' "$out"
    selftest_expect "$(grep -c '^image rm cccccccccccc' "$scratch/retagged-docker.log" || true)" 1 \
        'a dangling labelled image is removed by id' "$out"
    local retagged_dir
    retagged_dir=$(sed -nE 's/^cleanup completed status=[0-9]+ \(logs: (.*)\)$/\1/p' "$out" | tail -1)
    selftest_expect_grep 'retained-references=maintainer/keep:mine' "$retagged_dir/durations.txt" \
        'the retained image and its foreign reference are recorded'
    selftest_expect_grep 'NOTE [cleanup]: image dddddddddddd keeps references' "$out" \
        'the retention is named in the harness log'
    selftest_expect_grep 'cleanup completed status=0' "$out" \
        'a foreign reference is retained without failing cleanup'

    echo 'self-test: the canary observation is read, and the request path must match'
    out=$scratch/canary-quiet.log
    FAKE_DOCKER_MODE=record FAKE_CANARY_LOG=quiet FAKE_CANARY_STATE="$scratch/canary-quiet.state" \
        PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect "$(grep -c 'PASS \[first-use\]: first use toolchain-refused' "$out" || true)" 2 \
        'a refusal leg passes with zero recorder requests' "$out"
    selftest_expect "$(grep -c 'PASS \[first-use\]: first use gopls-refused' "$out" || true)" 2 \
        'the gopls leg passes with zero recorder requests' "$out"
    selftest_expect "$(grep -c 'none for golang.org/toolchain' "$out" || true)" 2 \
        'the opt-out leg fails when nothing was fetched' "$out"
    out=$scratch/canary-denied.log
    FAKE_DOCKER_MODE=record FAKE_CANARY_LOG=denied FAKE_CANARY_STATE="$scratch/canary-denied.state" \
        PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect "$(grep -c 'the canary recorder log could not be read' "$out" || true)" 6 \
        'an unreadable recorder log fails every first-use leg' "$out"
    selftest_expect_no_grep 'PASS [first-use]' "$out" \
        'an unreadable recorder log never passes a first-use leg'
    out=$scratch/canary-unrelated.log
    FAKE_DOCKER_MODE=record FAKE_CANARY_LOG=unrelated FAKE_CANARY_STATE="$scratch/canary-unrelated.state" \
        PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect "$(grep -c 'none for golang.org/toolchain' "$out" || true)" 2 \
        'the opt-out leg needs a request for golang.org/toolchain, not any request' "$out"
    selftest_expect "$(grep -c '1 proxy requests, not zero' "$out" || true)" 4 \
        'an unrelated request still fails the zero-request legs' "$out"
    # A lookalike path is a request, but not for the pinned module: the anchored
    # identity must not accept it, and the zero-request legs must still see it.
    out=$scratch/canary-lookalike.log
    FAKE_DOCKER_MODE=record FAKE_CANARY_LOG=lookalike FAKE_CANARY_STATE="$scratch/canary-lookalike.state" \
        PATH="$scratch/bin:$PATH" \
        bash "${BASH_SOURCE[0]}" --platform linux/amd64 >"$out" 2>&1 || true
    selftest_expect "$(grep -c 'none for golang.org/toolchain' "$out" || true)" 2 \
        'a lookalike toolchain path does not satisfy the opt-out leg' "$out"
    selftest_expect "$(grep -c '1 proxy requests, not zero' "$out" || true)" 4 \
        'a lookalike request still fails the zero-request legs' "$out"

    echo 'self-test: the verdicts preserve statuses and confirmed absence'
    verdict_controls "$scratch" "$scratch/verdicts.log"

    echo 'self-test: the LSP client discriminates every named defect'
    lsp_smoke_controls "$scratch" "$scratch/lsp-smoke.log"

    echo 'self-test: the closure mutator is coherent and only then reaches Go'
    mutate_closure_controls "$scratch" "$scratch/mutate-closure.log"

    echo 'self-test: the verifier requires empty diagnostics from a successful check'
    verifier_diagnostics_controls "$scratch" "$scratch/verifier-diagnostics.log"

    echo 'self-test: the adapters refuse values the image cannot honor'
    adapter_guard_controls "$scratch" "$scratch/adapter-guards.log"

    echo 'self-test: the run log binds the bytes it builds, not only the commit'
    input_digest_controls "$scratch/input-digests.log"

    echo 'self-test: passed'
}

# The first-use, negative-build and cleanup rules as pure functions: each wrong
# layer is driven directly, so the controls do not depend on a fake suite reaching
# that phase by accident.
verdict_controls() { # SCRATCH LOG
    local scratch=$1 out=$2 log verdict empty=$2.empty status
    : >"$empty"
    log=$scratch/canary-toolchain.log
    cat >"$log" <<'LOG'
canary listening on 8765
request /golang.org/toolchain/@v/v0.0.1-go1.99.0.linux-amd64.zip
LOG

    verdict=$(first_use_verdict '!' 1 0 none "$empty")
    selftest_expect_text 'zero requests with a readable log passes' "$verdict" 'ok'
    verdict=$(first_use_verdict '!' 1 0 none "$log")
    selftest_expect_contains "$verdict" '1 proxy requests, not zero' \
        'a fetch under a zero-request leg fails'
    verdict=$(first_use_verdict '!' 124 0 none "$empty")
    selftest_expect_contains "$verdict" 'timed out (status 124)' \
        'a watchdog timeout is not a refusal'
    verdict=$(first_use_verdict '!' 137 0 none "$empty")
    selftest_expect_contains "$verdict" 'died from a signal (status 137)' \
        'a signal-derived status is not a refusal'
    verdict=$(first_use_verdict '!' 1 1 none "$empty")
    selftest_expect_contains "$verdict" 'could not be read (status 1)' \
        'a failed recorder read is not zero requests'
    verdict=$(first_use_verdict '!' 0 0 none "$empty")
    selftest_expect_contains "$verdict" 'status 0 does not satisfy' \
        'a zero status fails a leg that expects a failure'
    verdict=$(first_use_verdict '!' 126 0 none "$empty")
    selftest_expect_contains "$verdict" 'could not be launched' \
        'a command that could not run fails the leg'
    verdict=$(first_use_verdict 'any' 0 0 none "$empty")
    selftest_expect_text 'the gopls leg accepts either status' "$verdict" 'ok'
    verdict=$(first_use_verdict '!' 1 0 'golang\.org/toolchain' "$log")
    selftest_expect_text 'the named toolchain request satisfies the opt-out leg' "$verdict" 'ok'
    log=$scratch/canary-unrelated.log
    printf 'request /unrelated-module/@v/list\n' >"$log"
    verdict=$(first_use_verdict '!' 1 0 'golang\.org/toolchain' "$log")
    selftest_expect_contains "$verdict" 'none for golang.org/toolchain' \
        'an unrelated request does not satisfy the opt-out leg'
    # The identity is anchored to the request path: a lookalike module name is a
    # request, but not a fetch of the pinned module.
    log=$scratch/canary-lookalike.log
    printf 'request /notgolang.org/toolchain/@v/list\n' >"$log"
    verdict=$(first_use_verdict '!' 1 0 'golang\.org/toolchain' "$log")
    selftest_expect_contains "$verdict" 'none for golang.org/toolchain' \
        'a lookalike path does not satisfy the opt-out leg'

    log=$scratch/build-checksum.log
    printf 'verifying github.com/fatih/camelcase@v1.0.0: checksum mismatch\nSECURITY ERROR\n' >"$log"
    verdict=$(mutant_build_verdict 1 "$log")
    selftest_expect_text 'the named checksum failure with SECURITY ERROR is evidence' "$verdict" 'ok'
    verdict=$(mutant_build_verdict 0 "$log")
    selftest_expect_contains "$verdict" 'built and published' \
        'a successful mutant build is never evidence'
    verdict=$(mutant_build_verdict 124 "$log")
    selftest_expect_contains "$verdict" 'timed out (status 124)' \
        'a timed-out mutant build is not the checksum refusal'
    log=$scratch/build-generic.log
    printf 'ERROR: failed to build: failed to solve: exit code: 1\n' >"$log"
    verdict=$(mutant_build_verdict 1 "$log")
    selftest_expect_contains "$verdict" 'without the named camelcase checksum mismatch' \
        'a generic mutant build failure is not evidence'
    log=$scratch/build-archive.log
    printf 'sha256sum: WARNING: 1 computed checksum did NOT match\nERROR: failed to build\n' >"$log"
    verdict=$(mutant_build_verdict 1 "$log")
    selftest_expect_contains "$verdict" 'retained archive digest' \
        'an archive digest failure is not evidence'
    log=$scratch/build-daemon.log
    printf 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n' >"$log"
    verdict=$(mutant_build_verdict 1 "$log")
    selftest_expect_contains "$verdict" 'daemon or client' \
        'a daemon fault during the mutant build is not evidence'
    log=$scratch/build-partial.log
    printf 'verifying github.com/fatih/camelcase@v1.0.0: checksum mismatch\n' >"$log"
    verdict=$(mutant_build_verdict 1 "$log")
    selftest_expect_contains "$verdict" 'without the named' \
        'the checksum mismatch alone, without SECURITY ERROR, is not evidence'

    log=$scratch/tag-absent.log
    printf '[]\nError response from daemon: No such image: agent-vm-go-dev-mutant:x\n' >"$log"
    verdict=$(mutant_absent_verdict 1 "$log")
    selftest_expect_text 'a confirmed not-found supports "published no image"' "$verdict" 'ok'
    verdict=$(mutant_absent_verdict 0 "$empty")
    selftest_expect_contains "$verdict" 'left an image behind' 'a present image fails the absence assertion'
    verdict=$(mutant_absent_verdict 124 "$empty")
    selftest_expect_contains "$verdict" 'not established' 'a timeout does not establish absence'
    log=$scratch/tag-fault.log
    printf 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n' >"$log"
    verdict=$(mutant_absent_verdict 1 "$log")
    selftest_expect_contains "$verdict" 'without confirming absence' \
        'a daemon fault does not establish absence'

    # Cleanup: only a resource-specific absence may stand in for a removal.
    log=$scratch/cleanup-context.log
    printf 'docker: context "missing" not found\n' >"$log"
    status=0
    cleanup_absent container "$log" || status=$?
    selftest_expect "$status" 1 'a context error is not "the container was already gone"' "$out"
    log=$scratch/cleanup-container.log
    printf 'Error response from daemon: No such container: 2d640162fc86\n' >"$log"
    status=0
    cleanup_absent container "$log" || status=$?
    selftest_expect "$status" 0 'a resource-specific container absence is successful cleanup' "$out"
    log=$scratch/cleanup-network.log
    printf 'Error response from daemon: network go-example-net not found\n' >"$log"
    status=0
    cleanup_absent network "$log" || status=$?
    selftest_expect "$status" 0 'a resource-specific network absence is successful cleanup' "$out"
    log=$scratch/cleanup-permission.log
    printf 'Error response from daemon: permission denied\n' >"$log"
    status=0
    cleanup_absent image "$log" || status=$?
    selftest_expect "$status" 1 'a permission error is not "the image was already gone"' "$out"
    log=$scratch/cleanup-image.log
    printf 'Error response from daemon: No such image: agent-vm-go-dev:x\n' >"$log"
    status=0
    cleanup_absent image "$log" || status=$?
    selftest_expect "$status" 0 'a resource-specific image absence is successful cleanup' "$out"
    # The same classification through cleanup_remove, which must preserve a real
    # status instead of reporting successful cleanup.
    LOG_DIR=$scratch/cleanup-records
    mkdir -p "$LOG_DIR"
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_SECONDS))
    status=0
    cleanup_remove container cleanup-control \
        sh -c 'echo "docker: context \"missing\" not found" >&2; exit 3' || status=$?
    selftest_expect "$status" 3 'cleanup_remove preserves a genuine client failure' "$out"
    status=0
    cleanup_remove container cleanup-control \
        sh -c 'echo "Error response from daemon: No such container: abc" >&2; exit 1' || status=$?
    selftest_expect "$status" 0 'cleanup_remove accepts a resource-specific absence' "$out"
    # A watchdog timeout or a signal-derived status means the removal was cut short.
    # Docker's absence text is still in the log in both controls, and it must not
    # turn the interrupted removal into successful cleanup.
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_SECONDS))
    status=0
    cleanup_remove container cleanup-timeout \
        sh -c 'echo "Error response from daemon: No such container: abc" >&2; exit 124' || status=$?
    selftest_expect "$status" 124 'a watchdog timeout is never successful cleanup' "$out"
    selftest_expect_grep 'No such container' "$LOG_DIR/cleanup-cleanup-timeout.log" \
        'the timeout control log does carry the absence text'
    selftest_expect_grep 'cleanup-cleanup-timeout interrupted status=124' "$LOG_DIR/durations.txt" \
        'the interrupted cleanup is recorded as interrupted, not as removed'
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_SECONDS))
    status=0
    cleanup_remove container cleanup-signal \
        sh -c 'echo "Error response from daemon: No such container: abc" >&2; exit 143' || status=$?
    selftest_expect "$status" 143 'a SIGTERM-derived status is never successful cleanup' "$out"
    selftest_expect_grep 'No such container' "$LOG_DIR/cleanup-cleanup-signal.log" \
        'the signal control log does carry the absence text'
    LOG_DIR=$SELFTEST_SCRATCH
}

# The verifier's diagnostics rule is a function so both failure directions can be
# exercised with a fake gopls: a successful check that prints diagnostics, and a
# check that cannot run at all. The function text is read from the committed
# verifier, so the control follows the code instead of a copy of it.
verifier_diagnostics_controls() { # SCRATCH LOG
    local scratch=$1 out=$2 verifier="$ROOT/$EXAMPLE/verify-toolchain.sh"
    local control="$scratch/verifier-diagnostics" status
    mkdir -p "$control/bin"
    printf 'package main\n\nfunc main() {}\n' >"$control/main.go"
    {
        echo 'set -euo pipefail'
        sed -n '/^require_clean_diagnostics() {/,/^}/p' "$verifier"
        grep -q 'require_clean_diagnostics' <<<"$(sed -n '/^require_clean_diagnostics() {/,/^}/p' "$verifier")" || {
            echo 'self-test: FAIL the verifier has no require_clean_diagnostics function' >&2
            exit 1
        }
    } >"$control/harness.sh"
    # The Go loop runs in the image, where the verifier bundle does not exist, so it
    # carries its own copy of the same rule; the copy is written out and driven here
    # too, or a weaker copy would pass every self-test the verifier controls run.
    {
        echo 'set -euo pipefail'
        clean_diagnostics_function
    } >"$control/loop-harness.sh"
    grep -q 'require_clean_diagnostics() {' "$control/loop-harness.sh" || {
        echo 'self-test: FAIL the Go loop carries no clean-diagnostics rule' >&2
        exit 1
    }
    for mode in quiet diagnostics newline failure; do
        cat >"$control/bin/gopls" <<GOPLS
#!/usr/bin/env bash
case '$mode' in
    quiet) exit 0 ;;
    diagnostics)
        echo './main.go:3:1: unused parameter or something'
        exit 0 ;;
    newline) printf '\n'; exit 0 ;;
    failure) echo 'gopls: cannot find go command' >&2; exit 4 ;;
esac
GOPLS
        chmod 0755 "$control/bin/gopls"
        status=0
        PATH="$control/bin:$PATH" bash -c ". '$control/harness.sh'; require_clean_diagnostics ./main.go" \
            >>"$out" 2>&1 || status=$?
        case "$mode" in
            quiet) selftest_expect "$status" 0 'a clean, silent check passes' "$out" ;;
            diagnostics)
                selftest_expect "$status" 1 'a check that prints diagnostics fails' "$out"
                selftest_expect_grep 'reported diagnostics for clean code' "$out" \
                    'the diagnostics failure is named' ;;
            newline)
                # A command substitution strips this to the empty string.
                selftest_expect "$status" 1 'a newline-only output fails the clean check' "$out"
                selftest_expect_grep 'reported diagnostics for clean code' "$out" \
                    'the newline-only output is named as diagnostics' ;;
            failure)
                selftest_expect "$status" 1 'a check that cannot run fails' "$out"
                selftest_expect_grep 'exited 4' "$out" 'the execution failure is named' ;;
        esac
        status=0
        PATH="$control/bin:$PATH" bash -c ". '$control/loop-harness.sh'; require_clean_diagnostics ./main.go" \
            >>"$out" 2>&1 || status=$?
        case "$mode" in
            quiet) selftest_expect "$status" 0 'the Go loop rule accepts a silent clean check' "$out" ;;
            newline)
                # The rule that replaced `test -z "$(...)"` must reject this.
                selftest_expect "$status" 1 'the Go loop rule rejects newline-only output' "$out" ;;
            *) selftest_expect "$status" 1 "the Go loop rule fails on $mode" "$out" ;;
        esac
    done
    # The copy proves the text is sound; this proves the loop still *uses* it. A
    # `test -z "$(gopls check ...)"` left in the command would otherwise pass every
    # control above, because the rule would still be defined and unused.
    loop_text=$(sed -n '/^GO_LOOP_COMMAND=/,/go loop ok/p' "${BASH_SOURCE[0]}")
    selftest_expect_contains "$loop_text" 'require_clean_diagnostics ./main.go' \
        'the Go loop asserts a clean file with the captured-file rule'
    case "$loop_text" in
        *'$(gopls check'*)
            echo 'self-test: FAIL the Go loop still checks gopls output by substitution' >&2
            exit 1 ;;
        *) echo 'self-test: ok   the Go loop does not read gopls output by substitution' ;;
    esac
}

# The run log's `source <sha> dirty=<n>` line says which commit, not which bytes:
# the closure, its helper and the adapters were uncommitted while this work was
# reviewed. input_digests is the self-binding half, so the control drives the real
# function against the real tree and compares one digest with an independently
# computed hash -- a path typo or a silently short file list fails here.
input_digest_controls() { # LOG
    local out=$1 listing count dockerfile line
    if ! listing=$(input_digests "$ROOT" "$EXAMPLE" 2>>"$out"); then
        selftest_expect 1 0 'the build-input digest is computable' "$out"
        return
    fi
    count=$(grep -c '^input [0-9a-f]\{64\}  ' <<<"$listing" || true)
    selftest_expect "$count" 9 \
        'the digest covers the recipe, two adapters, the helper and five closure files' "$out"
    selftest_expect "$(grep -c '^inputs-set [0-9a-f]\{64\}$' <<<"$listing" || true)" 1 \
        'the digest set carries one aggregate' "$out"
    dockerfile=$(sha256_file "$ROOT/$EXAMPLE/Dockerfile")
    line=$(awk '$1 == "input" && $3 == "examples/layers/go-dev/Dockerfile" { print $2 }' \
        <<<"$listing")
    selftest_expect_text 'the Dockerfile digest matches an independent sha256' \
        "$line" "$dockerfile"
    selftest_expect_contains "$listing" "  examples/layers/go-dev/install-gopls.sh" \
        'the gopls installer adapter is digested too'
    selftest_expect_contains "$listing" "  examples/layers/go-dev/gopls-closure/pin.json" \
        'the review record is digested too'
}

# The editor leg is evidence only if a broken server fails with its own reason, so
# the client is run against each fake-gopls defect. A healthy server passes; every
# other mode must fail with the named reason, not with a timeout or a crash.
lsp_smoke_controls() { # SCRATCH LOG
    local scratch=$1 out=$2 mode status
    # The transcript is plain text (content-length framed JSON-RPC), so it is a
    # .log like every other log here, not a .json document.
    run_lsp() { # MODE
        python3 "$ROOT/$FIXTURES/lsp-smoke.py" --deadline 10 --log "$scratch/lsp-$1-transcript.log" -- \
            python3 "$ROOT/$FIXTURES/fake-gopls.py" --mode "$1" >>"$out" 2>&1
    }
    for mode in healthy no-diagnostics wrong-definition corrupt-formatting hang crash; do
        status=0
        run_lsp "$mode" || status=$?
        if [ "$mode" = healthy ]; then
            selftest_expect "$status" 0 'a healthy LSP server passes the session' "$out"
            selftest_expect_grep 'lsp-smoke: ok   formatting returns' "$out" \
                'the healthy session checked formatting'
            selftest_expect_grep 'that produce the gofmt output' "$out" \
                'the formatting oracle compares the resulting source'
        else
            selftest_expect "$status" 1 "the $mode server fails the session" "$out"
        fi
    done
    selftest_expect_grep 'lsp-smoke: FAIL definition does not name greet.go' "$out" \
        'a wrong definition location is named'
    selftest_expect_grep 'lsp-smoke: FAIL diagnostics for a type error' "$out" \
        'a missing diagnostic is named'
    selftest_expect_grep 'lsp-smoke: FAIL formatting does not produce the gofmt output' "$out" \
        'a destructive formatting edit is named rather than accepted'
    selftest_expect_grep 'lsp-smoke: FAIL initialize: no answer within' "$out" \
        'a hanging server is a named deadline, not a hang'
    selftest_expect_grep 'lsp-smoke: FAIL initialize: the session closed' "$out" \
        'a crashing server is named'
}

# The hard negative needs a record the offline checker still accepts: if the
# mutation were incoherent, the build would stop before fetching anything and the
# Go checksum error would never be observed.
mutate_closure_controls() { # SCRATCH LOG
    local scratch=$1 out=$2 copy status
    copy=$scratch/mutant-closure
    mkdir -p "$copy"
    cp -R "$ROOT/$EXAMPLE/gopls-closure/." "$copy/"
    python3 "$ROOT/$FIXTURES/mutate-closure.py" --data "$copy" \
        --wrong-zip-h1 'github.com/fatih/camelcase@v1.0.0' >>"$out" 2>&1
    status=0
    env PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/$EXAMPLE/gopls_closure.py" check "$copy" \
        --expect-gopls "$(deploy_version GOPLS_VERSION)" \
        --expect-go "$(deploy_version GO_VERSION)" >>"$out" 2>&1 || status=$?
    selftest_expect "$status" 0 'the coherent mutation is still accepted offline' "$out"

    # The discriminating control: the same hash change without the pin update is
    # refused by the checker, which is what proves the negative reaches Go.
    copy=$scratch/incoherent-closure
    mkdir -p "$copy"
    cp -R "$ROOT/$EXAMPLE/gopls-closure/." "$copy/"
    awk '$0 ~ /^github\.com\/fatih\/camelcase v1\.0\.0 h1:/ { print "github.com/fatih/camelcase v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="; next } { print }' \
        "$copy/go.sum" > "$scratch/go.sum.mutant"
    cat "$scratch/go.sum.mutant" > "$copy/go.sum"
    status=0
    env PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/$EXAMPLE/gopls_closure.py" check "$copy" \
        --expect-gopls "$(deploy_version GOPLS_VERSION)" \
        --expect-go "$(deploy_version GO_VERSION)" >"$scratch/incoherent.log" 2>&1 || status=$?
    selftest_expect "$status" 1 'an unre-pinned hash change is refused offline' "$scratch/incoherent.log"
    selftest_expect_grep 'bytes differ from the committed digest' "$scratch/incoherent.log" \
        'the refusal is the digest rule'
}

# The installer and the verifier are the whole interface between the Dockerfile's
# pins and the helper, and each refuses a value the image cannot honor *before* it
# calls dpkg, reaches the network or writes a destination: a non-default GO_PREFIX
# would install a second gopls the ENV's PATH never sees, and a missing pin
# variable would otherwise surface only inside a real build. Nothing executed
# those first guards, so they are driven here. Every probe stops at its first
# guard, so no daemon, fake docker, network or destination is involved.
# `env -u` clears the variables an inherited host environment could satisfy.
adapter_guard_controls() { # SCRATCH LOG
    local scratch=$1 out=$2 status
    local installer="$ROOT/$EXAMPLE/install-gopls.sh"
    local verifier="$ROOT/$EXAMPLE/verify-toolchain.sh"

    status=0
    env -u GO_TOOLS_BIN -u GO_PREFIX GOPLS_VERSION=v0.23.0 GO_VERSION=1.27.3 \
        GO_PREFIX=/opt/other bash "$installer" >"$scratch/adapter-prefix.log" 2>&1 || status=$?
    cat "$scratch/adapter-prefix.log" >>"$out"
    selftest_expect "$status" 1 'a non-default GO_PREFIX is refused' "$out"
    selftest_expect_grep 'GO_PREFIX must be the fixed /opt/go' "$scratch/adapter-prefix.log" \
        'the refusal names the fixed GO_PREFIX'

    status=0
    env -u GO_PREFIX -u GO_TOOLS_BIN GOPLS_VERSION=v0.23.0 GO_VERSION=1.27.3 \
        GO_TOOLS_BIN=/tmp/other-bin bash "$installer" >"$scratch/adapter-bin.log" 2>&1 || status=$?
    cat "$scratch/adapter-bin.log" >>"$out"
    selftest_expect "$status" 1 'a non-default GO_TOOLS_BIN is refused' "$out"
    selftest_expect_grep 'GO_TOOLS_BIN must be the fixed /opt/go-tools/bin' \
        "$scratch/adapter-bin.log" 'the refusal names the fixed GO_TOOLS_BIN'

    status=0
    env -u GOPLS_VERSION -u GO_VERSION bash "$installer" \
        >"$scratch/adapter-missing.log" 2>&1 || status=$?
    cat "$scratch/adapter-missing.log" >>"$out"
    selftest_expect "$status" 1 'a missing GOPLS_VERSION stops the installer' "$out"
    selftest_expect_grep 'GOPLS_VERSION must be set' "$scratch/adapter-missing.log" \
        'the installer names the missing pin'

    status=0
    env -u GOLANGCI_LINT_VERSION GO_VERSION=1.27.3 GOPLS_VERSION=v0.23.0 \
        bash "$verifier" >"$scratch/verifier-missing.log" 2>&1 || status=$?
    cat "$scratch/verifier-missing.log" >>"$out"
    selftest_expect "$status" 1 'a missing GOLANGCI_LINT_VERSION stops the verifier' "$out"
    selftest_expect_grep 'GOLANGCI_LINT_VERSION must be set' "$scratch/verifier-missing.log" \
        'the verifier names the missing pin'
}

write_fake_docker() { # DESTINATION
    cat >"$1" <<'FAKE'
#!/usr/bin/env bash
# A deliberately unhelpful docker: it records argv and fails, hangs, reports a
# foreign server or reports a builder somewhere else where the self-test says so.
# It never pretends an acceptance run succeeded -- the self-test only asserts what
# the harness decided about these answers, never the harness's own PASS lines.
mode=${FAKE_DOCKER_MODE:-record}
if [ -n "${FAKE_DOCKER_LOG:-}" ]; then printf '%s\n' "$*" >>"$FAKE_DOCKER_LOG"; fi
server_arch=${FAKE_SERVER_ARCH:-linux/amd64}
builder=${FAKE_BUILDER_NAME:-fake-builder}
mutant=0
case " $* " in *'agent-vm-go-dev-mutant:'*) mutant=1 ;; esac

case "$1" in
    version)
        [ "$mode" = no-daemon ] && exit 1
        case " $* " in
            *' --format '*) printf '%s\n' "$server_arch" ;;
            *) echo 'fake client' ;;
        esac ;;
    context) printf '%s\n' "$builder" ;;
    buildx)
        case "$mode" in
            no-builder)
                echo 'ERROR: no builder instance to inspect' >&2
                exit 1 ;;
            remote-builder)
                # The reviewer's discriminating control: the daemon is native, but
                # BuildKit compiles on another host.
                printf 'Name: %s\nDriver: remote\nEndpoint: ssh://different-host\n' "${3:-$builder}" ;;
            misbound-builder)
                printf 'Name: %s\nDriver: docker\nEndpoint: other-daemon\n' "${3:-$builder}" ;;
            two-node-remote)
                # The reviewer's bypass: the FIRST node is the inspected daemon, the
                # SECOND compiles on another host. `buildx inspect` prints one
                # Endpoint per node, so a first-line-only parse accepts this.
                printf 'Name: %s\nDriver: docker-container\nLast Activity: 2026-01-01 00:00:00 +0000 UTC\n\nNodes:\nName: %s0\nEndpoint: %s\nStatus: running\n\nName: %s1\nEndpoint: ssh://different-host\nStatus: running\n' \
                    "${3:-$builder}" "${3:-$builder}" "${3:-$builder}" "${3:-$builder}" ;;
            two-node-local)
                # The same two-node shape with every node bound to the inspected
                # context: a multi-node builder is not refused for having nodes.
                printf 'Name: %s\nDriver: docker-container\nLast Activity: 2026-01-01 00:00:00 +0000 UTC\n\nNodes:\nName: %s0\nEndpoint: %s\nStatus: running\n\nName: %s1\nEndpoint: %s\nStatus: running\n' \
                    "${3:-$builder}" "${3:-$builder}" "${3:-$builder}" "${3:-$builder}" "${3:-$builder}" ;;
            no-endpoint)
                printf 'Name: %s\nDriver: docker\nLast Activity: 2026-01-01 00:00:00 +0000 UTC\n' \
                    "${3:-$builder}" ;;
            *)
                printf 'Name: %s\nDriver: %s\nLast Activity: 2026-01-01 00:00:00 +0000 UTC\n\nNodes:\nName: %s\nEndpoint: %s\nStatus: running\n' \
                    "${3:-$builder}" "${FAKE_BUILDER_DRIVER:-docker}" "${3:-$builder}" "${3:-$builder}" ;;
        esac ;;
    info) echo 'linux amd64 overlay2' ;;
    pull) exit 0 ;;
    build)
        [ "$mode" = hang-build ] && sleep 3600
        if [ "$mutant" -eq 1 ]; then
            case "$mode" in
                genuine-negative)
                    printf '#12 [6/7] RUN bash /tmp/install-gopls.sh\n'
                    printf '#12 3.1 verifying github.com/fatih/camelcase@v1.0.0: checksum mismatch\n'
                    printf '#12 3.1 \n#12 3.1 SECURITY ERROR\n'
                    printf '#12 3.1 \nthe zip for github.com/fatih/camelcase@v1.0.0 does not match\n'
                    exit 1 ;;
                fault-negative)
                    echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2
                    exit 1 ;;
                generic-negative)
                    echo 'ERROR: failed to build: failed to solve: process "/bin/sh -c bash /tmp/install-gopls.sh" did not complete successfully: exit code: 1' >&2
                    exit 1 ;;
            esac
        fi
        # A plausible cold build log: the install steps and the retained archive
        # verifications the harness looks for.
        printf '#1 [internal] load build definition from Dockerfile\n'
        printf '#5 [4/6] RUN --mount=type=bind,source=install-go.sh bash /tmp/install-go.sh\n'
        printf '#5 12.3 go1.27.3.linux-%s.tar.gz: OK\n' "${FAKE_ARCH:-amd64}"
        printf '#6 [5/6] RUN --mount=type=bind,source=install-golangci-lint.sh bash /tmp/install-golangci-lint.sh\n'
        printf '#6 8.1 golangci-lint-2.14.0-linux-%s.tar.gz: OK\n' "${FAKE_ARCH:-amd64}"
        printf '#7 [6/6] RUN --mount=type=bind,source=install-gopls.sh bash /tmp/install-gopls.sh\n'
        exit 0 ;;
    image)
        case "$2" in
            inspect)
                if [ "$mutant" -eq 1 ]; then
                    case "$mode" in
                        genuine-negative)
                            echo "Error response from daemon: No such image: $(printf '%s' "$*" | sed -nE 's/.*(agent-vm-go-dev-mutant:[^ ]+).*/\1/p')" >&2
                            exit 1 ;;
                        fault-negative)
                            echo 'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2
                            exit 1 ;;
                    esac
                fi
                # The shape follows the requested format: the harness validates
                # both fields, so an unhelpful fake must still model what it is
                # asked for instead of hiding a parsing defect.
                case " $* " in
                    *RepoDigests*)
                        printf 'sha256:%s fake@sha256:%s %s\n' \
                            "$(printf 'b%.0s' $(seq 1 64))" "$(printf 'b%.0s' $(seq 1 64))" \
                            "$server_arch" ;;
                    *)
                        printf 'sha256:%s 12345\n' "$(printf 'b%.0s' $(seq 1 64))" ;;
                esac ;;
            ls)
                # The reference listing the cleanup sweep asks for. `retagged`
                # models a maintainer's extra tag on this run's image plus a
                # dangling labelled image, and derives this run's tag from the
                # label filter it was given -- the fake never guesses an identity
                # the harness did not state.
                case "$mode" in
                    retagged)
                        label=$(printf '%s\n' "$*" |
                            sed -nE 's/.*label=org\.agent-vm\.test\.go-example=([^ ]+).*/\1/p')
                        printf 'dddddddddddd agent-vm-go-dev:%s\n' "$label"
                        printf 'dddddddddddd maintainer/keep:mine\n'
                        printf 'cccccccccccc <none>:<none>\n' ;;
                esac
                exit 0 ;;
            rm) exit 0 ;;
        esac ;;
    network) exit 0 ;;
    ps) exit 0 ;;
    logs)
        # The recorder's log, as `docker logs` returns it: the whole log each time,
        # accumulated in a state file so a per-leg delta is observable.
        case "${FAKE_CANARY_LOG:-ok}" in
            denied)
                echo 'Error response from daemon: permission denied' >&2
                exit 1 ;;
        esac
        if [ -n "${FAKE_CANARY_STATE:-}" ] && [ -f "$FAKE_CANARY_STATE" ]; then
            cat "$FAKE_CANARY_STATE"
        fi ;;
    rm) exit 0 ;;
    rmi) exit 0 ;;
    run)
        # A first-use guest leg: model the refusal each leg expects, so the controls
        # can hold the status contract fixed while the observation varies. What the
        # recorder saw is appended to the state file, one line per fetch.
        case " $* " in
            *GOPROXY=http://canary:8765*)
                if [ -n "${FAKE_CANARY_STATE:-}" ]; then
                    [ -f "$FAKE_CANARY_STATE" ] ||
                        printf 'canary listening on 8765\n' >"$FAKE_CANARY_STATE"
                    case "${FAKE_CANARY_LOG:-ok}" in
                        quiet) ;;
                        unrelated)
                            printf 'request /unrelated-module/@v/list\n' >>"$FAKE_CANARY_STATE" ;;
                        lookalike)
                            printf 'request /notgolang.org/toolchain/@v/list\n' >>"$FAKE_CANARY_STATE" ;;
                        *)
                            printf 'request /golang.org/toolchain/@v/v0.0.1-go1.99.0.linux-amd64.zip\n' \
                                >>"$FAKE_CANARY_STATE" ;;
                    esac
                fi
                case " $* " in
                    *GOTOOLCHAIN=auto*)
                        printf 'go: downloading go1.99.0\n'
                        exit 1 ;;
                    *)
                        printf 'go: GOTOOLCHAIN=local: would download go1.99.0\n'
                        exit 1 ;;
                esac ;;
        esac
        exit 0 ;;
esac
exit 0
FAKE
    chmod 0755 "$1"
}

# --- bounded commands and owned-resource cleanup ---------------------------
# Defined before the self-test dispatch so the cleanup and verdict rules below are
# exercised by daemon-free controls instead of only by a native run. The run
# identity and log directory are assigned after the dispatch, so a self-test never
# creates a run directory.
log() { printf '%s\n' "$*" | tee -a "$LOG_DIR/harness.log"; }
record() { printf '%s\n' "$*" >>"$LOG_DIR/durations.txt"; }
pass() { TESTS=$((TESTS + 1)); log "PASS [$PHASE]: $*"; }
fail() { TESTS=$((TESTS + 1)); FAILED=$((FAILED + 1)); log "FAIL [$PHASE]: $*"; }

# --- the clean-diagnostics rule the Go loop runs ---------------------------
# `gopls check` exits 0 whether or not it reports diagnostics in gopls v0.23.0,
# so the observation is the output -- but a command substitution strips a
# newline-only output, and `test -z` would then accept `printf '\n'` as clean.
# The rule is the verifier's (verify-toolchain.sh's require_clean_diagnostics):
# capture into a real file, require status 0 *and* size zero. It is repeated here
# because the verifier bundle is bind-mounted for one Dockerfile step only and is
# not in the image the Go loop runs in; the self-test writes this exact text out
# and drives all four outcomes, so the copy cannot drift into a weaker check.
clean_diagnostics_function() {
    cat <<'FUNCTION'
require_clean_diagnostics() { # FILE
    local file=$1 output status=0
    output=$(mktemp)
    gopls check "$file" >"$output" 2>&1 || status=$?
    if [ "$status" -ne 0 ]; then
        printf 'gopls check %s exited %s:\n' "$file" "$status" >&2
        cat "$output" >&2
        rm -f "$output"
        return 1
    fi
    if [ -s "$output" ]; then
        printf 'gopls check %s reported diagnostics for clean code:\n' "$file" >&2
        cat "$output" >&2
        rm -f "$output"
        return 1
    fi
    rm -f "$output"
    return 0
}
FUNCTION
}

# --- the bytes this run actually builds ------------------------------------
# `source <sha> dirty=<n>` binds a commit, not a working copy: the closure, the
# helper and the adapters can be uncommitted (they are, until the PR lands), and
# the dirty count says how many tracked files differ -- not which bytes were
# built. So the log also records a digest per build input and one over the set.
input_digests() { # ROOT EXAMPLE -- print "<sha256>  <path>" per input, then the set
    python3 "$ROOT/$FIXTURES/input_digests.py" "$1" "$2"
}

# An independent hasher for the digest control: the harness hashes with the
# sibling input_digests.py, so the control must not reuse that code. A host with
# neither tool is a prerequisite failure, never a silent fallback.
sha256_file() { # FILE
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d' ' -f1
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | cut -d' ' -f1
    else
        echo 'sha256_file: neither sha256sum nor shasum is available' >&2
        return 1
    fi
}

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

# Only Docker's own "there is no such <kind>" answer means the resource was
# already gone. A context, daemon, permission or in-use error says nothing about
# the resource, so it stays a failure instead of becoming "successful cleanup".
cleanup_absent() { # KIND LOG
    local kind=$1 log=$2
    [ -f "$log" ] || return 1
    case "$kind" in
        container) grep -qiE 'no such container' "$log" ;;
        network) grep -qiE 'no such network|network [^ ]+ not found' "$log" ;;
        image) grep -qiE 'no such image|no such object|image [^ ]+ not found' "$log" ;;
        *) return 1 ;;
    esac
}

# A removal whose only complaint is that this resource is already absent is a
# successful cleanup; a real daemon, permission or in-use error keeps its status.
# A watchdog timeout (124) or a signal-derived status (128+N) is the harness's own
# answer, not Docker's: the removal was cut short, so no line in the log -- not
# even "No such container" -- may turn it into successful cleanup.
cleanup_remove() { # KIND LABEL COMMAND...
    local kind=$1 label=$2 status=0 log
    shift 2
    log="$LOG_DIR/cleanup-$label.log"
    cleanup_timed "$label" "$@" || status=$?
    [ "$status" -eq 0 ] && return 0
    if [ "$status" -eq 124 ] || [ "$status" -ge 129 ]; then
        record "cleanup-$label interrupted status=$status"
        return "$status"
    fi
    if cleanup_absent "$kind" "$log"; then
        record "cleanup-$label already-absent"
        return 0
    fi
    return "$status"
}

# A label proves where an image came from, not that this invocation owns every
# reference to it: `docker image rm -f <id>` would delete a maintainer's extra tag
# along with ours. So only the two registered fixture tags are removed, by name; an
# image that keeps another reference is retained and named; and an untagged
# (dangling) labelled image -- which nothing but this label refers to -- is removed
# by id without forcing.
cleanup_images() {
    local id reference refs rc=0 foreign dangling
    if ! cleanup_timed images-list "$DOCKER" image ls \
            --format '{{.ID}} {{.Repository}}:{{.Tag}}' --filter "label=$LABEL"; then
        return 1
    fi
    for id in $(awk 'NF { print $1 }' "$LOG_DIR/cleanup-images-list.log" | sort -u); do
        refs=$(awk -v id="$id" '$1 == id { print $2 }' "$LOG_DIR/cleanup-images-list.log")
        foreign=''
        dangling=0
        while IFS= read -r reference; do
            [ -n "$reference" ] || continue
            case "$reference" in
                "$TAG"|"$MUTANT_TAG") ;;
                '<none>:<none>') dangling=1 ;;
                *) foreign="$foreign $reference" ;;
            esac
        done <<EOF
$refs
EOF
        while IFS= read -r reference; do
            [ -n "$reference" ] || continue
            case "$reference" in
                "$TAG")
                    cleanup_remove image "image-${reference%%:*}" \
                        "$DOCKER" image rm "$reference" || rc=1
                    OWNED_TAG=0 ;;
                "$MUTANT_TAG")
                    cleanup_remove image "image-${reference%%:*}" \
                        "$DOCKER" image rm "$reference" || rc=1
                    OWNED_MUTANT_TAG=0 ;;
            esac
        done <<EOF
$refs
EOF
        if [ -n "$foreign" ]; then
            foreign=${foreign# }
            record "cleanup-image-$id retained-references=$(printf '%s' "$foreign" | tr ' ' ',')"
            log "NOTE [cleanup]: image $id keeps references this invocation does not own: $foreign"
        elif [ "$dangling" -eq 1 ]; then
            cleanup_remove image "image-$id" "$DOCKER" image rm "$id" || rc=1
        fi
    done
    return "$rc"
}

if [ "$SELF_TEST" -eq 1 ]; then
    self_test
    exit 0
fi

# --- suite supervision ------------------------------------------------------
if [ "${GO_EXAMPLE_SUPERVISED:-0}" != 1 ]; then
    export GO_EXAMPLE_SUPERVISED=1
    exec python3 "$WATCHDOG" --grace-seconds "$SUPERVISOR_GRACE_SECONDS" \
        "$SUITE_SECONDS" bash "${BASH_SOURCE[0]}" ${ORIGINAL_ARGUMENTS[@]+"${ORIGINAL_ARGUMENTS[@]}"}
fi

RUN_ID="go-example-$(date -u +%Y%m%dT%H%M%SZ)-$$"
LOG_DIR="$ROOT/target/go-example/$RUN_ID"
mkdir -p "$LOG_DIR"
LABEL="org.agent-vm.test.go-example=$RUN_ID"
NETWORK_NAME="$RUN_ID-net"
CANARY_NAME="$RUN_ID-canary"
TAG="agent-vm-go-dev:$RUN_ID"
# A tag needs a lowercase repository name before the ':'; the run id alone is not
# a valid repository (it carries 'T' and 'Z').
MUTANT_TAG="agent-vm-go-dev-mutant:$RUN_ID"
BUNDLE="$LOG_DIR/verifier-bundle"
TESTS=0
FAILED=0
PHASE=start
CLEANUP_DEADLINE=0
OWNED_NETWORK=0
OWNED_CANARY=0
OWNED_TAG=0
OWNED_MUTANT_TAG=0

# Every host-side command is bounded and its own session (see run_bounded above,
# which the self-test also drives).

# --- cleanup ----------------------------------------------------------------
cleanup() {
    local status=$? id cleanup_status=0 containers_gone=0
    trap '' INT TERM HUP
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_SECONDS))
    if cleanup_timed containers-list "$DOCKER" ps -aq --filter "label=$LABEL"; then
        containers_gone=1
        while IFS= read -r id; do
            [ -n "$id" ] || continue
            cleanup_remove container "container-$id" "$DOCKER" rm -f "$id" || { containers_gone=0; cleanup_status=1; }
        done <"$LOG_DIR/cleanup-containers-list.log"
    else
        cleanup_status=1
    fi
    if [ "$containers_gone" -eq 1 ]; then
        OWNED_CANARY=0
    fi
    if [ "$OWNED_CANARY" -eq 1 ]; then
        cleanup_remove container container-canary "$DOCKER" rm -f "$CANARY_NAME" || cleanup_status=1
    fi
    if [ "$OWNED_NETWORK" -eq 1 ]; then
        cleanup_remove network network "$DOCKER" network rm "$NETWORK_NAME" || cleanup_status=1
    fi
    cleanup_images || cleanup_status=1
    # A registered tag can outlive a label sweep (a build that failed after tagging,
    # an image whose label the sweep cannot see), so both are also removed by name.
    if [ "$OWNED_TAG" -eq 1 ]; then
        cleanup_remove image image-tag "$DOCKER" image rm "$TAG" || cleanup_status=1
    fi
    if [ "$OWNED_MUTANT_TAG" -eq 1 ]; then
        cleanup_remove image image-mutant-tag "$DOCKER" image rm "$MUTANT_TAG" || cleanup_status=1
    fi
    printf 'completed status=%s seconds=%s\n' "$cleanup_status" \
        "$((SECONDS - (CLEANUP_DEADLINE - CLEANUP_SECONDS)))" >"$LOG_DIR/cleanup.txt"
    log "cleanup completed status=$cleanup_status (logs: $LOG_DIR)"
    [ "$cleanup_status" -eq 0 ] || { log 'FAIL [cleanup]: owned-resource cleanup failed'; status=1; }
    exit "$status"
}
trap cleanup EXIT

log "=== go-example $RUN_ID platform=$PLATFORM arch=$ARCH ==="
log "source $(git -C "$ROOT" rev-parse HEAD) dirty=$(git -C "$ROOT" status --porcelain | grep -cv '^?? ' || true)"
# Add-only self-binding: the digests of the bytes this run builds (recipe, both
# adapters, the helper, the five closure files) and one over the set. A digest that
# cannot be computed is logged as unavailable rather than becoming a new failure.
if input_listing=$(input_digests "$ROOT" "$EXAMPLE" 2>"$LOG_DIR/input-digests.err"); then
    while IFS= read -r digest_line; do log "$digest_line"; done <<<"$input_listing"
else
    log 'inputs-set unavailable (see input-digests.err)'
fi
run_bounded daemon "$DAEMON_SECONDS" "$LOG_DIR/daemon.log" "$DOCKER" version || true
run_bounded context "$DAEMON_SECONDS" "$LOG_DIR/context.log" "$DOCKER" context show || true
runner=$(head -1 "$LOG_DIR/context.log" 2>/dev/null || true)
[ -n "$runner" ] || runner=default
run_bounded server-arch "$DAEMON_SECONDS" "$LOG_DIR/server-arch.log" \
    "$DOCKER" version --format '{{.Server.Os}}/{{.Server.Arch}}' || true
SERVER_ARCH=$(cat "$LOG_DIR/server-arch.log" 2>/dev/null || true)
[ -n "$SERVER_ARCH" ] || SERVER_ARCH=unknown
PHASE=prerequisite
if [ "$SERVER_ARCH" != "linux/$ARCH" ]; then
    fail "BLOCKED: the daemon is $SERVER_ARCH, not native linux/$ARCH; QEMU is not acceptance"
    record 'native-prerequisite FAILED'
    exit 1
fi
# Where BuildKit compiles is the builder, not the Docker server, so the builder is
# inspected, must answer, and must be bound to the daemon that was just checked:
# a remote/ssh/kubernetes driver compiles on another host (or under emulation),
# which no architecture check of the *client's* daemon would reveal.
if ! run_bounded builder "$DAEMON_SECONDS" "$LOG_DIR/builder.log" \
        "$DOCKER" buildx inspect "$runner"; then
    fail "BLOCKED: buildx cannot inspect the builder '$runner'; no builder binding is evidence"
    record 'native-prerequisite FAILED (builder inspection)'
    exit 1
fi
BUILDER_DRIVER=$(sed -nE 's/^Driver:[[:space:]]*//p' "$LOG_DIR/builder.log" | head -1)
BUILDER_DRIVER=${BUILDER_DRIVER%$'\r'}
case "$BUILDER_DRIVER" in
    docker|docker-container) ;;
    '')
        fail "BLOCKED: the inspection of builder '$runner' named no driver"
        record 'native-prerequisite FAILED (builder driver)'
        exit 1 ;;
    *)
        fail "BLOCKED: builder '$runner' uses the '$BUILDER_DRIVER' driver, which is not daemon-backed"
        record "native-prerequisite FAILED (builder driver $BUILDER_DRIVER)"
        exit 1 ;;
esac
# `docker buildx inspect` reports one Endpoint per node, and Docker may schedule the
# build on any eligible node, so a builder whose FIRST node is the inspected daemon
# and whose second is `ssh://other-host` still compiles on the other host. Every
# node endpoint is therefore checked, not just the first one; a node whose endpoint
# does not name the inspected context is refused rather than assumed local.
BUILDER_NODES=0
BUILDER_FOREIGN_ENDPOINT=''
while IFS= read -r endpoint; do
    endpoint=${endpoint%$'\r'}
    endpoint=${endpoint%"${endpoint##*[![:space:]]}"}
    [ -n "$endpoint" ] || continue
    BUILDER_NODES=$((BUILDER_NODES + 1))
    [ "$endpoint" = "$runner" ] || BUILDER_FOREIGN_ENDPOINT=${BUILDER_FOREIGN_ENDPOINT:+"$BUILDER_FOREIGN_ENDPOINT "}$endpoint
done < <(sed -nE 's/^Endpoint:[[:space:]]*//p' "$LOG_DIR/builder.log")
if [ "$BUILDER_NODES" -eq 0 ]; then
    fail "BLOCKED: the inspection of builder '$runner' named no endpoint, so its binding is unknown"
    record 'native-prerequisite FAILED (builder endpoint)'
    exit 1
fi
if [ -n "$BUILDER_FOREIGN_ENDPOINT" ]; then
    if [ "$BUILDER_NODES" -eq 1 ]; then
        fail "BLOCKED: builder '$runner' is not bound to the inspected daemon (its endpoint is '$BUILDER_FOREIGN_ENDPOINT')"
    else
        fail "BLOCKED: builder '$runner' has $BUILDER_NODES nodes and not every one is bound to the inspected daemon (endpoint '$BUILDER_FOREIGN_ENDPOINT' is not '$runner')"
    fi
    record "native-prerequisite FAILED (builder endpoint $BUILDER_FOREIGN_ENDPOINT)"
    exit 1
fi
BUILDER_ENDPOINT=$runner
log "docker server $SERVER_ARCH builder $runner driver=$BUILDER_DRIVER endpoint=$BUILDER_ENDPOINT nodes=$BUILDER_NODES"
record "native-prerequisite OK ($SERVER_ARCH builder=$runner driver=$BUILDER_DRIVER endpoint=$BUILDER_ENDPOINT nodes=$BUILDER_NODES)"

GO_VERSION=$(deploy_version GO_VERSION)
GOLANGCI_LINT_VERSION=$(deploy_version GOLANGCI_LINT_VERSION)
GOPLS_VERSION=$(deploy_version GOPLS_VERSION)
GO_PIN_SHA=$(deploy_version "GO_$(echo "$ARCH" | tr '[:lower:]' '[:upper:]')_SHA256")
LINT_PIN_SHA=$(deploy_version "GOLANGCI_LINT_$(echo "$ARCH" | tr '[:lower:]' '[:upper:]')_SHA256")
DATA="$ROOT/$EXAMPLE/gopls-closure"
log "pins go=$GO_VERSION golangci-lint=$GOLANGCI_LINT_VERSION gopls=$GOPLS_VERSION"

# --- phase 0: the committed pins verify offline -----------------------------
# Everything below builds the image from these bytes, so an incoherent closure is
# a pin failure rather than a build mystery. The expectations come from the
# Dockerfile ARGs, never from an installed binary.
PHASE=pins
if run_bounded pin-check "$RUN_SECONDS" "$LOG_DIR/pin-check.log" \
        env PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/$EXAMPLE/gopls_closure.py" check "$DATA" \
        --expect-gopls "$GOPLS_VERSION" --expect-go "$GO_VERSION"; then
    pass 'the committed gopls closure verifies offline against the Dockerfile pins'
else
    fail 'the committed gopls closure does not verify; no native phase is meaningful'
    exit 1
fi
LICENSE_SHA=$(jq -r '.files["THIRD_PARTY_LICENSES"]' "$DATA/pin.json") ||
    { fail 'the pin has no THIRD_PARTY_LICENSES digest'; exit 1; }
[ -n "$LICENSE_SHA" ] && [ "$LICENSE_SHA" != null ] ||
    { fail 'the pin has no THIRD_PARTY_LICENSES digest'; exit 1; }

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
# A cached build proves nothing about the installer, so whether the gopls install
# stage actually ran is part of the record rather than an invisible property of
# the local cache.
install_step=$(awk '/^#[0-9]+ \[/ && /install-gopls\.sh/ { sub(/^#/, "", $1); print $1; exit }' \
    "$LOG_DIR/build.log")
if [ -z "$install_step" ]; then
    record 'build-gopls-install step=unknown'
    if [ "$COLD_BUILD" -eq 1 ]; then
        fail 'the cold build log does not name the gopls install step'
    else
        log 'NOTE [build]: the gopls install step is not named in the build log'
    fi
elif grep -qxF "#$install_step CACHED" "$LOG_DIR/build.log"; then
    record 'build-gopls-install cached=1'
    if [ "$COLD_BUILD" -eq 1 ]; then
        fail 'the gopls install stage came from the build cache in a --no-cache run'
    else
        log 'NOTE [build]: the gopls install stage came from the build cache; pass --no-cache for a cold build'
    fi
else
    record 'build-gopls-install cached=0'
    pass "the gopls install stage ran from scratch (step $install_step)"
fi
# The two retained archives are re-verified inside the build; their OK lines are
# this run's evidence that the committed digests still describe the bytes. The
# release tarballs are named `go<version>.linux-<arch>.tar.gz` and
# `golangci-lint-<version>-linux-<arch>.tar.gz`, so only the first uses a dot.
go_ok=$(grep -cE "go${GO_VERSION}\.linux-${ARCH}\.tar\.gz: OK" "$LOG_DIR/build.log" || true)
lint_ok=$(grep -cE "golangci-lint-${GOLANGCI_LINT_VERSION}-linux-${ARCH}\.tar\.gz: OK" \
    "$LOG_DIR/build.log" || true)
if [ "$go_ok" -ge 1 ] && [ "$lint_ok" -ge 1 ]; then
    pass 'both retained archives pass sha256sum -c in the build'
    record "archive-digests go=$GO_VERSION sha=$GO_PIN_SHA OK lint=$GOLANGCI_LINT_VERSION sha=$LINT_PIN_SHA OK"
else
    fail "the build log retains no archive OK line (go=$go_ok lint=$lint_ok)"
fi

PHASE=image
if ! run_bounded image-inspect "$RUN_SECONDS" "$LOG_DIR/image-inspect.log" \
        "$DOCKER" image inspect --format '{{.Id}} {{.Size}}' "$TAG"; then
    fail "the built image could not be inspected: $TAG"
    exit 1
fi
IMAGE_ID=''; IMAGE_SIZE=''
read -r IMAGE_ID IMAGE_SIZE <"$LOG_DIR/image-inspect.log" || true
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
    'env | sort; stat -c "%a %U:%G %n" /opt /opt/go /opt/go/bin/go /opt/go-tools/bin/gopls /opt/go-tools/share/gopls/THIRD_PARTY_LICENSES 2>&1; ls -d /run/go-verify /tmp/gopls-closure 2>&1 || true; command -v python3' \
    || true
if grep -qx 'GOTOOLCHAIN=local' "$LOG_DIR/inspect.log" &&
    ! grep -qE '^(GOFLAGS|GOPROXY|GOSUMDB|GONOSUMDB|GOPRIVATE|GONOPROXY|GOINSECURE)=' "$LOG_DIR/inspect.log"; then
    pass 'image ENV: GOTOOLCHAIN=local and no other GO* policy exported'
else
    fail 'image ENV contract'
fi
if grep -q '^755 root:root /opt/go-tools/bin/gopls$' "$LOG_DIR/inspect.log" &&
    grep -q '^644 root:root /opt/go-tools/share/gopls/THIRD_PARTY_LICENSES$' "$LOG_DIR/inspect.log" &&
    ! grep -qE '^(7[0-9]7|77[0-9]|[0-7]7[0-9]|[0-7]77) ' "$LOG_DIR/inspect.log"; then
    pass 'the installed gopls and its licence file are world-readable with the expected modes'
else
    fail 'installed tool modes or ownership'
fi
if grep -q "ls: cannot access '/run/go-verify': No such file or directory" "$LOG_DIR/inspect.log" &&
    grep -q "ls: cannot access '/tmp/gopls-closure': No such file or directory" "$LOG_DIR/inspect.log"; then
    pass 'no build-time closure bundle or helper remains in the image'
else
    fail 'the build-time closure bundle was copied into the image'
fi

# The licence bytes are checked from inside the image, independently of the
# verifier's own report.
PHASE=licenses
run_bounded licenses "$RUN_SECONDS" "$LOG_DIR/licenses.log" \
    "$DOCKER" run --rm --network none --label "$LABEL" --entrypoint /bin/sh "$TAG" -c \
    'sha256sum /opt/go-tools/share/gopls/THIRD_PARTY_LICENSES' || true
image_license_sha=$(awk '{ print $1 }' "$LOG_DIR/licenses.log" 2>/dev/null | head -1)
if [ "$image_license_sha" = "$LICENSE_SHA" ]; then
    pass 'the installed THIRD_PARTY_LICENSES bytes equal the committed digest'
else
    fail "installed licence digest '$image_license_sha' != committed '$LICENSE_SHA'"
fi

# --- phase 2: verifier, no network ------------------------------------------
# The same contract the Dockerfile runs, mounted read-only. The three version
# ARGs are not persisted ENV in the image, so they are passed explicitly.
PHASE=verifier
mkdir -p "$BUNDLE"
cp "$ROOT/$EXAMPLE/verify-toolchain.sh" "$ROOT/$EXAMPLE/gopls_closure.py" "$BUNDLE/"
cp -R "$DATA" "$BUNDLE/gopls-closure"
if run_bounded verifier "$RUN_SECONDS" "$LOG_DIR/verifier.log" \
        "$DOCKER" run --rm --network none --label "$LABEL" \
        -e "GO_VERSION=$GO_VERSION" -e "GOLANGCI_LINT_VERSION=$GOLANGCI_LINT_VERSION" \
        -e "GOPLS_VERSION=$GOPLS_VERSION" -e PYTHONDONTWRITEBYTECODE=1 \
        -v "$BUNDLE:/run/go-verify:ro" --entrypoint /bin/bash "$TAG" \
        /run/go-verify/verify-toolchain.sh; then
    pass 'network-none installed-state verifier passed'
else
    fail 'network-none installed-state verifier failed'
fi
if grep -q '== go-dev layer sanity OK ==' "$LOG_DIR/verifier.log"; then
    pass 'the verifier reached its final sanity marker'
else
    fail 'the verifier did not reach its final marker'
fi

# --- phase 3: the Go loop, as root and as a numeric uid ---------------------
# An empty writable HOME (tmpfs) as two identities, read-only /opt from the
# image, no network: build, vet, race-test, format, lint and both gopls check
# outcomes must work from preinstalled content.
PHASE=go-loop
# Docker's tmpfs default options include `noexec`, and `go test` compiles then
# executes a test binary under the cache in $HOME, so `exec` is explicit here.
#
# `gopls check` reports diagnostics but exits 0 either way in gopls v0.23.0
# (measured: a clean file and a seeded type error both exit 0), so the
# discriminating observation is the diagnostic text, not the status. A clean file
# must print nothing; a broken one must print `cannot use`. "Nothing" is a captured
# file of size zero with status 0 -- the verifier's rule, carried here because the
# verifier bundle is not in the image -- so a newline-only output is not mistaken
# for clean.
GO_LOOP_COMMAND="$(clean_diagnostics_function)
set -e
cp -R /work/. /tmp/project
cd /tmp/project
go build ./...
go vet ./...
go test -race ./...
test -z \"\$(gofmt -l .)\"
golangci-lint run
require_clean_diagnostics ./main.go
cp broken.go.txt broken.go
gopls check ./broken.go 2>&1 | tee /tmp/broken-check.log
grep -F \"cannot use\" /tmp/broken-check.log
echo \"go loop ok\""
for identity in 0:0 12345:23456; do
    label=${identity%%:*}
    [ "$label" = 0 ] && label=root
    if run_bounded "go-loop-$label" "$RUN_SECONDS" "$LOG_DIR/go-loop-$label.log" \
            "$DOCKER" run --rm --network none --label "$LABEL" -u "$identity" \
            --tmpfs /tmp:rw,mode=1777,exec -e HOME=/tmp -e TMPDIR=/tmp \
            -v "$ROOT/$FIXTURES/project:/work:ro" -w /tmp --entrypoint /bin/bash "$TAG" \
            -c "$GO_LOOP_COMMAND"; then
        if grep -q 'go loop ok' "$LOG_DIR/go-loop-$label.log"; then
            pass "Go loop as $identity: build/vet/race/gofmt/lint/gopls without network"
        else
            fail "Go loop as $identity produced no completion marker"
        fi
    else
        fail "Go loop as $identity"
    fi
done

# --- phase 4: a real editor-facing LSP session ------------------------------
PHASE=lsp
for identity in 0:0 12345:23456; do
    label=${identity%%:*}
    [ "$label" = 0 ] && label=root
    if run_bounded "lsp-$label" "$LSP_SECONDS" "$LOG_DIR/lsp-$label.log" \
            python3 "$ROOT/$FIXTURES/lsp-smoke.py" --deadline 60 \
            --log "$LOG_DIR/lsp-$label-transcript.log" -- \
            "$DOCKER" run -i --rm --network none --label "$LABEL" -u "$identity" \
            --tmpfs /tmp:rw,mode=1777,exec -e HOME=/tmp -e TMPDIR=/tmp \
            -v "$ROOT/$FIXTURES/project:/work:ro" -w /work "$TAG" gopls; then
        pass "LSP session as $identity: initialize/definition/hover/diagnostics/formatting/shutdown"
    else
        fail "LSP session as $identity"
    fi
done

# --- phase 5: first use against a canary proxy -----------------------------
# The canary network has no route off the daemon, so a fetch that reaches the
# recorder is the only fetch that could be made. The module in /work asks for a
# toolchain this image does not carry: GOTOOLCHAIN=local refuses it locally, and
# the opt-out control proves the recorder really sees a download -- a request for
# the named toolchain path, not merely any request. Each leg's observation is the
# recorder lines past the previous read, and a leg whose recorder read failed has
# no observation at all, so it cannot pass as "zero requests".
PHASE=first-use
canary_leg() { # LABEL IDENTITY EXPECTED EXPECT_REQUESTS PATTERN COMMAND
    local label=$1 identity=$2 expected=$3 requests=$4 pattern=$5 command=$6
    local id=${identity//:/-} status=0 recorder=0 observed=0 verdict
    local previous="$LOG_DIR/canary-previous.log" delta="$LOG_DIR/canary-requests-$label-$id.log"
    local before_lines=0 after_lines=0
    before_lines=$(wc -l <"$previous" 2>/dev/null | tr -d ' ' || true)
    run_bounded "first-use-$label-$id" "$RUN_SECONDS" "$LOG_DIR/first-use-$label-$id.log" \
        "$DOCKER" run --rm --network "$NETWORK_NAME" --label "$LABEL" -u "$identity" \
        --tmpfs /tmp:rw,mode=1777,exec -e HOME=/tmp -e TMPDIR=/tmp \
        -e GOPROXY=http://canary:8765 -e GOSUMDB=off \
        -v "$ROOT/$FIXTURES/toolchain-project:/work:ro" -w /tmp --entrypoint /bin/bash \
        "$TAG" -c "$command" || status=$?
    # The recorder's log is read, not assumed: a failed `docker logs` leaves the
    # audit with no observation at all, which is not "zero requests". `docker logs`
    # returns the whole log every time, so the leg's own lines are the tail past
    # the previous read -- otherwise one earlier fetch would satisfy a later leg.
    run_bounded "canary-server-$label-$id" "$RUN_SECONDS" "$LOG_DIR/canary-server.log" \
        "$DOCKER" logs "$CANARY_NAME" || recorder=$?
    if [ "$recorder" -eq 0 ]; then
        after_lines=$(wc -l <"$LOG_DIR/canary-server.log" 2>/dev/null | tr -d ' ' || true)
        if [ "${after_lines:-0}" -lt "${before_lines:-0}" ]; then
            fail "first use $label as $identity: the recorder log lost earlier lines, so this observation is not append-only"
            return 0
        fi
        tail -n "+$(( ${before_lines:-0} + 1 ))" "$LOG_DIR/canary-server.log" >"$delta"
        cp "$LOG_DIR/canary-server.log" "$previous"
    else
        : >"$delta"
    fi
    observed=$(canary_request_count "$delta")
    record "first-use-$label-$identity status=$status recorder-status=$recorder proxy-requests=$observed"
    verdict=$(first_use_verdict "$expected" "$status" "$recorder" "$requests" "$delta")
    if [ "$verdict" != 'ok' ]; then
        fail "first use $label as $identity: $verdict"
        return 0
    fi
    if [ "$pattern" != '-' ] &&
        ! grep -qF "$pattern" "$LOG_DIR/first-use-$label-$id.log"; then
        fail "first use $label as $identity: status $status, but the output does not name $pattern"
        return 0
    fi
    pass "first use $label as $identity: status $status, $observed proxy requests"
}
: >"$LOG_DIR/canary-previous.log"
OWNED_NETWORK=1
if run_bounded canary-network "$RUN_SECONDS" "$LOG_DIR/canary-network.log" \
        "$DOCKER" network create --internal --label "$LABEL" "$NETWORK_NAME"; then
    OWNED_CANARY=1
    if run_bounded canary-start "$RUN_SECONDS" "$LOG_DIR/canary-start.log" \
            "$DOCKER" run -d --name "$CANARY_NAME" --label "$LABEL" \
            --network "$NETWORK_NAME" --network-alias canary \
            -v "$ROOT/script/test/fixtures/rust-toolchain/canary.py:/canary.py:ro" \
            --entrypoint /usr/bin/python3 "$TAG" /canary.py 8765; then
        sleep 2
        for identity in 0:0 12345:23456; do
            while IFS='|' read -r label expected requests pattern command; do
                [ -n "$label" ] || continue
                canary_leg "$label" "$identity" "$expected" "$requests" "$pattern" "$command"
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

# --- phase 6: hard build negative ------------------------------------------
# One committed gopls content hash is replaced coherently, so the offline checker
# still accepts the record and the build reaches Go's own zip verification. A
# generic failure, an archive-digest failure, a stale-scope refusal, an invalid tag,
# a daemon fault, a timeout or a signal is not evidence: both the named checksum
# error and SECURITY ERROR must appear. The tag must then be *confirmed* absent by
# Docker; an inspection that errored for another reason is unresolved, not absence.
PHASE=negative
MUTANT_ROOT="$LOG_DIR/mutant"
mkdir -p "$MUTANT_ROOT"
cp -R "$ROOT/$EXAMPLE/." "$MUTANT_ROOT/"
if run_bounded mutant-mutation "$RUN_SECONDS" "$LOG_DIR/mutant-mutation.log" \
        python3 "$ROOT/$FIXTURES/mutate-closure.py" --data "$MUTANT_ROOT/gopls-closure" \
        --wrong-zip-h1 'github.com/fatih/camelcase@v1.0.0'; then
    if run_bounded mutant-offline "$RUN_SECONDS" "$LOG_DIR/mutant-offline.log" \
            env PYTHONDONTWRITEBYTECODE=1 python3 "$ROOT/$EXAMPLE/gopls_closure.py" \
            check "$MUTANT_ROOT/gopls-closure" --expect-gopls "$GOPLS_VERSION" \
            --expect-go "$GO_VERSION"; then
        pass 'the mutated closure is still coherent offline, so the negative reaches Go'
    else
        fail 'the mutated closure was refused offline; the negative would prove nothing'
        exit 1
    fi
else
    fail 'the closure mutation could not be applied'
    exit 1
fi
OWNED_MUTANT_TAG=1
mutant_status=0
run_bounded build-mutant "$BUILD_SECONDS" "$LOG_DIR/build-mutant.log" \
    "$DOCKER" build --no-cache --builder "$runner" --platform "$PLATFORM" --progress plain \
    --label "$LABEL" --build-arg "BASE_IMAGE=$PARENT_REF" -t "$MUTANT_TAG" "$MUTANT_ROOT" \
    || mutant_status=$?
record "build-mutant status=$mutant_status"
mutant_verdict=$(mutant_build_verdict "$mutant_status" "$LOG_DIR/build-mutant.log")
if [ "$mutant_verdict" = 'ok' ]; then
    pass 'the mutated content hash fails the build with the named Go checksum error'
else
    fail "the mutant build is not evidence: $mutant_verdict"
fi
mutant_present=0
run_bounded mutant-tag-absent "$RUN_SECONDS" "$LOG_DIR/mutant-tag.log" \
    "$DOCKER" image inspect "$MUTANT_TAG" || mutant_present=$?
absent_verdict=$(mutant_absent_verdict "$mutant_present" "$LOG_DIR/mutant-tag.log")
if [ "$absent_verdict" = 'ok' ]; then
    OWNED_MUTANT_TAG=0
    pass 'the failed mutant build published no image (confirmed absent)'
else
    fail "mutant publication is unresolved: $absent_verdict"
fi

PHASE=summary
record "summary tests=$TESTS failed=$FAILED platform=$PLATFORM image=$IMAGE_ID \
parent=$PARENT_REF parent-id=${PARENT_ID:-unknown}"
log "=== $TESTS checks, $FAILED failed (logs: $LOG_DIR) ==="
[ "$FAILED" -eq 0 ]
