# 0005: A committed manifest for the repository's own base inputs

Status: accepted. Maintainer decisions recorded here: the manifest is committed
and verified in the build with `sha256sum -c`, the copied extension carries a
generated default, and no `apt-get upgrade` was added. Evidence for the native
rows lives in [CONTRIBUTING](../../CONTRIBUTING.md#expensive-acceptance-and-ci)
and the pull request that introduced this ADR.

## Context

The parent pins in [the parent-pin inventory](../../images/parent-pins.json) bind
*external* parents: Debian, the Go builder and the released standard image are
addressed by digest and reviewed. The remaining edge is local. `images/standard/Dockerfile`
and `examples/base-extension/Dockerfile` build `FROM agent-vm-base:local`, and that
tag can point at a base built from any checkout of this repository — including one
whose installers, pnpm pin, recipe-contract source or apt baseline differ from the
tree the standard image was built from.

Two ways to close it were rejected:

- **Try to identify the image bytes.** A local base is not byte-reproducible:
  apt state and caches, platform, build arguments, Zellij overrides and a
  persisted build CA all move the image ID. A byte-identity scheme would either
  forbid those inputs or fail for honest rebuilds.
- **Require an immutable image ID only.** A launcher could pin the exact ID, but
  that is a distribution decision about a published artifact, not a repository
  gate, and it would not protect the documented local workflow.

## Decision

Commit a manifest of the sources the base is built from, re-hash it inside the
build, and let the base stamp its own manifest hash so a local consumer can
compare:

```
reviewed external pin ──Docker digest check──► base
tracked base files ─────host generator───────► images/base-inputs.sha256
                                                  │ sha256sum -c in base and standard
                                                  └─ manifest hash ──► parent stamp == consumer expectation
```

- `images/base-inputs.sha256` is the committed, sorted list of every git-tracked
  file under a fixed root set (`.dockerignore`, `Dockerfile`, `apt-policy`,
  `install-build-ca.sh`, `install-pnpm.sh`, `install-zellij.sh`, `pnpm`,
  `recipe-contract`, `verify-base-inputs.sh`). `script/build/base-inputs.sh`
  writes it and the extension default; it never runs during a build.
- The base's last RUN (`images/verify-base-inputs.sh`) validates the manifest
  grammar, re-hashes the entries with `sha256sum --strict --check` and writes
  `sha256:<manifest hash>` to `/opt/agent-vm/base/source-identity`.
- The standard recipe runs the same verification as its first product RUN and
  compares the inherited stamp with this checkout's manifest. The copied
  extension compares its generated `AGENT_VM_BASE_IDENTITY` default with the
  inherited stamp.

### Why a manifest plus `sha256sum -c`

- **No new language or CLI.** A `sha256sum` checklist is a format the build
  already has (`pnpm-inputs.sha256`, `keyrings.sha256`), readable by a human and
  by a reviewer's `--check` diff. The gate needs no Go, no helper binary and no
  native step, so it runs before apt and before any installation.
- **One list, two consumers.** The base and the standard image mount the same
  files at the same paths, so a single manifest cannot describe two different
  input sets; `script/test/image-sources` refuses a recipe, mount list or root
  list that drifts from the other.
- **Not a tree walk.** `sha256sum -c` checks exactly the listed entries. A build
  cannot silently consult whatever happens to be in the context, and a file added
  to the context without being regenerated does not become "covered".

### Consequences accepted

- **Explicit regeneration.** Editing a listed input, or adding one, requires
  `bash script/build/base-inputs.sh --write` and a reviewed manifest/default
  diff. `--check` runs in both contracts modes, so drift fails on the host before
  a build can pass. A newly added *tracked* file is caught by that gate; the
  in-build check cannot discover a missing entry on its own.
- **Content-only, untracked boundary.** Modes and mtimes are deliberately not
  hashed (`core.fileMode` cannot change the identity), untracked and ignored files
  are outside it, untracked content pulled in by a directory `COPY` is not
  described, and the manifest lists files, so an empty directory is invisible.
- **Effective build arguments and the exporter are outside it.** A base or
  descendant built with other `--build-arg` values, labels outside the verified
  set, or another exporter (metadata, SBOM attachment) still passes. That is why
  the identity is source identity and never "all inputs".
- **Churn in the copied extension.** The generated default changes whenever a
  listed input changes, so the example's Dockerfile is part of the diff. A user
  who copies the directory carries their checkout's default and must override it
  explicitly (`--build-arg AGENT_VM_BASE_IDENTITY=…`) to accept another base.
- **Trust, not authentication.** The daemon store, the inherited stamp and an
  externally supplied host-helper context are trusted. A forged stamp cannot
  attest descendant modifications, and the stamp never claims image-byte
  identity: same sources can produce a different image ID, and such bases and
  their descendants pass. A consumer needing exact bytes must select and trust an
  immutable image ID separately.
- **`docker build -f OTHER` is outside it.** Only the normal `-f images/Dockerfile`
  path is covered, where the mounted context Dockerfile is the recipe used.

## Evidence

- `script/test/base-inputs.py` (hermetic): golden manifest and default,
  byte/add/delete/rename drift and repair, mode flips and `core.fileMode=false`,
  symlink/gitlink/path refusals, untracked files excluded, manifest grammar
  refusals, stamp presence/format/mismatch, and the `--write` transaction
  (failed copy, failed rollback, signal).
- `script/test/image-sources` (Rust): mount/root/position rules, `.dockerignore`
  that would hide a listed input, Dockerfile-specific ignore overrides, the
  extension's ARG/check shape and mutation controls.
- `script/test/parent-pins.py`: a refresh co-publishes the manifest and the
  default, refuses untracked collisions at either, and restores both after a
  failed or an interrupted publication.
- `script/test/parent-identity.sh` (native, both architectures in CI): the base
  stamps this checkout's identity, a wrong parent digest never resolves, a
  stamp-less base fails both consumers, a regenerated manifest over old sources
  fails both consumers, the probe proves the regenerated identity is the one that
  matches, untracked extras and mode flips keep it, a content change without
  regeneration is refused, and the host-helper delivery yields the same stamp.
