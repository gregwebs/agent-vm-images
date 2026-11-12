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
#   KACHE_SHIM_DIR              optional; create the C/C++ compiler-name shims here
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

# The C/C++ compiler-name shims, created by the pinned binary: native symlinks to
# it plus the `.kache-shims` marker, which is what makes kache skip the directory
# when it resolves the real compiler later on PATH. Doing it here, from the bytes
# that were just verified, is what keeps "the image has kache" from meaning "a
# guest must run kache init first" (ADR 0015); the marker and the links belong to
# the read-only /opt tree, so a guest writes nothing to activate them.
#
# The farm keeps the C/C++ names only. `kache init --shims` links its whole
# canonical set, which also holds `cargo` and `rustdoc`; this image keeps Rust on
# RUSTC_WRAPPER alone, and the cargo shim additionally moves direct `cargo
# check`/`clippy` output to <workspace>/target/check and holds off target
# cleanup. Neither is a decision this image makes, so those two links go and the
# resulting shape is asserted below rather than assumed.
if [ -n "$KACHE_SHIM_DIR" ]; then
    case "$KACHE_SHIM_DIR" in
        /*) ;;
        *) echo "install-kache: KACHE_SHIM_DIR must be absolute (got $KACHE_SHIM_DIR)" >&2; exit 1 ;;
    esac
    echo "==> install-kache: C/C++ shims in $KACHE_SHIM_DIR"
    # PATH decides which kache the links name: the first one on it is the stable
    # path this release is installed at, not a temporary or version directory.
    PATH="$KACHE_DIR/bin:$PATH" HOME="$work/home" \
        "$KACHE_DIR/bin/kache" init --shims "$KACHE_SHIM_DIR" --canonical-only
    rm -f "$KACHE_SHIM_DIR/cargo" "$KACHE_SHIM_DIR/rustdoc"
    chmod -R a+rX "$KACHE_SHIM_DIR"
    # Exactly the marker and the six C/C++ names, each a symlink to the pinned
    # binary: a release that adds, drops or retargets a link fails the build here
    # instead of silently changing the compiler every guest runs.
    installed=$(find "$KACHE_SHIM_DIR" -mindepth 1 -maxdepth 1 -printf '%f\n' |
        LC_ALL=C sort | tr '\n' ' ')
    [ "$installed" = '.kache-shims c++ cc clang clang++ g++ gcc ' ] ||
        { echo "install-kache: $KACHE_SHIM_DIR holds '$installed', not the marker and the six C/C++ shims" >&2; exit 1; }
    for name in cc c++ gcc g++ clang clang++; do
        if [ "$(readlink "$KACHE_SHIM_DIR/$name" 2>/dev/null)" != "$KACHE_DIR/bin/kache" ] ||
            [ ! -x "$KACHE_SHIM_DIR/$name" ]; then
            echo "install-kache: $KACHE_SHIM_DIR/$name is not an executable symlink to $KACHE_DIR/bin/kache" >&2
            exit 1
        fi
    done
fi

echo "==> install-kache: $KACHE_VERSION ($arch) installed from committed pins"
