#!/usr/bin/env bash
# Cold-build one example layer on one native architecture (#33, ADR 0010).
#
# Every accepted build pulls the layer's committed digest-pinned parent and
# passes --no-cache, so a withdrawn but unchanged pin fails here instead of being
# masked by a warm instruction cache. Runtime coverage stays with the suites that
# own it, apart from the network-request egress smoke (ADR 0014): that one needs
# the image this run built, and cleanup removes the owned tag, so it runs here.
# Otherwise this runner claims only what an ordinary `docker build` plus a
# platform inspection proves.
#
# Usage: bash script/test/layer-build.sh --layer NAME --platform linux/amd64|linux/arm64 \
#          [--evidence-dir DIR]
#        bash script/test/layer-build.sh --self-test
# Status: 0 every phase passed and the owned tag was removed, 1 a phase or the
#         cleanup failed, 2 usage, 124 the supervising watchdog fired,
#         128+N on an inherited signal.
set -euo pipefail

ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
cd "$ROOT"
DOCKER=${DOCKER:-docker}
WATCHDOG="$ROOT/script/test/host-watchdog.py"
BUILDER_GUARD="$ROOT/script/test/require-daemon-builder.sh"
PARENT_REPOSITORY='ghcr.io/gregwebs/agent-vm-standard'

# Aggregate phase budgets, each including its children's escalation windows. The
# supervisor has to outlast their sum. Without an egress fixture:
# 300 + 1022 + 3002 + 60 + 60 = 4444 < 4500. With one, EGRESS is added:
# 300 + 1022 + 3002 + 60 + 1140 + 60 = 5584 < 5700.
PREFLIGHT_SECONDS=300
PULL_PHASE_SECONDS=1022        # 900 s pull + two 1 s windows + 120 s parent inspection
BUILD_PHASE_SECONDS=3002       # 3000 s build + two 1 s windows
INSPECT_PHASE_SECONDS=60
EGRESS_PHASE_SECONDS=1140      # 940 s runner + two 100 s windows
CLEANUP_SECONDS=60             # total deadline, not a per-command allowance
SUPERVISOR_SECONDS=4500        # a layer without an egress fixture
SUPERVISOR_GRACE_SECONDS=90    # > cleanup budget (60 s) + escalation + trap entry
EGRESS_SUPERVISOR_SECONDS=5700
# A supervisor's grace has to outlast BOTH escalation windows of the nested
# watchdog EGRESS runs under, plus this script's own cleanup:
# 2 x 100 + 60 + escalation = 290. A one-window grace did not cover that path.
EGRESS_SUPERVISOR_GRACE_SECONDS=290
# Per-command bounds, declared next to the commands that consume them.
DISCOVERY_SECONDS=30
BUILDER_GUARD_SECONDS=120
PULL_SECONDS=900
PARENT_INSPECT_SECONDS=120
BUILD_SECONDS=3000
KILL_GRACE_SECONDS=1
CLEANUP_COMMAND_SECONDS=5
CLEANUP_ESCALATION_SECONDS=1
CLEANUP_RESERVE_SECONDS=2
# The smoke has its own bounds: it supervises itself, and this runner has to
# outlast its finalizer reserve if it ever overruns.
EGRESS_SECONDS=940             # > the runner's own maximum (830 active + 90 finalizer + 10)
EGRESS_KILL_GRACE_SECONDS=100  # > the runner's finalizer reserve (90 s) + escalation + trap entry
EGRESS_RUNNER=${LAYER_BUILD_EGRESS_RUNNER:-$ROOT/script/test/network-requests-egress.sh}

PHASES='PREFLIGHT PULL BUILD INSPECT'
CURRENT=PREFLIGHT
LAYER=''
PLATFORM=''
EVID=''
SELF_TEST=0
SELFTEST_SCRATCH=''
ORIGINAL_ARGUMENTS=("$@")

usage() {
    echo 'usage: layer-build.sh --layer NAME --platform linux/amd64|linux/arm64 [--evidence-dir DIR]' >&2
    echo '       layer-build.sh --self-test' >&2
    exit 2
}
fail() { echo "layer-build: $*" >&2; exit 1; }
# The tag and the default evidence directory are derived from --layer and
# --platform before those arguments are validated, so the values are sanitized
# first: a rejected argument must not name a path or a tag.
sanitize() { printf '%s' "$1" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9-' '_'; }
# The host architecture is the only accepted build architecture: an emulated
# daemon would prove nothing about a native install.
native_platform() {
    case "$(uname -m)" in
        x86_64 | amd64) echo linux/amd64 ;;
        arm64 | aarch64) echo linux/arm64 ;;
        *) echo unknown ;;
    esac
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --self-test) SELF_TEST=1; shift ;;
        --layer | --platform | --evidence-dir)
            [ "$#" -ge 2 ] && [ -n "$2" ] || usage
            case "$1" in
                --layer) LAYER=$2 ;;
                --platform) PLATFORM=$2 ;;
                --evidence-dir) EVID=$2 ;;
            esac
            shift 2 ;;
        *) usage ;;
    esac
done

# Self-test-only seam: LAYER_BUILD_SELF_TEST=1 plus
# LAYER_BUILD_SELF_TEST_EGRESS_SECONDS=seconds:grace:supervisor-grace:cleanup
# scales the EGRESS bounds together for the timeout and nested-supervisor cases,
# which would otherwise wait minutes. CI sets neither, and the relations between
# the values stay the production ones (the budget invariants assert that).
EGRESS_WATCHDOG=$WATCHDOG
if [ "${LAYER_BUILD_SELF_TEST:-}" = 1 ] && [ -n "${LAYER_BUILD_SELF_TEST_EGRESS_SECONDS:-}" ]; then
    IFS=: read -r EGRESS_SECONDS EGRESS_KILL_GRACE_SECONDS EGRESS_SUPERVISOR_GRACE_SECONDS \
        CLEANUP_SECONDS <<<"$LAYER_BUILD_SELF_TEST_EGRESS_SECONDS"
    EGRESS_PHASE_SECONDS=$((EGRESS_SECONDS + 2 * EGRESS_KILL_GRACE_SECONDS))
    CLEANUP_COMMAND_SECONDS=1
    CLEANUP_ESCALATION_SECONDS=1
    CLEANUP_RESERVE_SECONDS=0
    EGRESS_WATCHDOG=${LAYER_BUILD_SELF_TEST_EGRESS_WATCHDOG:-$WATCHDOG}
fi

PARENT=''
BUILDER=''
ARCH=''
CONTEXT=''
SOURCE_SHA=''
DIRTY=''

# Which bounds the supervisor gets depends on one filesystem fact: whether this
# layer has an egress fixture. It has to be answered BEFORE the re-exec below,
# because the supervisor is started with the bound it must enforce, and the
# answer travels in LAYER_BUILD_EGRESS so the supervised run does not decide
# again. No layer name appears here: discovery is the filesystem (ADR 0010).
LAYER_BUILD_EGRESS='none'
case "$LAYER" in
    '' | -*) : ;;
    *[!a-z0-9-]*) : ;;
    *)
        egress_fixture="$ROOT/script/test/fixtures/network-requests/$LAYER"
        if [ -L "$egress_fixture" ]; then
            LAYER_BUILD_EGRESS='symlink'
        elif [ -d "$egress_fixture" ]; then
            LAYER_BUILD_EGRESS='yes'
        fi ;;
esac
export LAYER_BUILD_EGRESS
if [ "$LAYER_BUILD_EGRESS" = yes ]; then
    supervisor_seconds=$EGRESS_SUPERVISOR_SECONDS
    supervisor_grace=$EGRESS_SUPERVISOR_GRACE_SECONDS
else
    supervisor_seconds=$SUPERVISOR_SECONDS
    supervisor_grace=$SUPERVISOR_GRACE_SECONDS
fi

# --- self-test: fake Docker on PATH and the installers' URL diagnostics -------
selftest_expect() { # ACTUAL EXPECTED NAME
    if [ "$1" != "$2" ]; then
        echo "self-test: $3: expected '$2', got '$1'" >&2
        exit 1
    fi
    echo "self-test: ok   $3"
}

selftest_grep() { # PATTERN FILE NAME
    if ! grep -qF -- "$1" "$2" 2>/dev/null; then
        echo "self-test: $3: '$1' missing from $2" >&2
        sed -n '1,40p' "$2" >&2 || true
        exit 1
    fi
    echo "self-test: ok   $3"
}

selftest_refused() { # STATUS NAME
    if [ "$1" -eq 0 ]; then
        echo "self-test: $2: the runner accepted it" >&2
        exit 1
    fi
    echo "self-test: ok   $2"
}

write_fake_docker() { # DESTINATION
    cat >"$1" <<'FAKE'
#!/usr/bin/env bash
# A deliberately unhelpful docker: it records argv, models one image ledger and
# fails or lies exactly where FAKE_DOCKER_MODE says. It never claims a daemon.
set -uo pipefail
mode=${FAKE_DOCKER_MODE:-healthy}
state=${FAKE_DOCKER_STATE:?FAKE_DOCKER_STATE must be set}
mkdir -p "$state"
printf '%s\n' "$*" >>"$state/argv.log"
arg_after() { # FLAG ARGS... -> the value following FLAG
    local flag=$1 previous='' arg
    shift
    for arg in "$@"; do
        if [ "$previous" = "$flag" ]; then printf '%s\n' "$arg"; return 0; fi
        previous=$arg
    done
    return 0
}
last_arg() { local arg; for arg in "$@"; do :; done; printf '%s\n' "$arg"; }
# A label ledger is keyed by the reference, which may carry a registry path, so
# the key is flattened into one file name.
label_file() { printf '%s/labels.%s\n' "$state" "$(printf '%s' "$1" | tr '/' '_')"; }
ident() { printf 'sha256:%064x\n' "$1"; }
case "$1" in
    --version) echo 'Docker version 99.9.9, build fake' ;;
    version) printf '%s\n' "${FAKE_DAEMON_PLATFORM:-linux/amd64}" ;;
    context) echo fake-builder ;;
    buildx) printf 'Driver: docker\nBuildKit version: v0.0.0-fake\n' ;;
    pull)
        printf '%s %s\n' "$(arg_after --platform "$@")" "$(last_arg "$@")" >"$state/pulled"
        echo 'fake: pulled' ;;
    build)
        printf '%s\n' "$*" >"$state/build-argv"
        tag=$(arg_after -t "$@")
        [ -n "$tag" ] || { echo 'fake docker: no -t' >&2; exit 2; }
        [ "$mode" = build-fails ] && { echo 'ERROR: failed to solve: fake build failure' >&2; exit 1; }
        platform=$(arg_after --platform "$@")
        if [ "$mode" = wrong-platform ]; then
            case "$platform" in
                linux/arm64) platform=linux/amd64 ;;
                *) platform=linux/arm64 ;;
            esac
        fi
        ident "$(printf '%s' "$tag" | cksum | cut -d' ' -f1)" >"$state/tag-id.$tag"
        printf '%s\n' "$platform" >"$state/tag-platform.$tag"
        case "$mode" in
            labels-missing) printf '{}\n' >"$(label_file "$tag")" ;;
            *) [ -z "${FAKE_IMAGE_LABELS:-}" ] || cp "$FAKE_IMAGE_LABELS" "$(label_file "$tag")" ;;
        esac
        echo 'fake: built' ;;
    image)
        target=$(last_arg "$@")
        case "$2" in
            rm)
                [ "$mode" = cleanup-fails ] && {
                    echo "Error response from daemon: cannot remove $target: permission denied" >&2
                    exit 1
                }
                if [ -f "$state/tag-id.$target" ]; then
                    rm -f "$state/tag-id.$target" "$state/tag-platform.$target"
                    echo "Untagged: $target"
                    exit 0
                fi
                echo "Error response from daemon: No such image: $target" >&2
                exit 1 ;;
            inspect)
                [ "$mode" = image-inspect-fails ] && [ -f "$state/tag-id.$target" ] && {
                    echo 'garbage'
                    exit 0
                }
                if [ -f "$state/tag-id.$target" ]; then
                    id=$(cat "$state/tag-id.$target")
                    platform=$(cat "$state/tag-platform.$target")
                elif [ "$target" = "${FAKE_PARENT:-}" ]; then
                    id=$(ident 4242)
                    platform=${FAKE_DAEMON_PLATFORM:-linux/amd64}
                else
                    echo "Error response from daemon: No such image: $target" >&2
                    exit 1
                fi
                printf '[{"Id":"%s","RepoDigests":["%s"],"Os":"%s","Architecture":"%s","Size":12345,"Config":{"Labels":%s}}]\n' \
                    "$id" "$target" "${platform%%/*}" "${platform#*/}" \
                    "$([ -f "$(label_file "$target")" ] && cat "$(label_file "$target")" || echo '{}')" ;;
            *) exit 0 ;;
        esac ;;
    *) exit 0 ;;
esac
FAKE
}

self_test() {
    local scratch shims trace root case script url log status host_platform order pull_line build_line inspect_line first_tag second_tag
    local lost_tee real_tee git_shim real_git fake_parent control_repo control_layer
    local false_binary
    host_platform=$(native_platform)
    if [ "$host_platform" = unknown ]; then fail 'unsupported self-test host architecture'; fi
    # Global, not local: the EXIT trap runs after this function returns.
    SELFTEST_SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/layer-build-selftest.XXXXXX")
    scratch=$SELFTEST_SCRATCH
    trap 'rm -rf "$SELFTEST_SCRATCH"' EXIT
    mkdir -p "$scratch/bin"
    # No case needs a daemon: the sentinel socket below is used unless the caller
    # already set DOCKER_HOST, and the fake on PATH is the only Docker reachable.
    SELFTEST_DOCKER_HOST='unix:///nonexistent-layer-build-selftest.sock'
    write_fake_docker "$scratch/bin/docker"
    chmod 0755 "$scratch/bin/docker"
    fake_parent=$(awk '/^ARG BASE_IMAGE=/ { sub(/^ARG BASE_IMAGE=/, ""); print; exit }' \
        examples/layers/rust-dev/Dockerfile)
    mkdir -p "$scratch/labels"
    python3 "$ROOT/script/test/network_requests.py" source-labels \
        examples/layers/rust-dev/Dockerfile >"$scratch/labels/expected.json"

    run_case() { # MODE NAME [DOCKER_HOST] [PATH_PREFIX] [LAYER]
        # An inherited DOCKER_HOST is kept, so a host with a real daemon and a
        # host without one run exactly the same commands.
        local mode=$1 name=$2 docker_host=${3:-${DOCKER_HOST:-$SELFTEST_DOCKER_HOST}}
        local path_prefix=${4:-} layer=${5:-rust-dev}
        local labels=$scratch/labels/$name.json parent=$fake_parent
        RUN_EVID=$scratch/evidence/$name
        RUN_STATUS=0
        mkdir -p "$scratch/labels" "$scratch/state/$name"
        if [ "$layer" != rust-dev ]; then
            parent=$(awk '/^ARG BASE_IMAGE=/ { sub(/^ARG BASE_IMAGE=/, ""); print; exit }' \
                "examples/layers/$layer/Dockerfile")
            python3 "$ROOT/script/test/network_requests.py" source-labels \
                "examples/layers/$layer/Dockerfile" >"$labels"
        else
            # The built image's labels are the rust-dev recipe's own unless the case
            # is about a label that never reached the image or a rewritten parent.
            case "$mode" in
                labels-missing) printf '{}\n' >"$labels" ;;
                parent-label-overwritten)
                    printf '{"org.agent-vm.network-requests.image.base":"a.example"}\n' \
                        >"$scratch/state/$name/labels.$(printf '%s' "$fake_parent" | tr '/' '_')"
                    printf '{"org.agent-vm.network-requests.image.base":"b.example","org.agent-vm.network-requests.image.rust-dev":"crates.io,index.crates.io,static.crates.io"}\n' \
                        >"$labels" ;;
                *) cp "$scratch/labels/expected.json" "$labels" ;;
            esac
        fi
        # PATH first, DOCKER too: the builder guard invokes a bare `docker`, so
        # the fake has to be the only one reachable or the guard tests the host.
        # The EGRESS runner is the stub: a real one would need a daemon, and this
        # suite is about the phase, its bounds and its ordering.
        env PATH="$path_prefix$scratch/bin:$PATH" \
            DOCKER_HOST="$docker_host" \
            FAKE_DOCKER_MODE=$mode FAKE_DOCKER_STATE=$scratch/state/$name \
            FAKE_DAEMON_PLATFORM=$host_platform \
            FAKE_IMAGE_LABELS=$labels \
            FAKE_PARENT="$parent" \
            FAKE_EGRESS_MODE=${RUN_FAKE_EGRESS_MODE:-$mode} \
            LAYER_BUILD_SELF_TEST=${RUN_SELFTEST:-} \
            LAYER_BUILD_SELF_TEST_EGRESS_SECONDS=${RUN_EGRESS_BOUNDS:-} \
            LAYER_BUILD_SELF_TEST_EGRESS_WATCHDOG=${RUN_EGRESS_WATCHDOG:-} \
            LAYER_BUILD_EGRESS_RUNNER=$scratch/bin/egress-runner \
            DOCKER=$scratch/bin/docker \
            bash "${BASH_SOURCE[0]}" --layer "$layer" --platform "$host_platform" \
            --evidence-dir "$RUN_EVID" >"$scratch/$name.log" 2>&1 || RUN_STATUS=$?
    }
    # The EGRESS stub stands in for the real smoke: it records the argv it was
    # given and fails, hangs or finalizes on TERM exactly where a case needs it.
    # layer-build asks the runner which evidence names it owns, so the stub
    # forwards that one question and the self-test exercises the real list.
    {
        printf '#!/usr/bin/env bash\nset -uo pipefail\n'
        printf 'if [ "${1:-}" = --print-owned-files ]; then\n'
        printf '    exec bash %q --print-owned-files\n' "$EGRESS_RUNNER"
        printf 'fi\n'
        cat <<'STUB'
printf '%s\n' "$*" >>"${FAKE_DOCKER_STATE:?}/egress-argv.log"
printf 'egress-runner %s\n' "$*" >>"${FAKE_DOCKER_STATE:?}/argv.log"
mode=${FAKE_EGRESS_MODE:-healthy}
case "$mode" in
    egress-fails) exit 1 ;;
    egress-hangs)
        trap 'printf "finalized\n" >"${FAKE_DOCKER_STATE}/egress-finalized"; exit 0' TERM
        sleep 60
        exit 0 ;;
    egress-ignores-term)
        trap '' TERM
        sleep 60
        exit 0 ;;
esac
exit 0
STUB
    } >"$scratch/bin/egress-runner"
    chmod 0755 "$scratch/bin/egress-runner"

    echo 'self-test: healthy cold build'
    run_case healthy healthy
    selftest_expect "$RUN_STATUS" 0 'a healthy run exits 0'
    selftest_grep "PASS layer=rust-dev platform=$host_platform" "$scratch/healthy.log" \
        'the verdict names layer and platform'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt")" 'completed status=0' 'the owned tag was removed'
    selftest_expect "$(grep -c PASS "$RUN_EVID/verdicts.txt")" 5 'every phase passed'
    selftest_grep '--no-cache' "$scratch/state/healthy/build-argv" 'the build is cold'
    selftest_grep "--platform $host_platform" "$scratch/state/healthy/build-argv" 'the build is native'
    selftest_grep 'examples/layers/rust-dev' "$scratch/state/healthy/build-argv" 'the exact context is built'
    selftest_expect "$(cat "$scratch/state/healthy/pulled")" \
        "$host_platform $fake_parent" 'the committed parent is pulled for this platform'
    selftest_grep "--build-arg BASE_IMAGE=$(cut -d' ' -f2 "$scratch/state/healthy/pulled")" \
        "$scratch/state/healthy/build-argv" 'the build gets the pulled committed parent'
    selftest_expect "$(grep -c 'agent-vm-layer-build:' "$scratch/state/healthy/build-argv" || true)" 1 \
        'the run owns exactly one tag'
    first_tag=$(cat "$RUN_EVID/owned-tag.txt")
    selftest_grep "$first_tag" "$scratch/state/healthy/build-argv" \
        'the recorded owned tag is the tag that was built'
    pull_line=$(grep -n '^pull ' "$scratch/state/healthy/argv.log" | cut -d: -f1)
    selftest_expect "$(grep -c '^pull ' "$scratch/state/healthy/argv.log" || true)" 1 \
        'the parent is pulled exactly once'
    build_line=$(grep -n '^build ' "$scratch/state/healthy/argv.log" | cut -d: -f1)
    inspect_line=$(grep -n '^image inspect agent-vm-layer-build:' "$scratch/state/healthy/argv.log" |
        head -1 | cut -d: -f1)
    order=$([ "$pull_line" -lt "$build_line" ] && [ "$build_line" -lt "$inspect_line" ] &&
        echo pull-build-inspect || echo "$pull_line/$build_line/$inspect_line")
    selftest_expect "$order" pull-build-inspect \
        'the pull, the build and the output inspection happen in that order'
    selftest_expect "$(grep -c 'image rm' "$scratch/state/healthy/argv.log" || true)" 1 \
        'only one image is removed'

    echo 'self-test: the runner needs no Docker daemon'
    # A socket that cannot exist: if any part of the run reached a real Docker
    # client instead of the fake on PATH, this case would fail.
    selftest_expect "$SELFTEST_DOCKER_HOST" 'unix:///nonexistent-layer-build-selftest.sock' \
        'the control points DOCKER_HOST at a socket that cannot exist'
    run_case healthy daemon-free "$SELFTEST_DOCKER_HOST"
    selftest_expect "$RUN_STATUS" 0 'a healthy run exits 0 without a daemon'
    selftest_expect "$(grep -c '^buildx inspect' "$scratch/state/daemon-free/argv.log" || true)" 2 \
        'the shared builder guard and the runner both reached the fake docker'

    echo 'self-test: every run owns a fresh tag'
    run_case healthy healthy-again
    selftest_expect "$RUN_STATUS" 0 'a second healthy run exits 0'
    second_tag=$(cat "$RUN_EVID/owned-tag.txt")
    selftest_expect "$([ "$first_tag" != "$second_tag" ] && echo distinct || echo "$first_tag")" \
        distinct 'a second run cannot reuse the first run tag'

    echo 'self-test: a foreign tag is never touched'
    mkdir -p "$scratch/state/foreign"
    printf 'sha256:%064d\n' 7 >"$scratch/state/foreign/tag-id.foreign:keep"
    printf 'linux/amd64\n' >"$scratch/state/foreign/tag-platform.foreign:keep"
    run_case healthy foreign
    selftest_expect "$RUN_STATUS" 0 'a run sharing the daemon still passes'
    selftest_expect "$(cat "$scratch/state/foreign/tag-id.foreign:keep")" "$(printf 'sha256:%064d' 7)" \
        'the foreign image is still there'
    selftest_expect "$(grep -c 'foreign:keep' "$scratch/state/foreign/argv.log" || true)" 0 \
        'the foreign tag is never named'

    echo 'self-test: a wrong platform image is not acceptance'
    run_case wrong-platform wrong-platform
    selftest_refused "$RUN_STATUS" 'a wrong-platform image fails the run'
    selftest_grep 'built image platform mismatch' "$scratch/wrong-platform.log" 'the mismatch is named'
    selftest_expect "$(grep -c 'INSPECT FAIL' "$RUN_EVID/verdicts.txt" || true)" 1 'the failing phase is recorded'

    echo 'self-test: a failed build never reaches output inspection'
    run_case build-fails build-fails
    selftest_refused "$RUN_STATUS" 'a failed build fails the run'
    selftest_expect "$([ -f "$RUN_EVID/image-inspect.log" ] && echo inspected || echo uninspected)" \
        uninspected 'a failed build produces no output-inspection evidence'
    selftest_grep 'BUILD FAIL' "$RUN_EVID/verdicts.txt" 'the build failure is recorded'
    selftest_grep 'INSPECT NOT-RUN' "$RUN_EVID/verdicts.txt" 'the unrun phase is not a pass'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt")" 'completed status=0' \
        'a confirmed absent tag after a failed build is not a cleanup failure'
    selftest_grep 'cleanup=absent' "$RUN_EVID/evidence.txt" 'the absence is recorded as such'

    echo 'self-test: a malformed output inspection is not acceptance'
    run_case image-inspect-fails image-inspect-fails
    selftest_refused "$RUN_STATUS" 'a malformed inspection fails the run'
    selftest_grep 'built image inspection has no ID' "$scratch/image-inspect-fails.log" \
        'the malformed evidence is named'
    selftest_expect "$(grep -c 'INSPECT PASS' "$RUN_EVID/verdicts.txt" || true)" 0 \
        'the phase never passes'

    echo 'self-test: the built image must carry the request labels'
    selftest_grep 'network_requests=checked' "$scratch/evidence/healthy/evidence.txt" \
        'a healthy run records the label check'
    run_case labels-missing labels-missing
    selftest_refused "$RUN_STATUS" 'a built image without its request label fails the run'
    selftest_grep 'did not reach the final stage' "$RUN_EVID/network-requests.log" \
        'the missing label is named'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt")" 'completed status=0' \
        'the owned tag is still removed after a label failure'
    selftest_grep 'INSPECT FAIL' "$RUN_EVID/verdicts.txt" 'the failing phase is recorded'

    echo 'self-test: a layer cannot rewrite its parent request label'
    run_case parent-label-overwritten parent-label-overwritten
    selftest_refused "$RUN_STATUS" 'a rewritten inherited label fails the run'
    selftest_grep 'but the sources and parent imply' "$RUN_EVID/network-requests.log" \
        'the rewritten value is named'

    echo 'self-test: the egress smoke runs on the owned tag, before cleanup'
    selftest_grep 'egress_fixture=yes' "$scratch/evidence/healthy/evidence.txt" \
        'the fixture is recorded'
    selftest_grep "supervisor=$EGRESS_SUPERVISOR_SECONDS/$EGRESS_SUPERVISOR_GRACE_SECONDS" \
        "$scratch/evidence/healthy/evidence.txt" 'a layer with a fixture gets the egress bounds'
    selftest_grep 'EGRESS PASS' "$scratch/evidence/healthy/verdicts.txt" 'the egress phase passed'
    selftest_grep "$first_tag" "$scratch/state/healthy/egress-argv.log" \
        'the smoke ran on the tag this run built'
    selftest_grep '--evidence-dir' "$scratch/state/healthy/egress-argv.log" \
        'the smoke was given an evidence directory'
    egress_line=$(grep -n '^egress-runner ' "$scratch/state/healthy/argv.log" | cut -d: -f1)
    rm_line=$(grep -n '^image rm ' "$scratch/state/healthy/argv.log" | cut -d: -f1)
    selftest_expect "$([ "$egress_line" -lt "$rm_line" ] && echo egress-before-removal ||
        echo "$egress_line/$rm_line")" egress-before-removal \
        'the smoke runs before the owned tag is removed'

    echo 'self-test: a failing egress smoke fails the run and still cleans up'
    run_case egress-fails egress-fails
    selftest_refused "$RUN_STATUS" 'a failed smoke fails the run'
    selftest_grep 'EGRESS FAIL' "$RUN_EVID/verdicts.txt" 'the egress phase is the failure'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt")" 'completed status=0' \
        'the owned tag is still removed after a failed smoke'

    echo "self-test: a layer without a fixture keeps today's bounds"
    run_case healthy wirenboard "$SELFTEST_DOCKER_HOST" '' wirenboard-cpp
    selftest_expect "$RUN_STATUS" 0 'a layer without a fixture still passes'
    selftest_expect "$(grep -c PASS "$RUN_EVID/verdicts.txt")" 4 \
        'a layer without a fixture runs four phases'
    selftest_grep "supervisor=$SUPERVISOR_SECONDS/$SUPERVISOR_GRACE_SECONDS" \
        "$RUN_EVID/evidence.txt" 'a layer without a fixture keeps the current bounds'
    selftest_grep 'egress_fixture=none' "$RUN_EVID/evidence.txt" 'the absent fixture is recorded'
    selftest_expect "$([ -s "$scratch/state/wirenboard/egress-argv.log" ] && echo invoked ||
        echo never)" never 'the smoke is never invoked for a layer without a fixture'

    echo 'self-test: a symlinked fixture is refused in PREFLIGHT'
    # Discovery is the filesystem and the fixture path is fixed relative to the
    # checkout, so this case builds its OWN checkout inside the invocation-owned
    # scratch instead of planting a context in the real one. The path it used to
    # write is one a contributor could already occupy, and the control then
    # deleted it along with its own files.
    control_layer=network-requests-symlink-control
    control_repo=$scratch/symlink-repo
    mkdir -p "$control_repo/script/test/fixtures/network-requests" \
        "$control_repo/examples/layers/$control_layer"
    # The supervisor re-execs this script from its own checkout, so the control
    # tree carries the watchdog too, and the egress runner whose evidence list
    # this script reads at allocation.
    cp script/test/layer-build.sh script/test/host-watchdog.py \
        script/test/network-requests-egress.sh "$control_repo/script/test/"
    printf 'FROM scratch\n' >"$control_repo/examples/layers/$control_layer/Dockerfile"
    printf 'keep me\n' >"$control_repo/examples/layers/$control_layer/pre-existing-file"
    ln -s "$ROOT/script/test/fixtures/network-requests/rust-dev" \
        "$control_repo/script/test/fixtures/network-requests/$control_layer"
    RUN_EVID=$scratch/evidence/egress-symlink
    mkdir -p "$RUN_EVID"
    RUN_STATUS=0
    env PATH="$scratch/bin:$PATH" DOCKER_HOST="$SELFTEST_DOCKER_HOST" \
        DOCKER="$scratch/bin/docker" FAKE_DOCKER_MODE=healthy \
        FAKE_DOCKER_STATE="$scratch/state/egress-symlink" \
        FAKE_DAEMON_PLATFORM="$host_platform" \
        bash "$control_repo/script/test/layer-build.sh" --layer "$control_layer" \
        --platform "$host_platform" --evidence-dir "$RUN_EVID" \
        >"$scratch/egress-symlink.log" 2>&1 || RUN_STATUS=$?
    selftest_refused "$RUN_STATUS" 'a symlinked fixture is refused'
    selftest_grep 'is a symlink, not a real fixture directory' "$scratch/egress-symlink.log" \
        'the symlink is named'
    selftest_expect "$(grep -c 'PREFLIGHT FAIL' "$RUN_EVID/verdicts.txt" || true)" 1 \
        'the refusal is the recorded verdict'
    selftest_expect "$(cat "$control_repo/examples/layers/$control_layer/pre-existing-file" \
        2>/dev/null || echo gone)" 'keep me' \
        'a file already in the control context is not removed'
    selftest_expect "$([ -L "$control_repo/script/test/fixtures/network-requests/$control_layer" \
        ] && echo kept || echo gone)" kept 'the control fixture symlink is left alone'
    selftest_expect "$([ -e "examples/layers/$control_layer" ] && echo planted || echo clean)" \
        clean 'the control context was not planted in the real checkout'
    selftest_expect "$([ -e "script/test/fixtures/network-requests/$control_layer" ] && echo planted \
        || echo clean)" clean 'the control fixture was not planted in the real checkout'

    echo 'self-test: a hung smoke is bounded by the phase grace'
    RUN_SELFTEST=1
    RUN_EGRESS_BOUNDS="5:2:4000:3"
    RUN_FAKE_EGRESS_MODE=egress-hangs
    run_case healthy egress-hangs
    RUN_SELFTEST=''
    RUN_EGRESS_BOUNDS=''
    RUN_FAKE_EGRESS_MODE=''
    selftest_refused "$RUN_STATUS" 'a hung smoke fails the run'
    selftest_grep 'EGRESS FAIL' "$RUN_EVID/verdicts.txt" 'the egress phase is the failure'
    selftest_expect "$(grep -cE '^egress seconds=[0-9]+ status=124$' "$RUN_EVID/durations.txt" || true)" 1 \
        'the bound, not a signal, ended the smoke'
    selftest_grep 'finalized' "$scratch/state/egress-hangs/egress-finalized" \
        'the TERM reached the smoke and it finalized its evidence'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt")" 'completed status=0' \
        'the owned tag is still removed after a hung smoke'

    echo 'self-test: a supervisor TERM during EGRESS still reaches cleanup'
    # The chain under test: the real supervisor, `timed_graceful`, and a watchdog
    # whose group check consumes BOTH escalation windows while a TERM-ignoring
    # smoke runs. The supervisor grace has to outlast 2 x egress grace + cleanup.
    # The seam replaces the command, not its arguments: the committed fixture
    # loads the real watchdog beside it and overrides only the group check, so the
    # argv the real one parses is the argv it gets.
    selftest_expect "$WATCHDOG" "$ROOT/script/test/host-watchdog.py" \
        'the fault fixture loads the watchdog this runner is configured with'
    inner_watchdog="$ROOT/script/test/inner-watchdog-fault.py"
    # g = 4 s egress grace, c = 3 s cleanup, slack 0.5 s: the production formula is
    # supervisor_grace > 2g + c = 11 s, so 11.5 s covers the 2 x 4 s of escalation
    # windows plus the removal, and the one-window control (g + c + slack = 7.5 s)
    # is killed before its cleanup can start.
    nested() { # NAME SUPERVISOR_GRACE
        local name=$1 grace=$2 pid waited=0
        mkdir -p "$scratch/state/$name"
        cp "$scratch/labels/expected.json" "$scratch/labels/$name.json"
        RUN_EVID=$scratch/evidence/$name
        # `exec` in the background subshell makes the test's PID the supervisor's
        # own, which is what the signal has to reach.
        (
            export PATH="$scratch/bin:$PATH" DOCKER_HOST="$SELFTEST_DOCKER_HOST"
            export DOCKER="$scratch/bin/docker"
            export FAKE_DOCKER_MODE=healthy FAKE_DOCKER_STATE="$scratch/state/$name"
            export FAKE_DAEMON_PLATFORM="$host_platform"
            export FAKE_IMAGE_LABELS="$scratch/labels/$name.json"
            export FAKE_PARENT="$fake_parent" FAKE_EGRESS_MODE=egress-ignores-term
            export LAYER_BUILD_SELF_TEST=1
            export LAYER_BUILD_SELF_TEST_EGRESS_SECONDS="12:4:$grace:3"
            export LAYER_BUILD_SELF_TEST_EGRESS_WATCHDOG="$inner_watchdog"
            export LAYER_BUILD_EGRESS_RUNNER="$scratch/bin/egress-runner"
            exec bash "${BASH_SOURCE[0]}" --layer rust-dev --platform "$host_platform" \
                --evidence-dir "$scratch/evidence/$name"
        ) >"$scratch/$name.log" 2>&1 &
        pid=$!
        while [ "$waited" -lt 200 ]; do
            [ -s "$scratch/state/$name/egress-argv.log" ] && break
            sleep 0.1
            waited=$((waited + 1))
        done
        kill -TERM "$pid" 2>/dev/null || true
        RUN_STATUS=0
        wait "$pid" || RUN_STATUS=$?
    }
    nested nested-supervisor 11.5
    selftest_expect "$RUN_STATUS" 143 'the supervisor exits on the inherited TERM'
    selftest_grep 'EGRESS FAIL' "$RUN_EVID/verdicts.txt" 'the interrupted phase is a failure'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt" 2>/dev/null || echo none)" \
        'completed status=0' 'both escalation windows are covered and cleanup completed'
    selftest_grep 'image rm' "$scratch/state/nested-supervisor/argv.log" \
        'the owned tag was removed after the nested escalation'
    nested nested-supervisor-control 7.5
    selftest_expect "$([ -f "$RUN_EVID/cleanup.txt" ] && echo entered || echo skipped)" skipped \
        'a one-window grace is not enough: cleanup is killed before it starts'

    echo 'self-test: a genuine cleanup failure is still a failure'
    run_case cleanup-fails cleanup-fails
    selftest_refused "$RUN_STATUS" 'a cleanup failure fails the run'
    selftest_grep 'owned-tag cleanup failed' "$scratch/cleanup-fails.log" 'the cleanup failure is named'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt")" 'completed status=1' 'the cleanup result is recorded'

    echo 'self-test: a lost cleanup transcript is a cleanup failure'
    # A `tee` that records its transcript and then fails: the removal itself
    # succeeds, so only the record of it is lost.
    lost_tee=$scratch/lost-tee
    mkdir -p "$lost_tee"
    real_tee=$(command -v tee)
    cat >"$lost_tee/tee" <<SHIM
#!/usr/bin/env bash
"$real_tee" "\$@"
status=\$?
for argument in "\$@"; do log=\$argument; done
case "\$log" in */cleanup-*.log) exit 1 ;; esac
exit "\$status"
SHIM
    chmod 0755 "$lost_tee/tee"
    run_case healthy lost-cleanup-log "$SELFTEST_DOCKER_HOST" "$lost_tee:"
    selftest_refused "$RUN_STATUS" 'a lost cleanup transcript fails the run'
    selftest_grep 'owned-tag cleanup failed' "$scratch/lost-cleanup-log.log" \
        'the lost transcript is named'
    selftest_expect "$(cat "$RUN_EVID/cleanup.txt")" 'completed status=1' \
        'the cleanup status is recorded as failed'
    selftest_grep 'cleanup=remove-log-lost' "$RUN_EVID/evidence.txt" \
        'a lost record is not an absent image'
    selftest_expect "$(grep -c 'PASS' "$RUN_EVID/verdicts.txt" || true)" 5 'the build itself passed'

    echo 'self-test: a refused run keeps no earlier verdicts'
    RUN_EVID=$scratch/evidence/refused
    mkdir -p "$RUN_EVID"
    printf 'PREFLIGHT PASS\nPULL PASS\nBUILD PASS\nINSPECT PASS\n' >"$RUN_EVID/verdicts.txt"
    printf 'completed status=0\n' >"$RUN_EVID/cleanup.txt"
    printf 'agent-vm-layer-build:stale\n' >"$RUN_EVID/owned-tag.txt"
    printf '[{"Id":"sha256:stale"}]\n' >"$RUN_EVID/image-inspect.log"
    RUN_STATUS=0
    env PATH="$scratch/bin:$PATH" DOCKER_HOST="$SELFTEST_DOCKER_HOST" \
        FAKE_DOCKER_STATE=$scratch/state/refused FAKE_DAEMON_PLATFORM="$host_platform" \
        DOCKER=$scratch/bin/docker \
        bash "${BASH_SOURCE[0]}" --layer not-a-layer --platform "$host_platform" \
        --evidence-dir "$RUN_EVID" >"$scratch/refused.log" 2>&1 || RUN_STATUS=$?
    selftest_refused "$RUN_STATUS" 'a context that does not exist is refused'
    selftest_expect "$(grep -c 'PASS' "$RUN_EVID/verdicts.txt" || true)" 0 \
        'the earlier run PASS verdicts are gone'
    selftest_grep 'PREFLIGHT FAIL' "$RUN_EVID/verdicts.txt" 'the refusal is the recorded verdict'
    selftest_grep 'preflight_failure=' "$RUN_EVID/evidence.txt" 'the refusal reason is recorded'
    selftest_expect "$([ -f "$RUN_EVID/owned-tag.txt" ] && echo kept || echo removed)" removed \
        'the earlier run owned tag is not adopted'
    selftest_expect "$([ -e "$RUN_EVID/image-inspect.log" ] && echo kept || echo removed)" removed \
        'the earlier run phase log is gone with its verdict'

    echo 'self-test: a reused evidence directory cannot keep an old egress verdict'
    # The smoke owns $EVID/egress and is the only writer of it. An earlier run's
    # `strict=PASS` beside this run's `EGRESS NOT-RUN` is two run identities in one
    # uploaded tree, so the subdirectory is initialised at allocation, before
    # PREFLIGHT can fail. A Docker client that always fails makes PREFLIGHT the
    # failure, which is the earliest one there is.
    RUN_EVID=$scratch/evidence/stale-egress
    mkdir -p "$RUN_EVID/egress"
    printf 'strict=PASS\ndenial=PASS\n' >"$RUN_EVID/egress/summary.txt"
    printf 'ALLOW https://stale.example/\n' >"$RUN_EVID/egress/egress-strict.log"
    printf 'keep me\n' >"$RUN_EVID/egress/unrelated.txt"
    printf 'keep me\n' >"$RUN_EVID/keep.txt"
    false_binary=$(command -v false)
    RUN_STATUS=0
    env PATH="$scratch/bin:$PATH" DOCKER_HOST="$SELFTEST_DOCKER_HOST" \
        DOCKER="$false_binary" bash "${BASH_SOURCE[0]}" --layer rust-dev \
        --platform "$host_platform" --evidence-dir "$RUN_EVID" \
        >"$scratch/stale-egress.log" 2>&1 || RUN_STATUS=$?
    selftest_refused "$RUN_STATUS" 'a preflight failure after reuse still fails'
    selftest_expect "$(grep -c 'PREFLIGHT FAIL' "$RUN_EVID/verdicts.txt" || true)" 1 \
        'the refusal is the recorded verdict'
    selftest_grep 'EGRESS NOT-RUN' "$RUN_EVID/verdicts.txt" 'the egress phase never ran'
    selftest_expect "$([ -e "$RUN_EVID/egress/summary.txt" ] && echo kept || echo cleared)" \
        cleared 'the earlier run egress verdict is gone'
    selftest_expect "$([ -e "$RUN_EVID/egress/egress-strict.log" ] && echo kept || echo cleared)" \
        cleared 'the earlier run egress log is gone'
    selftest_expect "$(cat "$RUN_EVID/egress/unrelated.txt")" 'keep me' \
        'an unrelated file inside the egress directory stays'
    selftest_expect "$(cat "$RUN_EVID/keep.txt")" 'keep me' 'an unrelated evidence file is untouched'

    echo 'self-test: a usage error touches no evidence'
    # The one accepted exception to "every failure records itself": an unknown
    # option exits 2 while the arguments are parsed, before any file is opened.
    RUN_EVID=$scratch/evidence/usage
    mkdir -p "$RUN_EVID"
    printf 'PREFLIGHT PASS\n' >"$RUN_EVID/verdicts.txt"
    RUN_STATUS=0
    env PATH="$scratch/bin:$PATH" DOCKER_HOST="$SELFTEST_DOCKER_HOST" \
        FAKE_DOCKER_STATE=$scratch/state/usage DOCKER=$scratch/bin/docker \
        bash "${BASH_SOURCE[0]}" --layer rust-dev --platform "$host_platform" \
        --evidence-dir "$RUN_EVID" --no-such-option >"$scratch/usage.log" 2>&1 || RUN_STATUS=$?
    selftest_expect "$RUN_STATUS" 2 'an unknown option exits 2'
    selftest_expect "$(cat "$RUN_EVID/verdicts.txt")" 'PREFLIGHT PASS' \
        'a usage error leaves the evidence untouched'

    echo 'self-test: each git fact carries its own outcome'
    git_shim=$scratch/git-shim
    real_git=$(command -v git)
    # rev-parse fails while status reports a clean tree: the commit is unknown,
    # the tree state is not.
    mkdir -p "$git_shim/no-commit"
    cat >"$git_shim/no-commit/git" <<SHIM
#!/usr/bin/env bash
[ "\${1:-}" != rev-parse ] || { echo 'shim: rev-parse refused' >&2; exit 1; }
[ "\${1:-}" != status ] || exit 0
exec "$real_git" "\$@"
SHIM
    chmod 0755 "$git_shim/no-commit/git"
    run_case healthy git-no-commit "$SELFTEST_DOCKER_HOST" "$git_shim/no-commit:"
    selftest_expect "$RUN_STATUS" 0 'a run without a source commit still passes'
    selftest_grep 'source_sha=unknown' "$RUN_EVID/evidence.txt" 'the unestablished commit is unknown'
    selftest_grep 'dirty=no' "$RUN_EVID/evidence.txt" 'the clean tree is still recorded'
    selftest_expect "$(grep -c '^git=' "$RUN_EVID/evidence.txt" || true)" 0 \
        'no combined git fact contradicts them'
    selftest_expect "$(grep -cE '^source-sha seconds=[0-9]+ status=1$' "$RUN_EVID/durations.txt")" 1 \
        'the failed rev-parse carries its own failure mark'
    selftest_expect "$(grep -cE '^worktree-status seconds=[0-9]+ status=0$' "$RUN_EVID/durations.txt")" 1 \
        'the clean tree carries its own success mark'

    # The other direction: status fails, so the tree state is unknown even though
    # the checkout is at a commit.
    mkdir -p "$git_shim/no-status"
    cat >"$git_shim/no-status/git" <<SHIM
#!/usr/bin/env bash
[ "\${1:-}" != status ] || { echo 'shim: status refused' >&2; exit 1; }
exec "$real_git" "\$@"
SHIM
    chmod 0755 "$git_shim/no-status/git"
    run_case healthy git-no-status "$SELFTEST_DOCKER_HOST" "$git_shim/no-status:"
    selftest_expect "$RUN_STATUS" 0 'a run without a tree status still passes'
    selftest_grep 'dirty=unknown' "$RUN_EVID/evidence.txt" 'an unchecked tree is unknown, not clean'
    selftest_expect "$(grep -cE '^source_sha=[0-9a-f]{40}$' "$RUN_EVID/evidence.txt")" 1 \
        'the commit is recorded when rev-parse succeeds'

    echo 'self-test: installer URL diagnostics'
    shims=$scratch/installer-shims
    trace=$scratch/installer-trace
    root=$scratch/installers
    mkdir -p "$shims" "$root"
    cat >"$shims/dpkg" <<'SHIM'
#!/usr/bin/env bash
echo amd64
SHIM
    cat >"$shims/curl" <<'SHIM'
#!/usr/bin/env bash
set -uo pipefail
printf 'curl %s\n' "$*" >>"$INSTALLER_TRACE"
target=''
while [ "$#" -gt 0 ]; do
    case "$1" in -o) shift; target=$1 ;; esac
    shift
done
if [ "${FAKE_CURL_MODE:-wrong-bytes}" = 404 ]; then
    echo 'curl: (22) The requested URL returned error: 404' >&2
    exit 22
fi
[ -n "$target" ] && printf 'not the pinned bytes' >"$target"
exit 0
SHIM
    for tool in tar unzip; do
        printf '#!/usr/bin/env bash\nprintf "%s %%s\\n" "$*" >>"$INSTALLER_TRACE"\nexit 0\n' "$tool" \
            >"$shims/$tool"
    done
    chmod 0755 "$shims/dpkg" "$shims/curl" "$shims/tar" "$shims/unzip"
    command -v sha256sum >/dev/null 2>&1 || fail 'sha256sum required for the installer controls'

    control_version=0.0.0-control
    zeros=$(printf '0%.0s' $(seq 1 64))
    run_installer() { # SCRIPT MODE LOG
        local status=0
        : >"$trace"
        env PATH="$shims:$PATH" INSTALLER_TRACE="$trace" FAKE_CURL_MODE="$2" \
            VERUS_RELEASE="$control_version" VERUS_SHA256="$zeros" VERUS_DIR="$root/verus" \
            GO_VERSION="$control_version" GO_AMD64_SHA256="$zeros" GO_ARM64_SHA256="$zeros" \
            GO_PREFIX="$root/go" GOLANGCI_LINT_VERSION="$control_version" \
            GOLANGCI_LINT_AMD64_SHA256="$zeros" GOLANGCI_LINT_ARM64_SHA256="$zeros" \
            GO_TOOLS_BIN="$root/go-tools" \
            bash "$1" >"$3" 2>&1 || status=$?
        return "$status"
    }
    expected_url() { # CASE
        case "$1" in
            verus) echo "https://github.com/verus-lang/verus/releases/download/release/$control_version/verus-$control_version-x86-linux.zip" ;;
            go) echo "https://go.dev/dl/go$control_version.linux-amd64.tar.gz" ;;
            golangci-lint) echo "https://github.com/golangci/golangci-lint/releases/download/v$control_version/golangci-lint-$control_version-linux-amd64.tar.gz" ;;
        esac
    }
    for case in verus go golangci-lint; do
        case "$case" in
            verus) script=examples/layers/rust-dev/install-verus.sh ;;
            go) script=examples/layers/go-dev/install-go.sh ;;
            golangci-lint) script=examples/layers/go-dev/install-golangci-lint.sh ;;
        esac
        script="$ROOT/$script"
        if [ "$case" = verus ]; then
            # Verus writes a fixed /tmp path: only a scratch copy is repointed.
            sed "s|^archive=\"/tmp/|archive=\"$root/|" "$script" >"$root/install-verus.sh"
            script=$root/install-verus.sh
            selftest_grep "archive=\"$root/" "$script" "the $case control writes inside the scratch tree"
        fi
        url=$(expected_url "$case")
        for mode in 404 wrong-bytes; do
            log=$root/$case-$mode.log
            status=0
            run_installer "$script" "$mode" "$log" || status=$?
            selftest_refused "$status" "$case $mode: the installer refuses"
            selftest_grep "fetch $url" "$log" "$case $mode: the log names the exact URL"
            selftest_grep "$url" "$trace" "$case $mode: curl was given that exact URL"
            selftest_expect "$(grep -cE '^(tar|unzip) ' "$trace" || true)" 0 \
                "$case $mode: nothing was extracted"
            if [ "$mode" = 404 ]; then
                selftest_grep '404' "$log" "$case 404: the HTTP error is visible"
            else
                selftest_grep 'FAILED' "$log" "$case wrong-bytes: the digest check refused the bytes"
            fi
        done
    done
    # The bounds here and in the egress runner are one arithmetic system: every
    # outer value has to outlast the inner one it can interrupt, at both nesting
    # levels. The runner publishes its own totals, so neither script restates the
    # other's numbers; a later edit that breaks a relation fails this suite
    # instead of a CI row that is already minutes deep.
    echo 'self-test: budget invariants'
    local budget runner_active runner_finalize runner_max phases_without phases_with
    budget=$("$EGRESS_RUNNER" --print-budget)
    budget_value() { # KEY BUDGET -> the value, or empty when the key is missing
        local line
        for line in $2; do
            case "$line" in
                "$1="*) printf '%s\n' "${line#*=}"; return 0 ;;
            esac
        done
    }
    runner_active=$(budget_value ACTIVE_SECONDS "$budget")
    runner_finalize=$(budget_value FINALIZE_SECONDS "$budget")
    runner_max=$(budget_value MAX_SECONDS "$budget")
    for value in "$runner_active" "$runner_finalize" "$runner_max"; do
        case "$value" in
            '' | *[!0-9]*) fail "the egress runner printed no usable budget: $budget" ;;
        esac
    done
    selftest_expect "$([ "$runner_max" -gt "$runner_active" ] && echo grows || echo equal)" grows \
        'the runner maximum includes its finalizer'
    selftest_expect "$([ "$EGRESS_KILL_GRACE_SECONDS" -gt "$runner_finalize" ] && echo outlasts || echo shorter)" \
        outlasts 'the egress grace outlasts the runner finalizer'
    selftest_expect "$([ "$EGRESS_SECONDS" -gt "$runner_max" ] && echo outlasts || echo shorter)" outlasts \
        'the egress bound outlasts the runner maximum'
    selftest_expect "$EGRESS_PHASE_SECONDS" \
        "$((EGRESS_SECONDS + 2 * EGRESS_KILL_GRACE_SECONDS))" \
        'the EGRESS phase covers its runner and both escalation windows'
    selftest_expect \
        "$([ "$EGRESS_SUPERVISOR_GRACE_SECONDS" -gt \
            "$((2 * EGRESS_KILL_GRACE_SECONDS + CLEANUP_SECONDS))" ] && echo outlasts || echo shorter)" \
        outlasts 'the supervisor grace covers both nested windows and the cleanup'
    selftest_expect "$([ "$SUPERVISOR_GRACE_SECONDS" -gt "$CLEANUP_SECONDS" ] && echo outlasts || echo shorter)" \
        outlasts 'the supervisor grace covers the cleanup'
    phases_without=$((PREFLIGHT_SECONDS + PULL_PHASE_SECONDS + BUILD_PHASE_SECONDS \
        + INSPECT_PHASE_SECONDS + CLEANUP_SECONDS))
    phases_with=$((phases_without + EGRESS_PHASE_SECONDS))
    selftest_expect "$([ "$phases_without" -lt "$SUPERVISOR_SECONDS" ] && echo inside || echo over)" inside \
        'the phase sum without EGRESS fits its supervisor'
    selftest_expect "$([ "$phases_with" -lt "$EGRESS_SUPERVISOR_SECONDS" ] && echo inside || echo over)" inside \
        'the phase sum with EGRESS fits its supervisor'

    echo 'self-test: passed'
}
if [ "$SELF_TEST" -eq 1 ]; then
    [ "$LAYER" = '' ] && [ "$PLATFORM" = '' ] && [ "$EVID" = '' ] || usage
    self_test
    exit 0
fi

# The supervisor is the whole run's bound, so a hung daemon client cannot outlive
# it. The marker keeps the re-exec to exactly one level.
if [ "${LAYER_BUILD_SUPERVISED:-}" != 1 ]; then
    export LAYER_BUILD_SUPERVISED=1
    exec python3 "$WATCHDOG" --grace-seconds "$supervisor_grace" "$supervisor_seconds" \
        bash "${BASH_SOURCE[0]}" ${ORIGINAL_ARGUMENTS[@]+"${ORIGINAL_ARGUMENTS[@]}"}
fi

# Everything from here runs once, under the supervisor, and owns its evidence.
# Allocation comes before any argument or context validation, so a refused run
# writes its own verdicts instead of leaving a previous run's PASS in place. A
# usage error is the one exception: it exits 2 while the arguments are parsed
# above, before this point, and touches no file.
RUN_ID=$(printf '%s' \
    "${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$-$(date +%s)-$(sanitize "$LAYER")-$(sanitize "$PLATFORM")" |
    cut -c1-100)
EVID=${EVID:-$ROOT/target/layer-build/$RUN_ID}
mkdir -p "$EVID"
EVID=$(cd "$EVID" && pwd)
TAG="agent-vm-layer-build:$RUN_ID"

PHASE_DEADLINE=$SECONDS
CLEANUP_DEADLINE=$SECONDS
ACTIVE=''
ACTIVE_START=0
TEE_LOST=0
record() { printf '%s=%s\n' "$1" "$2" >> "$EVID/evidence.txt"; }
record_command() { # LABEL COMMAND...
    local label=$1
    shift
    {
        printf 'command.%s=' "$label"
        printf '%q ' "$@"
        printf '\n'
    } >> "$EVID/evidence.txt"
}
pass() { echo "$CURRENT PASS" >> "$EVID/verdicts.txt"; }
preflight_refuse() { # MESSAGE STATUS
    record preflight_failure "$1"
    echo "layer-build: $1" >&2
    exit "$2"
}
phase_begin() { PHASE_DEADLINE=$((SECONDS + $1)); }

# Every streamed command is judged by both ends of its pipeline: the child's
# own status is the verdict, and a `tee` that failed to record the transcript is
# a failure too, because the log is the only proof of what the child did.
# TEE_LOST tells the caller which end failed, so a lost record is never excused
# as "the child must have succeeded anyway".
stream() { # LOG SECONDS GRACE COMMAND...
    local log=$1 seconds=$2 grace=$3
    local -a ends=(0 0)
    shift 3
    TEE_LOST=0
    if ! python3 "${STREAM_WATCHDOG:-$WATCHDOG}" --grace-seconds "$grace" "$seconds" "$@" 2>&1 |
        tee -a "$log"; then
        ends=("${PIPESTATUS[@]}")
        [ "${ends[1]}" -eq 0 ] || TEE_LOST=1
        [ "${ends[0]}" -ne 0 ] || return 1
        return "${ends[0]}"
    fi
    return 0
}

# Every host command runs under the shared watchdog, clamped to what is left of
# its phase so a slow child cannot borrow the next phase's budget. The child's
# argv is recorded, and its output streams to the caller and the phase log at
# once, so an interrupted run keeps the transcript up to the interruption.
timed() { # LABEL SECONDS COMMAND...
    local label=$1 seconds=$2 status=0 remaining
    shift 2
    remaining=$((PHASE_DEADLINE - SECONDS))
    [ "$seconds" -le "$remaining" ] || seconds=$remaining
    if [ "$seconds" -le 0 ]; then
        printf '%s seconds=0 status=skipped-deadline\n' "$label" >> "$EVID/durations.txt"
        return 1
    fi
    record_command "$label" "$@"
    ACTIVE=$label
    ACTIVE_START=$SECONDS
    : > "$EVID/$label.log"
    stream "$EVID/$label.log" "$seconds" "$KILL_GRACE_SECONDS" "$@" || status=$?
    ACTIVE=''
    printf '%s seconds=%s status=%s\n' "$label" "$((SECONDS - ACTIVE_START))" "$status" \
        >> "$EVID/durations.txt"
    return "$status"
}

# The same, with the grace forwarded: EGRESS runs a watchdog of its own, so its
# own grace has to outlast that nested watchdog's finalizer, not this runner's
# one-second window. `egress` is the only caller.
timed_graceful() { # LABEL SECONDS GRACE COMMAND...
    local label=$1 seconds=$2 grace=$3 status=0 remaining
    shift 3
    remaining=$((PHASE_DEADLINE - SECONDS))
    [ "$seconds" -le "$remaining" ] || seconds=$remaining
    if [ "$seconds" -le 0 ]; then
        printf '%s seconds=0 status=skipped-deadline\n' "$label" >> "$EVID/durations.txt"
        return 1
    fi
    record_command "$label" "$@"
    ACTIVE=$label
    ACTIVE_START=$SECONDS
    : > "$EVID/$label.log"
    STREAM_WATCHDOG=$EGRESS_WATCHDOG stream "$EVID/$label.log" "$seconds" "$grace" "$@" ||
        status=$?
    ACTIVE=''
    printf '%s seconds=%s status=%s\n' "$label" "$((SECONDS - ACTIVE_START))" "$status" \
        >> "$EVID/durations.txt"
    return "$status"
}

# A removal that fails is only excused when the tag is confirmed absent; a daemon
# or permission error keeps failing, and the original phase failure is never
# turned into a pass. A lost transcript is not an absence, so no excuse covers it.
cleanup_remove() { # TAG
    local tag=$1 status=0 remaining
    remaining=$((CLEANUP_DEADLINE - SECONDS - CLEANUP_RESERVE_SECONDS))
    if [ "$remaining" -lt "$((CLEANUP_COMMAND_SECONDS + CLEANUP_ESCALATION_SECONDS))" ]; then
        printf 'cleanup-remove seconds=0 status=skipped-deadline\n' >> "$EVID/durations.txt"
        return 1
    fi
    ACTIVE=cleanup-remove
    ACTIVE_START=$SECONDS
    record_command cleanup-remove "$DOCKER" image rm "$tag"
    : > "$EVID/cleanup-remove.log"
    stream "$EVID/cleanup-remove.log" "$CLEANUP_COMMAND_SECONDS" "$CLEANUP_ESCALATION_SECONDS" \
        "$DOCKER" image rm "$tag" || status=$?
    ACTIVE=''
    printf 'cleanup-remove seconds=%s status=%s\n' "$((SECONDS - ACTIVE_START))" "$status" \
        >> "$EVID/durations.txt"
    if [ "$TEE_LOST" -eq 1 ]; then
        record cleanup remove-log-lost
        return 1
    fi
    if [ "$status" -eq 0 ]; then
        record cleanup removed
        return 0
    fi
    record_command cleanup-inspect "$DOCKER" image inspect "$tag"
    status=0
    : > "$EVID/cleanup-inspect.log"
    stream "$EVID/cleanup-inspect.log" "$CLEANUP_COMMAND_SECONDS" "$CLEANUP_ESCALATION_SECONDS" \
        "$DOCKER" image inspect "$tag" || status=$?
    if [ "$TEE_LOST" -eq 1 ]; then
        record cleanup inspect-log-lost
        return 1
    fi
    if [ "$status" -ne 0 ] && grep -qi 'no such image' "$EVID/cleanup-inspect.log"; then
        record cleanup absent
        return 0
    fi
    return 1
}

cleanup() {
    local status=$? tag cleanup_status=0
    CLEANUP_DEADLINE=$((SECONDS + CLEANUP_SECONDS))
    trap - EXIT
    # Bash ignores further signals, keeping the record and the removal going.
    trap '' INT TERM HUP
    if [ "$status" -ne 0 ]; then
        grep -q "^$CURRENT " "$EVID/verdicts.txt" || echo "$CURRENT FAIL" >> "$EVID/verdicts.txt"
    fi
    for phase in $PHASES; do
        grep -q "^$phase " "$EVID/verdicts.txt" || echo "$phase NOT-RUN" >> "$EVID/verdicts.txt"
    done
    # A signal preempts `timed` before it can record: the command is still ACTIVE.
    if [ -n "$ACTIVE" ]; then
        printf '%s seconds=%s status=interrupted\n' "$ACTIVE" "$((SECONDS - ACTIVE_START))" \
            >> "$EVID/durations.txt"
        ACTIVE=''
    fi
    if [ -s "$EVID/owned-tag.txt" ]; then
        tag=$(cat "$EVID/owned-tag.txt")
        cleanup_remove "$tag" || cleanup_status=1
    fi
    printf 'completed status=%s\n' "$cleanup_status" > "$EVID/cleanup.txt"
    if [ "$cleanup_status" -ne 0 ]; then
        echo 'layer-build: owned-tag cleanup failed' >&2
        [ "$status" -ne 0 ] || status=1
    fi
    if [ "$status" -eq 0 ]; then
        echo "layer-build: PASS layer=$LAYER platform=$PLATFORM parent=$PARENT evidence=$EVID"
    fi
    exit "$status"
}
# A reused --evidence-dir must not present a previous run's verdicts, owned tag
# or phase logs as this run's: a fresh `INSPECT NOT-RUN` beside a stale
# `image-inspect.log` would claim an inspection that did not happen. Only files
# this runner alone creates and reads back are removed. The traps go in first and
# the clearing immediately after them, so every failure from the first removed
# file on records itself instead of leaving the earlier run's PASS in place.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
: > "$EVID/verdicts.txt"
: > "$EVID/durations.txt"
: > "$EVID/evidence.txt"
rm -f "$EVID/cleanup.txt" "$EVID/owned-tag.txt" "$EVID"/*.log
# The egress smoke owns its subdirectory and is its only writer. Restoring it
# only when EGRESS happens to run leaves an earlier run's `strict=PASS` beside
# this run's `EGRESS NOT-RUN` after any earlier failure, which is two run
# identities in one uploaded tree. The runner names what it owns, so this file
# keeps no copy of the list that could drift; an unrelated file in the same
# directory belongs to nobody here and stays.
egress_owned=$("$EGRESS_RUNNER" --print-owned-files) ||
    fail 'cannot list the egress evidence files'
for name in $egress_owned; do
    rm -f "$EVID/egress/$name" || fail "cannot clear $EVID/egress/$name"
done
if [ "$LAYER_BUILD_EGRESS" = yes ]; then
    mkdir -p "$EVID/egress" || fail "cannot create $EVID/egress"
fi

# --- arguments and context: a refusal is recorded evidence, not silence -------
CURRENT=PREFLIGHT
case "$PLATFORM" in
    linux/amd64) ARCH=amd64 ;;
    linux/arm64) ARCH=arm64 ;;
    *) preflight_refuse '--platform must be linux/amd64 or linux/arm64' 2 ;;
esac
case "$LAYER" in
    '' | -*) preflight_refuse '--layer must name an example context' 2 ;;
    *[!a-z0-9-]*) preflight_refuse '--layer must be lowercase alphanumeric or hyphen' 2 ;;
esac
CONTEXT="examples/layers/$LAYER"
if [ -L "$CONTEXT" ] || [ ! -d "$CONTEXT" ]; then
    preflight_refuse "$CONTEXT is not a real example context" 1
fi
if [ -L "$CONTEXT/Dockerfile" ] || [ ! -f "$CONTEXT/Dockerfile" ]; then
    preflight_refuse "$CONTEXT/Dockerfile is not a regular file" 1
fi
# Recorded here, right after context validation, so a later failure records
# "EGRESS NOT-RUN" instead of saying nothing about the phase.
case "$LAYER_BUILD_EGRESS" in
    yes) PHASES="$PHASES EGRESS"; record egress_fixture yes ;;
    symlink)
        preflight_refuse "script/test/fixtures/network-requests/$LAYER is a symlink, not a real fixture directory" 1 ;;
    *) record egress_fixture none ;;
esac

# --- PREFLIGHT: metadata, native platform, guarded builder, committed parent --
phase_begin "$PREFLIGHT_SECONDS"
command -v python3 >/dev/null 2>&1 || fail 'python3 required'
# Each fact is recorded only by the command that establishes it, and its
# `timed` status is that fact's mark: a failed `rev-parse` leaves the commit
# unknown without casting doubt on the tree, and a failed `git status` is not a
# clean tree. No combined git label can contradict the two.
if timed source-sha "$DISCOVERY_SECONDS" git rev-parse HEAD; then
    SOURCE_SHA=$(head -n 1 "$EVID/source-sha.log")
else
    SOURCE_SHA=unknown
fi
if timed worktree-status "$DISCOVERY_SECONDS" git status --porcelain; then
    if [ -s "$EVID/worktree-status.log" ]; then DIRTY=yes; else DIRTY=no; fi
else
    DIRTY=unknown
fi
record source_sha "$SOURCE_SHA"
record dirty "$DIRTY"
record layer "$LAYER"
record arch "$ARCH"
record platform "$PLATFORM"
record context "$CONTEXT"
record run_id "$RUN_ID"
record owned_tag "$TAG"
record supervisor "$supervisor_seconds/$supervisor_grace"
NATIVE_PLATFORM=$(native_platform)
record host_arch "$(uname -m)"
[ "$NATIVE_PLATFORM" = "$PLATFORM" ] ||
    fail "the host is $(uname -m), not $PLATFORM; a native host is required, QEMU is not acceptance"
timed daemon "$DISCOVERY_SECONDS" "$DOCKER" version -f '{{.Server.Os}}/{{.Server.Arch}}'
DAEMON_PLATFORM=$(head -n 1 "$EVID/daemon.log")
record daemon_platform "$DAEMON_PLATFORM"
[ "$DAEMON_PLATFORM" = "$PLATFORM" ] ||
    fail "native daemon required: requested $PLATFORM, daemon ${DAEMON_PLATFORM:-unknown}; emulation refused"
timed client-version "$DISCOVERY_SECONDS" "$DOCKER" --version
record docker_client "$(head -n 1 "$EVID/client-version.log")"
timed context "$DISCOVERY_SECONDS" "$DOCKER" context show
BUILDER=$(head -n 1 "$EVID/context.log")
[ -n "$BUILDER" ] || fail 'no docker builder context'
record builder "$BUILDER"
timed builder-guard "$BUILDER_GUARD_SECONDS" bash "$BUILDER_GUARD" ||
    fail "the '$BUILDER' builder is not the daemon-backed docker driver"
timed builder "$DISCOVERY_SECONDS" "$DOCKER" buildx inspect "$BUILDER"
record buildkit_version "$(awk '/BuildKit version:/ { print $NF }' "$EVID/builder.log")"
parent_count=0
while IFS= read -r line; do
    PARENT=$line
    parent_count=$((parent_count + 1))
done < <(awk '/^ARG BASE_IMAGE=/ { sub(/^ARG BASE_IMAGE=/, ""); print }' "$CONTEXT/Dockerfile")
[ "$parent_count" -eq 1 ] ||
    fail "$CONTEXT/Dockerfile must have exactly one literal ARG BASE_IMAGE, found $parent_count"
case "$PARENT" in
    "$PARENT_REPOSITORY@sha256:"*) ;;
    *) fail "parent must be a released $PARENT_REPOSITORY digest, got ${PARENT:-nothing}" ;;
esac
PARENT_DIGEST=${PARENT#*@sha256:}
case "$PARENT_DIGEST" in
    '' | *[!0-9a-f]*) fail "parent digest is not 64 lowercase hex: $PARENT" ;;
esac
[ "${#PARENT_DIGEST}" -eq 64 ] || fail "parent digest is not 64 lowercase hex: $PARENT"
record parent_ref "$PARENT"
pass

# --- PULL: the committed parent, freshly, for this platform -------------------
phase_begin "$PULL_PHASE_SECONDS"
CURRENT=PULL
timed pull "$PULL_SECONDS" "$DOCKER" pull --platform "$PLATFORM" "$PARENT"
timed parent-inspect "$PARENT_INSPECT_SECONDS" "$DOCKER" image inspect "$PARENT"
PARENT_ID=$(jq -er '.[0].Id' "$EVID/parent-inspect.log") || fail 'parent inspection has no image ID'
PARENT_PLATFORM=$(jq -er '.[0] | .Os + "/" + .Architecture' "$EVID/parent-inspect.log") ||
    fail 'parent inspection has no platform'
case "$PARENT_ID" in
    sha256:*) case "${PARENT_ID#sha256:}" in *[!0-9a-f]* | '') fail "bad parent ID: $PARENT_ID" ;; esac ;;
    *) fail "bad parent ID: $PARENT_ID" ;;
esac
[ "$PARENT_PLATFORM" = "$PLATFORM" ] ||
    fail "parent platform mismatch: requested $PLATFORM, inspected $PARENT_PLATFORM"
record parent_id "$PARENT_ID"
record parent_platform "$PARENT_PLATFORM"
pass

# --- BUILD: the exact context, cold, with the committed parent ----------------
phase_begin "$BUILD_PHASE_SECONDS"
CURRENT=BUILD
printf '%s\n' "$TAG" > "$EVID/owned-tag.txt"
timed build "$BUILD_SECONDS" "$DOCKER" build --builder "$BUILDER" --platform "$PLATFORM" \
    --progress=plain --no-cache --build-arg "BASE_IMAGE=$PARENT" -t "$TAG" "$CONTEXT"
pass

# --- INSPECT: the built image is what was asked for ---------------------------
phase_begin "$INSPECT_PHASE_SECONDS"
CURRENT=INSPECT
timed image-inspect "$DISCOVERY_SECONDS" "$DOCKER" image inspect "$TAG"
IMAGE_ID=$(jq -er '.[0].Id' "$EVID/image-inspect.log") || fail 'built image inspection has no ID'
IMAGE_PLATFORM=$(jq -er '.[0] | .Os + "/" + .Architecture' "$EVID/image-inspect.log") ||
    fail 'built image inspection has no platform'
IMAGE_BYTES=$(jq -er '.[0].Size' "$EVID/image-inspect.log") || fail 'built image inspection has no size'
case "$IMAGE_ID" in
    sha256:*) case "${IMAGE_ID#sha256:}" in *[!0-9a-f]* | '') fail "bad image ID: $IMAGE_ID" ;; esac ;;
    *) fail "bad image ID: $IMAGE_ID" ;;
esac
[ "$IMAGE_PLATFORM" = "$PLATFORM" ] ||
    fail "built image platform mismatch: requested $PLATFORM, inspected $IMAGE_PLATFORM"
case "$IMAGE_BYTES" in '' | *[!0-9]*) fail "bad image size: $IMAGE_BYTES" ;; esac
record image_id "$IMAGE_ID"
record image_platform "$IMAGE_PLATFORM"
record image_bytes "$IMAGE_BYTES"
# Request labels (ADR 0014): the built layer carries its own label plus every
# request label of its parent, unchanged and still valid. This is the only place
# the built layer exists -- cleanup removes the owned tag.
timed network-requests "$DISCOVERY_SECONDS" \
    python3 "$ROOT/script/test/network_requests.py" check-image \
    --inspect "$EVID/image-inspect.log" --dockerfile "$CONTEXT/Dockerfile" \
    --inherits "$EVID/parent-inspect.log" ||
    fail 'built image network-request labels do not match the layer and its parent (ADR 0014)'
record network_requests checked
pass

# --- EGRESS: the declared hosts are the only ones the tool reaches ------------
# The built layer exists only in this run (cleanup removes the owned tag), so the
# smoke runs here, and only for a layer with a fixture. The runner writes its own
# evidence inside this run's evidence directory, which the workflow already
# uploads.
if [ "$LAYER_BUILD_EGRESS" = yes ]; then
    phase_begin "$EGRESS_PHASE_SECONDS"
    CURRENT=EGRESS
    timed_graceful egress "$EGRESS_SECONDS" "$EGRESS_KILL_GRACE_SECONDS" \
        bash "$EGRESS_RUNNER" --image "$TAG" --layer "$LAYER" --platform "$PLATFORM" \
        --evidence-dir "$EVID/egress"
    pass
fi

for phase in $PHASES; do
    grep -qx "$phase PASS" "$EVID/verdicts.txt" || fail "missing passing phase $phase"
done

