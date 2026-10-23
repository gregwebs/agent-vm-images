#!/bin/sh
# Equal selections validate locally; changed selections use a separate project.
# --refresh-lock is developer-only: it is the only path that re-pins the
# reviewed pnpm inputs, so a build can never re-pin what it verifies.
set -eu
if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
    echo 'usage: prepare-lock.sh PREFIX PLUGIN_VERSION [--refresh-lock]' >&2
    exit 2
fi
PREFIX=$1
version=$2
refresh=${3:-}
case "$refresh" in ''|--refresh-lock) ;; *) exit 2 ;; esac
CONTRACT=${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}
CHECK=${AGENT_VM_OPENCODE_LOCK_CHECK:-$(dirname "$0")/check-lock.mjs}
PIN=${AGENT_VM_OPENCODE_LOCK_PIN:-$(dirname "$0")/pin-pnpm-inputs.mjs}
if [ -n "$version" ]; then
    [ "$(printf '%s' "$version" | wc -l | tr -d ' ')" -eq 0 ] &&
        printf '%s' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' || {
        echo 'prepare-lock: selection must be canonical exact semver' >&2
        exit 1
    }
fi
[ -d "$PREFIX" ] || exit 1
prefix_phys=$(cd "$PREFIX" && pwd -P)
if [ "$prefix_phys" != /opt/agent-vm/opencode-plugins ]; then
    root=${AGENT_VM_FIXTURE_ROOT:?non-production prefix requires fixture root}
    root_phys=$(cd "$root" && pwd -P)
    case "$prefix_phys" in "$root_phys"/*) ;; *) exit 1 ;; esac
fi
PREFIX=$prefix_phys
# Reject preload injection before any Node process (pnpm, check-lock.mjs) starts.
[ -z "${NODE_OPTIONS:-}${NODE_PATH:-}${BUN_OPTIONS:-}" ] || { echo 'manager runtime overrides are unsupported' >&2; exit 1; }
"$CONTRACT/recipe-contract" pnpm-environment "$PREFIX"
committed=$(node "$CHECK" "$PREFIX")
if [ -z "$version" ] || [ "$version" = "$committed" ]; then
    echo "prepare-lock: $committed unchanged; committed inputs reused"
    exit 0
fi
# A changed selection rewrites the reviewed bytes, so it is refused unless the
# caller asks for a deliberate re-pin.
if [ "$refresh" != --refresh-lock ]; then
    echo "prepare-lock: committed inputs pin $committed, not $version; re-pin deliberately with upgrade-plugins.sh" >&2
    exit 1
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
mkdir -p "$work/project" "$work/home" "$work/config" "$work/cache" "$work/data" "$work/state"
export HOME="$work/home" XDG_CONFIG_HOME="$work/config" XDG_CACHE_HOME="$work/cache" XDG_DATA_HOME="$work/data" XDG_STATE_HOME="$work/state"
[ "$(timeout --kill-after=5 20 pnpm --version --pm-on-fail=error --config.update-notifier=false)" = 11.11.0 ] || {
    echo 'prepare-lock: pnpm 11.11.0 is required; refusing another manager version' >&2
    exit 1
}
cp "$PREFIX/pnpm-workspace.yaml" "$work/project/"
jq --arg version "$version" '.dependencies["@openchamber/opencode-claude"] = $version' "$PREFIX/package.json" >"$work/project/package.json"
if [ "$refresh" = --refresh-lock ]; then
    cp "$PREFIX/pnpm-lock.yaml" "$work/project/"
fi
(cd "$work/project" && sh "$CONTRACT/run-pnpm.sh" "$work/prepare.ndjson" -- \
    pnpm install --lockfile-only --no-frozen-lockfile \
        --config.update-notifier=false \
        --ignore-scripts --ignore-pnpmfile --pm-on-fail=error \
        --registry=https://registry.npmjs.org/ --store-dir "$work/store")
node "$PIN" "$work/project"
node "$CHECK" "$work/project" >/dev/null
# The caller stages a disposable project; developer publication is transactional
# across the manifest, lock, workspace, pins and central Dockerfile.
cp "$work/project/package.json" "$PREFIX/package.json"
cp "$work/project/pnpm-lock.yaml" "$PREFIX/pnpm-lock.yaml"
cp "$work/project/pnpm-inputs.sha256" "$PREFIX/pnpm-inputs.sha256"
