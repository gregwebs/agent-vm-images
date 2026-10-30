#!/bin/sh
# Owning install hook for the Pi layer, bind-mounted and never COPYed so it stays
# out of the shipped image.
#
# Base owns pnpm. The four reviewed inputs are installed frozen with scripts
# disabled and a disposable external store; node_modules/.pnpm is runtime
# payload, not a store. The graph itself was produced by the owning preparation
# driver (images/tools/pi/prepare-project.py), which this hook calls first: an
# empty or equal selection starts no package manager at all, and a changed
# selection resolves/imports, validates and publishes exactly four inputs.
#
# Four separate proofs belong to four owners, and this hook merely sequences
# them: preparation owns *what* the graph may be, pnpm owns the installed tree,
# the byte checker owns "the installed bytes are the authenticated ones", and the
# runtime checker owns "the installed tree is the selection the lock describes" --
# the root command is a direct link to the locked bin target and the N6 packages
# provide their shipped runtime. Both checkers run before the installed CLI is
# executed for the first time (`--help`, which derives the dispatch list): a tree
# that merely looks installed must not get to run.
# A successful lock import is neither byte equivalence nor runtime health, so the
# five/six sibling archives are fetched independently through download.sh, their
# SRI checked before extraction, and the pristine extractions compared against
# the installed tree by check-installed-bytes.mjs (never COPYed into runtime).
#
# The only soft-failable step is that independent curl fetch, and only for a
# transport failure the download contract positively classified: the producer
# then removes this whole authorized prefix and records `absent-transport CODE`
# outside it. A checksum, extraction, byte, preparation or pnpm failure is hard
# in every soft mode. The installer never writes the installed record; only the
# verifier does.
set -eu

fail() {
    echo "install-pi.sh: $*" >&2
    exit 1
}

# A Node preload would make every later check untrustworthy, so refuse injected
# runtime options before any Node process starts (the shared guard repeats this).
[ -z "${NODE_OPTIONS-}" ] || fail 'NODE_OPTIONS is not supported'
[ -z "${NODE_PATH-}" ] || fail 'NODE_PATH is not supported'
[ -z "${BUN_OPTIONS-}" ] || fail 'BUN_OPTIONS is not supported'

PREFIX=${AGENT_VM_PI_PREFIX:-/opt/agent-vm/pi}
SOURCE=${AGENT_VM_PI_SOURCE_DIR:-/tmp/pi-source}
PROJECT=${AGENT_VM_PI_SHARED:-$SOURCE/../pnpm-project.py}
PREPARE=$SOURCE/prepare-project.py
BYTES_CHECK=${AGENT_VM_PI_BYTES_CHECK:-$SOURCE/check-installed-bytes.mjs}
# Pinned by the standard image's install RUN, like the verifier's copy: the checker
# decides whether the installed tree is the lock's selection, so a build must not be
# able to inherit a different one from an image ENV.
RUNTIME_CHECK=${AGENT_VM_PI_RUNTIME_CHECK:-$SOURCE/check-runtime.mjs}
EXTRACTOR=$SOURCE/archive.py
DISPATCH_DIR=${AGENT_VM_PI_DISPATCH_DIR:-$SOURCE/dispatch}
TARGET_PLATFORM=${AGENT_VM_PI_TARGET_PLATFORM:-}
WORK_DIR=${AGENT_VM_PI_WORK_DIR:-}
PI_VERSION=${AGENT_VM_VERSION_PI:-}
soft_fail=${AGENT_INSTALL_SOFT_FAIL:-}
receipt=${AGENT_VM_TRANSPORT_RECEIPT:-}

for helper in "$PROJECT" "$PREPARE" "$BYTES_CHECK" "$RUNTIME_CHECK" "$EXTRACTOR"; do
    [ -f "$helper" ] || fail "helper is missing at $helper"
done
[ -n "$TARGET_PLATFORM" ] ||
    fail 'AGENT_VM_PI_TARGET_PLATFORM must name linux/amd64 or linux/arm64; the host is never guessed'

# Authorization covers the prefix, its four inputs, the base pin and the status
# destination, and runs before any mutation or destructive trap exists. The guard
# rejects ambient manager/registry/TLS/config bypasses before a Node process or a
# manager exists.
facts=$(python3 "$PROJECT" --consumer pi authorize "$PREFIX") || exit 1
base_pin=$(printf '%s\n' "$facts" | jq -er '.basePin') || fail 'authorization returned no base pin'
[ -f "$base_pin" ] || fail "the reviewed base pin is missing at $base_pin"
python3 "$PROJECT" --consumer pi guard "$PREFIX" || exit 1
CONTRACT=$(python3 "$PROJECT" --consumer pi contract-dir "$PREFIX") || exit 1
[ -x "$CONTRACT/recipe-contract" ] || fail "no executable recipe-contract in $CONTRACT"
[ -f "$CONTRACT/run-pnpm.sh" ] || fail "hard runner is missing in $CONTRACT"
[ -f "$CONTRACT/download.sh" ] || fail "classified download is missing in $CONTRACT"
[ -f "$CONTRACT/run-report.sh" ] || fail "bounded report runner is missing in $CONTRACT"

# The four reviewed inputs must be complete, pinned and internally consistent
# before preparation or a manager can be reached.
"$CONTRACT/recipe-contract" pnpm-lock check pi "$PREFIX" ||
    fail 'reviewed inputs failed pinned-input validation'
package_manager=$(jq -er '.packageManager' "$PREFIX/package.json") || fail 'committed manifest has no packageManager'
[ "$package_manager" = 'pnpm@11.11.0' ] || fail "manifest packageManager $package_manager is not the base pin pnpm@11.11.0"

# Preparation is a precondition, never a transport-dependent install step: it
# owns zero-manager equal/no-op, candidate comparison before pinning, a cold
# frozen validation and the four-input transaction. Its signals are the hook's.
status=0
python3 "$PREPARE" --consumer pi prepare "$PREFIX" "$PI_VERSION" || status=$?
case $status in
    0) ;;
    130 | 143 | 129) exit "$status" ;;
    3) fail 'preparation rollback is incomplete; the recovery root above must be inspected' ;;
    *) fail 'preparation failed' ;;
esac
committed_pi=$(jq -er '.dependencies["@earendil-works/pi-coding-agent"]' "$PREFIX/package.json") ||
    fail 'committed manifest selects no Pi version'
[ -z "$PI_VERSION" ] || [ "$PI_VERSION" = "$committed_pi" ] ||
    fail "prepared selection $committed_pi does not match the requested $PI_VERSION"
"$CONTRACT/recipe-contract" pnpm-lock check pi "$PREFIX" ||
    fail 'prepared inputs failed pinned-input validation'
"$CONTRACT/recipe-contract" pnpm-lock graph pi "$PREFIX" >/dev/null ||
    fail 'prepared inputs failed graph validation'

work=''
installed=0
on_exit() {
    status=$?
    if [ -n "$work" ]; then
        # A scratch left behind would be transient state inside the image layer.
        if ! python3 "$PROJECT" --consumer pi cleanup "$work" --prefix "$PREFIX" >/dev/null 2>&1; then
            echo 'install-pi.sh: invocation scratch cleanup failed' >&2
            [ "$status" -ne 0 ] || status=1
        fi
    fi
    if [ "$status" -ne 0 ] && [ "$installed" -eq 0 ]; then
        # A half-installed agent is broken rather than absent. Only the
        # authorized prefix's own node_modules is removable here, and no caller
        # cache is touched because the manager never saw one.
        python3 "$PROJECT" --consumer pi remove-install "$PREFIX" ||
            echo 'install-pi.sh: partial node_modules cleanup failed; recovery required' >&2
    fi
    exit "$status"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# AGENT_VM_PI_WORK_DIR is an authorized *parent*: the invocation still allocates a
# unique 0700 child with mkdtemp and never removes the caller's directory, so two
# concurrent runs cannot share selector output, a store or an extraction.
if [ -n "$WORK_DIR" ]; then
    work=$(python3 "$PROJECT" --consumer pi scratch "$PREFIX" --parent "$WORK_DIR") || exit 1
else
    work=$(python3 "$PROJECT" --consumer pi scratch "$PREFIX") || exit 1
fi
HOME=$work/home
XDG_CONFIG_HOME=$work/config
XDG_CACHE_HOME=$work/cache
XDG_DATA_HOME=$work/data
XDG_STATE_HOME=$work/state
TMPDIR=$work/tmp
export HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME TMPDIR
# The five reviewed defaults travel in the owning subprocess *and* on every
# command line (see the install argv below): neither an inherited environment nor
# a future manager default can change what was reviewed.
export PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=true
export PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=false
export PNPM_CONFIG_TRUST_POLICY=no-downgrade
export PNPM_CONFIG_IGNORE_SCRIPTS=true
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$TMPDIR"
export AGENT_VM_PI_TARGET_PLATFORM=$TARGET_PLATFORM AGENT_VM_CONTRACT_DIR=$CONTRACT

# project.py owns manager selection: the fixed base launcher, or an authorized
# fixture fake. Reading its argv back line by line keeps spaces intact.
manager=$(python3 "$PROJECT" --consumer pi manager "$PREFIX") || exit 1
set --
while IFS= read -r word; do
    set -- "$@" "$word"
done <<EOF
$manager
EOF
[ "$#" -ge 1 ] || fail 'authorization returned no manager argv'

# Every manager call, including the identity check, carries the policy flags: an
# ambient registry or a caller pnpmfile must not influence even `--version`.
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

# Independent authenticated archives. The checker learns the reviewed family and
# the bin-providing dependencies from the lock alone; this hook downloads each
# one itself through the classified download contract and checks its SRI before
# anything is extracted, so pnpm's own SRI check is never the only barrier.
archives=$work/archives
mkdir -p "$archives" "$archives/evidence" || fail 'cannot create the extraction parent'
inventory=$work/inventory.json
node "$BYTES_CHECK" inventory "$PREFIX" >"$inventory" ||
    fail 'cannot derive the sibling inventory from the reviewed lock'

# Soften a positively classified transport failure: remove the authorized prefix
# and record the absence outside it. Only the independent curl call can reach
# here, and only with a fresh receipt this build wrote.
soften() { # $1 = curl code, $2 = message
    local_message=$2
    if [ -z "$soft_fail" ]; then
        echo "$local_message" >&2
        exit 1
    fi
    # The removal capability is the installer's own and is minted before the
    # prefix disappears; the invocation scratch is cleaned here, while it still
    # belongs to this invocation, so the exit trap cannot fail on a prefix that is
    # already gone. Only a classified curl failure reaches this function.
    python3 "$PROJECT" --consumer pi cleanup "$work" ||
        fail 'invocation scratch cleanup failed before the soft-fail removal'
    work=''
    python3 "$PROJECT" --consumer pi pi-transport-removal "$PREFIX" "$1" ||
        fail 'authorized removal after a classified download failure failed'
    installed=1
    echo "$local_message (soft-fail mode; image will ship without pi)"
    exit 0
}

fetch() { # $1 = url, $2 = destination, $3 = integrity, $4 = label
    rc=0
    sh "$CONTRACT/download.sh" "$1" "$2" || rc=$?
    if [ "$rc" -ne 0 ]; then
        message="==> pi: could not fetch $1 for $4"
        if [ "$rc" -eq 75 ] && [ -n "$receipt" ] &&
            grep -Eq '^transport download [0-9]+$' "$receipt"; then
            soften "$(awk '{print $3}' "$receipt")" "$message"
        fi
        echo "$message" >&2
        exit 1
    fi
    actual="sha512-$(openssl dgst -sha512 -binary "$2" | openssl base64 -A)"
    if [ "$actual" != "$3" ]; then
        echo "==> pi: INTEGRITY MISMATCH for $4 ($1)" >&2
        echo "    the lock says $3" >&2
        echo "    fetched      $actual" >&2
        exit 1
    fi
}

family_count=$(jq -er '.family | length' "$inventory") || fail 'inventory has no family'
[ "$family_count" -ge 1 ] || fail 'inventory has an empty family'
index=0
anchor_name=
anchor_root=
while [ "$index" -lt "$family_count" ]; do
    name=$(jq -er ".family[$index].name" "$inventory") || fail 'inventory family entry has no name'
    version=$(jq -er ".family[$index].version" "$inventory") || fail "inventory entry $name has no version"
    tarball=$(jq -er ".family[$index].tarball" "$inventory") || fail "inventory entry $name has no tarball"
    integrity=$(jq -er ".family[$index].integrity" "$inventory") || fail "inventory entry $name has no integrity"
    tarball_path=$work/$(printf '%s-%s' "$name" "$version" | tr '/@' '__').tgz
    fetch "$tarball" "$tarball_path" "$integrity" "$name@$version"
    root=$archives/$name/$version
    mkdir -p "$root" || fail "cannot create the $name extraction root"
    python3 "$EXTRACTOR" extract "$tarball_path" "$root" ||
        fail "$name@$version could not be extracted safely"
    if [ "$index" -eq 0 ]; then
        anchor_name=$name
        anchor_root=$root
    fi
    index=$((index + 1))
done

provider_count=$(jq -er '.providers | length' "$inventory") || fail 'inventory has no provider list'
index=0
while [ "$index" -lt "$provider_count" ]; do
    name=$(jq -er ".providers[$index].name" "$inventory") || fail 'provider entry has no name'
    version=$(jq -er ".providers[$index].version" "$inventory") || fail "provider $name has no version"
    tarball=$(jq -er ".providers[$index].tarball" "$inventory") || fail "provider $name has no tarball"
    integrity=$(jq -er ".providers[$index].integrity" "$inventory") || fail "provider $name has no integrity"
    evidence=$archives/evidence/$name/$version
    mkdir -p "$evidence" || fail "cannot create $name evidence"
    fetch "$tarball" "$evidence/.pending.tgz" "$integrity" "$name@$version"
    # The evidence filename is the lock SRI's digest, derived from the bytes that
    # just passed the SRI check rather than from a caller-supplied spelling.
    digest=$(openssl dgst -sha512 -binary "$evidence/.pending.tgz" | od -An -tx1 | tr -d ' \n')
    mv "$evidence/.pending.tgz" "$evidence/$digest.tgz" || fail "cannot retain $name evidence"
    index=$((index + 1))
done

sibling=${anchor_name##*/}
node "$BYTES_CHECK" "$PREFIX" "$sibling" "$anchor_root" ||
    fail 'the installed bytes are not the authenticated bytes'

# Installed Pi code is executed for the first time below, so both identity proofs
# come first: the byte proof above says the tree is the authenticated archives, and
# this one says the tree is the selection the lock describes -- the root command is
# a direct link to the locked bin target, and the N6 lifecycle packages provide the
# shipped runtime rather than an install script. A tree that merely looks installed
# is not enough to run.
AGENT_VM_PI_TARGET_PLATFORM=$TARGET_PLATFORM \
    AGENT_VM_CONTRACT_DIR=$CONTRACT \
    timeout --kill-after=5 600 node "$RUNTIME_CHECK" "$PREFIX" >"$work/runtime.json" ||
    fail 'the installed Pi runtime failed its identity and N6 probes'
grep -q '^pi runtime: ' "$work/runtime.json" ||
    fail 'the runtime checker returned no report'

# The wrapper dispatches a subcommand only when the FIRST argument is one of the
# selected words, so the selected list has to describe *this* Pi. It is therefore
# generated here from the installed CLI's own bounded `--help`, checked against
# the reviewed expectation for the selected version, and published as data only:
# a root-owned 0644 file with one canonical sorted unique word per line. Pi
# 0.87.0 has no `mcp`; the wrapper's closed vocabulary still contains it, and a
# word that is not selected takes the mandatory-warning prompt path instead.
expectation="$DISPATCH_DIR/$committed_pi.txt"
[ -f "$expectation" ] ||
    fail "no reviewed dispatch expectation for Pi $committed_pi at $expectation -- the subcommand set needs review"
status=0
PI_SKIP_VERSION_CHECK=1 PI_TELEMETRY=0 \
    sh "$CONTRACT/run-report.sh" 60 "$work/help.out" "$work/help.err" "$PREFIX/node_modules/.bin/pi" --help ||
    status=$?
[ "$status" -eq 0 ] || fail "the installed pi --help exited $status; refusing to derive its subcommand list"
generated="$work/subcommands"
sed -n 's/^  pi \([a-z][a-z-]*\) .*/\1/p' "$work/help.out" | LC_ALL=C sort -u >"$generated" ||
    fail 'cannot read the installed CLI subcommand list'
[ -s "$generated" ] || fail 'the installed CLI advertises no subcommands'
cmp -s "$generated" "$expectation" ||
    fail "installed pi advertises [$(tr '\n' ' ' <"$generated")], the reviewed $committed_pi list is [$(tr '\n' ' ' <"$expectation")]"
dispatch="$PREFIX/agent-vm-subcommands"
if [ -e "$dispatch" ] || [ -L "$dispatch" ]; then
    [ -f "$dispatch" ] && [ ! -L "$dispatch" ] || fail "refusing to replace $dispatch"
fi
cp "$generated" "$work/subcommands.publish" || fail 'cannot stage the subcommand list'
chmod 0644 "$work/subcommands.publish" || fail 'cannot set the subcommand list mode'
mv "$work/subcommands.publish" "$dispatch" || fail 'cannot publish the subcommand list'

# The payload is published for arbitrary read-only runtime UIDs. Only the derived
# locked bin targets are executable (0755); nothing gains a write bit here.
chmod -R a+rX "$PREFIX" || fail 'cannot publish the installed payload a+rX'
installed=1
echo "install-pi.sh: installed pi $committed_pi from the reviewed inputs"
