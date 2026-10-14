#!/bin/sh
set -eu
export PYTHONDONTWRITEBYTECODE=1
SOURCE="${PNPM_SOURCE_DIR:-/tmp/pnpm-source}"
CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
python3 "$SOURCE/verify.py"
prefix=/opt/agent-vm/pnpm
node=/usr/bin/node
if [ -n "${PNPM_FIXTURE_DESTINATION:-}" ]; then
    prefix="$PNPM_FIXTURE_DESTINATION/opt/agent-vm/pnpm"
    node="$PNPM_FIXTURE_DESTINATION/usr/bin/node"
fi
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
want=$(python3 "$SOURCE/read-pin-version.py" "$SOURCE/pin.json")
sh "$CONTRACT/run-report.sh" 60 "$scratch/out" "$scratch/err" pnpm --version
[ "$(cat "$scratch/out")" = "$want" ] || {
    echo 'pnpm: exact report mismatch' >&2
    exit 1
}
python3 "$CONTRACT/check-tool-access.py" pnpm pnx pnpx "$node" \
    "$prefix/bin/pnpm.mjs" "$prefix/bin/pnpx.mjs"
mkdir "$scratch/home"
for pair in minimum-release-age:1440 minimum-release-age-strict:true \
    minimum-release-age-ignore-missing-time:false trust-policy:no-downgrade ignore-scripts:true; do
    key=${pair%:*}
    value=${pair#*:}
    got=$(env -u PNPM_CONFIG_MINIMUM_RELEASE_AGE -u PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT \
        -u PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME -u PNPM_CONFIG_TRUST_POLICY \
        -u PNPM_CONFIG_IGNORE_SCRIPTS -u XDG_CONFIG_HOME HOME="$scratch/home" \
        pnpm config get "$key")
    [ "$got" = "$value" ] || {
        echo "pnpm: default $key mismatch: $got" >&2
        exit 1
    }
done
