#!/bin/sh
# Owning install hook for the image-owned pi-antigravity payload, bind-mounted
# and never COPYed so it stays out of the shipped image. See
# images/tools/README.md and images/tools/pi/README.md.
#
# Same hard runner as Pi and the Claude bridge, and no absence producer of any
# kind: the four reviewed inputs are installed frozen with scripts disabled, in a
# disposable external store with a private HOME/XDG, and every failure is hard
# regardless of AGENT_INSTALL_SOFT_FAIL. A provider that is present but broken
# (or missing while Pi is present) is a broken feature, not a degraded one; the
# only absence this image can have is the derivative of a genuine Pi curl
# absence, and the verifier's single gate entry owns that record.
#
# The reviewed workspace is hoisted with `autoInstallPeers: false`: Pi supplies
# @earendil-works/pi-ai and @earendil-works/pi-coding-agent through its own
# loader, so nothing from Pi's tree is installed here and `recipe-contract`'s
# policy rejects any resolved @earendil-works identity. One reviewed exception is
# recorded in the workspace: `trustPolicyExclude: ['pi-antigravity@0.11.0']`,
# because 0.11.0 is published without the provenance attestation its predecessors
# carry (ADR 0016). It is not a general relaxation and it names one version.
set -eu

fail() {
    echo "install-pi-antigravity.sh: $*" >&2
    exit 1
}

[ -z "${NODE_OPTIONS-}" ] || fail 'NODE_OPTIONS is not supported'
[ -z "${NODE_PATH-}" ] || fail 'NODE_PATH is not supported'
[ -z "${BUN_OPTIONS-}" ] || fail 'BUN_OPTIONS is not supported'

PREFIX=${AGENT_VM_PI_ANTIGRAVITY_PREFIX:-/opt/agent-vm/pi-antigravity}
SOURCE=${AGENT_VM_PI_SOURCE_DIR:-/tmp/pi-source}
PROJECT=${AGENT_VM_PI_SHARED:-$SOURCE/../pnpm-project.py}
PREPARE=$SOURCE/prepare-project.py
WORK_DIR=${AGENT_VM_ANTIGRAVITY_WORK_DIR:-}
ANTIGRAVITY_VERSION=${AGENT_VM_VERSION_PI_ANTIGRAVITY:-}

for helper in "$PROJECT" "$PREPARE"; do
    [ -f "$helper" ] || fail "helper is missing at $helper"
done

# Authorization names the prefix, its four inputs, the base pin and the status
# destination before anything is mutated; the guard rejects ambient manager,
# registry, TLS and config bypasses before any Node process or manager exists.
facts=$(python3 "$PROJECT" --consumer antigravity authorize "$PREFIX") || exit 1
base_pin=$(printf '%s\n' "$facts" | jq -er '.basePin') || fail 'authorization returned no base pin'
[ -f "$base_pin" ] || fail "the reviewed base pin is missing at $base_pin"
python3 "$PROJECT" --consumer antigravity guard "$PREFIX" || exit 1
CONTRACT=$(python3 "$PROJECT" --consumer antigravity contract-dir "$PREFIX") || exit 1
[ -x "$CONTRACT/recipe-contract" ] || fail "no executable recipe-contract in $CONTRACT"
[ -f "$CONTRACT/run-pnpm.sh" ] || fail "hard runner is missing in $CONTRACT"

"$CONTRACT/recipe-contract" pnpm-lock check antigravity "$PREFIX" ||
    fail 'reviewed inputs failed pinned-input validation'
package_manager=$(jq -er '.packageManager' "$PREFIX/package.json") || fail 'committed manifest has no packageManager'
[ "$package_manager" = 'pnpm@11.11.0' ] || fail "manifest packageManager $package_manager is not the base pin pnpm@11.11.0"

status=0
python3 "$PREPARE" --consumer antigravity prepare "$PREFIX" "$ANTIGRAVITY_VERSION" || status=$?
case $status in
    0) ;;
    130 | 143 | 129) exit "$status" ;;
    3) fail 'preparation rollback is incomplete; the recovery root above must be inspected' ;;
    *) fail 'preparation failed' ;;
esac
committed_antigravity=$(jq -er '.dependencies["pi-antigravity"]' "$PREFIX/package.json") ||
    fail 'committed manifest selects no antigravity version'
[ -z "$ANTIGRAVITY_VERSION" ] || [ "$ANTIGRAVITY_VERSION" = "$committed_antigravity" ] ||
    fail "prepared selection $committed_antigravity does not match the requested $ANTIGRAVITY_VERSION"
"$CONTRACT/recipe-contract" pnpm-lock check antigravity "$PREFIX" ||
    fail 'prepared inputs failed pinned-input validation'
"$CONTRACT/recipe-contract" pnpm-lock graph antigravity "$PREFIX" >/dev/null ||
    fail 'prepared inputs failed graph validation'

work=''
installed=0
on_exit() {
    status=$?
    if [ -n "$work" ]; then
        if ! python3 "$PROJECT" --consumer antigravity cleanup "$work" --prefix "$PREFIX" >/dev/null 2>&1; then
            echo 'install-pi-antigravity.sh: invocation scratch cleanup failed' >&2
            [ "$status" -ne 0 ] || status=1
        fi
    fi
    if [ "$status" -ne 0 ] && [ "$installed" -eq 0 ]; then
        # A partially written provider tree is broken, not absent. Only the
        # authorized prefix's own node_modules is removable, and no caller cache
        # exists to touch because the manager never saw one.
        python3 "$PROJECT" --consumer antigravity remove-install "$PREFIX" ||
            echo 'install-pi-antigravity.sh: partial node_modules cleanup failed; recovery required' >&2
    fi
    exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

if [ -n "$WORK_DIR" ]; then
    work=$(python3 "$PROJECT" --consumer antigravity scratch "$PREFIX" --parent "$WORK_DIR") || exit 1
else
    work=$(python3 "$PROJECT" --consumer antigravity scratch "$PREFIX") || exit 1
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

manager=$(python3 "$PROJECT" --consumer antigravity manager "$PREFIX") || exit 1
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
# assumed from a successful exit: a hoisted tree with real package directories,
# the provider's one runtime dependency, no Pi alias Pi's loader would satisfy,
# and no private-store payload below the metadata-only `.pnpm` directory.
if [ -L "$PREFIX/node_modules/pi-antigravity" ] ||
    [ ! -d "$PREFIX/node_modules/pi-antigravity" ]; then
    fail 'the provider tree is not hoisted: the provider package is not a real directory'
fi
private=$(find "$PREFIX/node_modules/.pnpm" -mindepth 1 -type d -print 2>/dev/null | head -n 1)
[ -z "$private" ] ||
    fail "the provider tree carries private-store package payload ($private); the reviewed workspace is hoisted"
[ -r "$PREFIX/node_modules/pi-antigravity/src/index.ts" ] ||
    fail 'the installed provider has no loadable TypeScript entry point'
[ -f "$PREFIX/node_modules/undici/package.json" ] ||
    fail 'the provider runtime dependency undici is missing from the hoisted tree'
for aliased in '@earendil-works' 'typebox' '@sinclair/typebox' 'pi-agent-core' 'pi-ai' 'pi-tui'; do
    if [ -e "$PREFIX/node_modules/$aliased" ] || [ -L "$PREFIX/node_modules/$aliased" ]; then
        fail "the provider tree installs $aliased, which Pi's loader aliases to its own copy"
    fi
done

chmod -R a+rX "$PREFIX" || fail 'cannot publish the installed payload a+rX'
installed=1
echo "install-pi-antigravity.sh: installed pi-antigravity $committed_antigravity from the reviewed inputs"
