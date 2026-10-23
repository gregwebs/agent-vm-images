# 0004: Go build helpers with a composed-directory delivery seam

Status: accepted. Both delivery paths are evidenced (§ Evidence). Rows 18-19 are
the two native CI rows that closed acceptance item 4 and item 5's clean-daemon
repeat; rows 1-17 and 20-22 are the local measurements they confirm.

## Decision

The recipe-contract build helpers are Go. This slice converts
`install-status.py` and `check-tool-access.py` (~670 lines) into one binary,
`images/recipe-contract`, with two subcommands. Every agent verifier, the pnpm
verifier, the standard final gate and the test harness invoke `recipe-contract`
instead of `python3 FILE`.

```text
images/recipe-contract/{installstatus,toolaccess}   domain rules, typed results
                  ↑
        internal/cli                                argv, legacy bytes, statuses
                  ↑
       cmd/recipe-contract                          process assembly only
                  ↑
   one executable, two delivery adapters
┌──────────────────────────┴──────────────────────────┐
(a) pinned Go builder stage in the product Dockerfile   (b) host cross-compile
    FROM --platform=$BUILDPLATFORM golang@sha256:…          script/build/recipe-contract.sh
    GOOS=linux GOARCH=$TARGETARCH → /out                    → target/recipe-contract/OS-ARCH/out
    └──────────────────────────┬──────────────────────────┘
   RUN --mount=from=recipe-contract-bin,source=/out,target=/tmp/recipe-contract,ro
                  ↓ existing authorized contract-directory seam
        verifiers → no helper or compiler in any final layer
```

The packages are independent of deployment: `installstatus` and `toolaccess`
know nothing about argv, streams, exit statuses or file layout; `internal/cli`
owns the legacy byte contract; `cmd/recipe-contract` only assembles the process
environment. Splitting the binary later means adding `cmd/` entries, nothing more.

### Why Go, and what it does not give us

Go cross-compiles with `CGO_ENABLED=0`, compiles in seconds and is already the
language of the tooling image. It has no sum types, so a closed enum plus the
`exhaustive` linter stands in for case analysis: `PathState`, `RecordKind`,
`Mode` and the waypoint kinds are typed, and removing a case fails lint (checked
as a mutation control, not assumed). This is weaker than a real sum type; the
compensating rule is that unexported fields keep parsed `Record` and
`TransportCode` values behind their grammar. `PhysicalDirectory` labels a
caller-supplied syscall result; its constructor does not validate a path.

### Why a composed directory instead of a binary mount

Adding a single-file read-only mount *underneath* the existing
`recipe-contract` directory bind fails when the mountpoint does not exist
(`create mountpoint … read-only file system`). Both adapters therefore publish the
whole contract directory: the unchanged shell/Node sources plus `recipe-contract`.
The mount path is unchanged (`/tmp/recipe-contract`), so the authorized
`AGENT_VM_CONTRACT_DIR` seam and dsh's `project.py contract-dir` authorization are
untouched. No tracked placeholder binary, writable mount, runtime `COPY` or new
executable-policy override is introduced.

### What the mount carries

The composed directory is the module tree plus the binary, not a consumer-only
allowlist: the shell and Node helpers (`run-*.sh`, `pnpm-lock.mjs`,
`check-pnpm-environment.mjs`, `download.sh`, `vendor/`) are invoked from the
mounted path, and the Go sources (`*.go`, `go.mod`, `cmd/`, `internal/`,
`installstatus/`, `toolaccess/`, `testdata/`, `.golangci.yml`) travel with them so
there is one canonical tree and no maintained copy to drift. The extras are inert
read-only data in a **build-time-only** mount: nothing is copied into a layer, the
toolchain never enters the mounted tree, and the mount is `ro`.

The one thing not acceptable in that tree is Python, because it would be
*executable* helper code under the rule this slice is enforcing. `cp -R` would
also carry any untracked file, so the recipe itself refuses to publish a composed
directory containing `*.py`, `*.pyc` or `__pycache__` and names the offenders. That
is belt-and-braces for the repository-side Rust guard, which does not run inside
`docker build`; it narrows the Python-return guard rather than weakening it.

Standard cannot reuse a stage declared in base: Docker stages do not cross
Dockerfiles. Both product Dockerfiles carry the same short stage declaration and a
Rust guard keeps the two declarations byte-identical and the image version equal
to `go.mod`'s toolchain.

### Version ownership and upgrade procedure

`images/recipe-contract/go.mod` owns the Go version: the `go` line is the language
minimum and the `toolchain` line is the selected patch release. The Dockerfile
`golang:` tag and its digest are the only other copies and are guarded against
that line; `actions/setup-go` reads the module too. To upgrade:

1. change `toolchain go<version>` in `go.mod`;
2. resolve the new multi-platform index digest for `golang:<version>-trixie`
   (`docker buildx imagetools inspect golang:<version>-trixie`), review the
   amd64/arm64 manifests and the in-image `go version`;
3. update the tag+digest in **both** Dockerfiles (the guard fails until they
   match);
4. re-run `bash script/test/contracts.sh` — the builder stage's builder-only
   compiler check (`-tags recipe_contract_image_compiler`,
   `GOTOOLCHAIN=local`) fails a stale image compiler instead of letting
   `GOTOOLCHAIN=auto` download a different one.

### Delivery facts proven in this slice

* Path (a) builds `--target recipe-contract-bin` on colima/arm64 and leaves a
  composed `/out` with 18 entries including the dotfiles (`ls -A`).
* At `cf50c1b` (historical Go sources), path (b) is byte-identical to path (a)
  for **both** architectures with the same
  source SHA, toolchain, `CGO_ENABLED=0`, `-trimpath -buildvcs=false` and
  features: linux/arm64 sha256
  `aa88151442718305db018acb1a333c55b2a26484b85c21a803b84b5ea354e098` (2 750 384
  bytes) and linux/amd64 sha256
  `643ea00d1d7d60045a49591cf5bf481552e4d309b38d4ac2f02017b575f77278` (2 811 879
  bytes); `shasum -a 256` and `cmp` agree for both. Byte identity is evidence
  only, not a gate. At `89d348a` (the current Go sources), independent verifier
  §V6 confirms `cmp` clean for both paths: arm64
  `4c63087aaa4602533ae3c0d8537f45bd65282f0cd4440344d2e61b14f9c4d150`
  (2 750 239 B), amd64
  `ad7009caf3187d5aec63ac4263258ab8c4781e0c40c26dce6409f959105a5e28`
  (2 811 862 B). The final fix pass changes no Go sources.
* The host recipe serializes publication (a per-output lock with a pid, build in a
  private stage, publish by rename, previous tree unlinked only after the swap) and
  refuses to publish a composed directory containing Python, so concurrent suites
  replacing one `out/` and a stray untracked `.py` both fail safe. A fake sleeping
  `go` reproduces the pre-fix race (`mv: rename …/out …: No such file or
  directory`, verifications D20), and `script/test/build-entrypoint.py` asserts
  both concurrent builds succeed at HEAD. Residual: the swap is two renames, so a
  reader resolving exactly between them sees the path absent — exclusive to
  publishers, and an already-open reader keeps the old inode. Dead-pid recovery
  has one reaping claimant, which rechecks the owner before renaming the lock;
  normal cleanup uses `rmdir` so it cannot remove that claim. A pid-less lock or
  an abandoned reaping claim fails closed after the bounded wait and needs manual
  inspection; elapsed time alone does not prove an owner is dead.
* A named `--build-context recipe-contract-bin=<host context>` replaces the stage;
  the helper is absent in the following RUN in both variants.
* `bash images/build.sh {base,standard} --host-helpers` compiles with the host Go
  toolchain and passes exactly one named context. Every other `images/build.sh`
  invocation is unchanged and needs no Go on the client, because the builder stage
  runs inside Docker.

### Behavior parity, and the three disclosed exceptions

Parity was measured, not assumed. A temporary differential runner
(`/tmp/avm-74/compare-recipe-contract.py`, not committed) ran 213 vectors of
bytes argv against the Python oracle under one cwd/PATH/uid and compared
`(stdout, stderr, status)`:

* macOS host: 191 match, 0 mismatch, 22 Linux-only vectors reported
  not-applicable (never counted as passes).
* Linux arm64 in the maintained image as uid 12345:23456: 213/213 match,
  including the four independent non-UTF8 stream goldens (a 0755 path on stdout
  and a 0644 path on stderr are separate vectors), `/dev/full` and closed-reader
  flush cases, the 40/41 symlink and 5/6 interpreter boundaries, interpreter
  cycles, tree/FIFO vectors and invalid-UTF8 argv diagnostics. The runner is
  unprivileged and has no ACL vector; POSIX ACL handling is exercised by the
  native `tool-access`/`standard-image` suites instead, not by this count.

Supported environment: the maintained image's UTF-8 setup (`LANG=C.UTF-8`,
UTF-8 filesystem and stdio, default buffered stdout with
`errors=surrogateescape`, stderr with `errors=backslashreplace`). Python-specific
overrides (`PYTHONIOENCODING`, `PYTHONUNBUFFERED`, a non-UTF8 locale) are not part
of the promise. `internal/cli` reproduces the stream split: raw non-UTF8 path
bytes round-trip on stdout, and become `\udcXX` on stderr.

Three narrow, deliberate differences remain (legacy help/usage bytes, including
retired source names, stay frozen as compatibility output rather than current
source documentation):

1. **Synchronous stdout-write failure traceback.** When stdout fails while the
   program is running, the legacy interpreter exited 1 with a Python traceback
   containing retired `.py` source frames. The port keeps the status (1), the
   same exception line (`OSError: [Errno 28] No space left on device`,
   `BrokenPipeError: [Errno 32] Broken pipe`) and the same
   "stop processing immediately" behaviour, but does not reproduce source frames.
   If stderr cannot accept the exception report, the status is 120 instead.
   Deferred flush failures keep their exact bytes and status 120
   (`Exception ignored on flushing sys.stdout:` + the exception line), which this
   exception does NOT cover.

   The same traceback exception covers the other legacy uncaught internal failures
   (a removed working directory, a readlink race, a NUL byte in a path derived from
   a script): status and the final exception line are preserved, source frames are
   not. The line itself is reproduced exactly, including CPython's OSError subclass
   picked from the errno (`FileNotFoundError`, `PermissionError`,
   `NotADirectoryError`, `IsADirectoryError`, `FileExistsError`,
   `BlockingIOError`, `BrokenPipeError`, `ProcessLookupError`, `InterruptedError`,
   `ChildProcessError`, `TimeoutError`, the connection subclasses, and plain
   `OSError` for the rest), and CPython's
   `ValueError: lstat: embedded null character in path` — a `ValueError`, because
   CPython rejects the NUL before the syscall, so it is not an errno line at all.

2. **CPython's silent, shape-dependent flush failure.** In the maintained image
   (CPython 3.13.5), many small writes totalling **8190–8193 bytes** to
   `/dev/full` leave Python at **0 with the output dropped**, while Go returns
   **120**. In the same verifier probe, 4095/4096-byte totals give both 120
   with identical deferred-flush diagnostics; a separate 8580-byte shape gives
   both 1 (subject to exception 1). There is no measured universal total-byte
   boundary independent of write shape. Verifier §V25 reproduction command:
   `docker run -i … --entrypoint python3 agent-vm-base:local - < /tmp/avm74-boundary.py`.
   Earlier probes also observed silent loss on closed-reader stdout. The accepted
   difference is this silent-loss class, not arbitrary stream mismatches: Go
   reports a failed final flush as 120 rather than emulating success after losing
   audit output. The merged-stream ordering fixture remains narrower: CPython
   flushes the pending text
   *before* appending a write that would not fit, so a violation diagnostic lands
   after the last whole line that fit (byte 8160 for 34-byte success lines, never
   8194); the port does the same and `TestMergedStreamDiagnosticOffset` pins it.
3. **Closed stderr (`2>&-`).** CPython's `sys.stderr` is `None` there, so its
   `print(..., file=sys.stderr)` silently falls back to **stdout**: an unknown
   option exits 2 with `check-tool-access.py: unknown option --bogus` on stdout.
   The port writes nothing to either stream and exits 2 (measured, both in
   `agent-vm-base:local`). A closed descriptor is outside the supported
   environment, no consumer closes stderr, and republishing a diagnostic on an
   unrelated stream would be worse than silence; the status still matches.
   `DiagnosticStream` models the closed case so the failure statuses stay exact.

### Accepted ACL lookup risk (legacy retention)

The Linux ACL predicate intentionally retains the oracle's fail-open xattr
lookup: either `Getxattr` call failing, or returning a size/read count ≤0,
means no ACL. This includes EACCES, EIO and ERANGE, not just
ENODATA/ENOTSUP/EOPNOTSUPP. A positive size followed by a successful positive
read count is rejected as an access ACL, without parsing the value. Non-Linux
builds do not inspect ACLs. Thus a lookup failure can certify a path whose ACL the
auditor could not inspect; mode bits alone are not proof in that case. This is
an accepted parity risk for this behaviour-preserving port, not an assertion
that unexpected errors are safe. A fail-closed change needs separately reviewed
diagnostics/statuses and Linux fault controls; it is not silently included here.

## Evidence

Historical measurements below have reachable source counterpart `cf50c1b`
(`cf50c1b4b648d800589d18147befbabacc01c154`): its Go sources equal those of
the original pre-rewrite measurement, and the follow-up reviewer reproduced both
historical hashes from it. These runs used macOS arm64 with colima (Docker driver,
BuildKit), `agent-vm-base:local` (CPython 3.13.5) and the pinned Go image.
Historical product rows are not current-source acceptance. Current Go-source
identity/parity at `89d348a` are rows 20–22, from the reviewer and independent
verifier (§V of `/tmp/avm-74/verifications.md`). Every row below ran; none is OPEN.
Rows 18-19 were measured at source `21af10d`; this ADR edit that records them is
documentation only.

Paths under `/tmp` in the rows below are the review's own scratch artifacts, not
repository data: each row names the command that reproduces its result, and
nothing in the repository depends on those files.

| # | Claim | Command | Result |
|---|---|---|---|
| 1 | Historical Go source counterpart | `git rev-parse cf50c1b` | `cf50c1b4b648d800589d18147befbabacc01c154`; same Go sources as the original measurement |
| 2 | One version source | `images/recipe-contract/go.mod` | `go 1.27.0`, `toolchain go1.27.3`; builder image `golang:1.27.3-trixie@sha256:fbb7ae40…`; linter `2.14.0` from `images/recipe-contract/golangci-lint.version` |
| 3 | Module gates | `go vet ./...`, `gofmt -l .`, `golangci-lint run`, `go test -count=1 ./...` (macOS; and Linux in the pinned image as uid 12345, `GOTOOLCHAIN=local`) | clean, `0 issues.`, all packages ok on both; `TestMergedStreamDiagnosticOffset`, `TestGoldenStreamFailures` and `TestBothStreamFailureStatus` PASS on Linux (not skipped) |
| 4 | Builder-only compiler control rejects a stale pinned image | scratch module with `toolchain go1.27.4`, pinned image, `GOTOOLCHAIN=local go test -tags recipe_contract_image_compiler -run '^TestImageCompiler$' ./internal/buildcheck` | FAIL as intended: `compiler is go1.27.3, want go1.27.4 (go.mod language minimum 1.27.0, toolchain go1.27.4)…`, status 1 |
| 5 | Behaviour parity, Linux | differential runner over 213 byte-argv vectors, `agent-vm-base:local`, uid 12345:23456, CPython 3.13.5 (`/tmp/avm-74/parity-linux-part2.jsonl`, sha256 `eaae4501…`, candidate `aa881514…`) | 213 match, 0 mismatch, 0 not-applicable; the 0644 `bad-\xff` vector is its own status-1 stderr case |
| 6 | Behaviour parity, macOS | same runner, host CPython 3.15.0, uid 502 (`/tmp/avm-74/parity-host-part2.jsonl`, sha256 `f2805b90…`, candidate `af8fa335…`) | 191 match, 0 mismatch, 22 Linux-only vectors not-applicable |
| 7 | Historical `cf50c1b` path (a) == path (b), arm64 | `docker build --builder colima --platform linux/arm64 --target recipe-contract-bin -f images/Dockerfile images`, `docker cp <cid>:/out/recipe-contract` vs `bash script/build/recipe-contract.sh --os linux --arch arm64` | identical (`cmp`), sha256 `aa88151442718305db018acb1a333c55b2a26484b85c21a803b84b5ea354e098`, 2 750 384 bytes |
| 8 | Historical `cf50c1b` path (a) == path (b), amd64 | same commands with `/amd64` | identical (`cmp`), sha256 `643ea00d1d7d60045a49591cf5bf481552e4d309b38d4ac2f02017b575f77278`, 2 811 879 bytes |
| 9 | Builder stage builds for both arches | the two builds in rows 7-8 | ok on this host with a warm build cache |
| 10 | Native consumer rows (linux/arm64 containers on this host) | `bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer primitives --platform linux/arm64` | PASS in ~90 s: 28 probes, 7 stages x 4 uid pairs; helper `go1.27.3` recorded in `helper.json` (historical run; reachable source counterpart `cf50c1b`, not a rerun of the current adapter wiring) |
| 11 | Native dsh consumer row | `bash script/test/standard-node-consumers.sh agent-vm-base:local --consumer dsh --platform linux/arm64` | PASS in ~5 min: real pnpm install, 4 uid pairs, web/PTY/koffi/genai/protobuf/subprocess probes (historical run; source counterpart `cf50c1b`, same caveat) |
| 12 | macOS path (b), base | `bash images/build.sh base --host-helpers` | exit 0, 51 s (warm cache) |
| 13 | macOS path (b), standard | `bash images/build.sh standard --host-helpers` | exit 0, 5 m 38 s; exactly one `--build-context recipe-contract-bin=<ctx>` reaching the build |
| 14 | The path-(b) standard image is a working standard image | `bash script/test/standard-image.sh agent-vm-base:local agent-vm-standard:local --platform linux/arm64` | `finished standard image acceptance passed`, exit 0, 2 m 07 s: labels, reports, T5 audits and status records for six agent tools x 4 uid pairs, twelve T5 negatives rejected with the offending path, repaired fixture passes |
| 15 | Artifact and reviewer-facing audits | `standard-image.sh --artifact` (134 s), `standard-image.sh --pi-audit-status` (4 s), `opencode-runtime.sh` (40 s), `pi-runtime.sh` (86 s, needs a colima-visible `TMPDIR`), `shipped-installer-network.sh` (285 s) | all exit 0 |
| 16 | No runtime helper or compiler | `docker run --rm --entrypoint sh agent-vm-standard:local -c 'test ! -e /tmp/recipe-contract; command -v recipe-contract; command -v go; ls /opt/agent-vm/install-status \| wc -l'` | `no-mount`, `no-helper`, `no-go`, 8 install-status records |
| 17 | Full `contracts.sh` | `PATH=<lint 2.14.0> RUSTUP_TOOLCHAIN=1.98.1 bash script/test/contracts.sh` | **blocked on this host** at `ci-boot-runtime.sh` (`stat: illegal option -- c`, GNU coreutils absent) — the same failure on `main`; every later suite was run individually and passes, including the new `contracts-formatter` suite. |
| 18 | Acceptance item 4: both native CI architectures build through path (a) | `.github/workflows/build-local.yml` `build` + `audit` rows, source `21af10d`: run [39835706207](https://github.com/gregwebs/agent-vm-images/actions/runs/39835706207) `amd64` (ubuntu-24.04, job `120032417314`) and `arm64` (ubuntu-24.04-arm, job `120032417296`); `contracts` run [39835706287](https://github.com/gregwebs/agent-vm-images/actions/runs/39835706287); transports run [39835706211](https://github.com/gregwebs/agent-vm-images/actions/runs/39835706211) | PASS, all three workflows green: both rows ran `ci-native.sh build` through the pinned `golang:1.27.3-trixie` stage with no named host context, then `ci-native.sh audit` (`standard-image.sh` acceptance, `--artifact`, `--pi-audit-status`, `standard-certification.sh`) within the audit budget |
| 19 | Acceptance item 5 on a clean daemon | the same work as rows 12-14 on a machine without a warm cache: the row 18 `build` + `audit` phases on the disposable hosted runners | PASS: both rows built base and standard from an empty Docker cache (GitHub-hosted runners, no persisted volumes or build cache) and ran the same acceptance audits green. The maintained macOS host still only fits a warm-cache run, so the CI rows are the clean-daemon evidence. |
| 20 | Current Go-source identity, `89d348a` | independent verifier §V6: stage extraction vs host adapter, both architectures, `shasum -a 256` and `cmp` | arm64 `4c63087aaa4602533ae3c0d8537f45bd65282f0cd4440344d2e61b14f9c4d150` (2 750 239 B); amd64 `ad7009caf3187d5aec63ac4263258ab8c4781e0c40c26dce6409f959105a5e28` (2 811 862 B); both `cmp` clean |
| 21 | Current Go-source parity, macOS, `89d348a` | `/tmp/avm-74/followup-review-parity-host-HEAD-summary.json`, copied verbatim to `/tmp/avm-74/part4-parity-host-summary.json` | 191 match, 0 mismatch, 22 not-applicable; binary `46b23dcc…` |
| 22 | Current Go-source parity, Linux, `89d348a` | `/tmp/avm-74/followup-review-parity-linux-HEAD-summary.json`, copied verbatim to `/tmp/avm-74/part4-parity-linux-summary.json` | 213 match, 0 mismatch, 0 not-applicable; binary `4c63087a…` |

Two known host-level failures are environmental and reproduce on `main`:
GNU `stat` is absent on macOS, and `render-released-agent-probe.py --self-test`
reports `healthy hashes must pass`. Neither is caused by this change.

## Alternatives

Keeping Python preserves byte identity for free but keeps the helpers untyped and
keeps a `python3` dependency in the build path for code `CODING_STANDARDS.md`
already asks to type. A Node port would fuse the helpers with the runtime layer
they serve. Compiling in every consumer RUN would put a toolchain in the build
path repeatedly and make cache behaviour worse. Committing a prebuilt binary would
be an unreviewed artifact and would not build for both CI architectures.

A two-PR split (module first, cutover second) is unsuitable:
production callers, fixtures and both delivery paths must move together or the
image is broken between the PRs.

## Consequences and limits

* Python stays in the image as a user-facing feature and for the unconverted
  helpers (`pnpm-lock.mjs` is Node; `check-pnpm-environment.mjs` and
  `script/release/` are a follow-up ticket). `contracts.sh` keeps Python as a
  prerequisite and keeps the embedded-Python gate.
* The harness stays in Python/Node/shell; only invocation and mount wiring moved.
* `golangci-lint` 2.14.0, not 2.13.2: 2.13.2 cannot read Go 1.27 export data
  (`cannot decode "internal/byteorder", export data version 5 is greater than
  maximum supported version 4`), reproduced inside `golang:1.27.3-trixie`.
  `contracts.sh` and CI pin 2.14.0 exactly.
* `actions/setup-go` v5.6.0 reads only the `go` directive from
  `go-version-file`, not `toolchain` (verified against the pinned commit's
  `parseGoVersionFile`). CI therefore asserts the selected toolchain in a
  following step instead of assuming the action performs the selection: it runs
  the module's own `TestImageCompiler` under `GOTOOLCHAIN=auto`, so no shell
  parses `go.mod` or `go version` output. go.mod stays the only source of truth.
* Measured overhead on this host: stage build 14 s cold-ish, module `go test
  ./...` ~1 s, `golangci-lint run` ~2 s, `contracts.sh --guard-only` 24 s.
  Linux ACL, device and numeric-uid acceptance still belong to the native CI rows
  and the native smoke suites.

## Amended by #75 (2026-10-22)

The decision above stays as written. Two of its statements are now read through
this amendment:

- **What the mount carries.** The composed directory still holds the whole module
  tree plus the binary, but the Node helpers (`pnpm-lock.mjs`,
  `check-pnpm-environment.mjs`) and the npm `vendor/yaml/` tree are gone. In their
  place are the Go packages `pnpmlock/`, `pnpmenv/`, `dshupgrade/`,
  `internal/document/` and the committed Go vendor closure
  (`vendor/go.yaml.in/yaml/v3`, `vendor/modules.txt`). The tree is still inert
  read-only source in a build-time-only mount, and `build.sh` still refuses to
  publish a composed directory containing Python.
- **Consequences.** The pnpm helpers are subcommands now; the "follow-up ticket"
  sentence no longer applies to them. Command ownership and the build/vendor
  rules are defined in [ADR 0001's amendment](0001-build-only-pnpm-graph-parser.md). Native host upgrades name the composed directory their own
  `script/build/recipe-contract.sh --native` build published
  (`target/recipe-contract/OS-ARCH/out`), which `project.py` authorizes as the
  checkout's host shape.
