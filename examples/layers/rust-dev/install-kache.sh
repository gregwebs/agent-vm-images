#!/usr/bin/env bash
# Install the pinned kache compiler cache (https://github.com/kunobi-ninja/kache).
#
# Runs both standalone and from examples/layers/rust-dev/Dockerfile, which passes
# the pins, the committed evidence directory and the install location.
#
# Environment:
#   KACHE_VERSION               required; the tagged release (e.g. 2.0.0)
#   KACHE_AMD64_SHA256          required; sha256 of the x86_64 static musl tarball
#   KACHE_ARM64_SHA256          required; sha256 of the aarch64 static musl tarball
#   KACHE_RELEASE_CERT_SHA256   required; sha256 of the committed release key
#   KACHE_DIR                   default /opt/kache
#   KACHE_DATA                  default kache/ beside this script
#
# Upstream publishes one static musl tarball per host plus a `.sha256` sidecar
# and a detached OpenPGP signature. The committed sidecar bytes are the reviewed
# evidence for the pinned digest; the committed key is the trust material. A
# changed digest, sidecar, key or signature is a hard failure: there is no
# fallback, no `latest` lookup and no unsigned path.
set -euo pipefail

: "${KACHE_VERSION:?KACHE_VERSION must be set to the pinned tagged release}"
: "${KACHE_AMD64_SHA256:?KACHE_AMD64_SHA256 must be set to the pinned tarball digest}"
: "${KACHE_ARM64_SHA256:?KACHE_ARM64_SHA256 must be set to the pinned tarball digest}"
: "${KACHE_RELEASE_CERT_SHA256:?KACHE_RELEASE_CERT_SHA256 must be set to the committed release key digest}"
KACHE_DIR=${KACHE_DIR:-/opt/kache}
HERE=$(cd "${BASH_SOURCE[0]%/*}" && pwd)
KACHE_DATA=${KACHE_DATA:-$HERE/kache}

printf '%s' "$KACHE_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
    { echo "install-kache: KACHE_VERSION must be a release version (got $KACHE_VERSION)" >&2; exit 1; }

arch=$(dpkg --print-architecture)
case "$arch" in
    amd64)
        triple=x86_64-unknown-linux-musl
        sha=$KACHE_AMD64_SHA256
        ;;
    arm64)
        triple=aarch64-unknown-linux-musl
        sha=$KACHE_ARM64_SHA256
        ;;
    *)
        echo "install-kache: unsupported architecture: $arch" >&2
        exit 1
        ;;
esac

asset="kache-${triple}.tar.gz"
printf '%s' "$sha" | grep -Eq '^[0-9a-f]{64}$' ||
    { echo "install-kache: the pinned digest for $arch is not a sha256" >&2; exit 1; }
url="https://github.com/kunobi-ninja/kache/releases/download/v${KACHE_VERSION}/${asset}"
sidecar="$KACHE_DATA/${asset}.sha256"
key="$KACHE_DATA/release-key.asc"
[ -f "$sidecar" ] || { echo "install-kache: missing evidence file: $sidecar" >&2; exit 1; }
[ -f "$key" ] || { echo "install-kache: missing release key: $key" >&2; exit 1; }

echo "==> install-kache: $KACHE_VERSION ($arch) into $KACHE_DIR"

# The committed key is the reviewed trust material, so a changed key fails here
# as loudly as changed archive bytes.
key_actual=$(sha256sum "$key" | cut -d' ' -f1)
[ "$key_actual" = "$KACHE_RELEASE_CERT_SHA256" ] ||
    { echo "install-kache: release key ${key##*/} hashing to $key_actual, not the pinned $KACHE_RELEASE_CERT_SHA256" >&2; exit 1; }

# The committed sidecar is the reviewed upstream publication for this digest;
# a pin left disagreeing with it means one of the two was edited by hand.
[ "$(cat "$sidecar")" = "$sha  $asset" ] ||
    { echo "install-kache: ${sidecar##*/} does not record the pinned digest $sha" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# kache writes its diagnostics under HOME even for `--version`, so every command
# here runs in the scratch home instead of the image's.
mkdir -p "$work/home" "$work/keyring"

GNUPGHOME="$work/keyring" gpg --batch --quiet --import "$key"
# One primary key, no secret material: the detached signature is verified
# against exactly the committed bytes.
[ "$(GNUPGHOME="$work/keyring" gpg --batch --with-colons --list-keys |
    awk -F: '$1 == "pub" { count++ } END { print count + 0 }')" -eq 1 ] ||
    { echo 'install-kache: the committed release key must hold exactly one primary key' >&2; exit 1; }
[ "$(GNUPGHOME="$work/keyring" gpg --batch --with-colons --list-secret-keys |
    awk -F: '$1 == "sec" { count++ } END { print count + 0 }')" -eq 0 ] ||
    { echo 'install-kache: the committed release key must not hold secret material' >&2; exit 1; }

for suffix in '' .sha256 .asc; do
    fetch_url="${url}${suffix}"
    echo "==> install-kache: fetch $fetch_url"
    curl -fSL --retry 3 -o "$work/${asset}${suffix}" "$fetch_url"
done

# The published sidecar is fetched so a re-published release that kept the
# tarball but changed the sidecar fails here instead of being absorbed; the
# digest check comes first because the pinned digest is the authority.
(cd "$work" && echo "$sha  $asset" | sha256sum -c -)
cmp -s "$work/${asset}.sha256" "$sidecar" ||
    { echo "install-kache: the published sidecar differs from ${sidecar##*/}" >&2; exit 1; }

# `--status-fd` is what makes this a check rather than a warning: gpg's advisory
# on stderr ("Good signature", trust warnings) is not parsed.
GNUPGHOME="$work/keyring" gpg --batch --status-fd 1 --verify "$work/${asset}.asc" "$work/${asset}" \
    >"$work/signature.status" 2>"$work/signature.err" ||
    { cat "$work/signature.err" >&2; echo 'install-kache: the release signature does not verify' >&2; exit 1; }
grep -q '^\[GNUPG:\] VALIDSIG ' "$work/signature.status" ||
    { echo 'install-kache: no valid signature over the release tarball' >&2; exit 1; }

# The archive layout is reviewed, so an extra member, an absolute name or a
# traversal is refused before anything is written. v2 installs a paired
# `kache-engine`; a tarball without it is not this release.
members=$(tar tzf "$work/$asset")
[ -n "$members" ] || { echo 'install-kache: the release tarball is empty' >&2; exit 1; }
for member in $members; do
    case "$member" in
        kache | kache-engine | kache.asc | kache-engine.asc) ;;
        *) echo "install-kache: unexpected archive member: $member" >&2; exit 1 ;;
    esac
done
for member in kache kache-engine; do
    printf '%s\n' "$members" | grep -qx "$member" ||
        { echo "install-kache: the release tarball lacks: $member" >&2; exit 1; }
done

echo "==> install-kache: extracting $asset"
mkdir -p "$work/unpack" "$KACHE_DIR/bin"
tar xzf "$work/$asset" -C "$work/unpack" kache kache-engine
install -m 0755 "$work/unpack/kache" "$KACHE_DIR/bin/kache"
install -m 0755 "$work/unpack/kache-engine" "$KACHE_DIR/bin/kache-engine"
# World-readable for the arbitrary-uid guest.
chmod -R a+rX "$KACHE_DIR"

# Both binaries report the pinned release; a paired version that drifted is
# refused. Run in the scratch home so no log or probe state survives.
for binary in kache kache-engine; do
    reported=$(HOME="$work/home" "$KACHE_DIR/bin/$binary" --version)
    [ "$reported" = "kache $KACHE_VERSION" ] ||
        { echo "install-kache: $binary reports '$reported', not 'kache $KACHE_VERSION'" >&2; exit 1; }
done

echo "==> install-kache: $KACHE_VERSION ($arch) installed from committed pins"
