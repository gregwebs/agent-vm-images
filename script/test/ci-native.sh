#!/usr/bin/env bash
# Disposable hosted-runner evidence phases, not a user build interface.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
cd "$ROOT"
phase=${1:?phase required}
arch=${2:?native architecture required}
case "$arch:$(uname -m)" in amd64:x86_64 | arm64:aarch64) ;; *) echo 'runner is not requested native architecture' >&2; exit 1 ;; esac
platform="linux/$arch"
BUILDER=$(docker context show)
bash "$ROOT/script/test/require-daemon-builder.sh" || { echo 'daemon-backed builder required' >&2; exit 1; }
mkdir -p target/ci-logs
measure_capacity() {
    local root available
    root=$(docker info --format '{{.DockerRootDir}}')
    available=$(sudo df -Pk "$root" | awk 'NR==2 {print $4}')
    printf 'daemon_root=%s available_KiB=%s\n' "$root" "$available"
    [ "$available" -ge $((25*1024*1024)) ] || { echo 'below 25 GiB full-standard threshold' >&2; return 1; }
}
timed() {
    local label=$1 start status=0
    shift
    start=$SECONDS
    "$@" >"target/ci-logs/$label.log" 2>&1 || status=$?
    cat "target/ci-logs/$label.log"
    printf '%s seconds=%s status=%s\n' "$label" "$((SECONDS-start))" "$status" | tee -a target/ci-logs/durations.txt
    return "$status"
}
# The audit step's host watchdog in build-local.yml is the outer bound, so the
# audit must fail on its own measured wall-clock before it can consume it.
#
# PROVISIONAL: 4800s sits 6900 / 4800 = 1.44x under the 6900s watchdog, but no
# native audit has recorded per-leg durations yet, so this bound is arithmetic,
# not measurement. The local durations that exist describe a diagnostic stale
# base, not this leg set. The first green native audit must replace it with the
# sum in target/ci-logs/durations.txt plus that same 1.44x headroom. Never widen
# it for convenience: an overrun means splitting the dsh legs into a second audit
# step (with its own watchdog inside the same job, because images do not survive
# a job change) and re-checking the 360-minute job total: 20 prerequisites + 20
# capacity + 180 build + 120 audit + 10 upload = 350. No gate is ever dropped to
# fit.
AUDIT_BUDGET_SECONDS=4800
check_audit_budget() {
    local first_line=$1 measured
    measured=$(awk -v first="$first_line" 'NR > first {
        for (field = 1; field <= NF; field++) {
            if ($field ~ /^seconds=/) { sub(/^seconds=/, "", $field); total += $field }
        }
    } END { print total + 0 }' target/ci-logs/durations.txt)
    printf 'audit-budget measured_seconds=%s budget_seconds=%s watchdog_seconds=6900\n' \
        "$measured" "$AUDIT_BUDGET_SECONDS" | tee -a target/ci-logs/durations.txt
    [ "$measured" -le "$AUDIT_BUDGET_SECONDS" ] ||
        { echo "audit wall-clock ${measured}s exceeds its ${AUDIT_BUDGET_SECONDS}s budget" >&2; return 1; }
}
case "$phase" in
    prepare)
        uname -a; df -Pk; docker version; docker buildx version
        docker info; docker system df; docker context show; docker buildx inspect "$BUILDER"
        if ! measure_capacity; then echo 'pre-cleanup capacity is below threshold; reclaiming listed runner SDKs'; fi
        # Explicit runner-owned SDK paths only, never Docker data or toolcache.
        timed runner-sdk-cleanup timeout --kill-after=10s 600 sudo rm -rf \
            /usr/share/dotnet /usr/local/lib/android /opt/ghc /usr/local/share/boost
        measure_capacity
        ;;
    build)
        timed base docker build --builder "$BUILDER" --platform "$platform" -t agent-vm-base:local -f images/Dockerfile images
        measure_capacity
        timed standard docker build --builder "$BUILDER" --platform "$platform" -t agent-vm-standard:local --build-arg BASE_IMAGE=agent-vm-base:local -f images/standard/Dockerfile images
        timed example docker build --builder "$BUILDER" --platform "$platform" -t my-agent-vm:local examples/base-extension
        timed helper-base bash images/build.sh base
        timed helper-standard bash images/build.sh standard
        for image in agent-vm-base:local agent-vm-standard:local my-agent-vm:local; do
            docker image inspect "$image" --format '{{.Id}} {{.Os}}/{{.Architecture}} size={{.Size}} labels={{json .Config.Labels}}'
            [ "$(docker image inspect "$image" --format '{{.Os}}/{{.Architecture}}')" = "$platform" ]
        done
        ;;
    audit)
        touch target/ci-logs/durations.txt
        audit_durations_start=$(wc -l <target/ci-logs/durations.txt)
        timed node-primitives bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer primitives --platform "$platform"
        timed dsh-default bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer dsh --platform "$platform"
        timed dsh-changed bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer dsh --version 0.1.5-rc.1 --platform "$platform"
        timed dsh-policy bash script/test/standard-node-policy.sh agent-vm-base:local --consumer dsh --platform "$platform"
        # The ticket's real Cordis-removal negative: the same committed inputs with
        # the four reviewed overrides removed must be rejected (exact peers) or fail
        # at startup, and the run then installs the committed inputs and proves
        # readiness. One extra resolution, inside the measured audit budget.
        timed dsh-cordis-negative bash script/test/standard-node-consumers.sh agent-vm-base:local \
            --consumer dsh --cordis-negative --platform "$platform"
        timed pnpm-runtime bash script/test/pnpm-runtime.sh agent-vm-base:local agent-vm-standard:local --platform "$platform"
        timed standard-uid-t5 bash script/test/standard-image.sh agent-vm-base:local agent-vm-standard:local --platform "$platform"
        # Artifact mode is the only path that runs the three rendered guest helper
        # sources: the renderer's byte roundtrip proves the guest is sent them, not
        # that they execute, so this leg keeps the rendered probe covered on both
        # native architectures. One more bounded step (~85 s on a native
        # linux/arm64 daemon) inside the same provisional audit sum, so the bound
        # above stands unchanged.
        timed artifact bash script/test/standard-image.sh --artifact agent-vm-standard:local --platform "$platform"
        timed opencode-runtime bash script/test/opencode-runtime.sh agent-vm-base:local agent-vm-standard:local
        timed pi-runtime bash script/test/pi-runtime.sh agent-vm-base:local agent-vm-standard:local
        timed pi-audit-status bash script/test/standard-image.sh --pi-audit-status agent-vm-base:local --platform "$platform"
        timed example-uid docker run --rm --user 12345:23456 --cap-drop ALL --network none -e HOME=/tmp my-agent-vm:local
        printf 'hello from my base extension\nLinux\n12345\n' >target/ci-logs/example-expected
        cmp target/ci-logs/example-expected target/ci-logs/example-uid.log
        prior=$(docker image inspect agent-vm-standard:local --format '{{.Id}}')
        # The stale tag stays inspectable, so record where it came from: this ID is
        # the artifact that existed before the failing build, not its result.
        printf 'stale-tag control: prior agent-vm-standard id %s = prior artifact, not a result of the failing build\n' \
            "$prior" | tee -a target/ci-logs/durations.txt
        if timed bad-version docker build --builder "$BUILDER" --platform "$platform" -t agent-vm-standard:local --build-arg AGENT_VERSION_DSH=latest -f images/standard/Dockerfile images; then
            echo 'invalid exact selection succeeded' >&2; exit 1
        fi
        [ "$(docker image inspect agent-vm-standard:local --format '{{.Id}}')" = "$prior" ]
        # Repository base identity (#95): the base stamps this checkout's manifest,
        # a stale or stamp-less local tag fails both consumers, and the stamp does
        # not depend on untracked context files or on the helper delivery. Native
        # only, because the whole point is the images this host just built.
        timed parent-identity bash script/test/parent-identity.sh agent-vm-base:local --platform "$platform"
        timed final-controls bash script/test/standard-certification.sh
        check_audit_budget "$audit_durations_start"
        ;;
    network-build)
        timed network-base docker build --builder "$BUILDER" --platform "$platform" -t agent-vm-base:local -f images/Dockerfile images
        ;;
    network-audit)
        args=()
        if [ "${NETWORK_OVERRIDES:-false}" = true ]; then args+=(--overrides); fi
        timed strict-egress bash script/test/shipped-installer-network.sh agent-vm-base:local --platform "$platform" ${args[@]+"${args[@]}"}
        ;;
    *) echo "unknown phase $phase" >&2; exit 2 ;;
esac
printf '%s %s PASS\n' "$phase" "$platform" | tee -a target/ci-logs/verdicts.txt
