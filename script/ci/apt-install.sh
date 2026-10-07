#!/usr/bin/env bash
# Install apt packages on a disposable CI host, bounded and retried.
#
# The Azure Ubuntu mirror intermittently goes unreachable, and an unbounded
# `apt-get update` then sits in an `Ign:` loop for tens of minutes until the job
# dies. Bound every attempt, let apt retry its own transient failures, and retry
# the whole sequence so a mirror blip costs one attempt instead of a runner.
set -euo pipefail
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_OS:-}" = Linux ] || {
    echo 'disposable Linux Actions runner only' >&2; exit 1; }
[ "$#" -ge 1 ] || { echo "usage: ${0##*/} PACKAGE..." >&2; exit 2; }
acquire=(-o Acquire::Retries=3 -o Acquire::http::Timeout=15 -o Acquire::https::Timeout=15)
for attempt in 1 2; do
    if timeout --kill-after=10s 180 sudo apt-get update "${acquire[@]}" \
        && timeout --kill-after=10s 240 sudo apt-get install -y "${acquire[@]}" "$@"; then
        exit 0
    fi
    echo "apt attempt $attempt failed; retrying" >&2
    sleep 10
done
echo "apt install failed after 2 attempts: $*" >&2
exit 1
