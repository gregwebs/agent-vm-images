#!/usr/bin/env bash
# Stage owning inputs and publish them together; retain incomplete rollback material.
set -euo pipefail

PACKAGE=@openchamber/opencode-claude

usage() {
    cat <<EOF
Usage: bash images/tools/opencode/plugins/upgrade-plugins.sh [VERSION]

Pin ${PACKAGE} to VERSION, an exact version (default: the
registry's \`latest\` dist-tag), and update package.json, pnpm-lock.yaml and the
reviewed pnpm input pins beside this script.
EOF
}

case "${1:-}" in
    -h | --help)
        usage
        exit 0
        ;;
esac
if [ $# -gt 1 ]; then
    usage >&2
    exit 2
fi

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$DIR/../../../.." && pwd)"
# require_tools comes from npm-pin.sh; resolve_npm_version is deliberately unused
# here because this plugin path resolves versions with `pnpm view`, never npm.
. "$REPO_ROOT/script/build/npm-pin.sh"
. "$REPO_ROOT/script/build/dockerfile-label.sh"
. "$REPO_ROOT/script/build/transactional-publish.sh"

require_tools jq pnpm node
[ "${GITHUB_ACTIONS:-}" != true ] || { echo 'refusing GitHub Actions' >&2; exit 1; }

current=$(jq -r --arg p "$PACKAGE" '.dependencies[$p]' "$DIR/package.json")

work=$(mktemp -d)
keep_recovery=false
trap 'if [ "$keep_recovery" = false ]; then rm -rf "$work"; fi' EXIT
CONTRACT="$REPO_ROOT/images/recipe-contract"
# Reject preload injection before starting the Node policy checker itself.
[ -z "${NODE_OPTIONS:-}${NODE_PATH:-}${BUN_OPTIONS:-}" ] || { echo 'manager runtime overrides are unsupported' >&2; exit 1; }
node "$CONTRACT/check-pnpm-environment.mjs" "$DIR"
mkdir -p "$work/home" "$work/config" "$work/cache" "$work/data" "$work/state"
export HOME="$work/home" XDG_CONFIG_HOME="$work/config" XDG_CACHE_HOME="$work/cache" XDG_DATA_HOME="$work/data" XDG_STATE_HOME="$work/state"
[ "$(timeout --kill-after=5 20 pnpm --version --pm-on-fail=error --config.update-notifier=false)" = 11.11.0 ] || { echo 'pnpm 11.11.0 required' >&2; exit 1; }
version_arg="${1:-}"
version=$version_arg
if [ -z "$version" ]; then
    # Deliberately separate from the install reporter: view returns a scalar,
    # not the runner's NDJSON stream. Private state and a 60s bound still apply.
    # No RLIMIT_FSIZE: a packument is a file pnpm legitimately writes and may
    # exceed any small limit, which would abort a correct query with SIGXFSZ.
    (timeout --kill-after=5 60 pnpm view "$PACKAGE" dist-tags.latest \
        --config.update-notifier=false \
        --json --registry=https://registry.npmjs.org/) >"$work/latest.json"
    version=$(jq -er 'select(type == "string")' "$work/latest.json")
fi
echo "==> pinning ${PACKAGE}: ${current} -> ${version}"

mkdir -p "$work/plugins"
cp "$DIR/package.json" "$work/plugins/package.json"
cp "$DIR/pnpm-lock.yaml" "$work/plugins/pnpm-lock.yaml"
cp "$DIR/pnpm-workspace.yaml" "$work/plugins/pnpm-workspace.yaml"
cp "$DIR/pnpm-inputs.sha256" "$work/plugins/pnpm-inputs.sha256"
cp "$REPO_ROOT/images/standard/Dockerfile" "$work/Dockerfile"

echo "==> updating pnpm-lock.yaml"
AGENT_VM_CONTRACT_DIR="$REPO_ROOT/images/recipe-contract" AGENT_VM_FIXTURE_ROOT="$work" sh "$DIR/prepare-lock.sh" "$work/plugins" "$version" --refresh-lock

staged=$(jq -r --arg p "$PACKAGE" '.dependencies[$p]' "$work/plugins/package.json")
[ "$staged" = "$version" ] || {
    echo "error: staged manifest pins ${staged}, expected ${version}" >&2
    exit 1
}

set_version_label "$work/Dockerfile" opencode-claude AGENT_VERSION_OPENCODE_CLAUDE "$version"
grep -Fq "org.agent-vm.version.opencode-claude=\"\${AGENT_VERSION_OPENCODE_CLAUDE:-${version}}\"" "$work/Dockerfile" || {
    echo "error: the staged Dockerfile plugins label does not mirror ${version}" >&2
    exit 1
}

if publish_transactional "$work/publish-backup" \
    "$work/plugins/package.json" "$DIR/package.json" \
    "$work/plugins/pnpm-lock.yaml" "$DIR/pnpm-lock.yaml" \
    "$work/plugins/pnpm-workspace.yaml" "$DIR/pnpm-workspace.yaml" \
    "$work/plugins/pnpm-inputs.sha256" "$DIR/pnpm-inputs.sha256" \
    "$work/Dockerfile" "$REPO_ROOT/images/standard/Dockerfile"; then
    status=0
else
    status=$?
fi
if [ "$status" -ne 0 ]; then
    if [ "$status" -eq 3 ]; then
        keep_recovery=true
        echo "error: recovery material retained at $work" >&2
    fi
    exit "$status"
fi

echo "Pinned ${PACKAGE}@${version}; review manifest, lock, workspace, pins and Dockerfile."
