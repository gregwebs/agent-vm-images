# agent-vm image sources

Editable Docker sources, independent of the agent-vm launcher:

- **Base:** Debian 13, Bash, Node 22 (pinned release archive), GitHub CLI
  (pinned release archive), Python, Docker prerequisites, diagnostic
  utilities, zellij and pinned pnpm. No coding-agent CLI or fixed guest account.
- **Standard:** the base plus dsh, Pi, Codex, OpenCode, Claude and Copilot;
  inherited base pnpm and the Pi Claude bridge are also mandatory.
- **Examples:** user-owned Dockerfiles built from the base or the released standard image; see [Examples](#examples).

```text
images/Dockerfile → agent-vm-base:local
                   ├─ images/standard/Dockerfile → agent-vm-standard:local
                   └─ your Dockerfile            → your Linux image
```

No launcher, project configuration, Rust or contributor tooling is required to
build. The base is never published; the standard image is published as an
independent, versioned, digest-addressed OCI product at
`ghcr.io/gregwebs/agent-vm-standard` plus matching archive/SBOM release assets.
External parents are digest-pinned (see [image input policy](docs/image-input-policy.md)):
Debian base, Go helper compiler and the released standard image. The local
`agent-vm-base:local` edge is bound by a committed
[base-input manifest](images/base-inputs.sha256): the base stamps the manifest
hash of this checkout, and `images/standard/Dockerfile` and
`examples/base-extension/Dockerfile` refuse a base whose stamp disagrees. That is
source identity, not image-byte identity — see
[the ADR](docs/adr/0005-repository-base-identity.md). Official Debian
apt packages may float with normal apt authentication; Node and the GitHub CLI are
[pinned release artifacts](docs/image-input-policy.md#pinned-third-party-release-artifacts)
instead of apt or setup-script inputs (see
[ADR 0006](docs/adr/0006-third-party-artifact-pins.md)), and the optional Claude
plugins remain owned by [#101](https://github.com/gregwebs/agent-vm-images/issues/101).
Committed agent selections, locks and reviewed installer snapshots are not
silently refreshed.

## Clone and build

Requires modern Docker/BuildKit (bind/secret mounts and COPY chmod), a running
Linux daemon, upstream download access and sufficient daemon storage. From an
Apple Silicon host:

```bash
git clone https://github.com/gregwebs/agent-vm-images.git
cd agent-vm-images
BUILDER="$(docker context show)"
docker buildx inspect "$BUILDER"    # require Driver: docker

docker build --builder "$BUILDER" --platform linux/arm64 \
  -t agent-vm-base:local -f images/Dockerfile images
docker build --builder "$BUILDER" --platform linux/arm64 \
  -t agent-vm-standard:local --build-arg BASE_IMAGE=agent-vm-base:local \
  -f images/standard/Dockerfile images
```

Use `linux/amd64` on x86-64 Linux. Both maintained recipes use **`images/` as
context**, not a tool directory. The standard command installs all six agents
and fails if any required install, report, access check or final certification
fails. Optional Claude LSP downloads are bounded and log failures explicitly.
A failed rebuild leaves a previous successful Docker tag intact; that old tag
is not evidence that the new build succeeded.

The equivalent convenience commands (native client platform only) are:

```bash
bash images/build.sh base
bash images/build.sh standard
# Additional ordinary Docker options, as argv:
bash images/build.sh standard -- --build-arg AGENT_VERSION_CLAUDE=2.1.286 --progress=plain
```

The default needs Docker only. For optional host Go compilation, use
`--host-helpers`; see [contributor commands](CONTRIBUTING.md#go-build-helpers)
and [ADR 0004](docs/adr/0004-go-build-helpers.md).

The helper builds only the selected recipe, never auto-builds a missing base,
resolves latest, prunes, pushes or reads launcher configuration. Ordinary Docker
commands are the full customization interface. Standard accepts only a base whose
`/opt/agent-vm/base/source-identity` stamp equals the identity of this checkout's
committed manifest, and it has no override: `--build-arg BASE_IMAGE=your-local-base`
is for another tag built from this checkout's `images/` sources. The copied
extension carries one escape hatch, `--build-arg AGENT_VM_BASE_IDENTITY=<identity>`,
for a base deliberately built from a different tree. Editing a file the base
consumes is therefore a reviewed source change, not a rebuild:

```bash
bash script/build/base-inputs.sh --check   # drift gate; names the differing inputs
# edit images/… (new files: git add them first — the selection is the git index)
bash script/build/base-inputs.sh --write   # manifest + extension default in one transaction
git add images/base-inputs.sha256 examples/base-extension/Dockerfile
bash images/build.sh base && bash images/build.sh standard
```

See [repository base identity](docs/adr/0005-repository-base-identity.md) for the
trust boundary and [image input policy](docs/image-input-policy.md#repository-base-identity-a-committed-input-manifest)
for the recovery commands.

### Local FROM and builders

The context-named builder is `colima` on the development host, not universally
`default`. Inspect `docker context show` and `docker buildx ls`. If the named
builder is unavailable or not `docker`, stop and select the actual daemon-backed
builder for that context with ordinary commands. Do not create a new builder or
change endpoints to make the reference resolve.

A `docker` driver shares the daemon image store: `docker build` loads results
there and ordinary local `FROM` resolves. Equivalent explicit buildx usage is:

```bash
docker buildx build --builder "$BUILDER" --platform linux/arm64 --load \
  -t agent-vm-base:local -f images/Dockerfile images
docker buildx build --builder "$BUILDER" --platform linux/arm64 --load \
  -t agent-vm-standard:local --build-arg BASE_IMAGE=agent-vm-base:local \
  -f images/standard/Dockerfile images
```

`docker-container` builders need `--load` to run results in the daemon, but
loading the base does **not** give that builder access to a later local-only
FROM: it may still try a registry. Cache-only outputs are not local images.
Remote builders cannot assume access to the client's daemon store. Build one
host-compatible Linux platform at a time; multi-platform load and `--pull` on
the local base are not this quick start. No parent-image transport workaround
or registry is needed for the daemon-backed path.

### Optional build trust

An explicitly supplied public CA can be added with ordinary base-build options:

```bash
bash images/build.sh base -- --secret id=hostca,src=/path/to/ca.crt \
  --build-arg BUILD_CA_SHA256="$(sha256sum /path/to/ca.crt | cut -d ' ' -f1)"
```

The hash is **required**: it is the reviewed identity of the certificate, and
reviewing the CA file before you select its hash is the trust decision. A secret
without a hash, a hash without a secret, a malformed hash, a certificate that
contains anything other than exactly one PEM certificate, and a hash that does
not match the secret all fail before apt runs or trust is written. A successful
build installs `/usr/local/share/ca-certificates/agent-vm-build.crt` (0644),
refreshes the bundle and records `sha256:<hash>` in the image.

`CA_SHIM_CACHEBUST` is removed, and no legacy value is accepted: Docker ignores
undeclared build arguments it does not know, so the base declares a tombstone
only to reject the old interface. A legacy invocation fails with
`CA_SHIM_CACHEBUST was removed; use BUILD_CA_SHA256=<64-hex SHA-256 of hostca>`.

The certificate becomes **persisted trust material**. Do not bake a private
corporate CA into a public image. Secret bytes are not part of the build cache
key: the hash is, so a changed hash reruns verification while changed bytes with
an unchanged hash can reuse a previous valid layer. Prove a mismatch with
`--no-cache`; do not read a cached `0` as proof about new secret bytes. The base
records the hash in the `org.agent-vm.build-ca.sha256` label, and the public
release build refuses a base that carries a nonempty one or that omits the
label, and refuses `--label org.agent-vm.build-ca.sha256=...` on the build it
runs. Builder/pull TLS
trust is separate from RUN-layer trust: this secret cannot fix daemon registry
TLS errors. There is no CA auto-detection, host-network mode or softened
download failure. Persisted shim certificates survive distro CA updates; see the
[root-store advisory response](docs/ca-trust-response.md).

## Examples

Ordinary user-owned Dockerfiles; each README is the canonical build guide.

- [Base extension](examples/base-extension/README.md): tool-free, numeric-UID
  extension `FROM` your local base build.
- [Tool examples](examples/layers/README.md): `chrome-devtools`, `go-dev`, `rust-dev`
  and `wirenboard-cpp`, each `FROM` the released, digest-pinned standard image with its
  own directory as build context (moved from agent-vm; see
  [example ownership](docs/image-source-ownership.md#example-images-exampleslayers)).
  CI builds each one cold on both native architectures when a change touches it — see
  [CI builds](examples/layers/README.md#ci-builds) and
  [ADR 0010](docs/adr/0010-layer-build-ci.md); they are still examples, not published
  products.

## Extend and contribute

Scripts installed outside HOME survive HOME mounts. Use non-login `bash -c`
for agent commands: `bash -lc` can reset the image PATH through `/etc/profile`.

- [Pinned parent images and the apt exception](docs/image-input-policy.md)
- [Committed base-input manifest decision](docs/adr/0005-repository-base-identity.md)
- [Node package defaults and override limits](docs/node-package-defaults.md)
- [dsh graph-freeze decision](docs/adr/0003-dsh-graph-freeze.md)
- [Go build helpers and their delivery seam](docs/adr/0004-go-build-helpers.md)
- [Standard pnpm migration evidence](docs/standard-pnpm-migration.md)
- [Contributing and verification](CONTRIBUTING.md)
- [Root-store advisory response](docs/ca-trust-response.md)
- [Standard image releases](docs/standard-image-releases.md)
- [Installer security, locks and runtime payloads](images/tools/README.md)
- [Layout, migration manifest and staged ownership](docs/image-source-ownership.md)

Maintenance, hermetic audits and native non-publishing CI live in this repository.
See [CONTRIBUTING](CONTRIBUTING.md) for commands and acceptance evidence. Fast tests
are not proof of full standard installation: both native architecture builds and
mandatory runtime/strict-egress audits must pass before merge. Publication is a
separate maintainer-only workflow; a merged workflow or fixture is not released
acceptance. See [standard image releases](docs/standard-image-releases.md) for the
version contract, published registry/archive interface and promotion gates.
