#!/bin/sh
# Exact reports are necessary, not sufficient: web and subprocess health gate installed.
#
# Build/final gate for the dsh layer (bind-mounted, never COPYed). The version
# *string* is the assertion, not the exit code: dsh dispatches on
# `import.meta.main`, which Node only defines from 22.19, so an older Node loads
# bin.js, prints nothing and exits 0. Checking only the exit status would certify
# an image that ships no working dsh.
#
# dsh is never soft-failable: a partial or unhealthy agent is a broken image, so
# every failure here is hard regardless of AGENT_INSTALL_SOFT_FAIL (this script
# deliberately never consults it).
#
# pnpm is inherited from the base image, not a dsh dependency. Its expected
# report therefore comes from the base pin, and `images/pnpm/verify.py` proves
# the launcher/distribution identity. A same-version raw shadow earlier on PATH
# is a different program and fails.
#
# Only after the exact reports, the same-file PATH identity, the real runtime
# probes (sandbox, PTY, libc FFI, genai, protobuf, Cordis subprocess, bounded web
# readiness) and the all-uid access audit succeed is the slot recorded
# `installed`. A rerun first resets the record to `pending`, so a failed repeat
# can never leave a stale success behind.
set -eu

PREFIX=${AGENT_VM_DSH_PREFIX:-/opt/agent-vm/dsh}
# The command the image links to the installed bin. The production value is fixed
# by project.py; a fixture prefix may point at its own fixture command.
COMMAND=${AGENT_VM_DSH_COMMAND:-/usr/local/bin/dsh}
status_dir=${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}
status=$status_dir/dsh

die() {
    echo "  $*" >&2
    exit 1
}

# A Node preload or injected module path would make every later check
# untrustworthy, so refuse it before any Node process starts.
[ -z "${NODE_OPTIONS-}" ] || die 'dsh: NODE_OPTIONS is not supported'
[ -z "${NODE_PATH-}" ] || die 'dsh: NODE_PATH is not supported'
[ -z "${BUN_OPTIONS-}" ] || die 'dsh: BUN_OPTIONS is not supported'

SOURCE=${AGENT_VM_DSH_SOURCE_DIR:-}
if [ -z "$SOURCE" ]; then
    SOURCE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
fi
PROJECT=$SOURCE/project.py
RUNTIME_CHECK=$SOURCE/check-runtime.mjs
PROBE=$SOURCE/probe-dsh.py
for helper in "$PROJECT" "$RUNTIME_CHECK" "$PROBE"; do
    [ -f "$helper" ] || die "dsh: build helper is missing at $helper"
done
PNPM_SOURCE=${PNPM_SOURCE_DIR:-$SOURCE/../../pnpm}
[ -f "$PNPM_SOURCE/verify.py" ] || die "dsh: reviewed pnpm sources are missing at $PNPM_SOURCE"

python3 "$PROJECT" guard "$PREFIX" || die 'dsh: the environment guard rejected this invocation'
# The contract helpers this gate executes (run-report.sh, check-tool-access.py)
# are executable policy, so the directory comes from the same authorized verb the
# guard used instead of a fixed build-mount path: an unset AGENT_VM_CONTRACT_DIR
# resolves to this checkout's (or fixture's) own copy, and a caller-selected one
# is authorized before the first helper runs. There is no fallback path.
CONTRACT=$(python3 "$PROJECT" contract-dir "$PREFIX") || die 'dsh: contract directory rejected'
facts=$(python3 "$PROJECT" authorize "$PREFIX" --command "$COMMAND" --status-dir "$status_dir") ||
    die 'dsh: prefix, command or status destination is not authorized'
base_pin=$(printf '%s\n' "$facts" | jq -er '.basePin') || die 'dsh: authorization returned no base pin'
base_pnpm=$(python3 "$PNPM_SOURCE/read-pin-version.py" "$base_pin") || die 'dsh: cannot read the base pin'
want_dsh=$(jq -er '.dependencies["@deepseek-ai/dsh"]' "$PREFIX/package.json") ||
    die 'dsh: the committed manifest selects no dsh version'
package_manager=$(jq -er '.packageManager' "$PREFIX/package.json") ||
    die 'dsh: the committed manifest has no packageManager assertion'
[ "$package_manager" = "pnpm@$base_pnpm" ] ||
    die "dsh: manifest packageManager $package_manager disagrees with base pin $base_pnpm"

# A supplied slot must equal the effective selection; otherwise the preparation
# did not take effect and verifying the manifest would verify the wrong thing.
if [ -n "${AGENT_VM_VERSION_DSH:-}" ] && [ "$AGENT_VM_VERSION_DSH" != "$want_dsh" ]; then
    die "dsh: layer asked for ${AGENT_VM_VERSION_DSH} but the manifest pins ${want_dsh}"
fi
if [ -n "${AGENT_VM_VERSION_PNPM:-}" ] && [ "$AGENT_VM_VERSION_PNPM" != "$base_pnpm" ]; then
    die "pnpm: layer asked for ${AGENT_VM_VERSION_PNPM} but the base pin is ${base_pnpm}"
fi

# Reset the record before measuring so a rerun that fails cannot leave a
# misleading `installed` from an earlier image state.
mkdir -p "$status_dir"
printf 'pending\n' >"$status"

command -v dsh >/dev/null 2>&1 || die 'dsh: MISSING — hard sanity failure'
command -v pnpm >/dev/null 2>&1 || die "pnpm: MISSING — hard sanity failure ('dsh plugin' would fail)"
# The first lexical PATH candidate must be the installed file itself: a
# same-version copy is a different program, while a symlink to the same file is
# the normal image layout and passes.
python3 "$PROJECT" command-identity dsh "$COMMAND" ||
    die 'dsh: the first PATH candidate is not the installed command'
python3 "$PNPM_SOURCE/verify.py" || die 'pnpm: base launcher/distribution identity failed'

work=$(python3 "$PROJECT" scratch "$PREFIX") || die 'dsh: cannot create invocation scratch'
cleanup() {
    status=$?
    if ! python3 "$PROJECT" cleanup "$work" --prefix "$PREFIX" >/dev/null 2>&1; then
        echo '  dsh: invocation scratch cleanup failed' >&2
        [ "$status" -ne 0 ] || status=1
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

HOME=$work/home
XDG_CONFIG_HOME=$work/config
XDG_CACHE_HOME=$work/cache
XDG_DATA_HOME=$work/data
XDG_STATE_HOME=$work/state
TMPDIR=$work/tmp
export HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME TMPDIR
mkdir -p "$HOME" "$XDG_CONFIG_HOME" "$XDG_CACHE_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME" "$TMPDIR"

# report EXE OUTFILE ERRFILE -> echoes the single non-blank stdout line, or
# fails on a nonzero/signal/timeout status, empty output or extra lines. The
# exact mapping is a bare `<version>`; anything else is unrecognized.
report() {
    exe=$1
    out=$2
    err=$3
    rc=0
    sh "$CONTRACT/run-report.sh" 20 "$out" "$err" "$exe" --version || rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "  $exe: --version exited $rc; refusing to accept the report" >&2
        sed -n '1,10p' "$err" >&2
        exit 1
    fi
    first=$(sed -n '1p' "$out")
    if [ -z "$first" ]; then
        if [ "$exe" = dsh ]; then
            echo "  dsh: empty --version output — hard sanity failure" >&2
            echo "  dsh needs Node >=22.19 ('import.meta.main'); an older Node makes" >&2
            echo "  its CLI exit 0 with no output, so this must fail the build." >&2
        else
            echo "  $exe: empty --version output — hard sanity failure" >&2
        fi
        exit 1
    fi
    lineno=0
    while IFS= read -r line || [ -n "$line" ]; do
        lineno=$((lineno + 1))
        [ "$lineno" -eq 1 ] && continue
        [ -z "$line" ] && continue
        echo "  $exe: unexpected --version output on line $lineno: '$line'" >&2
        exit 1
    done <"$out"
    printf '%s\n' "$first"
}

got_dsh=$(report dsh "$work/dsh.out" "$work/dsh.err")
if [ "$got_dsh" != "$want_dsh" ]; then
    die "dsh: installed $got_dsh but the manifest pins $want_dsh"
fi

got_pnpm=$(report pnpm "$work/pnpm.out" "$work/pnpm.err")
if [ "$got_pnpm" != "$base_pnpm" ]; then
    die "pnpm: installed $got_pnpm but the base pin is $base_pnpm"
fi
echo "  dsh: $got_dsh (pnpm $got_pnpm)"

# Real behaviour, not a version report. The JS probes stay bounded internally
# (PTY 10 s, subprocess 10 s, range 5 s); the outer timeout is the belt-and-braces
# bound. The Python driver owns readiness (<=60 s) and TERM->KILL escalation.
runtime_scratch=$HOME/runtime
mkdir -p "$runtime_scratch"
if ! timeout --kill-after=5 60 node "$RUNTIME_CHECK" "$PREFIX" --runtime \
    >"$work/runtime.json" 2>"$work/runtime.err"; then
    sed -n '1,20p' "$work/runtime.err" >&2
    die 'dsh: installed runtime probes failed'
fi
if ! timeout --kill-after=5 70 python3 "$PROBE" --prefix "$PREFIX" --command "$COMMAND" \
    --scratch-root "$runtime_scratch" >"$work/web.json" 2>"$work/web.err"; then
    sed -n '1,20p' "$work/web.err" >&2
    die 'dsh: credential-free web readiness failed'
fi

# Health must not have rewritten the identity assumptions it just proved.
python3 "$PROJECT" command-identity dsh "$COMMAND" ||
    die 'dsh: PATH identity changed during the health gate'
python3 "$PNPM_SOURCE/verify.py" || die 'pnpm: base identity changed during the health gate'

# Tree access covers the whole payload; a tree audit alone does not certify
# executables or the interpreter chain, so the commands are audited explicitly,
# and every native module the probes actually loaded must be readable by all.
python3 "$CONTRACT/check-tool-access.py" --tree "$PREFIX" ||
    die 'dsh: installed tree is not readable/traversable by an arbitrary uid'
python3 "$CONTRACT/check-tool-access.py" dsh pnpm ||
    die 'dsh: a command or its interpreter is not usable by an arbitrary uid'
native=$(jq -r '.native[]?' "$work/runtime.json") || die 'dsh: runtime report is unreadable'
if [ -n "$native" ]; then
    if ! printf '%s\n' "$native" | while IFS= read -r file; do
        [ -n "$file" ] || continue
        python3 "$CONTRACT/check-tool-access.py" --content "$file" || exit 1
    done; then
        die 'dsh: a loaded native module is not readable by an arbitrary uid'
    fi
fi

printf 'installed\n' >"$status"
