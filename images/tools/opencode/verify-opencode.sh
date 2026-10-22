#!/bin/sh
# Build-time gate for the opencode layer. Bind-mounted, never COPYed.
#
# Reached after install-opencode.sh: a hard failure already aborted the build,
# and a softened transport failure exited 0 having left an `absent-transport
# CODE` record and no command. The gate accepts that degraded state ONLY when the
# record is present; otherwise a missing command is a hard failure regardless of
# AGENT_INSTALL_SOFT_FAIL. On a real install it runs a bounded, status-checked
# `opencode --version`, requires the exact v2 report, audits T5, and only
# then records `installed` (selection labels are not health).
set -eu

CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
# Production prefix is the installer's $HOME; the seam matches
# install-opencode.sh.
prefix="${AGENT_VM_OPENCODE_PREFIX:-/opt/agent}"
opencode_bin="$prefix/.opencode/bin/opencode"
slot="${AGENT_VM_VERSION_OPENCODE:?verify-opencode.sh needs AGENT_VM_VERSION_OPENCODE}"
status_dir="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"
status="$status_dir/opencode"

# Path state via lstat: only a genuinely nonexistent command may be waved away
# by an absence record. A dangling symlink, a present-but-unusable file or a
# permission error on an ancestor is a contract violation, not an absence.
bin_state=0
"$CONTRACT/recipe-contract" install-status path-state "$opencode_bin" || bin_state=$?
if [ "$bin_state" -eq 0 ]; then
    if "$CONTRACT/recipe-contract" install-status code "$status" >/dev/null 2>&1; then
        echo "  opencode: ABSENT ($(cat "$status")); raw developer build only"
        exit 0
    fi
    echo "  opencode: command missing with no valid absence record -- hard failure" >&2
    exit 1
fi
if [ "$bin_state" -eq 2 ]; then
    echo "  opencode: cannot stat $opencode_bin (permission/loop) -- hard failure" >&2
    exit 1
fi

scratch=$(mktemp -d)
# shellcheck disable=SC2064
trap "rm -rf '$scratch'" EXIT INT TERM HUP
out="$scratch/out"
err="$scratch/err"

status_code=0
sh "$CONTRACT/run-report.sh" 60 "$out" "$err" "$opencode_bin" --version || status_code=$?
if [ "$status_code" -ne 0 ]; then
    echo "  opencode: --version exited $status_code; refusing to accept the report" >&2
    sed -n '1,10p' "$err" >&2
    exit 1
fi
# The report is exact bytes; reject non-printable bytes (a NUL would be erased
# by command substitution and canonicalize a corrupted report).
if [ "$(cat "$out" "$err" | LC_ALL=C tr -d '\11\12\40-\176' | wc -c | tr -d ' ')" != 0 ]; then
    echo "  opencode: --version report contains non-printable bytes -- hard failure" >&2
    exit 1
fi
# Both old and new report forms on stderr contradict the exact stdout report.
if LC_ALL=C grep -Eq '^(opencode v|v)?[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$' "$err"; then
    echo "  opencode: version report on stderr -- hard failure" >&2
    exit 1
fi
printf 'opencode %s\n' "$slot" >"$scratch/expected"
if ! cmp -s "$scratch/expected" "$out"; then
    echo "  opencode: report does not match $slot exactly" >&2
    exit 1
fi
echo "  opencode: $slot (pinned)"

"$CONTRACT/recipe-contract" tool-access opencode

mkdir -p "$status_dir"
printf 'installed\n' >"$status"
