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

## Selective migration manifest

Source and destination paths are identical for these payloads (headers and
READMEs may be adapted to their new owner):

- `images/install-zellij.sh`
- `images/recipe-contract/run-install.sh`
- `images/recipe-contract/download.sh`
- `images/recipe-contract/run-npm.sh`
- `images/recipe-contract/run-report.sh`
- `images/recipe-contract/install-status.py`
- `images/recipe-contract/check-tool-access.py`
- `images/tools/dsh/install-dsh.sh`
- `images/tools/dsh/verify-dsh.sh`
- `images/tools/dsh/prepare-lock.sh`
- `images/tools/dsh/check-lock-update.js`
- `images/tools/dsh/package.json`
- `images/tools/dsh/package-lock.json`
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
| `script/test/ci-contracts.sh` `bash -n` + shellcheck of the wrapper and Go/Rust installers | here: `script/test/example-layers.sh`, default shellcheck severity |
| `script/check-rust-toolchain.sh` rust-dev digest-verification requirement | here: `script/test/example-layers.sh` |
| same script, rust-dev `ARG` values vs agent-vm pins | retired; policy below |

`script/test/contracts.sh` runs `example-layers.sh` in both modes and its
`--self-test` mutation controls in default mode.

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

agent-vm owns the canonical pins: the Rust channel in its `rust-toolchain.toml`
and the Verus release/digest in its `.github/workflows/verus.yml`.
`examples/layers/rust-dev/Dockerfile` mirrors them (`ARG RUST_TOOLCHAIN`,
`ARG VERUS_RELEASE`, `ARG VERUS_SHA256`).

Bump order: agent-vm lands its pin bump first; then a PR here moves the mirrored
`ARG`s (both Verus values together), citing the agent-vm commit.

Machine-checked:
- here: the three `ARG` lines match their anchor shapes, the Verus digest
  is 64 hex, and `install-verus.sh` has a non-comment `sha256sum` token (same
  token/ARG-shape guards for Go and golangci-lint). These static checks do not
  prove execution, digest consumption or fail-closed behavior. Execution evidence
  comes only from successful native builds; Verus requires native amd64, not
  exercised on the local arm64 host;
- in agent-vm: its own consumers (Cargo, workflows, macOS build) agree with its
  canonical pin.

Not machine-checked: equality of the rust-dev `ARG`s with agent-vm's pins. This
coverage is deliberately given up; a self-consistent stale Rust/Verus pair can go
undetected, including through the bundled Verus smoke test. Neither CI checks
out/reads the other's current tree in this design. A separate checkout/fetch is
possible; reading through the release-pinned submodule would block launcher
bumps on image releases. Use-time failures are possible, not guaranteed: rustup
normally selects the checkout's channel and may attempt installation, which may
fail with a missing toolchain or write-permission error; explicitly using older
cargo may hit `rust-version`; Verus may fail with newer launcher vstd. None is an
equality check. An inconsistent Verus release/digest should fail when the amd64
installer executes, but that is not local arm64 evidence.

This is a documented trade-off for optional user-built examples: independent
ownership and non-blocking launcher bumps, written bump order and build-arg
overrides, not guaranteed mismatch detection. A networked drift report is deferred.
This repository's contributor Rust toolchain (`contracts.yml`, test-only Cargo
package) is unrelated to this pin.

### Landing order

1. This repository adds `examples/layers/` and its checks (#23).
2. agent-vm removes its copy, retargets its checks/docs, and leaves a pointer
   here ([agent-vm follow-up](https://github.com/gregwebs/agent-vm/issues/293)). Until then agent-vm's copy is frozen migration
   material; do not edit both.
3. When agent-vm adopts a standard release built from a commit containing
   `examples/layers/`, its submodule-based Chrome agreement read activates.

Not covered by CI here yet: example image builds ([#33](https://github.com/gregwebs/agent-vm-images/issues/33))
and Chrome runtime checks ([#24](https://github.com/gregwebs/agent-vm-images/issues/24)).
