#!/usr/bin/env bash
# Exercise the owning four-file transaction without registry access.
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
work=$(mktemp -d)
work=$(cd "$work" && pwd -P)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/repo/images/tools" "$work/repo/images/standard" "$work/repo/script/build" "$work/bin" "$work/scratch"
cp -R "$ROOT/images/tools/opencode" "$work/repo/images/tools/"
cp -R "$ROOT/images/recipe-contract" "$work/repo/images/"
cp "$ROOT/images/standard/Dockerfile" "$work/repo/images/standard/"
cp "$ROOT/script/build/"{npm-pin,dockerfile-label,transactional-publish}.sh "$work/repo/script/build/"
cat >"$work/bin/pnpm" <<'SH'
#!/bin/sh
case "$1" in
    --version) echo 11.11.0 ;;
    view) echo '"1.4.2"' ;;
    install)
        node --input-type=module - <<'JS'
import fs from 'node:fs';
const lock = fs.readFileSync(process.env.SEED, 'utf8');
fs.writeFileSync('pnpm-lock.yaml', lock.replaceAll('1.4.1', '1.4.2'));
JS
        ;;
    *) exit 91 ;;
esac
SH
cat >"$work/bin/cp" <<'SH'
#!/bin/sh
for arg; do dest=$arg; done
if [ "$dest" = "$FAIL_DEST" ]; then
    if [ ! -f "$FAILED" ]; then
        echo partial >"$dest"
        touch "$FAILED"
        exit 1
    fi
    [ "${ROLLBACK_FAIL:-}" != 1 ] || exit 1
fi
exec "$REAL_CP" "$@"
SH
chmod +x "$work/bin/"*
REAL_CP=$(command -v cp)
export REAL_CP
export PATH="$work/bin:$PATH" SEED="$ROOT/images/tools/opencode/plugins/pnpm-lock.yaml"
export FAIL_DEST=none FAILED="$work/failed" TMPDIR="$work/scratch"
owner="$work/repo/images/tools/opencode/plugins"
upgrade="$owner/upgrade-plugins.sh"
# GitHub Actions must be refused before any write, so a runner is refused even
# when the selection is valid. Assert that here, then clear the variable for the
# transaction cases below, which exercise the developer path.
if GITHUB_ACTIONS=true bash "$upgrade" 1.4.2 >"$work/ci-log" 2>&1; then
    echo 'opencode plugin upgrade: expected refusal under GITHUB_ACTIONS' >&2
    exit 1
fi
grep -Fq 'refusing GitHub Actions' "$work/ci-log"
cmp "$ROOT/images/tools/opencode/plugins/package.json" "$owner/package.json"
# Successful changed selection publishes only owning inputs.
env -u GITHUB_ACTIONS bash "$upgrade" 1.4.2 >"$work/log" 2>&1
[ "$(jq -r '.dependencies["@openchamber/opencode-claude"]' "$owner/package.json")" = 1.4.2 ]
grep -Fq 'AGENT_VERSION_OPENCODE_CLAUDE:-1.4.2' "$work/repo/images/standard/Dockerfile"
# The regenerated lock is re-pinned deliberately and the committed checker then
# accepts the staged bytes; a pin that still matched the old lock would not.
[ "$(node "$owner/check-lock.mjs" "$owner")" = 1.4.2 ]
if cmp -s "$ROOT/images/tools/opencode/plugins/pnpm-inputs.sha256" "$owner/pnpm-inputs.sha256"; then
    echo 'opencode plugin upgrade: the pin was not refreshed with the new lock' >&2
    exit 1
fi
for mode in restored retained; do
    cp "$ROOT/images/tools/opencode/plugins/"{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} "$owner/"
    cp "$ROOT/images/standard/Dockerfile" "$work/repo/images/standard/"
    rm -f "$FAILED"
    export FAIL_DEST="$owner/pnpm-lock.yaml" ROLLBACK_FAIL=
    [ "$mode" != retained ] || export ROLLBACK_FAIL=1
    status=0
    env -u GITHUB_ACTIONS bash "$upgrade" 1.4.2 >"$work/log" 2>&1 || status=$?
    if [ "$mode" = restored ]; then
        [ "$status" = 1 ]
        for file in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do cmp "$ROOT/images/tools/opencode/plugins/$file" "$owner/$file"; done
        cmp "$ROOT/images/standard/Dockerfile" "$work/repo/images/standard/Dockerfile"
    else
        [ "$status" = 3 ]
        recovery=$(sed -n 's/^error: recovery material retained at //p' "$work/log")
        [ -f "$recovery/publish-backup/1" ]
        cmp "$ROOT/images/tools/opencode/plugins/pnpm-lock.yaml" "$recovery/publish-backup/1"
    fi
done
# The pin participates in the same transaction: a failure while publishing it
# restores every staged input, so a half-published pin can never ship.
cp "$ROOT/images/tools/opencode/plugins/"{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256} "$owner/"
cp "$ROOT/images/standard/Dockerfile" "$work/repo/images/standard/Dockerfile"
rm -f "$FAILED"
export FAIL_DEST="$owner/pnpm-inputs.sha256" ROLLBACK_FAIL=
status=0
env -u GITHUB_ACTIONS bash "$upgrade" 1.4.2 >"$work/log" 2>&1 || status=$?
[ "$status" = 1 ]
for file in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do cmp "$ROOT/images/tools/opencode/plugins/$file" "$owner/$file"; done
cmp "$ROOT/images/standard/Dockerfile" "$work/repo/images/standard/Dockerfile"
echo 'opencode plugin upgrade: five-input publication, inclusive rollback and status-3 recovery passed'
