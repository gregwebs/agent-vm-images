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
persist: locks/manifests, Pi wrapper/extensions, seed hooks, and the OpenCode adapter/configuration/pnpm inputs. All PATH changes
are additive (`ENV PATH=<new>:${PATH}`). The base provides `/opt/agent` mode 0755,
Node 22, base-owned pnpm, fetch/diagnostic facilities, explicit optional trust and an empty
`/opt/agent-vm/seed.d`; it ships no legacy `agent-vm-install` helper.

## Exact selections

| Label suffix | Docker ARG | Committed selection / spelling |
|---|---|---|
| codex | AGENT_VERSION_CODEX | rust-v0.159.3 / rust-vSEMVER |
| opencode | AGENT_VERSION_OPENCODE | v2.0.29 / v2.SEMVER |
| claude | AGENT_VERSION_CLAUDE | 2.1.286 / SEMVER |
| copilot | AGENT_VERSION_COPILOT | 1.0.90 / SEMVER |
| dsh | AGENT_VERSION_DSH | manifest 0.1.5-rc.2 / SEMVER |
| pnpm (inherited base) | AGENT_VERSION_PNPM (compatibility only) | base pin.json 11.11.0 / SEMVER |
| pi | AGENT_VERSION_PI | manifest 1.3.0 / SEMVER |
| opencode-claude | AGENT_VERSION_OPENCODE_CLAUDE | plugin manifest 1.4.1 / SEMVER |
| pi-claude-bridge | AGENT_VERSION_PI_CLAUDE_BRIDGE | bridge manifest 0.8.0 / SEMVER |

Single-slot defaults are exact nonempty ARGs; empty values, dist-tags, ranges,
URLs and malformed versions fail before network access. Locked slots have empty
ARGs intentionally. Empty/equal slots reuse committed manifest/lock/configuration bytes during preparation without
registry resolution; literal LABEL fallbacks mirror manifests because Docker
cannot execute jq in LABEL. These nine `org.agent-vm.version.*` labels record
**selection, not health**. There is no build-time latest query.

An explicit different exact locked selection prepares a build-local lock; it
never edits committed inputs. dsh accepts only a changed root specifier: retained
archive and peer-context snapshot records stay frozen, while records inside the
changed root's own closure may be replaced and nothing outside it may appear (see
[ADR 0003](../../docs/adr/0003-dsh-graph-freeze.md)). An incompatible pair fails
with the offending records named rather than silently refreshing the empty slot.
Pi/bridge regenerate a
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
remove its own partial artifacts and write `absent-transport CODE`. dsh never
softens a partial pnpm tree and Copilot never softens a partial npm tree.

**Maintained base and standard builds disable soft failure explicitly.** No
soft-fail ARG, skip flag or successful partial product exists. Each standard
block requires its bounded exact report, access audit and `installed` record.
Pi records `pi` and `pi-claude-bridge`; dsh records both dsh/pnpm health in `dsh`.
That is eight agent records for nine selections, including `opencode-claude`.
Bridge or plugin absence is not success.

The mandatory build-only final gate checks strict `installed\n` records before
and after re-running all seven agent verifiers (excluding separate pnpm verification calls). It compares the first lexically existing
PATH candidate for dsh, Codex, OpenCode, Claude and Pi to their fixed-path verified
files, before and after verification. dsh's target is the installed
`node_modules/@deepseek-ai/dsh/lib/bin.js` the image links directly, so a
generated `.bin` wrapper is a different file. Wrong executable shadows, dangling/cyclic
links and unusable candidates fail rather than falling through. Symlinks to the
same verified file pass. pnpm and Copilot already report through bare PATH names. Base pnpm verification
also checks immutable launcher/alias identity before and after the agent gates.
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
[Codex](codex/vendor/README.md), [OpenCode](opencode/vendor/README.md) with
[runtime notes](opencode/README.md), [Claude](claude/vendor/README.md). Exact
upstream assets still arrive over the network. Vendor manifests detect
corruption, not independent signing or upstream replacement of an exact-version
asset. OpenCode has no independent archive
checksum; its trust is TLS plus exact registry tarball URL. These are explicit trust limits, not
an assertion of fully reproducible OS or supply chain.

### dsh and pnpm

dsh is an image-owned pnpm project: `package.json`, `pnpm-lock.yaml`,
`pnpm-workspace.yaml` and `pnpm-inputs.sha256`. Base alone owns pnpm 11.11.0; dsh
plugin management finds that launcher through ordinary PATH. The dsh root has no
pnpm dependency or private pnpm executable. Its isolated layout uses private
hoist for sandbox resolution, four reviewed exact Cordis overrides and only the
`chokidar@4.0.3` trust exception. Installation is frozen, production-only,
scripts disabled, with a disposable external store; `node_modules/.pnpm` is
runtime payload.

The lock is functional, not only reproducibility. A floating dsh install can
pull a newer `dsh-base` and nest `dsh-sandbox-local` under its node_modules,
where the app's plugin loader cannot resolve it. The reviewed lock preserves the
private-hoist sandbox placement and every transitive integrity hash; the
graph only proves reachability, so the installed-tree check and the independent
layout oracle prove the physical layout separately. Base pin and retained
manifest must agree; standard cannot independently override pnpm. The stock
distribution is unmodified; see the canonical
[Node package defaults](../../docs/node-package-defaults.md).

A successful version report is not health. Certification also requires effective
exact peers, sandbox resolution, PTY and libc FFI, shipped protobuf/genai
runtime, actual `dsh-subprocess-local` execution and bounded credential-free HTTP
web readiness with complete process-group cleanup. Missing prebuilds fail;
scripts are never enabled as fallback. Default/equal inputs are byte-frozen;
a changed dsh selection permits root-closure transitive replacement while
preserving retained records. See the
[graph-freeze ADR](../../docs/adr/0003-dsh-graph-freeze.md) and
[migration evidence](../../docs/standard-pnpm-migration.md).

Installation disables lifecycle scripts, so no reviewed script ever runs; the
inspection evidence in the ledger shows they would only fetch prebuilds, restore
mode bits or write metadata. The dsh gate checks nonempty
exact output: `import.meta.main` is undefined on Node older than 22.19, where dsh
can exit 0 printing nothing. Exit status alone would certify a broken CLI. No community OAuth/subscription plugins or new host credential readers are
baked in, with one named exception: **`@openchamber/opencode-claude` only**.
It delegates to the installed Claude CLI via the Agent SDK without reading,
copying, storing or replaying Claude credentials. Credential-replay plugins
remain prohibited. This is not a general exemption for CLI-delegating plugins;
any further exception requires a maintainer decision. See the
[OpenCode runtime and maintenance contract](opencode/README.md#image-owned-claude-plugin).

### Pi integrity

Pi 1.0.4+ publishes no `npm-shrinkwrap.json`, so the committed, fully hashed
lock governs `npm ci` and npm authenticates every tarball it installs. The
committed lock has no nested shrinkwrap-only entries; the build reports that
explicitly instead of claiming five verified siblings.

A shrinkwrapped selection (Pi 1.0.3 and older) behaves differently: its
published shrinkwrap omits hashes for exactly five direct siblings (`chord`,
`pi-agent-core`, `pi-ai`, `pi-telemetry`, `pi-tui`). Reviewed lock entries
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
entries must carry integrity, the nested sibling set must be empty or exactly
those five at the pin, and sibling pin/layout changes require review.

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

### agent-vm-codex provider

The bare, dependency-free `agent-vm-codex.js` is agent-vm-owned, not an npm
package. The wrapper loads it with an existence-checked `--extension`; removing
it costs the provider, not Pi. It registers `agent-vm-codex` only for the exact
agent-vm access and refresh placeholders in `/agent-vm-state/codex/auth.json`.
The placeholder constants are a cross-repo contract: renaming them requires a
coordinated image release.

```text
host Codex login → guest placeholders → agent-vm-codex (SSE) → host proxy
```

The distinct provider id separates guest credentials; the built-in
`openai-codex-responses` api id preserves encrypted reasoning and tool identities
on replay. SSE is mandatory because only that transport exposes Pi's fetch seam.
Each stream can request one host-side refresh; failure returns a fixed diagnostic
asking for `codex login` on the host, never the refresh response's text.
The build gate seeds and removes a fixture and proves registration. Registration
and mock streams are not evidence of an authenticated session. See
[ADR-0044](https://github.com/gregwebs/agent-vm/blob/7438927/docs/adr/0044-pi-reaches-host-codex-through-an-image-owned-provider.md).

## Maintenance and runtime ownership

[CONTRIBUTING](../../CONTRIBUTING.md#selection-ownership-and-maintenance)
is canonical for maintenance commands, audit interfaces, prerequisites and CI
evidence. Bump scripts stage owning manifests/locks and the one standard Dockerfile
transactionally. Rollback status 3 preserves/report recovery material: stop and
recover before further edits. Historical transfer locks and pins were carried
unchanged; dsh's npm lock was then regenerated into the reviewed four-input pnpm
project in #51 (see the [ledger](../../docs/standard-pnpm-migration.md)), so the
old npm bytes remain only in git history. There are no standalone tool Dockerfiles. Run the linked
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

### OpenCode pnpm installation

The private plugin project is installed through the base pnpm 11.11.0 launcher,
frozen, production-only, without optional platform binaries or lifecycle scripts.
Its separate build-only runner treats every nonzero, timeout, signal, policy,
integrity or capture failure as hard, regardless of inherited soft-fail settings.
It does not share npm's classified transport protocol. Only behavioral
verification writes `opencode-claude`'s `installed\n` record.

The base's five safety defaults remain intact; ambient manager/registry/policy
bypasses and project hooks are rejected. Empty/equal preparation makes no registry
request; a frozen install still requests age/provenance metadata. The disposable
external store and invocation HOME/XDG state are removed in the same RUN.
`node_modules/.pnpm` is installed runtime content and must remain. Physical and
logical symlink paths are audited under all four numeric UID/GID pairs after
external-store removal. The separate fourth egress profile accounts for frozen
metadata verification without broadening native installer rules. That profile
admits registry metadata, provenance bundles and the three locked tarballs, and
only those: every pnpm invocation in this layer suppresses the update notifier,
so a build never spends a round trip on a version notice, and the profile says
exactly what the install needs. Denying the provenance bundles would instead mean
weakening the trust policy the base mandates.
