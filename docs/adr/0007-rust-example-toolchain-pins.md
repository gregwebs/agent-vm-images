# ADR 0007: pin the Rust example's bootstrap, manifest and selected payloads

Status: accepted (#102).

## Context

`examples/layers/rust-dev` installed Rust by downloading `https://sh.rustup.rs`
and letting rustup resolve the channel `1.98.1`. A channel name selects a
release; it identifies no bytes. The bootstrap script, the signed distribution
manifest and every component archive were therefore mutable inputs to an
image-owned build, which the [image input policy](../image-input-policy.md)
forbids. Pinning a version string, or trusting the server's manifest for hashes,
leaves the same hole: versioned URLs can be rewritten and nothing reviewed says
which bytes are expected.

The example must still install a real rustup toolchain: agent-vm's workflow
depends on rustup's component bookkeeping, `+toolchain` selection and shim
behaviour, and on the separate digest-pinned amd64-only Verus install.

## Decision

Commit a reviewed finite closure and let rustup install only from it.

- `examples/layers/rust-dev/rust-toolchain/` holds the exact per-architecture
  `rustup-init` digest (with its original `.sha256` sidecar bytes as evidence),
  the original `channel-rust-<version>.toml` plus its `.sha256` and detached
  `.asc`, Rust's release key, and the SHA-256 and detached signature of every
  selected archive for both `linux/amd64` and `linux/arm64`.
- The selected closure is exactly six archives per architecture: `rustc`,
  `cargo`, `rust-std`, `clippy-preview`, `rustfmt-preview` for the GNU host and
  `rust-std` for the matching musl target. The selection is derived from the
  signed manifest's minimal profile plus the explicitly installed clippy/rustfmt
  and the musl target, and must equal the committed list. Derivation follows
  rustup 1.28.2's own rules, not an approximation of them: `src/dist/manifest.rs`
  *merges* the host's `extensions` rows into its `components` vector (forcing
  `is_extension = true`) and every later lookup searches that one merged table,
  so `get_profile_components` finds each profile name — the first row with that
  name whose target is the host or the target-independent `*` — and keeps that
  row's own target; `src/dist/mod.rs` resolves the explicit clippy/rustfmt by
  rename, promotes one to target-independent only when the merged table's first
  row for that package has no target, and adds the explicitly requested musl
  `rust-std`; the results are deduplicated by package and target. There is no
  second, independent table and no `get_extension_components` in 1.28.2.
  Because the reviewed pin can only describe concrete archives, a
  target-independent selection (for example a coherent wildcard `rust-mingw`
  extension with a wildcard package, which rustup would install as a seventh
  component) is refused, as is any derived set other than the committed six.
  The explicit components/targets must also still be declared in the host's own
  extension table. A changed profile, rename, removed or retargeted
  component/extension, an added host extension, or a newly required Linux
  component stops maintenance for review instead of silently widening or
  narrowing the closure. Component rows are `{pkg, target}` only: rustup's
  `is_extension` is forced true by the merge and no profile lookup filters on
  it, and the singular `extension` key is not a rustup field at all (its TOML
  deserializer ignores unknown keys), so neither can disable a row here.
- The standalone installer refuses both install homes unless they are absent and
  every existing ancestor is a real directory: a symlinked ancestor would
  redirect the installation, and the failure cleanup that removes only
  invocation-created homes, outside the tree the caller named. Any unsuccessful
  exit — a refusal, a failed `execve`, or a home that could not be created —
  removes the homes this invocation created, so a retry starts clean.
- Only rustup **1.28.2** is an audited installer. Its format preference, install
  argv and hash-only bootstrap exception are properties of that version, so a
  different `--rustup` value is refused before any fetch and needs a code and
  policy review rather than inheriting this record's rationale.
- `rust_toolchain.py` verifies all of that before anything executes: strict pin
  decoding, canonical origins computed independently from the validated channel
  and date, real gpg under a private keyring against the committed primary
  fingerprint, bounded streaming HTTPS, hash-before-chmod/exec, ELF architecture
  checks, and rust-installer format-3 metadata/coverage preflight. It then builds
  an invocation-private `file://` dist layout from the original manifest bytes and
  runs rustup with automatic installation and self-update disabled. Ordinary builds never resolve, rewrite
  or repair a pin. The transport's whole-request deadline is a watchdog that
  interrupts the request's socket (`shutdown(SHUT_RDWR)`) from another thread:
  a socket timeout only bounds inactivity, so a peer that stalls *inside* a read
  outlives the budget, and closing the response does not cancel it either --
  `HTTPResponse.close()` waits on the same buffered-reader lock the in-progress
  read holds. Only interrupting the socket wakes the blocked read.
- `script/dev/rust-toolchain-update.py` + `script/dev/upgrade-rust-toolchain.sh`
  are the only writers. A run downloads and authenticates the entire
  two-architecture closure (never executing the foreign bootstrap) into scratch
  and publishes the fixed destination list as one transaction with the
  existing publisher. An incomplete download, a bad signature or a refused
  endpoint leaves the committed sources byte-identical.
- Signed archives are verified with OpenPGP, so they need no exception. The
  `rustup-init` binaries have no detached signature, attestation or release
  object for 1.28.2 (`.asc` endpoints return 404, the GitHub tag has no
  release), so `pin.json` records a **scoped, dated, per-architecture hash-only
  exception** with the exact URLs and digests, the upstream `.sha256` and
  installation-document evidence, the reviewer, and a reassessment condition.
  A changed bootstrap identity requires an explicit renewed review — the writer
  refuses to carry the old approval forward and never renews the date itself.

## Consequences

- Withdrawn or changed upstream inputs fail builds; payloads are not mirrored,
  so an ordinary build still fetches the exact committed URLs.
- Signed content is identified, not declared safe: payload semantics and
  licences remain part of adoption review.
- `RUSTUP_AUTO_INSTALL=0` and the persisted `auto-install`/`auto-self-update`
  disables mean an absent channel, tool or target fails locally instead of
  downloading. A guest that explicitly runs `rustup install/add/update` is
  outside the guarantee, as are project dependencies. A project
  `rust-toolchain.toml` that merely *declares* extra components or targets may
  still build (rustup ignores those lists when auto-install is off); a command
  that needs them fails.
- `RUSTUP_TOOLCHAIN` is deliberately not exported: forcing a channel would
  ignore project selection and hide a stale layer. `CARGO_HOME` is not exported
  either, so cargo keeps a per-user `$HOME/.cargo` cache.

## Alternatives rejected

- **Pin `rustup-init` and trust the server's manifest**: no committed manifest
  or selected-archive identities.
- **Network dist proxy or a rewritten/re-signed reduced manifest**: a new
  service and lifecycle, and the upstream signed-byte relationship is lost.
- **Extract component tarballs by hand or `rustup toolchain link`**: duplicates
  rustup's bookkeeping and risks Verus/alias behaviour.
- **A generic artifact-pin framework**: the Node/gh schema models origins,
  schemas, sizes and payloads; Rust needs manifest semantics and rustup
  installation, so this stays a per-example deep module like the Chrome MCP pin.

## Verification

Hermetic: `script/test/rust-toolchain-pins.py` (pin/install faults against a
signed synthetic distribution, with the production transport exercised through
a controlled opener) and `script/test/rust-toolchain-maintenance.py` (staging,
publication and signal faults of the real writer). Static:
`script/test/example-layers.sh` (81 cases). The native harness
`script/test/rust-example.sh` bounds every command, supervises the suite, owns its
cleanup and has `--self-test` controls that need no daemon. Real adoption of
channel 1.98.1 /
rustup 1.28.2 downloads and authenticates the full closure. Native evidence is
`bash script/test/rust-example.sh --platform linux/arm64|linux/amd64` on a
native daemon; permanent example CI stays with #33.
