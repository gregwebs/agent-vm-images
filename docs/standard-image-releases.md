# Standard image releases

This is the canonical operations contract for publishing exactly one public
image product, `ghcr.io/gregwebs/agent-vm-standard`, and for consuming its
released artifacts. It maps to the image sources in `images/`, the release
module in `script/release/` and the workflows `.github/workflows/release-*.yml`.

The base image (`images/Dockerfile`) remains local-only. There is no base
release, no per-tool artifact, no `latest` tag, no hourly rebuild, no launcher
version lookup and no automatic retention. A registry release is an immutable,
digest-addressed **public prerelease** until a maintainer explicitly promotes
the same bytes to stable.

## Identity model

A platform manifest, not a tar filename or Docker image ID, is the same-image
identity across transports.

```text
images/standard/version  v0.1.0
images/Dockerfile --local FROM--> images/standard/Dockerfile (built once/arch)
        |
        +-- Buildx type=oci,tar=false --> canonical OCI layout (one build/arch)
        |          |
        |          +-- tar + docker image load --> existing native audits
        |          +-- skopeo --preserve-digests --> GHCR :vVERSION-ARCH
        |          +-- tar same layout ----------> GitHub Release archive parts
        |
        +-- buildx imagetools create --> GHCR :vVERSION (two-platform index)
```

- The published tag `:v0.1.0` points at a two-entry OCI index. `:v0.1.0-amd64`
  and `:v0.1.0-arm64` point at the platform manifests. Digests are
  authoritative; version refs are never overwritten.
- The platform manifest hash identifies the same graph in a registry, a
  downloaded OCI layout and a tarred OCI layout. Config and ordered layer
  blobs are byte-identical across transports. A tar checksum and an index hash
  are **not** the platform manifest hash.
- Docker's reported `.Id` is recorded separately (`docker_reported_id`) from the
  canonical OCI config digest. On the supported containerd-backed Docker store
  it reports the platform manifest hash; it is never asserted to equal the
  config digest.

## Versioning and release dispatch

- `images/standard/version` is the single owner of the image version. It must
  contain a stable `MAJOR.MINOR.PATCH` value plus a trailing newline. Image
  releases are independent of any launcher workspace version; never bump the
  launcher here.
- A version is consumed once. After any partial publication, that version is
  abandoned; a new reviewed version must be committed to build again.
- `.github/workflows/release-standard.yml` is `workflow_dispatch` only. It has
  no PR/push/schedule trigger and no arbitrary source/config/build-arg inputs.
  The publication ref must be `refs/heads/main` of `gregwebs/agent-vm-images`;
  the version is read from the checked-out committed file.

To publish:

1. Land the new `images/standard/version` (and any source change) on `main`.
2. Dispatch **Release standard image** from `main`.
3. There is exactly one approval per release: the `assemble` job pauses in the
   protected `image-release` environment after both native builds. Confirm the
   effective `Contents: write` grant (Set up job → GITHUB_TOKEN Permissions)
   and, on the first release only, make the GHCR package public before
   approving — a new user-owned package defaults to private and anonymous
   consumption is required. `preflight` and `native` do not pause.
4. After `assemble` publishes the prerelease, `boot-amd64` runs the hosted amd64
   boot gate below. Red does not unpublish anything: **do not promote**. Inspect
   its evidence artifact, then re-run the job or dispatch **Verify released
   standard image boot (amd64)** for that version.

## Rehearsal before a merge

The build and its native audits are the expensive, failure-prone half of a
release, and they are worth exercising without publishing anything.
`.github/workflows/release-rehearsal.yml` runs on any push to a branch under
`test/release-standard/`:

```
git push origin HEAD:test/release-standard/<name>
```

It runs the same `script/release/build-standard.sh` sequence as the release
(base + standard build, canonical OCI export, Docker load, capacity sampling,
SBOM, and all five native audits) on both architectures and uploads the per-arch
evidence. It holds no publication authority at all: `contents: read` only, no
GHCR login, no attestations and no Release, so it stops short of the registry
push.

The build is invoked with `--rehearsal`, which validates `GITHUB_REF` against
`refs/heads/test/release-standard/` (`operations.rehearsal_run`). That check is
deliberately disjoint from the `refs/heads/main` check `operations.trusted_run`
performs, so neither mode can pass the other's guard: a rehearsal build on
`main` is refused, and a publication build on a rehearsal branch is refused.

A green rehearsal is evidence, not a release. Nothing it produces is
consumable, and the version is still published only by the `main` dispatch
above.

The build rehearsal cannot boot-verify a signed release because it signs nothing.
Exercise the boot gate against an **already published** version instead:
`git push origin HEAD:test/release-boot/<VERSION>` (optionally `/<name>`). It has
only `contents: read`, no secrets. Push to this repository, not a fork, with a
credential able to trigger Actions (not an ordinary workflow `GITHUB_TOKEN`
push). Push is the reliable initial registration path; API/CLI branch dispatch
may work after registration/first run, even before merge.

## Permissions and approval hosts

- Top level is `contents: read`. Each job narrows or widens explicitly.
- `boot-amd64` / `verify-release-boot.yml`: `contents: read` only, no secrets,
  no environment, hosted `ubuntu-24.04`. `gh attestation verify` needs no token
  for bundle verification; fresh-runner default TUF trust initialization remains
  first-run acceptance.
- `assemble` is the only job in the protected `image-release` environment, so
  a release needs a single review; `preflight` and `assemble` hold
  `contents: write` so the draft-inclusive Release list is authoritative, and
  `preflight` performs GETs only and does not pause.
- `native` (matrix `amd64` → `ubuntu-24.04`, `arm64` → `ubuntu-24.04-arm`)
  holds `contents: read`, `packages: write`, `id-token: write`,
  `attestations: write`. It publishes only after build and audits.
- `GHCR_PACKAGE_INVENTORY_TOKEN` is a repository secret: an owner classic token
  restricted to `read:packages`. It is the authoritative owner-wide package
  inventory; a repo token's concealed 404 can never prove first-package
  absence. It is never a write credential and is never injected into build RUNs.
- There are no hand-set release authority variables. Authority is the trusted
  `refs/heads/main` invocation (checked in-process) plus the real draft/asset
  writes failing closed. GitHub exposes no effective-permission introspection
  for installation tokens (`GET /repos` `permissions`, GraphQL
  `viewerPermission` and `GET /installation/repositories` all report
  `false`/`null` for a write-capable `GITHUB_TOKEN`), so no `write` attestation
  is consulted and none need be maintained. If the invocation is not the
  trusted main one, preflight classifies `UNAUTHORIZED_OR_CONCEALED` and stops.

## Create-once and recovery

GHCR and GitHub Releases cannot be committed atomically, and GHCR tags lack an
atomic create-if-absent operation. Concurrency plus a namespace policy that
reserves this package/version to this pipeline serialize writers; TOCTOU
remains an explicit constraint.

- Initial preflight refuses any existing platform ref, version index, git tag
  or Release (including a **draft without a git tag**, found across every
  paginated list page).
- Each native job refuses a pre-existing platform ref immediately before push.
- Assembly accepts only its own just-pushed platform refs and refuses any
  pre-existing version index, tag or Release; it re-runs the same draft-aware
  absence check immediately before its first write.
- Any nonzero or missing evidence stops the run. There is no rebuild or
  fallback under an already-published version. Failed runs may leave platform
  refs or a draft; resume direct verification and explicit promotion against
  those immutable bytes, or abandon the version and commit a new one. Do not
  delete prior stable content and do not implement remote rollback/GC.

## Attestations, SBOM and trust limits

- Per platform: an image provenance attestation and an SPDX-JSON SBOM
  attestation, both bound to the platform manifest digest and pushed to the
  registry. A file-provenance attestation covers the frozen `platform.json`,
  the archive files/parts and the SBOM; its bundle is
  `platform-ARCH-files.sigstore.json`.
- `platform.json` is a strict schema containing the full image graph, the eight
  committed selections, source SHA/version/product/arch, Docker reported
  ID/driver/version plus canonical config digest, archive/part/SBOM
  names/sizes/hashes, `build_run_id`, `build_run_attempt` and the exact
  invocation URL. Assembly verifies its exact SHA-256 subject, signer
  workflow/ref, hosted-runner attestation and SLSA invocation ID **before**
  creating the index, then rehashes every payload and OCI byte it binds.
- The release index and the final `release.json`/`SHA256SUMS` are attested
  separately (`release-index.sigstore.json`, `release-metadata.sigstore.json`).
  `release.json` records the index digest, both platform records, and the
  build run ID/attempt and invocation URL.
- Verification delegates all cryptography and OIDC/Rekor trust to GitHub and
  Sigstore tooling (`gh attestation verify`, `--deny-self-hosted-runners`). No
  custom signer is implemented. Signatures authenticate provenance; hashes
  authenticate content; neither proves tool health or the absence of
  vulnerabilities. An SPDX SBOM also does not prove the absence of
  vulnerabilities.
- CI installs Syft `v1.20.0` from its committed SHA-256 pins in
  `script/release/tool-pins.json`. Host operators may provide their own tools
  but must record exact versions. CI also checksum-pins gh `v2.97.0`; apt
  skopeo/coreutils/jq/Python float and are not immutable pins.

## Prerequisites

- Disposable Linux CI runners must use the Docker containerd image store before
  any standard build (`script/release/prepare-ci.sh`; never reconfigure an
  operator daemon). The direct `type=oci,tar=false` exporter and Docker loader
  are proven by a tiny capability gate before the expensive build.
- Native capacity is measured per filesystem and per phase
  (`script/release/storage.py`, `script/release/capacity.py`). The daemon
  filesystem must satisfy the summed peak budgets and the existing **>=25 GiB**
  post-base source floor. Insufficient or unmeasurable space is a hard failure;
  never prune to get tests passing.
- Verify tooling needs a compatible `gh attestation` verifier, `skopeo` with
  OCI `--preserve-digests` and a real standalone `msb` runtime with matching
  libkrun firmware for VM checks. CI uses the pinned runtime described in
  Hosted amd64 boot verification; host operators provide and declare their own.

Required native environment (set from the reviewed standalone distribution):

```bash
export MSB_LIBKRUNFW_PATH=/absolute/path/to/matching/libkrunfw
export MSB_LIBKRUN_VERSION=REVIEWED_RUNTIME_VERSION
# Dynamically linked distributions: exact resolved dependency shown by ldd/otool.
export MSB_LIBKRUN_PATH=/absolute/path/to/loaded/libkrun
# OR statically embedded distributions, after reviewing the distribution linkage:
# export MSB_LIBKRUN_EMBEDDED=1
# macOS / thin-provisioned local Docker VM: actual existing backing disk path.
export RELEASE_VM_BACKING_PATH=/absolute/path/to/docker-vm-backing-disk
```

The verifier records linkage output and hashes the resolved runtime library, or
explicitly models libkrun as embedded in the recorded msb executable (not a
mislabelled library hash). `doctor` and a tiny fixture pull/load/inspect plus
native boot with `--boot` must pass before standard payload re-download/import.
Unsupported isolated runtime provisioning blocks the expensive standard checks.

## Consume a released image (anonymous)

Pull by digest on a host without Docker/GHCR keyring credentials for the
package. An anonymous GHCR bearer token for public pull is normal; a maintainer
credential is not allowed. The package must be public.

```bash
IMAGE=ghcr.io/gregwebs/agent-vm-standard
ARCH=amd64   # or arm64
VERSION=0.1.0
DIGEST=$(skopeo inspect --raw "docker://$IMAGE:v$VERSION" \
  | python3 -c 'import hashlib,sys;print("sha256:"+hashlib.sha256(sys.stdin.buffer.read()).hexdigest())')
skopeo copy --src-no-creds --preserve-digests --override-os linux --override-arch "$ARCH" \
  "docker://$IMAGE@$DIGEST" "oci:./registry-layout:standard"
```

`script/release/download-release.sh` and `script/release/verify-release.sh`
(see below) select the exact version/arch only; they never fall back to
`latest`, another architecture or a build.

## Download, verify and load (exact manual operations)

These are read-only, explicit interfaces. They never build, never try another
URL/product and reject missing assets/parts rather than substituting.

```bash
# Explicit download of the fixed metadata + this arch's exact assets.
bash script/release/download-release.sh --version VERSION --arch ARCH --out NEW_DIR

# Authenticate provenance, content-check, Docker numeric-UID audit and (with
# --boot) isolated native msb pull/load/inspect/boot. Needs the source SHA
# checkout and a real msb runtime.
bash script/release/verify-release.sh --version VERSION --arch ARCH \
  --assets-dir NEW_DIR --msb-bin /path/to/msb --boot
```

For manual loading, first authenticate metadata/bundles using the download
interface and use the exact signed-source checkout (do not execute unauthenticated
release code). Assemble into a **new** path; this checks each part in signed order,
full tar hash/size and OCI graph before exposing the output:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 script/release/content.py assemble-archive \
  --release NEW_DIR/release.json --arch ARCH --assets-dir NEW_DIR \
  --output /owned/new/standard.oci.tar
msb image load --input /owned/new/standard.oci.tar --tag my-owned-standard:VERSION
msb image inspect --format json my-owned-standard:VERSION
```

Do not improvise wildcard `cat`; missing/swapped equal-sized parts fail validated
assembly. Loading never selects a registry fallback. Commands and verified
statements are retained as hashed evidence; a nonzero/timeout/missing phase never
yields a passing `verification-ARCH.json`.

Native batch verification on **both** architectures is a required completion
gate before stable promotion:

1. On fresh amd64 and arm64 hosts, run the source-SHA-pinned
   `download-release.sh` / `verify-release.sh --boot` interfaces with fresh
   short private states, no GHCR credentials and a compatible standalone runtime.
   A green `verify-release-boot.yml` run satisfies the amd64 leg: the hosted
   runner is a fresh host with fresh private state and no GHCR credentials.
   Arm64 still needs a fresh native arm64 host.
2. Each run writes `verification-ARCH.json` plus hashed command/guest/probe logs
   recording source/version/run identity, the current attested `release.json`
   subject hash, index/child/config/layer identities, host/tools hashes, UID/GID
   pairs, cold-cache isolation and every check's status.
3. `python3 -B script/release/content.py check-evidence --release release.json --evidence-dir DIR`
   hash-binds both architecture records. It validates evidence; it does **not**
   prove a boot occurred.
4. A named maintainer independently reviews both direct runs' logs, records the
   evidence bundle SHA-256, source/version/index/child/archive identities and the
   decision in a linked issue/Release comment.
5. **Immediately before promotion**, anonymously re-download metadata, bundles,
   archive parts and SBOMs with `download-release.sh` into new directories on both
   architectures, authenticate signatures, and compare every payload hash and
   the attested `release.json` subject with the reviewed evidence. Run the
   verifier again to compare live index/platform version refs and both digest-downloaded
   graphs with those reviewed subjects (no rebuild/fallback). Any changed bytes,
   refs or missing/denied asset block promotion and require renewed verification
   and maintainer review. For amd64, dispatch `verify-release-boot.yml` again
   immediately before promotion; arm64 still needs a fresh native-host
   verification. Link the maintainer decision/evidence comment in the existing
   Release notes before explicitly promoting the **same bytes**:

   ```bash
   gh release edit vVERSION --prerelease=false --latest=false \
     --repo gregwebs/agent-vm-images
   ```

   Promotion replaces no asset and rebuilds nothing. If anything changed since
   review, repeat verification and review against the new bytes.

## Hosted amd64 boot verification

`verify-release-boot.yml` runs after publication in `release-standard.yml`, bound
to that run's `GITHUB_SHA`; it also accepts `workflow_dispatch(version)` and
pushes to `test/release-boot/<VERSION>[/<name>]`. All modes first invoke the local
reusable `contracts.yml` (no inputs/secrets); duplicate contracts on release calls
is intentional. Initial dispatch registration/UI depends on default-branch
visibility; registered workflows may dispatch branch/tag versions via API/CLI.
Failed contracts means boot **NOT RUN**, with no boot artifact.

Two sibling clean checkouts keep responsibilities separate: `harness/` is the
workflow commit, providing tools, runtime and initial authenticated download;
`source/` is the authenticated `release.json.source_sha`, providing its own
unchanged `verify-release.sh --boot`. No unauthenticated SHA selects code.
Initial amd64 archive/SBOM transfers repeat in the signed verifier's
`public-assets` check (v0.1.3's archive is 868,034,560 bytes per transfer).
Opposite-architecture controls, both registry graphs, layouts and tars consume
additional disk. Initial assets remain allocated when capacity is checked.

On the disposable amd64 runner only, `prepare-ci-boot.sh` reclaims exactly
`/usr/share/dotnet`, `/usr/local/lib/android`, `/opt/ghc`, and
`/usr/local/share/boost` within ten minutes, then grants KVM:

```bash
echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' \
  | sudo tee /etc/udev/rules.d/99-kvm4all.rules
sudo udevadm control --reload-rules
sudo udevadm trigger --name-match=kvm
```

It asserts readable/writable `/dev/kvm` and proves an `open(O_RDWR)`. Never run
this host mutation on an operator machine. `ubuntu-24.04-arm` has no `/dev/kvm`:
there is **no hosted arm64 leg**. Capacity failure remains failure, never prune.
For v0.1.3 the verifier requires about 51.43 GiB free after initial download,
plus successive roughly 25-GiB `/tmp` floors and measured daemon capacity;
GitHub's documented standard-runner availability does not guarantee those floors.

The reviewed latest published runtime is upstream microsandbox **v0.7.7**,
not the launcher's msb. `tool-pins.json` pins SHA-256 for `msb-linux-x86_64` and
`libkrunfw-linux-x86_64.so`; `install-ci-msb.sh` checks both hashes, version and
linkage. agentd is embedded. The explicit declaration is:

```text
MSB_LIBKRUN_EMBEDDED=1
MSB_LIBKRUN_VERSION=msb_krun 0.1.40 (statically linked into microsandbox v0.7.7)
MSB_LIBKRUNFW_PATH=<runtime>/lib/libkrunfw.so.5.6.1
```

This proves boot under the **upstream runtime**, not launcher compatibility;
agent-vm#286 owns that join. There is no source build or automatic latest lookup.

### Bumping the runtime pin

A pin bump is a reviewed source change, never an automatic CI lookup. For a new
microsandbox release `vX`:

1. Take asset SHA-256 digests for `msb-linux-x86_64` and
   `libkrunfw-linux-x86_64.so` and cross-check `checksums.sha256`.
2. Read `LIBKRUNFW_VERSION` in `crates/utils/lib/lib.rs` and `msb_krun` in
   `Cargo.lock` at tag `vX`.
3. Confirm `ldd` / `objdump -p` shows no libkrun. Otherwise switch to an honest
   `MSB_LIBKRUN_PATH` declaration instead of embedded.
4. Update the five runtime keys in `tool-pins.json`.
5. Prove the pin with a `test/release-boot/<latest published VERSION>` run at
   the proposed harness SHA before merge.

### Retained evidence and budgets

Artifact `boot-amd64-v<V>-<run>-<attempt>` is retained for 30 days:
`verification/` holds the success record and hashed logs; `release/` holds
metadata and bundles; `runtime/` holds KVM/capacity, msb provenance and verifier
console logs. No image archives or layouts are uploaded. Exactly one original
success record must match the staged record's hash and size; staged metadata is
re-authenticated and bound to source/version/subject/platform/index. **Every**
inventoried log is hash/size-verified. Copy or validation failure makes CI red
even after a successful boot. Always-upload attempts remaining diagnostics;
cancellation, runner loss or job timeout can prevent it. Missing evidence is
never success or promotion input.

The first measured hosted run was **37854354512** (job `boot-amd64`, commit
`fe240ca`): per-phase wall-clock maxima were harness checkout ~1 s, bind <0.1 s,
verifier tools 8.8 s, containerd 0.6 s, reclaim/KVM 53.6 s, runtime 1.0 s,
download/authentication/source binding 43.1 s, signed source checkout/check
0.9 s, native boot verifier 350.3 s (5 m 50 s), staging 0.3 s, staged validation
6.5 s, upload 2.9 s and summary <0.1 s. Whole-job wall clock was **7 m 51 s**;
the staged artifact was **6,194,500 bytes** (188 files). Retuned limits in
minutes are checkout/bind 5+3, tools 20, containerd 5, reclaim/KVM 12, runtime
10, download/authentication 20 (900-second whole-process watchdog), source
checkout/check 5+3, verifier 60 (3300-second watchdog), stage 5, staged
authentication/validation 10 (420-second watchdog), upload/summary 15+5.
Phase limits sum to **178 minutes**, leaving **122 minutes** inside the
**300-minute** job bound: 7 minutes for setup/post steps and transitions plus
**115 minutes** of additional safety headroom. Contracts has its separate
45-minute limit. Keep bounds at **at least 1.5× measured maxima**, including
attestations and upload/summary, with upload/overhead headroom. Re-measure when
the runtime pin, runner image or archive size changes materially; a materially
larger release archive is grounds to re-measure and raise the verifier bound,
not let it fail spuriously. Record phase maxima and maximum staged artifact
size in hosted acceptance evidence. Never drop checks or shorten capacity
floors to fit a budget; report a host/timing constraint and seek a larger/native
host instead of weakening a gate. A forced phase-timeout exercise must prove
the diagnostic tail reaches upload; this exercise is **still to be run**.

A green run is amd64 evidence for maintainer review, **not promotion**. Combine
`verification/` with native arm64 evidence and run unchanged
`content.py check-evidence`, which still requires both architectures. A failed
post-publication boot leaves the immutable prerelease published and unpromoted.

## Evidence and independent cadence

Existing native source builds, strict-egress audits, Pi/runtime and
certification gates are retained; a merged workflow or a tiny fixture is never
released-standard acceptance. `.github/workflows/release-transports.yml` (PR,
`main` push and manual; read-only, no GHCR credentials, no VM boot) runs the
loopback registry/archive/negative transport controls on both native
architectures. Passing those proves transport mechanics only.

Do not claim success for any phase that did not run. Native standard builds,
live GHCR publication, public visibility, anonymous archive/SBOM consumption
and both `msb` boots are deployment gates. The amd64 boot runs in CI after
publication; arm64 still needs a native host. Without both boot legs or the
required credentials, the release stays a prerelease and
[#264](https://github.com/gregwebs/agent-vm/issues/264) stays open. Packaging a
launcher that consumes the release is [#265](https://github.com/gregwebs/agent-vm/issues/265).
