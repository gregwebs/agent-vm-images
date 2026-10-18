# Base-owned pnpm

`pin.json` is the reviewed selection and provenance record. The archive is
https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz, upstream tag v11.11.0 at
19696e1f4957caa2813ce3beb026d9629e24e6e1. Bootstrap checks committed SHA-512/SRI
equivalence before extraction, package layout/version/MIT license, and pristine
`dist/pnpm.mjs` SHA-256 before installing. The upstream MIT license and bundled
notices stay in `/opt/agent-vm/pnpm`; no upstream distribution bytes are edited.
The build-only Python helpers do not ship in the installed package.

The shared POSIX launcher exports five caller-overridable defaults and execs
the fixed Node interpreter and immutable upstream entrypoint. Alias symlinks
select upstream pnpx rather than parsing or rebuilding arguments. This preserves
stock discovery and state handling; it is not enforcement or a pnpm patch.
See the canonical [runtime guide](../../docs/node-package-defaults.md).

## Reviewed updates

Review the new exact upstream artifact, release source and MIT notices; obtain
and independently recompute SHA-512/SRI and fresh extracted-bundle SHA-256.
Update all pin fields together and the base's literal pnpm label. Base remains
the only pnpm selection and launcher owner. Migrated dsh asserts
`pnpm@11.11.0` in `packageManager` and has no private pnpm dependency. A base
update must review every consumer's `packageManager` assertion and lock
together; do not update dsh independently with `--pnpm`. Host lock work uses the
authenticated distribution, not a newer host manager; see
[dsh maintenance](../../CONTRIBUTING.md#dsh-maintenance-and-graph-freeze).
Do not automatically trust registry metadata or
refresh digests. Rebuild base before standard, run hermetic/artifact probes and
both native full builds/runtime/strict-egress gates. Never certify an old tag
left by a failed rebuild.
