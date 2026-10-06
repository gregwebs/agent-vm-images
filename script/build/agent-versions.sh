#!/usr/bin/env bash
# Developer write tool: resolve each single-slot installer block's current
# upstream release and stage the reviewed exact default in its Dockerfile.
#
# Usage: script/build/agent-versions.sh --write [--repo-root DIR]
#        script/build/agent-versions.sh --help
#
# This is an EXPLICIT developer step, never a build or CI step. Ordinary builds
# consume the committed exact `ARG AGENT_VERSION_*` defaults; nothing resolves
# "latest" at build time. After a run: review `git diff`, run the owning installer
# tests / the Docker audit, then commit the reviewed source changes.
#
# It resolves exactly the four sources the single-slot installers install from:
#   - codex:    openai/codex releases/latest tag -> canonical `rust-v<semver>`
#   - opencode: anomalyco/opencode releases/latest tag -> canonical `v<semver>`
#   - claude:   downloads.claude.ai native channel `/latest` -> bare `<semver>`
#   - copilot:  npm `@github/copilot` latest -> bare `<semver>`
# dsh and pi are lock-backed layers with EMPTY version ARGs; their pins (and
# generated LABEL fallbacks) are bumped by images/tools/{dsh,pi}/upgrade-*.sh.
#
# GitHub API calls are authenticated with $GH_TOKEN when set; the token never
# appears in diagnostics. Rejected before any network call under GITHUB_ACTIONS,
# so an accidental workflow reintegration fails instead of silently restoring a
# build-time latest resolution.

set -euo pipefail

case "${BASH_SOURCE[0]}" in
    */*) script_dir_path="${BASH_SOURCE[0]%/*}" ;;
    *) script_dir_path=. ;;
esac
# shellcheck source=/dev/null
. "$script_dir_path/npm-pin.sh"
# shellcheck source=/dev/null
. "$script_dir_path/transactional-publish.sh"

usage() {
    cat <<'EOF'
Usage: script/build/agent-versions.sh --write [--repo-root DIR]
       script/build/agent-versions.sh --help

Resolve the current upstream release of each single-slot installer block and
stage the reviewed exact default in the owning Dockerfile. This is a developer
write tool: it performs no commit, build, or push, and it refuses to run under
GitHub Actions.
EOF
}

fail() {
    echo "error: $1" >&2
    exit 1
}

repo_root=
write=false
while [ "$#" -gt 0 ]; do
    case "$1" in
        --write)
            write=true
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

if [ "$write" != true ]; then
    usage >&2
    exit 2
fi

if [ "${GITHUB_ACTIONS:-}" = true ]; then
    fail "refusing to run under GitHub Actions: version bumps are an explicit developer step"
fi

if [ -z "$repo_root" ]; then
    repo_root="$(cd "$script_dir_path/../.." && pwd)"
fi
[ -d "$repo_root" ] || fail "--repo-root is not a directory: $repo_root"
repo_root="$(cd "$repo_root" && pwd -P)"

# Fail before a lookup so an HTTP-200-but-garbage response reports a per-tool
# message instead of this chokepoint.
require_tools curl jq npm

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
            case "$raw" in
                v*) body="${raw#v}" ;;
                *) fail "opencode tag is not v<semver>: '${raw}'" ;;
            esac
            printf '%s' "$body" | grep -Eq "$opencode_re" ||
                fail "opencode version is not exact semver: '${raw}'"
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
raw_opencode="$(github_latest_tag anomalyco/opencode)" \
    || fail "opencode version lookup failed (anomalyco/opencode releases/latest)"
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
keep_recovery=false
trap 'if [ "$keep_recovery" = false ]; then rm -rf "$work"; fi' EXIT
cp -p "$file" "$work/Dockerfile"
changed=false
old_values=()
stage_one() {
    local tool=$1 value=$2 arg count current
    arg=$(tool_arg "$tool")
    count=$(grep -c "^ARG ${arg}=" "$work/Dockerfile" || true)
    [ "$count" -eq 1 ] || fail "expected exactly one 'ARG ${arg}=' in images/standard/Dockerfile, found ${count}"
    current=$(sed -n "s/^ARG ${arg}=//p" "$work/Dockerfile")
    old_values+=("$tool ${current} -> ${value}")
    if [ "$current" != "$value" ]; then
        sed "s|^ARG ${arg}=.*\$|ARG ${arg}=${value}|" "$work/Dockerfile" >"$work/next"
        cp "$work/next" "$work/Dockerfile"
        changed=true
    fi
}
stage_one codex "$codex"
stage_one opencode "$opencode"
stage_one claude "$claude"
stage_one copilot "$copilot"
if [ "$changed" = false ]; then
    echo 'All single-slot defaults are already current:'
    printf '  %s\n' "${old_values[@]}"
    exit 0
fi
if publish_transactional "$work/publish-backup" "$work/Dockerfile" "$file"; then
    status=0
else
    status=$?
fi
if [ "$status" -ne 0 ]; then
    if [ "$status" -eq 3 ]; then
        keep_recovery=true
        echo "error: recovery material retained at $work" >&2
    fi
    exit "$status"
fi
echo 'Staged version bumps:'
printf '  %s\n' "${old_values[@]}"
echo 'Changed files: images/standard/Dockerfile'
echo "Next: review 'git diff', run the installer tests / standard-image audit, then commit."
