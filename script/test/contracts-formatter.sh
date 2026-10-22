#!/usr/bin/env bash
# Fault controls for the formatter gate in script/test/contracts.sh.
#
# The defect these guard: a negated status capture (`test -z "$(gofmt -l .)"`,
# `[ -x ... ] || exit`) passes when the formatter is missing, when it exits
# non-zero with empty stdout, or when a stale gofmt earlier on PATH reports
# differently. Each control runs contracts.sh itself with the prerequisites it
# checks before the gate stubbed, so a failing formatter must stop the run before
# any behaviour suite starts, in the default and the --guard-only mode alike.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/contracts-formatter.XXXXXX")"
trap 'rm -rf "$work"' EXIT
lint_version=$(<"$ROOT/images/recipe-contract/golangci-lint.version")
shim="$work/bin"
mkdir "$shim"
# Only the tools contracts.sh inspects before the gate are stubbed; the rest of
# the prerequisites (jq, node, npm, python3, cargo, dash) come from PATH.
printf '#!/bin/sh\nprintf "v1.7.7\\n"\n' >"$shim/actionlint"
printf '#!/bin/sh\nprintf "version: 0.11.0\\n"\n' >"$shim/shellcheck"
printf '#!/bin/sh\nprintf "timeout (GNU coreutils) 9.7\\n"\n' >"$shim/timeout"
printf '#!/bin/sh\nprintf "golangci-lint has version %s built with go1.27.3\\n"\n' "$lint_version" >"$shim/golangci-lint"
# A gofmt on PATH that would be selected if the gate trusted PATH instead of the
# module's own GOROOT: it fails and reports, so a leaked PATH lookup is visible.
printf '#!/bin/sh\nprintf "SHOULD-NOT-RUN\\n"\nexit 9\n' >"$shim/gofmt"
cat >"$shim/go" <<'SHIM'
#!/bin/sh
printf '%s\n' "$*" >>"$GO_LOG"
if [ "${1:-}" = env ] && [ "${2:-}" = GOROOT ]; then
    [ -z "${FAKE_GO_ENV_STATUS:-}" ] || exit "$FAKE_GO_ENV_STATUS"
    [ -n "${FAKE_GOROOT:-}" ] || exit 1
    printf '%s\n' "$FAKE_GOROOT"
    exit 0
fi
# The gate is the last thing before these; failing here bounds a passing run.
case "${1:-}" in
    test) [ "$*" = 'test ./internal/buildcheck' ] && exit 0 ;;
    vet) printf 'stop-at-vet\n' >&2; exit 4 ;;
esac
exit 99
SHIM
chmod 0755 "$shim"/*
goroot="$work/goroot"
mkdir -p "$goroot/bin"
# A clean formatter for the selected GOROOT; cases replace it.
printf '#!/bin/sh\nexit 0\n' >"$goroot/bin/gofmt"
chmod 0755 "$goroot/bin/gofmt"

fail() { echo "FAIL: $*" >&2; exit 1; }

# contract_run MODE [VAR=VALUE...]: runs contracts.sh with the stubbed
# prerequisites and records its status/output.
contract_run() {
    mode=$1
    shift
    contract_status=0
    : >"$work/go-log"
    if [ "$mode" = default ]; then
        env PATH="$shim:$PATH" GO_LOG="$work/go-log" FAKE_GOROOT="$goroot" "$@" \
            bash "$ROOT/script/test/contracts.sh" >"$work/stdout" 2>"$work/stderr" || contract_status=$?
    else
        env PATH="$shim:$PATH" GO_LOG="$work/go-log" FAKE_GOROOT="$goroot" "$@" \
            bash "$ROOT/script/test/contracts.sh" --guard-only >"$work/stdout" 2>"$work/stderr" || contract_status=$?
    fi
    contract_go_calls=$(cat "$work/go-log")
}

expect_failure() {  # MODE EXPECTED_STATUS STDERR_NEEDLE
    contract_run "$1" "${@:4}"
    [ "$contract_status" = "$2" ] || fail "$1/$3: status $contract_status, want $2: $(cat "$work/stderr")"
    grep -qF -- "$3" "$work/stderr" || fail "$1/$3: missing '$3' in: $(cat "$work/stderr")"
    case "$contract_go_calls" in
        *test*) fail "$1/$3: a behaviour suite started before the gate failed" ;;
    esac
}

for mode in default guard-only; do
    # 1. No formatter in the selected GOROOT.
    rm -f "$goroot/bin/gofmt"
    expect_failure "$mode" 1 "missing formatter: $goroot/bin/gofmt"
    printf '#!/bin/sh\nexit 0\n' >"$goroot/bin/gofmt"
    chmod 0755 "$goroot/bin/gofmt"

    # 2. The formatter exits non-zero with empty stdout: the exit status, not the
    # empty output, has to decide.
    printf '#!/bin/sh\nexit 7\n' >"$goroot/bin/gofmt"
    chmod 0755 "$goroot/bin/gofmt"
    expect_failure "$mode" 7 "formatter failed: $goroot/bin/gofmt (status 7)"
    printf '#!/bin/sh\nexit 0\n' >"$goroot/bin/gofmt"
    chmod 0755 "$goroot/bin/gofmt"

    # 3. The formatter names a file that is not formatted.
    printf '#!/bin/sh\nprintf "internal/cli/unformatted.go\\n"\n' >"$goroot/bin/gofmt"
    chmod 0755 "$goroot/bin/gofmt"
    expect_failure "$mode" 1 "internal/cli/unformatted.go"
    printf '#!/bin/sh\nexit 0\n' >"$goroot/bin/gofmt"
    chmod 0755 "$goroot/bin/gofmt"

    # 4. `go env GOROOT` itself fails: the failure is reported with its own status
    # rather than being read as "no formatter output".
    expect_failure "$mode" 3 "cannot select Go formatter toolchain" FAKE_GO_ENV_STATUS=3

    # 5. A failing gofmt earlier on PATH is ignored: the selected GOROOT's clean
    # formatter decides, and the run continues to the next gate (`go vet`).
    contract_run "$mode"
    [ "$contract_status" = 4 ] || fail "$mode/wrong-path: status $contract_status, want 4: $(cat "$work/stderr")"
    grep -qF 'stop-at-vet' "$work/stderr" || fail "$mode/wrong-path: never reached go vet: $(cat "$work/stderr")"
    case "$(cat "$work/stderr")" in
        *SHOULD-NOT-RUN*|*'formatter failed'*|*'missing formatter'*) fail "$mode/wrong-path: used the PATH gofmt" ;;
    esac
    grep -qx 'vet ./...' "$work/go-log" || fail "$mode/wrong-path: go log: $(cat "$work/go-log")"
    echo "formatter gate control $mode passed"
done
echo 'contracts formatter gate controls passed'
