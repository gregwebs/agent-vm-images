# agent-vm image sources

Editable Docker sources, independent of the agent-vm launcher:

- **Base:** Debian 13, Bash, Node 22, Python, Docker prerequisites, diagnostic
  utilities and zellij. No coding-agent CLI or fixed guest account.
- **Standard:** the base plus dsh, Pi, Codex, OpenCode, Claude and Copilot;
  pinned pnpm and the Pi Claude bridge are also mandatory.

```text
images/Dockerfile → agent-vm-base:local
                   ├─ images/standard/Dockerfile → agent-vm-standard:local
                   └─ your Dockerfile            → your Linux image
```

No launcher, project configuration, Rust or contributor tooling is required to
build. The base is never published; the standard image is published as an
independent, versioned, digest-addressed OCI product at
`ghcr.io/gregwebs/agent-vm-standard` plus matching archive/SBOM release assets.
OS/apt inputs and optional Claude plugins float; committed agent selections,
locks and reviewed installer snapshots are not silently refreshed.

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

The helper builds only the selected recipe, never auto-builds a missing base,
resolves latest, prunes, pushes or reads launcher configuration. Ordinary Docker
commands are the full customization interface; edit either Dockerfile or pass
`--build-arg BASE_IMAGE=your-local-base` to standard.

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
  --build-arg CA_SHIM_CACHEBUST="$(shasum -a 256 /path/to/ca.crt | awk '{print $1}')"
```

The certificate becomes **persisted trust material**. Do not bake a private
corporate CA into a public image. Secret contents are not cache keys; the hash
invalidates the trust RUN when they change. Builder/pull TLS trust is separate
from RUN-layer trust: this secret cannot fix daemon registry TLS errors. There
is no CA auto-detection, host-network mode or softened download failure.

## Extend and contribute

Build/run an ordinary numeric-UID [user-owned extension](examples/base-extension/README.md).
Scripts installed outside HOME survive HOME mounts. Use non-login `bash -c`
for agent commands: `bash -lc` can reset the image PATH through `/etc/profile`.

- [Contributing and verification](CONTRIBUTING.md)
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
