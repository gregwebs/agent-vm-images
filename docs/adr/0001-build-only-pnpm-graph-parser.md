# 0001: Build-only bounded pnpm graph parsing

Status: accepted for #42's primitive slice.

## Decision

Image-owned install preparation must preserve exact changed-selection interfaces
for dsh, Pi and bridge. A bounded, data-only v9 graph API validates reviewed
inputs before later slices adopt it. It neither installs nor resolves packages.
OpenCode keeps its independent equal-only byte checker and does not adopt this
parser. PR 1 migrates no production consumer or lock.

```text
untrusted inputs → bounded AST → policy + graph + exact-peer checks → validated graph
reviewed archive → SHA-512 → pristine Node YAML distribution → SHA-256 inventory
```

Vendor yaml@2.9.0's Node distribution with its ISC license, pristine package.json,
exact retained-file manifest and provenance. Import its explicit local entrypoint;
never pnpm internals. Explicit Node tests check digests, set equality and mutations;
Rust checks source/inventory boundaries without Node subprocesses. Build helpers
are read-only recipe-contract mounts, not runtime image payloads. Updates require
reviewed archive authentication, advisory/license inspection and inventory review.
This detects retained-byte changes; it is not independent upstream attestation.

## Alternatives

OpenCode's equal-only byte validation avoids parsing but would remove the other
consumers' approved override compatibility. Regex/hand-written YAML is unsafe and
incomplete. Host PyYAML introduces host-dependent behavior and an undeclared
runtime dependency. pnpm-internal parsing couples a security boundary to manager
implementation details. A small authenticated vendor provides a stable AST seam.

## Consequences

Untrusted YAML needs byte/depth limits, duplicate/tag/anchor rejection and closed
consumer policies. Pins are consistency records, not review approval; re-pinning
cannot excuse structural/policy failures. Exact peer checks use each snapshot's
actual context, not a matching archive elsewhere. Fixtures prove primitive
behavior only; later slices must regenerate exact configs and cold-install real
consumers before claiming runtime compatibility. dsh's later graph freeze is
weaker than its current location freeze and requires explicit review.
