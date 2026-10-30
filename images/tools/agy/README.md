# agy installer

Installs the Google Antigravity CLI (`agy`) from a committed, hash-only content pin.
`agy` is the seventh agent the standard image installs and certifies.

- [`../../standard/Dockerfile`](../../standard/Dockerfile) holds
  `ARG AGENT_VERSION_AGY` (default `1.4.11`) and labels the image
  `org.agent-vm.version.agy=<that value>`. The label records the *selection*; the
  build gate decides health.
- `install-agy.sh` validates the slot as bare `X.Y.Z`, refuses an architecture
  other than `amd64`/`arm64`, reads the version the committed
  [`pin.json`](pin.json) selects through the shared validator, and refuses any
  other exact version. It then hands the install to the shared release-artifact
  installer, which is the one place that fetches, verifies and publishes.
- `verify-agy.sh` runs a bounded `agy --version` and accepts exactly
  `1.4.11\n` on stdout with empty stderr; it also requires
  `AGY_CLI_DISABLE_AUTO_UPDATE=1` and the variable's name to still appear in the
  binary, requires the published file to be mode `0755` and owned by `0:0` (the
  digest binds the bytes, not the permission bits that decide who may replace
  them), then audits any-UID access and writes the `installed` record.

`pin.json`, `evidence/` and the two scripts are bind-mounted, not COPYed: only
`/usr/local/bin/agy` persists. Neither an `antigravity` name nor a `~/.gemini`
seed is shipped.

## The selection is a content pin, not a version

There is no build-time `latest` query, mirror or fallback. The pin names one
release URL per architecture and the sha256 of its archive; `evidence/` holds
the raw GitHub release document and both Google updater manifests, and the build
first re-checks that these committed records still agree with the pin, then
accepts only the committed digests. `script/dev/pin-agy.py` writes all five files
in one transaction and only when the downloaded bytes equal **both** GitHub's
asset digest and Google's manifest sha512. See
[ADR 0012](../../../docs/adr/0012-agy-hash-only-artifact-pin.md) for why that is
the weakest guarantee in this repository, and
[CONTRIBUTING](../../../CONTRIBUTING.md#agy-hash-only-pin) for the maintenance
commands.

## Recorded transcript

Captured from `agy` 1.4.11 on linux/arm64 (`debian:13-slim`, `--network none`,
uid 12345:23456), exit 0:

```text
1.4.11
```

Exactly one line and one newline, nothing on stderr. A second line, a missing
final newline, a warning beside the correct version or a nonzero exit fails the
build rather than being guessed at.

## Auto-update switch

The image sets `ENV AGY_CLI_DISABLE_AUTO_UPDATE=1`. agy would otherwise replace
the reviewed binary from the background inside a guest, which no build could
certify. The gate greps the installed binary for the variable name, so a release
that renames or drops the switch fails the bump instead of shipping a silently
self-updating CLI.

## Out of scope

- `~/.gemini` persistence and host credential wiring: a launcher concern tracked
  in [gregwebs/agent-vm#474](https://github.com/gregwebs/agent-vm/issues/474).
- musl, macOS and Windows assets; any version the pin does not select.
- Telemetry defaults, `agy install` shell setup and the upstream `install.sh`.

## License

agy is proprietary Google software shipped under Google's terms of service, not
an open-source license. It is included by explicit operator decision, as are
Claude and Copilot; this repository makes no redistribution claim.
