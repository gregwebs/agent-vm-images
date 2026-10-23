#!/bin/sh
# This new pnpm consumer is hard-failure-only. Certification owns installed.
set -eu
PREFIX=${AGENT_VM_OPENCODE_PLUGINS_PREFIX:-/opt/agent-vm/opencode-plugins}
CONTRACT=${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}
SOURCE=${AGENT_VM_OPENCODE_SOURCE_DIR:-/tmp/opencode-source}
[ -d "$PREFIX" ] || exit 1
PREFIX=$(cd "$PREFIX" && pwd -P)
pnpm=/usr/local/bin/pnpm
if [ "$PREFIX" != /opt/agent-vm/opencode-plugins ]; then
    root=${AGENT_VM_FIXTURE_ROOT:?non-production prefix requires fixture root}
    root=$(cd "$root" && pwd -P)
    case "$PREFIX" in "$root"/*) ;; *) exit 1 ;; esac
    pnpm=${AGENT_VM_FIXTURE_PNPM:-$pnpm}
fi
work=$(mktemp -d)
success=false
cleanup() {
    if [ "$success" != true ]; then rm -rf "$PREFIX/node_modules"; fi
    rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
# Reject preload injection before any Node process (pnpm, check-lock.mjs) starts.
[ -z "${NODE_OPTIONS:-}${NODE_PATH:-}${BUN_OPTIONS:-}" ] || { echo 'manager runtime overrides are unsupported' >&2; exit 1; }
# The build hook never honors the test seams either: only prepare-lock.sh's
# developer path may re-pin, and no image build can reach it.
unset AGENT_VM_OPENCODE_LOCK_CHECK AGENT_VM_OPENCODE_LOCK_PIN
"$CONTRACT/recipe-contract" pnpm-environment "$PREFIX"
mkdir -p "$work/home" "$work/config" "$work/cache" "$work/data" "$work/state"
export HOME="$work/home" XDG_CONFIG_HOME="$work/config" XDG_CACHE_HOME="$work/cache" XDG_DATA_HOME="$work/data" XDG_STATE_HOME="$work/state"
export AGENT_INSTALL_SOFT_FAIL=
sh "$SOURCE/plugins/prepare-lock.sh" "$PREFIX" "${AGENT_VM_VERSION_OPENCODE_CLAUDE:-}"
# Every pnpm invocation here suppresses the update notifier. A build must not
# spend a registry round trip on a "newer pnpm is available" notice, and the
# strict-egress profile must describe exactly the traffic the install needs.
# Same reasoning as the Pi wrapper's enforced PI_SKIP_VERSION_CHECK.
[ "$(timeout --kill-after=5 20 "$pnpm" --version --pm-on-fail=error --config.update-notifier=false)" = 11.11.0 ] || {
    echo 'install-opencode-plugins: pnpm 11.11.0 is required' >&2
    exit 1
}
(
    cd "$PREFIX"
    sh "$CONTRACT/run-pnpm.sh" "$work/install.ndjson" -- \
        "$pnpm" install \
        --config.update-notifier=false \
        --frozen-lockfile --prod --no-optional \
        --ignore-scripts --ignore-pnpmfile --pm-on-fail=error \
        --registry=https://registry.npmjs.org/ \
        --package-import-method=copy --store-dir "$work/store"
)
chmod -R a+rX "$PREFIX"
success=true
