#!/usr/bin/env bash
# Independent finished-image report/status/UID oracle; builds only disposable T5 fixtures.
set -euo pipefail

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
FIXTURE_DOCKERFILE="$REPO_ROOT/script/test/fixtures/t5-negative/Dockerfile"
# The committed status-record validator; the external oracle validates the exact
# status bytes with it rather than a first-line key/value extraction (review F2).
STATUS_VALIDATOR="$REPO_ROOT/images/recipe-contract/install-status.py"

usage() { echo "usage: $0 BASE_IMAGE STANDARD_IMAGE [--platform linux/ARCH] [--expect SUFFIX=VERSION ...] | --artifact STANDARD_IMAGE [--platform linux/ARCH] [--expect SUFFIX=VERSION ...] | --self-test | --pi-audit-status BASE_IMAGE [--platform linux/ARCH]" >&2; }
self_test=false
pi_audit=false
artifact=false
BASE_IMAGE="" STANDARD_IMAGE="" platform=""
keep=false
expect_overrides=()
if [ "${1:-}" = --self-test ]; then
    self_test=true; shift
elif [ "${1:-}" = --artifact ]; then
    artifact=true; shift
    [ "$#" -ge 1 ] || { usage; exit 2; }
    STANDARD_IMAGE=$1; shift
elif [ "${1:-}" = --pi-audit-status ]; then
    pi_audit=true; shift
    [ "$#" -ge 1 ] || { usage; exit 2; }
    BASE_IMAGE=$1; shift
else
    [ "$#" -ge 2 ] || { usage; exit 2; }
    BASE_IMAGE=$1; STANDARD_IMAGE=$2; shift 2
fi
while [ "$#" -gt 0 ]; do
    case "$1" in
        --platform) [ "$#" -ge 2 ] || { usage; exit 2; }; platform=$2; shift 2 ;;
        --expect) [ "$#" -ge 2 ] || { usage; exit 2; }; expect_overrides+=("$2"); shift 2 ;;
        *) usage; exit 2 ;;
    esac
done
if [ -z "$platform" ]; then
    case "$(uname -m)" in
        arm64 | aarch64) platform=linux/arm64 ;;
        amd64 | x86_64) platform=linux/amd64 ;;
        *) usage; exit 2 ;;
    esac
fi
case "$platform" in linux/amd64 | linux/arm64) ;; *) usage; exit 2 ;; esac

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# run_with_watchdog SECONDS CMD... -> the command's own status, or 124 on
# timeout. macOS has no guaranteed `timeout`, so host watchdogs use the shared
# Python3 process-group helper (script/test/host-watchdog.py); on timeout the
# whole group is signalled so no buildx builder, container or daemon client is
# orphaned.
run_with_watchdog() {
    python3 "$REPO_ROOT/script/test/host-watchdog.py" "$@"
}

# --- byte-exact report/status capture (review F2) ----------------------------
# The container audit emits each tool's COMPLETE stdout report and the exact
# status bytes base64-encoded, so a first-line key/value extraction can never
# hide a contradictory trailing report line or a multi-line status record.
# `b64file` runs in the container (injected via `declare -f`); `MISSING` marks
# an absent/unreadable file -- base64 output is always a multiple of four
# characters, so the sentinel can never collide with a real value.
b64file() { # $1 path -> base64 of its bytes, or MISSING
    if [ -r "$1" ]; then
        python3 -c 'import base64,sys; sys.stdout.write(base64.b64encode(open(sys.argv[1],"rb").read()).decode())' "$1"
    else
        printf 'MISSING'
    fi
}

# decode_b64 BLOB FILE -> write the decoded bytes to FILE; 1 when the blob is
# the MISSING sentinel.
decode_b64() { # $1 blob, $2 out file
    [ "$1" != MISSING ] || return 1
    printf '%s' "$1" | python3 -c 'import base64,sys; sys.stdout.buffer.write(base64.b64decode(sys.stdin.read()))' >"$2"
}

# blob -> base64 of stdin (host helpers seed the self-test with exact bytes).
blob() {
    python3 -c 'import base64,sys; sys.stdout.write(base64.b64encode(sys.stdin.buffer.read()).decode())'
}

# sblob RECORD -> base64 of `RECORD` plus its trailing newline.
sblob() {
    printf '%s\n' "$1" | blob
}

# Per-build / per-runtime-probe budgets (plan task 5). Overridable only for
# targeted testing; the production values are the plan's 20 min / 3 min.
BUILD_WATCHDOG_SECONDS="${STANDARD_IMAGE_FIXTURE_WATCHDOG:-1200}"
RUNTIME_WATCHDOG_SECONDS="${STANDARD_IMAGE_RUNTIME_WATCHDOG:-180}"

# --- the committed exact alternate selections (never `latest`) ---------------
# Sources: /tmp/agent-vm-227/{initial-pin-candidates.txt,alternate-selection-capture.json}
# during the #227 revision. The defaults come from the Dockerfiles themselves.
RUN_ID="agent-vm-standard-$$"
# `target/` is gitignored and absent on a fresh CI checkout with no Rust build,
# so create it before mktemp (which does not create parent directories).
mkdir -p "$REPO_ROOT/target"
TMP="$(mktemp -d "$REPO_ROOT/target/standard-image.XXXXXX")"

prefix_images() {
    docker images --format '{{.Repository}}:{{.Tag}}' | grep "^${RUN_ID}-" || true
}

# Every probe container is given a run-scoped name so cleanup can force-remove
# one whose `docker run` client was killed by a watchdog. `--rm` only removes a
# container after it exits, not after its client dies; the name is what makes a
# leaked container findable on a shared daemon (we never prune globally).
#
# `docker_run` is an executable wrapper, not a shell function: the Python host
# watchdog execs argv directly (script/test/host-watchdog.py), so a shell
# function would fail with ENOENT/EACCES. `$TMP/bin` is put on PATH for both the
# harness and the watchdog child.
mkdir -p "$TMP/bin"
cat >"$TMP/bin/docker_run" <<'SH'
#!/bin/sh
exec docker run --name "${RUN_ID}-probe-$$" "$@"
SH
chmod 0755 "$TMP/bin/docker_run"
export RUN_ID
PATH="$TMP/bin:$PATH"
export PATH

cleanup() {
    [ "$self_test" = false ] || { rm -rf "$TMP"; return; }
    local cids
    cids="$(docker ps -aq --filter "name=^${RUN_ID}-" 2>/dev/null || true)"
    if [ -n "$cids" ]; then
        # shellcheck disable=SC2086  # word-splitting over the id list is intended
        docker rm -f $cids >/dev/null 2>&1 || true
    fi
    if [ "$keep" = false ]; then
        if [ -n "$(prefix_images)" ]; then
            # shellcheck disable=SC2046  # word-splitting over the image list is intended
            docker rmi $(prefix_images) >/dev/null 2>&1 || true
        fi
    else
        echo "kept images matching ${RUN_ID} (--keep)" >&2
    fi
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

# --- helpers -----------------------------------------------------------------
labels_json() {
    docker image inspect "$1" --format '{{json .Config.Labels}}' 2>/dev/null || echo '{}'
}

label_value() { # $1 image, $2 full label key
    labels_json "$1" | jq -r --arg k "$2" 'if . == null then "" else (.[$k] // "") end'
}

version_keys() { # $1 image -> sorted org.agent-vm.version.* keys
    labels_json "$1" | jq -r 'if . == null then "" else . end
        | keys[] | select(startswith("org.agent-vm.version."))' | sort
}

valid_semver() {
    printf '%s' "$1" | grep -Eq \
        '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
}

recipe_keys() { # $1 recipe -> label suffixes
    case "$1" in
        codex) printf '%s\n' codex ;;
        opencode) printf '%s\n' opencode ;;
        claude) printf '%s\n' claude ;;
        copilot) printf '%s\n' copilot ;;
        dsh) printf '%s\n' dsh pnpm ;;
        pi) printf '%s\n' pi pi-claude-bridge ;;
        *) fail "unknown recipe: $1" ;;
    esac
}

suffix_arg() { # $1 label suffix -> build ARG name
    case "$1" in
        codex) printf '%s\n' AGENT_VERSION_CODEX ;;
        opencode) printf '%s\n' AGENT_VERSION_OPENCODE ;;
        claude) printf '%s\n' AGENT_VERSION_CLAUDE ;;
        copilot) printf '%s\n' AGENT_VERSION_COPILOT ;;
        dsh) printf '%s\n' AGENT_VERSION_DSH ;;
        pnpm) printf '%s\n' AGENT_VERSION_PNPM ;;
        pi) printf '%s\n' AGENT_VERSION_PI ;;
        pi-claude-bridge) printf '%s\n' AGENT_VERSION_PI_CLAUDE_BRIDGE ;;
        *) fail "unknown label suffix: $1" ;;
    esac
}

committed_arg() { sed -n "s/^ARG $1=//p" "$REPO_ROOT/images/standard/Dockerfile"; }
DEFAULT_CODEX=$(committed_arg AGENT_VERSION_CODEX)
DEFAULT_OPENCODE=$(committed_arg AGENT_VERSION_OPENCODE)
DEFAULT_CLAUDE=$(committed_arg AGENT_VERSION_CLAUDE)
DEFAULT_COPILOT=$(committed_arg AGENT_VERSION_COPILOT)
DEFAULT_DSH=$(jq -r '.dependencies["@deepseek-ai/dsh"]' "$REPO_ROOT/images/tools/dsh/package.json")
DEFAULT_PNPM=$(jq -r '.dependencies.pnpm' "$REPO_ROOT/images/tools/dsh/package.json")
DEFAULT_PI=$(jq -r '.dependencies["@earendil-works/pi-coding-agent"]' "$REPO_ROOT/images/tools/pi/package.json")
DEFAULT_BRIDGE=$(jq -r '.dependencies["pi-claude-bridge"]' "$REPO_ROOT/images/tools/pi/bridge/package.json")

default_selection() { # $1 recipe -> newline-separated suffix=value
    case "$1" in
        codex) printf '%s\n' "codex=$DEFAULT_CODEX" ;;
        opencode) printf '%s\n' "opencode=$DEFAULT_OPENCODE" ;;
        claude) printf '%s\n' "claude=$DEFAULT_CLAUDE" ;;
        copilot) printf '%s\n' "copilot=$DEFAULT_COPILOT" ;;
        dsh) printf '%s\n' "dsh=$DEFAULT_DSH" "pnpm=$DEFAULT_PNPM" ;;
        pi) printf '%s\n' "pi=$DEFAULT_PI" "pi-claude-bridge=$DEFAULT_BRIDGE" ;;
        *) fail "unknown recipe: $1" ;;
    esac
}

# assert_expected_selection IMG 'suffix=value'... -- the label must equal the
# INDEPENDENTLY-supplied selection, not merely agree with the image's report.
assert_expected_selection() { # $1 image, $2 newline-separated suffix=value
    local img="$1" lines="$2" line suffix value got reason
    while IFS= read -r line; do
        [ -n "$line" ] || continue
        suffix="${line%%=*}"
        value="${line#*=}"
        got="$(label_value "$img" "org.agent-vm.version.$suffix")"
        reason="$(expect_label "$img" "$suffix" "$value" "$got")" || fail "$reason"
    done <<<"$lines"
}

# assert_recipe_labels IMG RECIPE [EXPECTED_LABELS_FILE] [STRICT]
# Validates presence, spelling and syntax, and exact equality when given. With
# STRICT=0 (the chained image, which carries every recipe) extra labels from the
# other recipes are allowed.
assert_recipe_labels() { # $1 image, $2 recipe, $3 expected file, $4 strict
    local img="$1" recipe="$2" expected_file="${3:-}" strict="${4:-1}" keys want got suffix full value
    keys="$(recipe_keys "$recipe")"
    if [ "$strict" = 1 ]; then
        want="$(for suffix in $keys; do echo "org.agent-vm.version.$suffix"; done | sort)"
        got="$(version_keys "$img")"
        [ "$got" = "$want" ] ||
            fail "$recipe labels: expected exactly [$want], got [$(echo "$got" | tr '\n' ' ')]"
    fi
    while IFS= read -r suffix; do
        [ -n "$suffix" ] || continue
        full="org.agent-vm.version.$suffix"
        value="$(label_value "$img" "$full")"
        [ -n "$value" ] || fail "$recipe label $full is empty"
        case "$suffix" in
            codex)
                if [ "${value#rust-v}" = "$value" ] || ! valid_semver "${value#rust-v}"; then
                    fail "$recipe label $full='$value' is not rust-v<semver>"
                fi ;;
            opencode)
                if [ "${value#v}" = "$value" ] || ! valid_semver "${value#v}"; then
                    fail "$recipe label $full='$value' is not v<semver>"
                fi ;;
            *)
                if ! valid_semver "$value"; then
                    fail "$recipe label $full='$value' is not canonical semver"
                fi ;;
        esac
        if [ -n "$expected_file" ]; then
            expected="$(sed -n "s/^$suffix=//p" "$expected_file")"
            [ "$value" = "$expected" ] ||
                fail "$recipe label $full changed across replay: '$value' != '$expected'"
        fi
    done <<<"$keys"
}

# --- container audit ---------------------------------------------------------
# Emits `rcN=`, `lineN=`, `t5rc=`, `status=`, `warn=`, ... lines the harness
# asserts. Runs as an arbitrary numeric uid with no capabilities and no network.
container_script() { # $1 recipe
    # Inject the byte-exact capture helper: every report and status record is
    # emitted base64-encoded so a first-line extraction cannot hide a
    # contradictory trailing line or a multi-line record (review F2).
    printf '%s\n' "$(declare -f b64file)"
    # shellcheck disable=SC2016 # evaluated inside the container
    printf '%s\n' 'uid=$(id -u); if [ "$uid" != 0 ] && getent passwd "$uid" >/dev/null; then echo "unexpected passwd identity" >&2; exit 1; fi'

    case "$1" in
        codex)
            cat <<'EOS'
printf '=== RUN ===\n'
timeout --kill-after=5s 60 codex --version >/tmp/o 2>/tmp/e
printf 'rc0=%s\n' "$?"
printf 'report_b64=%s\n' "$(b64file /tmp/o)"
printf 'stderr_b64=%s\n' "$(b64file /tmp/e)"
printf '=== T5 ===\n'
python3 /contract/check-tool-access.py codex
printf 't5rc=%s\n' "$?"
printf '=== STATUS ===\n'
printf 'status_b64=%s\n' "$(b64file /opt/agent-vm/install-status/codex)"
EOS
            ;;
        opencode)
            cat <<'EOS'
printf '=== RUN ===\n'
timeout --kill-after=5s 60 opencode --version >/tmp/o 2>/tmp/e
printf 'rc0=%s\n' "$?"
printf 'report_b64=%s\n' "$(b64file /tmp/o)"
printf 'stderr_b64=%s\n' "$(b64file /tmp/e)"
printf '=== T5 ===\n'
python3 /contract/check-tool-access.py opencode
printf 't5rc=%s\n' "$?"
printf '=== STATUS ===\n'
printf 'status_b64=%s\n' "$(b64file /opt/agent-vm/install-status/opencode)"
EOS
            ;;
        claude)
            cat <<'EOS'
printf '=== RUN ===\n'
timeout --kill-after=5s 60 claude --version >/tmp/o 2>/tmp/e
printf 'rc0=%s\n' "$?"
printf 'report_b64=%s\n' "$(b64file /tmp/o)"
printf 'stderr_b64=%s\n' "$(b64file /tmp/e)"
printf '=== T5 ===\n'
python3 /contract/check-tool-access.py claude
printf 't5rc=%s\n' "$?"
printf '=== STATUS ===\n'
printf 'status_b64=%s\n' "$(b64file /opt/agent-vm/install-status/claude)"
EOS
            ;;
        copilot)
            cat <<'EOS'
printf '=== RUN ===\n'
timeout --kill-after=5s 60 copilot --version >/tmp/o 2>/tmp/e
printf 'rc0=%s\n' "$?"
printf 'report_b64=%s\n' "$(b64file /tmp/o)"
printf 'stderr_b64=%s\n' "$(b64file /tmp/e)"
printf '=== T5 ===\n'
python3 /contract/check-tool-access.py copilot
printf 't5rc=%s\n' "$?"
printf '=== STATUS ===\n'
printf 'status_b64=%s\n' "$(b64file /opt/agent-vm/install-status/copilot)"
EOS
            ;;
        dsh)
            cat <<'EOS'
printf '=== RUN ===\n'
timeout --kill-after=5s 60 dsh --version >/tmp/o 2>/tmp/e
printf 'rc0=%s\n' "$?"
printf 'report_b64=%s\n' "$(b64file /tmp/o)"
printf 'stderr_b64=%s\n' "$(b64file /tmp/e)"
timeout --kill-after=5s 60 pnpm --version >/tmp/o1 2>/tmp/e1
printf 'rc1=%s\n' "$?"
printf 'report1_b64=%s\n' "$(b64file /tmp/o1)"
printf 'stderr1_b64=%s\n' "$(b64file /tmp/e1)"
printf '=== T5 ===\n'
python3 /contract/check-tool-access.py dsh pnpm
printf 't5rc=%s\n' "$?"
printf '=== STATUS ===\n'
printf 'status_b64=%s\n' "$(b64file /opt/agent-vm/install-status/dsh)"
EOS
            ;;
        pi)
            # bridge_marks is the ONE raw-output parser; inject its exact bytes
            # so the container audit cannot drift from the parser oracle_self_test
            # certifies against real probe text.
            printf '%s\n' "$(declare -f bridge_marks)"
            cat <<'EOS'
printf '=== RUN ===\n'
PI_TELEMETRY=0 timeout --kill-after=5s 60 pi --version >/tmp/o 2>/tmp/e
printf 'rc0=%s\n' "$?"
printf 'report_b64=%s\n' "$(b64file /tmp/o)"
printf 'stderr_b64=%s\n' "$(b64file /tmp/e)"
printf '=== WARN ===\n'
PI_TELEMETRY=0 timeout --kill-after=5s 120 pi --mode rpc --no-session --no-approve </dev/null >/tmp/w.out 2>/tmp/w.err
printf 'warn_rc=%s\n' "$?"
printf 'warn=%s\n' "$(grep -c 'agent-vm: signing in here' /tmp/w.out || true)"
printf 'bridgever=%s\n' \
    "$(jq -r .version /opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/package.json 2>/dev/null || echo MISSING)"
printf '=== BRIDGE PROBE ===\n'
cat > /tmp/probe.js <<'PROBE'
export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    const p = ctx.modelRegistry.getProvider("claude-bridge");
    const n = p && typeof p.getModels === "function" ? p.getModels().length
            : p && Array.isArray(p.models) ? p.models.length : 0;
    ctx.ui.notify(p ? `AGENT-VM-BRIDGE-REGISTERED models=${n}` : "AGENT-VM-BRIDGE-MISSING",
                  "warning");
  });
}
PROBE
PI_TELEMETRY=0 timeout --kill-after=5s 120 pi -e /tmp/probe.js --mode rpc --no-session --no-approve </dev/null >/tmp/p.out 2>/tmp/p.err
printf 'probe_rc=%s\n' "$?"
# The registration result: bridge_marks emits exactly one line per
# registration-bearing probe output line (or an AGENT-VM-BRIDGE-MALFORMED
# sentinel for a malformed one) and NEVER truncates or drops a marker, so a
# positive contradicted by a missing/malformed one is visible as probes>1 and
# `models=12"garbage` does not survive as `models=12`. The parser is defined once
# in bridge_marks() (host) and injected above.
bridge_marks </tmp/p.out >/tmp/p.marks
printf 'probe=%s\n' "$(head -n 1 /tmp/p.marks)"
printf 'probes=%s\n' "$(wc -l </tmp/p.marks | tr -d ' ')"
printf '=== T5 ===\n'
# The audit shell runs without `set -e`. Capture BOTH access checks separately
# and combine them: the content check must not overwrite a failed Pi/seed-hook
# access check (review MAJOR: a failed all-uid access audit reported t5rc=0).
t5_pi_seed=0
python3 /contract/check-tool-access.py pi /opt/agent-vm/seed.d/20-pi-claude-bridge || t5_pi_seed=$?
t5_pi_content=0
python3 /contract/check-tool-access.py --content /opt/agent-vm/pi-extensions/guest-credential-warning.js /opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src/index.ts || t5_pi_content=$?
t5rc=0
[ "$t5_pi_seed" -eq 0 ] || t5rc=1
[ "$t5_pi_content" -eq 0 ] || t5rc=1
printf 't5rc=%s\n' "$t5rc"
printf '=== STATUS ===\n'
printf 'status_b64=%s\n' "$(b64file /opt/agent-vm/install-status/pi)"
printf 'bridgestatus_b64=%s\n' "$(b64file /opt/agent-vm/install-status/pi-claude-bridge)"
EOS
            ;;
        *) fail "unknown recipe: $1" ;;
    esac
}

# Regression for the generated Pi audit's status propagation (review MAJOR).
# The audit shell runs WITHOUT `set -e`, so the first two access checks (the Pi
# command and the seed hook) and the extension content check must each make the
# emitted t5rc nonzero: the content check must never overwrite a failed first
# check. This executes the REAL generated script (container_script pi) against a
# controlled fixture mounted over /opt/agent-vm, not a seeded t5rc literal.
pi_audit_status_regression() {
    local fixture="$TMP/pi-audit-fixture" generated
    mkdir -p "$fixture/opt/agent-vm/seed.d" \
        "$fixture/opt/agent-vm/pi-extensions" \
        "$fixture/opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src" \
        "$fixture/opt/agent-vm/install-status" "$fixture/bin"
    printf '#!/bin/sh\necho pi\n' >"$fixture/bin/pi"
    printf '#!/bin/sh\necho seed\n' >"$fixture/opt/agent-vm/seed.d/20-pi-claude-bridge"
    printf 'export default function () {}\n' >"$fixture/opt/agent-vm/pi-extensions/guest-credential-warning.js"
    printf 'export const bridge = true\n' >"$fixture/opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src/index.ts"
    printf 'installed\n' >"$fixture/opt/agent-vm/install-status/pi"
    printf 'installed\n' >"$fixture/opt/agent-vm/install-status/pi-claude-bridge"
    chmod 0755 "$fixture/bin/pi" "$fixture/opt/agent-vm/seed.d/20-pi-claude-bridge"
    chmod 0644 "$fixture/opt/agent-vm/pi-extensions/guest-credential-warning.js" \
        "$fixture/opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src/index.ts"
    generated="$(container_script pi)"

    expect_t5rc() { # $1 label, $2 expected t5rc
        local label="$1" want="$2" rc=0 out got
        out="$(run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
            docker_run --rm --platform "$platform" --user 12345:23456 \
            --cap-drop ALL --network none --tmpfs /tmp:rw,exec -e HOME=/tmp \
            -v "$fixture/opt/agent-vm:/opt/agent-vm:ro" \
            -v "$fixture/bin:/fixture-bin:ro" \
            -v "$REPO_ROOT/images/recipe-contract:/contract:ro" \
            -e "PATH=/fixture-bin:/usr/bin:/bin" \
            --entrypoint sh "$BASE_IMAGE" -c "$generated" 2>&1)" || rc=$?
        [ "$rc" -ne 124 ] || fail "pi audit regression ($label) timed out after ${RUNTIME_WATCHDOG_SECONDS}s"
        got="$(kv "$out" t5rc)"
        [ -n "$got" ] ||
            fail "pi audit regression ($label): generated audit emitted no t5rc: $(printf '%s' "$out" | head -c 400)"
        [ "$got" = "$want" ] ||
            fail "pi audit regression ($label): t5rc=$got, want $want: $(printf '%s' "$out" | head -c 400)"
        echo "  pi audit regression: $label -> t5rc=$got"
    }

    # Positive control: a healthy fixture passes BOTH access checks.
    expect_t5rc healthy 0
    # First check (Pi command + seed hook) fails, the content check still passes.
    chmod 0700 "$fixture/bin/pi"
    expect_t5rc pi-command-denied 1
    chmod 0755 "$fixture/bin/pi"
    chmod 0700 "$fixture/opt/agent-vm/seed.d/20-pi-claude-bridge"
    expect_t5rc seed-hook-denied 1
    chmod 0755 "$fixture/opt/agent-vm/seed.d/20-pi-claude-bridge"
    # The extension content check fails independently of a healthy first check.
    chmod 0600 "$fixture/opt/agent-vm/pi-extensions/guest-credential-warning.js"
    expect_t5rc content-denied 1
    chmod 0644 "$fixture/opt/agent-vm/pi-extensions/guest-credential-warning.js"
    echo '  pi audit status: first-check failure is not overwritten by a passing content check'
}

# run_container IMG RECIPE UID -> stdout+stderr of the audit script
run_container() { # $1 image, $2 recipe, $3 uid:gid
    local img="$1" recipe="$2" uid="$3" rc=0
    run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
        docker_run --rm --platform "$platform" --user "$uid" \
        --cap-drop ALL --network none --tmpfs /tmp:rw,exec \
        -e HOME=/tmp \
        -v "$REPO_ROOT/images/recipe-contract:/contract:ro" \
        --entrypoint sh "$img" -c "$(container_script "$recipe")" 2>&1 || rc=$?
    [ "$rc" -eq 0 ] || fail "$recipe ($uid) runtime capture failed: $rc"
}

kv() { # $1 output, $2 key -> first matching value
    printf '%s\n' "$1" | sed -n "s/^$2=//p" | head -n 1
}

# --- oracle predicates -------------------------------------------------------
# These are the value-level verdicts the acceptance oracle applies. They are
# deliberately pure (no docker) so `--self-test` can seed every rejected state
# and prove the oracle REPORTS the failure, not merely that the assertion text
# exists. Each prints a one-line reason on stdout and returns 1 on rejection, so
# a caller surfaces it with `reason="$(predicate ...)" || fail "$reason"`.

# exact_report FILE EXPECTED -- the COMPLETE report must be exactly EXPECTED,
# optionally followed by one trailing newline. A contradictory trailing line, a
# second line, or extra blank lines are rejected (review F2).
exact_report() { # $1 file, $2 expected single line (no newline)
    python3 - "$1" "$2" <<'PY'
import sys

data = open(sys.argv[1], "rb").read()
expected = sys.argv[2].encode()
sys.exit(0 if data in (expected, expected + b"\n") else 1)
PY
}

# copilot_report FILE VERSION -- copilot's genuine --version output is the
# banner followed by the documented update footer; the owning gate accepts the
# footer on every line after the banner. Mirror that exact record rather than
# demanding a single line, while still rejecting any other extra line (F2).
copilot_report() { # $1 file, $2 installed version (no prefix)
    python3 - "$1" "$2" <<'PY'
import sys

data = open(sys.argv[1], "rb").read()
version = sys.argv[2].encode()
# Reject non-printable bytes: a NUL would be erased by command substitution.
if any(not (b in (9, 10) or 32 <= b <= 126) for b in data):
    sys.exit(1)
lines = data.split(b"\n")
if lines and lines[-1] == b"":
    lines.pop()
banner = b"GitHub Copilot CLI " + version + b"."
footer = b"Run 'copilot update' to check for updates."
if not lines or lines[0] != banner:
    sys.exit(1)
sys.exit(0 if all(line in (b"", footer) for line in lines[1:]) else 1)
PY
}

# check_report RECIPE LABEL0 LABEL1 CONTAINER_OUTPUT -- the COMPLETE stdout
# report, decoded from its base64 capture, must be exactly each tool's allowed
# record; the whole report is validated, never just its first line (review F2).
# Preserve harmless diagnostic stderr, but never certify a contradictory banner
# there or normalize away nonprintable bytes. Independent of owning verifiers.
check_stderr() {
    python3 - "$1" "$2" <<'PYSTDERR'
import re, sys
name, path = sys.argv[1:]
data = open(path, "rb").read()
if any(not (b in (9, 10) or 32 <= b <= 126) for b in data):
    sys.exit(1)
patterns = {
    "codex": rb"codex-cli [0-9A-Za-z.+-]+",
    "claude": rb"[0-9][0-9A-Za-z.+-]* \(Claude Code\)",
    "copilot": rb"GitHub Copilot CLI .*",
}
pattern = patterns.get(name, rb"v?[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*")
sys.exit(1 if any(re.fullmatch(pattern, line) for line in data.splitlines()) else 0)
PYSTDERR
}

check_report() { # $1 recipe, $2 label0 value, $3 label1 value, $4 output
    local recipe="$1" v0="$2" v1="$3" out="$4" expected file
    file="$TMP/report-stderr.$$"
    decode_b64 "$(kv "$out" stderr_b64)" "$file" || { printf 'stderr capture missing'; return 1; }
    check_stderr "$recipe" "$file" || { printf '%s version report has contradictory stderr' "$recipe"; return 1; }
    file="$TMP/report-check.$$"
    decode_b64 "$(kv "$out" report_b64)" "$file" ||
        { printf '%s report is missing' "$recipe"; return 1; }
    if [ "$recipe" = copilot ]; then
        copilot_report "$file" "$v0" || {
            printf '%s report is not the exact copilot record for %s: %s' \
                "$recipe" "$v0" "$(tr -d '\0' <"$file" | head -c 200)"
            return 1
        }
    else
        case "$recipe" in
            codex) expected="codex-cli ${v0#rust-v}" ;;
            opencode) expected="${v0#v}" ;;
            claude) expected="$v0 (Claude Code)" ;;
            dsh | pi) expected="$v0" ;;
            *) printf 'unknown recipe %s' "$recipe"; return 1 ;;
        esac
        exact_report "$file" "$expected" || {
            printf '%s report is not the exact record "%s": %s' \
                "$recipe" "$expected" "$(tr -d '\0' <"$file" | head -c 200)"
            return 1
        }
    fi
    case "$recipe" in
        dsh)
            [ "$(kv "$out" rc1)" = 0 ] || { printf 'dsh pnpm exited %s' "$(kv "$out" rc1)"; return 1; }
            file="$TMP/report-pnpm-stderr.$$"
            decode_b64 "$(kv "$out" stderr1_b64)" "$file" || { printf 'pnpm stderr capture missing'; return 1; }
            check_stderr pnpm "$file" || { printf 'pnpm contradictory stderr'; return 1; }
            file="$TMP/report-check-pnpm.$$"
            decode_b64 "$(kv "$out" report1_b64)" "$file" ||
                { printf 'dsh pnpm report is missing'; return 1; }
            exact_report "$file" "$v1" || {
                printf 'pnpm report is not the exact record "%s": %s' \
                    "$v1" "$(tr -d '\0' <"$file" | head -c 200)"
                return 1
            } ;;
    esac
    return 0
}

# check_status RECIPE CONTAINER_OUTPUT -- decode the exact status bytes and
# validate them with the committed install-status.py record predicate, so a
# multi-line or unterminated record is rejected rather than truncated to its
# first line (review F2). Pi's separate bridge record is validated too.
check_status() { # $1 recipe, $2 output
    local recipe="$1" out="$2" file got
    file="$TMP/status-check.$$"
    decode_b64 "$(kv "$out" status_b64)" "$file" ||
        { printf '%s status record is missing' "$recipe"; return 1; }
    got="$(python3 "$STATUS_VALIDATOR" record "$file" 2>/dev/null)" || {
        printf '%s status record is not a valid record: %s' \
            "$recipe" "$(tr -d '\0' <"$file" | head -c 200)"
        return 1
    }
    [ "$got" = installed ] || { printf '%s status is not "installed": %s' "$recipe" "$got"; return 1; }
    if [ "$recipe" = pi ]; then
        file="$TMP/status-check-bridge.$$"
        decode_b64 "$(kv "$out" bridgestatus_b64)" "$file" ||
            { printf 'pi bridge status record is missing'; return 1; }
        got="$(python3 "$STATUS_VALIDATOR" record "$file" 2>/dev/null)" || {
            printf 'pi bridge status record is not a valid record: %s' \
                "$(tr -d '\0' <"$file" | head -c 200)"
            return 1
        }
        [ "$got" = installed ] || { printf 'pi bridge status is not "installed": %s' "$got"; return 1; }
    fi
    return 0
}

# bridge_marks -- parse the bridge probe's raw stdout (one notification per line)
# into one registration result per output line. A result is a COMPLETE plain
# marker line (`AGENT-VM-BRIDGE-REGISTERED models=<digits>` or
# `AGENT-VM-BRIDGE-MISSING`) or the `.message` of a valid single-line JSON
# notification. A marker-bearing line that is neither -- truncated/invalid JSON,
# a non-numeric or zero-padded count, trailing garbage -- is emitted as
# `AGENT-VM-BRIDGE-MALFORMED`, so a malformed result can never be truncated into
# a well-formed-looking count (`models=12"garbage`) nor erased (a positive
# contradicted by an unparseable MISSING that `jq ... || true` swallowed).
#
# Defined ONCE here and injected verbatim (via `declare -f`) into the container
# audit in container_script(), so the parser the real build runs is exactly the
# bytes oracle_self_test certifies against RAW probe output.
bridge_marks() {
    local line msg n
    while IFS= read -r line || [ -n "$line" ]; do
        [ -n "$line" ] || continue
        msg="$line"
        case "$line" in
            '{'*)
                # Whole-notification parsing: a JSON line that does not parse
                # as one value is not a trustworthy result.
                if msg="$(printf '%s\n' "$line" | jq -r \
                    'if type == "object" and (.message | type) == "string" then .message else empty end' \
                    2>/dev/null)"; then
                    :
                else
                    case "$line" in
                        *AGENT-VM-BRIDGE-REGISTERED* | *AGENT-VM-BRIDGE-MISSING*)
                            printf 'AGENT-VM-BRIDGE-MALFORMED\n' ;;
                    esac
                    continue
                fi ;;
        esac
        case "$msg" in
            AGENT-VM-BRIDGE-MISSING)
                printf 'AGENT-VM-BRIDGE-MISSING\n' ;;
            'AGENT-VM-BRIDGE-REGISTERED models='*)
                n="${msg#AGENT-VM-BRIDGE-REGISTERED models=}"
                case "$n" in
                    '' | *[!0-9]*)
                        printf 'AGENT-VM-BRIDGE-MALFORMED\n' ;;
                    *)
                        printf 'AGENT-VM-BRIDGE-REGISTERED models=%s\n' "$n" ;;
                esac ;;
            *AGENT-VM-BRIDGE-REGISTERED* | *AGENT-VM-BRIDGE-MISSING*)
                printf 'AGENT-VM-BRIDGE-MALFORMED\n' ;;
        esac
    done
}

# check_bridge CONTAINER_OUTPUT EXPECTED_BRIDGE_VERSION -- the behavioral probe
# must report EXACTLY ONE registration result with a canonical strictly-positive
# model count; metadata and a stale record are not proof. The probe/probes pair
# is what the container audit emits, so the oracle validates the external result
# independently of verify-pi.sh.
check_bridge() { # $1 output, $2 expected bridge version
    local out="$1" v1="$2" probe probes n
    [ "$(kv "$out" warn_rc)" = 0 ] || { printf 'pi rpc invocation exited %s' "$(kv "$out" warn_rc)"; return 1; }
    [ "$(kv "$out" warn)" != 0 ] || { printf 'pi does not load the mandatory warning extension'; return 1; }
    [ "$(kv "$out" bridgever)" = "$v1" ] ||
        { printf 'installed bridge %s != %s' "$(kv "$out" bridgever)" "$v1"; return 1; }
    [ "$(kv "$out" probe_rc)" = 0 ] || { printf 'bridge probe exited %s' "$(kv "$out" probe_rc)"; return 1; }
    probes="$(kv "$out" probes)"
    probe="$(kv "$out" probe)"
    [ "$probes" = 1 ] || {
        if [ "$probes" = 0 ]; then
            printf 'bridge did not register its provider: "%s"' "$probe"
        else
            printf 'bridge emitted %s conflicting registration results' "$probes"
        fi
        return 1
    }
    case "$probe" in
        AGENT-VM-BRIDGE-MISSING)
            printf 'bridge did not register its provider'; return 1 ;;
        'AGENT-VM-BRIDGE-REGISTERED models='*)
            n="${probe#AGENT-VM-BRIDGE-REGISTERED models=}"
            case "$n" in
                '' | *[!0-9]*) printf 'bridge reported a non-numeric model count "%s"' "$n"; return 1 ;;
                0 | 0*) printf 'bridge registered an empty model catalog'; return 1 ;;
            esac
            return 0 ;;
        *) printf 'bridge did not register its provider: "%s"' "$probe"; return 1 ;;
    esac
}

# expect_label IMG SUFFIX EXPECTED GOT -- a label must equal the
# INDEPENDENTLY-supplied selection; this is what catches a dropped override arg.
expect_label() { # $1 image, $2 suffix, $3 expected, $4 got
    [ "$4" = "$3" ] || { printf '%s ignored the requested %s=%s (label is %s)' "$1" "$2" "$3" "$4"; return 1; }
    return 0
}

# assert_files_identical A B MESSAGE -- byte identity, used for the
# committed-unchanged lock preservation check; a drift must fail.
assert_files_identical() { # $1 a, $2 b, $3 message
    cmp -s "$1" "$2" || { printf '%s' "$3"; return 1; }
    return 0
}

# oracle_self_test -- negative/positive CERTIFICATION of the predicates above.
# Every state the review named (a dropped override arg, lock drift, a
# missing/stale/degraded status, a non-registering bridge) must be rejected,
# and the matching good state must pass. Run with `--self-test`.
oracle_self_test() {
    local reason

    check_report codex rust-v0.159.3 "" "report_b64=$(printf 'codex-cli 0.159.3\n' | blob)" >/dev/null ||
        fail "self-test: a matching codex report was rejected"
    reason="$(check_report codex rust-v0.159.3 "" "report_b64=$(printf 'codex-cli 0.159.2\n' | blob)")" &&
        fail "self-test: a mismatched codex report was accepted"
    [ -n "$reason" ] || fail "self-test: a rejected report must carry a reason"
    # F2: a correct first line contradicted by a trailing line must be rejected
    # (the old `head -n 1` capture certified it).
    reason="$(check_report codex rust-v0.159.3 "" "report_b64=$(printf 'codex-cli 0.159.3\n9.9.9\n' | blob)")" &&
        fail "self-test: a contradictory trailing report line was accepted"
    [ -n "$reason" ] || fail "self-test: contradictory report rejection needs a reason"
    reason="$(check_report codex rust-v0.159.3 "" 'report_b64=MISSING')" &&
        fail "self-test: a missing codex report was accepted"
    reason="$(check_report dsh 0.1.5-rc.2 11.11.0 "$(printf 'report_b64=%s\nreport1_b64=%s\nrc1=0\n' \
        "$(printf '0.1.5-rc.2\n' | blob)" "$(printf '11.10.0\n' | blob)")")" &&
        fail "self-test: a wrong pnpm report was accepted"

    check_status codex "status_b64=$(sblob installed)" >/dev/null ||
        fail "self-test: an installed codex status was rejected"
    check_status pi "$(printf 'status_b64=%s\nbridgestatus_b64=%s\n' "$(sblob installed)" "$(sblob installed)")" >/dev/null ||
        fail "self-test: an installed pi + bridge status was rejected"
    local bad_status
    for bad_status in pending 'absent-transport 6' FAILED; do
        reason="$(check_status codex "status_b64=$(sblob "$bad_status")")" &&
            fail "self-test: codex status '$bad_status' was accepted"
        [ -n "$reason" ] || fail "self-test: status '$bad_status' rejection needs a reason"
    done
    reason="$(check_status codex 'status_b64=MISSING')" &&
        fail "self-test: a missing codex status was accepted"
    # F2: a valid first line followed by a contradictory record, and a record
    # with no trailing newline, must both be rejected by the byte-exact
    # validator (the old first-line extraction certified the former).
    reason="$(check_status codex "status_b64=$(printf 'installed\nabsent-transport 6\n' | blob)")" &&
        fail "self-test: a multi-line installed+absence status was accepted"
    [ -n "$reason" ] || fail "self-test: multi-line status rejection needs a reason"
    reason="$(check_status codex "status_b64=$(printf 'installed' | blob)")" &&
        fail "self-test: an unterminated installed status was accepted"
    reason="$(check_status pi "$(printf 'status_b64=%s\nbridgestatus_b64=%s\n' "$(sblob installed)" "$(sblob 'absent-transport 6')")")" &&
        fail "self-test: a degraded (absent) bridge status was accepted"
    reason="$(check_status pi "$(printf 'status_b64=%s\nbridgestatus_b64=MISSING\n' "$(sblob installed)")")" &&
        fail "self-test: a missing bridge status was accepted"

    check_bridge $'warn_rc=0\nwarn=1\nbridgever=0.8.0\nprobe_rc=0\nprobe=AGENT-VM-BRIDGE-REGISTERED models=3\nprobes=1' 0.8.0 >/dev/null ||
        fail "self-test: a registering bridge was rejected"
    local bad_probe probe_prefix=$'warn_rc=0\nwarn=1\nbridgever=0.8.0\nprobe_rc=0\nprobes=1\nprobe='
    for bad_probe in 'AGENT-VM-BRIDGE-MISSING' 'AGENT-VM-BRIDGE-REGISTERED models=0' \
        'AGENT-VM-BRIDGE-REGISTERED models=' 'AGENT-VM-BRIDGE-REGISTERED models=00' \
        'AGENT-VM-BRIDGE-REGISTERED models=garbage' 'AGENT-VM-BRIDGE-REGISTERED models=-1'; do
        reason="$(check_bridge "$probe_prefix$bad_probe" 0.8.0)" &&
            fail "self-test: a non-registering bridge ('$bad_probe') was accepted"
        [ -n "$reason" ] || fail "self-test: bridge '$bad_probe' rejection needs a reason"
    done
    # A positive marker contradicted by a missing one is ambiguous: the raw
    # count of registration results must be exactly one.
    reason="$(check_bridge $'warn_rc=0\nwarn=1\nbridgever=0.8.0\nprobe_rc=0\nprobe=AGENT-VM-BRIDGE-REGISTERED models=12\nprobes=2' 0.8.0)" &&
        fail "self-test: a contradictory bridge report was accepted"
    [ "$reason" = 'bridge emitted 2 conflicting registration results' ] ||
        fail "self-test: contradictory bridge rejection lost its reason: $reason"
    reason="$(check_bridge $'warn_rc=0\nwarn=1\nbridgever=0.8.0\nprobe_rc=0\nprobe=AGENT-VM-BRIDGE-REGISTERED models=3\nprobes=0' 0.8.0)" &&
        fail "self-test: an empty registration result set was accepted"
    reason="$(check_bridge $'warn_rc=0\nwarn=1\nbridgever=0.7.0\nprobe_rc=0\nprobe=AGENT-VM-BRIDGE-REGISTERED models=3\nprobes=1' 0.8.0)" &&
        fail "self-test: a stale bridge version was accepted"
    reason="$(check_bridge $'warn_rc=0\nwarn=1\nbridgever=0.8.0\nprobe_rc=1\nprobe=AGENT-VM-BRIDGE-REGISTERED models=3\nprobes=1' 0.8.0)" &&
        fail "self-test: a nonzero bridge probe was accepted"

    # --- bridge extraction: the REAL parser over RAW probe output -----------
    # The container audit pipes the bridge's raw stdout through bridge_marks and
    # feeds the marks to check_bridge. Seed RAW text (never a sanitized predicate
    # value) and require the pair to accept the valid forms and reject the
    # review's exact `models=12"garbage` counterexample, a positive contradicted
    # by a malformed JSON MISSING, and a plain contradiction. A regression that
    # truncates (`grep -oE '...models=[^" ]*'`) or drops (`jq ... || true`) a
    # malformed marker therefore fails HERE, through the same bytes the build
    # ships.
    command -v jq >/dev/null 2>&1 || fail "self-test: jq is required for the bridge extraction tier"
    local rawfile="$TMP/self-bridge-raw"
    check_raw_bridge() { # $1 raw probe stdout, $2 expect pass (1) / reject (0)
        printf '%s\n' "$1" >"$rawfile"
        bridge_marks <"$rawfile" >"$rawfile.marks"
        local fields reason accepted=0
        fields="$(printf 'probe=%s\nprobes=%s\n' \
            "$(head -n 1 "$rawfile.marks")" \
            "$(wc -l <"$rawfile.marks" | tr -d ' ')")"
        if reason="$(check_bridge "$(printf 'warn_rc=0\nwarn=1\nbridgever=0.8.0\nprobe_rc=0\n%s\n' "$fields")" 0.8.0)"; then
            accepted=1
        fi
        if [ "$2" = 1 ]; then
            [ "$accepted" = 1 ] || fail "self-test: a valid raw bridge report was rejected: $reason"
        else
            [ "$accepted" = 0 ] || fail "self-test: a malformed raw bridge report was accepted: '$1'"
            [ -n "$reason" ] || fail "self-test: raw bridge rejection lost its reason: '$1'"
        fi
    }
    check_raw_bridge 'AGENT-VM-BRIDGE-REGISTERED models=12' 1
    check_raw_bridge '{"type":"notification","message":"AGENT-VM-BRIDGE-REGISTERED models=12","notifyType":"warning"}' 1
    check_raw_bridge 'AGENT-VM-BRIDGE-REGISTERED models=12"garbage' 0
    check_raw_bridge '{"message":"AGENT-VM-BRIDGE-REGISTERED models=12"' 0
    check_raw_bridge "$(printf 'AGENT-VM-BRIDGE-REGISTERED models=12\n{"message":"AGENT-VM-BRIDGE-MISSING"')" 0
    check_raw_bridge "$(printf 'AGENT-VM-BRIDGE-REGISTERED models=12\nAGENT-VM-BRIDGE-MISSING')" 0

    expect_label img codex rust-v0.159.2 rust-v0.159.2 >/dev/null ||
        fail "self-test: a matching override label was rejected"
    reason="$(expect_label img codex rust-v0.159.2 rust-v0.159.3)" &&
        fail "self-test: a dropped override arg (default label) was accepted"

    local a="$TMP/self-lock-a" b="$TMP/self-lock-b"
    printf 'lock-a\n' >"$a"
    cp "$a" "$b"
    assert_files_identical "$a" "$b" "self-test" >/dev/null ||
        fail "self-test: identical locks were rejected"
    printf 'drift\n' >"$b"
    reason="$(assert_files_identical "$a" "$b" 'pi lock drifted')" &&
        fail "self-test: a drifted lock was accepted"
    [ "$reason" = 'pi lock drifted' ] || fail "self-test: lock drift rejection lost its reason"

    echo "oracle self-test: every rejected state failed with a reason"
    ( oracle_wiring_self_test )
}

# audit_image IMG RECIPE [STRICT_LABELS] -> labels + reports + T5 + status.
audit_image() { # $1 image, $2 recipe, $3 strict-label-exactness (default 1)
    local img="$1" recipe="$2" strict="${3:-1}" out v0 v1 k0 k1 reason
    assert_recipe_labels "$img" "$recipe" "" 0
    : "$strict"
    assert_expected_selection "$img" "$EXPECTED"

    case "$recipe" in
        codex) k0=codex ;;
        opencode) k0=opencode ;;
        claude) k0=claude ;;
        copilot) k0=copilot ;;
        dsh) k0=dsh; k1=pnpm ;;
        pi) k0=pi; k1=pi-claude-bridge ;;
        *) fail "unknown recipe: $recipe" ;;
    esac
    v0="$(label_value "$img" "org.agent-vm.version.$k0")"
    v1=""
    [ -n "${k1:-}" ] && v1="$(label_value "$img" "org.agent-vm.version.$k1")"

    for uid in 0:0 12345:23456 12345:45678 54321:12345; do
        out="$(run_container "$img" "$recipe" "$uid")"
        [ "$(kv "$out" rc0)" = 0 ] ||
            fail "$recipe ($uid) report exited $(kv "$out" rc0); output: $(printf '%s' "$out" | head -c 400)"
        [ "$(kv "$out" t5rc)" = 0 ] ||
            fail "$recipe ($uid) T5 audit failed: $out"
        reason="$(check_report "$recipe" "$v0" "$v1" "$out")" ||
            fail "$recipe ($uid) report mismatch: $reason"
        reason="$(check_status "$recipe" "$out")" ||
            fail "$recipe ($uid) $reason"
        if [ "$recipe" = pi ]; then
            # The behavioral probe must run under a numeric uid too: metadata
            # and a stale `installed` record are not proof a working extension.
            reason="$(check_bridge "$out" "$v1")" || fail "pi ($uid) $reason"
        fi
    done
    echo "  $recipe: labels ok; reports/T5/status ok as 0:0, 12345:23456, 12345:45678 and 54321:12345"
}

image_file() { # $1 image, $2 absolute path in image, $3 out file
    local rc=0
    run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
        docker_run --rm --platform "$platform" --entrypoint cat "$1" "$2" >"$3" 2>/dev/null || rc=$?
    [ "$rc" -ne 124 ] || fail "$1: timed out reading $2"
    [ "$rc" -eq 0 ] || fail "$1: cannot read $2"
}

# --- T5 built-image negatives ------------------------------------------------
t5_negatives() {
    local img="$RUN_ID-t5" status out rc=0
    run_with_watchdog "$BUILD_WATCHDOG_SECONDS" \
        docker buildx build --builder "$BUILDER" --platform "$platform" --load -t "$img" \
        -f "$FIXTURE_DOCKERFILE" --build-arg BASE_IMAGE="$BASE_IMAGE" \
        "$REPO_ROOT/images" >"$TMP/build-t5.log" 2>&1 || rc=$?
    if [ "$rc" -ne 0 ]; then
        sed -n '1,60p' "$TMP/build-t5.log" >&2
        [ "$rc" -ne 124 ] || fail "T5 fixture build timed out after ${BUILD_WATCHDOG_SECONDS}s"
        fail "T5 fixture build failed"
    fi

    # expect_fail UID TARGET [PATH_OVERRIDE] NEEDLE [WORKDIR]
    expect_fail() {
        local uid="$1" target="$2" pathov="$3" needle="$4" workdir="${5:-}" out status
        local args=(--rm --platform "$platform" --user "$uid" --cap-drop ALL --network none)
        if [ -n "$pathov" ]; then
            args=(-e "PATH=$pathov" "${args[@]}")
        fi
        if [ -n "$workdir" ]; then
            args=(--workdir "$workdir" "${args[@]}")
        fi
        args+=(-v "$REPO_ROOT/images/recipe-contract:/contract:ro"
            --entrypoint /usr/bin/python3 "$img" /contract/check-tool-access.py "$target")
        set +e
        out="$(run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" docker_run "${args[@]}" 2>&1)"
        status=$?
        set -e
        [ "$status" -ne 124 ] ||
            fail "T5 negative '$target' ($uid) timed out after ${RUNTIME_WATCHDOG_SECONDS}s"
        [ "$status" -ne 0 ] || fail "T5 negative '$target' ($uid) unexpectedly passed"
        case "$out" in
            *"$needle"*) : ;;
            *) fail "T5 negative '$target' ($uid) failed without '$needle': $out" ;;
        esac
    }

    expect_fail 12345:23456 /t5/neg/symlink0700/bin/link "" "not traversable by all"
    expect_fail 12345:23456 tool "/t5/neg/dir0700/bin:/usr/bin:/bin" "not traversable by all"
    expect_fail 12345:23456 /t5/neg/root0700/cmd "" "not executable by all"
    expect_fail 12345:23456 /t5/neg/gid0701/bin/cmd "" "not traversable by all"
    # F6: the 0701 group-match DENIAL fixture is owned root:23456, so the group
    # class denies the audited 12345:23456 identity and the checker fails closed.
    # Assert execution is denied before repair and allowed after.
    expect_fail 12345:23456 /t5/neg/gid0701-deny/bin/cmd "" "does not exist"
    local gid_rc=0
    run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
        docker_run --rm --platform "$platform" --user 0:0 \
        --cap-drop ALL --cap-add SETUID --cap-add SETGID --network none \
        --entrypoint sh "$img" -c '
        set -eu
        if setpriv --reuid 12345 --regid 23456 --clear-groups /t5/neg/gid0701-deny/bin/cmd >/dev/null 2>&1; then
            echo "gid0701-deny executed before repair" >&2
            exit 10
        fi
        chmod 0755 /t5/neg/gid0701-deny
        setpriv --reuid 12345 --regid 23456 --clear-groups /t5/neg/gid0701-deny/bin/cmd >/dev/null 2>&1 || {
            echo "gid0701-deny still denied after repair" >&2
            exit 11
        }
        echo "gid0701-deny: denied before repair, allowed after"
        ' || gid_rc=$?
    [ "$gid_rc" -ne 124 ] || fail "the gid0701-deny execution fixture timed out"
    [ "$gid_rc" -eq 0 ] || fail "the gid0701-deny execution fixture failed (rc=$gid_rc)"
    expect_fail 12345:45678 /t5/neg/owner0001/cmd "" "not executable by all"
    expect_fail 12345:23456 /t5/neg/script0111/cmd "" "cannot read"
    expect_fail 12345:23456 /t5/neg/dangling/bin/link "" "does not exist"
    expect_fail 12345:23456 /t5/neg/cycle/bin/a "" "symlink chain exceeds"
    # SP1: the `#!/usr/bin/env NAME` shebang must audit NAME on PATH...
    expect_fail 12345:23456 /t5/neg/env0700/bin/tool \
        "/t5/neg/env0700/interp:/usr/bin:/bin" "not executable by all"
    # ...and a PATH-changing `-S` shebang must audit NAME under the EFFECTIVE
    # PATH its own assignment selects, not the auditor's (which here holds a
    # good interpreter).
    expect_fail 12345:23456 /t5/neg/envpath/bin/tool \
        "/t5/neg/envpath/good:/usr/bin:/bin" "not executable by all"
    # ...and a NESTED env interpreter that sets no PATH of its own inherits the
    # effective PATH its ancestor selected; resolving it on the auditor's PATH
    # would certify a tool the kernel cannot execute.
    expect_fail 12345:23456 /t5/neg/envnested/bin/tool \
        "/t5/neg/envnested/good:/usr/bin:/bin" "not executable by all"
    # ...and a relative command path must still audit the directory `..` leaves.
    expect_fail 12345:23456 private/../bin/tool "" "not traversable by all" \
        /t5/neg/lexical0700
    echo "  T5 negatives: all twelve rejected with the offending path"

    # The repaired tree passes under two unrelated numeric gids.
    local ok_uid
    for ok_uid in 12345:54321 54321:12345; do
        rc=0
        run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
            docker_run --rm --platform "$platform" --user "$ok_uid" \
            --cap-drop ALL --network none \
            -v "$REPO_ROOT/images/recipe-contract:/contract:ro" \
            --entrypoint /usr/bin/python3 "$img" \
            /contract/check-tool-access.py /t5/pass/one/two/cmd /t5/pass/multi/two \
            >"$TMP/t5-pass-$ok_uid.out" 2>&1 || rc=$?
        [ "$rc" -ne 124 ] ||
            fail "T5 repaired fixture timed out under uid $ok_uid after ${RUNTIME_WATCHDOG_SECONDS}s"
        [ "$rc" -eq 0 ] ||
            fail "T5 repaired fixture failed under uid $ok_uid: $(cat "$TMP/t5-pass-$ok_uid.out")"
        # The repaired `#!/usr/bin/env NAME` command passes once NAME is 0755.
        rc=0
        run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
            docker_run --rm --platform "$platform" --user "$ok_uid" \
            --cap-drop ALL --network none -e "PATH=/t5/pass/env/interp:/usr/bin:/bin" \
            -v "$REPO_ROOT/images/recipe-contract:/contract:ro" \
            --entrypoint /usr/bin/python3 "$img" \
            /contract/check-tool-access.py /t5/pass/env/bin/tool \
            >"$TMP/t5-pass-env-$ok_uid.out" 2>&1 || rc=$?
        [ "$rc" -ne 124 ] ||
            fail "T5 repaired env fixture timed out under uid $ok_uid after ${RUNTIME_WATCHDOG_SECONDS}s"
        [ "$rc" -eq 0 ] ||
            fail "T5 repaired env fixture failed under uid $ok_uid: $(cat "$TMP/t5-pass-env-$ok_uid.out")"
        # The repaired PATH-changing `-S` command passes once NAME is 0755.
        rc=0
        run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
            docker_run --rm --platform "$platform" --user "$ok_uid" \
            --cap-drop ALL --network none -e "PATH=/t5/pass/envpath/good:/usr/bin:/bin" \
            -v "$REPO_ROOT/images/recipe-contract:/contract:ro" \
            --entrypoint /usr/bin/python3 "$img" \
            /contract/check-tool-access.py /t5/pass/envpath/bin/tool \
            >"$TMP/t5-pass-envpath-$ok_uid.out" 2>&1 || rc=$?
        [ "$rc" -ne 124 ] ||
            fail "T5 repaired envpath fixture timed out under uid $ok_uid after ${RUNTIME_WATCHDOG_SECONDS}s"
        [ "$rc" -eq 0 ] ||
            fail "T5 repaired envpath fixture failed under uid $ok_uid: $(cat "$TMP/t5-pass-envpath-$ok_uid.out")"
        # The repaired nested inherited-PATH command passes once NAME is 0755.
        rc=0
        run_with_watchdog "$RUNTIME_WATCHDOG_SECONDS" \
            docker_run --rm --platform "$platform" --user "$ok_uid" \
            --cap-drop ALL --network none -e "PATH=/t5/pass/envnested/bad:/usr/bin:/bin" \
            -v "$REPO_ROOT/images/recipe-contract:/contract:ro" \
            --entrypoint /usr/bin/python3 "$img" \
            /contract/check-tool-access.py /t5/pass/envnested/bin/tool \
            >"$TMP/t5-pass-envnested-$ok_uid.out" 2>&1 || rc=$?
        [ "$rc" -ne 124 ] ||
            fail "T5 repaired nested-env fixture timed out under uid $ok_uid after ${RUNTIME_WATCHDOG_SECONDS}s"
        [ "$rc" -eq 0 ] ||
            fail "T5 repaired nested-env fixture failed under uid $ok_uid: $(cat "$TMP/t5-pass-envnested-$ok_uid.out")"
    done
    echo "  T5 repaired fixture: passes under 12345:54321 and 54321:12345"
}

# --- chain -------------------------------------------------------------------

# Validate selections independently of image labels; overrides audit, never install.
EXPECTED=$(for tool in codex opencode claude copilot dsh pi; do default_selection "$tool"; done)
for entry in ${expect_overrides[@]+"${expect_overrides[@]}"}; do
    suffix=${entry%%=*}; value=${entry#*=}
    [ "$entry" != "$suffix" ] || fail 'expect requires suffix=version'
    suffix_arg "$suffix" >/dev/null || fail "unknown selection $suffix"
    body=$value
    case "$suffix" in codex) body=${value#rust-v}; [ "$body" != "$value" ] || fail 'codex requires rust-v' ;;
        opencode) body=${value#v}; [ "$body" != "$value" ] || fail 'opencode requires v' ;; esac
    valid_semver "$body" || fail "invalid expected version $value"
    EXPECTED=$(printf '%s\n' "$EXPECTED" | sed "s/^$suffix=.*/$suffix=$value/")
done

check_locks() {
    local image=$1 project committed suffix pin file reason
    for project in dsh pi pi-packages; do
        case "$project" in
            dsh) committed=images/tools/dsh; suffix=dsh; pin=$DEFAULT_DSH
                [ "$(printf '%s\n' "$EXPECTED" | sed -n 's/^pnpm=//p')" = "$DEFAULT_PNPM" ] || continue ;;
            pi) committed=images/tools/pi; suffix=pi; pin=$DEFAULT_PI ;;
            pi-packages) committed=images/tools/pi/bridge; suffix=pi-claude-bridge; pin=$DEFAULT_BRIDGE ;;
        esac
        [ "$(printf '%s\n' "$EXPECTED" | sed -n "s/^$suffix=//p")" = "$pin" ] || continue
        for file in package.json package-lock.json; do
            image_file "$image" "/opt/agent-vm/$project/$file" "$TMP/extracted"
            reason=$(assert_files_identical "$REPO_ROOT/$committed/$file" "$TMP/extracted" "$project $file drifted") || fail "$reason"
        done
    done
}

# Drive the real audit call sites with complete wire captures, not predicates alone.
oracle_wiring_self_test() {
    assert_recipe_labels() { return 0; }
    label_value() {
        case "$2" in
            *version.codex) printf '%s\n' "$DEFAULT_CODEX" ;;
            *version.opencode) printf '%s\n' "$DEFAULT_OPENCODE" ;;
            *version.claude) printf '%s\n' "$DEFAULT_CLAUDE" ;;
            *version.copilot) printf '%s\n' "$DEFAULT_COPILOT" ;;
            *version.dsh) printf '%s\n' "$DEFAULT_DSH" ;;
            *version.pnpm) printf '%s\n' "$DEFAULT_PNPM" ;;
            *version.pi) printf '%s\n' "$DEFAULT_PI" ;;
            *version.pi-claude-bridge) printf '%s\n' "$DEFAULT_BRIDGE" ;;
        esac
    }
    run_container() { printf '%s\n' "$WIRE"; }
    local healthy
    healthy=$(printf 'rc0=0\nt5rc=0\nreport_b64=%s\nstatus_b64=%s\n' "$(printf 'codex-cli %s\n' "${DEFAULT_CODEX#rust-v}" | blob)" "$(sblob installed)")
    WIRE=$healthy
    audit_image img codex >/dev/null || fail 'healthy wire rejected'
    for status in MISSING "$(sblob pending)" "$(sblob 'absent-transport 6')"; do
        WIRE=$(printf '%s\n' "$healthy" | sed "s/^status_b64=.*/status_b64=$status/")
        if ( audit_image img codex ) >/dev/null 2>&1; then fail 'bad status wire accepted'; fi
    done
    WIRE=$(printf '%s\n' "$healthy" | sed 's/^rc0=0/rc0=1/')
    if ( audit_image img codex ) >/dev/null 2>&1; then fail 'nonzero report wire accepted'; fi
    WIRE=$(printf '%s\n' "$healthy" | sed "s/^report_b64=.*/report_b64=$(printf 'codex-cli 0.0.1\n' | blob)/")
    if ( audit_image img codex ) >/dev/null 2>&1; then fail 'stale selection wire accepted'; fi
    WIRE=$healthy
    # shellcheck disable=SC2030,SC2031 # mutation intentionally confined to child
    if ( EXPECTED=$(printf '%s\n' "$EXPECTED" | sed 's/^codex=.*/codex=rust-v0.0.1/'); audit_image img codex ) >/dev/null 2>&1; then fail 'ignored explicit selection accepted'; fi
    WIRE="$healthy"$'\nstderr_b64='"$(printf 'codex-cli 0.0.1\n' | blob)"
    # kv takes the first field; healthy has no stderr field, so this is genuine.
    if ( audit_image img codex ) >/dev/null 2>&1; then fail 'wrong stderr accepted'; fi
    local pi_wire
    pi_wire=$(printf 'rc0=0\nt5rc=0\nreport_b64=%s\nstatus_b64=%s\nbridgestatus_b64=%s\nwarn_rc=0\nwarn=1\nbridgever=%s\nprobe_rc=0\nprobes=1\nprobe=AGENT-VM-BRIDGE-REGISTERED models=12\n' "$(printf '%s\n' "$DEFAULT_PI" | blob)" "$(sblob installed)" "$(sblob installed)" "$DEFAULT_BRIDGE")
    WIRE=$pi_wire
    audit_image img pi >/dev/null || fail 'healthy Pi wire rejected'
    for bad in 'AGENT-VM-BRIDGE-MISSING' 'AGENT-VM-BRIDGE-REGISTERED models=12"garbage' 'AGENT-VM-BRIDGE-REGISTERED models=0'; do
        WIRE=$(printf '%s\n' "$pi_wire" | sed "s/^probe=.*/probe=$bad/")
        if ( audit_image img pi ) >/dev/null 2>&1; then fail 'malformed bridge wire accepted'; fi
    done
    WIRE=$(printf '%s\n' "$pi_wire" | sed 's/^probes=1/probes=2/')
    if ( audit_image img pi ) >/dev/null 2>&1; then fail 'contradictory bridge wire accepted'; fi
    image_file() {
        local committed
        case "$2" in
            */pi-packages/*) committed=images/tools/pi/bridge ;;
            */pi/*) committed=images/tools/pi ;;
            */dsh/*) committed=images/tools/dsh ;;
        esac
        cp "$REPO_ROOT/$committed/${2##*/}" "$3"
    }
    check_locks img || fail 'healthy extracted lock wire rejected'
    image_file() { printf drift >"$3"; }
    if ( check_locks img ) >/dev/null 2>&1; then fail 'lock wire drift accepted'; fi
    echo 'finished-image wire mutations rejected'
}

if [ "$self_test" = true ]; then
    oracle_self_test
    echo 'standard-image --self-test: OK'
    exit 0
fi
if [ "$pi_audit" = true ]; then
    for required in docker python3; do command -v "$required" >/dev/null || fail "$required required"; done
    pi_audit_status_regression
    echo 'standard-image --pi-audit-status: OK'
    exit 0
fi
for required in docker jq python3; do command -v "$required" >/dev/null || fail "$required required"; done
if [ "$artifact" = false ]; then
    BUILDER=$(docker context show)
    docker buildx inspect "$BUILDER" | grep -Eq '^Driver:[[:space:]]+docker$' || fail 'T5 fixtures require daemon-backed builder'
fi
want=$(for suffix in codex opencode claude copilot dsh pnpm pi pi-claude-bridge; do echo "org.agent-vm.version.$suffix"; done | sort)
[ "$(version_keys "$STANDARD_IMAGE")" = "$want" ] || fail 'standard must have exactly eight selection labels'
# shellcheck disable=SC2031 # self-test mutation is confined to its own subshell
assert_expected_selection "$STANDARD_IMAGE" "$EXPECTED"
if [ "$artifact" = false ]; then
# shellcheck disable=SC2016 # in-container variables
run_with_watchdog 180 docker_run --rm --platform "$platform" --network none "$BASE_IMAGE" bash -c '
set -euo pipefail
bash --version >/dev/null; node --version; python3 --version; zellij --version
for tool in dsh pnpm pi codex opencode claude copilot; do if command -v "$tool"; then exit 1; fi; done
test ! -e /usr/local/bin/pi; test ! -e /opt/agent-vm/pi; test ! -e /opt/agent-vm/pi-extensions' || fail 'base is not tool-free'
fi
run_with_watchdog 180 docker_run --rm --platform "$platform" --network none "$STANDARD_IMAGE" bash -c 'set -euo pipefail; bash --version >/dev/null; node --version; python3 --version; zellij --version' || fail 'base facilities lost'
for tool in codex opencode claude copilot dsh pi; do audit_image "$STANDARD_IMAGE" "$tool" 0; done
check_locks "$STANDARD_IMAGE"
bash "$REPO_ROOT/script/test/seed-hooks.sh" "$STANDARD_IMAGE"
if [ "$artifact" = false ]; then
    t5_negatives
else
    probe=$(cat "$REPO_ROOT/script/test/fixtures/released-agent-probe.sh")
    for pair in "$(id -u):$(id -g)" 12345:23456 54321:34567; do
        run_with_watchdog 600 docker_run --rm --platform "$platform" --network none --cap-drop ALL \
            --user "$pair" --tmpfs /home/probe:rw,exec,mode=1777 -e HOME=/home/probe \
            --entrypoint bash "$STANDARD_IMAGE" -c "$probe" released-agent-probe \
            "${pair%:*}" "${pair#*:}" "$DEFAULT_CODEX" "$DEFAULT_OPENCODE" "$DEFAULT_CLAUDE" \
            "$DEFAULT_COPILOT" "$DEFAULT_DSH" "$DEFAULT_PNPM" "$DEFAULT_PI" "$DEFAULT_BRIDGE"
    done
fi
echo 'finished standard image acceptance passed'
