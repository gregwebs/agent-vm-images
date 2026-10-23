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

A pinned pnpm install of yaml (committed package.json and lock, installed once into
a build layer) was considered and rejected on footprint, not policy. It would meet
the repository's exact-pin rules and #42's "no npm installation" acceptance, because
yaml@2.9.0 has no dependencies and one tarball. The decisive difference is how
recipe-contract reaches the build: every consumer's RUN step receives it through a
read-only bind mount and leaves no layer behind, whereas an installed node_modules
must occupy a real image layer to be visible to those later, separate RUN steps.
That either ships a build-only dependency in every image or needs removal
bookkeeping the bind-mount pattern exists to avoid; installing it inside each
consumer step instead would repeat network access per consumer. It would also add a
strict-egress route and an install step before host maintenance scripts, which
currently need only node. Recorded here so the option is not silently reopened.

## Consequences

Untrusted YAML needs byte/depth limits, duplicate/tag/anchor rejection and closed
consumer policies. Vendoring costs roughly 480 KiB across 78 files in the build
context and repository history; it costs no runtime bytes, because the directory is
only ever bind-mounted and never COPYed into an image. Pins are consistency
records, not review approval; re-pinning cannot excuse structural/policy failures.
Exact peer checks use each snapshot's actual context, not a matching archive
elsewhere. Fixtures prove primitive
behavior only; later slices must regenerate exact configs and cold-install real
consumers before claiming runtime compatibility. dsh's later graph freeze is
weaker than its current location freeze and requires explicit review.

## Amended by #51 / ADR 0003 (2026-10-18)

The decision text above is the accepted #42 record and stays as written. Two
things in it are now read through this amendment rather than rewritten in place:

- **"Rust checks source/inventory boundaries without Node subprocesses" no longer
  holds for dsh.** The independent `script/test/image-sources` guards still author
  their own vectors, but for the dsh graph they run the reviewed CLI as a syntax
  adapter instead of parsing YAML themselves, because a second parser would be a
  second grammar. Rust checks the CLI's arguments and typed output; the graph
  rules remain Node-owned. The original wording describes PR 1, where no consumer
  used the parser yet.
- **Peer contexts are opaque.** A snapshot key can carry a 32-hex peer-context
  suffix that is an identity, not a readable provider list. Resolution goes
  through the dependency edge target, never by reconstructing the hash (ADR 0003
  § "Peer-context identity rules"). The exact-peer oracle is therefore per
  snapshot and per context, as the Consequences section already required.

The "later graph freeze is weaker than its current location freeze" sentence was
predictive; [ADR 0003](0003-dsh-graph-freeze.md) is that record. Nothing above
widens the parser boundary: it remains data-only, build-only and mounted, never
installed or resolved into an image.

## Amended by #75 / ADR 0004 (2026-10-22)

Status: accepted. The #42 decision and the #51 amendment above stay as written.
This amendment moves the parser to Go and replaces the npm vendor. The trust
boundary is unchanged: the helper is data-only, build-only, bind-mounted and never
installed or resolved into an image. OpenCode's own equal-only checker and pin
writer, the Python dsh installation fixture's pin writer and Rust's Node fixture
repinner remain independent and unchanged.

**What moved.** The bounded parser, closed consumer policies, graph/identity/
exact-peer rules, pins, the ADR 0003 comparator and the Cordis derivation are now
`recipe-contract` subcommands: `pnpm-lock`, `pnpm-environment`, `pnpm-yaml`,
`dsh-lock-update`, `dsh-cordis-overrides`, `dsh-upgrade-stage`. The code lives in
`images/recipe-contract/{pnpmlock,pnpmenv,dshupgrade,internal/document}`.
`pnpm-lock.mjs`, `check-pnpm-environment.mjs`, the three dsh Node adapters and the
npm `vendor/yaml/` (yaml@2.9.0, ISC, 78 files, 480 KiB) were deleted with their
callers in one commit.

**YAML boundary.** `go.yaml.in/yaml/v3` v3.0.5 is imported only by
`internal/document` and used through `yaml.Node`. A source scan rejects every
explicit tag indicator at a node start, including the non-specific `!` that v3
silently discards (`decode.go`), while allowing `!` inside quoted, block, plain and
comment content. The AST walker independently rejects tagged nodes, anchors and
aliases. The policies the retired decoder options expressed stay explicit here:
exactly one document; duplicate keys compared by resolved JavaScript `===`; no
merge key; mapping keys that resolve to strings; depth <= 100; input <= 16 MiB.
Quoted and flow continuation lines indented below the enclosing block are rejected
before v3 can fold them into one scalar. Plain scalars are resolved with our own
YAML 1.2 core-schema tests, not v3's YAML 1.1 tags (`017` is 17, `0b101` is the
string `0b101`).

There is **no tag exception**: the scan and the walker both reject every tag form
the retired parser rejected.

**JSON boundary.** Manifests and reviewed override maps take a bounded custom
RFC 8259 reader instead of yaml.v3, because v3 rejects valid JSON the retired
parser accepted (`\/`, surrogate-pair escapes, quoted flow keys over 1024
characters). Raw strings and keys keep every UTF-16 code unit, including unpaired
surrogates, through JavaScript equality, the ADR 0003 raw-record comparison and
JSON emission. Lone units emit lowercase `\udxxx` escapes; valid pairs emit their
character. No replacement-character normalization is performed for JSON. YAML
escapes are a separate path (D7 below), and invalid raw UTF-8 is rejected before
either parser.

**Provenance (measured 2026-10-22).**

| Item | Value |
|---|---|
| Module | `go.yaml.in/yaml/v3` (the YAML org's continuation of `gopkg.in/yaml.v3`) |
| Version | `v3.0.5`, the only requirement in `go.mod` |
| Zip hash (`go.sum`) | `h1:N6y/pJk8buWs9NY5ERU2HSMfm+IuD/OtfdAnq6kESPw=` |
| go.mod hash | `h1:HVTZu1O7/Vkt2N+BFy8Zza+lnLsABggaTM2ZpNIGuKg=` |
| Closure | 16 upstream files + `vendor/modules.txt`, 338,142 bytes, no transitive requirements |
| Imports | stdlib only: bytes, encoding, encoding/base64, errors, fmt, io, math, reflect, regexp, sort, strconv, strings, sync, time, unicode, unicode/utf8. No cgo, `unsafe`, `os` or `net` |
| License | dual: MIT for the libyaml-derived files (`apic emitterc parserc readerc scannerc writerc yamlh yamlprivateh`), Apache-2.0 for the rest; `NOTICE` retained. Compatible with this repository's Apache-2.0 |

**Advisory review (measured 2026-10-22).** `govulncheck` v1.1.4, source and
symbol level, Go 1.27.3, advisory database last modified 2026-10-21T00:27:29Z: no
findings (the JSON stream contains a config block, an SBOM for
`go.yaml.in/yaml/v3 v3.0.5` and 187 OSV records, and no findings block).
`vuln.go.dev/index/modules.json.gz` lists no advisory for the
`go.yaml.in/yaml/v3` path. The one advisory for the predecessor path is
GO-2022-0603 (CVE-2022-28948, GHSA-hp87-p4gw-j4gq), "panic in Unmarshal", symbol
`yaml.Unmarshal`, fixed in `3.0.0-20220521103104-8f96da9f5d5e`; it names a
different module path, v3.0.5 is a later release than that fix, and this helper
never calls `Unmarshal` (it decodes into a `yaml.Node`), with `recover()` as
defence in depth. `pkg.go.dev/vuln/list` was retrieved for the same review.

**Closure review.** `script/test/recipe-contract-vendor.sh` states the environment
explicitly (`GOENV=off GOWORK=off GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org
GOSUMDB=sum.golang.org` and empty private-module settings) with a fresh
invocation-owned module cache, downloads the pinned module, runs `go mod verify`,
regenerates `vendor` with `go mod vendor -o`, diffs it recursively against the
committed tree, checks `go mod tidy -diff`, and proves the diff can fail with a
mutated copy. Regeneration-plus-diff is the drift gate: `go mod vendor` does not
authenticate edits to a committed tree, and `go mod verify` never reads it. This is
Go checksum-policy verification, not independent upstream attestation; the closure
itself is reviewed in git (the exact file set, `modules.txt`, `go.sum`,
`go.mod`, and the single importing package are pinned by the Rust guards). Builds
use `-mod=vendor` and never fetch: the Docker builder stage, `build.sh` and
`contracts.sh` all set it, so a missing vendor tree fails the build instead of
reaching a proxy. Updates follow CONTRIBUTING's vendor procedure; upstream vendor
files are never hand-edited or reformatted.

**Rust's graph adapter.** Rust executes `recipe-contract pnpm-lock` for graph
syntax; its independent Node repinner is unchanged. Static guards are not
native-execution evidence.

**Compatibility ledger.** The approved exceptions and diagnostic differences are listed below.
D3 (tags), D4 (continuation indentation) and D8 (JSON surrogate handling)
are retired by enforcement and are not exceptions. D2, D5, D6 and D7 are approved
behaviour exceptions under the issue's owner decision 4: each can only reject input
the retired parser accepted, each fails closed with a diagnostic naming the
offending construct, and each has committed test vectors in
`internal/document/testdata/parse-vectors.json` (marked `approval:
approved-by-owner-decision-4`). D20–D25 are the six further classes owner decision 6
approved: each is "the port rejects, the JS accepted", each names the corpus vector
group that proves it, and each is marked `approval: approved-by-owner-decision-6`.
All of them exist because a permissive source transform would be a second grammar,
which this ADR exists to prevent.

| ID | Remaining difference | Status / constraint |
|---|---|---|
| D1 | Node-version-specific assertion diffs become stable expected/got policy messages | diagnostics only; exact prefixes and statuses preserved |
| D2 | `%YAML`/`%TAG` directives accepted by the retired parser are rejected (v3 rejects `%YAML 1.2` and does not expose directives) | **approved exception**; diagnostic names directives |
| D5 | raw NEL/LS/PS (U+0085/U+2028/U+2029) rejected anywhere in the source, including comment content, instead of treated as content | **approved exception**; v3 turns them into line breaks or folded spaces, so the value cannot be recovered |
| D6 | raw disallowed C0/DEL/C1/U+FFFE in YAML scalars rejected | **approved exception**; v3's reader rejects them before a node exists (U+FFFF is D22; TAB/LF/CR/NEL are separate cases) |
| D7 | YAML `\/`, surrogate escapes and flow keys longer than 1024 source characters rejected | **approved exception**; JSON inputs are lossless, YAML lone surrogates are never silently repaired |
| D9 | YAML syntax/warning text uses v3/policy diagnostics, not npm code frames | status 1 and the command prefix preserved; no warning is silently ignored |
| D10 | invalid JSON uses `invalid JSON: reason at byte N`, not V8 text | status unchanged; lone CR outside strings fails closed under D20 |
| D11 | filesystem diagnostics keep the Node errno/operation/path shape with Go errno descriptions | matched code and prefix preserved; libuv prose may differ |
| D12 | failed stdout write gives status 1 and one line, not an async Node EPIPE stack | deterministic failure, never success with dropped output |
| D13 | every process-environment entry is checked, not Node's one-per-key view | stricter duplicate-entry handling |
| D14 | project/kind/mode constructors replace internal assertions | CLI vectors remain; snapshot/context identity fields are not yet opaque (follow-up typing refactor) |
| D15 | contract-directory authorization moves from the retired adapter to the existing filesystem-authorized caller | the fixed production directory rule stays tested in `dsh-project.py` |
| D16 | usage text names `recipe-contract` subcommands instead of retired file names | status 2 preserved; every caller moved in the same commit |
| D17 | developer workspace bytes use v3 style instead of the npm formatter | fail-closed re-parse and deep-equality guard; published lock bytes are never re-encoded; disposable derivation copies use block YAML to avoid D7 |
| D18 | the strip-cordis test fixture writes JSON instead of block YAML | fixture-only; values unchanged (JSON is YAML, subject to D7) |
| D19 | a forced temporary-name collision does not unlink a pre-existing pin temp | cleanup only removes invocation-owned files; explicitly tested |
| D20 | lone CR (U+000D) outside JSON strings and anywhere in a YAML source: the port rejects, the JS accepted (`F4-CR`, `YAML-CR`) | **approved exception (owner decision 6)**; npm yaml keeps a lone CR as content in some positions while v3 normalizes it to a line break, so recovering the JS value needs a permissive source transform, a second grammar |
| D21 | block-scalar body at EOF without a line break, over-indented whitespace-only line and column-zero comment content: the port rejects, the JS accepted (`F5`) | **approved exception (owner decision 6)**; v3 has no node for these bodies, so the value could only come from a hand-written block-scalar grammar |
| D22 | empty end-marker streams, root column-zero block bodies, tab-only lines, tab-indented comments, tabs at block-body indentation, BOM after document end, raw D2/D5/D6 characters and U+FFFF in comments, raw U+FFFF in scalars, and unterminated quotes at end of input in any context: the port rejects, the JS accepted (`F6`, non-explicit-key rows) | **approved exception (owner decision 6)**; npm's lexer states and line-start rules differ from v3's, and a source transformer for them would be a second grammar |
| D23 | explicit-key trailing content the npm parser silently ignored: the port rejects, the JS accepted (`F6`, explicit-key rows) | **approved exception (owner decision 6)**; reproducing the quirk means re-implementing npm's explicit-key tail handling |
| D24 | tabs before scalar or flow-collection content (including empty sequence entries and block headers), short multiline flow keys, indented explicit value indicators, and comment-separated block header/body indentation in compact mappings: the port rejects, the JS accepted (`R2-2`) | **approved exception (owner decision 6)**; npm-yaml's lenient grammar is not re-implementable without a second parser, which this ADR forbids |
| D25 | developer proof JSON with invalid UTF-8: the port rejects, the JS accepted (Node's `readFileSync(file, 'utf8')` replaced malformed bytes with U+FFFD) | **approved exception (owner decision 6)**; replacement decoding would silently change reviewed bytes; pinned by `TestProofInvalidUTF8Rejected`, not a corpus row |

The D2/D5–D7 counterexamples show limitations of the selected decoder and node
walker, not a proof that a separately reviewed source transform is impossible.
That work must not become a regex or hand-written YAML subset replacing the
selected node parser.

**Alternatives.** Retaining Node leaves these build helpers untyped and keeps a
second language in the build path for one YAML library. Decoding directly into Go
maps loses the duplicate/tag/anchor seam and the UTF-16 string identity. Hand-written
YAML parsing remains rejected for #42's reasons. The owner selected this module,
committed `go mod vendor` output and this amendment form; this record does not
re-open those choices.

**Evidence.** Commands, versions, counts and hashes below were run from the
repository root on 2026-10-22 (macOS arm64, Go 1.27.3, Node 26.12.0, Python 3.15.0)
unless a row says otherwise. Machine-readable copies live under
`target/ci-logs/avm-75/` (git-ignored) and are historical session evidence, not a reproducible checkout gate.
Rows span additive commit `6f2383b` and cut-over commit `927abca`; row 1 is not
the source revision for the later cut-over checks. The temporary row 7 harness
is not maintained. Use `node script/test/recipe-contract-oracle.mjs` to replay
the committed parser corpus against the retired oracle.

| # | Command | Result |
|---|---|---|
| 1 | `(date -u +%FT%TZ; git rev-parse HEAD; go version; node --version; python3 --version)` | `2026-10-22T23:16:02Z`, `6f2383b`, go1.26.5 host toolchain resolving go1.27.3, node v26.12.0, Python 3.15.0 |
| 2 | `govulncheck@v1.1.4 -json ./...` in the module (pinned env as above) | status 0, no findings, 187 OSV records, db 2026-10-21 |
| 3 | `vuln.go.dev` modules index + `GO-2022-0603.json` + `pkg.go.dev/vuln/list` | one advisory for the predecessor path only (see above); none for `go.yaml.in/yaml/v3` |
| 4 | `cat go.mod go.sum vendor/modules.txt; find vendor -type f; wc -c vendor/...` | 17 files, 338,142 bytes, two `go.sum` lines, one `require` |
| 5 | `bash script/test/recipe-contract-vendor.sh` | `go.sum verified, vendor identical, tidy clean, mutant rejected` |
| 6 | `go test -run '^$' -bench '^BenchmarkParseYAML$' -benchtime=3x -benchmem ./internal/document` | `Scalar16MiB` ≈248 ms/op, 224 MB allocated, 125 allocs; `CommittedDshLock` ≈126 ms/op (measurements, not a runtime bound; the earlier "~12 ms" claim is not reproduced) |
| 7 | `python3 /tmp/avm-75/parity/compare.py --oracle … --binary …` | 98 exact, 83 diagnostic-only, 0 failures over every fixture × verb × kind, the environment matrix, `dsh-lock-update` and the derivation |
| 8 | `node script/test/pnpm-lock-inputs.mjs` | both PASS lines, including the B2 raw-manifest refresh regression |
| 9 | `node script/test/pnpm-migration-ledger.mjs --check` | matches its generator; the regenerated ledger differs only in `provenance.graphReader` |
| 10 | `python3 script/test/dsh-project.py`, `bash script/test/dsh-prepare-lock.sh`, `bash script/test/dsh-verify.sh`, `/bin/bash script/test/upgrade-scripts.sh` | all pass |
| 11 | `(cd script/test/image-sources && RUSTUP_TOOLCHAIN=1.98.1 cargo test --locked && cargo fmt --check && cargo clippy --locked --all-targets -- -D warnings)` | 30 tests pass, fmt clean, no warnings |
| 12 | `actionlint .github/workflows/*.yml` | clean |
| 13 | `bash script/test/contracts.sh --guard-only` | passes (30 Rust tests included) |
| 14 | `bash script/test/contracts.sh` | not run to completion on this host: it stops at `ci-boot-runtime.sh` (pre-existing macOS limitation, #77). Every later suite was run individually (rows 8–12) |
| 15 | `docker build --platform linux/arm64 --network=none --target recipe-contract-bin -f images/Dockerfile images` | succeeds: the builder stage compiles with no network, so it needs no module proxy |
| 16 | path (a) vs (b): the stage's `/out/recipe-contract` against `bash script/build/recipe-contract.sh --os linux --arch arm64` | `shasum -a 256` and `cmp` agree: `3eacc3b9da36da6b4eddb2dc31b9bd5993df5ee10989b7bbf546ce554735474d` |
| 17 | `git grep -nF -e pnpm-lock.mjs -e check-pnpm-environment.mjs -e check-lock-update.js -e derive-cordis-overrides.mjs -e upgrade-project.mjs -e read-workspace.mjs -e pnpm-vendor.mjs -e vendor/yaml` | only the ADR text, the ledger's `RECORDED` history, `render-released-agent-probe.py:125` and the Rust deletion/boundary guards |
| 18 | native acceptance (`standard-node-consumers.sh`, `standard-node-policy.sh`, `standard-image.sh` against a built base) | not run in this pass: no base image was built. Their self-tests pass (row 13); CI `build-local`/native rows remain the acceptance evidence |
| 19 | `go build ./...` with `GOPROXY=off` inside the module | succeeds: the committed vendor tree is the whole closure |

### Owner decision 6 approvals (review follow-up)

Owner decision 6 approved the stricter-than-JS classes the review follow-up
inventoried (88 corpus vectors plus the proof test) as fail-closed exceptions:
lone CR anywhere in YAML (`YAML-CR`) or outside JSON strings (`F4-CR`);
block-scalar EOF, over-indented whitespace-only and column-zero comment bodies
(`F5`); empty end-marker streams, column-zero root block bodies, tab-only lines,
tab-indented comments, tabs at block-body indentation, BOM after document end, raw
D2/D5/D6 characters in comments, raw U+FFFF, and unterminated quotes at end of
input in any context (root, sequence entry or mapping value), plus explicit-key
trailing-content quirks (`F6`); tabs before scalar or flow-collection content
(including empty sequence entries and block headers), short multiline flow keys,
indented explicit value indicators and comment-separated block header/body
indentation in compact mappings (`R2-2`). Proof invalid UTF-8 is `D25`.

Each class rejects only input the retired parser accepted; nothing in the port
accepts input the retired parser rejected. Corpus rows keep their oracle-derived
JS values and carry `approval: approved-by-owner-decision-6`, so the accepted JS
outcome stays visible next to the rejection. The inventory is measured (the
committed corpus, `script/test/recipe-contract-oracle.mjs` and the differential
harnesses behind it), not a grammar-equivalence or completeness proof: any further
difference still stops the work and is reported.
