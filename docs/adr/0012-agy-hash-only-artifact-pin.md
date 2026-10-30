# 0012: The Antigravity CLI is a hash-only artifact pin, outside the base inputs

Status: accepted for [#131](https://github.com/gregwebs/agent-vm-images/issues/131)
(amends [ADR 0006](0006-third-party-artifact-pins.md)). Evidence for the native
rows lives in [CONTRIBUTING](../../CONTRIBUTING.md#agy-hash-only-pin) and the
pull request that introduced this ADR.

## Context

The standard image gained a seventh agent, Google's Antigravity CLI (`agy`). It
is an ordinary release archive — one `tar.gz` per architecture, one binary inside
it — but nothing about its upstream distribution can be verified the way this
repository verifies Node, the GitHub CLI or Codex:

- Google publishes **no signature and no attestation** for the assets. There is
  no `.sig`, no `.sigstore` bundle, no SLSA provenance and no checksums file on
  the release.
- The GitHub release is **mutable**. An asset can be replaced in place after
  publication, and the release is uploaded by an individual account rather than a
  workflow identity, so there is no signer to pin even if a bundle appeared.
- The only second record of the same bytes is an **updater manifest** Google
  serves per architecture. It carries a `version`, a GCS `url` and a `sha512` of
  the archive, and — importantly — it names **only the latest release**: an older
  release's manifest is not published anywhere.
- The upstream `install.sh` writes shell configuration and installs whatever its
  manifest currently names. It is not a pinned input and is not used.
- The binary self-updates in the background, which would replace a reviewed
  binary in a guest, so the image disables that switch.

[ADR 0006](0006-third-party-artifact-pins.md) established one shape for
third-party release artifacts: a committed pin whose bytes are authenticated by
an OpenPGP-signed manifest (Node) or a Sigstore bundle verified with `gh` (the
GitHub CLI). `agy` cannot satisfy that shape, and no amount of pinning creates a
signature that upstream does not publish.

## Decision

`agy` becomes a **third case in the same shared model**, with a new
authentication kind:

- **`hash-only` is a named kind in `images/artifacts/artifact_pin.py`.** It is
  bound to the artifact name — the loader accepts it only for `agy`, and Node and
  the GitHub CLI still refuse it — so it is not a general downgrade any pin can
  claim. The variant carries the two upstream records it was reviewed against
  (the release metadata URL plus the per-architecture manifest URLs) with the
  path and SHA-256 of each committed copy.
- **The pin lives in `images/tools/agy/pin.json`, not in `images/artifacts/`.**
  `images/artifacts/` is a *base* input: its files are hashed into
  `images/base-inputs.sha256` and a base build re-hashes them. `agy` is an agent
  selection that the standard recipe owns, so it follows the Codex closure
  (`images/tools/codex/`) rather than the base. Nothing in
  `images/base-inputs.sha256` covers it.
- **Two-channel corroboration happens at pin time.**
  `script/dev/pin-agy.py` downloads each architecture's archive once and accepts
  it only when the downloaded bytes equal **both** GitHub's asset `digest`
  (SHA-256) **and** the `sha512` in Google's manifest for that platform. The
  manifests must agree with each other and with `--version` on the version, and
  because the manifest names only the latest release, the tool can only pin the
  release the manifest currently names: an older `--version` is refused, not
  fetched.
- **The build re-checks the committed corroboration before it fetches.** The
  committed release metadata and both manifests are hashed against the pin and
  parsed again: the release must still name this version with these digests, and
  each manifest must still name this version with this sha512. Only then is the
  computed release URL fetched — exactly once — and its SHA-256 (and the
  committed SHA-512) compared before extraction. The archive must contain exactly
  one regular executable member named `antigravity` with exactly its reviewed mode
  (`0755`), within the per-artifact ceilings, and it must be an ELF for the pin's
  architecture. It is published as `/usr/local/bin/agy` only: no `antigravity`
  alias exists in the image.
- **Refresh is the same transaction as every other pin.** One owner scratch,
  one shared `Publisher` call, backups and reverse rollback, status 3 for an
  incomplete recovery with both directories printed absolutely. A same-release
  rewrite is refused rather than re-pinned, and a repeat run keeps the committed
  evidence bytes (download counts churn daily) so the proposal is byte-identical.

### Why this is the weakest guarantee in the repository

One publisher controls both channels. A compromised or malicious upload — bytes
whose GitHub digest *and* manifest sha512 agree because the same party wrote both
— passes every check here. The **integrity** check is cryptographic: SHA-256 and
SHA-512 are computed and compared at pin time and again at build time. Publisher
**authentication** is not: no signature is verified, no attestation is read, no
transparency-log entry or independent identity is consulted, and the committed
evidence is only a copy of what upstream said on the day of the pin. The build
proves *which bytes were selected and reviewed*, and that they have not changed
since, which is exactly what ADR 0006 says a pin can prove — no more. The cost is
written down here rather than implied: `agy` is the artifact in this image whose
review leans most heavily on a human looking at an upstream release.

### Alternatives rejected

- **A Codex-style separate loader.** Codex needed one because its verification is
  cosign and its closure has a different shape. `agy` is an archive with a digest,
  which is the shared installer's job; a second loader would duplicate member,
  ELF, ceiling and publication logic to gain nothing.
- **Placing the pin in `images/artifacts/`.** It would make an agent selection a
  base input, so every agent bump would regenerate the base manifest and require
  a base rebuild.
- **Fetching the manifest or the GCS URL at build time.** That is a build-time
  `latest` resolution and a second download source, both of which this repository
  forbids. The manifest is evidence, not a resolver.
- **The upstream `install.sh`.** It is unpinned, writes shell configuration and
  installs whatever the manifest currently names.
- **A reviewer-dated per-bump exception.** With a daily upstream cadence, a dated
  exception on every bump would be paperwork that stops being read. The
  exception is this ADR, reviewed once, plus a `verified_on` date in the pin.

### Reassessment and stop condition

This ADR holds the stop condition. **Stop refreshing `agy` as `hash-only` as soon
as Google publishes signatures, attestations, a checksums file or an immutable
release**, and do not resume until a verifying kind exists for `agy` and this ADR
is superseded. The pin's `reason` field links here rather than restating the
condition: the pin describes the conditions it was reviewed under, and a
maintained policy lives in the document that is amended when the policy changes.

## Consequences

- A bump is one maintainer run (`script/build/agent-versions.sh --write`, which
  resolves the manifest and delegates to `pin-agy.py`) plus a human edit of
  `script/test/fixtures/installer-egress/selections.tsv`. Nothing bumps
  automatically.
- A withdrawn release makes an old commit unbuildable — the same availability
  trade-off ADR 0006 records for Node and the GitHub CLI.
- The build's egress is one URL: `github.com`'s release asset path for the pinned
  version, plus the redirect GitHub issues to its asset CDN. The manifest host,
  the tag API, GCS and `latest` paths are denied by the strict-egress audit.
- The per-artifact ceilings (128 MiB archive, 512 MiB member) live in the shared
  model: `agy` amd64 1.4.11 is 67,990,723 B compressed and 233,943,440 B decoded,
  so the generic 64 MiB ceiling would have refused the reviewed binary. Raising a
  ceiling is a reviewed base-input change.
- The standard image grows by roughly 230 MB.

## Evidence

- `script/test/pin-agy-maintenance.py` (hermetic, 43 cases): the whole decision
  table with its fixture upstream, per-architecture manifest isolation, a
  wrong-ELF archive under consistent hashes, a member mode that is not 0755, a
  committed arm64 record disagreeing with the pin, a proposal whose own evidence
  hash is wrong, an older manifest, a same-release
  rewrite, a byte-identical repeat under download-count churn, a partial committed
  directory, 0/2 ARG lines and a missing label, CI refusal with zero fetches, the
  origin allowlist, the clean-process module binding, and the recovery contract
  (status 3, SIGINT → 130, signal plus refused restore → 3, first-adoption
  rollback) with the five proposed files, numbered backups, modes and `.complete`.
- `script/test/artifact-pins.py` (hermetic, the real installer as a subprocess):
  the happy install, the `--events` ordering (`verify corroboration` → one `fetch`
  → `extract` → `publish`), changed bytes, a sha512-only mismatch, every evidence
  tamper with **zero** fetch events (including the arm64 records while installing
  amd64), hostile members and permission modes, a 32-bit ELF, a wrong ELF machine,
  duplicate pin keys, an unknown artifact name, URL
  mutations, `latest`/musl/other-repo refusals and the per-artifact ceilings.
- `script/test/agy-installer.sh`: the hook's pin check and the verifier's byte-exact
  report, empty stderr, switch, published mode and owner (`755 0:0`), and the
  `installed` record.
- `script/test/image-sources` (Rust): the pin's shape, origins, payload, evidence
  and the recipe's mounts, plus per-field mutations and the "not a base input"
  assertion.
- `script/test/standard-image.sh`, `script/test/standard-certification.sh`,
  `script/test/agent-versions.sh` and
  `script/test/shipped-installer-network.sh`: the finished-image audit (byte-exact
  report, empty stderr, `0:0` ownership, no `antigravity` name, switch on and mode
  `755`), the final gate and PATH identity, the bumper's manifest-only resolution
  with the agy transaction before the Codex one (and its report of a half-finished
  bump when Codex then fails), and the one-URL egress audit.
