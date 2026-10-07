#!/usr/bin/env bash
# Exit 0 iff the current buildx builder is the daemon-backed `docker` driver.
#
# Written as a shared guard because the previous inline form
#   docker buildx inspect "$BUILDER" | grep -Eq '...' || fail
# raced under `set -o pipefail`: `grep -q` exits on the first match, which can
# SIGPIPE the still-writing producer, making the pipeline report 141 and fail
# the job sporadically. Read the whole stream (no -q) instead, and retry a few
# times to absorb genuinely transient runner state. Callers print their own
# message.
set -euo pipefail
attempts=5
for attempt in $(seq 1 "$attempts"); do
    builder=$(docker context show)
    if docker buildx inspect "$builder" 2>/dev/null | grep -E '^Driver:[[:space:]]+docker$' >/dev/null; then
        exit 0
    fi
    [ "$attempt" -eq "$attempts" ] || sleep 2
done
exit 1
