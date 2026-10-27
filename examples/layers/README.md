# Example Dockerfiles that extend a boot image

These directories are worked examples of building a **custom boot image** with
ordinary Docker. Each is a `Dockerfile` that starts `FROM ${BASE_IMAGE}` and
adds tools the default image does not carry — compilers, cross-toolchains,
Chromium, and so on. The explicit `agent-vm build` operation builds and imports one; launches never
build or compose these directories. Select the finished result separately. See
[Selecting the boot image](https://github.com/gregwebs/agent-vm/blob/main/USAGE.md#selecting-the-boot-image) and
[ADR-0035](https://github.com/gregwebs/agent-vm/blob/main/docs/adr/0035-consume-user-owned-boot-images.md).

The directories under `examples/layers/` are examples, not activated by default.
This repository also owns the maintained base/standard sources (`images/`) and a
[tool-free base extension](../base-extension/README.md). These examples require the
Debian-based standard image's apt/Node facilities; the minimal boot contract alone
is insufficient. Their default parent is the released standard index recorded in
[`images/parent-pins.json`](../../images/parent-pins.json) (version and
provenance there, digest in each recipe), overrideable with
`--build-arg BASE_IMAGE=…`; no local base build is needed.

### Build from an agent-vm-images checkout

Each example's directory is its own build context. Requires Docker with BuildKit
(the Go and Rust examples bind-mount their scripts) and a running Linux daemon; see
[Local FROM and builders](../../README.md#local-from-and-builders). From the
repository root:

```bash
BUILDER="$(docker context show)"
docker build --builder "$BUILDER" --platform linux/arm64 \
  -t agent-vm-chrome:dev examples/layers/chrome-devtools
```

Substitute `go-dev`, `rust-dev` or `wirenboard-cpp`; use `linux/amd64` on x86-64.
`agent-vm build` (below) builds the same context and also imports the result for
launching.

From this repository's root, build an example and select its finished result:

```sh
agent-vm build --builder native-oci \
  --build-arg BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de \
  --tag my-image:dev examples/layers/rust-dev
agent-vm shell --image my-image:dev                # or: image = "my-image:dev"
```

Every example needs the two lines below; the `ARG` is what
`--build-arg BASE_IMAGE=…` overrides:

```dockerfile
ARG BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de
FROM ${BASE_IMAGE}
```

Two conventions: expose environment through `ENV` (agent-vm reads the image's
OCI `ENV`), and leave `ENTRYPOINT`/`CMD` inert — agentd execs the agent
directly. These Debian-based examples are not portable to every minimal boot image.

## Index

| Example | Adds |
|---|---|
| [`wirenboard-cpp`](wirenboard-cpp/) | WB C/C++ build-essentials (debhelper, clang-format/clang-tidy, libcurl/libgtest/libmodbus/libsystemd-dev, cmake/ninja, ...) plus the armhf/arm64 cross toolchains, qemu-user-static, and the sbuild/schroot/debootstrap path. |
| [`rust-dev`](rust-dev/) | The Rust toolchain [agent-vm](https://github.com/gregwebs/agent-vm) pins (via its `rust-toolchain.toml`, with clippy and rustfmt), the host musl target, the native build libraries agent-vm's `ci.yml` installs, shellcheck, and the pinned Verus release — enough to build, test, lint and verify agent-vm's Rust code in the guest. The toolchain is installed from a reviewed pin set, not from the live installer. |
| [`go-dev`](go-dev/) | The pinned Go toolchain (go, gofmt, go vet) plus the two tools a Go project's editor and CI loop expect beyond it, `golangci-lint` and the `gopls` language server (built from a reviewed module closure, not `go install pkg@version`) — enough to build, test, lint and navigate a Go code base in the guest. |
| [`chrome-devtools`](chrome-devtools/) | Chromium, Chrome DevTools MCP wrapper, scoped NSS CA trust, and the Chrome capability marker. |

## Rust development

`rust-dev` is the layer that lets an in-VM agent iterate on agent-vm's own
Rust code. It installs, all under the world-readable `/opt`:

- the **pinned Rust toolchain** from agent-vm's `rust-toolchain.toml` (`1.98.1` at the
time of writing) with the `clippy` and `rustfmt` components, plus the host
`*-unknown-linux-musl` target the guest `agentd` build needs when building
the vendored `msb` (agent-vm's own gates do not);
- the native libraries agent-vm's `ci.yml` installs — `build-essential`, `pkg-config`,
`libcap-ng-dev`, `libdbus-1-dev` — plus `musl-tools` for that musl target, and
`shellcheck` for agent-vm's `script/test/ci-contracts.sh`;
- the **pinned Verus release** CI verifies with, on `linux/amd64` (see the
Apple Silicon note below).

Build it on top of the default boot image and select it:

```sh
agent-vm build --builder native-oci \
  --build-arg BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de \
  --tag agent-vm-rust-dev:dev examples/layers/rust-dev
agent-vm claude --image agent-vm-rust-dev:dev
```

Once booted, the guest can run agent-vm's Rust gates from an agent-vm checkout the way CI does. These
gates need no guest `agentd` build first: agent-vm drives an external `msb`,
and only `msb` embeds `agentd`.

```sh
cargo build --release -p agent-vm
cargo test -p agent-vm
cargo clippy --locked --workspace --all-targets -- -D warnings
cargo fmt --all -- --check
bash script/test/ci-contracts.sh
CARGO_TARGET_DIR=target/verus cargo verus verify --locked -p agent-vm
```

The toolchain lives under `RUSTUP_HOME=/opt/rustup` and the shims under
`/opt/cargo/bin`; both are `root:root` `0755` and shared by every guest uid, so a
non-root guest cannot write to them. `cargo`'s registry and build cache still
land in the guest's own writable `$HOME/.cargo`, so what the layer shares is only
the toolchain. A guest that runs as root still owns those directories, and an
explicit `rustup component add`, `rustup toolchain install` or `rustup update`
by such a guest is a user-triggered install: the pin policy does not cover it and
the layer does not prevent it when the guest has network access. What is
prevented is the implicit kind — automatic installation is disabled, so an
ordinary `cargo`, `rustc`, `cargo clippy` or `cargo fmt` invocation installs
nothing and fails locally instead (see [First use](#first-use)). The layer
pre-provisions exactly the pinned toolchain.

### Reviewed toolchain closure (#102)

Nothing in this layer resolves a channel or trusts the server's word for the
bytes. [`rust-toolchain/`](rust-dev/rust-toolchain/) commits, for both
`linux/amd64` and `linux/arm64`:

- the exact `rustup-init` (1.28.2) SHA-256 per host, with its original upstream
  `.sha256` sidecar as evidence;
- the original signed `channel-rust-<version>.toml` with its `.sha256` and
  detached `.asc`, plus Rust's release key;
- the SHA-256 and detached OpenPGP signature of the six selected archives:
  `rustc`, `cargo`, `rust-std`, `clippy-preview`, `rustfmt-preview` for the GNU
  host and `rust-std` for the matching musl target.

`rust_toolchain.py` verifies every committed digest, the manifest signature, the
upstream checksums, the archive signatures, each archive's ELF architecture and
rust-installer metadata, and only then lets rustup install from an
invocation-private `file://` layout built from the original manifest bytes. The
reasoning and the rejected alternatives are in
[ADR 0007](../../docs/adr/0007-rust-example-toolchain-pins.md).

`rustup-init` has no detached signature or attestation for 1.28.2, so its use is
a **scoped, dated, per-architecture hash-only exception** recorded in `pin.json`
(reviewer, UTC review date, exact URLs/digests, evidence URLs, reassessment
condition). It is not a claim that the bootstrap publisher is authenticated.

Updates are maintainer work on a host with network access, never a build step:

```sh
reviewed_on=$(date -u +%F)   # approval is dated when the review happens
bash script/dev/upgrade-rust-toolchain.sh 1.98.1 --rustup 1.28.2 \
  --key /absolute/reviewed/rust-key.gpg.ascii \
  --review-bootstrap-exception "$reviewed_on" --reviewer gregwebs
bash script/dev/upgrade-rust-toolchain.sh 1.98.1   # expected: unchanged
bash script/dev/upgrade-rust-toolchain.sh --check  # offline verification
```

The writer downloads and authenticates the whole two-architecture closure
without executing the foreign bootstrap and publishes the pins, the evidence and
the Dockerfile `ARG RUST_TOOLCHAIN` assertion as one transaction. A changed
bootstrap identity prints `hash-only bootstrap exception requires renewed
review` and publishes nothing until a human re-dates the exception. Status `3`
means an undecided rollback: recover the retained directory before further
commits.

`ARG RUST_TOOLCHAIN` is an exact-equal compatibility assertion, not a selector
for unreviewed bytes. A build that fails to fetch a committed URL fails; it does
not fall back.

### First use

The image exports `RUSTUP_AUTO_INSTALL=0` and persists rustup's
`auto-install disable` and `auto-self-update disable`, so an ordinary `cargo`,
`rustc`, `cargo clippy` or `cargo fmt` invocation that needs an absent toolchain,
tool or target fails locally instead of downloading. The selected channel, the
five host components and the musl standard library work with no Rust
distribution access. A project `rust-toolchain.toml` that only *declares* extra
components or targets may still build — rustup ignores those lists when
automatic installation is disabled — but a command that needs the absent item
fails. `RUSTUP_TOOLCHAIN` and `CARGO_HOME` are deliberately not exported: project
selection keeps working and cargo keeps its per-user cache. A guest that
explicitly runs `rustup install/add/update` is outside this guarantee.

### Native evidence

`bash script/test/rust-example.sh --platform linux/arm64|linux/amd64` on a
native daemon builds this recipe at the committed default parent, re-runs the
verifier with `--network none`, and exercises the toolchain as root and as numeric
uids with an empty writable `HOME`, a project `rust-toolchain.toml` and negative
first-use legs. Add `--no-cache` for acceptance evidence: a cached
toolchain-install stage proves nothing about the installer, so every run records
`build-toolchain-install cached=0|1` and a cached stage is called out in its log. Its first-use policy legs run on an `--internal` canary network
with writable clones of the shared homes, so a leg observes the policy rather than
the read-only permissions, and each runs as root as well as a numeric uid.
`linux/amd64` additionally proves Verus with an independent proof fixture and
shows that a wrong Verus digest fails the build before extraction; `linux/arm64`
shows the same digest stays a clean no-op with no fetch. Every command is bounded
by `host-watchdog.py`, the suite re-executes itself under a 45-minute supervisor,
cleanup has its own deadline and reports its own status, and
`--self-test` (which needs no daemon) proves those controls. Permanent example CI
stays with [#33](https://github.com/gregwebs/agent-vm-images/issues/33).

### Apple Silicon (no Verus asset)

Upstream publishes Verus for `linux/amd64` and `macos/arm64`, but **not**
`linux/arm64`. On an Apple Silicon host the guest is `linux/arm64`, so the
layer skips Verus; run the host's `arm64-macos` Verus there instead
([agent-vm CONTRIBUTING](https://github.com/gregwebs/agent-vm/blob/main/CONTRIBUTING.md#verifying-contracts-locally-optional)). Everything else in the
layer works on both architectures.

### Keeping the pins in lockstep

agent-vm owns the Rust channel and the Verus release/digest; this Dockerfile
mirrors them. Bump order and check coverage live in
[Example image ownership](../../docs/image-source-ownership.md#rust-and-verus-pins).
`script/test/example-layers.sh` guards the committed closure, the recipe shape,
ARG anchors and the Verus smoke shape statically; native amd64 builds exercise
Verus verification.

The layer's build steps are committed as standalone scripts beside the
Dockerfile — `install-rust.sh`, `install-verus.sh`, `verify-toolchain.sh` and
the build-only `rust_toolchain.py` — and bind-mounted in at build time, so each
can be read, reviewed and run on its own. They are guarded by
`script/test/example-layers.sh`, which `script/test/contracts.sh` runs.

## Go development

`go-dev` is the layer that lets an in-VM agent iterate on a Go code base. It
installs, under the world-readable `/opt`:

- the **pinned Go toolchain** from go.dev (`1.27.3` at the time of writing),
  verified against its per-architecture SHA-256 — `go`, `gofmt`, `go vet`,
  `go test`, and the rest of the standard distribution;
- **`build-essential`**, so cgo and `go test -race` work: with no C compiler
  present the Go command silently defaults `CGO_ENABLED=0`, and `-race` then
  fails with "requires cgo";
- the **pinned golangci-lint release** from its GitHub release tarball,
  likewise digest-verified;
- the **pinned `gopls` language server**, compiled at build time from a
  committed, reviewed module closure with the pinned compiler (upstream ships
  no binary) — see [Reviewed gopls closure](#reviewed-gopls-closure-103) below.

Build it on top of the default boot image and select it:

```sh
agent-vm build --builder native-oci \
  --build-arg BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de \
  --tag agent-vm-go-dev:dev examples/layers/go-dev
agent-vm claude --image agent-vm-go-dev:dev
```

Once booted, the usual Go loop works:

```sh
go version
go build ./...
go test ./...
go test -race ./...
golangci-lint run
gofmt -l .
gopls check main.go      # or just point your editor's LSP at `gopls`
```

The toolchain lives under `/opt/go` and the two extra tools in
`/opt/go-tools/bin`; both are on `PATH` and read-only for every guest uid.
Module downloads and build output stay in the guest's own writable, persistent
home (`$HOME/go/pkg/mod` and `$HOME/.cache/go-build`), so a second launch
reuses them — the layer shares only the toolchain, never a cache.

### `GOTOOLCHAIN=local`, and module downloads

The image sets `GOTOOLCHAIN=local`, so `go` never silently downloads a second
toolchain when a project's `go.mod` asks for a newer one: it fails with a
message naming both versions. Bump `GO_VERSION` (and its two digests) in
the layer's `Dockerfile` and rebuild the layer. To opt out for one command
where the network permits it, run `GOTOOLCHAIN=auto go …`.

Module, toolchain and checksum downloads a *project* triggers are user actions:
the build guarantee covers the image's own inputs, not a guest's `go get`,
`go install` or dependency resolution. Those commands need no extra flags — the
default network policy already reaches the public internet, which covers
`proxy.golang.org` and `sum.golang.org`. (`--allow-host` is unrelated — despite
the name it opens the *host's* loopback gateway for reaching a dev server, not a
hostname allow list.) A project that must not depend on the network can
`go mod vendor` and build with `-mod=vendor`.

### Reviewed gopls closure (#103)

`go install golang.org/x/tools/gopls@v0.23.0` used to let the `go` command pick
gopls's module graph during the build and accept checksum-database answers that
were never committed or reviewed here. The layer now commits the selection in
[`go-dev/gopls-closure/`](go-dev/gopls-closure/):

- a tiny wrapper module (`tool golang.org/x/tools/gopls`, every selected module
  as an indirect requirement) and the full tidy `go.sum` — 23 modules, 34 zip
  `h1` lines and 101 `/go.mod` lines for v0.23.0;
- the root module's own upstream `go.mod` bytes, hash-bound by their committed
  `/go.mod` line;
- `THIRD_PARTY_LICENSES`: the reviewed notice files (38 for v0.23.0), framed with
  per-file byte counts and digests;
- `pin.json`: the record that binds all four by digest and records the reviewed
  scope, the dated module-channel determination, the unauthenticated proxy
  provenance and the partial VCS signature inventory.

What a build proves: the committed bytes are the reviewed ones, the compiler is
the pinned one, and the finished binary's own build info reports exactly the
reviewed modules and sums — after which `gopls` and its licence file are
published. The `go build` runs as `-mod=readonly` in an environment
`gopls_closure.py` constructs (no inherited `GOFLAGS`/`GOPROXY`/`GONOSUMDB`,
`GOTOOLCHAIN=local`, `CGO_ENABLED=0`, `GOOS=linux` and the image's `GOARCH`,
private caches), so a new sum can only enter through a reviewed writer proposal.
What it does not prove: that the release is safe, or that the publisher
authorised the bytes. Module content is authenticated by `sum.golang.org` plus
the committed sums; available VCS release signatures are inventoried as evidence
and are not verified here (see
[#115](https://github.com/gregwebs/agent-vm-images/issues/115)).

Maintain it with the writer (maintainer host, never CI). The five closure files
and the `ARG GOPLS_VERSION` assertion are published as one transaction:

```bash
bash script/dev/upgrade-gopls.sh v0.23.0 \
  --review-determination YYYY-MM-DD --reviewer NAME   # date of a real human review
bash script/dev/upgrade-gopls.sh --check             # offline verification
```

A changed graph, sum set, notice layout or policy refuses with
`module-channel determination requires renewed review` and publishes nothing.
Decision and rejected alternatives:
[ADR 0008](../../docs/adr/0008-go-example-gopls-closure.md).

### Native evidence

`bash script/test/go-example.sh --platform linux/arm64|linux/amd64 --no-cache`
builds this layer on a native daemon and records the evidence: the cold build
with both retained archive `sha256sum -c` OK lines, the `--network=none`
installed-state verifier, the Go loop as root and as uid 12345, a real LSP
session (initialize, definition, hover, diagnostics, formatting, shutdown), the
first-use canary legs and a hard content-replacement build negative. QEMU is not
acceptance, and permanent example CI stays with
[#33](https://github.com/gregwebs/agent-vm-images/issues/33).

### Keeping the pins in lockstep

This is the *only* example here that pins a Go toolchain, so there is nothing to
cross-check against another repository. The Go version and, for the two prebuilt
downloads, the per-architecture digest are declared together in `Dockerfile`, and
each installer verifies its tarball against that digest, so a bumped version left
beside a stale digest fails the build.

A **Go or gopls bump goes through the writer**: `ARG GO_VERSION` and
`pin.json.go` are checked for equality, and gopls's selection, sums and notices
are regenerated by `script/dev/upgrade-gopls.sh`. Editing a version or a sum by
hand fails the static gate and the build. Because the reviewed Linux
build-settings allowlist is version-specific, a Go bump also needs a deliberate
helper/ADR change and a renewed review — it is not a cosmetic edit.

## Chrome DevTools

Build it on top of the default boot image and select it:

```sh
agent-vm build --builder native-oci \
  --build-arg BASE_IMAGE=ghcr.io/gregwebs/agent-vm-standard@sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de \
  --tag agent-vm-chrome:dev examples/layers/chrome-devtools
agent-vm claude --image agent-vm-chrome:dev
```

It installs Chromium, the Chrome DevTools MCP integration and one pinned
`chrome-devtools-mcp`; see
[agent-vm USAGE](https://github.com/gregwebs/agent-vm/blob/main/USAGE.md#chrome-devtools-mcp) for the runtime behavior.

### Pinned MCP package

The Dockerfile installs `chrome-devtools-mcp` at build time into
`/opt/agent-vm/chrome-devtools-mcp`. The inputs are four committed files in
[`chrome-devtools-mcp/`](chrome-devtools/chrome-devtools-mcp/): `package.json`
(one exact selection plus `packageManager: pnpm@11.11.0`),
`pnpm-workspace.yaml` (the install policy), `pnpm-lock.yaml` (a single-package
closure) and `pnpm-inputs.sha256` (SHA-256 of the other three). A bind-mounted
helper, [`chrome-devtools-mcp.sh`](chrome-devtools/chrome-devtools-mcp.sh), runs
the install; it is the only code in this example that starts pnpm.

The install is `pnpm install --frozen-lockfile --prod --no-optional
--ignore-scripts --ignore-pnpmfile`, with a private store, private
HOME/XDG/TMPDIR and a fixed PATH. The five
[runtime defaults](../../docs/node-package-defaults.md) — age 1440 minutes,
strict, missing publish time rejected, `no-downgrade`, scripts off — are
exported by the helper rather than inherited from the parent. The workspace keeps
`allowBuilds: {}`.

Any deviation fails the build: an unexpected or missing input, bytes that differ
from the pins, a selection that is not one canonical exact version, a policy
escape in the workspace file (any `minimumReleaseAge*`, `trustPolicy*`,
`onlyBuiltDependencies`, `overrides`, `registry` or similar key), a changed
dependency closure, a pnpm other than the `packageManager` pin, an ambient
package-manager or Node runtime variable, a failed install, or an installed tree
that fails the post-install invariant on ownership, modes, links, lock and pnpm's
recorded policy. There is no skip path and no build-time per-user cache: the
build ends with `/home/chrome` holding `.pki` only.

The apt line is unchanged — official Debian packages float behind apt
authentication. The MCP package, the parent image and the other third-party
inputs are the pinned ones. The parent stays the released standard digest recorded
in [`images/parent-pins.json`](../../images/parent-pins.json); a refreshed
release moves that digest and this example's `ARG` together through
`script/dev/refresh-parent-pins.sh standard-release`. The example supplies the
policy itself, and runtime needs no parent pnpm command.

### Runtime launcher

`/usr/local/bin/agent-vm-chrome-mcp` maps exactly two spellings onto the
installed file: `npx -y chrome-devtools-mcp@<installed version> ARGS…` (the
argv agent-vm launches) and the bare `chrome-devtools-mcp ARGS…`. Every later
word is passed through verbatim. Other direct package-manager commands are
refused with exit status 2: `npx` with any other package or spelling; `npm`,
`pnpm`, `pnpx`, `pnx`, `corepack`, `yarn`, `yarnpkg`, `bun`, `bunx`, including a
manager named by an explicit path; any command whose first word starts with `-`;
and an empty argv.

The guarantee covers the launcher's direct MCP entry: it executes the installed,
build-verified bytes and starts no package manager. Not covered: commands that
start a manager indirectly (`sh -c 'npx …'`, `env npx …`,
`sudo -u chrome npx …`, `node …/npm-cli.js …`), any other path the caller
names, the bin shim resolving `node` through the caller's `PATH`, user-requested
installs anywhere in the guest, and root inside the guest replacing or
overmounting `/opt`.

The prefix is outside HOME, so a HOME overmount cannot hide it: with
`--tmpfs /home/chrome` the wrapper still creates the NSS DB there and imports
the microsandbox CA, and with `--user 12345:12345 -e HOME=/tmp` it runs the
installed bin directly as that numeric UID.

### Maintaining the pin

Maintenance is maintainer-host only and never runs in CI:

```bash
bash script/dev/upgrade-chrome-devtools-mcp.sh [VERSION]
```

It resolves the lock inside the parent image, pins the bytes, requires the
published tarball's SHA-512 to equal the lock integrity, and verifies the npm
SLSA provenance under an explicit trusted identity — repository
`ChromeDevTools/chrome-devtools-mcp`, its publish workflow, issuer
`https://token.actions.githubusercontent.com`, source ref
`refs/tags/chrome-devtools-mcp-vX.Y.Z`, self-hosted runners denied, empty gh
config, no token — before publishing the five files as one transaction. Missing
or unverifiable provenance is a hard failure; there is no hash-only exception.
Prerequisites, statuses and recovery are in
[CONTRIBUTING](../../CONTRIBUTING.md#chrome-example-mcp-maintenance).

agent-vm launches `npx -y chrome-devtools-mcp@1.0.1`. Publishing another
version makes the wrapper refuse that argv until agent-vm changes it, so
coordinate the bump; the updater prints a warning for that case.

### Chrome runtime checks

Run the current-recipe suite with a native Docker daemon and its daemon-backed
`docker` builder:

```bash
bash script/test/chrome-example.sh
```

The [Chrome example (native) workflow](../../.github/workflows/chrome-example.yml)
builds and checks amd64 and arm64; both rows must be green before merge. The
suite checks parent exclusions, account and sudo scope, wrapper identity, numeric
UID, HOME overmount, NSS/CA import and headless Chromium. Evidence and phase
verdicts go to `target/chrome-example/`. Image-content checks remain in
`script/test/example-layers.sh`; launcher path agreement belongs to agent-vm
(see
[Example image ownership](../../docs/image-source-ownership.md#example-images-exampleslayers)).

Chromium's sandbox requires unprivileged user namespaces; Docker additionally
needs `--security-opt seccomp=unconfined`, which the suite supplies. Where these
are unavailable the check fails. Do not add `--no-sandbox`: that would stop
testing the sandbox the MCP uses.

Docker checks are not VM MCP integration proof; agent-vm's custom-image native
suite owns the launcher-side capability join. The retired API-1 composition
case is not revived: these examples are boot images, not launcher layers.

## Combining examples

To combine two examples, build one `FROM` the other with ordinary Docker (a
multistage or chained `docker build`), or copy the steps you need into one
Dockerfile. There is no launcher-side layering: the finished image is the one
you select.

## Provenance

Moved from agent-vm at `d5d8876` ([#23](https://github.com/gregwebs/agent-vm-images/issues/23));
bare `ADR-NNNN`/`#NNN` references in these files name agent-vm records, and earlier
history lives there. See
[Example image ownership](../../docs/image-source-ownership.md#example-images-exampleslayers).
