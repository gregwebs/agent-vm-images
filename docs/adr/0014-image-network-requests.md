# 0014: Every image declares its runtime network requests as OCI labels

Status: accepted. Launcher side is
[agent-vm#505](https://github.com/gregwebs/agent-vm/issues/505); the request
contract that labels feed is
[agent-vm ADR-0039](https://github.com/gregwebs/agent-vm/blob/main/docs/adr/0039-non-authoritative-tool-network-requests.md).
User-facing instructions are in
[examples/layers § Declaring network requests](../../examples/layers/README.md#declaring-network-requests);
maintenance commands are in [CONTRIBUTING](../../CONTRIBUTING.md).

## Context

An OCI image's config carries a flat `Labels` map, and `agent-vm` can read it
before a VM boots. That makes labels the one place an image can say which hosts
its software will try to reach. Agents under copilot and agy are not inventoried
yet ([#143](https://github.com/gregwebs/agent-vm-images/issues/143)), and the
launcher that reads the labels is a separate change (#505).

A `FROM parent` build inherits the parent's labels, and a child `LABEL` with the
same key **replaces** the parent's value. This is the constraint the key scheme
has to survive: a user-owned layer stacked on the released standard image must
not be able to erase what the base or the standard declares.

Three terms, used below and in the code:

- **Network request** — one TLS destination, written as an exact lowercase
  hostname with an optional port, that a component's software contacts at
  runtime under its default configuration. A request is *reported* by the
  launcher when a launch's egress does not cover it, and it never *grants*
  anything (agent-vm ADR-0039).
- **Image request** — a request declared under `image.<component>`. It applies to
  every launch of that image, because the software lives in the image filesystem.
- **Agent request** — a request declared under `agent.<agent>`. It applies only
  when that agent is the launched tool.

These are runtime requests. Hosts the build downloads from are a different
concern, pinned and audited by `script/test/shipped-installer-network.sh` and the
`--network=none` verifiers; nothing here opens a build-time path.

## Decision

### One key per component, never one key per scope

```text
org.agent-vm.network-requests.image.<component>     image request
org.agent-vm.network-requests.agent.<agent>         agent request
```

The prefix is `org.agent-vm.network-requests.`, then `image` or `agent`, then a
lowercase `[a-z0-9][a-z0-9-]*` name and nothing more. A single key per scope was
rejected: a `FROM` child replacing the same key would let the child rewrite its
parent's requests, and a layer's Dockerfile is exactly the thing a user owns and
edits. Separate keys make the union of all declarations the image's request set,
and separate keys permit additive stacking: a layer adds its own component's key
and no recipe in this repository may write another component's.

What enforces it is the built-image check, not the key scheme. A covered build
runs `check-image --inherits parent` against the layer's own inspected parent, so
a parent key that was rewritten, dropped or made invalid fails there. A Dockerfile
outside this repository's recipes is not covered: a user can set any key they
like, including another component's, and the launcher will read what the image
carries. The source check enforces ownership only over the recipes committed
here.

Ownership in the sources is enforced, not just documented: `images/Dockerfile`
declares only `image.base`, `images/standard/Dockerfile` only `agent.<x>` where
`images/tools/<x>/` exists, `examples/layers/<n>/Dockerfile` only `image.<n>`,
and any other Dockerfile declares nothing.

### The value grammar

A value is a comma-separated list with **no whitespace** of entries, each an
exact lowercase LDH hostname, optionally `:PORT`:

- labels are 1–63 bytes of `[a-z0-9-]` and do not start or end with `-`; the name
  is at most 253 bytes, with no trailing dot and no empty label;
- the final label is neither all digits nor `0x<hex>`, which keeps the launcher's
  kernel-mirroring oracle (its `egress_hostname.rs`) in agreement;
- `PORT` is decimal 1–65535 with no leading zero, excluding 53 and 853 (the DNS
  path); `:443` must be written bare, because the launcher canonicalizes `h:443`
  to `h` and an explicit 443 would make one entry look like two;
- no wildcard, scheme, path, userinfo, IPv4/IPv6 literal or `host.microsandbox.internal`;
- at most 64 entries, no duplicates.

**Canonical order** is ascending `(host, port or 0)`: among entries for the same
host the bare entry (443) comes first, then ascending numeric port, so
`h.example,h.example:1,h.example:8443`. That one key, `canonical_key`, is the
only sort order in the module; `NetworkRequest` deliberately has no generated
ordering, so a stray `sorted()` raises instead of inventing one. The order is a
writer convention, not launcher display parity: the launcher prints a non-default
port as `tcp://h:p` and accepts either.

**Empty** means "inventoried, contacts nothing". **Absent** means "not
inventoried". `agent.dsh`, `image.chrome-devtools` and `image.wirenboard-cpp`
ship as empty strings, and the launcher can tell that apart from copilot and agy,
whose keys are absent.

### The agent-request rule

A single-vendor agent requests its vendor's service hosts. A multi-provider agent
requests only what it contacts before the user picks a provider. Hosts reached
only through a provider choice, or through a link to another agent — the Pi
Claude bridge, the `agent-vm-codex` Pi extension, the `opencode-claude` plugin —
are the user's decision and are not that agent's request.

That makes the image's lists narrower than the launcher's own built-in catalog
for Pi and OpenCode, which is intended rather than drift: the catalog entry
inherits the bridge and extension hosts, and #505 records the question of whether
the catalog should narrow to match. The launcher combines both sources and
reports them by origin, so a narrower image request never hides a catalog one.

### Literal values, and what a build-arg override means

Every value is a literal in the recipe. `--build-arg AGENT_VERSION_CLAUDE=…` does
not change `agent.claude`, because the label describes the reviewed default
selection; a user who builds another version takes responsibility for that
version's hosts. Deriving the value from an `ARG` was rejected because the hosts
would stop being knowable from the source, which is the property both checks rely
on.

### The source form is strict, so no Dockerfile parser is needed

Each request label is its own single physical line in the **final** stage, in the
form `LABEL org.agent-vm.network-requests.<scope>.<name>="<literal>"`, with no
`$`, backslash, continuation or second key, and at most once per file. Anything
else that mentions the prefix — a continued `LABEL`, a multi-pair line, an
`${ARG}` value, a `--label` in a `RUN`, a line inside a heredoc body, the
singular `network-request.` typo — fails closed as an unsupported form rather
than guessing what Docker would do. Full-line comments are ignored, but they may
not contain a full key: a reader grepping for keys sees only real labels.

### Where the checks run

| Check | Command | Scope |
|---|---|---|
| Sources | `python3 script/test/network_requests.py check-sources` | `contracts.sh`, every PR, no Docker |
| Built base, standard, extension | `check-image` in `ci-native.sh`'s `build` phase | `build-local`, both native archs |
| Built example layer | `check-image --dockerfile … --inherits parent-inspect.log` in `layer-build.sh`'s INSPECT phase | `example-layers`, both native archs |
| Restricted-egress smoke | `network-requests-egress.sh` from `layer-build.sh`'s EGRESS phase | `example-layers`, layers with a fixture |

`script/test/network_requests.py` is the single owner of the grammar, the key
scheme, the source form, the ownership table and the image comparison. It is
Python because the built-image checks run on the native CI runners, which install
`python3` and no Rust toolchain.

`check-image` is exact equality with `source labels ∪ parent labels`, so it proves
that each declared label reached the final image, that no unexplained request
label appeared, and that a stacked layer preserved its parent's values. The
source check catches the same overwrite independently.

## Evidence: where each declared host comes from

Agent rows cite agent-vm `docs/default-tool-network-requests.md` and its
executable source `crates/agent-vm/src/default-tools.toml` at commit
`8ca1b712a32f142a29302b04a51e8c3b4a8c21c2`.

| Key | Value | Source |
|---|---|---|
| `image.base` | `api.github.com`, `github.com` | the `gh` CLI's API and git-over-https defaults |
| | `pypi.org`, `files.pythonhosted.org` | pip's default index and its file host |
| | `registry.npmjs.org` | npm/pnpm's default registry |
| `agent.claude` | `api.anthropic.com`, `mcp-proxy.anthropic.com`, `platform.claude.com` | evidence rows A/A1: inference, default-enabled claude.ai connectors, OAuth token exchange/refresh |
| `agent.codex` | `api.openai.com`, `auth.openai.com`, `chatgpt.com` | row B: API-key inference, first-party OAuth refresh, subscription inference |
| `agent.opencode` | `models.opencode.ai` | row D, credential-free provider-catalog refresh only |
| `agent.pi` | `pi.dev` | row G: `/model` provider-catalog refresh, credential-free, cached |
| `agent.dsh` | *(empty)* | dsh resolves models from an embedded catalog |
| `agent.copilot`, `agent.agy` | *(absent)* | not inventoried ([#143](https://github.com/gregwebs/agent-vm-images/issues/143)) |
| `image.rust-dev` | `index.crates.io`, `static.crates.io`, `crates.io` | cargo's sparse index, crate downloads, search/publish API |
| `image.go-dev` | `proxy.golang.org`, `sum.golang.org` | the `GOPROXY`/`GOSUMDB` defaults; the recipe sets `GOTOOLCHAIN=local`, so no toolchain is downloaded |
| `image.chrome-devtools` | *(empty)* | browsing is project-dependent; Chromium's background services are not needed |
| `image.wirenboard-cpp` | *(empty)* | apt, debootstrap and sbuild use plain-HTTP Debian mirrors |

**apt is excluded on purpose.** Debian's mirrors are plain HTTP
(`images/apt-policy/debian.sources`), which can never be a TLS network request, so
`apt-get install` adds no host to any list. A hop that upgrades to HTTPS is a
different question and would be a reviewed label change.

The same table is written literally in
`script/test/network-requests-tests.py::test_committed_values_match_reviewed_table`,
so moving a value is a two-file reviewed edit.

## Verification: what the restricted-egress smoke proves

`script/test/network-requests-egress.sh` reads the **built** image's own key (the
value that actually shipped), turns it into the installer gate's addon config, and
runs the layer's fixture command in a container whose only route out is a
digest-pinned mitmproxy on an `--internal` network. The proxy pin lives in
`script/test/fixtures/installer-egress/mitmproxy-image.ref`, shared with
`shipped-installer-network.sh`.

It proves two things and no more:

- **Strict run.** The command succeeds and every host it reached (CONNECT
  authority, SNI and upstream connection) is a declared one. Used hosts must be a
  **subset** of declared hosts: `cargo fetch` reaches only `index.crates.io` and
  `static.crates.io`, so `crates.io` is declared and never contacted, and Go may
  verify checksums through `proxy.golang.org` instead of `sum.golang.org`. The
  evidence records `used=` so a reviewer sees which path ran.
- **Denial control.** Removing one used host from the config makes the same
  command fail with `DENY connect-unlisted https://<that host>/` and no
  `UPSTREAM` line for it, which shows the gate is load-bearing rather than
  decorative.

It does not prove that every declared host is used, and agent lists are reviewed
data with cited sources rather than runtime-tested: they need credentials.

### TLS destinations, not HTTP Host headers

Host-and-path rules alone are not enough. The addon matches
`flow.request.pretty_host`, which is the HTTP `Host`/`:authority`, not the address
of the TLS connection; a client can send one host's `Host` header over a
connection to another host, and mitmproxy's default eager connection strategy can
contact the origin at CONNECT time, before any request hook runs. A probe against
the pinned proxy logged `ALLOW` for exactly that trick.

So the addon gained **one opt-in mode**, `"enforce_tls_destination": true`, which
only this smoke sets. With it on: the CONNECT authority must be declared (else
403, before any tunnel), the request's `Host` must equal the CONNECT authority and
the SNI (`authority-mismatch` / `sni-mismatch`), the upstream connection's address
and SNI must agree and be declared (`server-unlisted`), and `UPSTREAM` lines
record what was really contacted. `learn_redirects` is forced off, because a
learned redirect target would be an undeclared host allowed in by the back door,
and the runner passes `connection_strategy=lazy`; the addon refuses every request
if the strategy is eager. Every existing hook keeps its current behaviour when the
key is absent, and the installer gate's replay asserts that its own configs never
set it, so the installer policy is unchanged.

Each EGRESS run also executes destination controls with real Docker: an unlisted
CONNECT with a declared `Host`, a declared CONNECT with an unlisted SNI, a
declared CONNECT/SNI with an unlisted `Host`, and a positive control. The three
refusals must happen with no `UPSTREAM` line for the unlisted host.

### Fixtures are how a layer opts in

`script/test/fixtures/network-requests/<layer>/` holds the command that makes the
claim meaningful: a crate with one dependency and a committed lock for rust-dev, a
module with one dependency and no `go.sum` for go-dev. The phase runs only when
that directory exists, and its presence also selects the larger CI bounds. The
Rust and Go native suites stay manual maintenance evidence; this smoke is not
them.

## Consequences

- **A stacked layer keeps its parent's requests.** Nothing needs a repin: the
  example layers build `FROM` the released, digest-pinned standard image, so they
  carry the new base and standard labels only from the next standard release. Until
  then `--inherits` proves the rule against the layer's own label and becomes a
  real stacking proof afterwards. Against a parent that predates the labels the
  retention half of that check is vacuous -- there is nothing to retain -- so it
  must not be read as evidence that a layer preserved its parent.
- **CI bounds grow only where a phase was added.** A layer with an egress fixture
  gets a 105-minute cold-build step and a 155-minute row (230 if a runtime-suite
  layer ever gains a fixture); the runner is 830 s active plus a 90 s finalizer,
  and the supervisor needs 5700 s with a 290 s grace because it has to outlast
  both escalation windows of the smoke's own watchdog. Rows without a fixture keep
  80/130/205 minutes and 4500 s with a 90 s grace. Discovery is the filesystem,
  never a list of layer names, and the relations are asserted by
  `layer-build.sh --self-test`, so a future edit cannot quietly break them.
- **These are provisional floors.** ADR 0010's rule applies: raise every bound to
  at least 1.5× the measured first green duration per layer and architecture.
- **The smoke covers what has a fixture.** A component without one keeps only the
  static and built-image checks.
- **Changing what a component installs means reviewing its requests.** A new host
  a tool starts contacting is either already declared (nothing happens) or shows
  up as `DENY connect-unlisted` in an EGRESS row, with the URL in the evidence. The
  fix is a reviewed label change, and an install change that adds a component means
  adding its key.
- **Release-workflow label checks are a follow-up.** `release-standard.yml` and
  `release-rehearsal.yml` build the published product the launcher reads; running
  `check-image` there is a separate issue.

## Amends ADR 0010

[ADR 0010](0010-layer-build-ci.md) said no layer makes a runtime claim in CI
except Chrome. That is now: no layer's runtime suite runs except Chrome's, plus
the network-request egress smoke for layers with a fixture, which needs the image
this run built because cleanup removes the tag.
