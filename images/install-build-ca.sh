#!/usr/bin/env bash
# Host build-CA trust boundary (#95).
#
# Why: a CA supplied as a build secret becomes persisted trust material in the
# image, so it is installed only when the caller also states the SHA-256 of the
# certificate they reviewed. The hash is a build argument and therefore part of
# the build cache key: a changed hash reruns this step, while Docker may reuse a
# layer for changed secret bytes with an unchanged hash, so the hash is required
# rather than optional and a changed-secret proof needs --no-cache.
#
# CA_SHIM_CACHEBUST is gone. Docker silently ignores an undeclared build
# argument, so images/Dockerfile declares a tombstone only to reject that legacy
# invocation; it is not an alias and no legacy value is accepted.
#
# Structure, hash and key checks run before apt so a rejected secret never
# reaches the filesystem as trust material. Full X.509 parsing needs openssl,
# which the ca-certificates package provides, so it runs after the bootstrap
# install and before the certificate is written.
#
# BUILD_CA_TRUST_DIR, AGENT_VM_BASE_STAMP_DIR and APT_LISTS_DIR are fixture
# seams only: production reads the absolute paths below.
set -euo pipefail

if [ "$#" -ne 1 ]; then
    echo "usage: install-build-ca.sh SECRET_PATH" >&2
    exit 2
fi
secret=$1
reserved=__removed_use_BUILD_CA_SHA256__
trust_dir=${BUILD_CA_TRUST_DIR:-/usr/local/share/ca-certificates}
stamp_dir=${AGENT_VM_BASE_STAMP_DIR:-/opt/agent-vm/base}
apt_lists_dir=${APT_LISTS_DIR:-/var/lib/apt/lists}
stamp=$stamp_dir/build-ca-sha256
hash=${BUILD_CA_SHA256:-}

fail() {
    echo "install-build-ca: $*" >&2
    exit 1
}

[ "${REMOVED_CA_SHIM_CACHEBUST-$reserved}" = "$reserved" ] ||
    fail 'CA_SHIM_CACHEBUST was removed; use BUILD_CA_SHA256=<64-hex SHA-256 of hostca>'
# The fixture seam names the directory this step empties; anything that is not an
# apt lists directory would make the removal destructive somewhere else.
case "$apt_lists_dir" in
    */apt/lists | */lists) ;;
    *) fail 'APT_LISTS_DIR must name an apt lists directory' ;;
esac

supplied=false
if [ -f "$secret" ] && [ -s "$secret" ]; then
    supplied=true
fi

if [ "$supplied" = false ] && [ -z "$hash" ]; then
    mkdir -p "$stamp_dir"
    printf 'none\n' >"$stamp"
    echo '==> no build CA supplied; no certificate trusted'
    exit 0
fi
[ "$supplied" = true ] || fail 'BUILD_CA_SHA256 set but no hostca secret supplied'
[ -n "$hash" ] || fail 'BUILD_CA_SHA256 is required with --secret id=hostca'
case "$hash" in
    *[!0-9a-f]* | '') fail 'BUILD_CA_SHA256 must be 64 lowercase hexadecimal characters' ;;
esac
[ "${#hash}" -eq 64 ] || fail 'BUILD_CA_SHA256 must be 64 lowercase hexadecimal characters'

actual=$(sha256sum "$secret" | cut -d ' ' -f1)
[ "$actual" = "$hash" ] ||
    fail "hostca secret is not the reviewed certificate: expected $hash, got $actual"

# One certificate, nothing else: a bundle would trust more than the reviewed
# hash names, and a private key must never enter an image layer.
awk '
    { sub(/\r$/, "") }
    { trimmed = $0; gsub(/^[ \t]+/, "", trimmed); gsub(/[ \t]+$/, "", trimmed) }
    state == 0 && trimmed == "" { next }
    state == 0 && trimmed == "-----BEGIN CERTIFICATE-----" { state = 1; certificates++; next }
    state == 1 && trimmed == "-----END CERTIFICATE-----" { state = 2; next }
    state == 1 && trimmed == "" { next }
    state == 1 && trimmed !~ /^[A-Za-z0-9+\/=]+$/ { bad = 1 }
    state == 1 { body++; next }
    trimmed != "" { bad = 1 }
    END {
        if (bad || certificates != 1 || state != 2 || body == 0) exit 1
    }
' "$secret" || fail 'hostca must contain exactly one PEM certificate and no private key'

scratch=$(mktemp "${TMPDIR:-/tmp}/build-ca.XXXXXX") || fail 'cannot create a scratch file'
trap 'rm -f "$scratch"' EXIT
awk '/-----BEGIN CERTIFICATE-----/ { inside = 1; next }
     /-----END CERTIFICATE-----/ { inside = 0; next }
     inside' "$secret" | tr -d ' \t\r' | base64 -d >"$scratch" 2>/dev/null ||
    fail 'hostca must contain exactly one PEM certificate and no private key'
[ -s "$scratch" ] || fail 'hostca must contain exactly one PEM certificate and no private key'

apt-get update
apt-get install -y --no-install-recommends ca-certificates
if ! openssl x509 -noout -in "$secret" >/dev/null 2>&1; then
    fail 'hostca is not a parseable X.509 certificate; nothing was installed'
fi

install -d "$trust_dir"
install -m 0644 "$secret" "$trust_dir/agent-vm-build.crt"
update-ca-certificates
rm -rf "${apt_lists_dir:?}"/*
mkdir -p "$stamp_dir"
printf 'sha256:%s\n' "$hash" >"$stamp"
echo "==> build CA trusted for this build (sha256:$hash)"
