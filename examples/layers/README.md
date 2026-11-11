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

## CI builds

[`.github/workflows/example-layers.yml`](../../.github/workflows/example-layers.yml)
builds every context here on `linux/amd64` and `linux/arm64` when a change
touches it. Its `paths` list is a conservative superset of the build inputs, so an
unrelated change starts nothing;
[`script/ci/layer-changes.py`](../../script/ci/layer-changes.py) then narrows the
event to the contexts that changed — a layer's own directory selects that layer, a
shared build input (the shared watchdog, the apt helper, the parent inventory, the
workflow itself) selects all of them, and everything else selects none. A new
directory here with a `Dockerfile` joins the matrix with no workflow edit, while a
context that loses its recipe fails the selection instead of quietly disappearing.

Every row is a cold build. `script/test/layer-build.sh` pulls the committed,
digest-pinned released-standard parent on every run, refuses a daemon, host or
parent platform other than the requested native one, then builds with
`--no-cache`. A pin that upstream withdrew therefore fails a real fetch here with
the URL in the log, instead of being masked by a warm cache. The built image's
identity and platform are inspected afterwards, and the request labels of the
built image are compared with the layer's recipe and its inspected parent
([ADR 0014](../../docs/adr/0014-image-network-requests.md)), so a label that did
not reach the final stage fails the row. Only that run's own tag is removed.
`chrome-devtools` additionally keeps its runtime suite and the arm64
interrupted-run control.

What CI does not do: it publishes nothing — no tags, no release contract, no
provenance — and, apart from the network-request egress smoke below, no layer's
runtime suite runs except Chrome's. The deeper Rust and Go suites below stay
manual maintenance evidence. Decision and rejected alternatives, including why
both architectures build cold rather than from a cache:
[ADR 0010](../../docs/adr/0010-layer-build-ci.md).

Registering a new input is still reviewed work: a new third-party source is
registered in `images/parent-pins.json.recipes` (the source-policy gate requires
exact agreement with the discovered `Dockerfile`s), and a new script beside a
recipe is added to `script/test/example-layers.sh`'s guarded list.

## Declaring network requests

A user-owned layer is a component: it installs software, and that software
contacts hosts at runtime. Say which hosts on the layer's own label, so the
launcher can report a host the guest's egress does not cover. A request is
reported, never granted — see
[ADR 0014](../../docs/adr/0014-image-network-requests.md).

```dockerfile
FROM ${BASE_IMAGE}

# Runtime network requests (ADR 0014): cargo's sparse index, crate downloads and
# the crates.io API. Only this layer's own key: the parent's requests stay under
# their own keys.
LABEL org.agent-vm.network-requests.image.rust-dev="crates.io,index.crates.io,static.crates.io"
```

Use **your** component's name and never the parent's keys. A `FROM` child that
repeats a parent's key replaces the parent's value, which is why each component
has its own key. `image.base` belongs to the base recipe, `agent.claude` to the
standard image.

The value is a comma-separated list with no whitespace: exact lowercase
hostnames, optionally `:port` (not 443, 53 or 853), at most 64, sorted, no
wildcards or IPs. `""` means inventoried and contacts nothing by default; an
absent key means not inventoried. Do not derive it from an `ARG`: the hosts have
to be knowable from the source. `rust-dev` declares three hosts because `cargo`
uses `index.crates.io` for the sparse index, `static.crates.io` for downloads and
`crates.io` for the search/publish API; `apt` adds nothing, because Debian's
mirrors are plain HTTP and can never be a TLS request.

Check it before pushing:

```sh
python3 script/test/network_requests.py check-sources                 # the whole repository
python3 script/test/network_requests.py source-labels examples/layers/rust-dev/Dockerfile
# Your layer is checked against its own parent, so inspect that exact parent too
# and pass it with --inherits: a request label it carries is not your layer's, and
# without its inspect output the check reports every one of them as unexplained.
# PARENT_IMAGE is the pinned reference your FROM names, not your finished layer;
# this repository's layers keep it in the ARG BASE_IMAGE default.
PARENT_IMAGE=$(sed -n 's/^ARG BASE_IMAGE=//p' examples/layers/rust-dev/Dockerfile)
docker image inspect "$PARENT_IMAGE" > parent.json
docker image inspect my-layer:dev > i.json
python3 script/test/network_requests.py check-image --inspect i.json \
  --dockerfile examples/layers/rust-dev/Dockerfile --inherits parent.json
```

`--inherits` is what makes "a layer cannot erase its parent's requests" a check
rather than a claim: every request label of the parent must still be there with
the same value, and every label the layer adds must come from its own source. It
is vacuous only while the parent you build on predates the labels.

`contracts.sh` runs `check-sources` on every change, and CI runs `check-image`
on every built layer, so a label that is malformed, unsorted, in the wrong stage
or overwritten comes back as a named error.

### The egress smoke

A static label is a claim. For `rust-dev` and `go-dev`, CI also proves it:
[`script/test/network-requests-egress.sh`](../../script/test/network-requests-egress.sh)
reads the label off the built image, turns it into a strict proxy allow-list, and
runs the layer's own fixture command (`cargo fetch --locked`, `go mod download`)
in a container whose only route out is that proxy. The run passes when every TLS
destination the command reached is declared, and a second run with one used host
removed has to fail, which is what makes the gate load-bearing. It proves that
no undeclared host was contacted, not that every declared host is used.

A layer opts in by adding a fixture directory
`script/test/fixtures/network-requests/<layer>/` holding the command (`smoke.sh`)
and its project files; nothing else names the layer. That directory also selects
the larger CI bounds, because the phase runs inside the cold build. The Rust and
Go suites below stay manual.

## Index

| Example | Adds |
|---|---|
| [`wirenboard-cpp`](wirenboard-cpp/) | WB C/C++ build-essentials (debhelper, clang-format/clang-tidy, libcurl/libgtest/libmodbus/libsystemd-dev, cmake/ninja, ...) plus the armhf/arm64 cross toolchains, qemu-user-static, and the sbuild/schroot/debootstrap path. |
| [`rust-dev`](rust-dev/) | The Rust toolchain [agent-vm](https://github.com/gregwebs/agent-vm) pins (via its `rust-toolchain.toml`, with clippy and rustfmt), the host musl target, the native build libraries agent-vm's `ci.yml` installs, shellcheck, the pinned kache compiler cache every cargo invocation runs through, and the pinned Verus release — enough to build, test, lint and verify agent-vm's Rust code in the guest, reusing compiled crates across worktrees and launches. The toolchain and kache are installed from reviewed pin sets, not from the live installer. |
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
- **kache** (Apache-2.0, [kunobi-ninja/kache](https://github.com/kunobi-ninja/kache)),
installed as `RUSTC_WRAPPER` and as the C/C++ compiler shims on `PATH`, so every
cargo and C/C++ compile in the guest is cached (see
[kache build cache](#kache-build-cache));
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

### kache build cache

[kache](https://github.com/kunobi-ninja/kache) is a compiler cache for Rust (and
C/C++ and CUDA): it keys each rustc invocation on its inputs and restores
matching outputs across target directories, worktrees and launches, keeping one
copy of identical bytes. The layer installs both binaries of the paired release
under `/opt/kache/bin` and exports:

```sh
RUSTC_WRAPPER=/opt/kache/bin/kache
```

so the ordinary `cargo` loop is cached without a guest setup step, while the
shared `/opt` tree stays read-only.

C and C++ compiles are cached the same way, through a farm of compiler-name
shims the installer creates from the pinned binary:

```sh
PATH=/opt/kache/shims:/opt/kache/bin:/opt/cargo/bin:/opt/verus/verus-x86-linux:$PATH
```

`/opt/kache/shims` holds native symlinks named `cc`, `c++`, `gcc`, `g++`,
`clang`, `clang++` plus kache's `.kache-shims` marker, and it comes first on
`PATH` because a shim only works as the first compiler name the shell finds.
When kache is invoked that way it skips the marked directory, runs the real
compiler it finds later on `PATH`, and caches the object. This is the mechanism
the vendor packages ship (Debian's postinst runs `kache init --shims
/usr/lib/kache --force --canonical-only`; nixpkgs exports a `shims` output), and
it exists at build time, not as a guest step: `kache doctor` in a guest reports
`C/C++ shims: cc on PATH is a kache shim (/opt/kache/shims)`. The farm holds no
`cargo` or `rustdoc` link — Rust keeps running through `RUSTC_WRAPPER`, and the
`cargo` shim moves direct `cargo check`/`clippy` output to
`<workspace>/target/check` and holds off target cleanup, which this image does
not decide. `--canonical-only` also leaves versioned names (`gcc-13`,
`clang-19`) alone, as the Debian package does: they belong to the base image,
not to this layer.

The cache is split in two, because its halves have opposite requirements. The
**store** — the SQLite index, its WAL, the diagnostic log and the per-user daemon
— lives in the guest's own writable `$HOME/.cache/kache` and is never mounted:
that index needs single-writer local storage with working file locking, and a
shared or FUSE mount corrupts it. The **remote** is an immutable,
content-addressed blob tree, where write-once objects named by digest make
concurrent guests safe by construction rather than by locking.
`/opt/kache/config.toml` selects it, and `KACHE_CONFIG` points at that file, so a
guest needs no setup step:

```toml
[cache]
local_max_size = "20G"

[cache.remote]
type = "filesystem"
path = "/mnt/kache-remote"
```

`/mnt/kache-remote` is the contract with the launcher: it bind-mounts one host
directory there for every guest, so compiled crates are reused across VMs,
worktrees and launches without any guest sharing its store. A guest launched
without that mount still builds — `kache doctor` stops reporting a remote at all
rather than failing — and caches locally only. Collapsing the two halves by
mounting the store itself is the mistake that corrupts the index, so the image
leaves the mountpoint empty and guest-local.

The daemon is started by the first cache-enabled compile (no systemd user session
is needed, which is why `kache init`'s login service is not installed);
`kache daemon status`, `kache monitor` and `kache stats` show what it did, and
`kache stats --full` reports remote hits and uploads separately from local ones.

Turn it off for one command with `KACHE_DISABLED=1`, or for a whole session with
`env -u RUSTC_WRAPPER`; a build that must not go through the C/C++ shims takes
`/opt/kache/shims` off `PATH` or names the real compiler (`CC=/usr/bin/gcc`).
`kache init --no-service` adds what the image does not: test pacing, shell
completions, a login service, the `cargo` shim, and `--from-path` links for the
versioned compiler names. It is not needed for either cache to work.

`cargo verus` is unaffected by the wrapper: it sets `RUSTC_WRAPPER` to Verus's
own driver, which overrides the image value, so verification never runs through
the cache.

What it adds to the network: nothing. The remote is a bind-mounted filesystem,
not a service, so no host is contacted; the S3, GCS and OCI remote backends exist
but are not configured here. `org.agent-vm.network-requests.image.rust-dev`
therefore keeps the three crates hosts and the layer requests no new one.

#### Pinned inputs

The layer owns this selection: nothing else pins kache, so there is no mirrored
pin to follow. Four `ARG`s and [`kache/`](rust-dev/kache/) move together:
`KACHE_VERSION` plus the per-architecture tarball digest and the release
certificate's digest.
`kache/` commits the published `.sha256` sidecar for each static musl tarball and
the release key ("Kunobi Release Signing", RSA-4096, fingerprint
`6069 18FD A7F1 854E 78B6 83CE 9A99 0E6B CBCC A73B`, taken from
https://r2.kunobi.com/kache/apt/gpg.key).

[`install-kache.sh`](rust-dev/install-kache.sh) refuses a key that does not hash
to its pin, a sidecar that does not record the pinned digest, a tarball that does
not hash to it, a published sidecar that differs from the committed one, a
tarball whose detached OpenPGP signature does not verify against the committed
key, an archive member outside `{kache, kache-engine, kache.asc,
kache-engine.asc}`, and a binary that does not report `kache <KACHE_VERSION>`. A
refused download installs nothing; `layer-build.sh` exercises that against a
404 and against wrong bytes. The installer then creates the C/C++ shim farm from
the bytes it just verified and refuses any shape but the marker and the six
C/C++ links. The verifier re-checks both versions behind `RUSTC_WRAPPER`,
compiles and runs one C file through the shim, and runs `kache doctor --probe`
with no network: it compiles, stores and restores a small library in an isolated
cache and must report the shims active, so a broken wrapper, shim, daemon or
store fails the build instead of a guest.

Bump it on a maintainer host: download the new per-architecture tarballs, their
sidecars and the key, confirm the digests, replace `kache/` and move the four
`ARG`s together. Then run the native suite below. The decision and its rejected
alternatives are in [ADR 0015](../../docs/adr/0015-kache-compiler-cache.md).

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
first-use legs — each of those cargo runs also exercises kache as
`RUSTC_WRAPPER`, and each leg additionally compiles a C file through the shim, so
a broken wrapper, shim, daemon or store fails the suite. Add
`--no-cache` for acceptance evidence: a cached
toolchain-install stage proves nothing about the installer, so every run records
`build-toolchain-install cached=0|1` and a cached stage is called out in its log. Its first-use policy legs run on an `--internal` canary network
with writable clones of the shared homes, so a leg observes the policy rather than
the read-only permissions, and each runs as root as well as a numeric uid.
`linux/amd64` additionally proves Verus with an independent proof fixture and
shows that a wrong Verus digest fails the build before extraction; `linux/arm64`
shows the same digest stays a clean no-op with no fetch. Every command is bounded
by `host-watchdog.py`, the suite re-executes itself under a 45-minute supervisor,
cleanup has its own deadline and reports its own status, and
`--self-test` (which needs no daemon) proves those controls. That suite is the
deeper manual run and is not wired into CI; CI builds this layer cold on both
architectures ([CI builds](#ci-builds)).

### Apple Silicon (no Verus asset)

Upstream publishes Verus for `linux/amd64` and `macos/arm64`, but **not**
`linux/arm64`. On an Apple Silicon host the guest is `linux/arm64`, so the
layer skips Verus; run the host's `arm64-macos` Verus there instead
([agent-vm CONTRIBUTING](https://github.com/gregwebs/agent-vm/blob/main/CONTRIBUTING.md#verifying-contracts-locally-optional)). Everything else in the
layer works on both architectures.

### Keeping the pins in lockstep

agent-vm owns the Rust channel and the Verus release/digest; this Dockerfile
mirrors them, and this layer owns the kache selection (nothing else pins it).
Bump order and check coverage live in
[Example image ownership](../../docs/image-source-ownership.md#rust-verus-and-kache-pins).
`script/test/example-layers.sh` guards the committed closure, the recipe shape,
ARG anchors, the kache evidence, and the kache and Verus smoke shapes statically;
native amd64 builds exercise Verus verification, and every cold build on both
architectures proves the kache pins through the installer and the
`--network=none` probe.

The layer's build steps are committed as standalone scripts beside the
Dockerfile — `install-rust.sh`, `install-kache.sh`, `install-verus.sh`,
`verify-toolchain.sh` and the build-only `rust_toolchain.py` and
`kache_probe.py` — and bind-mounted
in at build time, so each can be read, reviewed and run on its own. They are
guarded by `script/test/example-layers.sh`, which `script/test/contracts.sh`
runs.

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
acceptance. This suite is not wired into CI; CI builds the layer cold on both
architectures ([CI builds](#ci-builds)).

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

The [Layer builds (native) workflow](../../.github/workflows/example-layers.yml)
runs this suite on amd64 and arm64 after the row's cold build; both rows must be
green before merge. The suite checks parent exclusions, account and sudo scope,
wrapper identity, numeric UID, HOME overmount, NSS/CA import and headless
Chromium. Run locally, evidence and phase verdicts go to `target/chrome-example/`.
In CI they go to `target/ci-logs/chrome-example/`, the arm64 interrupted-run
control's to `target/ci-logs/chrome-watchdog/`, and the cold build's own to
`target/ci-logs/layer-build/`. Image-content checks remain in
`script/test/example-layers.sh`; launcher path agreement belongs to agent-vm (see
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
