# Image source ownership

This repository is the canonical maintenance owner of the base and standard
image sources, effective with this migration. Legacy copies in agent-vm are
frozen migration material until [#265](https://github.com/gregwebs/agent-vm/issues/265)
removes them and adds contributor-facing source integration. Do not synchronize
changes back or maintain two source owners.

## Provenance

Selective transfer from agent-vm commit
[`3ec78eebb7bdf743c93942a6ed2e5510de15c52d`](https://github.com/gregwebs/agent-vm/tree/3ec78eebb7bdf743c93942a6ed2e5510de15c52d).
Only committed files were read. Manifests, locks, runnable vendor installers,
upstream snapshots, patches, license/notice files and runtime payloads retain
the reviewed bytes and pins. Private npm metadata names containing “layer” are
historical names, not separate image products.

[Slice #263](https://github.com/gregwebs/agent-vm/issues/263) implements the source
boundary of [parent #257](https://github.com/gregwebs/agent-vm/issues/257).
[Publication #264](https://github.com/gregwebs/agent-vm/issues/264) and launcher /
submodule integration #265 are separate. No public base artifact, per-tool
artifact service, release scheme or universal boot contract is introduced here.
Container checks do not demonstrate microsandbox boot or authenticated sessions.

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
| `script/test/shipped-installer-network.sh` | same path; migrated/adapted strict restricted-egress audit with canonical mounts and standard defaults (interface implemented; live acceptance run pending) |
| `script/test/{host-watchdog.sh,host-watchdog.py}` | same paths |
| `script/test/fixtures/t5-negative/Dockerfile` | same path; migrated/adapted disposable numeric-owner/group denial fixture for the finished-image audit (interface implemented; acceptance run pending) |
| `script/test/fixtures/installer-egress/{addon.py,selections.tsv}` | same paths, exact-selection restricted-egress audit |
| `script/test/ci-contracts.sh` | image-only `script/test/contracts.sh` aggregate |
| `crates/agent-vm/tests/image_sources.rs` | independent test-only package, not a launcher workspace copy |
| `.github/workflows/{build-image,shipped-tool-recipes,pi-layer}.yml` | historical references only; replacement contracts/build-local/installer-network CI |
| all other launcher source/docs/workflows | excluded |

This migration includes maintenance (§5), retained/new audits (§6) and native
non-publishing CI (§7). The plan's retained-suite list says 18 but enumerates 17
paths; all 17 are retained. Source-only integrity tests run in the independent
`script/test/image-sources` package, never the launcher workspace. New tests and
CI do not imply actual native acceptance passed: see CONTRIBUTING's evidence
requirements and the proposed PR run logs. Full standard builds and strict
egress on both native architectures remain mandatory before merge.
