#!/bin/sh
# Stand-in pnpm for the Y-series policy controls (#41).
#
# The controls must tell "the helper refused to call a manager" (zero calls)
# apart from "the reviewed manager refused the request" (a call happened), so
# this script stands on PATH in the derived image (sentinel.Dockerfile) and
# records every call before acting on /agent-vm-sentinel/mode. The real manager
# is one directory away under the same basename, because #40's launcher
# dispatches on ${0##*/}.
set -eu

SENTINEL=/agent-vm-sentinel
REAL_MANAGER=/usr/local/lib/agent-vm-test/real/pnpm

# The filter keeps exactly the inputs whose provenance the helper controls:
# package-manager configuration, HOME/XDG/TMPDIR and PATH (and NODE_*, which can
# carry runtime overrides). env.PWD records where the helper made the call. The
# record goes to a file, never to stdout: the helper reads pnpm's --version output.
log_call() {
    jq -cn --arg argv0 "$0" '{argv0: $argv0, argv: $ARGS.positional, cwd: env.PWD, env: ($ENV | with_entries(select(.key | ascii_downcase | test("^(npm_config_|pnpm_config_|corepack_|pnpm_home$|node_|home$|xdg_|path$|tmpdir$)"))))}' --args -- "$@" >> "$SENTINEL/calls.jsonl"
}

[ -f "$SENTINEL/mode" ] || { echo 'pnpm-sentinel: no mode file at /agent-vm-sentinel/mode' >&2; exit 96; }
mode=$(cat "$SENTINEL/mode")

# The lifecycle hook modes need the helper's --ignore-scripts gone; they run the
# real manager without `exec`, because the reviewed package.json must be back in
# place before the helper's install-time verification reads the pinned bytes.
case "$mode" in
    hooks-enabled | hooks-no-flag)
        kept=''
        for arg in "$@"; do
            [ "$arg" = --ignore-scripts ] || kept="$kept$arg
"
        done
        old_ifs=$IFS
        IFS='
'
        set -f
        # Deliberate word splitting: the helper's install argv is a fixed flag
        # list plus a mktemp store directory, so no argument holds a newline.
        set -- $kept
        set +f
        IFS=$old_ifs
        ;;
esac

log_call "$@"

case "$mode" in
    fail) exit 97 ;;
    pass) exec "$REAL_MANAGER" "$@" ;;
    version-12)
        [ "${1:-}" = --version ] || { echo 'pnpm-sentinel: version-12 answers only --version' >&2; exit 97; }
        echo 12.9.1
        ;;
    age-tighten)
        [ "${1:-}" = install ] || exec "$REAL_MANAGER" "$@"
        # One hundred years: every published version is inside the cutoff, so the
        # reviewed manager itself must refuse the committed lock (exp. 9).
        PNPM_CONFIG_MINIMUM_RELEASE_AGE=52560000
        export PNPM_CONFIG_MINIMUM_RELEASE_AGE
        exec "$REAL_MANAGER" "$@"
        ;;
    weaken)
        # The values pnpm records when the five exports are absent: proves the
        # installed-tree policy record is read from pnpm, not from a constant.
        PNPM_CONFIG_MINIMUM_RELEASE_AGE=0
        PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=false
        PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=true
        PNPM_CONFIG_TRUST_POLICY=off
        export PNPM_CONFIG_MINIMUM_RELEASE_AGE PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT \
            PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME PNPM_CONFIG_TRUST_POLICY
        exec "$REAL_MANAGER" "$@"
        ;;
    hooks | hooks-enabled | hooks-no-flag)
        [ "${1:-}" = install ] || exec "$REAL_MANAGER" "$@"
        if [ "$mode" = hooks-enabled ]; then
            PNPM_CONFIG_IGNORE_SCRIPTS=false
            export PNPM_CONFIG_IGNORE_SCRIPTS
        fi
        backup=$SENTINEL/package.json.reviewed
        cp -p package.json "$backup"
        jq '.scripts = {
                preinstall: "touch /agent-vm-sentinel/hook-preinstall",
                postinstall: "touch /agent-vm-sentinel/hook-postinstall",
                prepare: "touch /agent-vm-sentinel/hook-prepare"
            }' "$backup" > package.json
        status=0
        "$REAL_MANAGER" "$@" || status=$?
        cp -p "$backup" package.json
        exit "$status"
        ;;
    *)
        echo "pnpm-sentinel: unknown mode: $mode" >&2
        exit 95
        ;;
esac
