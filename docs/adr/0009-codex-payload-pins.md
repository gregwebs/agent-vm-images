# ADR 0009: commit per-platform content identities for Codex payloads

Status: accepted (#97).

## Context

`images/standard/Dockerfile` installed Codex by running OpenAI's published
installer, which read `https://api.github.com/repos/openai/codex/releases/tags/rust-v<version>`,
took the `sha256:` digest the API reported, downloaded
`codex-package_SHA256SUMS` from the same release, and only then downloaded
`codex-package-<target>.tar.gz` and compared it against the manifest line.

An exact release tag therefore selected a *name*, not bytes. One party controls
the metadata, the manifest and the archive, so replacing all three consistently
passed every check. The review also found the second half of the same problem:
the vendored installer no longer contained any URL, so a "no live resolution"
claim could not be made from the vendor alone.

Codex is also not shaped like the artifacts ADR 0006 covers. It publishes a
*prefixless* package (`bin/codex` at the archive root, ~160 MB compressed, 424 MB
decoded) that installs into a HOME-owned `packages/standalone` tree with a
`current` symlink, not into `/usr/local`. Forcing it into
`images/artifacts/<name>.json` would have required a fake member prefix and a
raised 64 MiB ceiling.

## Decision

Commit a dedicated closure and make the build read only that.

- `images/tools/codex/pin.json` is the reviewed selection for one exact release:
  the release and its numeric version, the two maintained Linux package assets
  (`linux/amd64` → `x86_64-unknown-linux-musl`, `linux/arm64` →
  `aarch64-unknown-linux-musl`) with exact URLs and SHA-256 digests, the original
  exact-tag API metadata, the original `codex-package_SHA256SUMS`, and both
  original `.sigstore` bundles — plus the signer policy enforced when the pin was
  made.
- `images/tools/codex/codex_pin.py` is a stdlib-only, offline loader and archive
  preflight. It reconstructs every URL, asset name and evidence path from the
  validated release and requires *equality*; it re-hashes all four evidence files
  and every downloaded payload; it validates the metadata's security projection
  and the SHA-256SUMS rows; and it preflights the archive (member policy, package
  metadata, ELF machine) before `tar` runs.
- The vendored installer keeps its extraction, linking and bounded native report,
  and takes its selection from `codex_pin.py select`, whose four fields it parses
  positionally. There is no `eval`, no metadata fetch, no checksum download and no
  legacy npm fallback left in it.
- Publication is shared, not new: `script/dev/pin-codex.py` reuses the
  `Publisher` transaction from `script/dev/pin-artifact.py`, so the pin, its four
  evidence files and the `ARG AGENT_VERSION_CODEX` default land together or not at
  all.

## Signature decision: cosign v2.4.1, pin time only

OpenAI publishes legacy cosign blob bundles (`base64Signature` + `cert` +
`rekorBundle`). `gh attestation verify` 2.97.0 and 2.103.2 reject that shape and
GitHub's stored-attestation endpoint returns 404 for these assets, so `gh` cannot
be the verifier and the bundle must not be converted into an invented GitHub
attestation.

The maintenance host runs the real command, with every default security check
left on:

```bash
cosign verify-blob --bundle <asset>.sigstore \
  --certificate-identity https://github.com/openai/codex/.github/workflows/rust-release.yml@refs/tags/<release> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  <asset>
```

- `cosign v2.4.1` is a **maintainer prerequisite**, like `gpg`: not an image
  input, not auto-installed, not replaced by another version, not invoked with
  `--insecure-ignore-tlog`, `--insecure-ignore-sct`, an identity regexp or a bare
  public key. A failed or unparseable `cosign version --json` is a refusal.
- The trust root is Sigstore's production TUF material, fetched and validated by
  that same verifier. This is an explicit new host dependency, analogous to ADR
  0006's live root: it is *not* a committed independent root snapshot, it is not
  distributed into the image, and pin-time maintenance fails closed without
  network.
- The exact workflow SAN and issuer are the enforced signer policy.
  `github-hosted` is an observed certificate property, not an enforced claim.

Ordinary builds perform **no** cryptographic verification and contact no Sigstore
endpoint. They prove the committed payload identity, the committed evidence
identity and the package/architecture constraints. There is no hash-only
exception in this design; a bundle that cannot be verified blocks the proposal.

## Alternatives rejected

- **Live checksum as the authority.** The failure this ADR answers.
- **Reuse `artifact_pin`/ADR 0006.** Wrong shape (prefix, size, destination) and
  the model hard-codes a `/usr/local` payload.
- **Verify with `gh attestation verify`.** It cannot read these bundles.
- **Verify inside the build.** Adds cosign and a Sigstore trust root to the
  image, needs network at build time, and would still have to trust whatever root
  the build fetched.
- **A hash-only exception.** Nothing in this change needs one; adopting one later
  is a policy change with its own review (see
  [reviewed hash-only exceptions](../image-input-policy.md#reviewed-hash-only-exceptions)).
- **Split the change.** A version-only or fixture-only intermediate is not
  usable: the pin needs the installer, the writer, the egress rules and the
  controls.

## Flow

```text
PIN TIME (maintainer host, script/dev/pin-codex.py)
  exact tag ──▶ metadata + SHA256SUMS + 2 payloads + 2 bundles (private scratch)
                 │                    │
                 │                    ├─▶ cosign verify-blob  (both architectures,
                 │                    │   exact identity/issuer, default chain/SCT/Rekor)
                 │                    └─▶ codex_pin.preflight_archive (both archives)
                 ▼
          render pin/evidence ──▶ load_pin (re-hash everything) ──▶ ONE transaction
          (6 files: pin, 4 evidence, Dockerfile ARG)

BUILD (ordinary, offline for this input)
  committed pin + evidence ──▶ codex_pin check (all 4 evidence hashes)
       │
  exact payload URL ──▶ one classified download ──▶ sha256 == committed digest
       │
  codex_pin preflight ──▶ package metadata + ELF machine + bounded inventory
       │
  vendored install ──▶ bounded native report ──▶ existing verify-codex.sh gate
```

## Availability and trust limits

- A build binds committed evidence bytes and a committed archive digest. It
  cannot prove that a maintainer's pin-time verification happened, or that the
  upstream certificate was not mis-issued. Trust in the reviewed pin and the
  maintenance host is unavoidable and is stated rather than hidden.
- The preflight ceilings (256 MiB compressed, 1 GiB decoded, 1 GiB expanded
  regular files, 4096 members/headers, 64 KiB package metadata, 64 KiB per
  extended header, 1 MiB of extended headers) are a policy, not a tunable. A
  larger release is a reviewed change.
- The shared build downloader (`images/recipe-contract/download.sh`) has no
  in-flight byte ceiling, so these limits bound parsing and extraction, not bytes
  received or temporary disk. The maintenance `Fetcher` does bound the fetch: one
  300 s budget per request, enforced by an independent whole-request timer as well
  as by per-read re-clamping of the socket timeout, so a peer that drips header or
  chunk-framing bytes cannot hold a maintainer run open.
- A same-release rewrite of the payload, the checksum manifest or the metadata's
  security projection is refused, never re-pinned automatically.
- Arbitrary exact Codex versions are no longer installable: the version must be
  the one the committed pin selects.

## Initial adoption

`rust-v0.159.3`, both architectures, verified 2026-10-27 on darwin/arm64 with
`cosign` v2.4.1 (positive and negative runs recorded in
`.pi/work/97/verifications.md`). Native amd64/arm64 install, runtime and strict
egress evidence is the separate acceptance step tracked by #97's CI rows.

## References

- [image input policy](../image-input-policy.md#codex-standalone-payloads)
- [CONTRIBUTING: selection ownership and maintenance](../../CONTRIBUTING.md#selection-ownership-and-maintenance)
- `docs/adr/0006-third-party-artifact-pins.md` (the shared publication
  transaction and the release-artifact model this deliberately does not reuse)
