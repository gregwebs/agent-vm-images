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
docker buildx inspect "$BUILDER" | grep -Eq '^Driver:[[:space:]]+docker$' || { echo 'daemon-backed builder required' >&2; exit 1; }
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
        timed standard-uid-t5 bash script/test/standard-image.sh agent-vm-base:local agent-vm-standard:local --platform "$platform"
        timed pi-runtime bash script/test/pi-runtime.sh agent-vm-base:local agent-vm-standard:local
        timed pi-audit-status bash script/test/standard-image.sh --pi-audit-status agent-vm-base:local --platform "$platform"
        timed example-uid docker run --rm --user 12345:23456 --cap-drop ALL --network none -e HOME=/tmp my-agent-vm:local
        printf 'hello from my base extension\nLinux\n12345\n' >target/ci-logs/example-expected
        cmp target/ci-logs/example-expected target/ci-logs/example-uid.log
        prior=$(docker image inspect agent-vm-standard:local --format '{{.Id}}')
        if timed bad-version docker build --builder "$BUILDER" --platform "$platform" -t agent-vm-standard:local --build-arg AGENT_VERSION_DSH=latest -f images/standard/Dockerfile images; then
            echo 'invalid exact selection succeeded' >&2; exit 1
        fi
        [ "$(docker image inspect agent-vm-standard:local --format '{{.Id}}')" = "$prior" ]
        timed final-controls bash script/test/standard-certification.sh
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
