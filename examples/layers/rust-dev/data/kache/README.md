# kache pin evidence

The committed evidence for the kache release `examples/layers/rust-dev/Dockerfile`
pins. The four `ARG`s are the pins; these are the upstream bytes a reviewer can
compare them with:

| File | What it is |
|---|---|
| `kache-x86_64-unknown-linux-musl.tar.gz.sha256` | the upstream `.sha256` sidecar published beside the `linux/amd64` static musl tarball |
| `kache-aarch64-unknown-linux-musl.tar.gz.sha256` | the same for `linux/arm64` |
| `release-key.asc` | the release certificate ("Kunobi Release Signing <feedback@kunobi.ninja>", RSA-4096, fingerprint `6069 18FD A7F1 854E 78B6 83CE 9A99 0E6B CBCC A73B`), published as <https://r2.kunobi.com/kache/apt/gpg.key> and also the key the vendor signs its apt repository with |

`../install-kache.sh` runs at build time with this directory mounted read-only. It
requires the certificate to hash to `KACHE_RELEASE_CERT_SHA256`, each sidecar to
record the pinned digest, the downloaded tarball to hash to it, the published
sidecar to be byte-identical to the committed one, and the detached signature to
verify against the committed certificate, before it extracts `kache` and
`kache-engine`.

To re-pin, on a maintainer host:

```sh
version=2.0.0   # the new release
base="https://github.com/kunobi-ninja/kache/releases/download/v$version"
for host in x86_64 aarch64; do
    curl -fsSLO "$base/kache-$host-unknown-linux-musl.tar.gz"
    curl -fsSLO "$base/kache-$host-unknown-linux-musl.tar.gz.sha256"
    curl -fsSLO "$base/kache-$host-unknown-linux-musl.tar.gz.asc"
    sha256sum -c "kache-$host-unknown-linux-musl.tar.gz.sha256"
    gpg --verify "kache-$host-unknown-linux-musl.tar.gz.asc" "kache-$host-unknown-linux-musl.tar.gz"
done
curl -fsSL https://r2.kunobi.com/kache/apt/gpg.key -o release-key.asc
sha256sum kache-*.tar.gz release-key.asc
```

Copy the two new sidecars and the certificate here, then move `KACHE_VERSION` and
all three digests in the Dockerfile together. `script/test/example-layers.sh`
refuses a pin that disagrees with these files, so a half-done bump fails before a
build.
