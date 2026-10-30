# Standard-image installers

These directories own installer inputs, exact locks, reviewed vendor snapshots
and explicit runtime payloads. They are **not** standalone image products.
[`../standard/Dockerfile`](../standard/Dockerfile) is the one maintained
all-agent recipe, built with `images/` as context. It installs in order
**dsh → Pi → Codex → OpenCode → Claude → Copilot → agy**. Larger, rarely bumped
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
| codex | AGENT_VERSION_CODEX | rust-v0.159.3 / rust-vPINSELECTED (see [Codex payload closure](codex/vendor/README.md)) |
| opencode | AGENT_VERSION_OPENCODE | v2.0.29 / v2.SEMVER |
| claude | AGENT_VERSION_CLAUDE | 2.1.286 / SEMVER |
| copilot | AGENT_VERSION_COPILOT | 1.0.90 / SEMVER |
| agy | AGENT_VERSION_AGY | 1.4.11 / X.Y.Z (pin-selected, see [ADR 0012](../../docs/adr/0012-agy-hash-only-artifact-pin.md)) |
| dsh | AGENT_VERSION_DSH | manifest 0.1.5-rc.2 / SEMVER |
| pnpm (inherited base) | AGENT_VERSION_PNPM (compatibility only) | base pin.json 11.11.0 / SEMVER |
| pi | AGENT_VERSION_PI | manifest 1.4.0 / SEMVER |
| opencode-claude | AGENT_VERSION_OPENCODE_CLAUDE | plugin manifest 1.4.1 / SEMVER |
| pi-claude-bridge | AGENT_VERSION_PI_CLAUDE_BRIDGE | bridge manifest 0.8.0 / SEMVER |

Single-slot defaults are exact nonempty ARGs; empty values, dist-tags, ranges,
URLs and malformed versions fail before network access. Locked slots have empty
ARGs intentionally. Empty/equal slots reuse committed manifest/lock/configuration bytes during preparation without
registry resolution; literal LABEL fallbacks mirror manifests because Docker
cannot execute jq in LABEL. These ten `org.agent-vm.version.*` labels record
**selection, not health**. There is no build-time latest query.

An explicit different exact locked selection prepares a build-local lock; it
never edits committed inputs. dsh accepts only a changed root specifier: retained
archive and peer-context snapshot records stay frozen, while records inside the
changed root's own closure may be replaced and nothing outside it may appear (see
[ADR 0003](../../docs/adr/0003-dsh-graph-freeze.md)). An incompatible pair fails
with the offending records named rather than silently refreshing the empty slot.
Pi and bridge are
reviewed pnpm projects too: a changed Pi is prepared from its **authenticated
selected archive** (shrinkwrap conversion when that archive carries one, an
ordinary exact resolution when it does not), and the graph is compared before
anything is pinned; a changed bridge starts from a fresh manifest and workspace.
New transitives may be registry-dependent; a reviewed committed bump is the
reproducible default. Pi and bridge are separate projects, so changing one does
not rewrite the other. See `images/tools/pi/README.md` and
[ADR 0011](../../docs/adr/0011-pi-selected-shrinkwrap.md).

## Failure and certification

Recipe-contract sources live in `images/recipe-contract/`, with no maintained
copies or sync process. [ADR 0004](../../docs/adr/0004-go-build-helpers.md)
owns their build-only delivery and record/access compatibility contract.

`run-install.sh NAME RESULT_FILE INTERPRETER SCRIPT [ARG ...]` resets the record
to `pending` before invocation. An inherited `installed` cannot mask failure.
Exit 75 plus a matching fresh private receipt is reserved for the
download contract's own low-level curl codes: the receipt and status grammar
accept `transport download CODE` only. The retired npm codes are ordinary
invalid records now that `run-npm.sh` is deleted. Unknown errors, HTTP failures,
checksum/sha512 mismatch, `EINTEGRITY`, native exit 75, timeout and every pnpm
failure are hard. Native subprocesses do not receive the receipt. Pi's own
independent archive download keeps the receipt-proved curl soft branch (soft 1
or 0); dsh never softens a partial pnpm tree, the bridge has no absence producer
at all, and Copilot remains an npm install until PR 4 of #42.

**Maintained base and standard builds disable soft failure explicitly.** No
soft-fail ARG, skip flag or successful partial product exists. Each standard
block requires its bounded exact report, access audit and `installed` record.
Pi records `pi` and `pi-claude-bridge`; dsh records both dsh/pnpm health in `dsh`.
That is nine agent records for ten selections, including `opencode-claude`.
Bridge or plugin absence is not success.

The mandatory build-only final gate checks strict `installed\n` records before
and after re-running all eight agent verifiers (excluding separate pnpm verification calls). It compares the first lexically existing
PATH candidate for dsh, Codex, OpenCode, Claude, Pi and agy to their fixed-path verified
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
checksum; its trust is TLS plus exact registry tarball URL. Two tools are the
exception. Codex's two payloads are bound by a committed content pin, so an exact
version that the pin does not select is refused — see the
[Codex payload policy](../../docs/image-input-policy.md#codex-standalone-payloads)
and [ADR 0009](../../docs/adr/0009-codex-payload-pins.md). agy publishes nothing
to verify against, so its pin is hash-only, corroborated at pin time by GitHub's
asset digest and Google's updater manifest and re-checked from the committed
evidence at build time — the weakest guarantee in this repository, recorded in
[ADR 0012](../../docs/adr/0012-agy-hash-only-artifact-pin.md) and in the
[agy hash-only artifact](../../docs/image-input-policy.md#the-agy-hash-only-artifact)
section of the input policy. These are explicit trust
limits, not an assertion of fully reproducible OS or supply chain.

### dsh and pnpm

dsh is an image-owned pnpm project: `package.json`, `pnpm-lock.yaml`,
`pnpm-workspace.yaml` and `pnpm-inputs.sha256`. Base alone owns pnpm 11.11.0; dsh
plugin management finds that launcher through ordinary PATH. The dsh root has no
pnpm dependency or private pnpm executable. Its isolated layout uses private
hoist for sandbox resolution, four reviewed exact Cordis overrides and only the
`chokidar@4.0.3` trust exception. Installation is frozen, production-only,
scripts disabled, with a disposable external store; `node_modules/.pnpm` is
runtime payload.

The filesystem/manager/publication boundary is not dsh-specific: it lives in
`images/tools/pnpm-project.py` (with `images/tools/pnpm-host-manager.py` for host
acquisition) because Pi and its bridge share it. `images/tools/dsh/project.py`
and `dsh/host-manager.py` are adapters that load the shared module and supply the
dsh consumer, so dsh's scripts, imports and mounts are unchanged; the dsh install
and verify RUNs add one read-only mount of the shared file at `/tmp/pnpm-project.py`.

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

Pi and its bridge are image-owned pnpm projects: `package.json`,
`pnpm-lock.yaml`, `pnpm-workspace.yaml` and `pnpm-inputs.sha256` (digests of
exactly the first three) per prefix, with **no** npm project lock. Base pnpm
11.11.0 installs both frozen, production-only, scripts disabled, from the npmjs
registry with a private HOME/XDG/store and copy-import. Neither project has its
own manager or a lifecycle-script approval.

Two routes, chosen by the **authenticated selected archive's contents** — never
by a version number and never as a fallback:

* the archive carries `npm-shrinkwrap.json` (e.g. 0.87.0): its root identity,
  SRI and every descendant are synthesized into a staged npm-project-shaped lock,
  imported once with bounded pnpm, compared against the pre-import expected
  graph, and only then pinned and validated frozen. The five legacy siblings
  whose shrinkwrap records omit `integrity` are completed from their own exact
  registry metadata **after** each sibling archive's SRI is verified. The
  planning diagnostic measured 143 identities, 144/144 byte-identical SRI and
  three representation normalisations, which the operator accepted on 2026-10-28
  as reviewed normalisations for that selection and which are never an allowlist
  (see [ADR 0011](../../docs/adr/0011-pi-selected-shrinkwrap.md));
* the archive carries no shrinkwrap (the 1.4.0 default): no fabricated
  shrinkwrap, no "refill"; the exact selection is resolved under the reviewed
  workspace and the complete identity/SRI/edge/optional/peer delta is reviewed.

The family shape is a reviewed per-selection expectation, not a constant: 1.4.0
reaches six members (`pi-agent-core`, `pi-ai`, `pi-codemode`, `pi-mcp`,
`pi-telemetry`, `pi-tui`; five direct root edges) and 0.87.0 the historical five
(`chord`, `pi-agent-core`, `pi-ai`, `pi-telemetry`, `pi-tui`; four direct edges).
1.4.0's published `chord` is unreachable and has no `esbuild` dependency.

Bytes are a separate proof. `install-pi.sh` downloads **all** reachable sibling
archives independently, checks each SRI before extraction, and runs the mounted
build-only `check-installed-bytes.mjs` against the pristine extraction. Only
exactly-derived package-root `node_modules/.bin` directories/links are
normalised; every other file, link, kind, byte and executable bit must match,
including `dist`, `package.json` and any `npm-shrinkwrap.json` inside a sibling.
The image's explicit `a+rX` step supplies read access; authenticated, locked
declared bin targets are expected 0755 after pnpm's own fix-bin. The finished
image is then audited independently by
`script/test/standard-node-layout.py --consumer pi`.

The bridge keeps `nodeLinker: hoisted`, keeps the SDK **JavaScript** and omits
only the SDK's per-platform native packages through the reviewed
`ignoredOptionalDependencies` pattern; the old `--omit=optional
--legacy-peer-deps` rationale is history (see
[ADR 0011](../../docs/adr/0011-pi-selected-shrinkwrap.md)).

The build-only modules under `images/tools/pi/` are wired into both install and
verify RUNs (never COPYed into runtime) and remain reviewable separately:

* `import-shrinkwrap.mjs` — authenticated archive data -> staged npm-shaped lock
  plus the expectation-vs-import delta. No download, npm, YAML or duplicate-key
  parsing (`recipe-contract pnpm-json` owns JSON syntax), no guessed hashes.
* `archive.py` — whole-archive preflight and bounded extraction, plus the
  per-invocation rehash of a retained provider tarball. Shell still owns the
  download and its head-of-line SRI check.
* `installed-graph.mjs` / `check-installed-bytes.mjs` — lock-derived physical
  graph and archive-versus-installed comparison; expectations never come from
  the observed tree.
* `check-runtime.mjs` — genai, protobufjs and the legacy chord-context esbuild
  transform from the real installed resolution context.
* `prepare-project.py` — the one owning process for a changed/refreshed
  selection.

### Pi wrapper and bridge

The wrapper at `/usr/local/bin/pi` enforces `PI_SKIP_VERSION_CHECK`, passes
positional subcommands through, always loads the guest credential warning and
loads the pinned bridge unless explicitly opted out. It injects no default
trust or telemetry policy. Image-owned extensions remain outside HOME, where
runtime state mounts cannot hide them. See [Pi notes](pi/README.md).

The separate `/opt/agent-vm/pi-packages` project is a hoisted pnpm install with
`autoInstallPeers: false`. Its peer declarations on the Pi family and `typebox`
stay unmet on purpose: no second Pi or `typebox` instance may exist, and peer
declaration text is data, not a violation. `ignoredOptionalDependencies:
['@anthropic-ai/claude-agent-sdk-*']` omits only the SDK platform packages, each
carrying a second native Claude CLI; standard already installs Claude at
`/opt/agent/.local/bin/claude`, and Pi's jiti supplies the SDK JavaScript. The
bridge has no shrinkwrap hash exceptions: every entry has integrity. Its
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
