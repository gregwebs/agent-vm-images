# 0015: kache is a signature-verified layer input, enabled as `RUSTC_WRAPPER`

Status: accepted. Applies to [`examples/layers/rust-dev`](../../examples/layers/rust-dev);
the pin record is [image-source-ownership](../image-source-ownership.md#rust-verus-and-kache-pins).

## Context

`rust-dev` is the layer an in-VM agent uses to build, test and lint Rust. Every
launch of it starts from the same reviewed toolchain but an empty `target/`, so
the same crates are compiled again and again — the one cost the pinned toolchain
does not address.

[kache](https://github.com/kunobi-ninja/kache) is a compiler cache in that
position: it is invoked as Cargo's `RUSTC_WRAPPER`, keys each `rustc` invocation
on its inputs, and restores matching outputs from a content-addressed store. Its
store, index and daemon are per user and local by default; a remote backend is
opt-in. It is Apache-2.0, published for `x86_64` and `aarch64` Linux as static
musl tarballs plus `.sha256` and a detached OpenPGP signature, and each tarball
holds the paired `kache` and `kache-engine` binaries.

Three constraints shape the decision:

- **Image-input policy.** A pinned image input needs a committed selection and
  content identity; a version alone is not a pin
  ([ADR 0006](0006-third-party-artifact-pins.md)). `cargo install kache` would
  let the crate graph be resolved during the build and cannot be pinned without
  committing a whole registry closure.
- **The layer is an example, not a release.** It owns its own pins where nothing
  else selects them (like the Chrome MCP package), and mirrors agent-vm only for
  the values agent-vm publishes (the Rust channel and Verus).
- **The guest is an arbitrary uid** ([ADR-0001](https://github.com/gregwebs/agent-vm/blob/main/docs/adr/0001-arbitrary-uid-guest.md)).
  Shared state has to live under `/opt` and be read-only; per-user state must
  land in the guest's own writable `$HOME`.

## Decision

`rust-dev` installs the pinned kache release, exports
`RUSTC_WRAPPER=/opt/kache/bin/kache`, puts `/opt/kache/shims` and
`/opt/kache/bin` on `PATH`, and verifies the result with no network.

1. **Pinned release artifact, not a package or a build.** `ARG KACHE_VERSION`,
   the per-architecture tarball digest and the release-certificate digest are the
   pins; `kache/` commits the published `.sha256` sidecar for each host and the
   vendor certificate ("Kunobi Release Signing", RSA-4096, fingerprint
   `6069 18FD A7F1 854E 78B6 83CE 9A99 0E6B CBCC A73B`, published as
   https://r2.kunobi.com/kache/apt/gpg.key). `install-kache.sh` refuses a
   certificate that does not hash to its pin, a sidecar that does not record the
   pinned digest, a tarball that does not hash to it, a published sidecar that
   differs from the committed one, a tarball whose detached signature does not
   verify against the committed certificate, an unexpected archive member, and a
   binary that does not report the pinned version. Nothing resolves `latest`.
2. **Enabled by default for the whole image.** The layer exists to build Rust, so
   the wrapper is image `ENV` rather than a per-guest `kache init`. The escape
   hatch is `KACHE_DISABLED=1` (or `env -u RUSTC_WRAPPER`), and `kache
   init --no-service` remains available for the pieces the image does not
   provide — test pacing, completions, a login service, and the versioned
   compiler names the farm omits. The C/C++ compiler shims are image state too
   ([amended below](#amended-the-cc-shims-are-image-state-2026-11-05)): the
   installer creates `/opt/kache/shims` from the pinned binary and the image
   `PATH` leads with it, so Make, CMake and `cc`-crate objects are cached like
   rustc output.
3. **State is per guest.** Only the two binaries and the shim farm are shared.
   The store, index, diagnostic log and daemon live in `$HOME/.cache/kache`; the
   daemon is started by the first cache-enabled compile, because a guest has no
   systemd user session. What kache adds to the layer's declared network requests
   is nothing: the store is local and the image configures no remote.
4. **Verified where the image is built.** After installation the `--network=none`
   verifier re-checks both versions behind `RUSTC_WRAPPER`, compiles one C file
   through the shim, and runs `kache doctor --probe --json`, which compiles,
   stores and restores a small crate in an isolated cache. `kache doctor` exits 0
   with findings, so `kache_probe.py` decides the report: a non-optional failure,
   a failed probe or shims that are not active fails the build.
   `layer-build.sh` also proves that a 404 or wrong bytes leaves nothing
   installed.

## Consequences

The cost is a second cache layer inside the guest: `$HOME/.cache/kache` grows and
the store enforces its own size limit (kache's GC runs at daemon start and every
six hours). A guest that must not pay compiler-wrapper overhead can set
`KACHE_DISABLED=1`, and a build that must not go through the C/C++ shims takes
`/opt/kache/shims` off `PATH` or names the real compiler (`CC=/usr/bin/gcc`).
`cargo verus` is unaffected, because cargo-verus overrides `RUSTC_WRAPPER` with
Verus's own driver; an ordinary `cargo build` is not.

Trust is limited to what the pin establishes: that the reviewed bytes are the
vendor's signed release. It is not a claim that the release is safe, and the
certificate's authorship rests on TLS from the vendor's domain and on its use for
the vendor's apt repository, not on a web of trust. There is no transparency log
or attestation to check the way there is for the GitHub CLI
([ADR 0006](0006-third-party-artifact-pins.md)); that gap is recorded rather than
excused.

Rejected alternatives:

- **`cargo install kache --locked --version …`** — resolves and compiles a crate
  graph at build time with the pinned compiler: exactly the unpinned,
  graph-regenerating install the policy refuses, and it would not authenticate
  the publisher either.
- **The vendor's apt repository** — a third-party apt source is outside the
  official-Debian exception and would change the image's apt policy for one
  tool.
- **`kache init` in the image build** — writes into a `$HOME` that is not the
  guest's and offers a systemd user unit a guest cannot run. The `--shims DIR`
  form, which writes only a directory, is what the amendment below adopts.
- **Installing without enabling** — leaves the cache one undocumented guest step
  away and makes "the rust-dev image has kache" mean only that two binaries
  exist.
- **sccache or no cache** — sccache is a separate distribution with its own
  server model; no cache leaves the repeated-compile cost in place. This
  repository makes no measured comparison between them (see
  [the storage and benchmark reports](https://kunobi.ninja/docs/kache/benchmarks)
  for the vendor's own numbers).

## Amended: the C/C++ shims are image state (2026-11-05)

Decision 2 originally left every shim to a guest: the image exported
`RUSTC_WRAPPER` and `kache init --no-service` was documented as the way to add
the C/C++ shims. In practice that meant the image had no shim state at all. A
guest that ran `kache init` got a farm in its own `$HOME`, and `kache doctor`
then reported it as *"installed at ~/.local/lib/kache/shims, not first on PATH"*
— a farm no `PATH` leads with — while every launch that did not run init compiled
C and C++ uncached. "The rust-dev image caches C and C++ too" was true only as an
instruction.

**What changes.** `install-kache.sh` creates the farm at `/opt/kache/shims` with
the pinned binary it has just verified, and the image `PATH` leads with that
directory. This is the shape the vendor packages ship — Debian's postinst runs
`kache init --shims /usr/lib/kache --force --canonical-only`, and nixpkgs exports
a `shims` output to prepend — so the mechanism, the link names and the
`.kache-shims` marker are upstream's, not ours. Nothing new is downloaded, and
the layer's network requests do not change: the farm is symlinks to a binary
already installed.

Three properties make it image state rather than guest state. The farm is written
once, at build time, from the bytes the signature check passed. It lives under
`/opt`, which the arbitrary-uid guest (ADR 0001) reads and cannot write, so no
guest setup step exists to forget. And because `kache` skips any directory
carrying the `.kache-shims` marker when it looks for the real compiler, the shim
and the compiler it shadows can coexist on one `PATH` without recursion.

**What it does not include.** `kache init --shims` links kache's whole canonical
set, so the installer removes two links from the farm: `cargo` and `rustdoc`.
Rust keeps running through `RUSTC_WRAPPER` alone (decision 2), and the `cargo`
link is not a no-op — upstream documents that it moves direct `cargo check` and
`cargo clippy` output to `<workspace>/target/check` and holds off target cleanup
while a Cargo command runs. Either might be a good decision; neither is one this
ADR makes, and an image where `cargo check` writes somewhere other than
`target/debug` is a surprise for the agents that work in it. `--canonical-only`
likewise leaves versioned drivers (`gcc-13`, `clang-19`) unwrapped, as the Debian
package does: those names come from the base image, not from this layer, and a
guest that adds a compiler version should not silently inherit a shim that has no
compiler behind it.

**How it is held in place.** The farm's shape is asserted where it is built —
exactly the marker and the six C/C++ names, each a symlink to the pinned binary —
so a release that adds, drops or retargets a link fails the build instead of
changing which compiler a guest runs. The `--network=none` verifier then compiles
and runs one C file through the shim and makes `kache doctor`'s *C/C++ shims*
finding a required check in `kache_probe.py`: kache grades it optional because a
Rust-only setup may skip the farm, but this image installs it, so "not active" is
a regression here. `script/test/rust-example.sh` asserts the same from the guest
side, as an arbitrary uid: `PATH` leads with the farm, `cc` is the shim, and a C
file compiles through it with no network.

**Cost.** Every C and C++ compile in every guest now goes through kache, including
the ones outside Cargo (Make, CMake, `configure`, apt source builds). That is the
point: upstream runs the real compiler unchanged when it cannot prove an
invocation is safe to cache, so the failure mode is a cache miss, not a wrong
build. The escape hatches are `KACHE_DISABLED=1`, a `PATH` without the farm, or
naming the real compiler in `CC`/`CXX`.
