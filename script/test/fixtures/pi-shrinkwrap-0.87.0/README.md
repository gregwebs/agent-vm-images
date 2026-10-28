# Pi 0.87.0 selected-shrinkwrap vectors (issue #52, Phase 0)

Data only. These record the executed feasibility gate (2026-10-28) that asked
whether authenticated `pnpm import` preserves a selected Pi archive's upstream
dependency shrinkwrap. `python3 script/test/pi-shrinkwrap-vectors.py` replays them
offline; no network, package manager, npm installation, or committed 0.87.0
`pnpm-lock.yaml` is involved.

| File | What it is |
|---|---|
| `authenticated-shrinkwrap.json` | The published 0.87.0 tarball's `npm-shrinkwrap.json`, byte-for-byte after the archive SHA-512 was verified. Root and five sibling records carry no integrity. |
| `resolutions.json` | Root plus the five legacy sibling archives (`chord`, `pi-agent-core`, `pi-ai`, `pi-telemetry`, `pi-tui`) with their exact registry integrity, URL and verified archive SHA-256. The gate resolved only these five missing SRI records; no arbitrary refill. |
| `synthesized-package-lock.json` | The npm-shaped project lock the gate synthesized from that shrinkwrap for `pnpm import`: root depends only on the exact selected Pi, every nonroot package location is prefixed under the Pi package, existing SRI is preserved. Never committed as a production input. |
| `result/parse-yaml.stdout` | The pnpm v9 lock the real authenticated 11.11.0 import emitted, captured as syntax-only JSON. This is a graph vector, not a lock file: the fixture deliberately contains no `pnpm-lock.yaml`. |
| `compare.py` | The independent stdlib comparator from the gate. It writes `comparison.json`/`comparison-summary.md` beside itself, so every run copies the fixture into scratch. Evidence only: the C3 converter must not import or share this implementation. |
| `expected-delta.json` | The reviewed aggregate result (identity, occurrence, SRI, resolved edge, optional, peer, bin counts), the three approved representation normalizations, and what the gate does not claim. |
| `mutants.json` | Five offline mutations the gate demonstrated (SRI, optional flag, resolved edge target, explicit optional peer expansion, added orphan) with the exact counter each must move. |

The only change from the gate's own directory is that the comparator reads the
renamed `resolutions.json` instead of the gate's `authentication.json`: the
container-local archive paths were dropped and the compared fields are unchanged.

## Limits

These vectors establish material lock-level fidelity for this selection, exact
pnpm 11.11.0 configuration and time. They are not a cold install, payload-byte,
physical-layout, runtime, egress or native proposed-SHA proof, and they do not
show that pnpm transplants graphs: every future selected shrinkwrap still needs
its own comparison. `bin` versus `hasBin` is presence only; pnpm drops command
names and paths. The comparator's `comparison.json` (per-record evidence) is kept
in the gate record, not here, because it is over 1 MiB.

## Why here and not Go testdata

The only production consumer of a selected-shrinkwrap conversion is the
build-only Pi converter (Node, `images/tools/pi/import-shrinkwrap.mjs`) and its
test `script/test/pi-shrinkwrap.mjs` (Phase 2/C3), so the vectors live beside the
other `script/test` fixtures. Go's independent graph vectors are hand-authored in
`images/recipe-contract/pnpmlock` and describe the typed projection; they are not
derived from this experiment.
