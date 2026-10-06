# Standard-image installers

These directories own installer inputs, exact locks, reviewed vendor snapshots
and explicit runtime payloads. They are **not** standalone image products.
[`../standard/Dockerfile`](../standard/Dockerfile) is the one maintained
all-agent recipe, built with `images/` as context. It installs in order
**dsh → Pi → Codex → OpenCode → Claude → Copilot**. Larger, rarely bumped
locked trees remain below frequently changed CLI installs for Docker cache
locality; there are no intermediate tool tags, predecessor identities or
launcher catalog inputs.

Canonical build/install audit helpers live in `images/recipe-contract/`.
Installers, verifiers and helpers are read-only bind mounts, not COPYed executable
interfaces. No copies or sync script are needed. Only explicit runtime COPYs
persist: locks/manifests, Pi wrapper/extensions, and seed hooks. All PATH changes
are additive (`ENV PATH=<new>:${PATH}`). The base provides `/opt/agent` mode 0755,
Node 22, fetch/diagnostic facilities, explicit optional trust and an empty
`/opt/agent-vm/seed.d`; it ships no legacy `agent-vm-install` helper.

## Exact selections

| Label suffix | Docker ARG | Committed selection / spelling |
|---|---|---|
| codex | AGENT_VERSION_CODEX | rust-v0.159.3 / rust-vSEMVER |
| opencode | AGENT_VERSION_OPENCODE | v1.18.34 / vSEMVER |
| claude | AGENT_VERSION_CLAUDE | 2.1.286 / SEMVER |
| copilot | AGENT_VERSION_COPILOT | 1.0.90 / SEMVER |
| dsh | AGENT_VERSION_DSH | manifest 0.1.5-rc.2 / SEMVER |
| pnpm | AGENT_VERSION_PNPM | dsh manifest 11.11.0 / SEMVER |
| pi | AGENT_VERSION_PI | manifest 0.87.1 / SEMVER |
| pi-claude-bridge | AGENT_VERSION_PI_CLAUDE_BRIDGE | bridge manifest 0.8.0 / SEMVER |

Single-slot defaults are exact nonempty ARGs; empty values, dist-tags, ranges,
URLs and malformed versions fail before network access. Locked slots have empty
ARGs intentionally. Empty/equal slots reuse committed JSON/lock bytes without
registry resolution; literal LABEL fallbacks mirror manifests because Docker
cannot execute jq in LABEL. These eight `org.agent-vm.version.*` labels record
**selection, not health**. There is no build-time latest query.

An explicit different exact locked selection prepares a build-local lock; it
never edits committed inputs. dsh rewrites only selected dependencies and the
freeze checker requires recursive equality at every location outside changed
roots, including shared/hoisted records. An incompatible pair fails with moved
paths rather than silently refreshing the empty slot. Pi/bridge regenerate a
changed project from its exact manifest and revalidate integrity, sibling and
loader-alias invariants. New override transitives may be registry-dependent;
a reviewed committed bump is the reproducible default. Pi and bridge are
separate projects, so changing one does not rewrite the other.

## Failure and certification

`run-install.sh NAME RESULT_FILE INTERPRETER SCRIPT [ARG ...]` resets the record
to `pending` before invocation. An inherited `installed` cannot mask failure.
Exit 75 plus a matching fresh private transport receipt is reserved for known
curl transport codes or npm error codes. Unknown errors, HTTP failures,
checksum/sha512 mismatch, `EINTEGRITY`, native exit 75 and timeout are hard.
Native subprocesses do not receive the receipt. Low-level isolated helpers
retain classified development failure behavior for audit fidelity; an owning
hook can only soften positively classified transport when explicitly enabled,
remove its own partial artifacts and write `absent-transport CODE`. dsh and
Copilot never soften partial npm trees.

**Maintained base and standard builds disable soft failure explicitly.** No
soft-fail ARG, skip flag or successful partial product exists. Each standard
block requires its bounded exact report, access audit and `installed` record.
Pi records `pi` and `pi-claude-bridge`; dsh records both dsh/pnpm health in `dsh`.
That is seven records for eight selections. Bridge absence is not success.

The mandatory build-only final gate checks strict `installed\n` records before
and after re-running all six verifiers. It compares the first lexically existing
PATH candidate for Codex, OpenCode, Claude and Pi to their fixed-path verified
files, before and after verification. Wrong executable shadows, dangling/cyclic
links and unusable candidates fail rather than falling through. Symlinks to the
same verified file pass. dsh/pnpm/Copilot already report through bare PATH names.
Access auditing includes command/interpreter/symlink ancestry and arbitrary-UID
usability; readable mode bits alone are not version proof. Future customization
after this gate is the image author's responsibility.

Claude is verified immediately after required installation, before optional
marketplace work. Marketplace add is bounded to 180s, each of four LSP installs
to 120s, list to 60s, each with 10s kill escalation; optional command budget is
at most 13 minutes. Every nonzero logs operation/status explicitly. The stash
under `/opt/agent-vm/claude-seed` remains readable outside HOME; optional download
failure can leave no stash and the seed hook safely no-ops. Required final
certification remains hard regardless of optional plugin availability.

## Trust limits and reviewed locks

Vendored snapshot + patch + runnable copy keep installer edits reviewable:
[Codex](codex/vendor/README.md), [OpenCode](opencode/vendor/README.md),
[Claude](claude/vendor/README.md). Exact upstream assets still arrive over the
network. Vendor manifests detect corruption, not independent signing or upstream
replacement of an exact-version asset. OpenCode has no independent archive
checksum; its trust is TLS plus exact tag. These are explicit trust limits, not
an assertion of fully reproducible OS or supply chain.

### dsh and pnpm

The lock is functional, not only reproducibility. A floating dsh install can
pull a newer `dsh-base` and nest `dsh-sandbox-local` under its node_modules,
where the app's plugin loader cannot resolve it. The reviewed lock preserves
app-root sandbox placement and every transitive integrity hash. pnpm is in the
same lock because `dsh plugin ... add ...` forwards to it. Both binaries are
linked into `/usr/local/bin`; neither exists in the base.

`npm ci --ignore-scripts` avoids upstream lifecycle execution. The reviewed
lifecycle scripts only fetch prebuilds, restore mode bits or write metadata.
The dsh gate checks nonempty exact output: `import.meta.main` is undefined on
Node older than 22.19, where dsh can exit 0 printing nothing. Exit status alone
would certify a broken CLI. No community OAuth/subscription plugins or new host
credential readers are baked in.

### Pi integrity

Pi's published shrinkwrap omits hashes for exactly five direct siblings:
`chord`, `pi-agent-core`, `pi-ai`, `pi-telemetry`, `pi-tui`. Reviewed lock entries
refill their registry `dist.integrity`. npm ignores our lock for that subtree,
so installation re-fetches each sibling tarball, compares its sha512 to the
committed integrity and compares extraction against the full installed tree.
There is no recursive basename exclusion such as `diff -x node_modules` that
could hide a shadow package inside a sibling's own dist directory.

Only the sibling's **lock-declared nested dependency directories** are folded
from the installed tree; npm authenticated their shrinkwrap integrities. Every
other path inside those five sibling trees must match reviewed archive bytes:
extra, changed, missing files/directories/symlinks fail hard. A package beside
one of those five is outside those compared trees; this is not an exhaustive
independent authentication of the entire npm install. All other non-root lock
entries must carry integrity, and sibling pin/layout changes require review.

### Pi wrapper and bridge

The wrapper at `/usr/local/bin/pi` enforces `PI_SKIP_VERSION_CHECK`, passes
positional subcommands through, always loads the guest credential warning and
loads the pinned bridge unless explicitly opted out. It injects no default
trust or telemetry policy. Image-owned extensions remain outside HOME, where
runtime state mounts cannot hide them. See [Pi notes](pi/README.md).

The separate `/opt/agent-vm/pi-packages` project installs with
`--ignore-scripts --omit=optional --legacy-peer-deps`. Legacy peers are mandatory
for lock generation and ci: Pi's loader aliases its own peers, and normal npm
peer solving would add a second version-skewed Pi. Optional omission drops the
Claude SDK platform packages, each carrying a second native Claude CLI; standard
already installs Claude at `/opt/agent/.local/bin/claude`.

The bridge has no shrinkwrap hash exceptions: every entry has integrity. Its
seed hook points the provider to the image's Claude. The wrapper loads it with
an explicit extension, not `pi install` or HOME auto-discovery. Registration
checks require actual positive provider/model counts; metadata existence alone
is not a working bridge or evidence of an authenticated turn.

## Maintenance and runtime ownership

[CONTRIBUTING](../../CONTRIBUTING.md#selection-ownership-and-maintenance)
is canonical for maintenance commands, audit interfaces, prerequisites and CI
evidence. Bump scripts stage owning manifests/locks and the one standard Dockerfile
transactionally. Rollback status 3 preserves/report recovery material: stop and
recover before further edits. Locks and pins were transferred unchanged, not
regenerated. There are no standalone tool Dockerfiles. Run the linked
[fast contracts](../../script/test/contracts.sh),
[finished-image audit](../../script/test/standard-image.sh),
[Pi runtime matrix](../../script/test/pi-runtime.sh) and
[strict installer egress audit](../../script/test/shipped-installer-network.sh)
at the appropriate verification tier.

Seed hooks are shipped integrations, not a universal boot contract. Docker CMD
does not execute them automatically; a caller prepares user-owned state and
invokes them explicitly. HOME persistence/credential provisioning belongs to
the downstream runtime. For historical rationale see the pinned upstream
[Pi wrapper ADR](https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0012-stable-pi-image-customization-seam.md),
[bridge ADR](https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0023-image-owned-pi-extension-packages.md)
and [dsh ADR](https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0022-dsh-tool-layer.md).
Those links explain runtime choices, not per-tool artifact contracts here.
