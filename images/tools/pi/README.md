# Pi runtime payloads

The [standard Dockerfile](../../standard/Dockerfile) installs the reviewed
manifest/lock (`0.87.1`) and the separate bridge project (`0.8.0`), then copies
the wrapper, mandatory warning extension and seed hook. The
[installer documentation](../README.md#pi-integrity) explains the five sibling
hashes, tarball/tree comparison, exact override semantics and bridge flags.

`pi.sh` stays separate from the installation so image authors can add extensions
under `/opt/agent-vm/pi-extensions` without replacing the required warning. It
enforces `PI_SKIP_VERSION_CHECK`, preserves positional subcommands and requires
the warning to load; removing or breaking it fails closed. The bridge's explicit
extension is optional only via the wrapper's documented opt-out, not during
standard installation/certification. There is no default trust/telemetry change.

The bridge's `--legacy-peer-deps` lock and `--omit=optional` install avoid a second
Pi and native Claude. The seed hook merges the image Claude executable path
into project-owned Pi state without clobbering user configuration. Hooks are
invoked by the caller, not Docker CMD. Registration probes are not authenticated
session evidence.

Upgrade with `bash images/tools/pi/upgrade-pi.sh VERSION` and
`bash images/tools/pi/bridge/upgrade-bridge.sh VERSION`;
see [CONTRIBUTING](../../../CONTRIBUTING.md#selection-ownership-and-maintenance).
Review all integrity entries, exact five direct sibling versions and the bridge
loader-alias exclusions. Do not regenerate locks during migration. Historical
extension compatibility rationale is in the pinned upstream
[bridge ADR](https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0023-image-owned-pi-extension-packages.md).
