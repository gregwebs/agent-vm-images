# 0011: Pi's selected archive decides the graph, and the bytes are proven separately

Status: accepted for the Pi and Pi-bridge migration (issue #52, PR 3 of 4 for
#42). Companion records: `0001-build-only-pnpm-graph-parser.md` (only Go parses
YAML), `0002-pnpm-terminal-failure-boundary.md` (every pnpm failure stays hard),
`0003-dsh-graph-freeze.md` (opaque peer contexts, no graph freeze by wishful
comparison), `0004-go-build-helpers.md` (no second YAML parser), `0005`–`0010`
(base identity, artifact pins, layer CI).

## Context

Pi and its Claude bridge were npm projects installed with `npm ci` plus flags
(`--omit=optional --legacy-peer-deps` for the bridge). Base owns pnpm exactly
11.11.0, and every other image-owned project already installs from four reviewed
inputs (`package.json`, `pnpm-lock.yaml`, `pnpm-workspace.yaml`,
`pnpm-inputs.sha256`) with scripts disabled. Pi and the bridge are the last
migrated consumers before Copilot (PR 4 of #42).

Three facts make Pi different from dsh and from a plain `pnpm import`:

1. A published Pi archive may carry an upstream **npm-shrinkwrap.json**. The
   current default, 1.4.0, does not; the historical 0.87.0 tarball does
   (`lockfileVersion: 3`, matching root identity, exactly five sibling records
   with no `integrity` field). Which route applies must be decided by the
   authenticated archive's *contents*, never by a version threshold.
2. pnpm does **not** consume a dependency's shrinkwrap. `pnpm import` reads a
   project-level `package-lock.json` and re-resolves; it does not transplant SRI,
   occurrences or edges. Treating "the tarball has a shrinkwrap" as "pnpm will
   install that graph" would silently change the selection.
3. A successful lock conversion proves neither installed bytes nor runtime
   health. The published family members are ordinary archives whose generated
   `node_modules/.bin` entries differ per version.

## Decision

### Routes are chosen by authenticated archive content

A changed or refreshed Pi selection is prepared by
`images/tools/pi/prepare-project.py` (through the stable
`prepare-lock.sh PREFIX VERSION [--refresh-lock]` shell interface) using exactly
one of two routes, decided after the selected archive is downloaded and its
SHA512 checked against its `dist.integrity`:

* **shrinkwrap present** — synthesize a staged npm-project-shaped lock from the
  authenticated archive (root archive record plus every shrinkwrap descendant
  prefixed under `node_modules/@earendil-works/pi-coding-agent/`), run one
  bounded `pnpm import`, compare the resulting graph against the pre-import
  expected graph, then validate frozen under the full policy;
* **no shrinkwrap** — resolve the selected exact version as an ordinary
  all-hashed graph under the same policy, and compare the complete
  identity/SRI/edge/optional/peer delta.

There is **no fallback** between the routes and no "resolve by range if the
import is inconvenient" branch. An archive that carries a shrinkwrap but cannot
preserve its graph under the reviewed pnpm is a stop for review, not a licence
to resolve freely. The only alternative would be an explicit maintainer decision
to drop shrinkwrap fidelity *and* report a complete shrinkwrap-vs-pnpm delta on
every run; that alternative is not selected.

Authenticity here means registry TLS plus tarball SRI checked before extraction.
It is **not** independent signer attestation, and no document may call it that.

### The missing-SRI completion is exact and authenticated

Both Pi shrinkwraps historically omit the `integrity` field of the same five
sibling records (`chord`, `pi-agent-core`, `pi-ai`, `pi-telemetry`, `pi-tui`) at
the selected version. Those five — and only those five, at the exact selected
version — are completed from their own exact registry metadata, and each
sibling archive is downloaded and SRI-verified *before* its integrity string is
admitted. Every integrity already present in the shrinkwrap is preserved
byte-for-byte; a discrepancy, a six/four-shaped set, a non-family identity or a
duplicate version/context is a review-required error. This replaces the old
"npm view refill" narrative: the completion is bounded pnpm/archive-
authenticated input, not hash invention.

### Fidelity comparator, before pinning

`validateShrinkwrapDelta` (JS, data only) and the Go candidate graph comparison
run before any pin is written. Permitted deltas are limited to reviewed
representation normalisations; added/dropped resolved identities, a changed SRI
or a changed effective edge target hard-fail. The 0.87.0 diagnostic import
(planning Phase 0) measured 143 identities and 144/144 byte-identical SRI with
three normalisation deltas:

1. two `agent-base@9.0.0` npm locations merge into one equivalent pnpm snapshot;
2. one explicit optional peer edge `openai@6.40.0 -> ws@8.21.0` appears, with the
   same effective npm peer target;
3. one optional peer *declaration* completion `debug@4.4.3 supports-color:*`,
   already optional metadata, remains absent.

The operator accepted those three as reviewed import normalisations on
2026-10-28 (implementation-diary approval 6), so a run may now present them as
accepted for the 0.87.0 selection. They are not an allowlist: a per-selection
comparison still runs, they are reported separately rather than allowed
silently, and a new or missing delta stops.

### The two accepted selection deltas of the default import

The default-selection conversion (Pi 1.4.0, bridge 0.8.0) was compared against
the reviewed npm baseline (`bc1b2df`) identity by identity, SRI by SRI and edge
by edge, and produced exactly two deltas. The operator accepted both on
2026-10-29; nothing else was approved, so any further delta still stops.

1. **Pi: one edge target inside a range.** `@aws-sdk/client-bedrock-runtime@3.1127.0`
   requires `@smithy/node-http-handler: ^4.11.3`; npm hoisted `4.12.1` while pnpm
   selects `4.13.0`. Both archives were already in the reviewed npm graph, so
   neither identity count (120/120) nor the 121/121 byte-identical SRIs change:
   the difference is which already-reviewed archive satisfies that one edge.
2. **Bridge: `ajv` optional→mandatory and three peer declarations became concrete
   edges.** `zod` and `ajv` are installed either way; their peer declarations in
   the bridge root now resolve to concrete snapshot edges, and `ajv` moves from
   an optional peer to a mandatory edge. That is strictly stricter than npm, not
   weaker. The 96 identities are exactly the npm non-optional identities; the 8
   omitted `@anthropic-ai/claude-agent-sdk-*` platform packages are the reviewed
   `ignoredOptionalDependencies` design, and 98/98 mapped SRIs are equal.

### Family shape is a reviewed per-version expectation

The reachable Pi family is not a constant:

| selected Pi | reachable family members | direct root edges | package-root generated bins |
|---|---|---|---|
| 1.4.0 | `pi-agent-core`, `pi-ai`, `pi-codemode`, `pi-mcp`, `pi-telemetry`, `pi-tui` (modern six) | five (telemetry is transitive) | core→`pi-ai`; ai→`anthropic-ai-sdk`; tui→`marked`; codemode/mcp/telemetry→none |
| 0.87.0 | `chord`, `pi-agent-core`, `pi-ai`, `pi-telemetry`, `pi-tui` (historical five) | four (telemetry is transitive) | chord→`esbuild`; core→`pi-ai`,`yaml`; ai→`anthropic-ai-sdk`; tui→`marked`; telemetry→none |

1.4.0's `chord` is published but **not reachable** and has no `esbuild`
dependency, so the old "five siblings with chord/esbuild" table is historical,
not the default. The table is an independent expectation: the installed tree
cannot define its own shape, and a four/six/seven/mismatched/unreachable family
is a hard failure. `pi`'s generated `.bin/pi` at the project level is the
reviewed seam; `/usr/local/bin/pi` remains the static warning wrapper (a regular
file, never a link to the package entry), and the positional subcommand dispatch
list is data generated per selected version (`images/tools/pi/dispatch/*.txt`),
because 0.87.0 advertises no `mcp` subcommand.

### Bin mode is derived, not observed

pnpm's own `linkBin`/`fix-bin` makes an authenticated, locked **declared bin
target** executable (0755) even when the archive mode is 0644 — pi-ai 1.4.0's
`dist/cli.js` is exactly this case. The byte proof therefore applies an exact
derived rule: only authenticated, locked declared bin targets are 0755 after
installation, every other archive entry keeps its own executable-bit
expectation, and the image's explicit `a+rX` step supplies read access. This is
a narrow interpretation recorded for review, not "modes are ignored": a
`chmod 0644`, `chmod 0700` or unrelated-executable mutant is killed by running
the checker directly, with no repair/normalisation pass in between.

### Bytes are proven from authenticated archives, never from the observed tree

`install-pi.sh` downloads each reachable sibling archive independently, checks
its SRI, extracts it safely, and hands the pristine extraction to the mounted
build-only `check-installed-bytes.mjs`. That checker consumes the validated Go
graph for identities, contexts and `hasBin`, and the authenticated manifests for
bin names/targets; it never derives an expectation from the installed tree. A
consistent SDK manifest+link tamper, a wrong-context same-version substitution,
an extra or missing file, a shadow `node_modules`, an archive-owned generated
path, an escaping dependency link or a retargeted `.bin` entry all fail. The only
permitted normalisations are the exactly-derived package-root
`node_modules/.bin` directories/links; there is no blanket `node_modules`
exclusion.

`script/test/standard-node-layout.py --consumer pi|bridge` is the independent
oracle for the finished image: it reads a virtual root, authors its own family,
bins, wrapper-shape and omission vectors, accepts pre-run authenticated archive
inventories (`--archive-inventories`), and never imports production JS.

### Bridge: hoisted, SDK JavaScript kept, platform payload omitted

The bridge keeps `nodeLinker: hoisted` with `autoInstallPeers: false` and
`strictPeerDependencies: false`. Its peer **declarations** on the Pi family and
`typebox` stay unmet on purpose; no second Pi or typebox instance may be
installed, and peer declaration text is data, not a violation. The exact
`ignoredOptionalDependencies: ['@anthropic-ai/claude-agent-sdk-*']` omits only
the SDK's per-platform native packages: the SDK JavaScript must exist and load
through Pi's jiti. `--no-optional` is not used (pnpm 11.11.0 then reports
`ERR_PNPM_LOCKFILE_MISSING_DEPENDENCY`), and no alias, NODE_PATH or source patch
replaces a resolver. Omitted descriptors may remain in the lock; omitted
payloads may not, and a native SDK payload anywhere in the bridge tree is a hard
layout failure.

## Implementation status (2026-10-28, focused harness evidence)

The **changed-selection route above is wired and proven** by the focused harness's
real `--version 0.87.0` leg (linux/arm64, diagnostic, 1 m 31 s). The route is
chosen by the authenticated archive's own content, never by a version number:

* `prepare-project.py prepare` queries the exact selected packument with one
  bounded `pnpm view` under the owning policy defaults, downloads the tarball
  through `download.sh`, checks its SHA-512 against the registry's
  `dist.integrity` **before** extraction, and extracts it with `archive.py`'s
  whole-archive preflight. A transport failure here is hard: preparation has no
  degraded route.
* If the extracted archive carries `npm-shrinkwrap.json`, the driver completes
  exactly the five legacy same-version sibling SRIs from exact metadata *and a
  verified archive each* (a four, six or non-legacy missing set is refused before
  any metadata call), reads both documents through `recipe-contract pnpm-json`,
  synthesizes the npm-shaped project lock with `import-shrinkwrap.mjs`, keeps it
  as read-only evidence, removes it before candidate validation, and runs
  `pnpm import` under the reviewed workspace.
* If it carries none, the existing ordinary range resolve runs unchanged.
* The imported graph is compared twice: the Go candidate comparator with the new
  `--shrinkwrap-authority` mode (which relaxes only the *retained snapshot* freeze,
  keeping the manifest/workspace/manager invariants, the archive SRI freeze and
  the closure rules), and `validateShrinkwrapDelta` against the pre-import
  expected graph. Neither can be reached by the ordinary update, adoption or
  bridge paths, which keep the retained freeze.

Evidence from that leg (`target/ci-logs/pnpm-primitives-4af1032ac2004d9f972ec03a53c0bdff`):
route `shrinkwrap` recorded in the build evidence with its reason and the selected
archive's SRI; five supplemental SRIs each with their `pnpm view` origin and
archive verification; 144 SRI pairs compared raw-equal, 0 unequal, 0 optional
classification mismatches, 0 added/removed identities; exactly the three reviewed
normalisations reported separately for review; the published `pnpm-lock.yaml`
digest `175f9a3a…9b7e` is byte-identical to the Phase 0 gate's imported lock. The
harness's own comparator (fed the registry SRI it captured *before* preparation)
independently returns the same three normalisations, and the run also proves 5/5
sibling archives and installed bytes, the chord-context esbuild transform
(`let x = 1;`, native `@esbuild/linux-arm64@0.28.2`), the seven-command dispatch
list, both health records and the offline capability-zero pairs.

No new trust or age exception was needed, and no canned 0.87 lock is committed.
A failing conversion still stops with evidence instead of falling back.

The preparation policy for both projects is stated here so it is not left implicit:
an **equal request starts no manager at all** (no `pnpm install`, no `pnpm view`,
no download), and every refusal that can be made before Node or the manager
(`NODE_OPTIONS`, `NODE_PATH`, `BUN_OPTIONS`, a project `.npmrc`,
`PNPM_CONFIG_REGISTRY`) is made before either runs. A changed or refreshed request
must fail when the authorized manager route cannot complete, because the
preparation boundary has no soft path. `script/test/standard-node-policy.py
--consumer pi|bridge` is that rule's test: a recorder shim is the only manager
route, so "the shim was never called" is an observed fact rather than the driver's
claim. That is why the pi/bridge legs run the process-policy matrix instead of a
real-manager matrix: the dsh workspace's own scenario matrix (its default
selection, upgrade path and web/egress legs) was **not** run for Pi or the bridge,
and nothing here inherits its result. What these two projects are proved against
is the zero-manager rule, the refusal set and their own cold legs.

Three further facts the real legacy graph settled, each with a killing mutant:

* pnpm records optionality on the **snapshot** (`optional: true`) and not always on
  the package record, so `installed-graph.mjs` accepts either spelling of that one
  fact. The locked platform selectors remain the *only* thing that makes an
  omission provable: a required or platform-compatible missing payload is still
  hard.
* A package's `bin` may be a string and may start with `./` (published
  `yaml@2.9.0` is `"bin": "./bin.mjs"`). Both spellings denote one location and
  are normalized; any other dot or empty segment is refused.
* `@esbuild/*` platform packages publish no `bin` at all, so the native command
  path is an authored expectation (`bin/esbuild`) bound by the version probe
  (`--version` must report the locked version), not a guess about a declaration.

Two normalisations this ADR did not enumerate were observed on the *first real
cold installs* and are now part of the reviewed rules, each with a mutant that
kills it:

* `images/tools/pi/installed-graph.mjs` ignores exactly the manager's own
  top-level metadata files (`node_modules/.modules.yaml`, `.package-map.json`,
  `.pnpm-workspace-state-v1.json`) in the orphan scan; a directory or a link of
  the same name is still reported.
* A hoisted bridge tree keeps a **metadata-only** `node_modules/.pnpm/lock.yaml`
  under pnpm 11.11.0. The installer, the verifier and
  `script/test/standard-node-layout.py` therefore reject private-store *payload*
  (any directory below the store) and require the bridge package to be a real
  directory, instead of rejecting the store's presence.
* `.bin/pi` may link the public package directory *or* the resolved virtual-store
  instance. `check-runtime.mjs` checks the resolved target inside the payload, not
  one relative spelling, because the real install produced the second shape.

Measured registry facts for the ledger: the reviewed bridge `0.8.0` registers 14
models beside Pi `1.4.0` and 11 beside the authenticated legacy `0.87.0` graph
(the bridge enumerates the providers the *selected* Pi SDK ships). The harness
records both counts per selection so a change stops review, while the verifier
keeps accepting one canonical positive N.

### Activation (2026-10-29)

Both four-input sets are committed and both npm project locks are deleted, so
every caller now reads the reviewed pnpm inputs: the build, both installers, both
verifiers, both upgrades, the released probe, the ledger, the focused harness and
the static/image-source guards. The provisional proposal path (`#52`) that let
the harnesses read a tree outside the repository is gone; a missing input is a
failure, never a second source. Native acceptance is still the CI run at the
pushed SHA; nothing local is claimed as product evidence.

## Consequences

* Changed and refreshed Pi selections have a real, testable route; refusing them
  would break the required interface, and resolving them freely would break the
  reviewed graph.
* Two independent byte/layout proofs exist (build checker + Python oracle) and
  disagree loudly rather than averaging.
* The default 1.4.0 conversion is an ordinary all-hashed import; the shrinkwrap
  route is exercised by the genuine changed-selection 0.87.0 harness leg, not by
  a canned committed 0.87 lock.
* Nothing here certifies future selections, amd64 layout or native acceptance.
  The required evidence is a clean native amd64+arm64 build at the proposed SHA
  with the focused harness legs and the independent oracle, and that evidence is
  not claimed by this document.
