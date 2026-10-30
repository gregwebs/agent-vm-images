#!/usr/bin/env bash
# Developer write tool: resolve each single-slot installer block's current
# upstream release, stage the reviewed exact default in its Dockerfile, and — for
# codex and agy — publish the committed payload closure beside it.
#
# Usage: script/build/agent-versions.sh --write [--cosign /abs/cosign] [--repo-root DIR]
#        script/build/agent-versions.sh --check [--repo-root DIR]
#        script/build/agent-versions.sh --help
#
# This is an EXPLICIT developer step, never a build or CI step. Ordinary builds
# consume the committed exact `ARG AGENT_VERSION_*` defaults; nothing resolves
# "latest" at build time. After a run: review `git diff`, run the owning installer
# tests / the Docker audit, edit `script/test/fixtures/installer-egress/selections.tsv`
# for a bumped agy, then commit the reviewed source changes.
#
# It resolves exactly the five sources the single-slot installers install from:
#   - codex:    openai/codex releases/latest tag -> canonical `rust-v<semver>`
#   - opencode: npm `@opencode/cli` latest -> canonical `v2.<semver remainder>`
#   - claude:   downloads.claude.ai native channel `/latest` -> bare `<semver>`
#   - copilot:  npm `@github/copilot` latest -> bare `<semver>`
#   - agy:      Google's per-architecture updater manifest -> bare `<semver>`
# dsh and pi are lock-backed layers with EMPTY version ARGs; their pins (and
# generated LABEL fallbacks) are bumped by images/tools/{dsh,pi}/upgrade-*.sh.
#
# Codex and agy are not just ARG edits: an exact tag (codex) or a manifest version
# (agy) names a release, not bytes, so each owning writer resolves both maintained
# Linux payloads, verifies them (a cosign signature and SHA-256 for codex; two
# agreeing SHA records for agy, which publishes no signature) and preflights both
# archives, then publishes its pin, its evidence files and its ARG as ONE
# transaction through the shared Python publisher. agy publishes FIRST: its likeliest failure — the updater manifest
# moving to a new release between resolution and pinning — then leaves every file
# untouched, and it reads the committed Dockerfile itself, so running it after
# Codex had staged a Dockerfile copy would make the Codex publication refuse or
# revert the agy ARG. `--check` validates both committed closures offline and
# touches nothing.
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
          For codex this additionally resolves, verifies and preflights both
          maintained Linux payloads and publishes the pin, its evidence and the
          Codex ARG as one transaction; for agy it requires Google's updater
          manifest to name that release and both channels to corroborate the
          bytes, then publishes the pin, its three evidence files and the agy ARG
          as its own transaction BEFORE codex runs. Developer write tool: no
          commit, build or push, and it refuses to run under GitHub Actions.
--check   validate the committed Codex and agy closures offline (pin schema,
          evidence, both Codex bundles' structure, package policy and each
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
agy_writer="$repo_root/script/dev/pin-agy.py"
[ -f "$agy_writer" ] || fail "missing $agy_writer"

if [ "$check" = true ]; then
    echo 'Codex and agy scope only: committed pins, evidence, bundles and the single ARG/label each.'
    # agy first, then the codex `exec` last: the codex check owns the final status.
    python3 "$agy_writer" --check --repo-root "$repo_root" || exit $?
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
# agy's pin and installer accept `X.Y.Z` only (artifact_pin.VERSION), so a
# pre-release or build suffix is refused here rather than committed.
agy_re="^${semver_core}\$"

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
        agy)
            printf '%s' "$raw" | grep -Eq "$agy_re" ||
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
# Google's updater manifest is agy's only release feed: there is no tag this
# repository trusts, and the writer accepts only the release the manifest names.
# The two per-architecture documents are compared here only on the version, which
# is the only field they share; a disagreement means no single release exists.
agy_manifest='https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests'
raw_agy_amd64="$(curl -fsSL "$agy_manifest/linux_amd64.json" | jq -r .version)" \
    || fail "agy version lookup failed ($agy_manifest/linux_amd64.json)"
raw_agy_arm64="$(curl -fsSL "$agy_manifest/linux_arm64.json" | jq -r .version)" \
    || fail "agy version lookup failed ($agy_manifest/linux_arm64.json)"
[ "$raw_agy_amd64" = "$raw_agy_arm64" ] \
    || fail "agy manifests disagree: amd64 names '${raw_agy_amd64}', arm64 names '${raw_agy_arm64}'"

# Resolve EVERY value before touching a file.
codex="$(canonicalize codex "$raw_codex")"
opencode="$(canonicalize opencode "$raw_opencode")"
claude="$(canonicalize claude "$raw_claude")"
copilot="$(canonicalize copilot "$raw_copilot")"
agy="$(canonicalize agy "$raw_agy_amd64")"

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
published_values=()
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
agy_committed="$(sed -n 's/^ARG AGENT_VERSION_AGY=//p' "$file")"

# agy publishes FIRST, in its own transaction: its likeliest failure (the updater
# manifest moving to a new release between the lookup above and this run) then
# leaves every file untouched. It reads the committed Dockerfile itself, so it
# cannot run after Codex has staged a Dockerfile copy: `--publish-stage` masks the
# other single-slot ARGs but not `AGENT_VERSION_AGY`, and would refuse or revert
# the agy default.
agy_status=0
python3 "$agy_writer" --version "$agy" --repo-root "$repo_root" || agy_status=$?
if [ "$agy_status" -ne 0 ]; then
    if [ "$agy_status" -eq 3 ]; then
        echo "error: agy publication is undecided; recover the directories printed above" >&2
    fi
    exit "$agy_status"
fi
# A Codex failure from here on is a PARTIAL bump: agy has already published in its
# own transaction, so a nonzero exit no longer means "nothing was written".
#
# Status 3 is the one Codex outcome that leaves its destinations UNDECIDED (the
# rollback was refused and recovery material was retained), so the clean-failure
# wording would contradict the recovery warning printed immediately above it: the
# operator would be told nothing was published while being told to recover.
report_agy_already_published() { # $1 = the Codex exit status the bumper is about to return
    local codex_status=$1
    if [ "$agy_committed" = "$agy" ]; then
        return 0
    fi
    if [ "$codex_status" -eq 3 ]; then
        echo "error: agy ${agy_committed} -> ${agy} was already published (its own transaction); codex is undecided — stop and recover before further edits, builds or commits" >&2
    else
        echo "error: agy ${agy_committed} -> ${agy} was already published (its own transaction); codex was not" >&2
    fi
    echo "Next: set agy ${agy} in script/test/fixtures/installer-egress/selections.tsv (a human edit; the strict-egress audit compares it to the Dockerfile)." >&2
}
if [ "$agy_committed" != "$agy" ]; then
    published_values+=("agy ${agy_committed} -> ${agy}")
fi

# Stage the codex closure: it writes its own complete Dockerfile proposal (the
# six-file set) into the owner's scratch, and the four single-slot ARG edits below
# then apply to that same scratch file. Nothing is published until all of them are
# staged, and the Dockerfile it stages is the one agy has already published.
codex_status=0
python3 "$codex_writer" --version "$codex" --stage-dir "$work" --repo-root "$repo_root" \
    "${cosign_args[@]}" || codex_status=$?
if [ "$codex_status" -ne 0 ]; then
    if [ "$codex_status" -eq 3 ]; then
        keep_recovery=true
        echo "error: codex staging is undecided; owner scratch retained at $work" >&2
    fi
    report_agy_already_published "$codex_status"
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
    report_agy_already_published "$publish_status"
    exit "$publish_status"
fi

echo 'Staged version bumps:'
printf '  %s\n' "${old_values[@]}"
if [ "${#published_values[@]}" -gt 0 ]; then
    echo 'Published closures (each its own transaction, already written to the tree):'
    printf '  %s\n' "${published_values[@]}"
fi
changed_files='images/standard/Dockerfile, images/tools/codex/pin.json and images/tools/codex/evidence/'
if [ "${#published_values[@]}" -gt 0 ]; then
    changed_files="$changed_files, images/tools/agy/"
fi
echo "Changed files: ${changed_files}"
if [ "$changed" = false ]; then
    echo 'The codex/opencode/claude/copilot ARG defaults were already current; both closures were still verified.'
fi
if [ "$agy_committed" != "$agy" ]; then
    echo "Next: set agy ${agy} in script/test/fixtures/installer-egress/selections.tsv (a human edit; the strict-egress audit compares it to the Dockerfile)."
fi
echo "Next: review 'git diff', run the installer tests / standard-image audit, then commit."
