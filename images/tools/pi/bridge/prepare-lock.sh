#!/bin/sh
# Prepare the pi-claude-bridge project's four reviewed inputs for an explicit
# version slot.
#
# Usage:
#   prepare-lock.sh PREFIX BRIDGE_VERSION [--refresh-lock]
#
# BRIDGE_VERSION is either an exact version or empty. Empty means "keep the
# committed selection"; a value equal to it is treated the same way, and with no
# `--refresh-lock` that request starts ZERO package managers and writes nothing.
#
# The two developer/maintenance shapes are deliberately different, and the owning
# driver (images/tools/pi/prepare-project.py) owns both:
#   * an ordinary build with a changed selection starts FRESH from the exact
#     manifest plus the reviewed workspace -- never from the old lock;
#   * an explicit `--refresh-lock` seeds the EXISTING pnpm lock, including for an
#     equal selection, so incremental maintenance stays incremental.
# There is no npm path and no `--legacy-peer-deps`/`--no-optional` translation:
# the reviewed hoisted workspace keeps the Claude Agent SDK JavaScript and omits
# only its platform packages through `ignoredOptionalDependencies`, and the Go
# helper rejects any resolved @earendil-works/typebox identity.
#
# This file is only the stable public interface (and argument grammar); the
# driver validates the candidate before pinning, runs a cold frozen validation
# and publishes exactly four inputs transactionally. Status: 0 prepared/equal,
# 1 handled failure, 2 usage (a repeated `--refresh-lock` is usage), 3 incomplete
# recovery, 128+N a signal.
set -eu

usage() {
    echo "usage: prepare-lock.sh PREFIX BRIDGE_VERSION [--refresh-lock]" >&2
}

if [ "$#" -lt 2 ]; then
    usage
    exit 2
fi

PREFIX=$1
BRIDGE_VERSION=$2
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
PREPARE=${AGENT_VM_PI_PREPARE_PROJECT:-$SELF_DIR/../prepare-project.py}
if [ ! -f "$PREPARE" ]; then
    echo "prepare-lock.sh: the owning preparation driver is missing at $PREPARE" >&2
    exit 1
fi

if [ "$refresh" -eq 1 ]; then
    set -- --consumer bridge prepare "$PREFIX" "$BRIDGE_VERSION" --refresh-lock
else
    set -- --consumer bridge prepare "$PREFIX" "$BRIDGE_VERSION"
fi

status=0
python3 "$PREPARE" "$@" || status=$?
case $status in
    0 | 3 | 130 | 143 | 129) exit "$status" ;;
    *) exit 1 ;;
esac
