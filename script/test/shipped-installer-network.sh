#!/usr/bin/env bash
# Restricted-egress gate for the shipped vendored installers (#227, plan task 2).
#
# It runs the REAL patched vendored installers + owning hooks inside a container
# whose only route off the network is a digest-pinned mitmproxy sidecar. The
# proxy denies by default and forwards only requests that match a committed
# exact-selection rule, learning the signed CDN redirect target from the
# response to an already-allowed request -- never a whole CDN namespace. That is
# what makes "no ordinary latest lookup / no mutable bootstrap" an executable
# claim: a build that reached `latest`, a channel, a dist-tag or an unversioned
# installer URL is denied and the run fails, even if the surrounding code
# swallows the HTTP error.
#
# Usage: script/test/shipped-installer-network.sh BASE_IMAGE \
#            [--overrides] [--observe] [--tool TOOL] [--platform PLATFORM]
#        script/test/shipped-installer-network.sh --self-test
#        script/test/shipped-installer-network.sh --emit-config TOOL [--platform P]
#
#   --overrides   also run the committed exact alternate selection per tool
#   --observe     capture mode: log every URL and forward it (builds the
#                 allowlist; not an acceptance run)
#   --tool TOOL   only codex|opencode|claude|agy|opencode-plugins (repeatable)
#   --platform P  Docker platform (default: the host's)
#   --self-test   hermetic: replay the request-log and `-`-placeholder stream
#                 assertions against correct and defective streams. No Docker, no
#                 network; it runs in the fast aggregate.
#   --emit-config print the allowlist config this tool+platform would be handed,
#                 so the hermetic addon table replays production data.
#
# Tier: real Docker + real network. It downloads real release assets, so it is
# a bounded PR/manual native CI audit, never part of the hermetic aggregate.

set -euo pipefail

# Digest-pinned mitmproxy sidecar (linux/arm64 + linux/amd64 multi-arch image).
# Reviewed/updated by a developer; there is deliberately no floating tag here.
MITM_IMAGE="mitmproxy/mitmproxy@sha256:00b77b5d8804c8ad18cb6caefbf9d5849e895e8986c5ce011f4ae30f4385962f"

REPO_ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
FIXTURES="$REPO_ROOT/script/test/fixtures/installer-egress"
SELECTIONS="$FIXTURES/selections.tsv"

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# Initialized here because `--self-test` and `--emit-config` run before the main
# argument parsing below, and both reach functions that read it under `set -u`.
overrides=false

# --- stream assertions (hermetic) --------------------------------------------
#
# These three checks are the reviewers' "extracted production functions": each is
# a small assertion about a request/selection STREAM, and each was wrong in a way
# only running it against a CORRECT stream reveals. They are defined here, above
# every Docker and recipe-contract prerequisite, and exercised by `--self-test`,
# so the fast aggregate catches a regression in them without a native run.

selection_for() { # $1 = tool, $2 = column (2=default, 3=alternate)
    awk -F '\t' -v t="$1" -v c="$2" '$1 == t { print $c }' "$SELECTIONS"
}

selection_versions() { # $1 = tool -> newline-separated versions to run
    # stdout is exclusively version records, so a caller can assert its exact
    # contents; the "no alternate" explanation goes to stderr.
    local alternate
    printf '%s\n' "$(selection_for "$1" 2)"
    if $overrides; then
        alternate="$(selection_for "$1" 3)"
        case "$alternate" in
            "" | "-")
                echo "  $1 has one committed payload selection; no alternate" >&2
                ;;
            *)
                printf '%s\n' "$alternate"
                ;;
        esac
    fi
    return 0
}

# The proxy's log line is `ALLOW https://host/path`: the addon logs the redacted
# URL, scheme included. Matching the host alone refused every correct run.
CODEX_PAYLOAD_ALLOW='^ALLOW https://github\.com/openai/codex/releases/download/'

assert_codex_request_log() { # $1 version, $2 marker, [$3 log file]
    local version=$1 marker=$2 log_file=${3:-$TMP/logs/egress.log} log
    log="$(tail -n "+$(( marker + 1 ))" "$log_file" 2>/dev/null || true)"
    printf '%s\n' "$log" | grep -q "$CODEX_PAYLOAD_ALLOW" ||
        fail "codex $version: no payload request was allowed"
    if printf '%s\n' "$log" | grep -q 'api\.github\.com'; then
        fail "codex $version: the release metadata API was contacted"
    fi
    if printf '%s\n' "$log" | grep -q 'codex-package_SHA256SUMS'; then
        fail "codex $version: the checksum manifest was downloaded at build time"
    fi
    if printf '%s\n' "$log" | grep -qi 'sigstore'; then
        fail "codex $version: a Sigstore bundle was downloaded at build time"
    fi
    echo "    request log: payload fetch only (no metadata, checksums or bundles)"
}

# agy's build fetches ONE url: the committed release asset for the architecture
# under test. GitHub's tag API and Google's updater manifest are pin-time inputs,
# so a line naming either -- or the manifest's GCS url -- is a defect, not a
# tolerated redirect (ADR 0012).
AGY_PAYLOAD_ALLOW='^ALLOW https://github\.com/google-antigravity/antigravity-cli/releases/download/'

assert_agy_request_log() { # $1 version, $2 marker, [$3 log file]
    local version=$1 marker=$2 log_file=${3:-$TMP/logs/egress.log} log count
    log="$(tail -n "+$(( marker + 1 ))" "$log_file" 2>/dev/null || true)"
    printf '%s\n' "$log" | grep -q "$AGY_PAYLOAD_ALLOW" ||
        fail "agy $version: no payload request was allowed"
    count="$(printf '%s\n' "$log" | grep -c "$AGY_PAYLOAD_ALLOW" || true)"
    [ "$count" = 1 ] || fail "agy $version: $count payload requests, expected exactly one"
    for channel in 'api\.github\.com' 'run\.app' 'storage\.googleapis\.com'; do
        if printf '%s\n' "$log" | grep -q "$channel"; then
            fail "agy $version: a pin-time channel ($channel) was contacted at build time"
        fi
    done
    echo "    request log: one pinned payload fetch only (no tag API, no manifest)"
}

# Exactly one version record, and never the `-` placeholder row. Comparing the
# WHOLE string is the point: `case "$out" in *-*)` matched every canonical
# `rust-v0.159.3` tag and so failed every correct Codex run.
require_codex_version_line() { # $1 = the stdout of `selection_versions codex`
    local out=$1
    case "$out" in
        -) fail "selection_versions emitted the '-' placeholder: $out" ;;
    esac
    [ "$(printf '%s\n' "$out" | grep -c .)" = 1 ] ||
        fail "selection_versions codex must emit exactly one version, got '$out'"
}

check_codex_overrides_stream() {
    local err out
    err="$(mktemp "${TMPDIR:-/tmp}/codex-overrides.XXXXXX")" ||
        fail "cannot create the --overrides stderr capture"
    out="$(overrides=true; selection_versions codex 2>"$err")"
    [ "$out" = "$(selection_for codex 2)" ] ||
        fail "selection_versions codex --overrides printed '$(printf '%s' "$out" | tr '\n' ' ')' on stdout"
    require_codex_version_line "$out"
    grep -q 'codex has one committed payload selection; no alternate' "$err" ||
        fail "selection_versions codex --overrides must explain the missing alternate on stderr"
    rm -f "$err"
    echo "  codex: --overrides stdout is exactly the committed default; explanation is on stderr"
}

stream_expect_ok() { # $1 label, rest: the command that must accept
    local label=$1 out
    shift
    if out="$("$@" 2>&1)"; then
        echo "  ok: $label"
    else
        echo "FAIL: self-test $label: a correct stream was rejected: $out" >&2
        return 1
    fi
}

stream_expect_fail() { # $1 label, $2 fragment, rest: the command that must refuse
    local label=$1 fragment=$2 out
    shift 2
    if out="$("$@" 2>&1)"; then
        echo "FAIL: self-test $label: a defective stream was accepted" >&2
        return 1
    fi
    case "$out" in
        *"$fragment"*) echo "  ok: $label" ;;
        *) echo "FAIL: self-test $label: expected '$fragment' in: $out" >&2; return 1 ;;
    esac
}

run_stream_self_test() {
    local dir status=0
    dir="$(mktemp -d "${TMPDIR:-/tmp}/installer-egress-selftest.XXXXXX")"
    local log="$dir/egress.log"
    local release
    release="$(selection_for codex 2)"
    [ -n "$release" ] || { echo 'FAIL: no codex selection' >&2; return 1; }
    {
        printf 'ALLOW https://github.com/openai/codex/releases/download/%s/codex-package-x86_64-unknown-linux-musl.tar.gz\n' "$release"
        printf 'LEARN https://release-assets.githubusercontent.com/x/y.tar.gz?<redacted>\n'
        printf 'ALLOW-LEARNED https://release-assets.githubusercontent.com/x/y.tar.gz?<redacted>\n'
    } >"$log"
    stream_expect_ok 'request log: payload allow + learned CDN redirect' \
        assert_codex_request_log "$release" 0 "$log" || status=1

    for defect in metadata checksums sigstore; do
        cp "$log" "$dir/defect.log"
        case "$defect" in
            metadata) printf 'ALLOW https://api.github.com/repos/openai/codex/releases/latest\n' >>"$dir/defect.log" ;;
            checksums) printf 'ALLOW https://github.com/openai/codex/releases/download/%s/codex-package_SHA256SUMS\n' "$release" >>"$dir/defect.log" ;;
            sigstore) printf 'ALLOW https://github.com/openai/codex/releases/download/%s/x.tar.gz.sigstore\n' "$release" >>"$dir/defect.log" ;;
        esac
        stream_expect_fail "request log rejects $defect" 'FAIL:' \
            assert_codex_request_log "$release" 0 "$dir/defect.log" || status=1
    done
    : >"$dir/empty.log"
    stream_expect_fail 'request log rejects no payload request' \
        'no payload request was allowed' \
        assert_codex_request_log "$release" 0 "$dir/empty.log" || status=1
    # The marker scopes the check to this selection: earlier lines are not evidence.
    stream_expect_fail 'request log honours the marker' \
        'no payload request was allowed' \
        assert_codex_request_log "$release" 99 "$log" || status=1

    stream_expect_ok 'a canonical rust-v release is exactly one version line' \
        require_codex_version_line "$release" || status=1
    stream_expect_fail 'the - placeholder is refused as a whole line' 'placeholder' \
        require_codex_version_line - || status=1
    stream_expect_fail 'a release plus a placeholder line is refused' \
        'exactly one version' require_codex_version_line "$release-
-" || status=1

    stream_expect_ok 'the committed table explains its single codex selection' \
        check_codex_overrides_stream || status=1

    local agy_release
    agy_release="$(selection_for agy 2)"
    [ -n "$agy_release" ] || { echo 'FAIL: no agy selection' >&2; return 1; }
    {
        printf 'ALLOW https://github.com/google-antigravity/antigravity-cli/releases/download/%s/agy_cli_linux_x64.tar.gz\n' "$agy_release"
        printf 'LEARN https://release-assets.githubusercontent.com/x/agy.tar.gz?<redacted>\n'
        printf 'ALLOW-LEARNED https://release-assets.githubusercontent.com/x/agy.tar.gz?<redacted>\n'
    } >"$log"
    stream_expect_ok 'agy request log: one pinned payload allow + learned CDN redirect' \
        assert_agy_request_log "$agy_release" 0 "$log" || status=1
    for defect in metadata manifest gcs; do
        cp "$log" "$dir/agy-defect.log"
        case "$defect" in
            metadata) printf 'ALLOW https://api.github.com/repos/google-antigravity/antigravity-cli/releases/tags/%s\n' "$agy_release" >>"$dir/agy-defect.log" ;;
            manifest) printf 'ALLOW https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/linux_x64.json\n' >>"$dir/agy-defect.log" ;;
            gcs) printf 'ALLOW https://storage.googleapis.com/antigravity-cli/agy.tar.gz\n' >>"$dir/agy-defect.log" ;;
        esac
        stream_expect_fail "agy request log rejects $defect" 'FAIL:' \
            assert_agy_request_log "$agy_release" 0 "$dir/agy-defect.log" || status=1
    done
    cp "$log" "$dir/agy-twice.log"
    printf 'ALLOW https://github.com/google-antigravity/antigravity-cli/releases/download/%s/agy_cli_linux_x64.tar.gz\n' "$agy_release" >>"$dir/agy-twice.log"
    stream_expect_fail 'agy request log rejects a second payload request' 'exactly one' \
        assert_agy_request_log "$agy_release" 0 "$dir/agy-twice.log" || status=1
    : >"$dir/agy-empty.log"
    stream_expect_fail 'agy request log rejects no payload request' \
        'no payload request was allowed' \
        assert_agy_request_log "$agy_release" 0 "$dir/agy-empty.log" || status=1
    rm -rf "$dir"
    [ "$status" -eq 0 ] || return 1
    echo '  the strict-egress stream assertions accept correct input and refuse each defect'
    return 0
}

# Escape a version for use INSIDE an ERE before it is interpolated into a
# jq-built allowlist path (review F4): an unescaped `.` made the exact selection
# `2.1.286` also match `/2/1/286/...` (and `2a1b286`).
escape_re() { # $1 raw -> ERE-safe literal
    printf '%s' "$1" | sed 's/[][\\.^$*+?(){}|]/\\&/g'
}

# Build the addon config for one tool+version. GitHub release downloads redirect
# to a signed CDN URL, which the addon learns from the allowed response.
write_config() { # $1 = tool, $2 = version, $3 = out file
    local tool="$1" version="$2" out="$3" numeric
    case "$tool" in
        codex)
            # Exactly one network endpoint: the committed release's package for
            # the architecture under test. Release metadata, the checksum
            # manifest and the Sigstore bundles are pin-time inputs, so they are
            # not in the build's allowlist at all -- their absence is asserted
            # from the request log, not merely left to default-deny.
            numeric="$(escape_re "${version#rust-v}")"
            jq -n --arg v "$numeric" --arg arch "$arch" '{
                https_only: true,
                # Only an already-allowed HTTPS response may hand over a target,
                # and only on these two GitHub asset hosts: an allowed response
                # pointing at HTTP, credentials, another port or another origin
                # is refused by the addon, not learned.
                learn_hosts: ["release-assets.githubusercontent.com",
                              "objects.githubusercontent.com"],
                allow: [
                    {host: "github.com", method: "GET",
                     path: ("^/openai/codex/releases/download/rust-v" + $v
                            + "/codex-package-" + $arch + "-unknown-linux-musl\\.tar\\.gz$")}
                ],
                deny: ["/latest", "releases/latest", "releases/tags/latest",
                       "channel", "/install\\.sh$", "releases/download/latest",
                       "codex-package_SHA256SUMS", "\\.sigstore"]
            }' >"$out"
            ;;
        agy)
            # One network endpoint: the committed release's asset for the
            # architecture under test. The tag API and the updater manifest are
            # pin-time inputs, so they are not in the build's allowlist at all.
            numeric="$(escape_re "$version")"
            local agy_token
            case "$arch" in
                aarch64) agy_token=arm64 ;;
                x86_64) agy_token=x64 ;;
            esac
            jq -n --arg v "$numeric" --arg t "$agy_token" '{
                https_only: true,
                # Only an already-allowed HTTPS response may hand over a target,
                # and only on these two GitHub asset hosts.
                learn_hosts: ["release-assets.githubusercontent.com",
                              "objects.githubusercontent.com"],
                allow: [
                    {host: "github.com", method: "GET",
                     path: ("^/google-antigravity/antigravity-cli/releases/download/" + $v
                            + "/agy_cli_linux_" + $t + "\\.tar\\.gz$")}
                ],
                deny: ["/latest", "releases/latest", "releases/tags/", "run\\.app",
                       "storage\\.googleapis\\.com", "_musl"]
            }' >"$out"
            ;;
        opencode-plugins)
            node "$REPO_ROOT/images/tools/opencode/plugins/check-lock.mjs" "$REPO_ROOT/images/tools/opencode/plugins" >/dev/null
            python3 "$FIXTURES/opencode-plugins.py" "$REPO_ROOT/images/tools/opencode/plugins/package.json" >"$out"
            ;;
        opencode)
            numeric="$(escape_re "${version#v}")"
            local oc_target
            case "$arch" in
                aarch64) oc_target="linux-arm64" ;;
                x86_64) oc_target="linux-x64 linux-x64-baseline" ;;
            esac
            jq -n --arg v "$numeric" --arg t "$oc_target" '{
                allow: ($t | split(" ") | map(. as $target | {host: "registry.npmjs.org",
                         path: ("^/@opencode/cli-" + $target + "/-/cli-" + $target + "-" + $v + "\\.tgz$")})),
                deny: ["/latest", "opencode\\.ai/(v2/)?install", "@opencode-ai", "channel"]
            }' >"$out"
            ;;
        claude)
            numeric="$(escape_re "$version")"
            local cl_arch cl_platform
            case "$arch" in
                aarch64) cl_arch="arm64" ;;
                x86_64) cl_arch="x64" ;;
            esac
            cl_platform="linux-$cl_arch"
            jq -n --arg v "$numeric" --arg p "$cl_platform" '{
                allow: [
                    {host: "downloads.claude.ai",
                     path: ("^/claude-code-releases/" + $v + "/manifest\\.json$")},
                    {host: "downloads.claude.ai",
                     path: ("^/claude-code-releases/" + $v + "/manifest\\.json\\.raw-sig\\.json$")},
                    {host: "downloads.claude.ai",
                     path: ("^/claude-code-releases/" + $v + "/manifest\\.zst\\.json$")},
                    {host: "downloads.claude.ai",
                     path: ("^/claude-code-releases/" + $v + "/" + $p + "/claude(\\.zst)?$")}
                ],
                deny: ["/latest", "claude-code-releases/latest", "claude\\.ai/install\\.sh",
                       "channel", "/api/event_logging/", "/api/claude_code/"]
            }' >"$out"
            ;;
        *) fail "unknown tool: $tool" ;;
    esac
}

# The allowlist config is production data, so the hermetic addon table asks the
# gate for it instead of keeping a second copy: `--emit-config codex` prints
# exactly what the proxy would be handed.
if [ "${1:-}" = --emit-config ]; then
    shift
    [ "$#" -ge 1 ] || { echo '--emit-config needs a tool' >&2; exit 2; }
    emit_tool="$1"
    shift
    emit_platform=""
    while [ "$#" -gt 0 ]; do
        case "$1" in
            --platform)
                [ "$#" -ge 2 ] || { echo '--platform needs a value' >&2; exit 2; }
                emit_platform="$2"
                shift
                ;;
            *) echo "unknown argument: $1" >&2; exit 2 ;;
        esac
        shift
    done
    case "${emit_platform:-$(uname -m)}" in
        linux/arm64 | arm64 | aarch64) arch=aarch64 ;;
        linux/amd64 | amd64 | x86_64) arch=x86_64 ;;
        *) echo "unsupported platform: ${emit_platform}" >&2; exit 2 ;;
    esac
    case "$emit_tool" in
        codex | opencode | claude | agy) ;;
        *) echo "unknown tool: $emit_tool" >&2; exit 2 ;;
    esac
    write_config "$emit_tool" "$(selection_for "$emit_tool" 2)" /dev/stdout
    exit 0
fi

if [ "${1:-}" = --self-test ]; then
    shift
    [ "$#" -eq 0 ] || { echo '--self-test takes no other argument' >&2; exit 2; }
    run_stream_self_test || { echo 'shipped-installer-network --self-test: FAILED' >&2; exit 1; }
    echo 'shipped-installer-network --self-test: OK'
    exit 0
fi

# Whole-gate watchdog (plan task 5). macOS has no guaranteed `timeout`, so the
# first invocation re-execs the ENTIRE gate under the shared Python3
# process-group watchdog. On timeout the whole group is signalled; the inner run
# still owns its cleanup traps. The marker stops the re-exec from recursing.
if [ "${SHIPPED_INSTALLER_NETWORK_WATCHED:-0}" != 1 ]; then
    command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }
    export SHIPPED_INSTALLER_NETWORK_WATCHED=1
    exec python3 "$REPO_ROOT/script/test/host-watchdog.py" \
        "${SHIPPED_INSTALLER_NETWORK_GATE_WATCHDOG:-5400}" bash "$0" "$@"
fi

if [ "$#" -lt 1 ]; then
    echo "usage: $0 BASE_IMAGE [--overrides] [--observe] [--tool TOOL] [--platform PLATFORM]" >&2
    echo "       $0 --self-test | $0 --emit-config TOOL [--platform PLATFORM]" >&2
    exit 2
fi
BASE_IMAGE="$1"
shift

overrides=false
observe=false
platform=""
tools=()
while [ "$#" -gt 0 ]; do
    case "$1" in
        --overrides) overrides=true ;;
        --observe) observe=true ;;
        --tool)
            [ "$#" -ge 2 ] || { echo "--tool needs a value" >&2; exit 2; }
            tools+=("$2")
            shift
            ;;
        --platform)
            [ "$#" -ge 2 ] || { echo "--platform needs a value" >&2; exit 2; }
            platform="$2"
            shift
            ;;
        *)
            echo "unknown argument: $1" >&2
            exit 2
            ;;
    esac
    shift
done

if [ -z "$platform" ]; then
    case "$(uname -m)" in
        arm64 | aarch64) platform="linux/arm64" ;;
        x86_64 | amd64) platform="linux/amd64" ;;
        *) echo "unsupported host architecture: $(uname -m)" >&2; exit 1 ;;
    esac
fi

case "$platform" in
    linux/arm64) arch="aarch64" ;;
    linux/amd64) arch="x86_64" ;;
    *) echo "unsupported platform: $platform" >&2; exit 1 ;;
esac

# The installers execute the contract helpers, so they mount a composed
# directory. The source tree stopped being one when the helpers became Go: it
# holds sources, not a binary. Compose it once for the platform under test.
CONTRACT="$(bash "$REPO_ROOT/script/build/recipe-contract.sh" --platform "$platform")/out"
[ -x "$CONTRACT/recipe-contract" ] || { echo "no recipe-contract in $CONTRACT" >&2; exit 1; }

if [ "${#tools[@]}" -eq 0 ]; then
    tools=(codex opencode claude agy opencode-plugins)
fi

for required in docker jq python3; do
    command -v "$required" >/dev/null 2>&1 || { echo "$required is required" >&2; exit 1; }
done

# run_with_watchdog SECONDS CMD... -> the command's own status, or 124 on
# timeout (the whole process group is signalled so no container is orphaned).
run_with_watchdog() {
    python3 "$REPO_ROOT/script/test/host-watchdog.py" "$@"
}

# Per-runtime-probe budget for the network installer container (plan task 5).
NETWORK_CONTAINER_WATCHDOG_SECONDS="${SHIPPED_INSTALLER_NETWORK_CONTAINER_WATCHDOG:-360}"

mode=strict
$observe && mode=observe

RUN_ID="installer-egress-$$"
NET_INTERNAL="$RUN_ID-internal"
NET_EXTERNAL="$RUN_ID-external"
PROXY="$RUN_ID-proxy"
# `target/` is gitignored and absent on a fresh CI checkout with no Rust build,
# so create it before mktemp (which does not create parent directories).
mkdir -p "$REPO_ROOT/target"
TMP="$(mktemp -d "$REPO_ROOT/target/installer-egress.XXXXXX")"
mkdir -p "$TMP/certs" "$TMP/logs"
# Create the cumulative egress log once. It is never truncated per proxy
# restart (markers scope each selection), so `mark_log` always has a file to
# read while the addon appends across restarts.
: >"$TMP/logs/egress.log"
# Run-scoped, already-redacted request evidence. It lives under target/ci-logs
# (the workflow's artifact path) rather than an unscoped /tmp name, so a failed
# audit still uploads its per-selection logs before scratch cleanup. Every copy
# failure is printed; a reported evidence path that does not exist is a failure.
EVIDENCE_DIR="${INSTALLER_EGRESS_EVIDENCE_DIR:-$REPO_ROOT/target/ci-logs/installer-egress-$RUN_ID}"
CURRENT_LABEL=""
CURRENT_MARKER=0

capture_evidence() { # $1 = label, $2 = marker line count in $TMP/logs/egress.log
    local label="$1" marker="$2" rc=0
    mkdir -p "$EVIDENCE_DIR" 2>/dev/null || {
        echo "FAIL: cannot create evidence directory $EVIDENCE_DIR" >&2
        return 1
    }
    if [ -f "$TMP/logs/egress.log" ]; then
        cp "$TMP/logs/egress.log" "$EVIDENCE_DIR/egress-full.log" 2>/dev/null || {
            echo "FAIL: cannot copy the egress log into $EVIDENCE_DIR" >&2
            rc=1
        }
        tail -n "+$(( marker + 1 ))" "$TMP/logs/egress.log" >"$EVIDENCE_DIR/$label.log" 2>/dev/null || {
            echo "FAIL: cannot write evidence $EVIDENCE_DIR/$label.log" >&2
            rc=1
        }
    fi
    # Label-scoped: the denial and the substitution segments are different
    # streams, and a shared file name published the substitution's ALLOW lines
    # under the denial control's name.
    if [ -f "$TMP/mutation.log" ]; then
        cp "$TMP/mutation.log" "$EVIDENCE_DIR/$label-mutation.log" 2>/dev/null || {
            echo "FAIL: cannot write evidence $EVIDENCE_DIR/$label-mutation.log" >&2
            rc=1
        }
    fi
    return "$rc"
}

cleanup() {
    # Preserve the run's already-redacted evidence before deleting scratch, so a
    # failed exit still uploads what was captured. Never a global prune: the
    # container filter is namescoped to this run.
    capture_evidence "${CURRENT_LABEL:-current}" "${CURRENT_MARKER:-0}" || true
    local cids
    cids="$(docker ps -aq --filter "name=^${RUN_ID}-" 2>/dev/null || true)"
    if [ -n "$cids" ]; then
        # shellcheck disable=SC2086  # word-splitting over the id list is intended
        docker rm -f $cids >/dev/null 2>&1 || true
    fi
    docker network rm "$NET_INTERNAL" "$NET_EXTERNAL" >/dev/null 2>&1 || true
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

# The internal network is `--internal`: containers on it get NO default route to
# the internet. The proxy is the only member with a second, external network.
docker network create --internal "$NET_INTERNAL" >/dev/null
docker network create "$NET_EXTERNAL" >/dev/null

# The selections.tsv default column must equal the committed Dockerfile ARG
# default. Otherwise a developer bump could leave this gate validating an old
# native binary while the shipped recipe ships a newer one.
assert_selection_defaults_match_dockerfiles() {
    local tool arg arg_default tsv_default
    for tool in codex opencode claude agy; do
        case "$tool" in
            codex) arg=AGENT_VERSION_CODEX ;;
            opencode) arg=AGENT_VERSION_OPENCODE ;;
            claude) arg=AGENT_VERSION_CLAUDE ;;
            agy) arg=AGENT_VERSION_AGY ;;
        esac
        arg_default="$(sed -n "s/^ARG ${arg}=//p" "$REPO_ROOT/images/standard/Dockerfile")"
        tsv_default="$(selection_for "$tool" 2)"
        [ -n "$arg_default" ] || fail "$tool Dockerfile has no $arg default"
        [ "$tsv_default" = "$arg_default" ] ||
            fail "selections.tsv default for $tool is '$tsv_default' but the Dockerfile default is '$arg_default'"
    done
    local pin_release
    pin_release="$(python3 "$FIXTURES/codex-pin-release.py" \
        "$REPO_ROOT/images/tools/codex/pin.json")"
    [ "$pin_release" = "$(selection_for codex 2)" ] ||
        fail "the committed codex pin is '$pin_release' but selections.tsv says '$(selection_for codex 2)'"
    local agy_pin
    agy_pin="$(python3 "$REPO_ROOT/images/artifacts/read-pin-version.py" \
        "$REPO_ROOT/images/tools/agy/pin.json")"
    [ "$agy_pin" = "$(selection_for agy 2)" ] ||
        fail "the committed agy pin is '$agy_pin' but selections.tsv says '$(selection_for agy 2)'"
    echo "  selections.tsv defaults match the committed Dockerfile ARGs, the codex pin and the agy pin"
}
assert_selection_defaults_match_dockerfiles

# Allowlist exactness (review F4): the committed config must allow ONLY the
# exact selected version path, never an adjacent version, a path-separator
# variant, or a prefix extension that an unescaped-dot regex would accept. This
# drives the REAL addon module in the pinned mitmproxy image with synthetic
# requests (no network) and fails on any mismatch.
run_allowlist_negatives() { # $1 tool, $2 version
    local tool="$1" version="$2" numeric sep adjacent cfg="$TMP/allow-$1-$2.json"
    local cases="$TMP/allow-cases-$1-$2.tsv" oc_target cl_platform agy_token agy_other rc=0
    write_config "$tool" "$version" "$cfg"
    : >"$cases"
    mkdir -p "$TMP/allowlog"
    : >"$TMP/allowlog/egress.log"
    case "$tool" in
        opencode-plugins)
            # One positive per allow entry: a typo in a profile path must not hide.
            printf '%s\t1\n' 'https://registry.npmjs.org/@openchamber%2Fopencode-claude' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk' \
                'https://registry.npmjs.org/zod' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-darwin-arm64' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-darwin-x64' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-linux-arm64' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-linux-arm64-musl' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-linux-x64' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-linux-x64-musl' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-win32-arm64' \
                'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-win32-x64' >>"$cases"
            printf '%s\t1\n' 'https://registry.npmjs.org/@openchamber/opencode-claude/-/opencode-claude-1.4.1.tgz' \
                'https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.281.tgz' \
                'https://registry.npmjs.org/zod/-/zod-4.6.7.tgz' >>"$cases"
            # Provenance bundles, fetched while pnpm enforces the base trust policy.
            printf '%s\t1\n' 'https://registry.npmjs.org/-/npm/v1/attestations/@openchamber/opencode-claude@1.4.1' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/zod@4.6.7' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-darwin-arm64@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-darwin-x64@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-linux-arm64@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-linux-arm64-musl@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-linux-x64@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-linux-x64-musl@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-win32-arm64@0.3.281' \
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-win32-x64@0.3.281' >>"$cases"
            printf '%s\t0\n' 'http://registry.npmjs.org/zod' 'https://registry.npmjs.org:444/zod' \
                'https://registry.npmjs.org/zod?token=REVIEW-TOKEN' 'https://registry.npmjs.org/pnpm' \
                'https://registry.npmjs.org/zod/-/zod-4.6.8.tgz' \
                'https://registry.npmjs.org/-/npm/v1/attestations/zod@4.6.8' \
                'https://registry.npmjs.org/-/npm/v1/attestations/evil-package@1.0.0' \
                'https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk-linux-arm64/-/claude-agent-sdk-linux-arm64-0.3.281.tgz' >>"$cases"
            printf '%s\t0\tPOST\n' 'https://registry.npmjs.org/zod' >>"$cases"
            ;;
        codex)
            # One payload positive, and an explicit negative for every endpoint
            # this build must never reach: the tag API, the latest API, the
            # checksum manifest, the Sigstore bundles, the other architecture, an
            # adjacent version, a separator mutation, an extra suffix, another
            # repository, a query, plain HTTP and a non-default port.
            numeric="${version#rust-v}"
            sep="${numeric//./\/}"
            adjacent="${numeric%.*}.$(( ${numeric##*.} + 1 ))"
            local asset="codex-package-$arch-unknown-linux-musl.tar.gz" other="$arch"
            if [ "$arch" = aarch64 ]; then other=x86_64; else other=aarch64; fi
            local other_asset="codex-package-$other-unknown-linux-musl.tar.gz"
            local release_url="https://github.com/openai/codex/releases/download/rust-v$numeric"
            printf '%s\t1\n' "$release_url/$asset" >>"$cases"
            printf '%s\t0\n' \
                "https://api.github.com/repos/openai/codex/releases/tags/rust-v$numeric" \
                "https://api.github.com/repos/openai/codex/releases/latest" \
                "$release_url/codex-package_SHA256SUMS" \
                "$release_url/$asset.sigstore" \
                "$release_url/$other_asset" \
                "https://github.com/openai/codex/releases/download/rust-v$adjacent/$asset" \
                "https://github.com/openai/codex/releases/download/rust-v$sep/$asset" \
                "$release_url/$asset/extra" >>"$cases"
            printf '%s\t0\n' \
                "$release_url/codexx-$asset" \
                "https://github.com/openai/codexx/releases/download/rust-v$numeric/$asset" \
                "$release_url/$asset?sig=REVIEW-SIGNED-SECRET&token=REVIEW-TOKEN" >>"$cases"
            printf '%s\t0\n' "http://github.com/openai/codex/releases/download/rust-v$numeric/$asset" \
                "https://github.com:444/openai/codex/releases/download/rust-v$numeric/$asset" >>"$cases"
            printf '%s\t0\tPOST\n' "$release_url/$asset" >>"$cases" ;;
        agy)
            # One payload positive; negatives for both pin-time channels, the
            # manifest's GCS url, the other architecture, a musl build, an
            # adjacent version, a separator variant, a `v` prefix, an extra
            # suffix, another repository, a query, plain HTTP and a port.
            numeric="$version"
            sep="${numeric//./\/}"
            adjacent="${numeric%.*}.$(( ${numeric##*.} + 1 ))"
            case "$arch" in
                aarch64) agy_token=arm64; agy_other=x64 ;;
                x86_64) agy_token=x64; agy_other=arm64 ;;
            esac
            local agy_url="https://github.com/google-antigravity/antigravity-cli/releases/download/$numeric"
            printf '%s\t1\n' "$agy_url/agy_cli_linux_$agy_token.tar.gz" >>"$cases"
            printf '%s\t0\n' \
                "https://api.github.com/repos/google-antigravity/antigravity-cli/releases/tags/$numeric" \
                "https://api.github.com/repos/google-antigravity/antigravity-cli/releases/latest" \
                "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/linux_$agy_token.json" \
                "https://storage.googleapis.com/antigravity-cli/agy.tar.gz" \
                "$agy_url/agy_cli_linux_$agy_other.tar.gz" \
                "$agy_url/agy_cli_linux_${agy_token}_musl.tar.gz" \
                "https://github.com/google-antigravity/antigravity-cli/releases/download/$adjacent/agy_cli_linux_$agy_token.tar.gz" \
                "https://github.com/google-antigravity/antigravity-cli/releases/download/$sep/agy_cli_linux_$agy_token.tar.gz" \
                "https://github.com/google-antigravity/antigravity-cli/releases/download/v$numeric/agy_cli_linux_$agy_token.tar.gz" \
                "$agy_url/agy_cli_linux_$agy_token.tar.gz.extra" \
                "https://github.com/attacker/repo/releases/download/$numeric/agy_cli_linux_$agy_token.tar.gz" \
                "$agy_url/agy_cli_linux_$agy_token.tar.gz?sig=REVIEW-SIGNED-SECRET&token=REVIEW-TOKEN" >>"$cases"
            printf '%s\t0\n' "http://github.com/google-antigravity/antigravity-cli/releases/download/$numeric/agy_cli_linux_$agy_token.tar.gz" \
                "https://github.com:444/google-antigravity/antigravity-cli/releases/download/$numeric/agy_cli_linux_$agy_token.tar.gz" >>"$cases"
            printf '%s\t0\tPOST\n' "$agy_url/agy_cli_linux_$agy_token.tar.gz" >>"$cases" ;;
        opencode)
            numeric="${version#v}"
            sep="${numeric//./\/}"
            adjacent="${numeric%.*}.$(( ${numeric##*.} + 1 ))"
            case "$arch" in
                aarch64) oc_target="linux-arm64" ;;
                x86_64) oc_target="linux-x64" ;;
            esac
            printf '%s\t1\n' "https://registry.npmjs.org/@opencode/cli-$oc_target/-/cli-$oc_target-$numeric.tgz" >>"$cases"
            printf '%s\t0\n' "https://registry.npmjs.org/@opencode/cli-$oc_target/-/cli-$oc_target-$sep.tgz" >>"$cases"
            printf '%s\t0\n' "https://registry.npmjs.org/@opencode/cli-$oc_target/-/cli-$oc_target-$adjacent.tgz" >>"$cases"
            printf '%s\t0\n' "https://registry.npmjs.org/@opencode/cli-$oc_target/-/cli-$oc_target-$numeric.tgz?token=REVIEW-TOKEN" >>"$cases"
            printf '%s\t0\n' "https://registry.npmjs.org/@opencode/cli-$oc_target" \
                "https://registry.npmjs.org/@opencode-ai/cli-$oc_target/-/cli-$oc_target-$numeric.tgz" \
                "https://registry.npmjs.org/@opencode/cli-$oc_target/-/cli-other-$numeric.tgz" \
                'https://opencode.ai/v2/install' 'https://opencode.ai/update/api/latest/cli/npm' >>"$cases" ;;
        claude)
            numeric="$version"
            sep="${numeric//./\/}"
            adjacent="${numeric%.*}.$(( ${numeric##*.} + 1 ))"
            case "$arch" in
                aarch64) cl_platform="linux-arm64" ;;
                x86_64) cl_platform="linux-x64" ;;
            esac
            printf '%s\t1\n' "https://downloads.claude.ai/claude-code-releases/$numeric/manifest.json" >>"$cases"
            printf '%s\t1\n' "https://downloads.claude.ai/claude-code-releases/$numeric/$cl_platform/claude" >>"$cases"
            printf '%s\t0\n' "https://downloads.claude.ai/claude-code-releases/$sep/manifest.json" >>"$cases"
            printf '%s\t0\n' "https://downloads.claude.ai/claude-code-releases/$adjacent/manifest.json" >>"$cases"
            printf '%s\t0\n' "https://downloads.claude.ai/claude-code-releases/$numeric/$cl_platform/claudex" >>"$cases"
            printf '%s\t0\n' "https://downloads.claude.ai/claude-code-releases/$numeric/manifest.json?sig=REVIEW-SIGNED-SECRET&token=REVIEW-TOKEN" >>"$cases" ;;
    esac
    # Audit the addon under the SAME invocation identity as the proxy: a
    # root-plus-cap-drop combination is not the demonstrated failure, but the
    # invocation-owned log directory is the one the real proxy writes too.
    docker run --rm --platform "$platform" --user "$(id -u):$(id -g)" \
        -v "$FIXTURES/addon.py:/addon/addon.py:ro" \
        -v "$cfg:/addon/config.json:ro" \
        -v "$cases:/addon/cases.tsv:ro" \
        -v "$REPO_ROOT/script/test/shipped-installer-network-check-allowlist.py:/addon/check-allowlist.py:ro" \
        -v "$TMP/allowlog:/addon/log" \
        -e EGRESS_CONFIG=/addon/config.json -e EGRESS_LOG=/addon/log/egress.log \
        --entrypoint python3 "$MITM_IMAGE" /addon/check-allowlist.py || rc=$?
    [ "$rc" -eq 0 ] || fail "$tool $version allowlist exactness failed (rc=$rc)"
    echo "  $tool $version: allowlist exactness ok (separator/adjacent/prefix negatives)"
}

start_proxy() { # $1 = config file
    docker rm -f "$PROXY" >/dev/null 2>&1 || true
    # Run mitmdump as the INVOKING numeric identity. With --cap-drop ALL the
    # process has no CAP_DAC_OVERRIDE, so UID 0 cannot write the runner-owned
    # cert/log bind mounts on a native runner (the reported failure); the
    # invocation-owned scratch directories are writable by this same identity.
    # The egress log is NOT truncated on restart: markers scope each selection's
    # already-redacted segment, and the cumulative log is the failure evidence.
    docker run -d --name "$PROXY" \
        --network "$NET_EXTERNAL" \
        --cap-drop ALL --security-opt no-new-privileges \
        --user "$(id -u):$(id -g)" \
        -v "$TMP/certs:/certs" \
        -v "$1:/addon/config.json:ro" \
        -v "$FIXTURES/addon.py:/addon/addon.py:ro" \
        -v "$TMP/logs:/logs" \
        -e EGRESS_CONFIG=/addon/config.json \
        -e EGRESS_LOG=/logs/egress.log \
        -e EGRESS_MODE="$mode" \
        --entrypoint mitmdump \
        "$MITM_IMAGE" \
        --set confdir=/certs --listen-host 0.0.0.0 --listen-port 8080 \
        -s /addon/addon.py --set console_eventlog_verbosity=error >/dev/null
    docker network connect "$NET_INTERNAL" "$PROXY"

    local counter=0
    while [ "$counter" -lt 120 ]; do
        [ -s "$TMP/certs/mitmproxy-ca-cert.pem" ] && return 0
        sleep 0.5
        counter=$((counter + 1))
    done
    echo "proxy container log:" >&2
    docker logs "$PROXY" >&2 || true
    fail "mitmproxy did not produce a CA certificate"
}

stop_proxy() {
    docker rm -f "$PROXY" >/dev/null 2>&1 || true
}

# The proxy log since a marker line, so each tool+version is judged alone.
mark_log() { wc -l <"$TMP/logs/egress.log"; }
since_marker() { # $1 = marker line count
    tail -n "+$(( $1 + 1 ))" "$TMP/logs/egress.log" 2>/dev/null || true
}

hook_for() { # $1 = tool
    case "$1" in
        codex) printf '%s\n' install-codex.sh ;;
        opencode) printf '%s\n' install-opencode.sh ;;
        claude) printf '%s\n' install-claude.sh ;;
        agy) printf '%s\n' install-agy.sh ;;
        *) fail "unknown tool: $1" ;;
    esac
}

version_var_for() { # $1 = tool
    case "$1" in
        codex) printf '%s\n' AGENT_VM_VERSION_CODEX ;;
        opencode) printf '%s\n' AGENT_VM_VERSION_OPENCODE ;;
        claude) printf '%s\n' AGENT_VM_VERSION_CLAUDE ;;
        agy) printf '%s\n' AGENT_VM_VERSION_AGY ;;
        *) fail "unknown tool: $1" ;;
    esac
}

inner_script() { # $1 = tool  (paths are already mounted)
    cat <<'INNER'
set -eu
if command -v update-ca-certificates >/dev/null 2>&1; then
    cp /mitm/ca.pem /usr/local/share/ca-certificates/installer-egress.crt
    update-ca-certificates >/dev/null 2>&1 || true
fi
if [ -f /etc/ssl/certs/ca-certificates.crt ]; then
    cat /mitm/ca.pem >>/etc/ssl/certs/ca-certificates.crt
fi
export CURL_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
export SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
export NODE_EXTRA_CA_CERTS=/mitm/ca.pem
# A build must not phone home; the native installer honours this and it is the
# reason the gate can keep api.anthropic.com out of the allowlist entirely.
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
export HOME=/opt/agent
sh /hook
INNER
}

report_script() { # $1 = tool
    case "$1" in
        codex) printf '%s\n' '/opt/agent/.local/bin/codex --version' ;;
        opencode) printf '%s\n' '/usr/local/bin/opencode --version' ;;
        claude) printf '%s\n' 'if [ -x /opt/agent/.local/bin/claude ]; then /opt/agent/.local/bin/claude --version; else find /opt/agent/.local/bin -name claude -type f -exec {} --version \; 2>/dev/null; fi' ;;
        agy) printf '%s\n' '/usr/local/bin/agy --version' ;;
    esac
}

# The installed command lives in the container's own filesystem, discarded with
# --rm, so the version report is read in the SAME container that installed it
# (the internal network has no internet route anyway, so the run is offline).
run_one_combined() { # $1 = tool, $2 = version
    local tool="$1" version="$2" cfg="$TMP/config.json" marker run_status out
    local hook var
    hook="$(hook_for "$tool")"
    var="$(version_var_for "$tool")"
    local extra_env=()
    [ "$tool" = opencode ] && extra_env+=(-e "AGENT_VM_OPENCODE_LINK=/usr/local/bin/opencode")
    if [ "$tool" = codex ]; then
        # The committed pin and its loader travel with the hook. The container
        # gets no expected hash, URL or trust policy through the environment.
        extra_env+=(-v "$REPO_ROOT/images/tools/codex:/codex-source:ro")
        extra_env+=(-e "AGENT_VM_CODEX_SOURCE_DIR=/codex-source")
        extra_env+=(-e "TMPDIR=/tmp")
    fi
    if [ "$tool" = agy ]; then
        # The pin, its evidence and the shared installer travel read-only; the
        # build receives no url, digest or version-derived expectation.
        # The mount target is named after the artifact, not after the hook: the
        # shared loader names a `pin.json` after its own directory.
        extra_env+=(-v "$REPO_ROOT/images/tools/agy:/agy:ro")
        extra_env+=(-v "$REPO_ROOT/images/artifacts:/artifact-source/artifacts:ro")
        extra_env+=(-v "$REPO_ROOT/images/install-artifact.sh:/artifact-source/install-artifact.sh:ro")
        extra_env+=(-e "AGENT_VM_AGY_SOURCE_DIR=/agy")
        extra_env+=(-e "ARTIFACT_SOURCE_DIR=/artifact-source")
        case "$arch" in
            aarch64) extra_env+=(-e "AGENT_VM_AGY_ARCH=arm64") ;;
            x86_64) extra_env+=(-e "AGENT_VM_AGY_ARCH=amd64") ;;
        esac
        extra_env+=(-e "TMPDIR=/tmp")
    fi
    # A vendor mount is added only for tools that have one: the daemon would
    # create a missing host path, turning a typo into an empty directory.
    local vendor_mounts=()
    if [ -d "$REPO_ROOT/images/tools/$tool/vendor" ]; then
        vendor_mounts=(-v "$REPO_ROOT/images/tools/$tool/vendor:/vendor:ro")
        vendor_mounts+=(-e "AGENT_VM_VENDOR_DIR=/vendor")
    fi

    write_config "$tool" "$version" "$cfg"
    marker="$(mark_log)"

    echo "--- $tool $version"
    out="$TMP/run.out"
    set +e
    run_with_watchdog "$NETWORK_CONTAINER_WATCHDOG_SECONDS" \
        docker run --rm --name "$RUN_ID-inst-$RANDOM" --platform "$platform" --network "$NET_INTERNAL" \
        -v "$TMP/certs/mitmproxy-ca-cert.pem:/mitm/ca.pem:ro" \
        ${vendor_mounts[@]+"${vendor_mounts[@]}"} \
        -v "$CONTRACT:/contract:ro" \
        -v "$REPO_ROOT/images/tools/$tool/$hook:/hook:ro" \
        -e "HTTP_PROXY=http://$PROXY:8080" -e "HTTPS_PROXY=http://$PROXY:8080" \
        -e "http_proxy=http://$PROXY:8080" -e "https_proxy=http://$PROXY:8080" \
        -e "ALL_PROXY=http://$PROXY:8080" -e "NO_PROXY=" \
        -e "AGENT_VM_CONTRACT_DIR=/contract" \
        -e "AGENT_VM_TRANSPORT_RECEIPT=/tmp/receipt" \
        -e "AGENT_VM_INSTALL_STATUS_DIR=/tmp/status" \
        -e "$var=$version" \
        "${extra_env[@]}" \
        "$BASE_IMAGE" \
        sh -c "$(inner_script "$tool")
echo '--- REPORT ---'
$(report_script "$tool")" >"$out" 2>&1
    run_status=$?
    set -e

    sed -n '1,60p' "$out" | sed 's/^/    /'

    if grep -q '^DENY ' <(since_marker "$marker"); then
        echo "    egress log:" >&2
        since_marker "$marker" | sed 's/^/      /' >&2
        fail "$tool $version reached a denied URL"
    fi
    if $observe; then
        echo "    (observe mode: not asserting install success)"
        return 0
    fi
    [ "$run_status" -eq 0 ] || fail "$tool $version installer failed (exit $run_status)"

    local report expect whole
    report="$(sed -n '/^--- REPORT ---$/,$p' "$out" | tail -n +2 | head -n 1)"
    if [ "$tool" = agy ]; then
        # The version report is byte-exact: one line, no second line, no footer.
        whole="$(sed -n '/^--- REPORT ---$/,$p' "$out" | tail -n +2)"
        [ "$whole" = "$version" ] ||
            fail "agy report is not exactly '$version': '$whole'"
        echo "    report ok: $report"
        assert_agy_request_log "$version" "$marker"
        return 0
    fi
    case "$tool" in
        codex) expect="codex-cli ${version#rust-v}" ;;
        opencode) expect="opencode $version" ;;
        claude) expect="$version" ;;
    esac
    case "$report" in
        *"$expect"*) echo "    report ok: $report" ;;
        *) fail "$tool report '$report' does not contain '$expect'" ;;
    esac
    if [ "$tool" = codex ]; then
        assert_codex_request_log "$version" "$marker"
    fi
}

# The proxy must forward claude's native `install` traffic too; observe mode is
# how the allowlist is captured, strict mode is how it is enforced.
#
# Mutation check: the plan requires that a build which reached `latest` is
# DENIED, not merely that the installed version happened to match. This rewrites
# the vendored codex metadata URL to `/releases/latest` and requires the run to
# fail with a logged denial.
# The mutant container reports what it published from an EXIT trap, so the
# sentinels survive the hook's own `set -e` exit. Without them "the mutation
# produced no report" was a claim about a script that never printed one.
mutant_script() { # $1 = tool
    cat <<'SENTINELS'
trap 'echo "--- MUTANT SENTINELS ---"
      test -x /opt/agent/.local/bin/codex && echo "SENTINEL codex-command: present" || echo "SENTINEL codex-command: absent"
      test -e /opt/agent/.codex/packages/standalone && echo "SENTINEL package-root: present" || echo "SENTINEL package-root: absent"
      ls -A /opt/agent/.codex/packages/standalone 2>/dev/null | sed "s/^/SENTINEL root-entry: /"
      ls -A /opt/agent/.codex/packages/standalone/releases 2>/dev/null | sed "s/^/SENTINEL release: /"
      cat /tmp/status/* 2>/dev/null | sed "s/^/SENTINEL status: /"' EXIT
SENTINELS
    inner_script "$1"
}

mutant_run() { # $1 label, $2 vendor dir -> container status in $MUTATION_STATUS
    local label=$1 vendor=$2 out="$TMP/mutation.out" status=0
    write_config codex "$(selection_for codex 2)" "$TMP/config.json"
    start_proxy "$TMP/config.json"
    marker="$(mark_log)"
    CURRENT_LABEL="$label"
    CURRENT_MARKER="$marker"
    set +e
    run_with_watchdog "$NETWORK_CONTAINER_WATCHDOG_SECONDS" \
        docker run --rm --name "$RUN_ID-mut-$RANDOM" --platform "$platform" --network "$NET_INTERNAL" \
        -v "$TMP/certs/mitmproxy-ca-cert.pem:/mitm/ca.pem:ro" \
        -v "$vendor:/vendor:ro" \
        -v "$REPO_ROOT/images/tools/codex:/codex-source:ro" \
        -v "$CONTRACT:/contract:ro" \
        -v "$REPO_ROOT/images/tools/codex/install-codex.sh:/hook:ro" \
        -e "HTTP_PROXY=http://$PROXY:8080" -e "HTTPS_PROXY=http://$PROXY:8080" \
        -e "http_proxy=http://$PROXY:8080" -e "https_proxy=http://$PROXY:8080" \
        -e "ALL_PROXY=http://$PROXY:8080" -e "NO_PROXY=" \
        -e "AGENT_VM_CONTRACT_DIR=/contract" \
        -e "AGENT_VM_VENDOR_DIR=/vendor" \
        -e "AGENT_VM_CODEX_SOURCE_DIR=/codex-source" \
        -e "AGENT_VM_TRANSPORT_RECEIPT=/tmp/receipt" \
        -e "AGENT_VM_INSTALL_STATUS_DIR=/tmp/status" \
        -e "AGENT_VM_VERSION_CODEX=$(selection_for codex 2)" \
        -e "TMPDIR=/tmp/codex-mutant" \
        "$BASE_IMAGE" \
        sh -c "mkdir -p \"\$TMPDIR\"
$(mutant_script codex)" >"$out" 2>&1
    status=$?
    set -e
    since_marker "$marker" >"$TMP/mutation.log"
    MUTATION_STATUS=$status
    # Publish this mutation's own segment immediately, so the denial control's
    # DENY stream survives the next mutation overwriting $TMP/mutation.log.
    mkdir -p "$EVIDENCE_DIR" 2>/dev/null &&
        cp "$TMP/mutation.log" "$EVIDENCE_DIR/$label-mutation.log" 2>/dev/null ||
        echo "FAIL: cannot write evidence $EVIDENCE_DIR/$label-mutation.log" >&2
}

# Mutation 1: a build that reached `latest` is DENIED, not merely a build whose
# installed version happened to match. The injection is a checked single-site
# edit of a scratch vendor copy and an ACTUAL request through the trusted
# downloader -- not a fake denial log, and not a pin edited to say `latest`
# (which would test the loader, not egress).
run_denial_mutation() {
    local mutdir="$TMP/mutated-codex"
    python3 "$REPO_ROOT/script/test/vendored-installers-mutate-vendor.py" \
        "$REPO_ROOT/images/tools/codex/vendor/install.sh" \
        "$mutdir/install.sh" latest-request
    echo "--- mutation: codex injected /releases/latest request"
    mutant_run mutation "$mutdir"
    sed 's/^/    /' "$TMP/mutation.log"
    [ "$MUTATION_STATUS" -ne 0 ] || fail "the /releases/latest mutation unexpectedly succeeded"
    grep -q '^DENY ' "$TMP/mutation.log" \
        || fail "the /releases/latest mutation was not denied and logged"
    grep -q 'releases/latest' "$TMP/mutation.log" \
        || fail "the mutation never requested a latest URL"
    [ "$(grep -c 'releases/latest' "$TMP/mutation.log")" -ge 1 ] ||
        fail "the mutation's latest request is missing from the log"
    grep -q '^SENTINEL codex-command: absent$' "$TMP/mutation.out" ||
        fail "the denied mutation still published a command: $(grep '^SENTINEL ' "$TMP/mutation.out" | tr '\n' ' ')"
    # The uploaded evidence, not just the scrollback, must carry the denial.
    grep -q '^DENY .*releases/latest' "$EVIDENCE_DIR/mutation-mutation.log" 2>/dev/null ||
        fail "the denial evidence $EVIDENCE_DIR/mutation-mutation.log does not contain the denied latest request"
    echo "    mutation denied and logged, nothing installed"
}

# Mutation 2: the payload the build downloaded is replaced with wrong bytes
# INSIDE the downloader -- after the one allowed download and before that
# helper's own digest check -- with the pin and its evidence untouched. This is
# real-network evidence that the COMMITTED digest, not the transport and not the
# server, is what authorizes the archive. The injection site is checked by the
# mutator: injecting after the call site would be refused by the later Python
# preflight instead, which is a different oracle and the wrong side of the hash.
run_substitution_mutation() {
    local mutdir="$TMP/substituted-codex" payload_allows learned_allows
    python3 "$REPO_ROOT/script/test/vendored-installers-mutate-vendor.py" \
        "$REPO_ROOT/images/tools/codex/vendor/install.sh" \
        "$mutdir/install.sh" substitute-before-digest
    echo "--- mutation: post-download payload substitution"
    mutant_run substitution "$mutdir"
    sed 's/^/    /' "$TMP/mutation.log"
    [ "$MUTATION_STATUS" -ne 0 ] ||
        fail "the post-download substitution unexpectedly succeeded"
    grep -q 'checksum did not match expected digest' "$TMP/mutation.out" \
        || fail "the substitution was not refused by the committed digest: $(tail -5 "$TMP/mutation.out")"
    # Exactly one payload request and one learned CDN target: the run did not
    # retry, and it did not reach a second URL.
    payload_allows="$(grep -c "$CODEX_PAYLOAD_ALLOW" "$TMP/mutation.log" || true)"
    [ "$payload_allows" = 1 ] ||
        fail "the substitution made $payload_allows payload requests, expected exactly 1: $(cat "$TMP/mutation.log")"
    learned_allows="$(grep -c '^LEARN https://release-assets.githubusercontent.com/' "$TMP/mutation.log" || true)"
    [ "$learned_allows" = 1 ] ||
        fail "the substitution followed $learned_allows redirect targets, expected exactly 1: $(cat "$TMP/mutation.log")"
    if grep -q '^DENY ' "$TMP/mutation.log"; then
        fail "the substitution tried a denied URL: $(cat "$TMP/mutation.log")"
    fi
    # The sentinels are the extraction/native oracle: nothing was installed.
    grep -q '^SENTINEL codex-command: absent$' "$TMP/mutation.out" ||
        fail "the substitution published a command: $(grep '^SENTINEL ' "$TMP/mutation.out" | tr '\n' ' ')"
    # The installer's own `acquire_install_lock` does `mkdir -p $STANDALONE_ROOT`
    # and `cleanup_stale_install_artifacts` creates `releases/`, both BEFORE the
    # download, so a digest refusal always leaves that root behind; requiring it
    # absent made this gate fail every time it actually ran. "Nothing was
    # installed" is the exact leftover state: the installer lock file and an
    # EMPTY releases/ directory, no release, no `current` link (a root entry) and
    # no command. A change that leaves anything else here must be re-reviewed.
    if grep -q '^SENTINEL release: ' "$TMP/mutation.out"; then
        fail "the substitution left a release directory: $(grep '^SENTINEL ' "$TMP/mutation.out" | tr '\n' ' ')"
    fi
    if [ "$(grep -c '^SENTINEL root-entry: ' "$TMP/mutation.out" || true)" != 2 ]; then
        fail "the substitution left an unexpected standalone-root entry set: $(grep '^SENTINEL ' "$TMP/mutation.out" | tr '\n' ' ')"
    fi
    grep -q '^SENTINEL root-entry: install\.lock$' "$TMP/mutation.out" ||
        fail "the substitution's leftover root is not the installer lock: $(grep '^SENTINEL ' "$TMP/mutation.out" | tr '\n' ' ')"
    if grep -q '^SENTINEL status: installed' "$TMP/mutation.out"; then
        fail "the substitution recorded an installed status"
    fi
    echo "    substitution refused by the committed digest; one payload request, one redirect, nothing installed"
}

run_plugin_profile() {
    local cfg="$TMP/config.json" marker rc=0
    run_allowlist_negatives opencode-plugins committed
    write_config opencode-plugins committed "$cfg"
    start_proxy "$cfg"
    marker=$(mark_log)
    CURRENT_LABEL=opencode-plugins
    CURRENT_MARKER=$marker
    # Cold private HOME/XDG/store are allocated by the owning hook. No cache mount.
    run_with_watchdog 660 docker run --rm --name "$RUN_ID-plugin-$RANDOM" --platform "$platform" --network "$NET_INTERNAL" \
        -v "$TMP/certs/mitmproxy-ca-cert.pem:/mitm/ca.pem:ro" \
        -v "$REPO_ROOT/images/tools/opencode:/source:ro" \
        -v "$CONTRACT:/contract:ro" \
        -e "HTTPS_PROXY=http://$PROXY:8080" -e "HTTP_PROXY=http://$PROXY:8080" \
        -e NODE_EXTRA_CA_CERTS=/mitm/ca.pem \
        -e AGENT_VM_CONTRACT_DIR=/contract -e AGENT_VM_OPENCODE_SOURCE_DIR=/source \
        --entrypoint sh "$BASE_IMAGE" -c '
            set -eu
            mkdir -p /opt/agent-vm/opencode-plugins
            cp /source/plugins/package.json /source/plugins/pnpm-lock.yaml /source/plugins/pnpm-workspace.yaml /source/plugins/pnpm-inputs.sha256 /source/plugins/index.js /opt/agent-vm/opencode-plugins/
            AGENT_INSTALL_SOFT_FAIL= sh /source/install-opencode-plugins.sh
            node --input-type=module -e "import(\"/opt/agent-vm/opencode-plugins/index.js\")"
        ' >"$TMP/plugin-run.log" 2>&1 || rc=$?
    capture_evidence opencode-plugins "$marker"
    if grep -q '^DENY ' <(since_marker "$marker"); then fail 'plugin attempted denied egress'; fi
    [ "$rc" -eq 0 ] || { tail -40 "$TMP/plugin-run.log" >&2; fail "plugin profile failed: $rc"; }
}

status=0
for tool in "${tools[@]}"; do
    if [ "$tool" = opencode-plugins ]; then
        run_plugin_profile
        continue
    fi
    while IFS= read -r version; do
        [ -n "$version" ] || continue
        write_config "$tool" "$version" "$TMP/config.json"
        # F4: the committed config must reject separator/adjacent variants of the
        # exact selection over the REAL addon, before the run.
        run_allowlist_negatives "$tool" "$version" || status=1
        # Restart the proxy so the addon reloads the new config file contents.
        # The cumulative egress log is never truncated; the marker scopes this
        # selection's already-redacted segment for the evidence copy.
        start_proxy "$TMP/config.json"
        CURRENT_LABEL="$tool-$version"
        CURRENT_MARKER="$(mark_log)"
        run_one_combined "$tool" "$version" || status=1
        capture_evidence "$CURRENT_LABEL" "$CURRENT_MARKER" || status=1
    done < <(selection_versions "$tool")
done

stop_proxy
# The table/stream assertions are Codex-specific and independent of --tool: they
# read the committed selection table, not the container. The two mutations run a
# Codex container, so they are scoped to a run that selected the Codex tool.
tool_selected() { # $1 = tool
    local selected
    for selected in "${tools[@]}"; do
        [ "$selected" = "$1" ] && return 0
    done
    return 1
}
[ "$observe" = true ] || check_codex_overrides_stream
if [ "$observe" = false ] && tool_selected codex; then
    run_denial_mutation
    run_substitution_mutation
fi
# Publish the run-scoped, already-redacted evidence into the CI evidence
# directory (target/ci-logs before scratch cleanup), one log per selection plus
# the mutation. A reported evidence directory with no file is a failure.
capture_evidence "${CURRENT_LABEL:-current}" "${CURRENT_MARKER:-0}" || status=1
if [ -z "$(ls -A "$EVIDENCE_DIR" 2>/dev/null)" ]; then
    echo "FAIL: no egress evidence was written to $EVIDENCE_DIR" >&2
    status=1
fi
echo "egress evidence: $EVIDENCE_DIR"

[ "$status" -eq 0 ] || fail "restricted-egress gate failed"
echo 'shipped-installer-network gate: OK'
