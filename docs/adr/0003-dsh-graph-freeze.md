# 0003: dsh graph freeze for a sole-root pnpm project

Status: accepted for [#51](https://github.com/gregwebs/agent-vm-images/issues/51)
(PR 2 of 4 for [#42](https://github.com/gregwebs/agent-vm-images/issues/42)),
subject to the independent review that PR requires.

## Context

dsh was an npm project with one hoisted `node_modules` tree, and its checker
froze **locations**: any lock location outside the selected dsh dependency had to
be byte-identical, because an unchanged location was assumed to describe the same
resolved package. That is a property of npm's tree, not of a graph.

The migrated project is one pnpm *root* (`packages: [.]`) that inherits the base
image's pnpm 11.11.0 launcher and owns exactly four input files. npm locations no
longer exist: pnpm records one **archive** per `name@version` and one **snapshot**
per peer context, and every snapshot key is a full identity. A selection change
therefore shows up as identities, peer contexts and edges, not as moved
directories:

```text
dsh A ──► x@1 ──► y@1            dsh B ──► x@2 ──► y@1
        y stays an identity; x may be replaced because B reaches it
        z (unreachable) may never be added; changing y's bytes fails
```

A rule is needed for "a developer changed the selected dsh version; which
records may move?" Copying npm's location freeze would reject legitimate updates;
freezing nothing would let a changed selection silently rewrite unrelated
records. This decision defines the boundary, and — just as importantly — states
what it does **not** guarantee.

## Decision

`compareDshUpdate({before, after, requestedVersion, refresh})` in
`images/recipe-contract/pnpm-lock.mjs` is the only production comparator. Both
sides are complete validated dsh projects (structure, policy, no second
importer, no pnpm archive/snapshot anywhere, reachable `dsh-sandbox-local`,
effective exact peers). The rules, in order:

1. **Selection changes only the root specifier.** After masking exactly
   `.dependencies['@deepseek-ai/dsh']` for a real changed request, the two
   manifests are deeply equal; the workspace is byte-equal; lockfile settings,
   overrides (selector *and* value) and peer policy are semantically equal. No
   other importer, field or setting may move.
2. **Equal or empty selection is byte-frozen.** All four files are compared as
   bytes through bounded regular-file reads, and preparation starts zero package
   managers. A whitespace or pin-order change is a change.
3. **Closure, not prefix.** For a changed selection the old and new dsh-root
   closures are traversed separately over real dependency edges, mandatory and
   optional, with a visited set so cycles terminate. Removed archives/snapshots
   must lie in the old closure and additions in the new one; orphan records fail
   on either side. Membership is exact identity equality — `dsh-extra` gains no
   privilege from a `dsh` prefix.
4. **Retained archive records are frozen.** For each `name@version` present on
   both sides, the full raw record must be canonically equal: SRI, resolver
   (including whether an explicit tarball was recorded), peer declarations and
   metadata, `engines`/`os`/`cpu`/`libc`, `optional`, deprecation, bin mapping.
5. **Retained snapshot records are frozen, with one exemption.** For each full
   peer-context key present on both sides, the raw snapshot metadata and all
   **non-root** dependency/optional edges must be equal, with the optional flag
   and peer context preserved. Only the changed dsh root's own outgoing edges may
   differ; a surviving snapshot that happens to be the root still keeps its
   archive record frozen.
6. **Equal selection may not be refreshed implicitly.** A developer who wants new
   transitives at the same selected version must ask for it explicitly
   (`--refresh-lock`); see "Refresh and the reviewed map" below.
7. **Everything else still fails**: invalid exact grammar, manager/base-pin
   disagreement, `packageManager` assertion drift, policy/settings edits, an
   unreachable or missing sandbox snapshot, a retained integrity change, an
   added unreachable identity and any unsatisfied effective peer.

## Peer-context identity rules

The real graph cannot be parsed by name alone. Two measured facts from the exact
committed workspace (see the [ledger](../standard-pnpm-migration.md)):

- When the joined peer-id list exceeds pnpm's suffix budget, the segment is
  replaced by an **opaque 32-hex string**: the first 32 hex characters of the
  sha256 of the sorted peer ids. 59 of 565 snapshots carry one, including the dsh
  root `@deepseek-ai/dsh@0.1.5-rc.2(711fd5df96d806afccbe1a29bd047122)`.
  It is an **identity, not data**: it is never reconstructed, never inverted and
  never used to grant or deny reachability. Anything that is not exactly 32
  lowercase hex inside parentheses is a parse failure.
- A context segment may name a provider by **bare package id** while that
  provider's snapshot key carries its own peer context. Segments therefore
  resolve as: full snapshot key if present, otherwise the **unique** snapshot with
  that package id. Ambiguity (more than one candidate) or a dangling reference is
  a hard failure that names the offending key; the resolved provider must also
  equal the dependency edge target.

Snapshot identity is the whole key string, so the comparator adds/removes/retains
whole keys. Opaque contexts never participate in closure traversal (only
dependency edges grant reachability), so they cannot hide a changed record.

The exact-peer oracle is **per context**, never "a satisfying version is
installed somewhere": for every snapshot, every exact peer declaration must be
satisfied by that snapshot's own edge target, and an absent peer is allowed only
when the declaration is marked optional. The committed graph reports 243 exact
declarations (229 `@deepseek-ai/cordis`), 0 unsatisfied and 0 optional missing;
those counts are recomputed output, never constants. Globally finding
`cordis@4.0.2` is not evidence that a plugin's context uses it.

## The accepted relaxation

The old npm rule froze hoisted records even when a changed root reached them. The
sole-root closure rule **permits replacement and removal of transitive
identities inside the changed dsh root's closure**, while freezing every retained
archive and snapshot record. Consequences that must be stated rather than
implied:

- A changed dsh selection is **policy-enforced, not fully transitive-frozen**.
  Records in that closure may change version or disappear; the comparator's job
  is to keep them inside the closure, keep retained bytes equal, and keep policy,
  integrity, sandbox and exact peers intact.
- The default committed graph is frozen. Nothing changes until a maintainer
  selects a different exact dsh version (or explicitly refreshes).
- Because the project has exactly one root, no "shared with an unchanged root"
  set exists. If a second importer is ever added, that generalization must first
  freeze every record shared with the unchanged root's closure; it is deliberately
  not implemented now, and the closed schema rejects a second root.

## Refresh and the reviewed map

`--refresh-lock` is a developer opt-in for the same selected version. It
deliberately relaxes retained/transitive freeze and byte equality, and deliberately
does not relax: exact selection, manifest-only selection changes, workspace and
override invariance, policy, no-pnpm, no-orphan, sandbox, exact peers or base
ownership. Image installation never passes it.

Cordis overrides are a *reviewed selection*, not a resolution escape hatch. A new
dsh version whose family selection differs is first resolved **without** the old
override map (a distinct tentative graph with no install/pin capability), which
produces the exact requirements and the proposed map. An ordinary upgrade stops
on a differing map. `--cordis-overrides FILE` publishes only a map that matches
that independent derivation, and only after the real native install/web/subprocess
gates pass. No generic "the graph changed" flag can alter the workspace.

## What a passing comparison does not mean

- It is not runtime health. A graph can be perfectly frozen and still ship a dsh
  whose web profile cannot start; readiness, native prebuilds, subprocess and the
  sandbox are separate gates, and a healthy-but-timed-out process is not proof.
- It is not physical layout proof. Graph validation requires a reachable sandbox
  snapshot; the private-hoist resolution that makes it loadable is proven only by
  the installed-tree check and the independent layout oracle.
- It is **not independent cryptographic attestation verification**. Comparing
  integrity and registry provenance metadata detects a changed or downgraded
  publication; it does not verify a signature or an attestation of upstream
  provenance.
- It does not certify an image. Only a clean native build at the proposed SHA
  does, and a stale local base probe is diagnostic.

## Alternatives

- **Equal-only builds (byte freeze only).** Rejected: the build deliberately keeps
  the `AGENT_VERSION_DSH` changed-selection interface, and equal-only would
  remove it (or force maintainers to hand-edit committed inputs during a build).
- **Stricter semantic closure freeze** (freeze every transitive record reachable
  from the root even under a changed selection). Not implied by this rule and
  rejected: it refuses many legitimate updates, because the upstream package's
  own dependency selection legitimately moves, and it would force maintainers to
  approve a hand-written lock. A changed selection stays policy-enforced.
- **Trusting pnpm's own peer resolution.** Rejected: correct-looking global
  versions coexist with wrong per-context providers, which is exactly the failure
  the Cordis overrides repair.
- **Reusing npm location-freeze semantics on pnpm.** Rejected: locations do not
  exist, and a name/version prefix is not an identity.

## Related records

- [ADR 0001](0001-build-only-pnpm-graph-parser.md): the build-only parser and
  vendor trust boundary this rule runs on.
- [ADR 0002](0002-pnpm-terminal-failure-boundary.md): every pnpm failure stays
  hard; no retry, receipt or npm fallback.
- [Standard pnpm migration ledger](../standard-pnpm-migration.md): the reviewed
  npm→pnpm delta, including the exact records this decision froze.
- [Contributor maintenance](../../CONTRIBUTING.md#dsh-maintenance-and-graph-freeze).
