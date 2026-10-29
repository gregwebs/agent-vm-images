#!/bin/sh
# Owning install hook for the image-owned pi-claude-bridge payload, bind-mounted
# and never COPYed so it stays out of the shipped image. See
# images/tools/README.md and
# https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0023-image-owned-pi-extension-packages.md.
#
# Same hard runner as Pi and no absence producer of any kind: the four reviewed
# inputs are installed frozen with scripts disabled, in a disposable external
# store with a private HOME/XDG, and every failure is hard regardless of
# AGENT_INSTALL_SOFT_FAIL. A bridge that is present but broken (or missing while
# Pi is present) is a broken feature, not a degraded one; the only bridge absence
# this image can have is the derivative of a genuine Pi curl absence, and the
# verifier's single gate entry owns that record. Retiring the npm-only
# `absent-transport` producer here is the acknowledged #42/ADR-0002 narrowing.
#
# The reviewed workspace is hoisted, keeps the Claude Agent SDK JavaScript and
# excludes only its platform native packages through
# `ignoredOptionalDependencies`. There is deliberately no `--no-optional` (it
# trips a pnpm 11.11.0 lockfile bug), no `--legacy-peer-deps` translation, no
# NODE_PATH, no source patch and no Pi/typebox alias: Pi supplies those peers to
# the bridge through its own loader, and `recipe-contract`'s bridge policy
# rejects any resolved @earendil-works/typebox identity.
set -eu

fail() {
    echo "install-pi-packages.sh: $*" >&2
    exit 1
}

[ -z "${NODE_OPTIONS-}" ] || fail 'NODE_OPTIONS is not supported'
[ -z "${NODE_PATH-}" ] || fail 'NODE_PATH is not supported'
[ -z "${BUN_OPTIONS-}" ] || fail 'BUN_OPTIONS is not supported'

PREFIX=${AGENT_VM_PI_PACKAGES_PREFIX:-/opt/agent-vm/pi-packages}
SOURCE=${AGENT_VM_PI_SOURCE_DIR:-/tmp/pi-source}
PROJECT=${AGENT_VM_PI_SHARED:-$SOURCE/../pnpm-project.py}
PREPARE=$SOURCE/prepare-project.py
WORK_DIR=${AGENT_VM_BRIDGE_WORK_DIR:-}
BRIDGE_VERSION=${AGENT_VM_VERSION_PI_CLAUDE_BRIDGE:-}

for helper in "$PROJECT" "$PREPARE"; do
    [ -f "$helper" ] || fail "helper is missing at $helper"
done

# Authorization names the prefix, its four inputs, the base pin and the status
# destination before anything is mutated; the guard rejects ambient manager,
# registry, TLS and config bypasses before any Node process or manager exists.
facts=$(python3 "$PROJECT" --consumer bridge authorize "$PREFIX") || exit 1
base_pin=$(printf '%s\n' "$facts" | jq -er '.basePin') || fail 'authorization returned no base pin'
[ -f "$base_pin" ] || fail "the reviewed base pin is missing at $base_pin"
python3 "$PROJECT" --consumer bridge guard "$PREFIX" || exit 1
CONTRACT=$(python3 "$PROJECT" --consumer bridge contract-dir "$PREFIX") || exit 1
[ -x "$CONTRACT/recipe-contract" ] || fail "no executable recipe-contract in $CONTRACT"
[ -f "$CONTRACT/run-pnpm.sh" ] || fail "hard runner is missing in $CONTRACT"

"$CONTRACT/recipe-contract" pnpm-lock check bridge "$PREFIX" ||
    fail 'reviewed inputs failed pinned-input validation'
package_manager=$(jq -er '.packageManager' "$PREFIX/package.json") || fail 'committed manifest has no packageManager'
[ "$package_manager" = 'pnpm@11.11.0' ] || fail "manifest packageManager $package_manager is not the base pin pnpm@11.11.0"

status=0
python3 "$PREPARE" --consumer bridge prepare "$PREFIX" "$BRIDGE_VERSION" || status=$?
case $status in
    0) ;;
    130 | 143 | 129) exit "$status" ;;
    3) fail 'preparation rollback is incomplete; the recovery root above must be inspected' ;;
    *) fail 'preparation failed' ;;
esac
committed_bridge=$(jq -er '.dependencies["pi-claude-bridge"]' "$PREFIX/package.json") ||
    fail 'committed manifest selects no bridge version'
[ -z "$BRIDGE_VERSION" ] || [ "$BRIDGE_VERSION" = "$committed_bridge" ] ||
    fail "prepared selection $committed_bridge does not match the requested $BRIDGE_VERSION"
"$CONTRACT/recipe-contract" pnpm-lock check bridge "$PREFIX" ||
    fail 'prepared inputs failed pinned-input validation'
"$CONTRACT/recipe-contract" pnpm-lock graph bridge "$PREFIX" >/dev/null ||
    fail 'prepared inputs failed graph validation'

work=''
installed=0
on_exit() {
    status=$?
    if [ -n "$work" ]; then
        if ! python3 "$PROJECT" --consumer bridge cleanup "$work" --prefix "$PREFIX" >/dev/null 2>&1; then
            echo 'install-pi-packages.sh: invocation scratch cleanup failed' >&2
            [ "$status" -ne 0 ] || status=1
        fi
    fi
    if [ "$status" -ne 0 ] && [ "$installed" -eq 0 ]; then
        # A partially written bridge tree is broken, not absent. Only the
        # authorized prefix's own node_modules is removable, and no caller cache
        # exists to touch because the manager never saw one.
        python3 "$PROJECT" --consumer bridge remove-install "$PREFIX" ||
            echo 'install-pi-packages.sh: partial node_modules cleanup failed; recovery required' >&2
    fi
    exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if [ -n "$WORK_DIR" ]; then
    work=$(python3 "$PROJECT" --consumer bridge scratch "$PREFIX" --parent "$WORK_DIR") || exit 1
else
    work=$(python3 "$PROJECT" --consumer bridge scratch "$PREFIX") || exit 1
fi
HOME=$work/home
XDG_CONFIG_HOME=$work/config
XDG_CACHE_HOME=$work/cache
XDG_DATA_HOME=$work/data
XDG_STATE_HOME=$work/state
TMPDIR=$work/tmp
export HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME TMPDIR
export PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=true
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=false
export PNPM_CONFIG_TRUST_POLICY=no-downgrade
export PNPM_CONFIG_IGNORE_SCRIPTS=true
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$TMPDIR"
export AGENT_VM_CONTRACT_DIR=$CONTRACT

manager=$(python3 "$PROJECT" --consumer bridge manager "$PREFIX") || exit 1
set --
while IFS= read -r word; do
    set -- "$@" "$word"
done <<EOF
$manager
EOF
[ "$#" -ge 1 ] || fail 'authorization returned no manager argv'

timeout --kill-after=5 20 "$@" --version --pm-on-fail=error --config.update-notifier=false \
    --ignore-pnpmfile --registry=https://registry.npmjs.org/ \
    >"$work/version.out" 2>"$work/version.err" || fail 'base manager identity check failed'
[ "$(cat "$work/version.out")" = '11.11.0' ] || fail 'base manager is not the pinned 11.11.0'

(cd "$PREFIX" && sh "$CONTRACT/run-pnpm.sh" "$work/install.ndjson" -- \
    "$@" install --prod --frozen-lockfile \
    --config.minimum-release-age=1440 \
    --config.minimum-release-age-strict=true \
    --config.minimum-release-age-ignore-missing-time=false \
    --config.trust-policy=no-downgrade --ignore-scripts \
    --ignore-pnpmfile --pm-on-fail=error --config.update-notifier=false \
    --registry=https://registry.npmjs.org/ \
    --package-import-method=copy --store-dir "$work/store") || status=$?
case $status in
    0) ;;
    130 | 143 | 129) exit "$status" ;;
    *) fail 'frozen install failed' ;;
esac
[ -d "$PREFIX/node_modules" ] || fail 'frozen install produced no node_modules'

# The reviewed layout is a packaging fact, so it is proved here rather than
# assumed from a successful exit: hoisted tree, SDK JavaScript present, no SDK
# platform payload, no alias Pi's loader would satisfy.
# A hoisted install carries real package directories, not private-store payload.
# pnpm 11.11.0 still writes a metadata-only `node_modules/.pnpm/lock.yaml` even
# with `nodeLinker: hoisted` (observed in the focused harness' first real cold
# install), so the store's *existence* is not the discriminator: any directory
# below it is. The bridge package itself must be a real directory.
if [ -L "$PREFIX/node_modules/pi-claude-bridge" ] ||
    [ ! -d "$PREFIX/node_modules/pi-claude-bridge" ]; then
    fail 'the bridge tree is not hoisted: the bridge package is not a real directory'
fi
private=$(find "$PREFIX/node_modules/.pnpm" -mindepth 1 -type d -print 2>/dev/null | head -n 1)
[ -z "$private" ] ||
    fail "the bridge tree carries private-store package payload ($private); the reviewed workspace is hoisted"
[ -f "$PREFIX/node_modules/@anthropic-ai/claude-agent-sdk/package.json" ] ||
    fail 'the Claude Agent SDK JavaScript is missing from the hoisted bridge tree'
platform_native=$(find "$PREFIX/node_modules/@anthropic-ai" -maxdepth 1 -name 'claude-agent-sdk-*' -print 2>/dev/null | head -n 1)
[ -z "$platform_native" ] || fail "the bridge tree ships the omitted SDK platform package $platform_native"
for aliased in '@earendil-works' 'typebox' '@sinclair/typebox' 'pi-agent-core' 'pi-ai' 'pi-tui'; do
    if [ -e "$PREFIX/node_modules/$aliased" ] || [ -L "$PREFIX/node_modules/$aliased" ]; then
        fail "the bridge tree installs $aliased, which Pi's loader aliases to its own copy"
    fi
done
[ -r "$PREFIX/node_modules/pi-claude-bridge/src/index.ts" ] ||
    fail 'the installed bridge has no loadable TypeScript entry point'

chmod -R a+rX "$PREFIX" || fail 'cannot publish the installed payload a+rX'
installed=1
echo "install-pi-packages.sh: installed pi-claude-bridge $committed_bridge from the reviewed inputs"
