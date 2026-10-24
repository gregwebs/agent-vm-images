#!/usr/bin/env bash
# Single source of the trusted publisher identity for chrome-devtools-mcp (#41).
# Sourced -- never executed -- by script/dev/upgrade-chrome-devtools-mcp.sh
# (maintenance, maintainer host only) and by script/test/chrome-example.sh
# (phase A, which re-verifies the committed bundle). Nothing here writes
# repository state: the updater owns every write.
#
# No hash-only exception exists on purpose. A missing or unverifiable
# provenance bundle is a hard failure; adding an exception means a reviewed
# change to this file.
CHROME_MCP_TRUSTED_REPO=ChromeDevTools/chrome-devtools-mcp
CHROME_MCP_TRUSTED_WORKFLOW=ChromeDevTools/chrome-devtools-mcp/.github/workflows/publish-to-npm-on-tag.yml
CHROME_MCP_TRUSTED_ISSUER=https://token.actions.githubusercontent.com
CHROME_MCP_SLSA=https://slsa.dev/provenance/v1
CHROME_MCP_REGISTRY=https://registry.npmjs.org

# The single `integrity: sha512-...` of the lock, as lowercase hex.
chrome_mcp_lock_integrity_hex() {
    local lock=$1 value
    # v9 locks carry it inline: `resolution: {integrity: sha512-...}`.
    value=$(grep -o 'integrity: sha512-[A-Za-z0-9+/=]*' "$lock" | sed 's/^integrity: sha512-//')
    if [ -z "$value" ]; then
        echo "error: no sha512 integrity in $lock" >&2
        return 1
    fi
    if [ "$(printf '%s\n' "$value" | wc -l | tr -d ' ')" -ne 1 ]; then
        echo "error: more than one sha512 integrity in $lock" >&2
        return 1
    fi
    printf '%s' "$value" | base64 -d | od -An -v -tx1 | tr -d ' \n'
    printf '\n'
}

# Download the published tarball and prove it is the locked artifact.
chrome_mcp_fetch_tarball() {
    local version=$1 hex=$2 out=$3 got
    if ! curl -fsS --max-time 120 -o "$out" \
        "$CHROME_MCP_REGISTRY/chrome-devtools-mcp/-/chrome-devtools-mcp-$version.tgz"; then
        echo "error: cannot download chrome-devtools-mcp@$version" >&2
        return 1
    fi
    got=$(sha512sum "$out" | awk '{ print $1 }')
    if [ "$got" != "$hex" ]; then
        echo "error: chrome-devtools-mcp@$version tarball sha512 ${got} does not match the lock (${hex})" >&2
        return 1
    fi
}

# Fetch the compact Sigstore bundle of the SLSA attestation for VERSION.
chrome_mcp_fetch_bundle() {
    local version=$1 out=$2 json count
    if ! json=$(curl -fsS --max-time 120 \
        "$CHROME_MCP_REGISTRY/-/npm/v1/attestations/chrome-devtools-mcp@$version"); then
        echo "no SLSA provenance for chrome-devtools-mcp@$version; a hash-only exception needs a reviewed change to script/build/chrome-mcp-provenance.sh" >&2
        return 1
    fi
    count=$(printf '%s' "$json" |
        jq --arg p "$CHROME_MCP_SLSA" '[.attestations[]? | select(.predicateType == $p)] | length')
    if [ "$count" -ne 1 ]; then
        echo "error: expected exactly one SLSA provenance attestation for chrome-devtools-mcp@$version, found $count" >&2
        return 1
    fi
    printf '%s' "$json" |
        jq -ce --arg p "$CHROME_MCP_SLSA" '[.attestations[] | select(.predicateType == $p)][0].bundle' >"$out"
}

# Verify the bundle under the trusted identity and against the locked digest.
# An empty GH_CONFIG_DIR proves no stored credential takes part.
chrome_mcp_verify_bundle() {
    local tgz=$1 bundle=$2 version=$3 hex=$4 scratch=$5
    mkdir -p "$scratch/gh"
    if ! env -u GH_TOKEN -u GITHUB_TOKEN -u GH_ENTERPRISE_TOKEN -u GITHUB_ENTERPRISE_TOKEN -u GH_HOST \
        GH_CONFIG_DIR="$scratch/gh" GH_PROMPT_DISABLED=1 GH_NO_UPDATE_NOTIFIER=1 \
        timeout --kill-after=10 180 gh attestation verify "$tgz" --bundle "$bundle" --digest-alg sha512 \
        --repo "$CHROME_MCP_TRUSTED_REPO" --signer-workflow "$CHROME_MCP_TRUSTED_WORKFLOW" \
        --source-ref "refs/tags/chrome-devtools-mcp-v$version" --cert-oidc-issuer "$CHROME_MCP_TRUSTED_ISSUER" \
        --deny-self-hosted-runners --format json >"$scratch/verify.json"; then
        echo "chrome-devtools-mcp@$version: provenance does not verify under the trusted identity" >&2
        return 1
    fi
    if ! jq -e --arg n "pkg:npm/chrome-devtools-mcp@$version" --arg h "$hex" --arg p "$CHROME_MCP_SLSA" \
        'length == 1
         and .[0].verificationResult.statement.predicateType == $p
         and .[0].verificationResult.statement.subject == [{name: $n, digest: {sha512: $h}}]' \
        "$scratch/verify.json" >/dev/null; then
        echo "chrome-devtools-mcp@$version: verified statement does not name the locked tarball" >&2
        return 1
    fi
}
