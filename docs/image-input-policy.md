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
| Local base (`agent-vm-base:local`) | local build | local tag | committed [base-input manifest](#repository-base-identity-a-committed-input-manifest) plus a stamp the consumers compare |

Pinning does not establish that a release is safe. It establishes which identity
was selected and reviewed, so that a wrong or changed input fails loudly instead
of being adopted. Ordinary builds only verify the committed pins: no latest
lookup, graph regeneration, checksum refresh, silent fallback or image-owned
first-use download. Only the explicit maintenance paths below - a
[parent refresh](#maintenance-refreshing-a-parent) and a
[release-artifact refresh](#maintenance-refreshing-a-pinned-release-artifact) -
may resolve and re-place a pin, and they write a reviewed proposal for a human to
commit; they never auto-adopt or auto-merge, and they refuse to run in CI. Installs that a user or their project triggers are outside this guarantee.

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
  supplied through `docker build -f OTHER` are outside this guarantee. The normal
  `-f images/Dockerfile` path is covered, because the Dockerfile the identity
  mounts is the one the build used.
- The inherited base stamp is trusted, and so is an externally supplied
  host-helper context (`images/build.sh --host-helpers`). A forged or stale stamp
  cannot attest descendant modifications; the stamp is a receipt the base wrote
  about its own sources, never authentication of the image.

Say "pinned parent" or "this base's sources", never "all inputs" or "same image".

## Official Debian apt packages may float

Only packages from official Debian repositories may float, and only with normal
apt authentication intact: HTTP to `deb.debian.org` is acceptable because the
Release files are signed. The extracted baseline in
[`images/apt-policy/`](../images/apt-policy) records the reviewed Debian
`URIs`/`Suites`/`Components` and `Signed-By` key path plus the shipped keyring
hashes. A future parent that changes that policy is a policy change, not a
refresh.

The exception covers *that* apt source set and nothing else: no third-party apt
repository or keyring remains in any maintained recipe. Node and the GitHub CLI
are [pinned release artifacts](#pinned-third-party-release-artifacts) instead,
and the optional Claude plugin payloads stay with
[#101](https://github.com/gregwebs/agent-vm-images/issues/101). No blanket
unauthenticated apt mode, no third-party exemption, no snapshot requirement.

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

The base re-runs the same checker immediately before its identity stamp, and
with [#96](https://github.com/gregwebs/agent-vm-images/issues/96) that is now a
statement about the *final* source set: no instruction adds an apt repository or
keyring after the first check any more, so the built image's apt sources and
keyrings are exactly the committed baseline. A future recipe that adds one fails
that second check as well as the gate below.

`images/parent-pins.json` still carries `apt_open_gaps`, and it is empty and
closed: it could only ever describe a third-party apt source or key
installation, so a refresh and the gate both refuse an entry rather than
compare it with a recipe. `script/test/image-sources` refuses the known
spellings of a new source, keyring or external setup endpoint in a maintained
Dockerfile or script — `/etc/apt` with any suffix (so COPY/ADD destinations
too), `sources.list`, `.sources`, `Signed-By`, `trusted.gpg`, the keyring
directories, `URIs:`, `Types: deb`, `Dir::Etc`, `apt-config`, plus the removed
instructions' own marks: `nodesource`, `setup22.x`, `cli.github.com` and
`githubcli` — matched case-insensitively and with any separator. It additionally
refuses a downloaded script piped into a shell (`curl … | bash|sh`, including an
`env`/`sudo` wrapper) and `nodejs`/`npm`/`gh` installed through apt, tokenising the
apt invocation instead of matching `apt-get install` as a literal (options before
the verb, line continuations, quoting, `:arch` qualifiers, globs and
`name=version` are all normalised first). Both rules cover every maintained helper
whose text is a program, not only `.sh` files. A recipe may not download an
archive and unpack it by hand: that installation surface belongs to
`images/install-artifact.sh`, and the gate refuses a recipe that does it. The gate
also requires both `images/apt-policy/check-sources.sh` call sites — the baseline
before the first package step and the final set after the last one — so deleting
either is a failure rather than a silently weaker build.

An external image input must be a reviewed one: a `COPY --from=` or a
`RUN --mount=from=` has to name a stage declared in that recipe, a stage index, or
a parent image whose digest is in `images/parent-pins.json`. This ratchet is a
static regression guard over reviewed sources, not a sandbox for arbitrary shell:
it catches those spellings, not every construction that could write the same
state, and a construction that names none of them is still a review question.

There is no `apt-get upgrade` here. A parent-layer security update needs a
reviewed parent refresh (below) and a rebuild; rebuilding alone cannot update a
frozen parent, and a floating `apt-get install` needs cache invalidation
(`--no-cache`) to requery the archive. Do not claim a rebuild fixes a parent
vulnerability.

## Pinned third-party release artifacts

Node and the GitHub CLI are **pinned release artifacts**: exact upstream
archives whose per-architecture SHA-256, upstream URL and authentication
evidence are committed. The record and the rules are in
[ADR 0006](adr/0006-third-party-artifact-pins.md); the reviewers' entry points
are:

| Artifact | Selected | Committed identity | Evidence |
|---|---|---|---|
| Node (major 22, bundled npm/npx/corepack) | `22.23.4` | `images/artifacts/node.json`: `linux/amd64` and `linux/arm64` archive URLs plus SHA-256 | detached OpenPGP signature over `SHASUMS256.txt` against the committed release key and fingerprint |
| GitHub CLI | `2.103.2` | `images/artifacts/gh.json`: both archives, plus a committed Sigstore bundle per architecture | Sigstore attestation with an explicit repository, signer workflow, OIDC issuer and hosted-runner policy |

What an ordinary build proves: the pin parses and names both supported
architectures; its URL must be *exactly* the computed
`<origin>/v<version>/<filename>` (a `..` segment, a lookalike host, an extra path
segment or a query string is refused); the archive it downloads hashes to the
committed digest; for Node the signed manifest line matches that digest before the
archive is fetched, the signature verifies against the committed key and
fingerprint, and the archive's bundled npm equals the version the pin records; for
the GitHub CLI the committed bundle verifies the archive with the pinned policy
*before* publication, with the verifier taken from the staged tree; nothing is
extracted until every member has been preflighted, every link chain resolved and
every destination ancestor checked; nothing is published outside the declared
payload, a conflict anywhere in the payload is found before the first write, and a
failure after a write takes the written paths back out. The selected versions are
recorded in `org.agent-vm.version.node` and `org.agent-vm.version.gh`, and the gate
refuses a label that disagrees with its pin.

The ordering is observed, not assumed: the installer appends every fetch,
extraction, attestation and publication to `--events FILE` when a caller asks for
one, and `script/test/artifact-pins.sh` asserts from that log that a mismatch
fetches the archive once and never starts extraction. The same suite replays the
two regressions a reviewer introduced into the installer (extraction before the
digest check, and a second fetch after a mismatch) against a copy and requires the
log check to catch both.

What it does not prove, and this document does not claim: that a release is safe,
that the origin served the same bytes to another build (the committed hash is the
authority), or that the in-build `gh` verifier is trustworthy beyond its own
committed hash — the authoritative attestation check for the GitHub CLI is the
pin-time one recorded in the pin, because `gh` is the artifact it verifies. There
is no fallback to apt, npm, a mirror or `latest`: an unreachable or changed input
fails the build.

The Sigstore trusted root is fetched through Sigstore's TUF channel at verify
time. It is live trust material rather than a pinned input, so a build without
network fails closed instead of skipping the check.

## The Rust example's toolchain closure

`examples/layers/rust-dev` is a *checked-in example*, so its Rust inputs are in
scope: the example commits a reviewed finite closure and rustup installs only
from it. The record is
[`rust-toolchain/pin.json`](../examples/layers/rust-dev/rust-toolchain/pin.json)
and the decision is [ADR 0007](adr/0007-rust-example-toolchain-pins.md).

What the closure contains, for `linux/amd64` and `linux/arm64`: the exact
`rustup-init` (1.28.2) SHA-256 per host with its original upstream `.sha256`
sidecar; the original signed `channel-rust-<version>.toml` with its `.sha256` and
detached `.asc`, plus Rust's release key; and the SHA-256 and detached OpenPGP
signature of each selected archive — `rustc`, `cargo`, `rust-std`,
`clippy-preview`, `rustfmt-preview` for the GNU host and `rust-std` for the
matching musl target.

What an ordinary build proves: every committed evidence digest matches; the
manifest's `.sha256` line and detached signature verify against the committed key
and primary fingerprint; the manifest's release date, version, profile, renames
and each selected `xz_url`/`xz_hash` agree with the pin; the selection is exactly
what rustup 1.28.2 derives for this host — the minimal profile's entries looked
up in the host's *merged* component table (rustup appends `extensions` to
`components` with `is_extension = true` and searches the result), keeping each
found row's own host or `*` target, plus the renamed explicit clippy/rustfmt
components and the explicitly requested musl standard library, deduplicated by
package and target — and it must equal the committed six (a changed profile,
rename, removed/retargeted component or extension, an added host extension, a
target-independent selection such as a coherent wildcard `rust-mingw`, or added
required host component stops for review); the exact key set of every pin object and of each
selected manifest entry is fixed, so unknown or renamed fields stop for review;
the selected entry advertises a full xz pair, **no** zstd fields and no gzip
half-pair
(rustup 1.28.2 prefers zstd, so a zstd pair would bypass the pinned hash); the
bootstrap's ELF machine matches the host; each archive's detached signature
verifies before its members are inspected; the decompressed stream is counted so
a small archive cannot expand past its bound through padding or GNU/PAX extended
headers; every member is canonical, inside the single expected prefix, has a
directory (never a file or link) as every ancestor, is free of duplicates,
devices, fifos and setuid/setgid on any member kind, and the top-level entry is
counted like any other member; the rust-installer version-3 metadata names
exactly the expected component, uses only canonical `file:`/`dir:` directives,
and covers every payload leaf; the expected
executables are ELF binaries for the selected architecture, and the rust-std
archive carries the selected target's standard library. Only then does the
committed `rustup-init` execute, against an invocation-private `file://` layout
holding the original manifest bytes and only the verified archives. There is no
fallback origin, no re-resolution and no checksum refresh.

The exception: `rustup-init` publishes a `.sha256` sidecar but no detached
signature, attestation or release object for 1.28.2, so its use is recorded as a
**scoped, dated, per-architecture hash-only exception** in `pin.json` — exact
versioned URL and digest per host, the upstream `.sha256` and installation-doc
evidence, the reviewer, and a reassessment condition. It is not a claim that the
bootstrap publisher is authenticated, and a nearby unsigned checksum is only a
cross-check. The signed toolchain archives are verified and therefore need no
exception. Changes to a hash-only exception need human review: the writer prints
`hash-only bootstrap exception requires renewed review`, shows the old and new
identity, and publishes nothing until a reviewer re-dates it.

## Repository base identity: a committed input manifest

The external pins above bound *parents*. A local base is a source input to the
standard image and to a copied extension, and the tag `agent-vm-base:local` can
point at a base built from any checkout. The committed manifest binds that local
edge to reviewed source bytes:

```
reviewed external pin ──Docker digest check──► base
tracked base files ─────host generator───────► images/base-inputs.sha256
                                                  │ sha256sum -c in base and standard
                                                  └─ manifest hash ──► parent stamp == consumer expectation
```

- [`images/base-inputs.sha256`](../images/base-inputs.sha256) lists every
  git-tracked file under the fixed roots `.dockerignore Dockerfile apt-policy
  artifacts install-artifact.sh install-build-ca.sh install-pnpm.sh
  install-zellij.sh pnpm recipe-contract verify-base-inputs.sh` — one
  `<64-hex>  <images-relative path>` line per file,
  byte-sorted, no header and no self entry. New files must be `git add`ed before
  they can be listed. The **identity** is `sha256:` plus the SHA-256 of the
  manifest bytes.
- `script/build/base-inputs.sh` is the only writer: `inputs`, `--check`,
  `--write` and an internal `--stage TREE OUT`. No image build can regenerate it.
  `--check` and `--write` hash the *worktree* under `images/` on purpose, so they
  publish exactly the bytes a local build would read; the tracked set still comes
  from the git index. Only `--stage`, which the parent-pin refresh uses, reads a
  tree the caller assembled and therefore isolates the identity from local edits.
- The base's **last** RUN re-hashes exactly the listed entries
  (`images/verify-base-inputs.sh`) and stamps the manifest hash at
  `/opt/agent-vm/base/source-identity`.
- The standard recipe's **first** product RUN re-hashes the same entries with an
  identical mount list and compares the inherited stamp, so a base from other
  sources fails before any agent installation.
- `examples/base-extension/Dockerfile` carries the generated default
  `AGENT_VM_BASE_IDENTITY` and compares it with the inherited stamp.
  `--build-arg AGENT_VM_BASE_IDENTITY=<identity>` is an explicit acceptance of
  another base, not authentication of it, and a copied extension keeps the
  default of the checkout it came from.

Limits, in addition to [what the guarantee is and is not](#what-this-guarantee-is-and-is-not):

- **Source identity, not byte identity.** Two builds of these sources can still
  differ: apt state and caches, platform, build arguments, Zellij overrides and a
  persisted build CA are outside the identity. Such bases and their descendants
  pass; a consumer that needs exact bytes must select and trust an immutable
  image ID separately.
- **Untracked content is outside it.** `sha256sum -c` checks listed entries and
  never walks the mount, so an untracked file that a directory `COPY` or mount
  happens to include is not described. Whether the list is *complete* is a
  host/CI question (`--check` compares the rendered manifest with the committed
  one; a build never discovers a missing entry), not something a single build can
  answer.
- **No protection against a forged stamp.** The daemon store and the inherited
  stamp are trusted, a stale stamp cannot attest later modifications, and an
  externally supplied host-helper context stays outside the identity.
- **`docker build -f OTHER` is outside it.** The identity covers the normal
  `-f images/Dockerfile` path, where the mounted Dockerfile is the recipe used.

A listed input that changed without regeneration fails with
`base-inputs: inputs/default drift; run: bash script/build/base-inputs.sh --write`
(the host gate names the differing files). A consumer that inherits a base
without a stamp fails with `base-inputs: BASE_IMAGE has no repository base
identity; rebuild the base from this checkout`, and a stale one with
`base-inputs: BASE_IMAGE declares <stamp>; this checkout expects <identity>;
rebuild the base`. The extension says `use --build-arg AGENT_VM_BASE_IDENTITY=<stamp>
only if you intend that base` instead. Editing a listed input requires:

```bash
bash script/build/base-inputs.sh --check   # green before you start
# edit images/… (or add a file and `git add` it)
bash script/build/base-inputs.sh --write   # manifest + extension default, one transaction
git add images/base-inputs.sha256 examples/base-extension/Dockerfile
bash images/build.sh base && bash images/build.sh standard
```

`--write` publishes both files transactionally: `0` published or unchanged, `1`
failed with the sources unchanged or fully restored, `2` usage, `3` undecided or
incomplete recovery — the work directory is retained and printed, its numbered
backups are `0` for the manifest and `1` for the extension, and you recover them
before any further edit, build or commit. It is never automatic: a build verifies
the committed bytes and nothing else. It needs GNU coreutils `sha256sum`; without
it the generator refuses with `1` and names the missing tool.

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

The Rust example's `rustup-init` bootstrap is a third instance, scoped to that
example: its record lives in the example's `pin.json`
([details above](#the-rust-examples-toolchain-closure)) and it is re-reviewed on
every bootstrap identity change or newly published signing/attestation scheme.

The released standard image is a different case: it must authenticate. Its pin
carries GitHub attestation provenance with repository `gregwebs/agent-vm-images`,
the composed signer workflow `.github/workflows/release-standard.yml`, source ref
`refs/heads/main`, hosted-runner refusal, no token, and both the metadata and
index bundles. A missing signature is a hard failure; there is no hash-only
fallback for it.

## Maintenance: refreshing a pinned release artifact

Only `script/dev/pin-artifact.py` writes `images/artifacts/*.json`, their evidence
files and the `org.agent-vm.version.<name>` labels, on a maintainer host:

```bash
python3 script/dev/pin-artifact.py --artifact node --version 22.23.5
python3 script/dev/pin-artifact.py --artifact gh --version 2.104.0
python3 script/dev/pin-artifact.py --check      # re-verify what is committed
```

It resolves that exact `X.Y.Z` for `linux/amd64` and `linux/arm64`, verifies every
piece of evidence (Node: the signature against `--key`, default the committed
release key; GitHub CLI: `gh attestation download` plus `gh attestation verify`
with the pinned policy, which needs the maintainer's `gh` login), and only then
publishes the pin, the bundles and the label as one transaction. Statuses match
the other maintenance tools: `0` published or unchanged, `1` failed with the
sources unchanged or fully restored, `2` usage, `3` undecided or incomplete
recovery (the work directory is retained and printed). It refuses
`GITHUB_ACTIONS=true`, refuses a version that is not exactly `X.Y.Z`, never
adopts a newer release by itself and never commits. A successful run prints the
follow-up step, because the pin is a base input:

```bash
bash script/build/base-inputs.sh --write
git add images/artifacts images/Dockerfile images/base-inputs.sha256 \
    examples/base-extension/Dockerfile
bash script/test/artifact-pins.sh && python3 script/test/pin-artifact-maintenance.py
bash script/test/node-toolchain.sh agent-vm-base:local --platform linux/arm64  # after a native rebuild
```

A Node key rotation is the same command with `--key new-key.asc`: the new key's
fingerprint becomes part of the reviewed pin, and a manifest signed by any other
key is refused.

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

A refresh also republishes `images/base-inputs.sha256` and the extension
default, because a moved local parent changes a listed input. Review those two
files with the diff; the inventory itself is metadata the base does not consume
and is deliberately not listed.

Then review the diff, run the gates and rebuild both products:

```bash
git diff -- images examples
python3 script/test/parent-pins.py
python3 script/test/base-inputs.py
bash script/test/contracts.sh
bash images/build.sh base && bash images/build.sh standard
bash script/test/parent-identity.sh agent-vm-base:local --platform linux/arm64
```

`golang-builder` selection must equal the `go.mod` toolchain; a Go toolchain
change is a coordinated change, not a side effect of a refresh. Changing the
Debian release in `script/dev/parent_pins.py` is likewise a reviewed source
change.

## Work still owned elsewhere

- [#101](https://github.com/gregwebs/agent-vm-images/issues/101) owns optional
  Claude plugin inputs.
- The local edge is closed by the committed base-input manifest and the parent
  stamp described [above](#repository-base-identity-a-committed-input-manifest);
  it adds no new input owner. Broader all-input reconciliation is
  [#106](https://github.com/gregwebs/agent-vm-images/issues/106)'s.
