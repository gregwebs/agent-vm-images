# ADR 0008: build the Go example's gopls from a committed, reviewed module closure

Status: accepted (#103).

## Context

`examples/layers/go-dev` provided gopls with
`go install golang.org/x/tools/gopls@v0.23.0`. The version string identifies no
bytes: the `go` command resolved gopls's module graph at build time and accepted
whatever content hashes the checksum database returned *during that build*.
Nothing committed in this repository said which modules the image compiles, and
nothing reviewed those sums, so the build — not a maintainer — decided what went
into the language server. The [image input policy](../image-input-policy.md)
requires a reviewed selection and content identity for every pinned image input,
and exact versions alone do not satisfy it.

Two properties of the Go module system matter for what a fix can honestly claim:

- `go.sum` is the lockfile analogue. It records an `h1:` hash of each module
  zip's content and of each module's `go.mod`. When a hash is missing, `go`
  consults the checksum database (sum.golang.org), an append-only Merkle log
  whose signed tree heads verify against a key compiled into the `go` command.
  The log establishes **consistency** — everyone sees the same bytes for
  `path@version` — not that the publisher approved them.
- Module content therefore has a real authenticated channel (log + committed
  sums), and it is not a source-code signature. Release tags and commits of some
  selected modules are signed; those signatures are not what the module system
  verifies, and binding a signed tag to a module zip's bytes is a separate piece
  of work.

## Decision

Commit the selection, then let one deep helper compile from it.

- `examples/layers/go-dev/gopls-closure/` holds a tiny wrapper Go module
  (`module agent-vm.invalid/go-dev/gopls-closure`, `tool golang.org/x/tools/gopls`,
  every selected module as an indirect requirement), the full tidy `go.sum`, the
  root module's own upstream `go.mod` bytes as evidence, a `THIRD_PARTY_LICENSES`
  transport of the reviewed notice files, and `pin.json` binding them. For
  v0.23.0 the selection is 23 modules (gopls plus 22 dependencies), 34 zip `h1`
  lines and 101 `/go.mod` lines, and 38 notice files.
- Vendoring was rejected: it commits 25 MB / 1,774 files of third-party source,
  and `go build` does not authenticate a vendored tree, so a networked drift gate
  would still be needed. A bespoke sums file would reimplement Go's resolver, and
  a committed prebuilt gopls would be an unreviewed binary.
- Ordinary builds run exactly one deep helper,
  `examples/layers/go-dev/gopls_closure.py`, through a thin adapter
  (`install-gopls.sh`). The helper:
  1. checks the committed data **offline** — exact file set, per-file digests,
     canonical `go.mod`, both checksum kinds for every pinned module, the root
     `go.mod` evidence hash, the reviewed notice sections, the record's review
     scope, and the current-pin adoption restriction below;
  2. runs exactly one `go build -mod=readonly -trimpath -buildvcs=false` inside an
     environment it **constructs** (never inherits);
  3. accepts the output only if the binary's own build info reports the pinned
     compiler, the pinned target and exactly the reviewed modules and sums;
  4. only then publishes `gopls` (0755) and the third-party licence file, and
     withdraws them again if any later step fails.
- The child environment is a closed 21-key allowlist. `PATH` is the pinned Go
  root only; `HOME`/`GOPATH`/`GOMODCACHE`/`GOCACHE`/`TMPDIR` are
  invocation-private scratch removed afterwards; `GOENV=off`, `GOWORK=off`,
  `GOFLAGS` empty, `GOTOOLCHAIN=local`, `GOPROXY=https://proxy.golang.org` with no
  `,direct`, `GOSUMDB` set to the full verifier key
  `sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8`, no
  `GONOSUMDB`/`GONOPROXY`/`GOPRIVATE`/`GOINSECURE`, `GOAUTH=off`, `GOVCS=*:off`,
  `CGO_ENABLED=0`, `GOOS=linux` and `GOARCH` from the validated image
  architecture. Only transport variables (`HTTP_PROXY`, `HTTPS_PROXY`,
  `NO_PROXY`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, and lowercase forms) may be added
  in production, because the bytes are hash-verified anyway.
- The `GoCommand` enum is closed to four members (`env GOVERSION`, `env GOROOT`,
  `build`, `version -m -json`): a `go get`, `go install`, `go mod tidy`,
  `go mod download` or `go mod vendor` is not expressible in the ordinary build
  path at all.
- `CGO_ENABLED=0` applies to gopls only. The guest keeps `build-essential` so a
  project can use cgo and `go test -race`.
- **Explicit Linux target.** Every Go invocation — including the build-info
  inspection — runs with `GOOS=linux` and the image's `GOARCH`, so a Darwin
  maintainer host cannot silently produce a Darwin binary, and the reviewed Linux
  build settings are checked as an exact map (`-buildmode=exe`, `-compiler=gc`,
  `-trimpath=true`, `CGO_ENABLED=0`, `GOOS`, `GOARCH`, the architecture's
  `GOAMD64=v1`/`GOARM64=v8.0`, and Go 1.27.3's `DefaultGODEBUG`). An unknown key
  or a changed value is refused: the map is a compatibility tripwire, not an
  authentication of every compiler input.
- **The current-pin adoption restriction.** Every module the wrapper selects must
  be named by the root module's own `go.mod` at the same version, and the
  post-build module set must equal the wrapper's requirement set exactly. This is
  a reviewed restriction of *this* record, not a general MVS invariant: a
  legitimate root → dep → leaf selection that Go's resolver would accept is
  refused, with the module named and a remediation that says to inspect the
  upstream `go.mod`, update the rule and the ADR in a reviewed proposal. The
  closure proof is the committed selection, both checksum kinds, isolated
  readonly resolution, and exact post-build identity on both Linux targets.
- Maintenance is `script/dev/upgrade-gopls.sh VERSION [--review-determination
  YYYY-MM-DD --reviewer NAME]` plus its stager `script/dev/gopls-closure-update.py`.
  The stager regenerates the graph and sums with the layer's **pinned Go**
  (`GOTOOLCHAIN=go<GO_VERSION>`, asserted via `go env GOVERSION`), fetches and
  log-verifies every new sum under the explicit key, cross-builds both Linux
  targets from the staged files with `GOPROXY=off`, transports the licence
  evidence, prints an auditable proposal (module table diff, per-line `go.sum`
  delta, notice and signature-inventory deltas, scope digests) and publishes the
  fixed six-destination list as one transaction with the existing publisher.
  Statuses are 0/1/2/3/128+N, `GITHUB_ACTIONS=true` is refused, and the writer
  never commits and never treats its own success as approval.
- **Review renewal.** The record carries a scoped, dated determination
  (`determination = "module-channel-sumdb-v1"`, `reviewed_on`, `reviewer`,
  `scope.root/go.mod/go.sum`). A change to the root, the sums, the reviewed
  notice layout, the signature inventory or the policy requires a renewed review;
  the writer prints `module-channel determination requires renewed review` with
  the old and new identity and publishes nothing until a human supplies the date
  they actually reviewed it. A refusal also retains the exact proposed bytes
  outside the checkout, and a later approved run can be bound to them
  (`--proposal DIR`), so an approval cannot silently cover a different proposal.

### Authentication evidence (what is and is not verified)

For this closure the reviewed authentication channel is sum.golang.org under the
explicit verifier key plus the committed `go.sum`. Maintenance-time sums are
fetched through that log; ordinary builds accept only committed sums and never
consult the log, a `@v/list` or a `@latest` endpoint.

VCS release signatures were inventoried against the *selected* versions and are
retained as evidence only. They are not verified here, and they are not used as
module-content authentication: `vcs_signature_policy` is
`inventoried-not-used-for-module-content` and every record has
`"verified": false`.

| Selected module | Upstream evidence | Observed object (not verified here) |
|---|---|---|
| `github.com/modelcontextprotocol/go-sdk@v1.6.0` | https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.6.0 | SSH-signed annotated tag `2c142340a561f6699d4d94d7774e1788c0381d3f`, peeled commit `f5f2015413f17f578fbacc839d2c05d0fdb3f1cc` (also signed) |
| `mvdan.cc/gofumpt@v0.10.0` | https://github.com/mvdan/gofumpt/releases/tag/v0.10.0 | SSH-signed annotated tag `dfdc8f312f71cb1ae35fd355e2862b4ac3132e9e`, peeled unsigned commit `14050252a6d591bb7a3f0167c925c3d6e6f9bdf8` |
| `github.com/fsnotify/fsnotify@v1.9.0` | https://github.com/fsnotify/fsnotify/releases/tag/v1.9.0 | unsigned annotated tag `09cce6e5c9ddf78d3dc5bbf3d823ab4d5a2e2c40`, peeled SSH-signed commit `ae0e7923765f64fb8061396db7edebb558cf6093` |

Reproduce a row in invocation-owned scratch (substitute repository/tag):

```sh
git clone --bare --depth=1 --branch TAG https://github.com/REPO.git DIR
git --git-dir=DIR cat-file -t refs/tags/TAG
git --git-dir=DIR cat-file -p refs/tags/TAG
git --git-dir=DIR cat-file -p 'refs/tags/TAG^{}'
```

Anyone tempted to shortcut this: do not run `verify-tag` against an unreviewed
key and call the result publisher authentication. This inventory is partial by
design — an unlisted module carries no absence claim — and it must be
re-inventoried on a version bump rather than reused. Trusted identities and the
binding from a signed tag to module-zip content are the follow-up below.

Provenance is the proxy's `.info` `Origin` (`vcs`, `url`, `subdir`, `ref`,
`hash`), labelled `"authenticated": false`: it is unauthenticated metadata that
agrees with the selection, not signed provenance.

### Retained notice evidence

`THIRD_PARTY_LICENSES` transports the exact bytes of every reviewed notice file,
framed by a header carrying the section's byte count and SHA-256, and the helper
compares the set and the bytes against the module cache before publishing. All 23
modules have a root notice; six additional nested notices are retained
deliberately:

| Module | Root files | Additional retained nested evidence |
|---|---|---|
| `github.com/BurntSushi/toml@v1.6.0` | `COPYING` | `cmd/toml-test-decoder/COPYING`, `cmd/toml-test-encoder/COPYING`, `cmd/tomlv/COPYING` |
| `github.com/segmentio/encoding@v0.5.4` | `LICENSE` | `json/fuzz/LICENSE` (retained although the path may not be linked) |
| `honnef.co/go/tools@v0.8.0-rc.1` | `LICENSE`, `LICENSE-THIRD-PARTY` | `go/gcsizes/LICENSE`, `go/ir/LICENSE` |
| `github.com/fatih/camelcase@v1.0.0` | `LICENSE.md` | — |
| `mvdan.cc/gofumpt@v0.10.0` | `LICENSE`, `LICENSE.google` | — |
| `golang.org/x/{mod,sync,sys,telemetry,text,tools,vuln}` | `LICENSE`, `PATENTS` | — |
| all other selected modules (including gopls) | `LICENSE` | — |

A regex is never asserted to be legally exhaustive: this transports evidence, and
adoption review must also read copied/generated code and non-matching notices. A
notice layout outside this inventory, a missing root notice or a symlink stops
maintenance for review instead of being silently accepted with
`--review-determination`.

### Audit boundary: what the record binds, and what it cannot

`pin.json` binds *build inputs* by digest: the five closure files
(`files`), both checksum kinds per module (`modules[].h1`/`go_mod_h1`), the
reviewed scope digests, and the notice bytes through their per-module identity.
Those are what an ordinary build re-checks offline, and the checks are what make
the committed bytes the reviewed bytes.

The record cannot bind *itself*. Its own human-facing metadata — `reviewed_on`,
`reviewer`, the `reason`/policy text, the evidence URLs, the provenance block and
the per-module signature inventory as text — is structure-checked (shape, one
consistent policy, the required scope keys) and reviewed as a diff, but no digest
inside the same file can authenticate it: a copy of the record with a backdated
`reviewed_on`, a changed `reviewer` or a swapped evidence URL still passes the
checker. The control there is code review plus git history, not a cryptographic
one, and no self-digest is claimed (adding one would only move the same problem
into the field that holds it).

The maintenance carry predicate bounds the practical drift: it refuses to carry a
review across a change to the graph, the sums, the notice identity, the signature
inventory or any policy/determination constant, so an edited record that matters
for a build stops a writer run. It does **not** compare `reviewed_on`, `reviewer`
or the provenance block, so a doctored date or attribution would be carried
forward into the next record with the rest of the scope unchanged. Audit-trail
integrity of those fields needs an external anchor (a signed release, a commit
signature, a detached digest kept outside the repository), which this record does
not claim.

## Consequences

- A withdrawn module, an unreachable proxy or changed upstream bytes fail the
  build. There is no fallback origin and no mirror: availability depends on
  `proxy.golang.org`.
- A Go version bump forces a writer re-run: `pin.json.go` and the Dockerfile's
  `ARG GO_VERSION` are checked for equality, and the build helper's reviewed
  Linux build-settings map is version-specific. **Recorded deviation:** the plan's
  sentence that a Go-only bump carries the review is therefore not satisfied as
  written — a compiler bump changes the reviewed settings allowlist, so it
  requires a deliberate code change and a renewed review before any writer run
  can accept it. The carry predicate itself still exists and is exercised case by
  case: `_carried` in `script/dev/gopls-closure-update.py` is driven by
  `test_carry_matrix` in `script/test/gopls-closure-pins.py` and by the
  maintenance suite's Go-only-bump case, and the maintenance suite measures the
  negative with a second real compiler (go1.26.5), which the settings tripwire
  refuses.
- What a build proves: the committed bytes are the reviewed ones, the compiler and
  target are the pinned ones, and the binary's own build info matches the
  selection. What it does not prove: that the release is safe, that the publisher
  authorised the bytes, or that the transported notices are legally sufficient.
- A guest that runs `go get`, `go install` or a project module download is
  outside the guarantee — those are user/project actions. `GOTOOLCHAIN=local` in
  the image bounds the *toolchain* half of that: a module asking for a newer
  toolchain fails locally instead of downloading one (and the native harness
  observes exactly that through a canary proxy).
- **AC3 is delivered closure-scoped, not as originally worded.** Retained archive
  authentication (the Go `.asc` and golangci-lint attestations) and a per-module
  VCS trusted-signer/zip-binding policy remain owned by
  https://github.com/gregwebs/agent-vm-images/issues/115, filed as a #94
  sub-issue under the owner's amendment on #103. Preserving the four archive
  SHA-256 ARGs (AC2) does not cover or excuse that remaining work.
- The image-source helper-inventory gate has a pre-existing defect
  (https://github.com/gregwebs/agent-vm-images/issues/116): its maintained-helper
  scan currently yields no entries, so a green `cargo test` does **not** certify
  this helper. This change relies on the explicit recipe checks, the helper's own
  schema/environment tests, the real-Go sandbox suites and manual security review
  instead. Do not cite that gate as helper coverage until #116 is fixed and
  `docs/image-input-policy.md` says so.

## Alternatives rejected

- **Keep `go install pkg@version`**: the gap itself — the graph and its sums are
  chosen at build time.
- **Vendor the closure**: 25 MB of third-party source, and `go build` does not
  authenticate vendored bytes, so a networked drift gate would remain.
- **A bespoke lock of sums, or `GOPROXY=off` with a committed module cache**:
  reimplements Go's resolver, or ships a cache nobody reviews.
- **A committed prebuilt gopls**: an unreviewed binary, and it abandons
  "compiled from the selected sources".
- **A Go-written checker**: examples keep their build helpers in Python (ADR
  0004), and the stager must share the model with the checker.
- **A generic artifact-pin framework**: same reasoning as ADR 0007 — the Node/gh
  schema models origins, sizes and payloads, while this needs module-graph
  semantics.

## Verification

Hermetic, real `go`, inside the unchanged OS network boundary:

```sh
python3 script/test/gopls-closure-pins.py --go-root /abs/native/go1.27.3
python3 script/test/gopls-closure-maintenance.py --go-root /abs/native/go1.27.3
python3 script/test/fixtures/gopls-closure/sandbox-feasibility.py --go-root /abs/native/go1.27.3
```

Static: `bash script/test/example-layers.sh` and
`bash script/test/example-layers.sh --self-test` (103 cases, including the
numeric label-sequence guard), plus the repository-wide `contracts.sh`. Native
evidence: `bash script/test/go-example.sh --platform linux/arm64|linux/amd64
--no-cache` on a native daemon, whose phases are the build with the retained
archive `sha256sum -c` lines, the offline verifier, the Go loop as root and as
uid 12345, a real LSP session, the first-use canary legs and the hard
content-replacement negative. Permanent example CI stays with #33.
