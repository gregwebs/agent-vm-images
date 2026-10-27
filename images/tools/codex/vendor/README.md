# codex — vendored installer

`install.upstream.sh` is a verbatim snapshot of OpenAI's published Codex
installer from the exact release it installs:

- source: `https://github.com/openai/codex/releases/download/rust-v0.159.3/install.sh`
- retrieved: 2026-10-01
- SHA-256: `150e3cf675682efeaac115aa3747add3f27887896d04ce6d0b56478d8b428bf6`

It is provenance, not an executable guard target, and is never run. The snapshot
is byte-identical to the release asset.

## License / attribution

The upstream snapshot is redistributed here under the Codex project's Apache
License 2.0 (SPDX `Apache-2.0`, Copyright 2025 OpenAI), not under this
repository's own license. The full license text is in `LICENSE` and the
provenance/attribution record is in `NOTICE`.

`install.sh` is the runnable patched copy. `install.patch` is the reviewable
patch; applying it to the snapshot reproduces `install.sh` byte-for-byte (follow-up audit suite:
`script/test/codex-installer.sh`). `git apply` it from a directory holding the
snapshot renamed `install.sh`.

## Trust boundary

This script does not decide which bytes are Codex. The selection authority is the
committed closure beside it — `../pin.json`, `../evidence/` and `../codex_pin.py`.
The patch below removes every live release-metadata and checksum lookup, takes the
exact URL and SHA-256 from `codex_pin.py select`, and runs `codex_pin.py
preflight` (bounded member policy, package metadata, ELF machine) before `tar`.
A build therefore never asks upstream what the expected bytes are, and a jointly
rewritten metadata/checksum/archive fails on the committed digest.

Cryptographic verification is not performed here and is not claimed: it happens
once, on a maintainer host, with the reviewed `cosign`. Policy:
[image input policy](../../../../docs/image-input-policy.md#codex-standalone-payloads)
and [ADR 0009](../../../../docs/adr/0009-codex-payload-pins.md).

`chmod`/mode repairs keep the result usable by any uid; they are not a
supply-chain guarantee.

## Patch hunks

1. **Exact version only.** `CODEX_RELEASE` has no `latest` default;
   `normalize_version` no longer maps empty/`latest` to `latest`, and
   `validate_version` rejects empty and floating values instead of returning
   early. The owning hook validates first; this is defence in depth.
2. **releases.openai.com path removed.** `RELEASES_BASE_URL`,
   `PREFER_RELEASES_OPENAI_COM`, `releases_url_for_asset`,
   `resolve_release_from_releases` and the fallback URLs are gone: builds always
   use the exact GitHub release tag.
3. **`download_file` routes through the shared classified helper**
   (`images/recipe-contract/download.sh`), so a transport failure is a receipt +
   75 and every other failure is hard.
4. **`download_file_with_fallback` is fallback-free and manifest-free.** There is
   exactly one committed asset and one committed digest: the single download is
   hashed immediately, any failure (75 included) propagates, and a mismatch is
   never refetched. There is no second endpoint to fall back to.
5. **No latest resolution and no metadata fetch.** `resolve_release`,
   `resolve_release_from_github`, the metadata parser, the asset-digest resolver,
   `download_text`, the checksum download and the legacy npm dispatch are gone.
   `resolve_release` normalizes and validates the exact release, then parses the
   four fields `codex_pin.py select` prints. The auto-updater guard that remains
   only compares the (already exact) `RELEASE` and has no lookup.
6. **Native subprocesses never see the transport receipt.** The script keeps the
   receipt path privately (`AGENT_VM_RECEIPT_PATH`) and `unset`s
   `AGENT_VM_TRANSPORT_RECEIPT`, handing it only to `download.sh`. A native
   `codex` exit 75 (or one that writes the receipt path) is therefore a hard
   failure, never a transport classification -- the plan requires native code
   never to receive the receipt environment.
7. **Committed-pin seam and a fresh-install precondition.** `CODEX_PIN_DIR` is
   required (there is no default), so a direct invocation of this patched script
   fails before it downloads anything. An existing `packages/standalone` root is
   refused before any probe, so a previously installed same-version executable
   cannot satisfy the version report and skip the content check. `preflight` runs
   at the top of `install_package_release`, before the staging directory exists.
8. **Bounded native probes.** `version_from_binary` and `verify_visible_command`
   run the native `--version` through the shared bounded, status-preserving
   `run-report.sh` (60s, `--kill-after`), so a hung or TERM-ignoring native
   fails the install instead of hanging the build before the verify gate runs.

The owning recipe is `images/standard/Dockerfile`, with read-only canonical
helper mounts including the whole `tools/codex` directory. Evidence:
`script/test/codex-installer.sh` (hook and pin gate, patch reproduction),
`script/test/vendored-installers.sh` (the real patched script, exact request
streams) and `script/test/codex-pins.py` (loader, preflight budgets, writer).
