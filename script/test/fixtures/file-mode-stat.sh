#!/bin/sh
# A `stat` that answers in one dialect only, installed as `stat` on PATH so a
# suite can force the dialect its host does not answer in. FILE_MODE_STAT_SHAPE
# picks it: `bsd` makes -c fail, `gnu` makes -f print a filesystem block and then
# fail (which is what GNU stat does, because -f means --file-system), and `none`
# fails both.
#
# The mode itself always comes from the real stat named by REAL_STAT: a stub that
# could answer a mode of its own would let a suite pass a check whose read it
# never exercised.
#
# No apostrophe in a `:?` message: bash reads one as the start of a quoted word
# there, even inside double quotes.
set -eu
: "${REAL_STAT:?REAL_STAT must name the stat binary that has the real mode}"
shape=${FILE_MODE_STAT_SHAPE:?FILE_MODE_STAT_SHAPE must be bsd, gnu or none}
case "$shape" in
    bsd | gnu | none) ;;
    *)
        echo "FILE_MODE_STAT_SHAPE must be bsd, gnu or none, not '$shape'" >&2
        exit 2
        ;;
esac

host_mode() { # $1 = path; the host's mode, in whichever dialect it answers
    if "$REAL_STAT" -c '%a' "$1" >/dev/null 2>&1; then
        "$REAL_STAT" -c '%a' "$1"
    else
        "$REAL_STAT" -f '%Lp' "$1"
    fi
}

# Any other call is the host's own, so the stub stays transparent.
if [ "$#" -ne 3 ]; then
    exec "$REAL_STAT" "$@"
fi
case "$1:$2:$shape" in
    -c:%a:gnu | -f:%Lp:bsd) host_mode "$3"; exit 0 ;;
    -f:%Lp:gnu)
        printf '  File: "%s"\n' "$3"
        printf '    ID: 0000 Namelen: 255 Type: fixture\n'
        exit 1
        ;;
    -c:%a:bsd | -c:%a:none | -f:%Lp:none) exit 1 ;;
esac
exec "$REAL_STAT" "$@"
