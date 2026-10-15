#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)
work=$(mktemp -d)
work=$(cd "$work" && pwd -P)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/project" "$work/bin"
cp "$ROOT/images/tools/opencode/plugins/"{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256,index.js} "$work/project/"
cat >"$work/bin/pnpm" <<'SH'
#!/bin/sh
[ "$1" != --version ] || { echo 11.11.0; exit 0; }
[ -z "${AGENT_VM_TRANSPORT_RECEIPT:-}" ] || exit 91
printf '%s\n' "$*" >"$ARGV"
printf '%s\n' "$HOME" >"$PRIVATE_HOME"
mkdir -p node_modules/.pnpm
printf partial >node_modules/.pnpm/fixture
exit "${RESULT:-0}"
SH
chmod +x "$work/bin/pnpm"
export PATH="$work/bin:$PATH" ARGV="$work/argv" PRIVATE_HOME="$work/private-home"
export AGENT_VM_FIXTURE_ROOT="$work" AGENT_VM_FIXTURE_PNPM="$work/bin/pnpm"
export AGENT_VM_OPENCODE_PLUGINS_PREFIX="$work/project"
export AGENT_VM_CONTRACT_DIR="$ROOT/images/recipe-contract" AGENT_VM_OPENCODE_SOURCE_DIR="$ROOT/images/tools/opencode"
export AGENT_VM_TRANSPORT_RECEIPT="$work/receipt" AGENT_INSTALL_SOFT_FAIL=1
for result in 0 1 75 124 143; do
    if RESULT=$result sh "$ROOT/images/tools/opencode/install-opencode-plugins.sh" >"$work/log" 2>&1; then
        [ "$result" = 0 ]
        [ -f "$work/project/node_modules/.pnpm/fixture" ]
    else
        [ "$result" != 0 ]
        [ ! -e "$work/project/node_modules" ]
    fi
    [ ! -e "$(cat "$PRIVATE_HOME")" ]
    [ ! -e "$work/receipt" ]
    grep -Fq -- 'install --config.update-notifier=false --frozen-lockfile --prod --no-optional --ignore-scripts --ignore-pnpmfile --pm-on-fail=error --registry=https://registry.npmjs.org/ --package-import-method=copy --store-dir ' "$ARGV"
    grep -q -- '--reporter=ndjson' "$ARGV"
    for file in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do
        cmp "$ROOT/images/tools/opencode/plugins/$file" "$work/project/$file"
    done
done
for key in PNPM_CONFIG_TRUST_LOCKFILE PNPM_CONFIG_REGISTRY npm_config_registry PNPM_CONFIG_IGNORE_SCRIPTS PNPM_CONFIG_MINIMUM_RELEASE_AGE_EXCLUDE PNPM_HOME; do
    rm -f "$ARGV"
    if env "$key=unsafe" sh "$ROOT/images/tools/opencode/install-opencode-plugins.sh" >"$work/log" 2>&1; then exit 1; fi
    [ ! -e "$ARGV" ]
done
# The build hook does not honor the test seams, so no ambient override can point a
# build at a different checker or at the developer-only pin writer.
rm -f "$ARGV" "$work/sentinel"
printf '#!/bin/sh\ntouch "%s"\n' "$work/sentinel" >"$work/sentinel-pin"
chmod +x "$work/sentinel-pin"
env AGENT_VM_OPENCODE_LOCK_CHECK=/bin/false AGENT_VM_OPENCODE_LOCK_PIN="$work/sentinel-pin" \
    sh "$ROOT/images/tools/opencode/install-opencode-plugins.sh" >"$work/log" 2>&1 || { tail -5 "$work/log" >&2; exit 1; }
[ ! -e "$work/sentinel" ]
for file in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do
    cmp "$ROOT/images/tools/opencode/plugins/$file" "$work/project/$file"
done
printf 'opencode pnpm install: frozen argv, private-state cleanup, hard failures and ambient-policy rejection passed\n'
