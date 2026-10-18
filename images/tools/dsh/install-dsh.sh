#!/bin/sh
# Base owns pnpm. This hook owns transient state, never caller caches.
#
# Owning install hook for the dsh layer, bind-mounted and never COPYed so it
# stays out of the shipped image. The four reviewed inputs are installed frozen
# with scripts disabled and a disposable external store; node_modules/.pnpm is
# runtime payload, not a store.
#
# No explicit version slot means "keep the committed selection". A changed exact
# dsh selection is prepared first, which resolves and validates a new graph; an
# empty or equal slot starts no package manager during preparation.
#
# Env contract (container defaults; fixture overrides only under
# AGENT_VM_FIXTURE_ROOT): AGENT_VM_DSH_PREFIX, AGENT_VM_DSH_SOURCE_DIR,
# AGENT_VM_CONTRACT_DIR, AGENT_VM_VERSION_DSH, AGENT_VM_VERSION_PNPM,
# AGENT_VM_DSH_COMMAND (fixed in production).
#
# dsh is never soft-failable: a partial node_modules is a broken agent, not a
# missing one, so every failure here is hard regardless of AGENT_INSTALL_SOFT_FAIL.
set -eu

fail() {
    echo "install-dsh.sh: $*" >&2
    exit 1
}

# A Node preload would make every later check untrustworthy, so refuse injected
# runtime options before any Node process starts.
[ -z "${NODE_OPTIONS-}" ] || fail 'NODE_OPTIONS is not supported'
[ -z "${NODE_PATH-}" ] || fail 'NODE_PATH is not supported'
[ -z "${BUN_OPTIONS-}" ] || fail 'BUN_OPTIONS is not supported'

PREFIX=${AGENT_VM_DSH_PREFIX:-/opt/agent-vm/dsh}
SOURCE=${AGENT_VM_DSH_SOURCE_DIR:-/tmp/dsh-source}
COMMAND=${AGENT_VM_DSH_COMMAND:-/usr/local/bin/dsh}
DSH_VERSION=${AGENT_VM_VERSION_DSH:-}
PNPM_VERSION=${AGENT_VM_VERSION_PNPM:-}

PROJECT=$SOURCE/project.py
PREPARE=$SOURCE/prepare-lock.sh
RUNTIME_CHECK=$SOURCE/check-runtime.mjs
[ -f "$PROJECT" ] || fail "dsh project helper is missing at $PROJECT"
[ -f "$PREPARE" ] || fail "prepare-lock.sh is missing at $PREPARE"
[ -f "$RUNTIME_CHECK" ] || fail "layout checker is missing at $RUNTIME_CHECK"

# Authorization covers the prefix, its four inputs, the base pin and the command
# destination this install will later be linked from. It runs before any mutation
# or destructive trap can exist.
facts=$(python3 "$PROJECT" authorize "$PREFIX" --command "$COMMAND") || exit 1
base_pin=$(printf '%s\n' "$facts" | jq -er '.basePin') || fail 'authorization returned no base pin'
python3 "$PROJECT" guard "$PREFIX" || exit 1
# The contract directory and its runner execute policy, so the same authorization
# names the directory this install may spawn from (build mount, fixture copy or
# the checkout's own tracked copy). Nothing in it runs before this line.
CONTRACT=$(python3 "$PROJECT" contract-dir "$PREFIX") || exit 1
[ -f "$CONTRACT/pnpm-lock.mjs" ] || fail "graph module is missing in $CONTRACT"
[ -f "$CONTRACT/run-pnpm.sh" ] || fail "hard runner is missing in $CONTRACT"

PNPM_SOURCE=${PNPM_SOURCE_DIR:-$SOURCE/../../pnpm}
[ -f "$PNPM_SOURCE/read-pin-version.py" ] || fail "cannot locate the reviewed pnpm sources at $PNPM_SOURCE"
base_pnpm=$(python3 "$PNPM_SOURCE/read-pin-version.py" "$base_pin") || fail 'cannot read the base pin'
[ "$base_pnpm" = '11.11.0' ] || fail "unsupported base manager selection: $base_pnpm"
[ -z "$PNPM_VERSION" ] || [ "$PNPM_VERSION" = "$base_pnpm" ] ||
    fail 'pnpm compatibility slot must equal base pin'
package_manager=$(jq -er '.packageManager' "$PREFIX/package.json") || fail 'committed manifest has no packageManager'
[ "$package_manager" = "pnpm@$base_pnpm" ] ||
    fail "manifest packageManager $package_manager disagrees with base pin $base_pnpm"

status=0
sh "$PREPARE" "$PREFIX" "$DSH_VERSION" "$PNPM_VERSION" || status=$?
# A signal reaches the owning shell unchanged instead of becoming a plain failure.
case $status in
    0) ;;
    130 | 143 | 129) exit "$status" ;;
    *) exit 1 ;;
esac
committed_dsh=$(jq -er '.dependencies["@deepseek-ai/dsh"]' "$PREFIX/package.json") || fail 'committed manifest selects no dsh version'
[ -z "$DSH_VERSION" ] || [ "$DSH_VERSION" = "$committed_dsh" ] ||
    fail "prepared selection $committed_dsh does not match the requested $DSH_VERSION"
node "$CONTRACT/pnpm-lock.mjs" check dsh "$PREFIX" || fail 'reviewed inputs failed pinned-input validation'

work=''
installed=0
on_exit() {
    status=$?
    if [ -n "$work" ]; then
        # A scratch left behind would be transient state inside the image layer, so
        # a failed cleanup is a failure, not a discarded best effort.
        if ! python3 "$PROJECT" cleanup "$work" --prefix "$PREFIX" >/dev/null 2>&1; then
            echo 'install-dsh.sh: invocation scratch cleanup failed' >&2
            [ "$status" -ne 0 ] || status=1
        fi
    fi
    if [ "$status" -ne 0 ] && [ "$installed" -eq 0 ]; then
        # Hard failure: no half-installed agent may survive, and no caller cache is
        # touched because the manager never saw it.
        python3 "$PROJECT" remove-install "$PREFIX" || \
            echo 'install-dsh.sh: partial node_modules cleanup failed; recovery required' >&2
    fi
    exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

work=$(python3 "$PROJECT" scratch "$PREFIX") || exit 1
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

# project.py owns manager selection: the fixed base launcher, or an authorized
# fixture fake. Reading its argv back line by line keeps spaces intact.
manager=$(python3 "$PROJECT" manager "$PREFIX") || exit 1
set --
while IFS= read -r word; do
    set -- "$@" "$word"
done <<EOF
$manager
EOF
[ "$#" -ge 1 ] || fail 'authorization returned no manager argv'

# Every manager call, including the identity check, carries the policy flags: a
# caller pnpmfile or an ambient registry must never influence even `--version`.
timeout --kill-after=5 20 "$@" --version --pm-on-fail=error --config.update-notifier=false \
    --ignore-pnpmfile --registry=https://registry.npmjs.org/ >"$work/version.out" 2>"$work/version.err" || fail 'base manager identity check failed'
[ "$(cat "$work/version.out")" = "$base_pnpm" ] || fail "base manager is not the pin $base_pnpm"

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

# The direct link target, the reviewed bin mapping and the absence of a second
# package manager are packaging facts, so native code owns them.
layout=$(node "$RUNTIME_CHECK" "$PREFIX" --layout) || fail 'installed layout failed validation'
entry=$(printf '%s\n' "$layout" | jq -er '.resolvedBin') || fail 'layout check returned no bin target'
# a+rX publishes the payload for the read-only runtime UIDs; the link itself is
# created by the image definition, never by a generated .bin wrapper.
chmod a+rx "$entry" || fail 'cannot make the dsh bin executable'
chmod -R a+rX "$PREFIX" || fail 'cannot publish the installed payload a+rX'
installed=1
echo "install-dsh.sh: installed dsh $committed_dsh from the reviewed inputs"
