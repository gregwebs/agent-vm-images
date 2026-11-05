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
its software will try to reach. Two agents, copilot and agy, were not inventoried
when this was written; [#143](https://github.com/gregwebs/agent-vm-images/issues/143)
inventoried them from their pinned artifacts (evidence I and J below), and the
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
ship as empty strings. No key in this repository is absent: every component here
has been inventoried, so absent is now only a state a Dockerfile outside these
recipes can express, and the launcher must keep reading it as "not inventoried"
rather than as "contacts nothing".

### The agent-request rule

A single-vendor agent requests its vendor's service hosts. A multi-provider agent
requests only what it contacts before the user picks a provider. Hosts reached
only through a provider choice, or through a link to another agent — the Pi
Claude bridge, the `agent-vm-codex` Pi extension, the `opencode-claude` plugin —
are the user's decision and are not that agent's request.

A host the vendor assigns per account is declared for every value the vendor
documents, because the image cannot know the launch's seat: copilot names its
service base in the sign-in response and GitHub publishes one host per plan
family, so `agent.copilot` carries all four. Declaring a host a given seat never
contacts is the same reviewed over-declaration as `crates.io` in
`image.rust-dev`, and it keeps a plan change from silently becoming an
undeclared request.

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
`8ca1b712a32f142a29302b04a51e8c3b4a8c21c2`. Sources I and J are the static
artifact inspections of the pinned copilot and agy selections, recorded below.

| Key | Value | Source |
|---|---|---|
| `image.base` | `api.github.com`, `github.com` | the `gh` CLI's API and git-over-https defaults |
| | `pypi.org`, `files.pythonhosted.org` | pip's default index and its file host |
| | `registry.npmjs.org` | npm/pnpm's default registry |
| `agent.claude` | `api.anthropic.com`, `mcp-proxy.anthropic.com`, `platform.claude.com` | evidence rows A/A1: inference, default-enabled claude.ai connectors, OAuth token exchange/refresh |
| `agent.codex` | `api.openai.com`, `auth.openai.com`, `chatgpt.com` | row B: API-key inference, first-party OAuth refresh, subscription inference |
| `agent.opencode` | `models.opencode.ai` | row D, credential-free provider-catalog refresh only |
| `agent.pi` | `pi.dev` | row G: `/model` provider-catalog refresh, credential-free, cached |
| `agent.copilot` | `api.business.githubcopilot.com`, `api.enterprise.githubcopilot.com`, `api.githubcopilot.com`, `api.individual.githubcopilot.com` | source I: the compiled Copilot service base plus the three seat bases GitHub publishes; the CLI's GitHub hosts are `image.base` |
| `agent.agy` | `cloudcode-pa.googleapis.com`, `daily-cloudcode-pa.googleapis.com`, `oauth2.googleapis.com`, `www.googleapis.com` | source J: inference and quota/onboarding on Cloud Code PA and its daily-channel variant, first-party token refresh, the account-validation lookups its literals name |
| `agent.dsh` | *(empty)* | dsh resolves models from an embedded catalog |
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

### I — Copilot 1.0.90 (static artifact inspection)

Inspected the artifact the image actually installs. `@github/copilot-linux-arm64@1.0.90`
from `registry.npmjs.org`, SHA-512 integrity
`sha512-lwc/dL93eacvmm7uTM+QZ5HHUniWaXu+p49rTN1Utg4+dGx6VeBuJ1pg5x4/3MhMeKRPiwFk3WSacazSpOI/nw==`,
tar SHA-256 `19bde7b8274432964337eccb1301611b1cfc9b2993729a9dd3afffb298a1130b`.
The `@github/copilot-linux-x64@1.0.90` artifact was inspected the same way: its
tar SHA-256 `ffb3399216bccc7295cf2e7c579e6f161648fe355f6ec5eeea96ae736c1f30b1`
and its embedded `package/prebuilds/linux-x64/runtime.node` SHA-256
`d60c778cd03ef97cb261abf5f4cb9703f01fc60edfd95ba1f55693a95ae48160` are the two
digests agent-vm's own study records, and `package/app.js` is byte-identical
across the two architectures (SHA-256
`f84061b62eeb1cd78a655555890fe1a776509203fca68144745467e26e443813`), so one
inspection covers both native images.

The binary is a Node SEA whose embedded `package/app.js` and
`prebuilds/*/runtime.node` were extracted and read as text. Findings:

- **`api.githubcopilot.com`** — the compiled Copilot service base, a literal in
  `runtime.node` alongside the session API and the GitHub MCP server path
  `https://api.githubcopilot.com/mcp`. Used when nothing overrides it.
- **the three seat bases** — neither architecture's `runtime.node` carries a
  seat hostname. The CLI reads its base from `settings.copilotUrl`, then
  `COPILOT_API_URL`, then the `CopilotUser.endpoints.api` field of the sign-in
  response fetched from `https://api.github.com/copilot_internal/user`, and only
  then falls back to the compiled literal. The allowlist reference publishes the
  plan families (`*.individual`, `*.business`, `*.enterprise.githubcopilot.com`)
  and calls `*.githubcopilot.com` the host that serves "authorized users
  regardless of Copilot plan"; its sibling re-usable
  [`cloud-agent-required-hosts.md`](https://github.com/github/docs/blob/main/data/reusables/copilot/cloud-agent-required-hosts.md)
  names the exact member of each family — `api.individual`, `api.business` and
  `api.enterprise.githubcopilot.com`, one per plan — which is why the declared
  names are those three and not a wildcard: the grammar cannot express one, and
  the exact members are the vendor's own published names. An enterprise tenant on
  `*.ghe.com` is outside this image's default selection.
- **the GitHub hosts are not repeated here.** `api.github.com` (the sign-in user
  lookup, `copilot_internal/managed_settings`, `copilot_internal/content_exclusion`)
  and `github.com` (device sign-in) are already `image.base`, which every launch
  of this image carries; repeating them under the agent would add no coverage.
- **Excluded.** The MCP servers a user configures, even when one lands on a
  declared host (`GITHUB_MCP_URL_OVERRIDE`, `api.github.com/cmc_internal/…`), the
  agent and MCP catalogue (`agentfinder.github.com`), enterprise identity
  brokering (`login.microsoftonline.com` and the `sts.windows.net` family, reached
  only for an Entra-brokered account), and a tenant host from
  `COPILOT_GH_HOST`/`GH_HOST` — each is a user or tenant choice, exactly as the
  agent-request rule treats them.
- **Absent, not excluded.** The allowlist reference's remaining entries do not
  occur in either artifact: the legacy suggestion proxies
  (`copilot-proxy`, `origin-tracker.githubusercontent.com`), client telemetry
  (`copilot-telemetry.githubusercontent.com`), analytics (`collector.github.com`)
  and the asset hosts (`github.githubassets.com`,
  `avatars.githubusercontent.com`). That is a statement about this CLI build, not
  about the Copilot product, which is why this key is narrower than the document.

This is static inspection: it executed no binary and collected no transport
trace, so it proves which names the artifact carries and what it does with them,
not a credentialed routing of every seat. agent-vm's
[#340](https://github.com/gregwebs/agent-vm/issues/340) still owns a consented
routing study, and its deferral note there stays accurate about what this does not
certify.

### J — agy 1.4.11 (static artifact inspection)

The release archive `agy_cli_linux_arm64.tar.gz` from the committed
[pin](../../images/tools/agy/pin.json), SHA-256
`debb2c056e387be63fdf86d48dfcc99ed1f523e51c5067abd119d78bd36e8a18`, and its
`linux/amd64` sibling `agy_cli_linux_x64.tar.gz`, SHA-256
`d17c426977326361f3d81f3ea22820e967164285923de020247e460dcb9d15c3`. Each archive's
single member `antigravity` is the installed `/usr/local/bin/agy` (SHA-256
`3e9f9909f11ca15e442b0aca839be9ed9e0055f5f18dba622b18cadffebfc666` arm64,
`4341b80b43fc3b4437ff65041673f0f71c950e49d69a6256e3b6f212c4694ed4` amd64). Each
is a stripped Go executable. Their string pools are not identical — the arches
carry their own runtime strings, such as `ld-linux-aarch64` against an x86-64
AES module — so the claim here is narrower: every endpoint literal listed below
was found in both binaries, once unless noted, and the counts match.

The evidence is exactly this: the literals, and the path and RPC names compiled
beside them. It shows which names the artifact carries and what each is for. It
does **not** establish that a given account's launch reaches them, in what order,
or under which condition — a stripped binary has no readable call graph, and the
account paths need credentials this repository will not use. Findings:

- **`cloudcode-pa.googleapis.com`** — the compiled Cloud Code base for the
  Google-account path: inference (`/v1beta/models/…:generateContent`), model
  discovery, quota and onboarding (`LoadCodeAssist`, `OnboardUser`,
  `FetchAvailableModels`), the plugin listing and the URL antivirus check the
  agent's guard calls. The binary reads a `CLOUD_CODE_URL` override; this image
  sets none.
- **`daily-cloudcode-pa.googleapis.com`** — the daily-channel variant of the same
  service, another endpoint literal in the binary. Declared rather than omitted,
  for the same reason `pi-antigravity` declares its daily fallbacks: the channel
  a launch uses is not readable from here, the same family is what that key
  declares, and the cost of a never-used entry is one unused entry rather than a
  missing one. This inspection shows the literal exists, not which account
  reaches it.
- **`oauth2.googleapis.com`** — `/token` for refresh and `/device/code` for the
  device authorization flow, both literals in the binary.
- **`www.googleapis.com`** — the binary's literals for account validation
  (`/oauth2/v2/userinfo`, `/oauth2/v3/tokeninfo`). Its other occurrences are OAuth
  *scope* identifiers (`/auth/drive`, `/auth/cloud-platform`) and the user-invoked
  Drive connector, which is not a default. Declared because account validation is
  in the baseline and these are the only literals that do it; whether a given
  sign-in runs them is one of the things this inspection cannot show.
- **Excluded.** Browser sign-in (`accounts.google.com/o/oauth2/auth` and the
  `antigravity.google/oauth-callback` redirect), which the baseline excludes for
  every agent; telemetry, `play.googleapis.com/log`, which the CLI gates on a
  server setting it reports as "failed to read telemetry setting from server,
  defaulting to disabled" — excluded by the baseline's telemetry rule, which means
  no request label here names it, so a launch whose egress does not cover it loses
  the telemetry rather than being told about it, unless some other source declares
  that host (a layer's own label, or the launcher's catalog); the URL
  guard's direct `safebrowsing.googleapis.com` v5 call, which the binary reports
  as "SafeBrowsing not configured" and no default here configures; API-key and
  Vertex provider modes (`generativelanguage.googleapis.com`,
  `aiplatform.googleapis.com`, `iamcredentials.googleapis.com`,
  `sts.googleapis.com`), which are the user's provider choice; the dev/sandbox
  channel hosts (`preprod-daily-…`, `autopush-….sandbox.googleapis.com`); and the
  updater at `antigravity-cli-auto-updater-….run.app`, which the image disables
  with `AGY_CLI_DISABLE_AUTO_UPDATE=1`.

This is static inspection: no binary was executed, no account was used and no
transport was traced, so it does not prove a login path or any given call order,
and `agy` remains certified only for `--version` (ADR 0012). Read it as the set of
vendor endpoints the reviewed artifact carries for the Google-account path, which
is what this repository accepts for an agent row whose paths need credentials —
not as a certified route.

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

- **Every shipped component is now inventoried.** `agent.copilot` and
  `agent.agy` joined the table after #143, so a launch of this image reports a
  request list for every agent it ships rather than "not inventoried" for two of
  them. Changing what either agent installs, or a new version's default base,
  means re-reading sources I and J and editing the same two files.
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
