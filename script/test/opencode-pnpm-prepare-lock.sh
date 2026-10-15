#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
SOURCE="$ROOT/images/tools/opencode/plugins"
work=$(mktemp -d)
work=$(cd "$work" && pwd -P)
trap 'rm -rf "$work"' EXIT
mkdir "$work/project" "$work/bin"
cp "$SOURCE/package.json" "$SOURCE/pnpm-lock.yaml" "$SOURCE/pnpm-workspace.yaml" "$SOURCE/pnpm-inputs.sha256" "$work/project/"
cat >"$work/bin/pnpm" <<'SH'
#!/bin/sh
printf '%s\n' "$*" >>"$CALLS"
if [ "$1" = --version ]; then printf '%s\n' "${MANAGER_VERSION:-12.9.1}"; exit 0; fi
exit 1
SH
cat >"$work/bin/npm" <<'SH'
#!/bin/sh
printf 'npm invoked\n' >>"$CALLS"
exit 99
SH
chmod +x "$work/bin/pnpm" "$work/bin/npm"
export PATH="$work/bin:$PATH" CALLS="$work/calls"
export AGENT_VM_FIXTURE_ROOT="$work" AGENT_VM_CONTRACT_DIR="$ROOT/images/recipe-contract"
for selection in '' 1.4.1; do
    sh "$SOURCE/prepare-lock.sh" "$work/project" "$selection"
    [ ! -e "$CALLS" ]
    for file in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do cmp "$SOURCE/$file" "$work/project/$file"; done
done
for bad in latest '^1.4.1' v1.4.1 1.4 '01.4.1' '1.4.1
1.4.2'; do
    if sh "$SOURCE/prepare-lock.sh" "$work/project" "$bad" >"$work/log" 2>&1; then exit 1; fi
    [ ! -e "$CALLS" ]
done
# A changed selection without --refresh-lock is refused before any manager runs:
# a build must never re-pin the bytes it verifies.
if sh "$SOURCE/prepare-lock.sh" "$work/project" 1.4.2 >"$work/log" 2>&1; then exit 1; fi
grep -Fq 're-pin deliberately with upgrade-plugins.sh' "$work/log"
[ ! -e "$CALLS" ]
: >"$CALLS"
if MANAGER_VERSION=11.11.0 sh "$SOURCE/prepare-lock.sh" "$work/project" 1.4.2 --refresh-lock >"$work/log" 2>&1; then exit 1; fi
! grep -q 'npm invoked' "$CALLS"
for file in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do cmp "$SOURCE/$file" "$work/project/$file"; done
if sh "$SOURCE/prepare-lock.sh" "$work" '' >"$work/log" 2>&1; then exit 1; fi
rm -f "$CALLS"
for key in PNPM_CONFIG_TRUST_LOCKFILE PNPM_CONFIG_TRUST_POLICY npm_config_registry NODE_OPTIONS; do
    if env "$key=unsafe" sh "$SOURCE/prepare-lock.sh" "$work/project" '' >"$work/log" 2>&1; then exit 1; fi
    [ ! -e "$CALLS" ]
done
# Reviewed bytes: a tampered input or pin is refused with no manager invocation.
cp "$work/project/pnpm-lock.yaml" "$work/lock-backup"
printf '\n' >>"$work/project/pnpm-lock.yaml"
if sh "$SOURCE/prepare-lock.sh" "$work/project" 1.4.1 >"$work/log" 2>&1; then exit 1; fi
grep -Fq 'bytes differ from the reviewed pin' "$work/log"
[ ! -e "$CALLS" ]
cp "$work/lock-backup" "$work/project/pnpm-lock.yaml"
cp "$work/project/pnpm-inputs.sha256" "$work/pins-backup"
printf 'not a pin\n' >>"$work/project/pnpm-inputs.sha256"
if sh "$SOURCE/prepare-lock.sh" "$work/project" 1.4.1 >"$work/log" 2>&1; then exit 1; fi
grep -Fq 'malformed line' "$work/log"
cp "$work/pins-backup" "$work/project/pnpm-inputs.sha256"
sh "$SOURCE/prepare-lock.sh" "$work/project" 1.4.1 >"$work/log" 2>&1
grep -Fq 'committed inputs reused' "$work/log"
printf 'opencode pnpm prepare-lock: equal selections reuse, changed selections require a deliberate re-pin, reviewed bytes enforced\n'
printf 'trust-lockfile=true\n' >"$work/project/.npmrc"
if sh "$SOURCE/prepare-lock.sh" "$work/project" '' >"$work/log" 2>&1; then exit 1; fi
[ ! -e "$CALLS" ]
echo 'opencode preparation: local no-op, invalid selections, manager pin, staged failure and prefix guard passed'
