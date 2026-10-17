# Contributing

This repository owns image sources, not launcher releases. Read the
[ownership manifest](docs/image-source-ownership.md) and
[installer trust boundaries](images/tools/README.md). No agent-vm workspace
version bump, Rust application build, submodule or launcher packaging is needed.

## Conventions

- Bash uses `set -euo pipefail`; POSIX installers use `set -eu`. Host scripts
  must work with macOS Bash 3.2. Invoke scripts according to their shebang.
- Preserve subprocess errors. Required-agent failure must never produce a healthy
  degraded standard image. Low-level classified-failure tests are audit fidelity,
  not a production bypass.
- Keep canonical docs linked from README, comments about invariants/reasons and
  multi-paragraph Why / How commits.
- Clean only invocation-owned scratch paths, containers and fixture tags. Never
  prune tagged images or volumes to get tests passing; no pruning in build
  helpers or audits. CI's explicit unused SDK cleanup is disposable-runner only.
- Review exact pins, integrity/lock diffs, vendor licenses/provenance and modes.
  No `curl | shell` replacement or automatic latest lookup during builds/CI.
  OS/apt inputs still float; this is not byte-reproducible distro construction.

## Selection ownership and maintenance

`images/standard/Dockerfile` owns eight selection labels; base owns pnpm via
`images/pnpm/pin.json` and a mirrored literal base label. Its deprecated standard
AGENT_VERSION_PNPM is a compatibility assertion only (empty/equal accepted).
Four exact nonempty installer defaults reference ARGs; dsh, Pi, bridge and OpenCode-plugin
empty/equal slots reuse committed manifest/lock bytes. Literal label fallbacks
mirror manifests. Labels record selection, not health. Unrelated slots/locks
must not change when one owner is bumped.

```bash
bash script/build/agent-versions.sh --write
bash images/tools/dsh/upgrade-dsh.sh VERSION --pnpm 11.11.0
bash images/tools/pi/upgrade-pi.sh VERSION
bash images/tools/pi/bridge/upgrade-bridge.sh VERSION
bash images/tools/opencode/plugins/upgrade-plugins.sh VERSION # requires pnpm 11.11.0
bash script/test/contracts.sh
# Review central Dockerfile, relevant JSON/lock diff and vendor provenance.
bash images/build.sh base   # rebuild changed base before standard
bash images/build.sh standard
bash script/test/standard-image.sh agent-vm-base:local agent-vm-standard:local
bash script/test/pi-runtime.sh agent-vm-base:local agent-vm-standard:local
bash script/test/opencode-runtime.sh agent-vm-base:local agent-vm-standard:local
```

pnpm updates require reviewed exact URL, equivalent SHA-512/SRI and freshly
extracted pristine-bundle SHA-256 together; never refresh checksums automatically.
See [base pnpm maintenance](images/pnpm/README.md) and
[runtime defaults](docs/node-package-defaults.md). dsh --pnpm can only equal the
reviewed base pin, never independently select a version/dist-tag. Its duplicate
npm lock package remains transitional until #42.

The bumper resolves/validates all four upstream values before co-editing one
scratch Dockerfile. Explicit developer latest queries are permitted; the bumper
refuses GitHub Actions. Builds consume committed selections and never call bump
scripts. Lock upgrades retain dsh sandbox/layout and location-freeze review, Pi
sibling hashes/full-tree checks and separate bridge peer-alias omission checks.
Shared-file edits are sequential developer operations, not parallel maintenance.

Local transactional replacement backs up every destination before publishing.
A failed partial copy restores the failing destination too, in reverse order,
including removal of originally absent paths. Status 1 means restored failure;
2 means usage; **3 means incomplete rollback**. On 3, callers preserve the entire
scratch directory and print its absolute recovery path. Stop and recover from
those backups before further edits/builds/commits. Never delete recovery material
as routine cleanup or mistake a failed operation for a successful bump.

Vendored updates require upstream snapshot + reviewed patch + byte-equivalence
checks, provenance and license review. Zellij changes require independently
reviewed archive SHA-256 pins for both architectures. Installer defaults also
require reviewed `script/test/fixtures/installer-egress/selections.tsv` updates
and strict restricted-egress tests, never automatic allowlist broadening.

Example images under `examples/layers/` are not part of the standard product.
`contracts.sh` checks them statically; building them is manual until
[#33](https://github.com/gregwebs/agent-vm-images/issues/33). Their pin policy and
bump order are in [Example images](docs/image-source-ownership.md#example-images-exampleslayers).

## Contributor prerequisites and fast gates

Build users need Docker only. Contributors running contracts need Bash, git, jq,
Node/npm, Python 3, GNU coreutils `timeout`, shellcheck, **actionlint v1.7.7**, and
Rust/rustfmt/clippy **1.98.1** for the independent test-only Cargo package.
On macOS:

```bash
brew install coreutils jq shellcheck actionlint
export PATH="$(brew --prefix coreutils)/libexec/gnubin:$PATH"
# If Homebrew actionlint differs, install the reviewed version (Go required):
go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
export PATH="$HOME/go/bin:$PATH"
rustup toolchain install 1.98.1 --component rustfmt --component clippy
bash script/test/contracts.sh
/bin/bash script/test/agent-versions.sh
/bin/bash script/test/upgrade-scripts.sh
```

`contracts.sh [--guard-only]` works from any cwd. Default runs the explicitly
listed hermetic suites, watchdog, transactional/gate/helper controls and
finished-image oracle `--self-test`, and `example-layers.sh --self-test`; both
modes run syntax, warning-level shellcheck, Python AST, Node syntax, Rust
test/fmt/clippy and pinned workflow lint, plus the example-image contract
(`example-layers.sh`: Chrome capability/wrapper recipe text, parent/digest ARG
shapes and checksum tokens, default-severity shellcheck of the listed example scripts).
Do not discover and execute every `.sh`: installers are not host test entrypoints.
The preserved Claude seed's literal JavaScript interpolation yields informational
SC2016; warning-level lint does not rewrite reviewed runtime bytes to silence it.

```bash
(cd script/test/image-sources && cargo test --locked)
(cd script/test/image-sources && cargo fmt --check)
(cd script/test/image-sources && cargo clippy --locked --all-targets -- -D warnings)
actionlint .github/workflows/*.yml
bash script/test/standard-image.sh --self-test
```

Rust guards retain lock/parser mutation checks and narrow source-presence checks;
static strings do not certify actual installation. The real final-certification
suite tests strict eight-record bytes and a genuine fixed-path Codex verifier
with a wrong-version PATH shadow that the final gate must reject.

## Expensive acceptance and CI

Inspect the context's daemon-backed `docker` driver and actual daemon storage.
Full standard requires at least **25 GiB free after base**, ideally 35–40 GiB.
Do not attempt it below threshold. No reduced image, skipped agent or emulation
substitutes for full native amd64 and arm64 evidence.

`standard-image.sh BASE STANDARD [--platform linux/ARCH] [--expect suffix=version …]`
audits finished images, not builds. Expectations derive from committed sources,
not labels. Exact overrides require independent `--expect`; labels and reports
must agree. It checks eight records, exact wire reports, four numeric UID/GID
pairs, Pi warning/provider registration, committed unchanged locks, synthetic
Claude/Pi seeds and disposable T5 negatives. Docker CMD does not run seeds
implicitly. `pi-runtime.sh BASE STANDARD` retains deeper credential-free runtime
behavior, replacement, warning, bridge and seed cases. Container checks do not
prove VM provisioning, authenticated sessions or actual LSP execution.

`standard-image.sh --pi-audit-status BASE_IMAGE [--platform linux/ARCH]` runs the
real generated Pi access audit against a controlled fixture (no full standard).
It requires a nonzero verdict when the Pi command or seed-hook check fails, even
when the later extension-content check passes, and a nonzero verdict when the
content check fails independently; CI runs it in the native audit phase.

```bash
bash script/test/shipped-installer-network.sh agent-vm-base:local --platform linux/arm64
# Strict defaults: all three vendored installers, the separate pnpm plugin profile and denied-latest control.
# --overrides adds tracked exact alternates; --observe is NEVER acceptance.
# Redacted request evidence (one log per selection plus the mutation) is written
# under target/ci-logs/installer-egress-<run>/ and uploaded; a run that writes no
# evidence fails, and mitmdump runs as the invoking numeric uid with --cap-drop
# ALL so it can write its invocation-owned cert/log mounts.
```

CI uses read-only permissions, pinned actions and checkout without persisted
credentials. `contracts.yml` is PR/push/manual and reusable via `workflow_call`;
both expensive workflows invoke it as a local prerequisite job (`needs`).
`build-local.yml` builds/audits one full product on native `ubuntu-24.04` amd64
and `ubuntu-24.04-arm`; 330-minute jobs separately bound setup, 180-minute builds
and 90-minute audits. `installer-network.yml` runs strict default PR audits on
both native architectures, with separate 60-minute base / 90-minute audit bounds
and 180-minute job limits. Optional alternates are manual only. macOS CI is
explicitly deferred; Bash 3.2 checks are local. `release-transports.yml` runs
read-only loopback registry/archive/negative fixture controls, with no GHCR
credentials and no VM boot, on both native architectures; it proves transport
mechanics only, never released-standard acceptance. `verify-release-boot.yml`
boot-verifies a published release on hosted amd64 after publication, on dispatch,
or on `test/release-boot/<VERSION>` pushes; never a PR gate. Arm64 boot stays native.

Hosted runner cleanup names four unused SDK directories only, is bounded to ten
minutes, and remeasures the daemon filesystem. Capacity/native availability
failure is failure, not green acceptance. Fix/rerun failed mandatory jobs. If a
runner cannot supply evidence, attach equivalent clean native-host runs at the
proposed SHA before merge; postmerge dispatch is not a substitute. Record first
run phase durations per architecture, use at least 1.5× measured maxima and keep
initial floors; if adequate headroom exceeds hosted limits, use recorded native
runs rather than drop audits. Cross-job image transport for a release is limited
to the run/attempt-scoped staging artifact; ordinary PR CI moves no image
archives.

## Standard image publication

`images/standard/version` is the single owner of the image version; bumping it
is a reviewed source change and never bumps any launcher workspace version.
Publication is maintainer-only and manual: `.github/workflows/release-standard.yml`
is `workflow_dispatch` only (no PR/push/schedule trigger and no arbitrary
build-arg/source input), requires `refs/heads/main` of `gregwebs/agent-vm-images`,
and runs in the protected `image-release` environment. Read
[standard image releases](docs/standard-image-releases.md) before dispatching.

Release prerequisites are deployment prerequisites, not PR gates: an
authoritative owner `read:packages` inventory token, the confirmed effective
`Contents: write` grant, GHCR package write and public visibility, and a native
arm64 host with a compatible standalone `msb` runtime (the amd64 boot runs on
hosted CI with the pinned runtime).
Checksum-pinned tooling (Syft `v1.20.0`, gh `v2.97.0`) and apt-selected
skopeo/coreutils/jq/Python (floating OS inputs, versions recorded) are installed only on disposable Linux CI by `script/release/install-ci-tools.sh`;
host operators may provide their own tools but must record exact versions.
Signing and SBOM use GitHub/Sigstore attestations; no custom signer exists.

Merging the release workflow is not release acceptance. #264 remains complete
only after the published prerelease is consumed anonymously by digest and by
exact archive/SBOM on **both** native architectures, both `msb` boots pass, and
a named maintainer reviews the hash-bound evidence and explicitly promotes the
same bytes. A merged workflow or a passing fixture must never be reported as a
native or published acceptance.

### Evidence and failures

Record source/image-repo SHAs, native architecture, driver, daemon free bytes,
commands/statuses, durations, image IDs/sizes, expected selections, tested numeric
IDs and logs/run links. CI always uploads logs and phase measurements, not image
archives. State each not-run check explicitly. Separate fast controls, actual
full installation, optional plugins, strict egress and runtime observations.
A timeout/nonzero is failure. Until both native full builds and mandatory audits
pass, merge remains blocked, even if contracts pass. Publication is
[#264](https://github.com/gregwebs/agent-vm/issues/264) and is described in
[standard image releases](docs/standard-image-releases.md); launcher/submodule
integration is [#265](https://github.com/gregwebs/agent-vm/issues/265).

## OpenCode plugin maintenance

The [OpenCode integration](images/tools/opencode/README.md#image-owned-claude-plugin)
is a new pnpm consumer, not a migration of dsh, Pi, bridge or Copilot (#42 remains
open). Host lock maintenance requires actual pnpm **11.11.0**; pnpm 12 must not
regenerate the lock. Empty/equal preparation validates committed YAML without a
manager or registry call; frozen installation still verifies registry metadata.
The hard-only runner never emits a transport receipt or retries with npm.

Eight required agent records and seven agent verifiers exclude the separate
base-pnpm identity/verification calls. Nine selection labels include base pnpm;
the plugin manifest's packageManager field creates no additional selection.
The four strict-egress profiles retain the native installers' narrower contracts;
the plugin profile permits only eleven exact package documents and three exact
tarballs, with HTTPS/443/GET/no query and no redirect learning. Both native full
builds, the real launcher policy controls and all runtime/egress audits remain
merge prerequisites. Do not substitute hermetic fake-manager controls for them.


## Bounded pnpm primitives

#42 PR 1 adds build-only graph/access interfaces, not an agent migration. Read
[ownership](docs/image-source-ownership.md#bounded-pnpm-primitives-42-pr-1) and the
[parser ADR](docs/adr/0001-build-only-pnpm-graph-parser.md). Node is required for
the bounded CLI; no host YAML install or package-manager invocation is needed:

```sh
node images/recipe-contract/pnpm-lock.mjs check dsh script/test/fixtures/pnpm-lock/tiny-dsh
node images/recipe-contract/pnpm-lock.mjs graph bridge script/test/fixtures/pnpm-lock/real-bridge
node script/test/pnpm-vendor.mjs
node script/test/pnpm-lock-inputs.mjs
bash script/test/tool-access.sh
bash script/test/standard-node-consumers.sh --self-test
bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer primitives --platform linux/arm64
```

`pin KIND PREFIX` deliberately rewrites only three-input SHA-256 consistency
pins after full structural/policy validation; builds must never re-pin failures.
Fixtures retain independent expected graphs and a real pnpm 11.11.0 emitted v9
lock. Never use host pnpm 12 for any lock work. Vendor files are digest-checked,
not linted/rewritten; Rust inventory guards run without Node. `contracts.sh`
explicitly runs vendor/parser and harness self-tests, without Docker/network.

`check-tool-access.py [--content | --tree] TARGET...` accepts explicit tree
directories (bare names beneath cwd, not PATH). It audits every physical entry
and symlink hop, rejects external targets/ACLs/special files and preserves lexical
ancestry. Tree readability does not prove bins executable: run normal command/
interpreter/native audits too. OpenCode's independent payload oracle stays intact.

The focused smoke needs an existing native Linux base and daemon-backed Docker
builder (a Darwin client is supported), Python, Node22/pnpm in base and functional
setpriv/Linux access xattrs/MKNOD. No apt/acl/setfacl prerequisite or base rebuild.
It requires 768 MiB free on daemon writable-layer and owned fixture-volume
filesystems: 512 MiB retained reserve plus a maximum 256 MiB setup allocation.
This threshold is **not** the full product's 25 GiB floor. Setup grants MKNOD only
to its own short-lived container; final four UID/GID probes have zero capabilities
and no attached networks. HOME overmounts are declared at create. ACL/device
creation/readback/removal and repaired positives are mandatory, never skips.
Owned logs/provenance are retained in `target/ci-logs/pnpm-primitives-*/`.
The overall watchdog is 1800s, individual runtime probes at most 180s.

Only `--consumer primitives` exists in this slice; dsh/pi/copilot return usage2.
A local base with matching pnpm bytes but no whole-source provenance provides
diagnostic smoke only. Both clean native CI rows must still build/audit the full
unchanged standard product at the proposed SHA, including current strict egress.
Do not attempt a full local build below the product capacity floor or prune
images to make room. The [failure ADR](docs/adr/0002-pnpm-terminal-failure-boundary.md)
records the proposed later bridge narrowing; no production receipt changes here.
