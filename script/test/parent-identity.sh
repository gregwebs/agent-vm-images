#!/usr/bin/env bash
# Native proof that a local base's source identity binds its consumers (#95).
#
# A manifest that only exists in the repository proves nothing: the interesting
# claims are that the built base stamps *this* checkout's identity, that a stale
# local tag fails both consumers before they install anything, and that the stamp
# does not depend on how the Go builder stage was delivered or on untracked files
# in the context. Everything runs against invocation-owned scratch copies and
# invocation-owned tags; no repository image is retagged, nothing is pruned and
# no agent is installed (every failure path stops at the first product RUN).
#
# usage: bash script/test/parent-identity.sh BASE --platform linux/ARCH
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
usage() {
    echo 'usage: bash script/test/parent-identity.sh BASE --platform linux/ARCH' >&2
    exit 2
}
[ "$#" -eq 3 ] && [ "$2" = --platform ] || usage
BASE=$1
platform=$3
case "$platform" in
    linux/amd64 | linux/arm64) ;;
    *) usage ;;
esac

log() { printf 'parent-identity: %s\n' "$*"; }
fail() {
    printf 'parent-identity: %s\n' "$*" >&2
    exit 1
}

command -v docker >/dev/null || fail 'docker is required'
timeout --version 2>/dev/null | grep -q 'GNU coreutils' || fail 'GNU timeout is required'
builder=$(docker context show)
bash "$ROOT/script/test/require-daemon-builder.sh" >/dev/null ||
    fail 'a daemon-backed builder is required'
base_id=$(docker image inspect "$BASE" --format '{{.Id}}' 2>/dev/null) ||
    fail "no such image: $BASE"
base_platform=$(docker image inspect "$BASE" --format '{{.Os}}/{{.Architecture}}')
[ "$base_platform" = "$platform" ] ||
    fail "$BASE is $base_platform, not $platform"

work=$(mktemp -d "${TMPDIR:-/tmp}/parent-identity.XXXXXX")
work=$(cd "$work" && pwd -P)
suffix=${work##*/}
owned_tags=()

cleanup() {
    local tag
    for tag in ${owned_tags[@]+"${owned_tags[@]}"}; do
        docker image rm "$tag" >/dev/null 2>&1 || true
    done
    rm -rf "$work"
}
trap cleanup EXIT

cp -R "$ROOT/images" "$work/images"
cp -R "$ROOT/examples/base-extension" "$work/extension"
mkdir "$work/empty"

fail_tag="agent-vm-parent-identity:fail-$suffix"
owned_tags+=("$fail_tag")
docker tag "$BASE" "$fail_tag"

expect_tag_unchanged() { # NAME
    local actual
    actual=$(docker image inspect "$fail_tag" --format '{{.Id}}')
    [ "$actual" = "$base_id" ] ||
        fail "$1: a failed build replaced $fail_tag with $actual"
}

# Every expected-failure build targets the invocation-owned tag, so a build that
# unexpectedly succeeds shows up as a changed tag as well as a zero status.
expect_build_failure() { # NAME NEEDLE -- DOCKER BUILD ARGS
    local name=$1 needle=$2
    shift 3
    local log=$work/failure.log
    if timeout 1800 docker build --builder "$builder" --platform "$platform" -t "$fail_tag" \
        "$@" >"$log" 2>&1; then
        fail "$name: the build succeeded"
    fi
    expect_tag_unchanged "$name"
    if ! grep -qF -- "$needle" "$log"; then
        cat "$log" >&2
        fail "$name: expected '$needle' in the build log"
    fi
    log "CHECK $name ok"
}

identity_of() { # MANIFEST
    local sum
    sum=$(sha256sum <"$1")
    printf 'sha256:%s\n' "${sum%% *}"
}

stamp_of() { # IMAGE
    docker run --rm --entrypoint cat "$1" /opt/agent-vm/base/source-identity
}

# --- 1. the built base stamps this checkout ---------------------------------

host_identity=$(identity_of "$ROOT/images/base-inputs.sha256")
base_stamp=$(stamp_of "$BASE") || fail "$BASE has no source identity stamp"
[ "$base_stamp" = "$host_identity" ] ||
    fail "$BASE stamps $base_stamp, but this checkout's inputs are $host_identity"
log "CHECK host manifest identity equals the base stamp ($host_identity) ok"

# --- 2. a wrong parent digest never resolves --------------------------------

zeros=$(printf '%064d' 0)
debian_reference=$(sed -n 's/^FROM \(debian:[^ ]*@sha256:[0-9a-f]*\)$/\1/p' \
    "$ROOT/images/Dockerfile" | head -n 1)
[ -n "$debian_reference" ] || fail 'the base recipe no longer pins Debian by digest'
wrong_debian=${debian_reference%%@sha256*}@sha256:$zeros
printf 'FROM %s\n' "$wrong_debian" >"$work/resolve.Dockerfile"
expect_build_failure 'wrong-parent-digest' "$wrong_debian" -- \
    -f "$work/resolve.Dockerfile" "$work/empty"

# --- 3. a base without the stamp fails both consumers -----------------------

printf 'FROM %s\nRUN rm -f /opt/agent-vm/base/source-identity\n' "$BASE" \
    >"$work/images/nostamp.Dockerfile"
nostamp="agent-vm-parent-identity:nostamp-$suffix"
owned_tags+=("$nostamp")
timeout 1800 docker build --builder "$builder" --platform "$platform" -t "$nostamp" \
    -f "$work/images/nostamp.Dockerfile" "$work/images" >"$work/nostamp.log" 2>&1 ||
    { cat "$work/nostamp.log" >&2; fail 'cannot build the stamp-less probe'; }

# The recipe's own Go stage is expensive and irrelevant to the identity, so the
# host toolchain supplies the named context; the delivery parity check below
# keeps that substitution honest.
host_context=$(bash "$ROOT/script/build/recipe-contract.sh" --platform "$platform") ||
    fail 'cannot build the host recipe-contract context'
expect_build_failure 'standard-rejects-missing-stamp' \
    'has no repository base identity' -- \
    --build-context "recipe-contract-bin=$host_context" \
    --build-arg "BASE_IMAGE=$nostamp" \
    -f "$work/images/standard/Dockerfile" "$work/images"
expect_build_failure 'extension-rejects-missing-stamp' \
    'has no repository base identity' -- \
    --build-arg "BASE_IMAGE=$nostamp" \
    -f "$work/extension/Dockerfile" "$work/extension"

# --- 4. an edited input with a regenerated identity is refused --------------

printf '\n# parent-identity: a listed input changed\n' >>"$work/images/install-pnpm.sh"
bash "$ROOT/script/build/base-inputs.sh" --stage "$work/images" "$work/render" \
    >"$work/stage.log" 2>&1 || { cat "$work/stage.log" >&2; fail '--stage failed'; }
cp "$work/render/base-inputs.sha256" "$work/images/base-inputs.sha256"
cp "$work/render/extension.Dockerfile" "$work/extension/Dockerfile"
edited_identity=$(identity_of "$work/render/base-inputs.sha256")
[ "$edited_identity" != "$host_identity" ] || fail 'the edited input did not move the identity'

expect_build_failure 'standard-rejects-edited-input' \
    "declares $base_stamp; this checkout expects $edited_identity" -- \
    --build-context "recipe-contract-bin=$host_context" \
    --build-arg "BASE_IMAGE=$BASE" \
    -f "$work/images/standard/Dockerfile" "$work/images"
expect_build_failure 'extension-rejects-edited-input' \
    "use --build-arg AGENT_VM_BASE_IDENTITY=$base_stamp" -- \
    --build-arg "BASE_IMAGE=$BASE" \
    -f "$work/extension/Dockerfile" "$work/extension"

# --- 5. a probe proves the regenerated identity is the one that matches -----

builder_block=$(awk '
    /^FROM --platform=\$BUILDPLATFORM golang:/ { inside = 1 }
    inside && !/^FROM --platform=\$BUILDPLATFORM golang:/ && /^FROM / { exit }
    inside { print }
' "$ROOT/images/Dockerfile")
stamp_block=$(awk '
    /^RUN --mount=type=bind,source=base-inputs\.sha256/ { inside = 1 }
    inside { print }
    inside && !/\\$/ { exit }
' "$ROOT/images/Dockerfile")
[ -n "$builder_block" ] || fail 'cannot read the recipe builder stage'
[ -n "$stamp_block" ] || fail 'cannot read the recipe identity stamp run'
{
    echo '# Generated by script/test/parent-identity.sh from images/Dockerfile.'
    printf '%s\n' "$builder_block"
    printf 'FROM %s\n' "$BASE"
    printf 'RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro \\\n'
    printf '    test -x /tmp/recipe-contract/recipe-contract\n'
    printf '%s\n' "$stamp_block"
} >"$work/images/identity-probe.Dockerfile"
probe="agent-vm-parent-identity:probe-$suffix"
owned_tags+=("$probe")
build_probe() { # DELIVERY
    local delivery=$1 log=$work/probe-$1.log
    shift
    if ! timeout 1800 docker build --builder "$builder" --platform "$platform" -t "$probe" \
        "$@" -f "$work/images/identity-probe.Dockerfile" "$work/images" >"$log" 2>&1; then
        cat "$log" >&2
        fail "the identity probe failed with the $delivery helper delivery"
    fi
}

build_probe recipe-stage
probe_stamp=$(stamp_of "$probe")
[ "$probe_stamp" = "$edited_identity" ] ||
    fail "the probe stamped $probe_stamp, not the regenerated $edited_identity"
log 'CHECK probe stamps the edited identity from the regenerated manifest ok'

# --- 6. BuildKit parity: untracked extras and modes are not identity --------

printf '{}\n' >"$work/images/pnpm/untracked-extra.json"
printf 'noise\n' >"$work/images/pnpm/untracked.log"
chmod +x "$work/images/install-zellij.sh"
build_probe recipe-stage-untracked
parity_stamp=$(stamp_of "$probe")
[ "$parity_stamp" = "$edited_identity" ] ||
    fail "untracked extras or a mode flip changed the identity to $parity_stamp"
log 'CHECK untracked extras and physical mode flips keep the identity ok'

# --- 7. helper delivery parity ---------------------------------------------

build_probe recipe-named-context --build-context "recipe-contract-bin=$host_context"
named_stamp=$(stamp_of "$probe")
[ "$named_stamp" = "$parity_stamp" ] ||
    fail "the named helper context changed the identity: $named_stamp vs $parity_stamp"
log 'CHECK the host-helper context delivers the same identity ok'

# --- 8. a content change without regeneration is refused --------------------

printf '\n# parent-identity: unregenerated content change\n' >>"$work/images/install-zellij.sh"
expect_build_failure 'probe-rejects-unregenerated-edit' \
    'base-inputs: inputs/default drift; run: bash script/build/base-inputs.sh --write' -- \
    -f "$work/images/identity-probe.Dockerfile" "$work/images"

log "BASE $BASE id $base_id"
log "stamp $base_stamp"
log "regenerated $edited_identity"
log "probe (recipe stage) $parity_stamp"
log "probe (named context) $named_stamp"
log 'all checks passed'
