# Bounded graph fixtures

These fixtures exercise build-only primitives, not an agent migration.
`peer-vectors.json` records the reviewed Cordis discriminator from #42's plan:
matching an unrelated 4.0.2 archive is insufficient when the snapshot resolves
4.0.4. The complete candidate comparison (243 peers, 238 wrong without overrides,
zero wrong with overrides) belongs to PR 2 and must be recomputed independently.
No production lock or source selection is changed here. Host pnpm 12 must never
write fixture locks; real emitted fixtures use authenticated pnpm 11.11.0.

## Real emitted dialect (not consumer acceptance)

`real-bridge/` preserves the unedited lock emitted on 2026-10-16 with stock
pnpm 11.11.0 via `/tmp/avm-42/bin/pnpm` (authenticated bootstrap wrapper, five
base policy defaults). Generation used invocation-owned HOME/XDG/store paths:

```sh
pnpm install --lockfile-only --ignore-scripts --ignore-pnpmfile --pm-on-fail=error --config.update-notifier=false --registry=https://registry.npmjs.org/ --store-dir="$SCRATCH/store"
```

The deliberately older bridge selection keeps this distinct from production.
Generation reported 101 resolutions; no payload install. The lock exhibits
scoped identities, peer suffixes, and a present optional Zod edge. Independent
review vectors in `real-bridge-evidence.json` were transcribed from the emitted
lock and upstream bridge metadata, not the production graph CLI. Tiny expected
graphs are independently authored from the three-node fixture specification;
their all-zero SHA-512 is synthetic, never an installable package claim.
No fixture certifies future production workspace runtime or interoperability.
