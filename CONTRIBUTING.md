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
  External parents are digest-pinned, and the local-base consumers
  (`images/standard/Dockerfile`, `examples/base-extension/Dockerfile`) are bound
  to `images/base-inputs.sha256`, the committed list of the sources the base is
  built from. Editing a listed input, or adding one, requires regenerating that
  manifest and the extension's `AGENT_VM_BASE_IDENTITY` default with
  `bash script/build/base-inputs.sh --write` before either consumer builds; a
  build only verifies the committed bytes. See
  [image input policy](docs/image-input-policy.md#repository-base-identity-a-committed-input-manifest)
  and [the ADR](docs/adr/0005-repository-base-identity.md).
  Official Debian apt packages may float with normal apt authentication;
  optional plugins stay with [#101](https://github.com/gregwebs/agent-vm-images/issues/101).
  Node and the GitHub CLI are
  [pinned release artifacts](docs/image-input-policy.md#pinned-third-party-release-artifacts):
  exact upstream archives whose per-architecture SHA-256, evidence files and
  version labels are committed, installed by `images/install-artifact.sh` and
  refreshed only by `script/dev/pin-artifact.py` (see
  [ADR 0006](docs/adr/0006-third-party-artifact-pins.md)). This is a
  pinned-parent boundary, not byte-reproducible distro construction. The base
  build verifies the Debian baseline with `images/apt-policy/check-sources.sh`
  both before and after the package steps, `images/parent-pins.json`
  `apt_open_gaps` is a closed empty record, and any apt source or key
  installation written in a spelling the image-source gate knows (`/etc/apt`
  with any suffix, `sources.list`, `.sources`, `Signed-By`, `trusted.gpg`, the
  keyring directories, `URIs:`, `Types: deb`, `Dir::Etc`, `apt-config`, the
  NodeSource and GitHub CLI marks), a downloaded script piped into a shell, or
  `nodejs`/`npm`/`gh` through apt fails it.
- `CODING_STANDARDS.md` is generated from the shared standards and stays
  unchanged. Its immutable-input rule applies here with exactly one exception:
  packages from official Debian apt repositories, with the reviewed policy in
  `images/apt-policy/` and normal apt authentication retained. That exception
  does not cover parent images, other apt repositories or plugins.

## Release-artifact maintenance

Node and the GitHub CLI come from committed release archives. Only
`script/dev/pin-artifact.py` writes their pins, evidence and labels, on a
maintainer host (it refuses `GITHUB_ACTIONS=true`):

```bash
python3 script/dev/pin-artifact.py --artifact node --version 22.23.5
python3 script/dev/pin-artifact.py --artifact gh --version 2.104.0
python3 script/dev/pin-artifact.py --check
```

It resolves both architectures, verifies every piece of evidence (the Node
manifest signature against the committed release key, or `gh attestation
download` plus `gh attestation verify` under the pinned policy for the GitHub
CLI, which needs your `gh` login) and only then publishes
`images/artifacts/<name>.json`, its evidence files and the
`org.agent-vm.version.<name>` label as one transaction. A partial upstream
failure leaves the committed files byte-identical; status `3` means the rollback
was incomplete and prints the work directory to recover. It never commits and
never adopts a newer version by itself. Because `artifacts/` and
`install-artifact.sh` are base inputs, finish with:

```bash
bash script/build/base-inputs.sh --write
bash script/test/artifact-pins.sh
python3 script/test/pin-artifact-maintenance.py   # the writer's own fault cases
bash images/build.sh base    # then: bash script/test/node-toolchain.sh agent-vm-base:local --platform linux/arm64
```

## Parent-pin maintenance

Only `script/dev/refresh-parent-pins.sh` writes parent pins, on a maintainer host
(the script refuses `GITHUB_ACTIONS=true`). Builds never resolve a parent:

```bash
bash script/dev/refresh-parent-pins.sh debian
bash script/dev/refresh-parent-pins.sh golang-builder [golang:VERSION-trixie]
bash script/dev/refresh-parent-pins.sh standard-release X.Y.Z
```

A parent-layer fix — including a `ca-certificates` revision or a withdrawn CA
root — needs a reviewed refresh and a rebuild of both products; rebuilding alone
keeps the frozen parent, and a floating `apt-get install` needs cache
invalidation (`--no-cache`) to requery the archive. Never add `apt-get upgrade`.
A refresh that moves a local parent also republishes `images/base-inputs.sha256`
and the extension default, because a listed input moved; review both files in the
diff, and run `bash script/build/base-inputs.sh --check` after committing.

The first adoption was `--bootstrap` (the manifest and extension default are
adopted separately with `bash script/build/base-inputs.sh --write`). A refresh
prints old/new references, per-platform digests and provenance, and the apt
policy diff, then publishes the inventory, the affected recipes, the extracted
Debian baseline and — for a local parent — the base-input manifest and extension
default in one transaction. Status 3 means an undecided or incomplete rollback: `recovery.json`
and the numbered backups are retained and must be recovered from before further
work. A changed hash-only exception digest prints
`hash-only exception requires renewed review`; re-date it in the diff before
merging. Policy and the exact exception language: [image input policy](docs/image-input-policy.md).

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
[runtime defaults](docs/node-package-defaults.md). dsh `--pnpm` and
`AGENT_VERSION_PNPM` accept only empty or the reviewed base pin, never an
independent version or dist-tag; see [dsh maintenance](#dsh-maintenance-and-graph-freeze).

The bumper resolves/validates all four upstream values before co-editing one
scratch Dockerfile. Explicit developer latest queries are permitted; the bumper
refuses GitHub Actions. Builds consume committed selections and never call bump
scripts. Lock upgrades retain dsh graph-freeze review (retained records and
sandbox reachability, not npm locations), Pi sibling hashes/full-tree checks and
separate bridge peer-alias omission checks.
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
`contracts.sh` checks them statically. `chrome-devtools` is built and runtime-checked
natively on amd64 and arm64 by `chrome-example.yml`; the interrupted-run path
(cleanup after the workflow watchdog fires) is re-proved by
`bash script/test/chrome-example-watchdog.sh` in the arm64 CI row after the suite,
reusing its inspected parent image ID; the other examples remain manual until
[#33](https://github.com/gregwebs/agent-vm-images/issues/33). Their pin policy and
bump order are in [Example images](docs/image-source-ownership.md#example-images-exampleslayers).

The Chrome harness reserves 90 seconds total for cleanup (5 seconds per client,
plus escalation and evidence). Any supervising watchdog's TERM grace must exceed
that budget plus a 5-second margin; use `host-watchdog.py --grace-seconds 110`.
Its default 10-second grace can kill cleanup before evidence or removal finishes.
Set `CHROME_EXAMPLE_WATCHDOG_GRACE_SECONDS=110` alongside that flag to enable the
harness's fail-fast contract check; it cannot discover arbitrary parent settings.
The interrupted-run control uses the same grace. Its slow-cleanup case simulates
a daemon outage with a reduced, scratch-only 9-second cleanup budget: a listing
must time out, the next must be `skipped-deadline`, and cleanup must report failure.
Ordinary listings and real suite runs must instead complete cleanup successfully.

## Chrome example MCP maintenance

The Chrome example installs one pinned `chrome-devtools-mcp` selection. Only
`script/dev/upgrade-chrome-devtools-mcp.sh` writes the committed inputs; a build
only verifies them. Run it on a maintainer host, never in CI (it refuses
`GITHUB_ACTIONS=true`):

```bash
bash script/dev/upgrade-chrome-devtools-mcp.sh [VERSION]
```

`VERSION` defaults to the registry's `latest`, which discovery only proposes: the
resolve step still applies the pinned age and trust policy. The script
re-resolves the lock inside the parent image, pins the bytes, requires the
published tarball's SHA-512 to equal the lock integrity, verifies the npm SLSA
provenance under the identity in `script/build/chrome-mcp-provenance.sh`
(repository `ChromeDevTools/chrome-devtools-mcp`, its publish workflow, OIDC
issuer `https://token.actions.githubusercontent.com`, source ref
`refs/tags/chrome-devtools-mcp-vX.Y.Z`, `--deny-self-hosted-runners`, empty
`GH_CONFIG_DIR`, no token), and publishes the four inputs plus the bundle as one
transaction. Missing or unverifiable provenance is a hard failure; do not add a
hash-only exception.

Prerequisites: Docker with a running daemon, `jq`, `gh` (tested 2.97.0; needs no
login or token), `curl`, GNU coreutils (`timeout` must be GNU; `sha256sum`,
`sha512sum`, `base64`, `od`, `awk` and `cmp` are also called), and network
access. The first adoption, with no lock, pins or bundle committed, skips the
committed-input check; every later run requires the committed inputs to pass
`check` first, and any other mix of missing files is refused as partial pinned
state.

Status 0 means published or unchanged (a repeat run prints `unchanged`); 1 means
failed with the sources unchanged or fully restored; 2 is usage; 3 means the
publication result is undecided or a rollback is incomplete — the scratch
directory is retained and its path printed, and must be recovered from before
further edits, builds or commits; a signal reports 128+N, or 3 when its window
cannot restore the sources. Never hand-edit a pin to clear a failure.

agent-vm launches `npx -y chrome-devtools-mcp@1.0.1` and the wrapper maps exactly
that spelling. Publishing a different version makes the launcher's MCP entry
refuse with exit status 2 until agent-vm changes its argv, so coordinate such a
bump before merging; the updater warns for that case. The pinned policy and
wrapper behaviour are documented in
[Example images](docs/image-source-ownership.md#chrome-mcp-pin) and
[the Chrome example README](examples/layers/README.md#pinned-mcp-package).

`bash script/test/chrome-example.sh` re-verifies the committed bundle under the
same identity, so it requires `gh` too.

## dsh maintenance and graph freeze

dsh uses four reviewed pnpm inputs (`package.json`, `pnpm-lock.yaml`,
`pnpm-workspace.yaml`, `pnpm-inputs.sha256`) and the inherited base manager.
`--pnpm` and `AGENT_VERSION_PNPM` accept only empty or the base pin;
`packageManager` is an assertion, not another selection. Host image maintenance
requires authenticated pnpm **11.11.0**, never a PATH `pnpm` 12, and Go: a host
upgrade builds and authorizes the native composed helper
(`script/build/recipe-contract.sh --native`) before the first graph check, so a
source checkout on its own is not a runnable contract directory. Empty/equal
preparation checks all four file bytes and the graph without starting a package
manager. Changed exact dsh selections freeze retained identities and peer-context
snapshots, but may replace transitives within dsh's own closure; they are not
fully transitive-frozen. Read [ADR 0003](docs/adr/0003-dsh-graph-freeze.md) and
the [migration ledger](docs/standard-pnpm-migration.md).

Cordis overrides are derived without the old map before any final resolution. A
changed map stops an ordinary upgrade. Use
`bash images/tools/dsh/upgrade-dsh.sh VERSION --propose-cordis-overrides /tmp/dsh-cordis.json`,
review the exact requirements/delta, then
`bash images/tools/dsh/upgrade-dsh.sh VERSION --cordis-overrides /tmp/dsh-cordis.json`.
The supplied map must match independent derivation; real native install/web/
subprocess gates run before publication. A failed compatible changed-selection
leg remains blocked, not a synthetic success. The four inputs and central
Dockerfile publish transactionally; rollback status 3 retains recovery material
and requires recovery before further work.

Lock work on a host without the Linux launcher (macOS) authenticates the stock
distribution instead of using PATH pnpm: extract the reviewed archive with
`images/pnpm/bootstrap.py` into invocation-owned scratch, run that
`package/bin/pnpm.mjs` with an absolute Node path, and export the five defaults
plus a private HOME/XDG. That is authenticated stock 11.11.0 and claims no
launcher identity. The upgrade script does this internally; run it, do not
reconstruct the argv by hand. The five image defaults arrive through the exported
environment plus the explicit policy flags on every manager call, and the
resolving call is `install --lockfile-only --no-frozen-lockfile`, which
re-verifies the emitted lock.

```bash
bash script/test/dsh-prepare-lock.sh
bash script/test/dsh-verify.sh
python3 script/test/dsh-project.py
/bin/bash script/test/upgrade-scripts.sh
python3 script/test/standard-node-layout.py --self-test
bash script/test/standard-node-policy.sh --self-test
bash script/test/standard-node-consumers.sh --self-test
node script/test/pnpm-lock-inputs.mjs
```

Focused real runs need a native base image and take the dsh handler, the changed
`0.1.5-rc.1` candidate and the policy matrix separately. A stale local base makes
them diagnostic; only clean native amd64 **and** arm64 runs at the proposed SHA
are acceptance:

```bash
bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer dsh --platform linux/arm64
bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer dsh --version 0.1.5-rc.1 --platform linux/arm64
bash script/test/standard-node-policy.sh agent-vm-base:local --consumer dsh --platform linux/arm64
```

## Contributor prerequisites and fast gates

Build users need Docker only. Contributors running contracts need Bash, git >= 2.30, jq,
Node/npm, Python 3, GNU coreutils (`timeout`, and `sha256sum` for the committed
base-input manifest), `xz` on Linux (GNU `tar -cJf` in the
shellcheck suite's fixture), **ShellCheck 0.11.0**, **actionlint v1.7.7**, and
Rust/rustfmt/clippy **1.98.1** for the independent test-only Cargo package.
The fixture tests trace `commit`'s maintenance child, which git 2.30 introduced.
`#!/bin/sh` scripts are syntax-checked with `dash -n` (macOS ships `/bin/dash`);
without dash the gate falls back to `sh -n` with a note, and CI requires dash.
On macOS:

```bash
brew install coreutils jq shellcheck actionlint
export PATH="$(brew --prefix coreutils)/libexec/gnubin:$PATH"
# If Homebrew shellcheck is not 0.11.0, install the reviewed release (any absolute dir outside the checkout):
bash script/dev/install-shellcheck.sh "$HOME/.local/opt/shellcheck-0.11.0"
export PATH="$HOME/.local/opt/shellcheck-0.11.0:$PATH"
# If Homebrew actionlint differs, install the reviewed version (Go required):
go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7
export PATH="$HOME/go/bin:$PATH"
rustup toolchain install 1.98.1 --component rustfmt --component clippy
bash script/test/contracts.sh
/bin/bash script/test/agent-versions.sh
/bin/bash script/test/upgrade-scripts.sh
```

`contracts.sh [--guard-only]` works from any cwd; both modes first run
`bash script/build/base-inputs.sh --check`, so a stale manifest or extension
default fails before any suite. Default runs the explicitly
listed hermetic suites, watchdog, transactional/gate/helper controls, the
dsh project authorization/publisher unit table, the independent layout oracle
`--self-test`, the policy harness `--self-test`, the finished-image oracle
`--self-test`, and `example-layers.sh --self-test`; both
modes run syntax checks by shebang (dash for sh scripts when available),
pinned warning-level shellcheck, Python AST, Node syntax, Rust
test/fmt/clippy and pinned workflow lint, plus the example-image contract
(`example-layers.sh`: Chrome capability/wrapper recipe text, parent/digest ARG
shapes and checksum tokens, default-severity shellcheck of the listed example scripts).
Do not discover and execute every `.sh`: installers are not host test entrypoints.
Contributor tooling lives in `script/dev/` and is never a suite.
The preserved Claude seed's literal JavaScript interpolation yields informational
SC2016; warning-level lint does not rewrite reviewed runtime bytes to silence it.

```bash
(cd script/test/image-sources && cargo test --locked)
(cd script/test/image-sources && cargo fmt --check)
(cd script/test/image-sources && cargo clippy --locked --all-targets -- -D warnings)
actionlint .github/workflows/*.yml
bash script/test/standard-image.sh --self-test
python3 script/test/base-inputs.py
bash script/build/base-inputs.sh --check
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
# Repository base identity: base stamp == this checkout, stale/stamp-less bases
# refused by both consumers, and the same identity under both helper deliveries.
bash script/test/parent-identity.sh agent-vm-base:local --platform linux/arm64
# Strict defaults: all three vendored installers, the separate pnpm plugin profile and denied-latest control.
# --overrides adds tracked exact alternates; --observe is NEVER acceptance.
# Redacted request evidence (one log per selection plus the mutation) is written
# under target/ci-logs/installer-egress-<run>/ and uploaded; a run that writes no
# evidence fails, and mitmdump runs as the invoking numeric uid with --cap-drop
# ALL so it can write its invocation-owned cert/log mounts.
```

CI uses read-only permissions, pinned actions and checkout without persisted
credentials. `chrome-example.yml` has provisional bounds of 5 minutes checkout,
20 prerequisites, 60 suite (3300-second watchdog plus 110-second TERM grace),
13 arm64 watchdog control (600-second watchdog plus 110-second TERM grace),
5 summary and 10 upload inside a 120-minute job (113 minutes of steps plus
7 minutes margin); reset bounds to at least 1.5× first green per-architecture
measurements, keeping these floors. `contracts.yml` is PR/push/manual and reusable via `workflow_call`;
expensive build/audit workflows invoke it as a local prerequisite job (`needs`).
`build-local.yml` builds/audits one full product on native `ubuntu-24.04` amd64
and `ubuntu-24.04-arm`. Every bounded step plus the evidence upload fits the job:
20 minutes prerequisites + 20 minutes capacity/builder + 180 minutes build +
120 minutes audit + 10 minutes upload = 350 minutes inside a 360-minute job, and
each host watchdog is shorter than its own step bound so a step timeout cannot
pre-empt the audit's measured budget. `installer-network.yml` runs strict default PR audits on
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


## Go build helpers

Read [ADR 0004](docs/adr/0004-go-build-helpers.md) for version ownership,
build-only delivery, supported environments, parity exceptions and evidence.
Install the module-owned lint pin through Go's checksum database:

```sh
bash script/dev/install-golangci-lint.sh
(cd images/recipe-contract && GOTOOLCHAIN=auto go test ./... && GOTOOLCHAIN=auto go vet ./... && golangci-lint run)
bash script/test/contracts.sh
bash script/test/contracts.sh --guard-only
```

Dependencies are vendored (`images/recipe-contract/vendor/`, `-mod=vendor` in
every build). To update one, follow ADR 0001 § Amended by #75:

```sh
cd images/recipe-contract
GOTOOLCHAIN=auto GOFLAGS=-mod=mod go get go.yaml.in/yaml/v3@vX
GOTOOLCHAIN=auto GOFLAGS=-mod=mod go mod tidy
GOTOOLCHAIN=auto GOFLAGS=-mod=mod go mod vendor
```

then review the vendor diff, license/NOTICE and advisories (`govulncheck`), update
the pinned expectations in `script/test/image-sources/tests/image_sources.rs`
(`go.mod`, `go.sum`, `modules.txt`, the file set) and `script/test/recipe-contract-vendor.sh` (`expected_sum`),
run the parity vectors and
`bash script/test/recipe-contract-vendor.sh`, and update the ADR pin row. Never
hand-edit a vendored file.

Build and audit commands (mount `<context>/out`, never a Darwin binary in Linux):

```sh
bash script/build/recipe-contract.sh --native
bash script/build/recipe-contract.sh --platform linux/arm64
bash images/build.sh base --host-helpers
bash images/build.sh standard --host-helpers
bash script/test/tool-access.sh
```

The committed process goldens run in the module tests. For a compatibility
change, run them on Linux as an unprivileged uid and record exact commands,
statuses and any discrepancy under the ADR's evidence policy. The spike's
temporary differential experiment is historical evidence, not a contributor
prerequisite or a maintained test command.

## Bounded pnpm primitives

The default contracts gate replays the parser corpus with the retired JS oracle
at git commit `3b21103` (test-only, no npm/network). Fetch full history before
running it in a shallow clone (`git fetch --unshallow`); CI jobs running
`script/test/contracts.sh` must use `fetch-depth: 0`.

The graph, environment and dsh staging helpers are `recipe-contract`
subcommands ([Go build helpers](#go-build-helpers)); dsh is built on them
([dsh maintenance](#dsh-maintenance-and-graph-freeze)). Read
[ownership](docs/image-source-ownership.md#bounded-pnpm-primitives-42-pr-1) and the
[parser ADR](docs/adr/0001-build-only-pnpm-graph-parser.md). Go builds the helper;
no host YAML install or package-manager invocation is needed:

```sh
C="$(bash script/build/recipe-contract.sh --native)/out"
"$C/recipe-contract" pnpm-lock check dsh script/test/fixtures/pnpm-lock/tiny-dsh
"$C/recipe-contract" pnpm-lock graph bridge script/test/fixtures/pnpm-lock/real-bridge
node script/test/pnpm-lock-inputs.mjs
bash script/test/recipe-contract-vendor.sh
bash script/test/tool-access.sh
bash script/test/standard-node-consumers.sh --self-test
bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer primitives --platform linux/arm64
```

`pin KIND PREFIX` deliberately rewrites only three-input SHA-256 consistency
pins after full structural/policy validation; builds must never re-pin failures.
Fixtures retain independent expected graphs and a real pnpm 11.11.0 emitted v9
lock. Never use host pnpm 12 for any lock work. The vendored dependency policy
and drift gate are defined in [ADR 0001](docs/adr/0001-build-only-pnpm-graph-parser.md).
Rust inventory guards run without Node, while the dsh graph guards run
`recipe-contract pnpm-lock` as a syntax adapter. `contracts.sh` explicitly runs the
vendor, parser and harness self-tests without Docker. The vendor drift gate
requires the Go proxy and checksum database (and may download the selected Go
toolchain); it is not network-free.

`recipe-contract tool-access [--content | --tree] TARGET...` accepts explicit tree
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

`--consumer primitives` and `--consumer dsh` exist in this slice; pi/copilot
return usage2 (dsh accepts `--version EXACT` for the reviewed changed candidate).
The native audit phase runs both focused dsh legs and the policy matrix, records
every leg in `target/ci-logs/durations.txt` and fails if the measured phase
exceeds its predeclared budget (1.5× headroom under the 120-minute step
watchdog) instead of quietly consuming the watchdog.
A local base with matching pnpm bytes but no whole-source provenance provides
diagnostic smoke only. Both clean native CI rows must still build/audit the full
unchanged standard product at the proposed SHA, including current strict egress.
Do not attempt a full local build below the product capacity floor or prune
images to make room. The [failure ADR](docs/adr/0002-pnpm-terminal-failure-boundary.md)
records the proposed later bridge narrowing; no production receipt changes here.
