#!/usr/bin/env bash
# Runtime evidence that the built image holds the pinned Node and GitHub CLI (#96).
#
# The pins, the installer and the labels prove what a build *selected*; only the
# running image proves what it has. This suite reads the two pins, then asks the
# image itself: the node major and exact version, the npm and npx that ship in
# that same archive, the pinned-interpreter path /usr/bin/node, gh, and that the
# inherited pnpm launcher still resolves through /usr/bin/node.
#
# usage: bash script/test/node-toolchain.sh IMAGE --platform linux/ARCH
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
usage() {
    echo 'usage: bash script/test/node-toolchain.sh IMAGE --platform linux/ARCH' >&2
    exit 2
}
[ "$#" -eq 3 ] && [ "$2" = --platform ] || usage
IMAGE=$1
platform=$3
case "$platform" in
    linux/amd64 | linux/arm64) ;;
    *) usage ;;
esac

fail() {
    printf 'node-toolchain: %s\n' "$*" >&2
    exit 1
}
command -v docker >/dev/null || fail 'docker is required'
docker image inspect "$IMAGE" >/dev/null 2>&1 || fail "no such image: $IMAGE"
image_platform=$(docker image inspect "$IMAGE" --format '{{.Os}}/{{.Architecture}}')
[ "$image_platform" = "$platform" ] || fail "$IMAGE is $image_platform, not $platform"

# The pins are the expectation; the image is the claim being tested. `npm` is
# compared against the version recorded for the reviewed Node archive, not against
# whatever npm the image reports: an equal-but-wrong npm/npx pair passed before.
pin_version() {
    python3 "$ROOT/images/artifacts/read-pin-version.py" "$ROOT/images/artifacts/$1.json"
}
node_version=$(pin_version node) || fail 'cannot read the node pin'
gh_version=$(pin_version gh) || fail 'cannot read the gh pin'
bundled_npm=$(python3 "$ROOT/images/artifacts/read-pin-version.py" \
    "$ROOT/images/artifacts/node.json" --npm) || fail 'cannot read the bundled npm pin'
pnpm_version=$(python3 "$ROOT/images/pnpm/read-pin-version.py" "$ROOT/images/pnpm/pin.json")

# One container, one command, every claim: a second run could see a different
# image if the tag moved underneath the suite.
report=$(docker run --rm --network none "$IMAGE" sh -c '
    node --version
    npm --version
    npx --version
    /usr/bin/node --version
    readlink -f /usr/bin/node
    readlink -f /usr/local/bin/node
    command -v npm
    readlink -f "$(command -v npm)"
    command -v npx
    readlink -f "$(command -v npx)"
    gh --version | head -n 1
    pnpm --version
') || fail 'the image did not report its toolchain'

line() { printf '%s\n' "$report" | sed -n "$1p"; }
[ "$(line 1)" = "v$node_version" ] || fail "node --version is $(line 1), not v$node_version"
[ "$(line 4)" = "v$node_version" ] || fail "/usr/bin/node is $(line 4), not v$node_version"
[ "$(line 5)" = "$(line 6)" ] ||
    fail "/usr/bin/node resolves to $(line 5), not the artifact at $(line 6)"
case "$(line 11)" in
    "gh version $gh_version "*) ;;
    *) fail "gh --version is '$(line 11)', not version $gh_version" ;;
esac
[ "$(line 12)" = "$pnpm_version" ] ||
    fail "pnpm --version is $(line 12), not the pinned $pnpm_version"
# npm and npx are the ones bundled in the reviewed Node archive: the exact value
# is the pin's, and each command must resolve into that archive's npm tree.
[ "$(line 2)" = "$bundled_npm" ] ||
    fail "npm --version is $(line 2), not the pinned bundled $bundled_npm"
[ "$(line 3)" = "$bundled_npm" ] ||
    fail "npx --version is $(line 3), not the pinned bundled $bundled_npm"
for command_line in 8 10; do
    case "$(line "$command_line")" in
        /usr/local/lib/node_modules/npm/*) ;;
        *) fail "$(line $((command_line - 1))) resolves to $(line "$command_line"), not into \
/usr/local/lib/node_modules/npm" ;;
    esac
done

printf 'node-toolchain: node v%s, npm %s, npx %s, gh %s, pnpm %s on %s\n' \
    "$node_version" "$bundled_npm" "$(line 3)" "$gh_version" "$pnpm_version" "$platform"
