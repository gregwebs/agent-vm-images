# 0017: Programs are Go; shell is for entry points and automation

Status: accepted. Amends [ADR 0004](0004-go-build-helpers.md) (layout, delivery,
parity bar). Being implemented.

## Context

ADR 0004 and its #75 amendment moved the recipe-contract build helpers to Go.
About 6.4k lines of Python still run as build helpers (pin verifiers, the pnpm
project module, the Pi helpers, the OpenCode probe). Another 3.5k run
in the example layers, 7.5k are maintainer programs (pin writers among them,
some importing build helper modules), 4k are release and CI code, and 75k make
up the test harness. Much of the shell does parsing and policy as well as
orchestration.

## Decision

**Every program in the repository is Go.** That covers build helpers, pin
writers, release, CI and the test harness. Two kinds of code stay as they are:

- **Shell** is for entry points and automation: running commands in sequence,
  passing arguments and handling exit statuses. Simple transforms of data from
  external systems are fine (a jq filter, `awk '{print $2}'`, a sed
  substitution). Parsing documents, policy decisions, loops over records and
  helper functions beyond `fail`/`usage` belong in Go. The `install-*.sh` and
  `verify-*.sh` recipe entry points remain thin shell, so the recipe contract
  is unchanged.
- **Node runtime probes** (`check-runtime.mjs`, `check-claude-runtime.mjs`)
  stay Node because they exist to exercise Node. The other `.mjs` files are
  ported after the Python.

**Layout.** One module rooted at the repository
(`github.com/gregwebs/agent-vm-images`) owns `go.mod`, `go.sum`, the vendored
closure and the Go version. Shared code lives under `./internal`. Each `main`
package lives beside what uses it: `images/recipe-contract` keeps the build
helper subcommands, and the host has one binary per audience (`avm-pin` for
pin writers, plus `avm-release`, `avm-ci` and `avm-test`). Maintainers and CI
invoke host binaries through `.sh` wrappers at stable paths that run
`go run`. The `images/recipe-contract` module is merged into the root module.

**Delivery to product images is the host cross-compile only.** The
`golang:` builder stage that ADR 0004 put in both product Dockerfiles is
removed. `images/build.sh`, CI and the release build always run
`script/build/recipe-contract.sh` (`CGO_ENABLED=0`, `-trimpath
-buildvcs=false`) and pass the result as `--build-context
recipe-contract-bin=…`. `images/` stays the Docker context. Each Dockerfile
keeps a placeholder `FROM scratch AS recipe-contract-bin` stage. Without it,
an unsupplied `--mount=from=recipe-contract-bin` would resolve to the registry
image `docker.io/library/recipe-contract-bin`. The first consuming `RUN` fails
with a message naming `images/build.sh`.

**Example layers stay self-contained.** Users build them with
`agent-vm image build examples/layers/X`, and agent-vm has no
`--build-context` flag. So each example's helper is its own Go module in its
directory, built by the example's own pinned `golang:` stage. The stage is the
recipe's first `FROM`, compiles with `--network=none`, `GOFLAGS=-mod=vendor` and
`GOTOOLCHAIN=local`, and its binary is mounted read-only into the consuming
`RUN`s rather than copied. The committed vendor tree is the build's dependency
closure; `-mod=vendor` is policy, because Go selects vendor mode by default when
`vendor/modules.txt` exists. What authenticates those bytes is the module's own
regeneration-and-diff gate, `script/test/example-helper-vendor.sh`, which runs in
the default contracts mode and not in `--guard-only`. Deleting `vendor/` is a
missing-closure witness, not a proof of vendoring. A guard ties that image to the root `go.mod`
toolchain, in five links. It binds a compiler version and the recipe/inventory
relationship — **not source bytes**: a comment-only edit under `helper/` changes
nothing any gate sees, and binding source bytes for a product image is
[#233](https://github.com/gregwebs/agent-vm-images/issues/233)'s separate question.

1. the stage's own `RUN`: the image compiler must be the helper `go.mod`'s
   `toolchain` line, or the build fails;
2. `script/test/example-layers.sh`: the helper `go.mod`'s `go` and `toolchain`
   lines must equal the root `go.mod`'s;
3. `script/test/example-layers.sh`: the builder `FROM` must be exactly
   `golang:<root toolchain>-trixie` at the digest `images/parent-pins.json`
   records for `.pins["golang-builder"]`;
4. `script/test/image-sources` (`check_recipe_parents`): the same `FROM` must be
   the reference the parent inventory binds to the stage;
5. from #187 slice 4: every requirement the two `go.mod` files share is at the
   same version, so MVS cannot raise a version in the root and make `avm-pin`
   link a different decoder than the image compiles.

The compiler pin is a parent image like the base: `images/parent-pins.json`
carries its digest, its platform manifests and their reviewed provenance, and
`script/dev/refresh-parent-pins.sh golang-builder` refreshes it. Its *version* is
not a selection: the refresher derives the tag from the **root `go.mod`
toolchain**, so a compiler bump is a coordinated edit of `go.mod`, the inventory
and the recipe, and link 3 fails until all three agree. The build reads only those
committed bytes — the recipe's digest and the pinned `FROM` — and resolves nothing
at build time.

Logic shared with `./internal` (OpenPGP, archive hashes) is duplicated
deliberately. When agent-vm accepts named build contexts
([agent-vm#627](https://github.com/gregwebs/agent-vm/issues/627)), examples can
move to the host cross-compile.

**Fixture seams in a shipped binary are a build tag, not a flag.** A ported
example helper must expose suite-only inputs — the reviewed trust it checks
against, the upstream it fetches from, the program it runs a bootstrap with —
without letting the binary an image ships honour any of them. So those inputs
live behind `//go:build example_fixture`: the fixture grammar, the seam values
and the code that reads them are compiled only into a build that says so on the
command line (`script/build/example-helper.sh --fixture`). A seam that exists
only in a differently built binary is stronger than a flag a build argument
could reach, and the layer's builder stage passes no tags at all, which
`script/test/example-layers.sh` asserts. The shipped binary's own test builds
both variants and requires every seam name to be absent from the shipped bytes
and from its grammar, so a seam that leaked is caught even if nothing calls it;
the test fails if the fixture build stops carrying the names, so the absence
check cannot pass vacuously. `avm-pin`'s plain "suites only" flags remain
acceptable only because `avm-pin` never ships.

**Parity bar.** A port keeps exit status and meaning. The existing harness runs
against the Go program and is the oracle. Output text is frozen only where a
consumer parses it. ADR 0004's byte-level differential parity is not required
for new ports.

**A ported reader has two implementations until the Python is deleted.** `#187
slice 2` moved the rust-dev layer's `check` and `verify-installed` to its Go
helper while `rust_toolchain.py` stayed in the build (the install leg is #231 and
the writer is #232). Two implementations of one rule set is a hazard, so it is
held as a *differential*: both readers run over the same bytes on every host gate
and every image build, and a disagreement fails the gate. Python is the oracle,
never a fallback; nothing chooses between them at run time. Concretely,
`script/test/example-layers.sh` runs both over the committed pin directory and
over each of its self-test mutants, the pins suite runs both in-process (the
fixture trust) and through the two real CLIs (the reviewed trust), and the image
build runs Python's `load_pin` inside `install` and Go's `verify-installed` in the
verifier over the same bytes. A differential disagreement is a stop reported on
the issue, never reconciled in code.

**A TOML decoder is chosen by measurement, not by name.** The pin reader's TOML
target is **TOML 1.0 with strict table semantics**, because that is what decides
the bytes in a build: Debian trixie's Python 3.13 in the image, Python 3.12 in CI,
and rustup's own parser. The vendored decoder is `pelletier/go-toml/v2` v2.2.4.
Measured on the nine grammar vectors, every release from v2.0.8 to v2.4.3 matches
TOML 1.0 with strict tables on all of them but one: they accept the TOML 1.1
`\e` escape. `BurntSushi/toml` was rejected on measurement rather than taste — it
lets a later key extend an already-sealed inline table, which is a *semantic*
acceptance, and v1.4.0 also switches to TOML 1.1 when `BURNTSUSHI_TOML_110` is in
the environment.

The `\e` acceptance is a declared divergence, and
`TestTOMLGrammar/escape-e` pins the behaviour it accepts (the document decodes, and
the value is `U+001B`) rather than describing it. The suite's oracle is probed,
never version-sniffed: `tomllib` is TOML 1.1 from CPython 3.15, so a 3.15 host
compares the five 1.1-only rows against the measured TOML 1.0 column and prints how
many rows it compared and against which grammar, making a thin run visible. The
same measurement found a second, independent oracle deviation, ruled **P5** on
#230: `tomllib` accepts an integer beyond ±2^63, which TOML 1.0 requires to be an
error. Go refuses it, which is also what the producer does — `settings.toml` is
written and read by `rustup`, through Rust's int64 `toml` crate — so the Go
reader agrees with the program that opens the file and the oracle is the odd one
out. P5 is its own entry because P4 was scoped to grammar that varies by Python
version; this does not vary. A declared divergence must fail when it stops being
true, so the row asserts both directions: Go refuses at the int64 limit *and* the
oracle still accepts, so the day `tomllib` enforces int64 the suite goes red and
the declaration is deleted rather than inherited.

**Conventions for new commands.** Arguments are parsed with go-arg and logs go
through slog, vendored like `yaml/v3`. Every HTTP client goes through
failsafe-go behind one internal package that checks status codes; a readiness
probe talking to its own loopback server is not a client of that package.

**Build egress.** A `recipe-contract` subcommand that acquires bytes, whoever
invokes it, does so through `internal/fetch`, which owns the retry, status and
redirect policy, and fetches only the URLs its pin names — never a URL composed
from a version, a mirror or a dist-tag. A subcommand that acquires nothing
fetches nothing.

Egress this rule does not govern includes:

- **A package manager resolving its own graph.** `pnpm install
  --frozen-lockfile`, `npm install -g <pkg>@<version>`, `apt-get` (a [Floating
  distro input](../../CONTEXT.md)) and a `FROM` pull use the tool's own
  transport. Whether that input is pinned is a question about that tool's
  mechanism — a lockfile where there is one, a digest where there is one — and
  not something `internal/fetch` can supply. Where none exists yet, as with
  `install-copilot.sh`, that is the pinning gap [#42](https://github.com/gregwebs/agent-vm-images/issues/42)
  already tracks.
- **An example layer's install step.** An example layer is the image's own
  content, not a build helper: `agent-vm image build examples/layers/X` assembles
  a user's image from a recipe that fetches what that image installs, so its
  install step may download. #187's slice 3 moves that fetch into the example's
  own `internal/fetch`, keeping one attempt, Python's bounds and no redirects;
  slice 1 changes nothing about it. Its bounds are the pin's: seven canonical
  rust-lang.org URLs recorded in `data/rust-toolchain/pin.json`, one digest and
  one OpenPGP signature per archive, a 300 s per-request deadline, a 30 s
  inactivity bound, size caps per kind and status 200 only, all verified before
  anything is extracted or executed. The recipe's *build* steps acquire nothing:
  the helper module is vendored and compiles under `--network=none`.
- **A build step from before this port**, which still downloads with whatever
  it always did — `download.sh`, `curl`, a package manager, an interpreter's
  own HTTP client — for example the vendored Claude, Codex and OpenCode
  installers, Pi, `install-zellij.sh`, and the example layers. The order list
  replaces what can be replaced. A vendored upstream script stays vendored, and
  the example layers stay self-contained, so not all of these become Go.
- **A readiness probe** speaking HTTP to a server the program itself started on
  the loopback interface. That is not a fetch and has no pin.
- **A maintainer program.** A *pin writer* fetches the upstream bytes it is
  pinning — that is its subject, not a side effect — and the bump scripts
  resolve dist-tags with `curl`, `npm view` and `pnpm view`. They do not all use
  `internal/fetch`: `avm-pin` shells to `gh attestation download` and `cosign
  verify-blob`, which carry their own transports. What bounds them is review of
  their diff, not this rule.

**Known gap.** `install-artifact` runs `gh attestation verify --bundle` without
`--custom-trusted-root`, so `gh` refreshes the Sigstore trusted root over its own
transport: a build-time fetch from URLs no pin names, outside `internal/fetch`.
The build is therefore not hermetic in that step, and `install.go`'s comment that
`--bundle` reads only the committed bytes is wrong. Pinning a trusted root and
passing it explicitly is [#215](https://github.com/gregwebs/agent-vm-images/issues/215).

**Order.**

0. Restructure into the root module and switch to host-only delivery, with no
   behaviour change.
1. Pin pairs, each verifier together with its pin writer: artifact, Claude,
   Codex, agy.
2. dsh, OpenCode and the pnpm bootstrap. OpenCode's two install entry points
   ([#219](https://github.com/gregwebs/agent-vm-images/issues/219)) complete the
   item; `probe-opencode.py` is not part of it, for the reason recorded below.
3. The pnpm project module and Pi. This rescopes #62–#65 to Go instead of
   writing Python that would be deleted.
4. Example layers.
5. Release and CI.
6. The remaining maintainer programs.
7. The test harness.

Each component's harness moves in a follow-up after the component passes the
existing harness in Go, so the port is never checked by tests written alongside
it.

**Black-box pin suites.** A ported component's suite lives in
`script/test/pinsuite/`, one package per suite, over `pinsuite/harness` — the
fixture bytes (tar/gz/xz, ELF headers, a throwaway gpg signer) and the process
plumbing. `harness` imports nothing under `internal`: a suite must be able to
fail because the program is wrong, and must never pass because a helper agreed
with it. Every path a suite hands a program is absolute and symlink-resolved,
because the wrappers `cd` to their own checkout first.

A suite that needs synthetic release bytes shares its builder with the suites
above it through `avm-test`, which the shell suites reach at
`script/dev/avm-test.sh`. `write-fixture --tool agy` writes one staged release through the
real writer and copies the closure out; `write-fixture --tool claude` synthesizes a
committed-shape closure instead, because a fixture closure is signed by a throwaway
key the review has never seen. `script/test/agent-versions.sh` feeds its bumper
from both, so the bytes the bumper is driven with are the ones the suite holds a
writer to — not a hand-written lookalike. The Claude suite's `keygen`, `elf`,
`closure` and `two-primaries` verbs are the same builder its cases call, so
`script/test/claude-installer.sh` and `script/test/vendored-installers.sh` need no
Python fixture either. The Codex suite adds `write-fixture --tool codex` (a
synthetic closure, because a fixture's evidence cannot come from a real cosign
run) and `write-installer-fixture --tool codex`, which its hook and
vendored-installer suites mount and `script/test/agent-versions.sh` serves from.

**The pnpm bootstrap is Go.** `recipe-contract pnpm-bootstrap|pnpm-verify|
pnpm-extract|pnpm-pin-version` replace
`images/pnpm/{bootstrap,verify,read-pin-version}.py`, which are deleted.
`images/pnpm/` keeps its reviewed inputs plus what the recipes actually install
and run from it: `launcher.sh` is installed as `/usr/local/bin/pnpm`,
`node-version-check.js` is executed once by the bootstrap, and
`verify-pnpm.sh` is the entry point the recipes mount. `images/install-pnpm.sh`
and `images/pnpm/verify-pnpm.sh` stay shell as those entry points, resolving
`RECIPE_CONTRACT_HELPER`, then `AGENT_VM_CONTRACT_DIR`, then the fixed build
mount.

The bootstrap is an installer: it fetches the pinned archive through
`internal/fetch` and nothing else. That means the hermetic suite cannot shim a
downloader onto PATH any more, so `pnpm-bootstrap --fixture-archive
ABSOLUTE_PATH` names the bytes it would have fetched, and the download branch
itself — statuses, retries, ceiling — is proved by driving a fake transport
through the real client. An unreadable fixture archive is a refusal that leaves
no destination behind, which is the case the harness names.

The certification the shell entry point used to spell out (the exact report, the
exec-target access audit, the five policy defaults) is `pnpm-verify --certify`,
because its `pnpm: …` diagnostics are quoted in documentation and read in build
logs; a child that fails keeps its own status and stderr. Bare `pnpm-verify` is
the distribution audit the dsh hook re-runs around its own health gate, and the
same verb over `--prefix/--commands/--node/--source/--fixture-root` is what the
host manager authenticates with, so host maintenance has no second copy of the
pin, digest, member or package rules. `pnpm-extract` is that host acquisition
boundary, and it is in scope of build egress above: with `--download` it
fetches the pin's own URL through `internal/fetch`, and with `--archive` it
authenticates bytes the caller already holds. Neither path skips the pin.

**The dsh probe and three of its four hooks are Go (#184).**
`recipe-contract dsh-probe` replaces `images/tools/dsh/probe-dsh.py`, which is
deleted. It keeps the source job: monotonic deadlines, a private session,
bounded concurrent capture of both pipes, the token handshake, the TERM-to-KILL
teardown that proves nothing survived, and the Linux listener-ownership proof
through procfs. The ownership proof still fails closed on Linux or in
production, and only an explicit non-Linux fixture host records a skip. Its
fixture inputs are flags (`--fixture-child-env NAME=VALUE`, `--fixture-exec-tmpdir`)
and it reads no environment variable of its own. The gate above it still names the
existing `DSH_WEB_FIXTURE_` boundary from its own environment and hands it down as
those flags; moving that boundary is #213's, and until then a production capability
refuses the input instead of ignoring it. Statuses follow one principle there: a
fixture seam is a well-formed request production cannot honour, so it is a refusal
(1), while a readiness or escalation bound only limits the run and is accepted in
production. `check-runtime.mjs` stays Node.

`recipe-contract dsh-install`, `dsh-prepare-lock` and `dsh-verify` replace the
policy in `images/tools/dsh/{install-dsh.sh,prepare-lock.sh,verify-dsh.sh}`, which
remain as entry points that resolve the composed helper through the shared
project module's `contract-dir` verb and pass their caller's argv. That is a
deliberate divergence from `install-pnpm.sh`, which falls back to the fixed
build mount: `dsh-verify.sh` must never execute a helper from a fixed mount, and
its suite proves a decoy there is never reached. Exit status and diagnostics are
the predecessors': usage is 2, a refusal is 1 on stderr, and a child keeps its own
status and its own bytes. Two differences are deliberate and tested: an install
hook collapses a preparation failure to 1, as its `*) exit 1` did, and a fixture
input in a production probe is a refusal (1), while a readiness or escalation bound
is accepted there because it only limits the run. `dsh` still needs `python3` at
install time, because `project.py` is Python until #62.

**Ordering in the entry points.** `contract-dir` now runs before the environment
guard, because the entry point has to locate the helper before it can exec it, and
`verify-dsh.sh` may not fall back to a fixed mount. That is deliberate: the guard
still authorizes the composed directory itself (it runs `pnpm-environment` from it)
and refuses the same environment, so the only change is that a hostile variable is
reported one process later than it used to be.

**The OpenCode build hooks are Go (#218).** `recipe-contract
opencode-prepare-lock`, `opencode-verify` and `opencode-verify-plugins` replace the
policy in `images/tools/opencode/plugins/prepare-lock.sh`,
`images/tools/opencode/verify-opencode.sh` and
`images/tools/opencode/verify-opencode-plugins.sh`, which remain as entry points
that name the container's environment as flags. The `.mjs` programs stay Node
because they are the plugin's own runtime, and
`images/tools/opencode/probe-opencode.py` stays Python deliberately: its bytes are
rendered into the released image's own self-certification, which runs with no
network and no mounts and cannot carry the composed helper, so porting it would
create a second implementation of one protocol rather than remove one (#185,
closed on that ground).

Unlike the dsh hooks, the two OpenCode gates do not execute
`images/recipe-contract/run-report.sh`: each runs its own report child. The
envelope's `timeout` puts the managed command in a second process group, which a
cancelled session cannot reach, so a hung `--version` left that child running with
nobody left to forward the signal — the dsh path is safe only because
`run-pnpm.sh` TERMs `timeout` while it is still alive to forward. Two consequences
are deliberate. The envelope's own resolution rule is reimplemented there: the
first PATH candidate that exists is the one run, so a present-but-unusable
`node` is refused rather than skipped for a later, working one. And
`AGENT_VM_CONTRACT_DIR` still locates the helper for all three entry points, but it
is a subcommand flag only for `opencode-prepare-lock`, the one verb that still runs
a shared envelope (`run-pnpm.sh`).

**The OpenCode install hooks are Go (#219).** `recipe-contract opencode-install` and
`opencode-install-plugins` replace the policy in
`images/tools/opencode/{install-opencode.sh,install-opencode-plugins.sh}`, which remain
as entry points. Neither addresses the network itself: the vendored installer keeps
OpenCode's own transport, and the plugins hook runs the manager through the composed
`run-pnpm.sh`, so build egress still has one owner for the download and the manager's
own for its graph.

Both entry points take the helper from `AGENT_VM_CONTRACT_DIR`, then the fixed build
mount, and refuse otherwise. There is no third step: a directory the caller names is
used as it stands, so a checkout names the composed directory its own adapter
published and a stale mount cannot decide which binary a gate tested. The OpenCode
black-box harness consequently names `AGENT_VM_CONTRACT_DIR` for the install hook the
way it already did for the gate.

Two rules were deleted rather than ported, because they could not decide anything: an
empty receipt path was already unreadable, so it already failed classification, and a
child environment that strips `AGENT_INSTALL_SOFT_FAIL` has nothing to clear by
setting it to empty.

One diagnostic changes, deliberately: a receipt with more than one classified
`transport download` line used to join the third field of every line into the absence
record, and now the first classified line names the code. `download.sh` truncates the
receipt and writes exactly one line, so no build can reach the difference; where the
predecessor joined `$3`, this takes the first matching line.

`images/tools/dsh/upgrade-dsh.sh` is **not** ported. Its load-bearing surface is
the five-file publication transaction with signal-inclusive rollback, which lives
in `script/build/transactional-publish.sh` and is shared with the Pi and bridge
upgrades; porting one of the three consumers would leave two owners for one
rollback contract. The transaction moves once, when all three upgrades do.

## Considered options

- **Keep ADR 0004's byte parity.** Rejected: it cost a 213-vector differential
  runner and mostly protected output nobody parses.
- **Repository root as the product Docker context with an allowlist
  `Dockerfile.dockerignore`.** It would let the builder stage see `./internal`,
  but every `source=` path changes, a missed ignore entry sends `target/`
  (gigabytes) to the daemon, and two delivery paths remain to maintain.
- **Go source as a second named context.** This needs a composed staging
  directory on every build, with no benefit over passing the compiled binary.
- **Building examples from the repository root (`-f examples/layers/X/…`).**
  Rejected: it breaks the documented one-command `agent-vm image build` of an
  example directory.
- **A ratchet guard on remaining `.py` files.** Not adopted; review is enough.

## Consequences

- Every product image build needs Go on the client. Release reproducibility
  rests on the root `go.mod` toolchain and `-trimpath -buildvcs=false`, not a
  pinned builder image. ADR 0004 showed both paths produce identical bytes.
- ADR 0004's builder-stage digest guard and the image-compiler check
  (`recipe_contract_image_compiler`) go away for the product images. The
  example builder stages keep an equivalent guard.
- Python stays in the base image as a user feature. CI drops `python3` only
  after the harness port.
