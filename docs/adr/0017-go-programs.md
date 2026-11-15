# 0017: Programs are Go; shell is for entry points and automation

Status: accepted. Amends [ADR 0004](0004-go-build-helpers.md) (layout, delivery,
parity bar). Not yet implemented.

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
is duplicated deliberately. When agent-vm accepts named build contexts,
examples can move to the host cross-compile.

**Parity bar.** A port keeps exit status and meaning. The existing harness runs
against the Go program and is the oracle. Output text is frozen only where a
consumer parses it. ADR 0004's byte-level differential parity is not required
for new ports.

**Conventions for new commands.** Arguments are parsed with go-arg and logs go
through slog, vendored like `yaml/v3`. Every HTTP client goes through
failsafe-go behind one internal package that checks status codes. Build
helpers make no network requests; `download.sh` stays the only download path
during a build.

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
