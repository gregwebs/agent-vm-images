# Pi runtime payloads

The [standard Dockerfile](../../standard/Dockerfile) installs the reviewed
manifest/lock (`1.4.0`) and the separate bridge project (`0.8.0`), then copies
the wrapper, mandatory warning and Codex extensions, and seed hook. The
[installer documentation](../README.md#pi-integrity) explains the hashed-lock
contract, the shrinkwrap-era sibling checks and bridge flags.

`pi.sh` stays separate from the installation so image authors can add extensions
under `/opt/agent-vm/pi-extensions` without replacing the required warning. It
enforces `PI_SKIP_VERSION_CHECK`, preserves positional subcommands and requires
the warning to load; removing or breaking it fails closed. The bridge's explicit
extension is optional only via the wrapper's documented opt-out, not during
standard installation/certification. The third extension, `agent-vm-codex`, exposes
host Codex placeholders as a distinct provider.
Its existence check makes a removed file cost the provider, not Pi.
There is no default trust/telemetry change.

The bridge's `--legacy-peer-deps` lock and `--omit=optional` install avoid a second
Pi and native Claude. The seed hook merges the image Claude executable path
into project-owned Pi state without clobbering user configuration. Hooks are
invoked by the caller, not Docker CMD. Registration probes are not authenticated
session evidence.

## Proposed authenticated defences (#52, inactive)

Five build-only modules are committed here without being wired into the
installation. Nothing below changes the current npm install, inputs or locks.

| Module | Owns | Never owns |
|---|---|---|
| `import-shrinkwrap.mjs` | Authenticated archive data -> an npm-shaped project lock for `pnpm import`, plus the expectation-vs-import delta | Download, npm, YAML or duplicate-key parsing (`recipe-contract pnpm-json`), guessed hashes, an allowlist |
| `archive.py` | Whole-archive preflight and bounded extraction, and the per-invocation rehash of a retained provider tarball | The head-of-line SRI check of a download, any network call |
| `installed-graph.mjs` | Lock-derived instances, dependency and bin-link expectations, non-following inventories, the reviewed family shapes | Reading identity out of `.pnpm` entry names, parsing YAML, trusting an installed link |
| `check-installed-bytes.mjs` | `PREFIX SIBLING EXTRACTED_PACKAGE`: archive-versus-installed bytes, modes and derived links for every reachable sibling | Download, a caller allowlist, deriving expectations from the observed tree |
| `check-runtime.mjs` | genai, protobufjs and the chord-context esbuild/native probes from the real installed resolution context | Installing, a script fallback, a version-threshold shape guess |

The independently reviewed expectations are the historical five-sibling table
(`chord` -> `esbuild`, `pi-agent-core` -> `pi-ai`,`yaml`, `pi-ai` ->
`anthropic-ai-sdk`, `pi-telemetry` -> none, `pi-tui` -> `marked`) and the modern
six-sibling table (`pi-agent-core` -> `pi-ai`, `pi-ai` -> `anthropic-ai-sdk`,
`pi-codemode` -> none, `pi-mcp` -> none, `pi-telemetry` -> none, `pi-tui` ->
`marked`). They are test expectations only: the checkers derive their own maps
from authenticated provider manifests.

Operands and seams the activation commit must supply: `AGENT_VM_CONTRACT_DIR`
(the composed `recipe-contract`), `AGENT_VM_PI_TARGET_PLATFORM`
(`linux/amd64` or `linux/arm64`, never the host), the extraction parent layout
`PARENT/<package>/<version>/` with `PARENT/evidence/<package>/<version>/<sha512hex>.tgz`,
and read-only mounts of the Pi source directory. Suites:
`node script/test/pi-shrinkwrap.mjs` and `node script/test/pi-installed-bytes.mjs`.

Upgrade with `bash images/tools/pi/upgrade-pi.sh VERSION` and
`bash images/tools/pi/bridge/upgrade-bridge.sh VERSION`;
see [CONTRIBUTING](../../../CONTRIBUTING.md#selection-ownership-and-maintenance).
Review all integrity entries, the nested sibling layout (empty for Pi 1.0.4+,
otherwise exactly the reviewed five at the pin) and the bridge loader-alias
exclusions. Do not regenerate locks during migration. Historical
extension compatibility rationale is in the pinned upstream
[bridge ADR](https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0023-image-owned-pi-extension-packages.md).
