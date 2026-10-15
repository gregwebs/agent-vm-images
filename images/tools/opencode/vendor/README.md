# opencode — vendored installer

`install.upstream.sh` is a verbatim snapshot of OpenCode's published installer:

- source: `https://opencode.ai/v2/install`
- retrieved: 2026-10-15
- SHA-256: `6ef97e3d5b8782179efc2b04e6ce2e90ced23675df5792c9ea4f4a0465531865`

It is provenance, not an executable guard target, and is never run.

## License / attribution

The upstream snapshot is redistributed here under the OpenCode project's MIT
License (SPDX `MIT`, Copyright (c) 2025 opencode), not under this repository's
own license. The full license text is in `LICENSE` and the
provenance/attribution record is in `NOTICE`.

`install.sh` is the runnable patched copy. `install.patch` is the reviewable
patch; applying it to the snapshot reproduces `install.sh` byte-for-byte (follow-up audit suite:
`script/test/opencode-installer.sh`). `git apply` it from a directory holding the
snapshot renamed `install.sh`.

## Trust boundary

The snapshot is pinned; the exact release archive it downloads is fetched at
build time. Upstream publishes no separate checksum for the archive, so integrity
comes from TLS plus the exact-version registry tarball URL, not independent signing. `chmod`/mode
repairs keep the result usable by any uid; they are not a supply-chain guarantee.

## Patch hunks

1. Require the recipe contract and an exact version. Remove the latest lookup,
   package-existence probes and old-scope fallback; use only `@opencode`.
2. Route the download through `images/recipe-contract/download.sh`. Remove the
   progress helpers and inherited-command version probe.
3. Use private scratch with exit cleanup and terminating signal traps.
4. Quote the `GITHUB_PATH` redirect.

The v2 archive layout is `package/bin/opencode`. Preserve the upstream
`opencode2` forwarding shim. TLS and an exact tarball URL are the archive trust
boundary: this route does not consume registry integrity or independent signing.

The owning recipe is `images/standard/Dockerfile`. The hermetic
`script/test/opencode-installer.sh` checks patch reproduction and the owning
hook; `script/test/vendored-installers.sh` exercises the runnable installer.
Native restricted-egress evidence is a separate acceptance requirement.
