# Standard pnpm migration ledger

This is the reviewed npm→pnpm selection ledger for the one migrated consumer,
**dsh** ([#51](https://github.com/gregwebs/agent-vm-images/issues/51), PR 2 of 4
for [#42](https://github.com/gregwebs/agent-vm-images/issues/42)), together with
the evidence that produced it. [ADR 0003](adr/0003-dsh-graph-freeze.md) owns the
freeze rule this ledger is reviewed against, [ADR 0001](adr/0001-build-only-pnpm-graph-parser.md)
owns the build-only graph parser that produced the records, and
[ADR 0002](adr/0002-pnpm-terminal-failure-boundary.md) keeps every pnpm failure
hard. Pi, bridge and Copilot stay on their current npm paths until their own
slices; OpenCode was already a separate pnpm consumer with its own byte-only
checker. Nothing here announces those migrations.

The ledger compares two **selections**, not two health claims. Every row answers
"what identity, archive, edge or peer did this file select", and a successful
graph comparison never stands for a working `dsh`: the version string, the HTTP
readiness probe and the native/subprocess probes are separate gates. Identity
counts, including a matching total, are observations to recompute — never a
constant to assert — and a preserved identity count is not a claim that edge
selection, peer context or optional payload also stayed equal.

The old npm graph is read-only history: `git show
6c51509e949ea643b82515f5c8a53acd122861c0:images/tools/dsh/package-lock.json`.
Its SHA-256, location and identity counts are recorded in the linked JSON, so
deleting the file from the working tree loses no reviewed input.

## Reviewed inputs and manager provenance

dsh is one pnpm project of four files, all four published transactionally:

| File | Owns |
|---|---|
| `package.json` | the one exact `@deepseek-ai/dsh` selection; `packageManager: pnpm@11.11.0` is an assertion, not a second selection |
| `pnpm-workspace.yaml` | the reviewed settings, four exact Cordis override selectors and the single `chokidar@4.0.3` trust exception |
| `pnpm-lock.yaml` | the resolved graph this ledger reviews |
| `pnpm-inputs.sha256` | the reviewed bytes of the three files above |

The base image remains the only pnpm selection and launcher owner; dsh has no
private pnpm dependency or executable, and the resolved graph contains zero pnpm
archives and zero pnpm snapshots. The manager that produced the lock was the
authenticated stock **11.11.0** distribution, invoked by absolute path with a
private HOME/XDG set, all five image defaults explicitly exported and the same
five flags on the resolving call. Host `pnpm` 12 was never invoked
(`hostPnpm12Used: false`). The exact commands, argv and timings are in the
`commands` array; the archive SHA-512/SRI, pristine bundle SHA-256 and upstream
tag/commit are in `manager`.

## Graph delta

```text
BEFORE (npm, one hoisted tree)          AFTER (pnpm, one root, isolated)
package-lock.json                      pnpm-lock.yaml
588 locations, 569 identities          565 archives = 565 snapshots (peer-context keys)
  dsh 0.1.5-rc.2                         dsh 0.1.5-rc.2(711fd5df…opaque)
  pnpm 11.11.0 (duplicate)               (base-owned; removed)
  cordis 4.0.4 + 4.0.2, loader           cordis 4.0.2, loader 1.0.3,
    1.0.5 + 1.0.3, timer 1.1.6 + 1.1.4     timer 1.1.4, include 1.0.7
```

The JSON block linked below carries every row and, for every delta row, the
reason derived from that row. The prose here only states what to look for:

- **565 retained identities**, `sriEqual` true for all 565: every surviving
  identity keeps its exact reviewed archive integrity, and no identity was
  added.
- **4 removed identities**: the three Cordis family substitutions
  (`cordis` 4.0.4 → **4.0.2**, `cordis-plugin-loader` 1.0.5 → **1.0.3**,
  `cordis-plugin-timer` 1.1.6 → **1.1.4**) and the duplicate `pnpm@11.11.0`,
  which the baseline root declared and the base image owns.
  `cordis-plugin-include` 1.0.7 was already the only selection, so its override
  drops nothing. Each row names the override selector that removed it.
- **23 representation-only metadata deltas**: same tarball, different recorded
  projection (`libc`, `engines`, peer declarations or peer metadata).
- **921 mandatory-edge rows**: 912 added peer providers, 3 retargets and 6
  removals. Five removals belong to the three removed Cordis identities. The
  sixth is **not** a Cordis row: `ajv-formats@3.0.1 → ajv` moved from the npm
  mandatory projection to the pnpm optional (peer-provider) edge set, which is
  the one non-Cordis removal and is counted again in the optional edges below.
- **32 optional edges added, none lost**: `autoInstallPeers` materializes
  optional peer providers as `optionalDependencies`; the 78 platform optionals
  pnpm also records were already npm optional dependencies and are not deltas.
- **5 identities deduplicated** from up to 8 npm locations each (for example
  `debug@4.4.3` and `ms@2.1.3`). npm hoisting is a location tree; pnpm keeps one
  snapshot per peer context, so an identity's npm location count is never a
  pnpm record count.

### The fifth Cordis companion is not an override

`schemastery` is the one Cordis-family package the review does **not** override,
and the ledger says so instead of filing the change under "dedup". The root
selection `@deepseek-ai/dsh` declares the range `^3.18.2`, so pnpm selected the
newest satisfying version **3.18.4**; `@deepseek-ai/dsh-settings` declares the
exact peer **3.18.2** and keeps its own 3.18.2 context. In npm the root's
3.18.2 was the same instance that satisfied that exact peer, so this is a
**singleton selection change**, not a merge of two records. It is allowed only
because `strictPeerDependencies` is `false`; both versions stay in the graph and
the config/web path is proven by the runtime readiness leg, not by this row. No
fifth override was added — `delta.singletonSelections` records the derived
declarations, snapshots and reason.

## Peer context and sandbox

The exact-peer oracle is per snapshot, never "a satisfying version exists
somewhere": 243 exact peer declarations, 229 of them `@deepseek-ai/cordis`, 0
unsatisfied, 0 optional peers missing. The 62 rows whose provider is named only
through the opaque 32-hex context suffix are resolved through the dependency edge
target — see [ADR 0003](adr/0003-dsh-graph-freeze.md) for why the hash is an
identity and never reconstructed. All 243 rows are in the linked JSON.
`graph.opaquePeerContexts` counts snapshots, not hashes: **59** snapshots have a
peer context that is exactly one opaque hash, those carry **57** distinct
hashes, and **102** snapshot keys mention one of them (the key text renders a hash
in more positions than the parsed peer context records).

One `dsh-sandbox-local` snapshot is retained, with two canonical owners
(`dsh-base`, `dsh-sdk-minimal`) reaching the same full peer-context key. That is
graph reachability; the private-hoist physical resolution is proven separately by
the installed-tree check and the independent layout oracle.

## Scripts stay disabled

`ignoreScripts: true` is explicit on preparation and installation, and the
workspace grants no `allowBuilds` entry. Seven script-bearing or prebuilt
identities were inspected instead of executed; each row in `scripts` records the
script map, the script-file hashes, the shipped payload and the runtime
discriminator that proves the prebuild is real:

- `node-pty@1.2.0-beta.15` — `install`/`postinstall` only check or prune prebuilds;
  the tarball ships `prebuilds/linux-{arm64,x64}/pty.node` and a real PTY
  spawn/read/exit proves it.
- `koffi@3.3.1` plus `@koromix/koffi-linux-{x64,arm64}@3.3.1` — the `install`
  script is a source build; the Linux `koffi.node` prebuilds are the payload and
  a libc FFI call proves it.
- `@google/genai@1.52.0` — the installable hooks are literal `echo` no-ops;
  importing its shipped `dist` from the owning context proves it.
- `protobufjs@7.6.6` — `postinstall` returns immediately without a declared
  `versionScheme`; a Type/Field encode/decode roundtrip proves it.
- `@deepseek-ai/dsh-subprocess-local@0.1.5-rc.3` — `postinstall` only restores the
  executable bit of an existing node-pty spawn-helper, and the exact Linux payload
  ships no spawn-helper, so a real subprocess launch through the package is the
  discriminator. The inspected script excerpt and hash are in `scriptExcerpts`.

A missing prebuild fails the gate; no script approval is added and no npm
fallback exists.

## Changed selections

The default/equal path is byte-frozen: with empty or equal slots preparation
starts **zero** manager processes and compares all four files, and a real equal
copy comparison reproduces `243 exact peers satisfied` (see `integration`).
`0.1.5-rc.1` is the reviewed **changed** candidate: preparation resolves it under
the same unchanged workspace, publishes build-local pins and validates the exact
update rules. Its native ready/web/subprocess run is a separate mandatory leg and
is not claimed below.

A changed selection is policy-enforced, **not fully transitive-frozen**: retained
records keep their bytes, but identities reachable in dsh's own closure may be
replaced or removed. That is the accepted relaxation, argued and limited in
[ADR 0003](adr/0003-dsh-graph-freeze.md); it is not a licence to change the
workspace, the settings, an override selector or a record outside that closure.

## What this ledger does not claim

- **No independent cryptographic attestation verification.** Integrity/SRI and
  registry provenance metadata were compared; that is not signature or
  attestation verification of the upstream publication, and pnpm's policy check
  is not one either.
- **No completion of #42.** dsh alone is migrated here; Pi, bridge and Copilot
  keep their npm paths, and a dsh strict-egress profile is still PR 4.
- **No "identity count" shortcut.** A preserved count (for example the Pi slice's
  "143 identities" observation, which belongs to a different consumer) never
  substitutes for SRI, edge and peer comparison.
- **No native or proposed-SHA acceptance.** `acceptance` is pending for both
  architectures and the independent review. The local baseline stays unproven:
  the `install-zellij` signal-INT case is spec-carried and did **not** reproduce
  in the green local full-contract runs; it reproduced twice on a loaded host and
  passes standalone, so it is recorded as flaky test behaviour, not a product
  defect and not a green result.
- **No diagnostic re-use.** Local probes against a stale local base are
  diagnostic only: they cannot certify a built image, because the base has no
  whole-source provenance. `runs` is empty here on purpose rather than filled
  with a placeholder.

## Machine-readable ledger

[`docs/standard-pnpm-migration.json`](standard-pnpm-migration.json) is the one
canonical machine record: every retained archive row, every edge row with its
derived reason, all per-context peer rows and the inspected script excerpts.
It is **generated**, not hand-written, from exactly two reviewable inputs — the
baseline npm lock at commit `6c51509` (read through `git show`, so the deleted
file still needs no copy) and the committed
`images/tools/dsh/{package.json,pnpm-workspace.yaml,pnpm-lock.yaml}`:

```bash
node script/test/pnpm-migration-ledger.mjs           # rewrite the ledger
node script/test/pnpm-migration-ledger.mjs --check   # drift gate: nonzero on any byte
```

`script/test/contracts.sh` runs the `--check` form, so a hand-edited or stale
ledger fails contracts; the check reads the baseline commit object and fails
loudly (never skips) when that object is unavailable. Recorded measurements that
no input file contains — manager provenance, command transcripts and inspected
script payloads — live in the generator's `RECORDED` block and are cited above.
Do not edit the JSON by hand: change an input, or the generator.
