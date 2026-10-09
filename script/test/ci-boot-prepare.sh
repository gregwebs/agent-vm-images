#!/usr/bin/env bash
# Offline controls for prepare-ci-boot.sh: the mutating path must be unreachable
# unless this is a disposable hosted Linux amd64 runner. Every command that can
# change the host (sudo, and the reclaim's timeout) is a PATH stub that records
# the fact it was reached, so "no mutation before the guards" is observable
# without sudo, udev or /dev/kvm. The same script is then driven down the
# permitted path to prove the sentinel is live, making the guard the only
# difference between refused and mutating.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/bin"
report="$work/mutations"
# Reached-the-mutation sentinel. The reclaim is invoked as
# `timeout … sudo rm -rf …`, so recording the `timeout` stub is enough to prove
# the line was reached; it exits nonzero so nothing after it (udev, the /dev/kvm
# poll) runs and no host command can be attempted.
cat > "$work/bin/timeout" <<SH
#!/usr/bin/env bash
printf '%s\n' timeout >> "$report"
exit 1
SH
cat > "$work/bin/sudo" <<SH
#!/usr/bin/env bash
printf '%s\n' sudo >> "$report"
exit 0
SH
cat > "$work/bin/python3" <<SH
#!/usr/bin/env bash
printf '%s\n' python3 >> "$report"
exit 0
SH
cat > "$work/bin/uname" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "${FAKE_ARCH:-x86_64}"
SH
chmod +x "$work/bin/"*
export PATH="$work/bin:$PATH"
fail() { echo "prepare regression: $*" >&2; exit 1; }

# run NAME EXPECTED_PREFIX [SCRIPT_ARGS…]; sets $status, $log and $mutations.
run() {
    local name=$1 prefix=$2; shift 2
    local case_dir="$work/$name"
    mkdir -p "$case_dir"
    rm -f "$report"
    status=0
    (
        export GITHUB_ACTIONS GITHUB_REPOSITORY RUNNER_OS RUNNER_TEMP="$case_dir/tmp" FAKE_ARCH
        mkdir -p "$RUNNER_TEMP"
        bash "$ROOT/script/release/prepare-ci-boot.sh" "$@"
    ) > "$case_dir/log" 2>&1 || status=$?
    log="$case_dir/log"
    mutations=$(cat "$report" 2>/dev/null || true)
    [ -z "$prefix" ] || grep -Fq -- "$prefix" "$log" || fail "$name missing diagnostic '$prefix'"
}

# --- refusals: correct diagnostic, nonzero exit, nothing that mutates reached --
for case in unset-actions false-actions non-linux; do
    case "$case" in
        unset-actions) unset GITHUB_ACTIONS; RUNNER_OS=Linux; FAKE_ARCH=x86_64 ;;
        false-actions) GITHUB_ACTIONS=false; RUNNER_OS=Linux; FAKE_ARCH=x86_64 ;;
        non-linux)     GITHUB_ACTIONS=true;  RUNNER_OS=Windows; FAKE_ARCH=x86_64 ;;
    esac
    run "$case" 'disposable Linux Actions runner only' "$work/$case/logs"
    [ "$status" != 0 ] || fail "$case accepted a non-disposable host"
    [ -z "$mutations" ] || fail "$case mutated the host before the guard: $mutations"
    echo "prepare case $case passed"
done

GITHUB_ACTIONS=true RUNNER_OS=Linux FAKE_ARCH=aarch64
run arm64 'amd64-only' "$work/arm64/logs"
[ "$status" != 0 ] || fail 'arm64 accepted'
[ -z "$mutations" ] || fail "arm64 mutated the host before the guard: $mutations"
echo 'prepare case arm64 passed'

# --- usage: refused before any directory is created ---------------------------
GITHUB_ACTIONS=true RUNNER_OS=Linux FAKE_ARCH=x86_64
run no-args 'usage:'                          # LOG_DIR is required
[ "$status" != 0 ] || fail 'missing LOG_DIR accepted'
run two-args 'usage:' "$work/two-args/a" "$work/two-args/b"
[ "$status" != 0 ] || fail 'extra arguments accepted'
[ -z "$mutations" ] || fail "usage error mutated the host: $mutations"
[ ! -e "$work/two-args/a" ] && [ ! -e "$work/two-args/b" ] || fail 'usage error created LOG_DIRs'
echo 'prepare case usage passed'

# --- liveness: the permitted path does reach the reclaim line -----------------
# Same guards and one argument as `no-args`; only the architecture differs from
# `arm64`. A nonempty sentinel here proves the sentinel is live and that the
# guards above are the only thing standing between a caller and the host mutation.
GITHUB_ACTIONS=true RUNNER_OS=Linux FAKE_ARCH=x86_64
run allowed '' "$work/allowed/logs"
[ "$mutations" = timeout ] || fail "permitted amd64 path did not reach the reclaim: '$mutations'"
[ -f "$work/allowed/logs/kvm-before.log" ] || fail 'missing pre-reclaim KVM probe log'
[ -f "$work/allowed/logs/capacity-before.log" ] || fail 'missing pre-reclaim capacity log'
echo 'prepare case allowed passed'

echo 'ci boot prepare controls passed'
