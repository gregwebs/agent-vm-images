# 0017: Programs are Go; shell is for entry points and automation

Status: accepted. Amends [ADR 0004](0004-go-build-helpers.md) (layout, delivery,
parity bar). Not yet implemented; step 0 is #178.

## Context

ADR 0004 and its #75 amendment moved the recipe-contract build helpers to Go.
About 6.4k lines of Python still run as build helpers (pin verifiers, the pnpm
project module, the Pi helpers, the dsh and OpenCode probes). Another 3.5k run
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
`agent-vm build examples/layers/X`, and agent-vm has no `--build-context`
flag. So each example's helper is its own Go module in its directory, built by
the example's own pinned `golang:` stage. A guard ties that image to the root
`go.mod` toolchain. Logic shared with `./internal` (OpenPGP, archive hashes)
is duplicated deliberately. When agent-vm accepts named build contexts
([agent-vm#627](https://github.com/gregwebs/agent-vm/issues/627)), examples can
move to the host cross-compile.

**Parity bar.** A port keeps exit status and meaning. The existing harness runs
against the Go program and is the oracle. Output text is frozen only where a
consumer parses it. ADR 0004's byte-level differential parity is not required
for new ports.

**Conventions for new commands.** Arguments are parsed with go-arg and logs go
through slog, vendored like `yaml/v3`. Every HTTP client goes through
failsafe-go behind one internal package that checks status codes, and an
installer fetches exactly the URL its pin names through it — never a mirror, a
dist-tag or a second origin. A helper that is not an installer makes no network
requests at all, and `download.sh` stays the only path for the evidence and
archives a build downloads beside its pins. The pnpm bootstrap is an installer
that runs during the base build: it fetches the one archive its pin names, so
this rule names it instead of exempting it silently.

**Order.**

0. Restructure into the root module and switch to host-only delivery, with no
   behaviour change.
1. Pin pairs, each verifier together with its pin writer: artifact, Claude,
   Codex, agy.
2. dsh, OpenCode and the pnpm bootstrap.
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
`script/dev/avm-test.sh`. `write-fixture --tool agy` publishes one release with the
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
boundary.

**The dsh probe and three of its four hooks are Go (#184).**
`recipe-contract dsh-probe` replaces `images/tools/dsh/probe-dsh.py`, which is
deleted. It keeps the source job: monotonic deadlines, a private session,
bounded concurrent capture of both pipes, the token handshake, the TERM-to-KILL
teardown that proves nothing survived, and the Linux listener-ownership proof
through procfs. The ownership proof still fails closed on Linux or in
production, and only an explicit non-Linux fixture host records a skip. Its
fixture inputs are flags (`--fixture-child-env NAME=VALUE`, `--fixture-exec-tmpdir`)
and it reads no environment variable of its own; the gate above it still names the
existing `DSH_WEB_FIXTURE_` boundary from its own environment and hands it down as
those flags, and a production capability refuses that input instead of ignoring it.
`check-runtime.mjs` stays Node.

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
input in a production probe is a refusal (1), because the request is well formed
and the capability cannot honour it. `dsh` still needs `python3` at install time,
because `project.py` is Python until #62.

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
  Rejected: it breaks the documented one-command `agent-vm build`.
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
