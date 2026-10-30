#!/bin/sh
# Build-time gate for the agy layer. Bind-mounted, never COPYed.
#
# The transcript is the contract: `agy --version` must print exactly the pinned
# version and one newline on stdout, nothing on stderr, and exit 0. A version
# string that merely contains the expectation, an extra line, a warning on stderr
# or a nonzero exit is a hard failure, because agy keeps running when the install
# is subtly wrong.
#
# The switch check is why this is a gate rather than a smoke test: the image turns
# self-update off with AGY_CLI_DISABLE_AUTO_UPDATE, which only works while the
# release still reads that variable. A release that renames it would ship a binary
# that silently updates itself in a guest, so a missing string fails the bump.
set -eu

CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
agy_bin="${AGENT_VM_AGY_BIN:-/usr/local/bin/agy}"
want="${AGENT_VM_VERSION_AGY:?verify-agy.sh needs AGENT_VM_VERSION_AGY}"
status_dir="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"

if [ -z "$want" ]; then
    echo "  agy: the layer supplied no version to verify against" >&2
    exit 1
fi
if [ "${AGY_CLI_DISABLE_AUTO_UPDATE:-}" != 1 ]; then
    echo "  agy: AGY_CLI_DISABLE_AUTO_UPDATE is not set to 1 in the build environment" >&2
    exit 1
fi
if ! LC_ALL=C grep -q -a -F AGY_CLI_DISABLE_AUTO_UPDATE "$agy_bin"; then
    echo "  agy: $agy_bin no longer names AGY_CLI_DISABLE_AUTO_UPDATE; the pinned" >&2
    echo "  agy: release would self-update in a guest -- review the new release" >&2
    exit 1
fi

# Every guest UID runs this binary: the reviewed archive's member is mode 0755, and
# a group- or world-writable /usr/local/bin/agy would let one guest replace what
# another executes. The shared installer refuses an archive member that is not
# exactly 0755 — the mode is part of the reviewed artifact's shape, not something
# the digest alone decides — so this asserts the published result instead of a
# second rule. AGENT_VM_AGY_EXPECTED_OWNER exists because the black-box suite
# cannot create a root-owned file; production never sets it.
expected_owner="${AGENT_VM_AGY_EXPECTED_OWNER:-0:0}"
if ! shape=$(stat -c '%a %u:%g' "$agy_bin" 2>/dev/null || stat -f '%Lp %u:%g' "$agy_bin"); then
    echo "  agy: cannot read the mode and owner of $agy_bin" >&2
    exit 1
fi
if [ "$shape" != "755 $expected_owner" ]; then
    echo "  agy: $agy_bin is '$shape', want '755 $expected_owner'" >&2
    exit 1
fi

scratch=$(mktemp -d)
# shellcheck disable=SC2064
trap "rm -rf '$scratch'" EXIT INT TERM HUP
out="$scratch/out"
err="$scratch/err"

status=0
sh "$CONTRACT/run-report.sh" 60 "$out" "$err" "$agy_bin" --version || status=$?
if [ "$status" -ne 0 ]; then
    echo "  agy: --version exited $status; refusing to accept the report" >&2
    sed -n '1,10p' "$err" >&2
    exit 1
fi

# Exact bytes: the version and one trailing newline, and nothing on stderr. A
# trimmed comparison would accept a second line or a warning beside a correct one.
printf '%s\n' "$want" >"$scratch/want"
if ! cmp -s "$out" "$scratch/want"; then
    echo "  agy: --version did not report exactly '$want'" >&2
    sed -n '1,10p' "$out" >&2
    exit 1
fi
if [ -s "$err" ]; then
    echo "  agy: --version wrote to stderr" >&2
    sed -n '1,10p' "$err" >&2
    exit 1
fi

"$CONTRACT/recipe-contract" tool-access agy

mkdir -p "$status_dir"
printf 'installed\n' >"$status_dir/agy"
