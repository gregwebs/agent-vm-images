# 0006: Third-party release artifacts come from committed pins

Status: accepted for [#96](https://github.com/gregwebs/agent-vm-images/issues/96)
(parent [#94](https://github.com/gregwebs/agent-vm-images/issues/94)). Amended by
[ADR 0012](0012-agy-hash-only-artifact-pin.md): a third artifact (`agy`) whose
authentication is `hash-only`, whose pin lives outside `images/artifacts/` because
it is an agent selection rather than a base input, and which needs per-artifact
archive and member ceilings. Evidence for the native rows lives in
[CONTRIBUTING](../../CONTRIBUTING.md#expensive-acceptance-and-ci) and the pull
request that introduced this ADR.

## Context

Two instructions in the base recipe selected Node and the GitHub CLI by *running*
or *repeating* something live:

- `curl -fsSL https://deb.nodesource.com/setup_22.x | bash -` executed a remote
  script that rewrote apt sources, then installed whatever `nodejs` that
  repository served that day. Node, npm and npx were therefore a floating
  third-party apt selection.
- `curl https://cli.github.com/packages/githubcli-archive-keyring.gpg` plus a
  third-party apt repository installed a floating `gh`.

Neither input had a reviewed identity: the bytes could change between two builds
of the same commit, and no committed hash or signature said which release was
meant. The parent-pin policy already required a selection *and* a content
identity for every image input, and recorded both instructions as open
violations (`apt_open_gaps`) owned by #96.

The obvious replacements each had a problem:

- **Keep apt, add a snapshot.** A NodeSource snapshot is still a third-party apt
  repository with a key that has to be pinned, and it still needs a live package
  resolution per build.
- **Use npm to install Node.** A package manager cannot install the interpreter it
  runs on, and it would reintroduce a floating resolution through a runtime
  channel this image deliberately does not trust at build time.
- **Add cosign for the GitHub CLI.** That is a third floating input with its own
  trust root, added to verify an input we can already hash *and* attest.

## Decision

One shape, two artifacts, three pieces of code:

```
images/artifacts/<name>.json      reviewed selection: version, per-arch URL + SHA-256,
                                  member prefix, payload, authentication evidence
images/install-artifact.sh        the only installer; runs the pin, never a resolver
script/dev/pin-artifact.py        the only writer of a pin (maintenance host, one transaction)
```

- **Exact upstream archives.** `node-v22.23.4-linux-{x64,arm64}.tar.xz` from
  `nodejs.org/dist` and `gh_2.103.2_linux_{amd64,arm64}.tar.gz` from the
  `cli/cli` release. The pin carries the URL, the archive kind, the archive's own
  top-level prefix and the lower-case SHA-256 of each architecture. The URL is
  *computed* from the kind, the version and the platform and the pin must equal it
  exactly, in both the installer's loader and the gate: a prefix match let
  `…/releases/download/../../attacker/repo/…` pass, which GitHub resolves into
  another repository. The build
  downloads that URL and nothing else: no apt, npm, mirror, `latest` or
  checksum-fetching fallback exists in the installer, so an unreachable origin
  fails the build instead of resolving something unreviewed.
- **Node's chain is verified before the payload is fetched.** `SHASUMS256.txt`
  and its detached OpenPGP signature are fetched, their committed SHA-256 is
  required, the signature must verify against the committed release key, the
  `VALIDSIG` fingerprint must equal the pinned one, and the signed line for this
  archive must equal the committed digest. Only then is the tarball requested.
- **The GitHub CLI is the honest bootstrap.** `cli/cli` publishes no detached
  OpenPGP signature; its SLSA provenance is readable only by
  `gh attestation verify`, and `gh` is the artifact being installed. So the
  *authoritative* verification is the pin-time one: the maintenance tool downloads
  the Sigstore bundle, verifies the archive against it with the pinned policy
  (`--repo cli/cli`, `--signer-workflow
  cli/cli/.github/workflows/deployment.yml`, `--cert-oidc-issuer
  https://token.actions.githubusercontent.com`, `--deny-self-hosted-runners`) and
  records the outcome and identity in the pin. The *build* re-verifies the
  committed bundle with the just-installed `gh` after its hash-verified extraction
  and fails closed; `--bundle` needs no token. The committed `checksums.txt` hash
  is the pin-time cross-check that the arch line equals the committed digest and
  makes an upstream rewrite visible; ordinary builds do not fetch it, because the
  bundle proves more.
- **The payload is the reviewed archive.** Node installs everything the tarball
  carries (`bin`, `include`, `lib`, `share`, `CHANGELOG.md`, `LICENSE`,
  `README.md`) into `/usr/local`, so `npm`, `npx`, `corepack` and
  `lib/node_modules/npm` are the ones shipped in that artifact - no separate npm
  selection. The pin *records* the npm version read from that archive
  (`bundled.npm`), the installer refuses an archive whose
  `lib/node_modules/npm/package.json` disagrees, and the runtime suite compares
  `npm`/`npx` against the recorded value and requires both commands to resolve
  into `lib/node_modules/npm`. The GitHub CLI publishes `bin/gh` and `share/man`. An absolute
  `/usr/bin/node` symlink to `/usr/local/bin/node` preserves the pinned
  interpreter path that the pnpm launcher, its verifiers, the dsh helper and
  several suites hardcode.
- **Extraction is preflighted, never trusted.** Before a byte is written, every
  member is checked: absolute names, `..` components, *non-canonical spellings*
  (`prefix/bin/./node` is a second name for a file the archive already carries),
  names outside the pinned top-level prefix, duplicates, file/directory conflicts,
  devices/fifos/sockets, setuid/setgid bits, hard links to absent or non-file
  members, and links that resolve outside the prefix. The whole link graph is
  resolved through the archive's own links, with a cycle bound, so
  `bin/b -> a/../..` is not checked lexically; a member whose parent is a link is
  refused, and extraction re-checks the filesystem for a symlink ancestor before
  every write. Files and directories are written first and links last, into a
  private staging tree, and only the declared payload is published into the root.
  Publication is planned first (every destination ancestor `lstat`ed and every
  target required to be free) and only then written, so a conflict in the last
  file cannot leave the first one installed; the GitHub CLI's attestation is
  verified from the *staged* tree before anything is published; and a later
  failure removes what was written.
- **The apt baseline is now the final source set.** Both recorded instructions and
  their keyring are gone, so `images/parent-pins.json` records
  `apt_open_gaps: []` and the base re-runs `images/apt-policy/check-sources.sh`
  immediately before its identity stamp. The gate requires *both* call sites (the
  baseline before the first package step and the final set after the last one),
  refuses any re-spelling of the removed instructions (NodeSource, the GitHub CLI
  package repository and keyring), any downloaded script piped into a shell
  (`env`/`sudo` wrappers included) in any maintained recipe *or* program helper,
  `nodejs`/`npm`/`gh` through apt in any ordinary spelling, a hand-unpacked
  downloaded archive, and any `--from` that is neither a declared stage nor a
  digest from the parent inventory.
- **Refresh is transactional and never automatic.** `script/dev/pin-artifact.py`
  resolves one exact `X.Y.Z`, verifies every architecture and every piece of
  evidence, and only then publishes the pin, its evidence files and the
  `org.agent-vm.version.<name>` label as one transaction with backups, reverse
  rollback and status 3 for an incomplete recovery: staging, a *complete* backup
  (marked and consulted) and publication are distinct phases, so a failed backup
  restores nothing because nothing was written, and a rollback covers exactly the
  destinations publication reached, including the one being written. Every `gpg`
  call gets the private `GNUPGHOME` the tool constructs, so host keyring state
  cannot influence pin-time verification. It refuses `GITHUB_ACTIONS=true`, never
  commits, and cannot be reached from a build.

## Consequences and limits

- The committed hash is the guarantee, not the origin. A build that cannot reach
  `nodejs.org` or `github.com` fails; rebuilding a year-old commit may fail
  because the release was withdrawn. That is the deliberate availability
  trade-off: an unreachable pinned input is a failure, never a fallback.
- A pin refresh is a reviewed source change, including the
  `images/base-inputs.sha256` regeneration it implies. Nothing regenerates a pin
  at build time, and no build accepts a version that differs from the pin's.
- The Node key is one pinned publishing key. Key rotation is a reviewed pin
  refresh (`--key`), not a fallback: a manifest signed by another key fails.
- The in-build `gh` check cannot be stronger than the artifact it installs. If the
  installed `gh` were wrong in a way that still matched its hash, the check would
  use it; the authoritative verification remains the pin-time one, and the gate
  cross-checks the archive hash, the label and the committed bundle.
- The Sigstore trusted root is fetched through Sigstore's TUF channel when
  `gh attestation verify` runs. It is live trust material, not a pinned input, so
  a build without network fails closed rather than skipping the check.
- Pinning does not establish that a release is safe. It establishes which bytes
  were selected and reviewed, so a wrong or changed input fails loudly.
- The gate cannot hash. `script/test/image-sources` validates the pin's shape,
  origins, platforms, digests and evidence presence; the committed evidence bytes
  are bound by `images/base-inputs.sha256` (host `--check` plus the in-build
  re-hash) and by the installer's own digest comparison.

## Evidence

- `script/test/artifact-pins.sh` (hermetic, no network): an invented `--events`
  log per run, from which the suite asserts that a mismatch fetches the archive
  once and never extracts, and that a successful install extracts only after the
  hash; the two installer regressions a reviewer introduced (extraction before
  the digest check, a second fetch after a mismatch) are replayed against a copy
  and the log check must catch both. A fixture release tree
  with its own OpenPGP key drives the real installer - committed-hash match and
  published payload, changed bytes, a consistent hash over the wrong
  architecture, a wrong version, a tampered signature, a substituted key with a
  consistent hash, a deleted or downgraded authentication block, a missing asset
  (404), a refused connection, hostile members (absolute, `..`, escaping
  symlink, a symlink chain reached through a link, a two-hop hard link, `.`
  spellings, a file/directory conflict, device, duplicate, setuid, outside the
  prefix), every URL mutation (traversal, lookalike host, extra segment, query,
  another repository), a symlinked `--root` ancestor, a publication conflict that
  must publish nothing, a failing attestation that must publish nothing, an
  archive whose bundled npm disagrees with the pin, and that a mismatch reaches
  no apt/npm/curl fallback.
- `script/test/node-toolchain.sh` (native): `node --version`, `npm --version`,
  `npx --version`, `/usr/bin/node` and `gh --version` equal the pins, `npm` and
  `npx` resolve into `/usr/local/lib/node_modules/npm`, and `pnpm` still resolves
  through `/usr/bin/node`.
- `script/test/pin-artifact-maintenance.py` (hermetic): the maintenance writer's
  transactional guarantees under a real fault - a failed second architecture, a
  publication that fails on its last file (inclusive rollback), a SIGINT during
  publication, an incomplete recovery (exit 3 with retained material), a
  symlinked destination and symlinked parent, a failed backup (nothing restored,
  nothing reported as published), a restore without a complete backup, an ambient
  `GNUPGHOME` that must stay untouched, `GITHUB_ACTIONS=true`, a non-`X.Y.Z`
  version, and `--check` against a mutated pin.
- `script/test/image-sources` (Rust): pin schema/version/platform/origin/digest
  and authentication rules plus per-field mutations, the base recipe's installer
  mount and `$TARGETARCH` invocation, the version labels, the closed
  `apt_open_gaps`, and the re-spelling, piped-script and apt-package refusals.
- `script/test/parent-pins.py`: a refresh keeps the closed gap record empty and
  refuses an inventory that records one.
