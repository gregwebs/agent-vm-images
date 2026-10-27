#!/usr/bin/env bash
# Developer write tool: resolve each single-slot installer block's current
# upstream release, stage the reviewed exact default in its Dockerfile, and — for
# codex only — stage and publish the committed payload closure beside it.
#
# Usage: script/build/agent-versions.sh --write [--cosign /abs/cosign] [--repo-root DIR]
#        script/build/agent-versions.sh --check [--repo-root DIR]
#        script/build/agent-versions.sh --help
#
# This is an EXPLICIT developer step, never a build or CI step. Ordinary builds
# consume the committed exact `ARG AGENT_VERSION_*` defaults; nothing resolves
# "latest" at build time. After a run: review `git diff`, run the owning installer
# tests / the Docker audit, then commit the reviewed source changes.
#
# It resolves exactly the four sources the single-slot installers install from:
#   - codex:    openai/codex releases/latest tag -> canonical `rust-v<semver>`
#   - opencode: npm `@opencode/cli` latest -> canonical `v2.<semver remainder>`
#   - claude:   downloads.claude.ai native channel `/latest` -> bare `<semver>`
#   - copilot:  npm `@github/copilot` latest -> bare `<semver>`
# dsh and pi are lock-backed layers with EMPTY version ARGs; their pins (and
# generated LABEL fallbacks) are bumped by images/tools/{dsh,pi}/upgrade-*.sh.
#
# Codex is not just a four-ARG edit: an exact tag names a release, not bytes, so
# `script/dev/pin-codex.py` resolves both maintained Linux payloads, verifies
# their Sigstore bundles with the reviewed cosign, preflights both archives and
# stages the pin, its four evidence files and the Codex ARG as ONE transaction.
# That transaction is published through the shared Python publisher, so the
# Dockerfile and the closure cannot land independently. `--check` validates the
# committed Codex closure offline and touches nothing.
#
# GitHub API calls are authenticated with $GH_TOKEN when set; the token never
# appears in diagnostics. Write mode is rejected before any network call under
# GITHUB_ACTIONS, so an accidental workflow reintegration fails instead of
# silently restoring a build-time latest resolution.

set -euo pipefail

case "${BASH_SOURCE[0]}" in
    */*) script_dir_path="${BASH_SOURCE[0]%/*}" ;;
    *) script_dir_path=. ;;
esac
# shellcheck source=/dev/null
. "$script_dir_path/npm-pin.sh"

usage() {
    cat <<'EOF'
Usage: script/build/agent-versions.sh --write [--cosign /abs/cosign] [--repo-root DIR]
       script/build/agent-versions.sh --check [--repo-root DIR]
       script/build/agent-versions.sh --help

--write   resolve the current upstream release of each single-slot installer
          block and stage the reviewed exact default in the owning Dockerfile.
          For codex this additionally resolves, authenticates and preflights both
          maintained Linux payloads and publishes the pin, its evidence and the
          Codex ARG as one transaction. Developer write tool: no commit, build or
          push, and it refuses to run under GitHub Actions.
--check   validate the committed Codex payload closure offline (pin schema,
          evidence hashes, both bundles' structure, package policy and the
          single ARG/label). No network, no payload archive, no cosign, no
          writes. It does NOT re-verify signatures and does not cover the other
          tools' ARG defaults.

--cosign  the reviewed cosign (v2.4.1) used to verify both legacy .sigstore
          bundles. Required for --write; never downloaded or substituted.
EOF
}

fail() {
    echo "error: $1" >&2
    exit 1
}

repo_root=
write=false
check=false
cosign=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --write)
            write=true
            ;;
        --check)
            check=true
            ;;
        --cosign)
            [ "$#" -ge 2 ] || fail "--cosign requires an absolute path"
            cosign="$2"
            shift
            ;;
        --repo-root)
            [ "$#" -ge 2 ] || fail "--repo-root requires a directory"
            repo_root="$2"
            shift
            ;;
        --help | -h)
            usage
            exit 0
            ;;
        *)
            echo "error: unknown argument: $1" >&2
            usage >&2
            exit 2
            ;;
    esac
    shift
done

if [ "$write" = true ] && [ "$check" = true ]; then
    fail "--write and --check are exclusive"
fi
if [ "$write" != true ] && [ "$check" != true ]; then
    usage >&2
    exit 2
fi
if [ "$check" = true ] && [ -n "$cosign" ]; then
    fail "--cosign belongs to --write; --check is offline"
fi

if [ -z "$repo_root" ]; then
    repo_root="$(cd "$script_dir_path/../.." && pwd)"
fi
[ -d "$repo_root" ] || fail "--repo-root is not a directory: $repo_root"
repo_root="$(cd "$repo_root" && pwd -P)"

codex_writer="$repo_root/script/dev/pin-codex.py"
[ -f "$codex_writer" ] || fail "missing $codex_writer"

if [ "$check" = true ]; then
    echo 'Codex scope only: committed pin, evidence, bundles and the single ARG/label.'
    exec python3 "$codex_writer" --check --repo-root "$repo_root"
fi

if [ "${GITHUB_ACTIONS:-}" = true ]; then
    fail "refusing to run under GitHub Actions: version bumps are an explicit developer step"
fi

# Fail before a lookup so an HTTP-200-but-garbage response reports a per-tool
# message instead of this chokepoint. `python3` and the reviewed cosign are
# write-mode prerequisites: nothing here resolves an upstream release until both
# are present, and pin-codex re-checks cosign's exact version before it fetches.
require_tools curl jq npm python3
if [ -n "$cosign" ]; then
    case "$cosign" in
        /*) ;;
        *) fail "--cosign must be an absolute path: $cosign" ;;
    esac
    [ -x "$cosign" ] || fail "--cosign is not an executable file: $cosign"
else
    cosign="$(command -v cosign || true)"
    [ -n "$cosign" ] || fail "the reviewed cosign (v2.4.1) is required: install it or pass --cosign /absolute/path"
fi
# The exact-version check is the writer's own, run BEFORE any lookup: an
# incompatible verifier must stop the run without resolving a single upstream
# tag (a lookup that then fails to verify leaves a half-finished bump story).
probe_status=0
python3 "$codex_writer" --probe-cosign "$cosign" --repo-root "$repo_root" \
    || probe_status=$?
[ "$probe_status" -eq 0 ] || exit "$probe_status"
cosign_args=("--cosign" "$cosign")

github_latest_tag() {
    local repo=$1 auth=()
    if [ -n "${GH_TOKEN:-}" ]; then
        auth=(-H "Authorization: Bearer ${GH_TOKEN}")
    fi
    # `${auth[@]+...}`: bash 3.2 (macOS) treats an empty array as unset under -u.
    curl -fsSL ${auth[@]+"${auth[@]}"} -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/${repo}/releases/latest" | jq -r .tag_name
}

# Canonical semver 2.0.0 grammar (semver.org), as ERE components reused per
# tool below. Each anchor rejects multi-line/whitespace/HTML/interpolation, and
# every character class rejects a shell metacharacter before it can reach a
# Dockerfile ARG.
#   - version core numeric identifier: no leading zero  (0|[1-9][0-9]*)
#   - pre-release identifier: a numeric identifier (no leading zero) OR an
#     alphanumeric identifier (at least one letter or hyphen). A dot-separated
#     list of these, so empty/leading-zero-numeric ids are NOT exact.
#   - build identifier: digits (leading zeros allowed) OR an alphanumeric
#     identifier. A dot-separated list; empty ids are NOT exact.
numeric='(0|[1-9][0-9]*)'
pre_id='(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
build_id='[0-9A-Za-z-]+'
semver_core="${numeric}\.${numeric}\.${numeric}"
semver_pre="(-${pre_id}(\.${pre_id})*)?"
semver_build="(\+${build_id}(\.${build_id})*)?"
# claude's owning hook ships no build metadata; opencode/copilot accept the full
# canonical grammar; codex permits only its alpha/beta subset. Each tool regex
# must be NO more permissive than its owning installer, or the bumper would
# commit a value the owning hook rejects (review E1).
claude_re="^${semver_core}${semver_pre}\$"
opencode_re="^${semver_core}${semver_pre}${semver_build}\$"
copilot_re="^${semver_core}${semver_pre}${semver_build}\$"
codex_re="^${semver_core}(-alpha(\.${numeric}){0,2}|-beta(\.${numeric})?)?\$"

canonicalize() { # $1 = tool, $2 = raw
    local tool=$1 raw=$2 body
    case "$raw" in
        "" | null)
            fail "${tool} version empty/null"
            ;;
        *[!0-9A-Za-z.+-]*)
            fail "${tool} version has unexpected characters: '${raw}'"
            ;;
    esac
    case "$tool" in
        codex)
            case "$raw" in
                rust-v*) body="${raw#rust-v}" ;;
                v*) body="${raw#v}" ;;
                *) fail "codex tag is not rust-v<semver>: '${raw}'" ;;
            esac
            printf '%s' "$body" | grep -Eq "$codex_re" ||
                fail "codex version is not exact semver: '${raw}'"
            printf 'rust-v%s\n' "$body"
            ;;
        opencode)
            body="$raw"
            printf '%s' "$body" | grep -Eq "$opencode_re" ||
                fail "opencode version is not exact semver: '${raw}'"
            case "$body" in
                2.*) ;;
                *) fail "opencode requires reviewed major 2: '${raw}'" ;;
            esac
            printf 'v%s\n' "$body"
            ;;
        claude)
            printf '%s' "$raw" | grep -Eq "$claude_re" ||
                fail "${tool} version is not exact semver: '${raw}'"
            printf '%s\n' "$raw"
            ;;
        copilot)
            printf '%s' "$raw" | grep -Eq "$copilot_re" ||
                fail "${tool} version is not exact semver: '${raw}'"
            printf '%s\n' "$raw"
            ;;
        *)
            fail "unknown tool: ${tool}"
            ;;
    esac
}

raw_codex="$(github_latest_tag openai/codex)" \
    || fail "codex version lookup failed (openai/codex releases/latest)"
raw_opencode="$(npm view @opencode/cli dist-tags.latest)" \
    || fail "opencode version lookup failed (npm @opencode/cli)"
raw_claude="$(curl -fsSL https://downloads.claude.ai/claude-code-releases/latest)" \
    || fail "claude version lookup failed (downloads.claude.ai/claude-code-releases/latest)"
raw_copilot="$(npm view @github/copilot dist-tags.latest)" \
    || fail "copilot version lookup failed (npm @github/copilot)"

# Resolve EVERY value before touching a file.
codex="$(canonicalize codex "$raw_codex")"
opencode="$(canonicalize opencode "$raw_opencode")"
claude="$(canonicalize claude "$raw_claude")"
copilot="$(canonicalize copilot "$raw_copilot")"

tool_arg() { # $1 = tool -> ARG name
    case "$1" in
        codex) printf 'AGENT_VERSION_CODEX\n' ;;
        opencode) printf 'AGENT_VERSION_OPENCODE\n' ;;
        claude) printf 'AGENT_VERSION_CLAUDE\n' ;;
        copilot) printf 'AGENT_VERSION_COPILOT\n' ;;
    esac
}

file="$repo_root/images/standard/Dockerfile"
[ -f "$file" ] || fail "missing Dockerfile: $file"
work=$(mktemp -d)
# Absolute before anything can be retained: the recovery report is only usable if
# it names a directory that still resolves from wherever the reader is.
work="$(cd "$work" && pwd -P)"
keep_recovery=false
trap 'if [ "$keep_recovery" = false ]; then rm -rf "$work"; fi' EXIT
old_values=()
changed=false
stage_one() { # $1 tool, $2 value, $3 (optional) previous value to report
    local tool=$1 value=$2 previous=${3:-} arg count current
    arg=$(tool_arg "$tool")
    count=$(grep -c "^ARG ${arg}=" "$work/Dockerfile" || true)
    [ "$count" -eq 1 ] || fail "expected exactly one 'ARG ${arg}=' in images/standard/Dockerfile, found ${count}"
    current=$(sed -n "s/^ARG ${arg}=//p" "$work/Dockerfile")
    [ -n "$previous" ] || previous=$current
    old_values+=("$tool ${previous} -> ${value}")
    if [ "$current" != "$value" ]; then
        sed "s|^ARG ${arg}=.*\$|ARG ${arg}=${value}|" "$work/Dockerfile" >"$work/next"
        cp "$work/next" "$work/Dockerfile"
        changed=true
    fi
}

# The committed codex default, before staging rewrites the scratch copy: the
# report must name what the repository actually held.
codex_committed="$(sed -n 's/^ARG AGENT_VERSION_CODEX=//p' "$file")"

# Stage the codex closure FIRST: it writes its own complete Dockerfile proposal
# (the six-file set) into the owner's scratch, and the four single-slot ARG edits
# below then apply to that same scratch file. Nothing is published until all of
# them are staged.
codex_status=0
python3 "$codex_writer" --version "$codex" --stage-dir "$work" --repo-root "$repo_root" \
    "${cosign_args[@]}" || codex_status=$?
if [ "$codex_status" -ne 0 ]; then
    if [ "$codex_status" -eq 3 ]; then
        keep_recovery=true
        echo "error: codex staging is undecided; owner scratch retained at $work" >&2
    fi
    exit "$codex_status"
fi

stage_one codex "$codex" "$codex_committed"
stage_one opencode "$opencode"
stage_one claude "$claude"
stage_one copilot "$copilot"

# ONE publication for the Dockerfile and the whole codex closure. The Python
# publisher is the shared transaction; there is no second shell publish and no
# nested transaction. The owner scratch is named on stderr BEFORE the publisher
# starts: a signal-driven rollback exits from inside it with os._exit(3), so a
# report printed after the call would never be reached.
echo "pin-codex: owner scratch: $work (retained if the publication is undecided)" >&2
publish_status=0
python3 "$codex_writer" --publish-stage "$work" --repo-root "$repo_root" \
    || publish_status=$?
if [ "$publish_status" -ne 0 ]; then
    if [ "$publish_status" -eq 3 ]; then
        keep_recovery=true
        echo "error: recovery material retained; owner scratch retained at $work" >&2
    fi
    exit "$publish_status"
fi

echo 'Staged version bumps:'
printf '  %s\n' "${old_values[@]}"
echo 'Changed files: images/standard/Dockerfile, images/tools/codex/pin.json and images/tools/codex/evidence/'
if [ "$changed" = false ]; then
    echo 'The four single-slot ARG defaults were already current; the codex closure was still verified.'
fi
echo "Next: review 'git diff', run the installer tests / standard-image audit, then commit."
