#!/bin/sh
# Prepare the Pi project's four reviewed inputs for an explicit version slot.
#
# Usage:
#   prepare-lock.sh PREFIX PI_VERSION [--refresh-lock]
#
# PI_VERSION is either an exact version or empty. Empty means "keep the committed
# selection"; a value equal to it is treated the same way, and with no
# `--refresh-lock` that request starts ZERO package managers (not even a
# `--version` or a metadata query) and writes nothing: the owning driver re-reads
# the committed bytes and the reviewed graph through the ordinary pinned path.
#
# Everything else is the owning driver's business (images/tools/pi/prepare-project.py):
# a changed Pi whose selected archive carries npm-shrinkwrap.json is converted
# from that authenticated archive, a changed Pi without one is resolved as an
# ordinary all-hashed graph, and `--refresh-lock` deliberately re-resolves the
# equal selection for developer maintenance. The driver validates the candidate
# against this prefix with the Go helper before pinning, runs a cold frozen
# validation, and publishes exactly four inputs transactionally. This file is
# only the stable public interface (and argument grammar) for that driver, so no
# caller can reorder "compare, then pin" or skip the validation.
#
# PREFIX is the production installation prefix, or -- only in tests -- a
# canonical child of AGENT_VM_FIXTURE_ROOT. Status: 0 prepared/equal, 1 handled
# failure, 2 usage (a repeated `--refresh-lock` is usage), 3 incomplete
# recovery, 128+N a signal.
set -eu

usage() {
    echo "usage: prepare-lock.sh PREFIX PI_VERSION [--refresh-lock]" >&2
}

if [ "$#" -lt 2 ]; then
    usage
    exit 2
fi

PREFIX=$1
PI_VERSION=$2
shift 2

refresh=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --refresh-lock)
            if [ "$refresh" -ne 0 ]; then
                usage
                exit 2
            fi
            refresh=1
            ;;
        *)
            usage
            exit 2
            ;;
    esac
    shift
done

SELF_DIR=$(cd "$(dirname "$0")" && pwd -P)
PREPARE=${AGENT_VM_PI_PREPARE_PROJECT:-$SELF_DIR/prepare-project.py}
if [ ! -f "$PREPARE" ]; then
    echo "prepare-lock.sh: the owning preparation driver is missing at $PREPARE" >&2
    exit 1
fi

if [ "$refresh" -eq 1 ]; then
    set -- --consumer pi prepare "$PREFIX" "$PI_VERSION" --refresh-lock
else
    set -- --consumer pi prepare "$PREFIX" "$PI_VERSION"
fi

status=0
python3 "$PREPARE" "$@" || status=$?
case $status in
    0 | 3 | 130 | 143 | 129) exit "$status" ;;
    *) exit 1 ;;
esac
