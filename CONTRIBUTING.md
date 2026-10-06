# Contributing

This repository owns image sources, not launcher releases. Read the
[ownership manifest](docs/image-source-ownership.md) and
[installer trust boundaries](images/tools/README.md). No agent-vm workspace
version bump, Rust application build, submodule or launcher packaging is needed.

## Conventions

- Bash scripts use `set -euo pipefail`; POSIX installers use `set -eu`.
  Host scripts must work with macOS Bash 3.2. Invoke scripts with `bash` or `sh`
  according to their shebang; Docker establishes runtime executable modes.
- Preserve errors through subprocesses; a required agent failure is never a
  successful degraded standard image. Retain low-level classified-failure
  behavior for auditing, not as a production bypass.
- Keep comments about invariants and reasons. Keep canonical documentation linked
  from README. Use multi-paragraph Why / How commits.
- Clean only invocation-owned scratch paths. Never prune tagged images or volumes
  to get tests passing; no pruning in build helpers or audit suites.
- Review exact pins, hash/lock diffs, vendor licenses/provenance and executable
  permissions. No `curl | shell` replacement for reviewed vendored installers.
  No automatic latest lookup during builds or CI. OS/apt inputs still float.

## Selection ownership and maintenance (follow-up)

`images/standard/Dockerfile` is the **one** owner of all eight version ARGs and
selection labels. Four installer defaults are exact nonempty ARGs. dsh/pnpm,
Pi and the bridge reuse committed manifest/lock bytes for empty/equal slots;
their literal LABEL fallbacks must mirror manifests. Labels are not health.
Unrelated labels/locks must not change when one owner is bumped.

The maintenance rewrite is not implemented in this scoped pass. Do **not** use
legacy agent-vm bump scripts on these sources: they target per-tool Dockerfiles
and have a known incomplete rollback path. The approved follow-up will provide:

```bash
# Planned interfaces; unavailable until the maintenance/audit pass lands.
bash script/build/agent-versions.sh --write
bash images/tools/dsh/upgrade-dsh.sh VERSION --pnpm PNPM_VERSION
bash images/tools/pi/upgrade-pi.sh VERSION
bash images/tools/pi/bridge/upgrade-bridge.sh VERSION
bash script/test/contracts.sh
```

That pass must validate all four queried installer versions before co-editing
one staged Dockerfile, preserve every affected destination including a failed
partial copy, and retain recovery backups on rollback failure. Explicit host
latest queries are developer-only, refused in CI; build mode accepts exact
versions only. Shared-file maintenance operations are sequential, not a service.
Review the central Dockerfile and relevant JSON/lock diff before building.

Vendored updates must preserve a reviewed upstream snapshot + patch, runnable
byte-equivalence checks, provenance and license review. Zellij changes require
independently reviewed archive SHA-256 pins for **both** architectures. Installer
changes also require strict restricted-egress audit and reviewed exact-selection
fixtures, never automatic allowlist broadening.

## Verification

Build users need Docker only. Contributor hermetic suites (pending migration)
will require Bash, git, jq, Node/npm, Python 3, GNU `timeout`, shellcheck, pinned
actionlint and the independent test-only Rust package/toolchain. On macOS:

```bash
brew install coreutils jq shellcheck actionlint
export PATH="$(brew --prefix coreutils)/libexec/gnubin:$PATH"
```

Fast checks available now: shell syntax by shebang, shellcheck on active scripts
(not upstream provenance snapshots), Python AST, `node --check` for the dsh freeze
checker, and `docker build --check` for both recipes. See README for base/standard
builds and the example for numeric-UID execution. Do not execute host installers
by discovering and running every `.sh`.

The next pass will supply hermetic contracts/source guards, gate mutations,
fake-Docker helper tests and independent finished-image acceptance interfaces:

```bash
# Planned audit interfaces; not yet shipped.
bash script/test/standard-image.sh agent-vm-base:local agent-vm-standard:local
bash script/test/pi-runtime.sh agent-vm-base:local agent-vm-standard:local
```

Before acceptance, require full base + all-six-agent standard + user example
builds on **native linux/amd64 and linux/arm64**, exact reports and all seven
installed records, numeric UID/GID matrix, Pi warning/bridge/provider behavior,
seed-hook idempotence and access negatives, plus strict installer-network audits
(including denied-latest control) on both architectures. No reduced recipe,
QEMU-only parity or skipped job substitutes for this evidence. Optional LSP
availability is reported separately. Container UID checks are not VM boot proof.

Inspect the context's `docker` driver and daemon storage before building. The
full standard requires at least 25 GiB free after base (35–40 preferred). Record
capacity failures, do not skip agents or claim a green full build. CI setup and
native-run fallback are follow-up work; no CI pass is claimed now.

### Evidence and failures

Report source/image-repo SHAs, native architecture, driver, daemon free bytes,
exact command/status, durations, image IDs/sizes, expected selections, tested
numeric IDs, and log paths. State each not-run check explicitly. Separate fast
checks, actual full installation, optional plugins, strict egress and runtime
observations. A build timeout/nonzero is failure, not a healthy image.

Future native CI must be PR-triggered and read-only, record capacity and separate
build/audit budgets, and upload logs even on failure. Rerun failed mandatory jobs
after fixing the cause. If a native runner is unavailable or lacks capacity,
attach equivalent full recorded native evidence at the proposed commit before
merge; do not defer acceptance to a postmerge manual dispatch. Publication is
[#264](https://github.com/gregwebs/agent-vm/issues/264), not this repository's
build helper or transactional local-file maintenance.
