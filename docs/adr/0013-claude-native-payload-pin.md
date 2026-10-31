# 0013: The Claude native payload is a committed closure, and its installer runs offline

Status: accepted for [#98](https://github.com/gregwebs/agent-vm-images/issues/98).
Canonical policy is [docs/image-input-policy.md § Claude native
payload](../image-input-policy.md#claude-native-payload); maintenance commands are
in [CONTRIBUTING § Claude native payload pin](../../CONTRIBUTING.md#claude-native-payload-pin).

## Context

The Claude layer selected an exact version but took the expected bytes from the
same live source. `vendor/install.sh` fetched
`https://downloads.claude.ai/claude-code-releases/<v>/manifest.json` at build time
and trusted its `checksum` as the only identity for the downloaded executable, so
whoever could change the payload could also change the digest beside it. Nothing
about the manifest was committed, so upstream could change the accepted bytes of
an unchanged selected version.

Worse, the executable then ran `claude install <v>` — an opaque native installer
with a hardcoded CDN base — which re-downloaded a secondary payload. The audited
request stream of an ordinary build was:

```
ALLOW https://downloads.claude.ai/claude-code-releases/2.1.286/manifest.json
ALLOW https://downloads.claude.ai/claude-code-releases/2.1.286/linux-arm64/claude
ALLOW https://downloads.claude.ai/claude-code-releases/2.1.286/manifest.zst.json
ALLOW https://downloads.claude.ai/claude-code-releases/2.1.286/manifest.json
ALLOW https://downloads.claude.ai/claude-code-releases/2.1.286/manifest.json.raw-sig.json
ALLOW https://downloads.claude.ai/claude-code-releases/2.1.286/linux-arm64/claude.zst
```

Lines 3-6 are the native step. `claude.zst` decompresses to bytes identical to the
plain `claude` asset the build had already downloaded, so the native step was a
second, unverified acquisition of bytes the build already held. The ~241 MB binary
was fetched only to obtain a program that fetches the same content again.

Three upstream facts made a closure possible:

- `https://downloads.claude.ai/claude-code-releases/<v>/manifest.json.sig` exists:
  a detached OpenPGP signature over the manifest (v4 RSA-4096, SHA-512).
- `https://downloads.claude.ai/keys/claude-code.asc` is the release signing key
  (`Anthropic Claude Code Release Signing <security@anthropic.com>`, primary
  fingerprint `31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE`), documented at
  <https://code.claude.com/docs/en/setup>. That page states that verifying the
  manifest transitively verifies every binary it lists.
- `manifest.json.raw-sig.json` exists but is **not** usable: it is an
  RSA-PKCS1-v1_5-SHA512 record over an undocumented payload that does not verify
  over the manifest bytes under any tried canonicalization. It is neither fetched
  nor committed.

## Decision

### One committed closure

`images/tools/claude/` holds `pin.json`, `evidence/manifest.json`,
`evidence/manifest.json.sig`, the hand-committed release key and `claude_pin.py`.
The pin records, for `linux/amd64` and `linux/arm64`, the manifest platform token,
the exact canonical URL, SHA-256 and byte size of the plain `claude` executable,
plus the URL, evidence path and SHA-256 of the manifest and the signature, and the
key's URL, path, SHA-256 and primary fingerprint. Every URL is recomputed from the
validated version, so a pin that names another location is not this closure. The
key is committed **by hand**: a key change is always a reviewed diff, and no tool
writes it.

### Signer policy

The build verifies the detached signature over the committed manifest with the
committed key, and requires gpg's `VALIDSIG` **primary** fingerprint (its last
field) to equal the pin's `key.fingerprint`. The pin's fingerprint is bound to the
key file's single primary (only the `fpr` record following a `pub` record counts)
and to the signature, so a signature made by a signing subkey of the reviewed
primary is accepted and a pin or key file naming a subkey is refused. A `sec` or
`ssb` record is refused outright rather than counted: `show-only` lists secret keys
in their own records, so a key file that carried the reviewed public key plus any
secret key would otherwise pass the single-primary count and let the import install
an unnamed second signer. No hash-only exception is used.

The reviewed fingerprint `31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE` is a constant
in `claude_pin.py`, but **the build does not read it**. What holds the pin to the
reviewed signer is `script/dev/pin-claude.py` (write mode and `--check`) and the
Rust source gate. The build's job is narrower and structural: bind the pin's
fingerprint to the key file and the signature at this commit.

Every gpg invocation runs in a private `GNUPGHOME` created as a new 0700 directory
under `/tmp` — never `TMPDIR`, never the user's keyring. gpg-agent's control
socket lives in that directory and `sun_path` is 104 bytes on macOS / 108 on
Linux, so a deeply nested temp root makes the key import fail. An unusable root,
or a home path longer than 64 characters, is a refusal with no fallback. A failing
key import is reported as an import failure and is never followed by `--verify`.

### Verified placement, then an offline native step

The install RUN order is: the hook refuses an exact-but-unpinned version; the
vendored installer refuses a non-fresh layout, takes `URL SHA256 SIZE` from
`claude_pin.py select`, makes exactly one classified download, checks its SHA-256,
size and ELF machine, and only then places the bytes at
`$HOME/.local/share/claude/versions/<v>` with an absolute launcher symlink.
Nothing executes in that RUN.

A separate `RUN --network=none` runs `setup-claude.sh`, which re-verifies the
placed bytes and layout and then runs exactly `"$installed" install "<v>"` under
`timeout --kill-after=5 300`. That step writes only local state. A future release
whose native installer insists on downloading fails the build instead of silently
adopting bytes.

### Egress

The build's strict-egress allowlist is exactly one `GET` of
`https://downloads.claude.ai/claude-code-releases/<v>/<token>/claude`, with
`https_only: true`, `deny_redirects: true`, `learn_redirects: false` and no
`learn_hosts`. The gate also runs the offline setup step **through** the proxy so
that any native request is a logged `DENY`, and a real-network control removes the
launcher and requires that denial.

**Egress asymmetry.** Maintenance is different on purpose: every *initial* request
`script/dev/pin-claude.py` makes is on `downloads.claude.ai`, but the shared owner
transport may follow a redirect to `release-assets.githubusercontent.com` or
`objects.githubusercontent.com`, because that transport is shared with the Codex
and agy writers. A followed redirect cannot change what is adopted: the manifest
and signature only count when the committed key verifies them, the published key
only when it is byte-identical to the committed one, and an executable only when
its SHA-256 and size equal the signed manifest. `downloads.claude.ai` does not
redirect at all today. The build, by contrast, fetches one reviewed URL and any
redirect is a route change to review, so its gate denies and learns nothing.

### Maintenance and ordering

`script/dev/pin-claude.py` is the only writer of the pin, both evidence files and
the `ARG AGENT_VERSION_CLAUDE` default, and it publishes them as one transaction
through the shared publisher after printing a field-by-field payload delta. It
refuses GitHub Actions before any fetch, refuses a same-release rewrite, and
refuses a published key that differs from the committed one.
`script/build/agent-versions.sh --write` runs the three closures in the order
agy → Claude → Codex: Codex stages from the Dockerfile Claude has already
published and now refuses a staged Claude ARG difference, because Claude has its
own owner transaction.

## Resolution of the native-installer limitation

The ticket requires that the native installer's limitation be explicitly resolved
rather than declared away. What was measured:

| Pre-state in HOME | Network | Result |
|---|---|---|
| binary only in `.claude/downloads/` (today's shape) | proxy logger | CONNECT `downloads.claude.ai`; on failure leaves a 0-byte `versions/<v>` |
| `versions/<v>` only, no launcher | proxy logger | still CONNECTs `downloads.claude.ai` |
| `versions/<v>` **+ absolute launcher symlink** | proxy logger | exit 0; only CONNECT is `registry.npmjs.org` from the npm update notifier |
| same + `NPM_CONFIG_UPDATE_NOTIFIER=false` | proxy logger | exit 0, **zero** connection attempts; payload inode and bytes unchanged; idempotent |
| same, but `install <other version>` | proxy logger | CONNECTs the CDN and fails, so a version mismatch cannot pass offline |
| same, placed file modified (1 byte appended) | — | still exits 0 — the native step does **not** verify the placed bytes, which is why `setup-claude.sh` verifies before executing |
| real BuildKit `RUN --network=none` from `agent-vm-base:local` | none | "Claude Code successfully installed"; payload `0292fa22…`, `755 0:0`, absolute symlink, `.claude.json` native/false, `claude --version` → `2.1.286 (Claude Code)` |
| `claude doctor` on that image vs today's | none | identical: "Running: native (2.1.286) … No installation issues found." |

Before and after, one architecture:

```text
BEFORE                                           AFTER
RUN install (network)                            RUN install (network)
  GET <v>/manifest.json        (live trust)        claude_pin select (committed manifest + signature)
  GET <v>/<p>/claude           (241 MB)            GET <v>/<p>/claude   (the only request)
  sha256 == live manifest?                         sha256 + size + ELF == pin
  claude install <v>  ── opaque ──┐                place versions/<v> 0755, launcher -> versions/<v>
     GET manifest.zst.json         │ unverified   RUN setup --network=none
     GET manifest.json             │ secondary      installed (bytes/layout) BEFORE exec
     GET manifest.json.raw-sig     │ payload        versions/<v> install <v>  -> local state only
     GET <p>/claude.zst (84 MB)    │                installed + native-state AFTER
     write versions/<v>, symlink ──┘              RUN verify: installed + native-state, then --version
```

Rejected alternatives:

- *Keep the opaque download and pin its products* (`manifest.zst.json`,
  `claude.zst`, raw-sig): the native step fetches them itself from a hardcoded URL
  with no hook to verify them before use. Pinning identities we cannot enforce
  before use fails the ticket's second criterion.
- *One RUN, relying on pre-placement plus the gate*: correct today, but a
  production build would silently accept a future secondary download.
- *Skip the native step and write `.claude.json` ourselves*: reimplements
  undocumented native state and loses the installer's own setup behaviour.
- *Fetch the binary in a separate build stage and bind-mount it*: needs
  cross-stage soft-fail/status plumbing for no gain.
- *A poisoned proxy environment instead of `--network=none`*: not structural; a
  raw socket ignores proxy environment variables.

## Trust limits

- Pinning establishes which bytes were reviewed, not that a release is safe.
- The build binds the pin's fingerprint to the committed key and the signature and
  does **not** read `REVIEWED_SIGNER_FINGERPRINT`. `pin-claude.py --check` and the
  Rust gate hold the pin to that constant. Do not describe a build as enforcing
  it.
- `manifest.json.raw-sig.json` is not verified and not used. There is no hash-only
  exception for this payload.
- Key rotation is manual and not supported by the writer in this release:
  replacing the key file in place, even together with the constants, makes the
  committed pin unverifiable and leaves the writer unusable. The procedure is in
  [CONTRIBUTING](../../CONTRIBUTING.md#claude-native-payload-pin).
- A withdrawn release makes commits that pin it unbuildable. That is the intended
  failure mode: a missing reviewed payload fails instead of being replaced.
- The npm update notifier is suppressed in the offline step because
  `claude install` runs `npm uninstall --global @anthropic-ai/claude-code`; it is
  the only other network use, and it is not a payload.
- gpg homes live under `/tmp` because of the gpg-agent socket path limit; that
  root must be writable on the maintainer host and in the build.

## Stop conditions

- If a future version's native step needs the network with a pre-placed payload,
  the build fails. Do not loosen `--network=none`; reassess this decision.
- If the real-network egress control stops producing a `DENY`, stop acceptance. A
  plain proxy that only refuses `CONNECT` is not an equivalent control: the native
  step exits 0 behind one despite its connection attempts (measured).
- If the published key changes, the writer refuses and nothing is written. Follow
  the rotation procedure; do not replace the key file in place.

## Evidence

The measurements this decision rests on are recorded above and are reproducible
without private scratch:

- the audited before/after request streams (§ Context, § Resolution);
- the native-step pre-state table (§ Resolution): a placed payload plus an
  absolute launcher makes `claude install <v>` exit 0 with zero connection
  attempts once `NPM_CONFIG_UPDATE_NOTIFIER=false` is set, while a missing
  launcher or a version that is not placed still reaches the CDN;
- the signature verification (`gpg --import` of
  `https://downloads.claude.ai/keys/claude-code.asc`, then
  `gpg --verify manifest.json.sig manifest.json` against the committed
  `evidence/` bytes) and the gpg-home length limit (§ Trust limits);
- the build property itself: `docker build -f images/standard/Dockerfile` fails
  on any committed-closure mutation, and the `RUN --network=none` setup step
  cannot fetch.

The pull request that introduced this decision carries the run logs: the native
arm64 build, the finished-image audit, the strict-egress Claude request log and
its denial control, and the closure-mutation builds. `script/test/claude-pins.py`
holds the hermetic case table for every refusal described here.
