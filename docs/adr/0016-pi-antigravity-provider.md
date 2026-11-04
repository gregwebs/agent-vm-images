# 0016: The Antigravity provider is an image-owned Pi extension package

Status: accepted. Companion records: `0011-pi-selected-shrinkwrap.md` (the
reviewed pnpm project route both Pi extension packages use),
`0023` of the launcher repository (image-owned Pi extension packages),
`0014-image-network-requests.md` (why this adds no request label).

## Context

`pi-antigravity` is a Pi *provider* extension, not a CLI bridge: it registers a
provider that speaks to Google Antigravity / Cloud Code Assist directly, with
no child process and no external executable. It declares Pi's own two packages as
peers (`@earendil-works/pi-ai`, `@earendil-works/pi-coding-agent`), which Pi
supplies through its loader, and one real runtime dependency, `undici`.

A guest cannot install it usefully by hand: `pi install npm:pi-antigravity`
writes into a mutable per-guest tree outside any reviewed selection, and the
standard image's Pi is a root-owned picommand. The image owns both Pi extension
packages it ships, so it owns this one too.

## Decision

### A third reviewed pnpm project, in its own prefix

The provider is installed by the base pnpm exactly `11.11.0` from four reviewed
inputs (`images/tools/pi/antigravity/`), into `/opt/agent-vm/pi-antigravity`.
It does not share the bridge's prefix: the reviewed-input policy is one root
package per project (the Go helper's `Kind` names exactly one root dependency),
and the two packages have different layouts — the bridge omits SDK platform
packages through `ignoredOptionalDependencies`, the provider omits nothing.

The reviewed workspace is hoisted, `autoInstallPeers: false`, and installs no
peer at all. `recipe-contract pnpm-lock` rejects any resolved
`@earendil-works` identity, so a second Pi (or typebox) inside the provider tree
is a build failure, not a resolution. The wrapper loads it as a third explicit
`--extension` after the bridge; an absent or opted-out provider costs the
provider, not `pi` (`AGENT_VM_PI_NO_ANTIGRAVITY`).

At `0.11.0` the provider reaches exactly two identities: itself and
`undici@8.12.3`. That is an independent reviewed expectation, asserted by the
Rust source-identity suite and by the finished-image layout oracle; a different
shape is a review stop.

### The one reviewed trust exception is exact-versioned

The reviewed install envelope carries pnpm's
`--config.trust-policy=no-downgrade`. Every published version up to and including
`0.10.0` carries SLSA provenance attestations; `0.11.0` carries none, so pnpm
refuses it as a possible package takeover:

```
[ERR_PNPM_TRUST_DOWNGRADE] High-risk trust downgrade for "pi-antigravity@0.11.0"
```

The operator selected `0.11.0` on 2026-11-03, having been shown the refusal and
the `0.10.0` alternative, and the workspace therefore names exactly one
exception: `trustPolicyExclude: ['pi-antigravity@0.11.0']`. The exception is not
a policy relaxation: it names one version, it is compared byte for byte by the
Go workspace policy (so a wildcard, a second entry or another version fails
every gate), and it is re-reviewed whenever the pin moves.

The exception means this package's *content* is authenticated only by registry
TLS plus the lock's own SRI, and by the review of the published tarball for this
version. It establishes no signer identity and no build reproducibility, and no
document may call it attestation.

### No network request

The provider contacts `accounts.google.com`, `oauth2.googleapis.com`,
`www.googleapis.com` and the `cloudcode-pa` endpoints, but only after a guest
signs in and selects the provider. Under ADR 0014's vocabulary those are hosts
reached through a provider choice: the user's decision, not an image or agent
request. The Pin image therefore adds no `org.agent-vm.network-requests.*` label
for it, exactly like the Claude bridge and the Codex extension. Loading the
extension performs no network call: the provider registers its catalog offline
with no credential present, which is what the build gate probes. The count
follows the selected Pi SDK's own catalog rather than the provider's: the
focused harness measured **8** beside Pi `1.4.0` and **9** beside the historical
`0.87.0`, exactly as the bridge's catalog moves (14 versus 11). Each selection's
count is an authored expectation, not a constant.

## Consequences

- Pi has two image-owned extension packages with separate prefixes, records,
  seeds (the bridge's seed stays its own), gates and version slots. One failing
  slot withholds its own status record only.
- A genuine Pi curl absence still retires the whole feature: the retirement
  inventory now names both payload roots.
- The trust exception is the first one this image carries for a package that
  lost provenance rather than never having had it. A future bump that again
  trips `ERR_PNPM_TRUST_DOWNGRADE` is a stop for review — the cheapest correct
  answer may be to stay on the last provenance-carrying version.
