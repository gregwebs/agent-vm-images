# Image input policy: pinned parents

This is the canonical policy for the *external* inputs of every maintained image
recipe. It covers the base, the standard image, the copied base extension and the
four layer examples. Payload, lock and toolchain ownership is separate and stays
with [dsh graph freeze](adr/0003-dsh-graph-freeze.md), [Go build helpers](adr/0004-go-build-helpers.md)
and the [ownership manifest](image-source-ownership.md).

## Binding policy

A **pinned image input** is an external input to an image-owned build or setup
operation whose selection and expected content identity are fixed in reviewed
repository data; a version selection alone is not a pin.
[`images/parent-pins.json`](../images/parent-pins.json) is that record. Every
maintained recipe names its parents literally, and the gate in
`script/test/image-sources` refuses a recipe, an inventory or a document that
disagrees with it. Nothing here is a floating tag, an implicit `latest`, or a
registry lookup during an ordinary build.

The one deliberate exception is a **floating distro input**: a package input from
an official Debian apt repository, which may change between builds with normal
apt authentication retained (see [below](#official-debian-apt-packages-may-float)).
A floating distro input is not a category that includes third-party apt
repositories, and it is not a pinned image input.

| Parent | Kind | Selected by | Provenance |
|---|---|---|---|
| Debian base | Docker Official Image | `debian:13-slim` + index digest | registry digest plus per-platform source revision |
| Go helper compiler | Docker Official Image | `golang:<toolchain>-trixie` + index digest | registry digest plus per-platform source revision, build fragment bound to the tag |
| Standard image | this repository's release | index digest only | GitHub attestation (metadata and index), source SHA, run invocation |
| Local base (`agent-vm-base:local`) | local build | local tag | **not pinned yet**: [C] binds `images/standard/Dockerfile` and `examples/base-extension/Dockerfile` to the committed base inputs, so today they still accept a stale local tag |

Pinning does not establish that a release is safe. It establishes which identity
was selected and reviewed, so that a wrong or changed input fails loudly instead
of being adopted. Ordinary builds only verify the committed pins: no latest
lookup, graph regeneration, checksum refresh, silent fallback or image-owned
first-use download. Only the explicit [maintenance path](#maintenance-refreshing-a-parent)
below may resolve and re-place a pin, and it writes a reviewed proposal for a
human to commit; it never auto-adopts or auto-merges, and it refuses to run in
CI. Installs that a user or their project triggers are outside this guarantee.

A wrong parent identity, wrong source, or digest mismatch fails. There is no
fallback to a tag: the resolver hashes the bytes it received and compares them
with the addressed digest, so a registry that serves different bytes is a
failure rather than a silent retarget. The same holds for a released example
parent: `examples/layers/*` address `ghcr.io/gregwebs/agent-vm-standard` by
digest, and a stale digest in a recipe, a README or the inventory is a gate
failure.

## What this guarantee is, and is not

The committed pin binds the *source identity and selection* of a parent. It does
**not** establish image-byte identity or authenticated artifact provenance for
what you build:

- Two builds of the same pinned sources can produce different image IDs: apt
  state and caches, platform, build arguments, Zellij overrides and a persisted
  build CA are all outside this identity.
- A base is not required to be byte-reproducible; a consumer that needs exact
  bytes must select and trust an immutable image ID separately.
- The local daemon store is trusted, and untracked files, file modes, empty
  directories, effective build arguments, Docker's exporter output and a recipe
  supplied through `docker build -f OTHER` are outside this guarantee.
- [C] An inherited base stamp and any externally supplied host-helper context are
  trusted. A forged or stale inherited stamp cannot attest descendant
  modifications.

Items marked [C] describe the committed base-input manifest and the parent
stamp that a later #95 change adds. A recipe in this change verifies no inherited
stamp, so read those limits as the boundary the next change must keep, not as
protection that exists today.

Say "pinned parent", never "all inputs" or "same image".

## Official Debian apt packages may float

Only packages from official Debian repositories may float, and only with normal
apt authentication intact: HTTP to `deb.debian.org` is acceptable because the
Release files are signed. The extracted baseline in
[`images/apt-policy/`](../images/apt-policy) records the reviewed Debian
`URIs`/`Suites`/`Components` and `Signed-By` key path plus the shipped keyring
hashes. A future parent that changes that policy is a policy change, not a
refresh.

The exception covers *that* apt source set and nothing else. NodeSource and the
GitHub CLI repository remain unpinned third-party apt inputs — open violations
owned by [#96](https://github.com/gregwebs/agent-vm-images/issues/96) — and the
optional Claude plugin payloads stay with [#101](https://github.com/gregwebs/agent-vm-images/issues/101).
No blanket unauthenticated apt mode, no third-party exemption, no snapshot
requirement.

### What the build checks, and what it does not

The base runs `images/apt-policy/check-sources.sh` as its first package step. It
is a **parent baseline** check: `debian.sources` must equal the committed bytes,
`sources.list.d` may hold nothing else, `/etc/apt/sources.list` may exist only
with no active source line, `/etc/apt/trusted.gpg` must be absent,
`trusted.gpg.d` must be exactly the listed keyrings, every listed keyring must
hash to `keyrings.sha256`, and no
active apt configuration may re-enable unauthenticated or insecure sources,
accept expired metadata, redirect apt at other source or key directories
(`Dir::Etc*`, `Dir::Parts`) or pull in further configuration (`#include`,
`#clear` are apt.conf(5) directives, not comments). Comments in the parent's
shipped configuration are not active policy. Any failure names the offending
path and stops the build.

It is not a description of the final source set. The two #96 instructions add
NodeSource and the GitHub CLI repository afterwards, so the built image has
more sources than the baseline. That is why `images/parent-pins.json` also
records `apt_open_gaps`: the *complete physical text* of each reviewed legacy
instruction, with the owning issue. `script/test/image-sources` refuses the
known spellings of a new source, keyring or external setup endpoint in a
maintained Dockerfile or script — `/etc/apt` with any suffix (so COPY/ADD
destinations too), `sources.list`, `.sources`, `Signed-By`, `trusted.gpg`, the
keyring directories, `URIs:`, `Types: deb`, `Dir::Etc` and `apt-config`, matched
case-insensitively and with any separator — and refuses an edited, extended,
duplicated or vanished recorded instruction
with `apt gap changed/closed`. The entry is a human review record, never a URL
marker and never regenerated by a refresh; [#96](https://github.com/gregwebs/agent-vm-images/issues/96)
removes it together with its installer. This ratchet is a static regression
guard over reviewed sources, not a sandbox for arbitrary shell: it catches
those spellings, not every construction that could write the same state, and a
construction that names none of them is still a review question.

There is no `apt-get upgrade` here. A parent-layer security update needs a
reviewed parent refresh (below) and a rebuild; rebuilding alone cannot update a
frozen parent, and a floating `apt-get install` needs cache invalidation
(`--no-cache`) to requery the archive. Do not claim a rebuild fixes a parent
vulnerability.

## Build CA: verified content, refused in public releases

A host CA supplied as a build secret is persisted trust material, so it is
installed only together with the SHA-256 of the certificate a reviewer chose:
`BUILD_CA_SHA256`, as described in
[optional build trust](../README.md#optional-build-trust).
`images/install-build-ca.sh` refuses a missing or malformed hash, a hash that
does not match the secret, anything other than exactly one PEM certificate, and
a secret that does not parse as X.509; all structure and hash checks run before
apt or any trust write. The successful path installs the certificate and records
`sha256:<hash>` at `/opt/agent-vm/base/build-ca-sha256` and in the
`org.agent-vm.build-ca.sha256` label; a build without a CA records `none`.
`script/release/build.py` inspects that label after the base build and refuses to
continue when it is missing or nonempty, and `script/release/operations.py`
refuses `--label org.agent-vm.build-ca.sha256=...` on a public build, so a
published standard image is never built through the build-CA interface of this
repository. `CA_SHIM_CACHEBUST` is removed; the declared
tombstone argument exists only to reject it.

What that does not cover: the refusal is a read of one label plus a command-line
check, not an inspection of image content. A base built by other means, or
labelled by other tooling, is outside it; `images/standard/Dockerfile` is not
inspected at all, so a certificate installed by an edited standard recipe would
be a reviewed source change rather than something this gate can detect.

The hash is a build cache key, the secret bytes are not: a changed hash reruns
the verification, changed bytes under an unchanged hash may reuse a previous
valid layer, and a mismatch proof needs `--no-cache`. A build CA does not make
the image reproducible or authenticated; the image ID stays outside this
policy's identity.

## Reviewed hash-only exceptions

The two Docker Official Image parents carry a dated, digest-bound exception
instead of an upstream signature:

- **Why:** Docker Content Trust, the channel that signed official images, is
  retired and its DOI certificates expire. Docker points base-image consumers at
  signed Docker Hardened Images, and no supported replacement signing scheme is
  published for these two images as of the recorded date. Unsigned in-index
  attestation manifests are metadata, not authenticated provenance.
- **What is reviewed instead:** the registry digest, the per-platform source
  revision and creation time, and (for Go) the build-fragment directory that must
  match the selected tag. Evidence URLs and a reassessment condition are recorded
  in the inventory itself.
- **Reviewed on:** see `verification.reviewed_on` per pin. A refresh that changes
  a digest prints `hash-only exception requires renewed review` and must be
  re-dated by a reviewer before merge. The resolver never rewrites the
  rationale, its evidence or the review date: on a changed digest it keeps all
  three exactly as reviewed, advances only `resolved_on`, and leaves the
  now-stale `reviewed_on` in place so a human has to bump it deliberately.
- **Stop condition:** if a supported verifiable signing scheme is published for
  either image, refresh stops until a verifier and a policy update exist.

The released standard image is a different case: it must authenticate. Its pin
carries GitHub attestation provenance with repository `gregwebs/agent-vm-images`,
the composed signer workflow `.github/workflows/release-standard.yml`, source ref
`refs/heads/main`, hosted-runner refusal, no token, and both the metadata and
index bundles. A missing signature is a hard failure; there is no hash-only
fallback for it.

## Maintenance: refreshing a parent

Only `script/dev/refresh-parent-pins.sh` writes these pins, on a maintainer host,
never in CI and never during a build:

```bash
bash script/dev/refresh-parent-pins.sh debian
bash script/dev/refresh-parent-pins.sh golang-builder [golang:VERSION-trixie]
bash script/dev/refresh-parent-pins.sh standard-release X.Y.Z
```

Prerequisites on that host: Docker with a reachable Linux daemon and buildx,
`python3`, `git`, GNU coreutils (`sha256sum`), `diff`, `jq`, and `gh` with
`attestation verify` (tested 2.97.0). `gh` needs no login — parent attestations
are verified anonymously.

It resolves against the live registries, authenticates the release metadata,
stages complete candidate copies, prints the proposal (old/new references,
per-platform digests, revisions, sources, verification method and the apt policy
diff) and then publishes every destination as one transaction.

Statuses match the other maintenance tools: `0` published or unchanged, `1`
failed with the sources unchanged or fully restored, `2` usage, `3` undecided or
incomplete rollback — the scratch directory is retained and printed, and
`recovery.json` beside the numbered backups lists every destination, whether it
existed and which backup restores it. Recover those destinations before further
edits, builds or commits. A rerun of an identical resolution prints `unchanged`
and rewrites no dates.

Safety properties: the destination list is computed by the tool and the publisher
refuses any staged path outside it; it refuses dirty, staged or untracked
*destinations*, symlinked or non-directory destination ancestors, a dirty or
staged **tracked** file under a base-input root, and any checked input that
changes while resolution runs. Only tracked files are enumerated, so an untracked
file elsewhere in the checkout neither blocks a refresh nor is covered by this
check. It never deletes a path it did not back up or create.

Then review the diff, run the gates and rebuild both products:

```bash
git diff -- images examples
python3 script/test/parent-pins.py
bash script/test/contracts.sh
bash images/build.sh base && bash images/build.sh standard
```

`golang-builder` selection must equal the `go.mod` toolchain; a Go toolchain
change is a coordinated change, not a side effect of a refresh. Changing the
Debian release in `script/dev/parent_pins.py` is likewise a reviewed source
change.

## Work still owned elsewhere

- [#96](https://github.com/gregwebs/agent-vm-images/issues/96) removes the
  NodeSource / GitHub CLI apt installs; today they are recorded violations.
- [#101](https://github.com/gregwebs/agent-vm-images/issues/101) owns optional
  Claude plugin inputs.
- Later #95 work: [C] binds local base consumers to a committed input manifest
  and a parent stamp, and owns the final source-identity documentation. This change
  ([B]) enforces the Debian baseline in the build and verifies the persisted
  build-CA content.
- [#106](https://github.com/gregwebs/agent-vm-images/issues/106) is the final
  all-input reconciliation; it cannot substitute for this policy or for the
  native evidence in [CONTRIBUTING](../CONTRIBUTING.md).
