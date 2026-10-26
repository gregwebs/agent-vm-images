# Image source ownership

This repository is the canonical maintenance owner of the base and standard
image sources, effective with this migration. Legacy base and standard copies
in agent-vm were frozen migration material until
[#265](https://github.com/gregwebs/agent-vm/issues/265) removed them and added
contributor-facing source integration. Do not synchronize
changes back or maintain two source owners. Example image definitions under
`examples/layers/` are owned here too; the copy agent-vm still holds is frozen
until [agent-vm #293](https://github.com/gregwebs/agent-vm/issues/293) deletes it
(see [Example images](#example-images-exampleslayers)).

## Provenance

Selective transfer from agent-vm commit
[`3ec78eebb7bdf743c93942a6ed2e5510de15c52d`](https://github.com/gregwebs/agent-vm/tree/3ec78eebb7bdf743c93942a6ed2e5510de15c52d).
Only committed files were read. Manifests, locks, runnable vendor installers,
upstream snapshots, patches, license/notice files and runtime payloads retain
the reviewed bytes and pins. Private npm metadata names containing “layer” are
historical names, not separate image products.

[Slice #263](https://github.com/gregwebs/agent-vm/issues/263) implements the source
boundary of [parent #257](https://github.com/gregwebs/agent-vm/issues/257) and its
own native source/audit/strict-egress gates passed (see the
[#263 close-out](https://github.com/gregwebs/agent-vm/issues/263#issuecomment-6018677984);
that evidence belongs to #263, not to a publication run).
[Publication #264](https://github.com/gregwebs/agent-vm/issues/264) adds a
maintained, image-owned standard release contract (one
`ghcr.io/gregwebs/agent-vm-standard` product, independent image versions,
digest-addressed OCI index plus archive/SBOM assets), described in
[standard image releases](standard-image-releases.md). The base stays local-only;
there is still no per-tool artifact service or universal boot contract. The
launcher/submodule integration remains #265 and is not claimed here, and
container/fixture checks do not demonstrate microsandbox boot or authenticated
sessions.

## Parent pins (#95)

`images/parent-pins.json` owns the external parents of every maintained recipe:
the Debian base, the build-only Go helper compiler and the released standard
image, each with its digest, per-platform provenance and reviewed verification
method. `images/apt-policy/` records the reviewed Debian repository/key baseline
extracted from the pinned parent; `images/apt-policy/check-sources.sh` is the
build-time check of that baseline, and `images/parent-pins.json`
`apt_open_gaps` is a closed, empty record: [#96](https://github.com/gregwebs/agent-vm-images/issues/96)
removed both remaining third-party instructions, so no maintained recipe may add
one.
`images/install-build-ca.sh` owns the optional host build CA: it verifies the
reviewed `BUILD_CA_SHA256`, installs the certificate, and records the hash in
the image and in a label the public release refuses. The only writer
of the pins and the baseline is
`script/dev/refresh-parent-pins.sh` (with `script/dev/parent_pins.py`), which
refuses CI execution; builds and the isolated release entrypoints only read them.
`images/Dockerfile:20` and the four `examples/layers/*` `ARG BASE_IMAGE`
declarations must name the pinned references literally. Policy, exceptions and
statuses: [image input policy](image-input-policy.md).

The *local* edge has its own owners: `images/base-inputs.sha256` is the committed
manifest of the images/ sources the base is built from;
`script/build/base-inputs.sh` is its only writer (it also rewrites the
`AGENT_VM_BASE_IDENTITY` default in `examples/base-extension/Dockerfile`);
`images/verify-base-inputs.sh` is the in-build check that stamps the base and
compares a consumer's inherited stamp; and
`examples/base-extension/check-base-identity.sh` is the extension's check. A
parent refresh republishes the manifest and the default, since a moved local
parent changes a listed input. `images/parent-pins.json` is metadata the base
does not consume and is deliberately not listed. Decision and limits:
[ADR 0005](adr/0005-repository-base-identity.md).

## Third-party release artifacts (#96)

`images/artifacts/<name>.json` owns the reviewed selection of Node and the GitHub
CLI: the exact upstream archive per architecture with its SHA-256, the member
prefix, the declared payload and the authentication evidence (a committed Node
release key, or a Sigstore bundle per architecture). `images/install-artifact.sh`
(with `images/artifacts/install_artifact.py` and `artifact_pin.py`) is the only
installer and the only reader of a pin in an image;
`script/dev/pin-artifact.py` is the only writer, and it also owns the
`org.agent-vm.version.<name>` labels in `images/Dockerfile`. `artifacts/` and
`install-artifact.sh` are base inputs, so a pin refresh is followed by
`bash script/build/base-inputs.sh --write`. Decision, limits and evidence:
[ADR 0006](adr/0006-third-party-artifact-pins.md) and the
[image input policy](image-input-policy.md#pinned-third-party-release-artifacts).

## Current pnpm ownership

Base owns `images/pnpm/`, authenticated bootstrap and the pnpm selection label.
dsh now owns its four pnpm inputs (`package.json`, `pnpm-lock.yaml`,
`pnpm-workspace.yaml`, `pnpm-inputs.sha256`) and its build-only preparation,
comparison, override-derivation, install and runtime-check helpers; it inherits
the base launcher without a private pnpm package. Pi, bridge and Copilot retain
their current npm paths pending their #42 slices. Historical transfer entries
below are historical, not current install instructions. See
[runtime defaults](node-package-defaults.md),
[ADR 0003](adr/0003-dsh-graph-freeze.md) and the
[migration ledger](standard-pnpm-migration.md).

## Selective migration manifest (historical)

Source and destination paths are identical for these payloads (headers and
READMEs may be adapted to their new owner):

- `images/install-zellij.sh`
- `images/recipe-contract/run-install.sh`
- `images/recipe-contract/download.sh`
- `images/recipe-contract/run-npm.sh`
- `images/recipe-contract/run-report.sh`
- `images/recipe-contract/` — Go record/access predicates; see [ADR 0004](adr/0004-go-build-helpers.md)
- `images/tools/dsh/install-dsh.sh`
- `images/tools/dsh/verify-dsh.sh`
- `images/tools/dsh/prepare-lock.sh`
- `images/tools/dsh/check-lock-update.js` — historical: deleted in #75 with the
  other Node adapters; the comparator is `recipe-contract dsh-lock-update`
- `images/tools/dsh/package.json`
- `images/tools/dsh/package-lock.json` — historical: deleted in #51 when dsh
  moved to the four pnpm inputs; the reviewed bytes remain in git history at
  `6c51509e949ea643b82515f5c8a53acd122861c0`
- `images/tools/pi/install-pi.sh`
- `images/tools/pi/install-pi-packages.sh`
- `images/tools/pi/verify-pi.sh`
- `images/tools/pi/prepare-lock.sh`
- `images/tools/pi/pi.sh`
- `images/tools/pi/seed-claude-bridge-config.sh`
- `images/tools/pi/README.md`
- `images/tools/pi/package.json`
- `images/tools/pi/package-lock.json`
- `images/tools/pi/extensions/guest-credential-warning.js`
- `images/tools/pi/bridge/package.json`
- `images/tools/pi/bridge/package-lock.json`
- `images/tools/pi/bridge/prepare-lock.sh`
- `images/tools/codex/install-codex.sh`
- `images/tools/codex/verify-codex.sh`
- `images/tools/opencode/install-opencode.sh`
- `images/tools/opencode/verify-opencode.sh`
- `images/tools/claude/install-claude.sh`
- `images/tools/claude/verify-claude.sh`
- `images/tools/claude/seed-claude-plugins.sh`
- `images/tools/copilot/install-copilot.sh`
- `images/tools/copilot/verify-copilot.sh`
- `images/tools/copilot/README.md`
- `images/tools/codex/vendor/LICENSE`
- `images/tools/codex/vendor/NOTICE`
- `images/tools/codex/vendor/README.md`
- `images/tools/codex/vendor/install.patch`
- `images/tools/codex/vendor/install.sh`
- `images/tools/codex/vendor/install.upstream.sh`
- `images/tools/opencode/vendor/LICENSE`
- `images/tools/opencode/vendor/NOTICE`
- `images/tools/opencode/vendor/README.md`
- `images/tools/opencode/vendor/install.patch`
- `images/tools/opencode/vendor/install.sh`
- `images/tools/opencode/vendor/install.upstream.sh`
- `images/tools/claude/vendor/README.md`
- `images/tools/claude/vendor/install.patch`
- `images/tools/claude/vendor/install.sh`
- `images/tools/claude/vendor/install.upstream.sh`

Other mapping decisions:

| Source | Destination / disposition |
|---|---|
| `images/Dockerfile` | same path; tool-free facilities retained, obsolete helper/soft policy removed |
| `images/build.sh` | same path; rewritten as ordinary two-recipe convenience |
| `images/tools/{dsh,pi,codex,opencode,claude,copilot}/Dockerfile` | executable instructions consolidated in `images/standard/Dockerfile` |
| `images/tools/*/contract/*` | not copied; six canonical `images/recipe-contract` helpers bind-mounted directly |
| `images/tools/README.md` | rewritten for standard installer ownership |
| `images/min-agent-vm-version` | not migrated; obsolete launcher publication gate |
| `script/build/sync-recipe-contracts.sh`, `script/test/sync-recipe-contracts.sh` | retired; no copies to sync |
| `script/build/macos.sh` | excluded; launcher packaging |
| `script/build/{agent-versions,dockerfile-label,npm-pin,transactional-publish}.sh` | same paths; shared-file rewrite and inclusive rollback repair |
| `images/tools/{dsh,pi}/upgrade-*.sh`, `images/tools/pi/bridge/upgrade-bridge.sh` | same paths; staged central-Dockerfile edits and recovery retention |
| `script/test/{agent-versions,claude-installer,codex-installer,copilot-installer,copilot-verify,opencode-installer,dsh-prepare-lock,dsh-verify,pi-install,pi-prepare-lock,pi-verify,pi-wrapper,shipped-installer-contracts,tool-access,vendored-installers,upgrade-scripts,install-zellij}.sh` | same paths; canonical-helper and central-Dockerfile adaptations |
| `script/test/shipped-tool-recipes.sh` | `script/test/standard-image.sh`, finished-image numeric-UID/report oracle, not recipe builds |
| `script/test/pi-layer-runtime.sh` | `script/test/pi-runtime.sh`, accepts BASE_IMAGE STANDARD_IMAGE |
| `script/test/shipped-installer-network.sh` | same path; migrated/adapted strict restricted-egress audit with canonical mounts and standard defaults (interface and native gating evidence from #263) |
| `script/test/{host-watchdog.sh,host-watchdog.py}` | same paths |
| `script/test/fixtures/t5-negative/Dockerfile` | same path; migrated/adapted disposable numeric-owner/group denial fixture for the finished-image audit (interface and native gating evidence from #263) |
| `script/test/fixtures/installer-egress/{addon.py,selections.tsv}` | same paths, exact-selection restricted-egress audit |
| `script/test/ci-contracts.sh` | image-only `script/test/contracts.sh` aggregate |
| `crates/agent-vm/tests/image_sources.rs` | independent test-only package, not a launcher workspace copy |
| `.github/workflows/{build-image,shipped-tool-recipes,pi-layer}.yml` | historical references only; replacement contracts/build-local/installer-network CI |
| all other launcher source/docs/workflows | excluded |

This migration includes maintenance (§5), retained/new audits (§6) and native
non-publishing CI (§7). The plan's retained-suite list says 18 but enumerates 17
paths; all 17 are retained. Source-only integrity tests run in the independent
`script/test/image-sources` package, never the launcher workspace. #263's own
native source/audit/strict-egress gates passed at its close-out; a publication
run (#264) does not re-claim that evidence, and new tests/CI do not imply that
native acceptance passed. Full standard builds and strict egress on both native
architectures remain mandatory before merge, and released-standard consumption
(anonymous digest/archive plus both native `msb` boots) is a separate
publication gate.

## Example images (`examples/layers/`)

This repository owns the four example image definitions under
[`examples/layers/`](../examples/layers/README.md) — `chrome-devtools`, `go-dev`,
`rust-dev`, `wirenboard-cpp` — as of [#23](https://github.com/gregwebs/agent-vm-images/issues/23).
They are user-owned boot images built `FROM` the released, digest-pinned standard
index, not launcher layers; the retired layer-composition contract (agent-vm
ADR-0003) is not revived. agent-vm keeps launcher responsibilities: capability
detection and its `defaults.rs` path constants, image selection/import and generic
boot-image/runtime tests. Installed launchers read no image sources.

### Provenance and path mapping

Copied from agent-vm commit
[`d5d887677295b1468d6097c4b933850a88c1bc13`](https://github.com/gregwebs/agent-vm/tree/d5d887677295b1468d6097c4b933850a88c1bc13/examples/layers)
with `git archive` (committed bytes and modes only). Paths are unchanged:
`agent-vm/examples/layers/<p>` → `agent-vm-images/examples/layers/<p>`. Earlier
history lives in agent-vm. Bare `ADR-NNNN` and `#NNN` references in these files
name agent-vm records.

| Files | Disposition |
|---|---|
| `chrome-devtools/{Dockerfile,agent-vm-chrome-mcp}`, `go-dev/{install-go,install-golangci-lint,install-gopls,verify-toolchain}.sh`, `rust-dev/{install-verus,verify-toolchain}.sh` | byte-identical |
| `{go-dev,rust-dev,wirenboard-cpp}/Dockerfile`, `rust-dev/install-rust.sh` | comments only: launcher file references qualified as agent-vm's; the rust-dev pin comment states the policy below |
| `README.md` | launcher links made absolute; build-from-checkout section; check ownership and pin policy point here |

| Check (agent-vm, before) | Owner after |
|---|---|
| `script/test/chrome-example-contract.sh` Dockerfile/wrapper content | here: `script/test/example-layers.sh` (literal paths) |
| same script, marker/wrapper paths vs `crates/agent-vm/src/defaults.rs` | agent-vm (see below) |
| `script/test/chrome-layer-runtime.sh` runtime checks | here: `script/test/chrome-example.sh` / `chrome-example.yml`; API-1 composition case retired (no layer-composition contract) |
| `script/test/ci-contracts.sh` `bash -n` + shellcheck of the wrapper and Go/Rust installers | here: `script/test/example-layers.sh`, default shellcheck severity |
| `script/check-rust-toolchain.sh` rust-dev digest-verification requirement | here: `script/test/example-layers.sh` |
| same script, rust-dev `ARG` values vs agent-vm pins | retired; policy below |

`script/test/contracts.sh` runs `example-layers.sh` in both modes and its
`--self-test` mutation controls in default mode.

### Chrome MCP pin

[#41](https://github.com/gregwebs/agent-vm-images/issues/41) makes the example's
`chrome-devtools-mcp` selection a maintained payload rather than a build-time
fetch. Its owned files are:

- `examples/layers/chrome-devtools/chrome-devtools-mcp/`: the four pinned inputs
  `package.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml` and
  `pnpm-inputs.sha256` that the image installs from.
- `examples/layers/chrome-devtools/chrome-devtools-mcp.sh`: the build helper
  (`check`, `install`, `verify`, `resolve`), bind-mounted at build time and never
  stored in the image.
- `examples/layers/chrome-devtools/chrome-devtools-mcp.sigstore.json`: the npm
  SLSA provenance bundle kept as review evidence; not copied into the image.
- `script/build/chrome-mcp-provenance.sh`: repo-only trusted identity and
  verification helpers, sourced by the updater and by
  `script/test/chrome-example.sh`.
- `script/dev/upgrade-chrome-devtools-mcp.sh`: repo-only maintainer-host writer
  of the five files above.

The updater is the only writer of the committed inputs; the helper's `resolve`
writes a lock into a maintenance directory, and a build only verifies the
committed bytes. Maintenance is host-only (see
[CONTRIBUTING](../CONTRIBUTING.md#chrome-example-mcp-maintenance)); no build
step resolves or refreshes a selection. Chrome capability paths are unchanged:
the example still writes `/etc/agent-vm-capabilities/chrome-devtools-mcp` and
installs `/usr/local/bin/agent-vm-chrome-mcp`.

### Chrome capability paths

The example writes `/etc/agent-vm-capabilities/chrome-devtools-mcp` as its last
build step and installs `/usr/local/bin/agent-vm-chrome-mcp`; agent-vm's
`CHROME_MCP_CAPABILITY_PATH` / `CHROME_MCP_WRAPPER_PATH` must name the same paths.
Each side checks its own literals. agent-vm additionally reads this example
through its release-pinned `vendor/agent-vm-images` submodule once that pin
includes `examples/layers/`; until then agreement is by these mirrored literals,
not a cross-repository read. An image-only literal/recipe path change can escape
launcher detection until the release pin advances; that later read checks release
source, not a published example image. Changing either path is a coordinated
change in both repositories. The standard image must not supply Chrome, the wrapper or the
marker. `example-layers.sh` scans only the current base/standard recipe text for
direct forbidden tokens; it does not inspect the released parent filesystem or
helper-installed content. Harmless comments containing those tokens also fail.

### Rust and Verus pins

agent-vm owns the canonical selections: the Rust channel in its
`rust-toolchain.toml` and the Verus release/digest in its
`.github/workflows/verus.yml`. `examples/layers/rust-dev/Dockerfile` mirrors them
(`ARG RUST_TOOLCHAIN`, `ARG VERUS_RELEASE`, `ARG VERUS_SHA256`).

Bump order: agent-vm lands its pin bump first; then a PR here moves the mirrored
selection (both Verus values together), citing the agent-vm commit.

#### Rust: reviewed closure, not a channel name (#102)

`examples/layers/rust-dev/rust-toolchain/` is the reviewed input set for the
Rust layer and the example owns it, like the Chrome MCP pin:

- exact `rustup-init` per architecture with its original upstream `.sha256`
  sidecar bytes as evidence;
- the original signed `channel-rust-<version>.toml`, its `.sha256` and detached
  `.asc`, and Rust's release key (primary fingerprint
  `108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE`);
- SHA-256 and detached OpenPGP signature for each selected archive: `rustc`,
  `cargo`, `rust-std`, `clippy-preview`, `rustfmt-preview` for the GNU host and
  `rust-std` for the matching musl target, for both `linux/amd64` and
  `linux/arm64`.

`rust_toolchain.py` is the only program that verifies and installs this; an
ordinary build never resolves a channel, rewrites the signed manifest, refreshes
a hash or falls back to another origin. Missing or changed bytes fail the build.
The `rustup-init` binaries carry no detached signature for 1.28.2, so `pin.json`
records a **scoped, dated, per-architecture hash-only exception** with exact
URLs/digests, upstream `.sha256` and installation-document evidence, reviewer and
a reassessment condition; it is not a claim that the bootstrap publisher is
authenticated. Signed archives are verified, not excused. Decision and rejected
alternatives: [ADR 0007](adr/0007-rust-example-toolchain-pins.md).

Only `script/dev/upgrade-rust-toolchain.sh` (with
`script/dev/rust-toolchain-update.py`) writes these files, on a maintainer host
(it refuses `GITHUB_ACTIONS=true`). It authenticates the entire
two-architecture closure without executing the foreign bootstrap and publishes
the pins, evidence and the Dockerfile `ARG RUST_TOOLCHAIN` assertion as one
transaction:

```bash
bash script/dev/upgrade-rust-toolchain.sh 1.98.1 --rustup 1.28.2 \
  --key /absolute/reviewed/rust-key.gpg.ascii \
  --review-bootstrap-exception "$(date -u +%F)" --reviewer gregwebs
bash script/dev/upgrade-rust-toolchain.sh 1.98.1   # expected: unchanged
bash script/dev/upgrade-rust-toolchain.sh --check  # offline
```

A changed bootstrap identity prints `hash-only bootstrap exception requires
renewed review` and publishes nothing; the review date and reviewer come from a
human. Never hand-edit a pin or renew the date to clear a failure. Status `3`
means an undecided rollback: recover the retained directory before further
commits.

First use is bounded: the image exports `RUSTUP_AUTO_INSTALL=0` and persists
`auto-install disable` and `auto-self-update disable`, so an ordinary `cargo`,
`rustc`, `cargo clippy` or `cargo fmt` invocation that needs an absent toolchain,
tool or target fails locally instead of downloading. The selected channel, its
five host components and the musl standard library work with no Rust
distribution access. A project `rust-toolchain.toml` that only declares extra
components or targets may still build; a command that needs them fails.
`RUSTUP_TOOLCHAIN` and `CARGO_HOME` are deliberately not exported, so project
selection keeps working and cargo keeps its per-user `$HOME/.cargo` cache. An
explicit guest `rustup install/add/update` is outside the guarantee, as are
project dependencies.

Machine-checked here: the committed closure (file set, digests, checksum lines,
key identity, manifest signature, manifest-derived selection, ARG equality), the
exact installer/verifier RUN shapes and mounts, `--network=none` on the verifier,
the absence of a runtime `CARGO_HOME`/`RUSTUP_TOOLCHAIN`, banned live-bootstrap
or mutable-channel tokens, and the Verus smoke shape — by
`script/test/example-layers.sh` (81 cases). Hermetic behaviour:
`python3 script/test/rust-toolchain-pins.py` and
`python3 script/test/rust-toolchain-maintenance.py`. Native evidence:
`bash script/test/rust-example.sh --platform linux/arm64|linux/amd64` on a
native daemon. Equality with agent-vm's current selections is still not
machine-checked (see below).

### Landing order

1. This repository adds `examples/layers/` and its checks (#23).
2. agent-vm removes its copy, retargets its checks/docs, and leaves a pointer
   here ([agent-vm follow-up](https://github.com/gregwebs/agent-vm/issues/293)). Until then agent-vm's copy is frozen migration
   material; do not edit both.
3. When agent-vm adopts a standard release built from a commit containing
   `examples/layers/`, its submodule-based Chrome agreement read activates.

Not covered by CI here yet: other example image builds
([#33](https://github.com/gregwebs/agent-vm-images/issues/33)).

## Maintained OpenCode 2 additions

These are new maintained sources, not changes to the historical transfer above.
The plugin is a new pnpm consumer; existing Pi/bridge/Copilot migration stays
with #42 (dsh moved in #51, below). Ownership covers:

- Runtime payloads: `images/tools/opencode/opencode.json`,
  `images/tools/opencode/plugins/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,index.js}`.
- OpenCode build/maintenance helpers:
  `images/tools/opencode/install-opencode-plugins.sh`, `check-claude-runtime.mjs`,
  `probe-opencode.py`, `check-claude-payload.mjs`, `verify-opencode-plugins.sh`,
  `plugins/{check-lock.mjs,pin-pnpm-inputs.mjs,prepare-lock.sh,upgrade-plugins.sh}` and extended `README.md`.
- Shared build-only helpers: `images/recipe-contract/run-pnpm.sh` and the composed
  `recipe-contract` helper (its `pnpm-environment` subcommand is the environment guard).
- Pinned install inputs: `images/tools/opencode/plugins/pnpm-inputs.sha256` records the reviewed bytes of
  `package.json`, `pnpm-lock.yaml` and `pnpm-workspace.yaml`. The build verifies those bytes rather than
  interpreting YAML; OpenCode does not import the shared build-only graph parser. `check-lock.mjs`
  verifies the pins and still asserts the manifest policy structurally, and `pin-pnpm-inputs.mjs`
  (developer-only, never copied into an image) is the only writer. The manifest is pinned so the
  reported selection cannot disagree with the pinned lockfile.
- Tests: `script/test/{opencode-pnpm-prepare-lock.sh,opencode-pnpm-install.sh,opencode-pnpm-upgrade.sh,opencode-plugins-verify.sh,opencode-plugins-verify.py,opencode-probe.py,opencode-egress.py,pnpm-inputs.mjs,run-pnpm.sh}`.
- Independent audit/runtime/release helpers:
  `script/test/{opencode-audit.py,opencode-audit-tests.py,opencode-access-files.py,opencode-input-audit.py,opencode-guest-audit.sh,opencode-runtime.sh,opencode-runtime-mutations.py,opencode-pnpm-policy.py,render-released-agent-probe.py}`.
- Fixtures: `script/test/fixtures/{opencode-server.py,opencode-query.js,opencode-compose.sh}` and
  `script/test/fixtures/installer-egress/opencode-plugins.py`.

The existing standard recipe, final gate, certification/wire/version/upgrade
suites, Rust source guards, egress addon/selections, release selection plumbing,
rendered guest probe and explicitly enumerated contracts/native CI remain their
existing owners. No executable maintenance helper ships in the image.


## dsh pnpm migration (#51, PR 2 of 4 for #42)

dsh is now an image-owned pnpm project. Its owning sources are:

- Reviewed inputs: `images/tools/dsh/{package.json,pnpm-lock.yaml,pnpm-workspace.yaml,pnpm-inputs.sha256}`.
- Preparation, installation and verification: `images/tools/dsh/{prepare-lock.sh,install-dsh.sh,verify-dsh.sh}`.
- Build-only helpers: `images/tools/dsh/{project.py,host-manager.py,check-runtime.mjs,probe-dsh.py}`.
  `project.py` owns filesystem authorization and handled-failure four-file
  publication and is not a lock parser; it authorizes the composed contract
  directory **before** anything in it is executed (the environment guard included)
  and accepts exactly these shapes: the production build mount
  `/tmp/recipe-contract` with a `/opt/agent-vm/dsh` prefix, a directory inside an
  authorized fixture root, the checkout's own tracked `images/recipe-contract`, or
  the composed directory that checkout's `script/build/recipe-contract.sh --native`
  published (`target/recipe-contract/OS-ARCH/out`), with prefixes below that
  checkout or inside the invocation's scratch root (`AGENT_VM_SCRATCH_ROOT`, the
  developer-upgrade spelling of the same capability, which never claims a fixture
  root it does not have). The graph reader, comparator, derivation and staging
  helpers are `recipe-contract` subcommands (`pnpm-lock`, `dsh-lock-update`,
  `dsh-cordis-overrides`, `dsh-upgrade-stage`) reaching
  `images/recipe-contract/pnpmlock`; `host-manager.py` represents the authenticated
  stock manager as argv for host maintenance (`project.py manager`). None of these
  ship in the image.
- Maintenance: `images/tools/dsh/upgrade-dsh.sh` (transactional four-input and
  central-Dockerfile publication, Cordis proposal/approval).
- Tests: `script/test/{dsh-prepare-lock.sh,dsh-verify.sh,dsh-project.py,dsh-upgrade.py,upgrade-scripts.sh,standard-node-layout.py,standard-node-policy.sh,standard-node-policy.py,standard-node-policy-self-test.py,standard-node-consumers.sh,standard-node-consumers.py,standard-node-dsh.py}`,
  fixtures `script/test/fixtures/{dsh/*,standard-node-dsh.py,standard-node-policy.py}`
  and derivation/update lock fixtures under `script/test/fixtures/pnpm-lock/`.
- Rules and evidence: [ADR 0003](adr/0003-dsh-graph-freeze.md) and the
  [migration ledger](standard-pnpm-migration.md).

Two implementations answer different questions and must not be merged: the
production graph validator (`images/recipe-contract/pnpmlock`, CLI
`recipe-contract pnpm-lock`) parses the reviewed lock and enforces policy, identity
and per-context peers; the
independent Python filesystem oracle (`script/test/standard-node-layout.py`)
reads only a virtual extraction of a finished image and resolves the installed
files, links and manifests with no production parser import. Neither replaces
the runtime probes.

## Bounded pnpm primitives (#42, PR 1)

The `recipe-contract` helper (`pnpmlock`, `pnpmenv`, `dshupgrade`,
`internal/document`) and its committed Go vendor closure
(`vendor/go.yaml.in/yaml/v3`) are build-only data validators, not runtime payloads.
They replaced the Node `pnpm-lock.mjs` and its npm `vendor/yaml/` distribution in
#75. The [parser ADR](adr/0001-build-only-pnpm-graph-parser.md) owns the
trust/update boundary, the pin and the compatibility ledger. OpenCode retains its independent byte-only checker.
PR 1 (historical) changed no consumer, production lock, failure producer or
standard recipe. dsh has since adopted the parser in #51 (above); Pi, bridge and
Copilot still use their current npm paths until their own slices.

The public check/graph/pin CLI, hostile fixtures, explicit vendor inventory tests,
generic access `--tree` and primitive-only native harness are prerequisite
interfaces, not consumer acceptance. The exact future workspace configs must be
regenerated and cold-installed in their owning slices. Rust owns source/path
boundaries and runs this CLI as a syntax adapter for the migrated dsh project;
Node owns graph, vendor digest and mutation checks.
See [contributor commands](../CONTRIBUTING.md#bounded-pnpm-primitives).

The [terminal-failure ADR](adr/0002-pnpm-terminal-failure-boundary.md) records why
pnpm remains hard-only and the proposed bridge-only loss of degraded transport
classification. Maintainer acknowledgement on #42 is required before PR 3 removes
that producer. No receipt clearing or adapter retirement occurs in PR 1.
