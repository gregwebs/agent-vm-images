# Node package defaults

The base supplies **stock, unmodified pnpm 11.11.0** from a reviewed registry
archive. [The module](../images/pnpm/README.md) owns exact provenance and update
procedure. The prefix is root-owned, read/traverse for numeric UIDs, and outside
HOME; it is image material, not writable user cache or global-install space.

```text
Before: base Node/npm/npx → standard private pnpm
After:  base authenticated pristine pnpm → standard inherited launcher
        /usr/local/bin/{pnpm,pnx,pnpx} → five env defaults → exec stock entrypoint
        writable HOME → stock per-user cache/state/store
```

`pnpm` is the preferred/default install path, not a restriction. `pnx` and
`pnpx` use upstream's dlx entrypoint; they execute the requested bin, not npm's
npx. **Real npm/npx are untouched** and retain native lifecycle semantics.

## Defaults and overrides

| Uppercase environment key | Default |
|---|---|
| PNPM_CONFIG_MINIMUM_RELEASE_AGE | 1440 (minutes) |
| PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT | true |
| PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME | false |
| PNPM_CONFIG_TRUST_POLICY | no-downgrade |
| PNPM_CONFIG_IGNORE_SCRIPTS | true |

Unset **or empty** values get defaults. Nonempty valid typed caller values win:
numeric age `0` is valid; booleans must be literal `true`/`false`. Uppercase
`PNPM_CONFIG_*` is the supported interface; pnpm also accepts lowercase
`pnpm_config_*`, and either case overrides the default (invalid types do not).
CLI beats env for one invocation, e.g.
`pnpm --config.minimum-release-age=7 config get minimum-release-age`.
Explicit minimumReleaseAge already implies strictness in this pinned version;
the launcher still exports explicit STRICT=true. Young exact/ordinary frozen
locked resolutions, missing registry time and metadata provenance downgrades
are hard failures; there is no npm retry or soft healthy partial install. A
range or `@latest` does **not** fail: it resolves to the newest *eligible*
version, so a too-young newest release is skipped in favor of an older eligible
one rather than rejecting. Registry provenance comparison is **not
cryptographic attestation verification**.

### Two maintainer-approved deviations from the literal issue

1. **Direct overrides of these five settings require caller env or CLI.** The
   launcher env tier outranks direct global/user YAML and project
   `pnpm-workspace.yaml`/`.npmrc` assignments. Project age10 loses to image1440;
   caller env9999 wins both; CLI7 wins env9999. Auxiliary project
   `minimumReleaseAgeExclude`, `trustPolicyExclude`, and `trustLockfile: true`
   can weaken or bypass age/trust checks **without changing the five values**.
   These are explicit escape routes, not safe defaults. Ordinary/default cold
   frozen installs still verify policy.
2. **Unreadable HOME is unsupported.** Unknown UID inheriting `/root` can hit
   stock early EACCES. There is no discovery adapter or fallback; runtime must
   provide a writable HOME, as with `docker run --user 12345:23456 -e HOME=/tmp`.
   Writable numeric-UID/HOME-overmount support is not arbitrary-HOME support.

## Scripts and requested binaries

ignoreScripts=true suppresses entry, dependency and project installation hooks,
including dlx's auto-approved entry package. Stock dependency-build denial alone
would not suppress dlx entry hooks. The requested bin still executes: choosing a
bin is permission to execute it, independent of install-hook policy.

Opt-in needs `PNPM_CONFIG_IGNORE_SCRIPTS=false` **and** narrow build approval.
For local installs use an `allowBuilds` map approving the exact entry and
its script-bearing dependencies in `pnpm-workspace.yaml`. Global installs do
not read that invoking project's approval map; use the pinned global command's
narrow approval flags, e.g.:

```sh
PNPM_CONFIG_IGNORE_SCRIPTS=false pnpm add -g \
  --allow-build=fixture-hooks --allow-build=fixture-dependency fixture-hooks@1.0.0
```

For dlx (and pnx/pnpx), use `--allow-build=fixture-dependency` before the requested
package together with `PNPM_CONFIG_IGNORE_SCRIPTS=false`; upstream auto-approves
the entry. This explicit-approval evidence covers **registry** packages, which do
run approved hooks; linked `file:` dependencies still fail
`ERR_PNPM_IGNORED_BUILDS` under the same `ignore-scripts=false` + approval
(stock pnpm linked-dependency behavior). Project `ignoreScripts: false` alone
cannot override image env.
An allowBuilds map is a separate approval input, not an override of the five
settings. CLI overrides are not re-exported: a CLI ignore-scripts=false run can
execute approved hooks while its bin/nested pnpm inherits IGNORE_SCRIPTS=true.
Observations of exported values apply to bins spawned by that pnpm invocation,
not global bins subsequently launched from the caller shell. Real npm does not
consume PNPM-prefixed keys.

## Writable native state and global bins

Leave HOME, XDG values and config/auth discovery caller-owned. For global use:

```sh
export PNPM_HOME="$HOME/.local/share/pnpm"
mkdir -p "$PNPM_HOME/bin"
export PATH="$PNPM_HOME/bin:$PATH"
pnpm bin -g
export PATH="$(pnpm bin -g):$PATH"
```

A subprocess cannot change its parent's PATH. Stock path overrides remain
available; explicit unwritable config/state choices fail visibly rather than
triggering relocation. A writable HOME overmount hides old state/config but not
the image launchers. No image-wide PNPM_HOME or shared mutable /opt cache is set.

## Private registries and limits

Registries missing timestamps need explicit exceptions for **both age and
trust**, not just age0 or ignore-missing-time. A broader whole-invocation opt-out
is `PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=true` together with
`PNPM_CONFIG_TRUST_POLICY=off`; this weakens safety and should be exceptional.
Scoped age/trust exclusions are preferable. The native fixture verifies:

```sh
PNPM_CONFIG_MINIMUM_RELEASE_AGE_EXCLUDE='["@private/*"]' \
PNPM_CONFIG_TRUST_POLICY_EXCLUDE='["@private/*"]' \
  pnpm install
```

These JSON arrays are parsed by stock 11.11.0. They scope both exceptions to
the named private scope, without changing the five image defaults.

Alternate managers, direct immutable entrypoint invocation, explicit env/CLI,
auxiliary project exclusions/lock trust, non-registry dependencies, explicit run
commands and manager switching bypass these preferences. Manager switching is an
explicit escape, not a hidden one: `corepack enable`/`corepack pnpm` and running
`/opt/agent-vm/pnpm/dist/pnpm.mjs` directly both skip the launcher defaults.
These are preferences, not enforcement. Disabling scripts does
not unbuild previously approved caches; script verification uses cold state.
Package age and metadata provenance comparison are not complete security.

## Maintained OpenCode consumer

The [OpenCode Claude project](../images/tools/opencode/README.md#image-owned-claude-plugin)
is the first new standard-image pnpm consumer. It uses the actual base launcher,
a reviewed v9 `pnpm-lock.yaml`, isolated unhoisted modules, a frozen production
install, `--no-optional`, and disabled scripts/hooks. It grants no build approval,
age/trust exclusion, lock-trust bypass or alternate-manager fallback. Maintained
preparation/installation rejects ambient package-manager overrides except exact
base defaults. This does not remove the runtime caller escape routes above.

The external content-addressed store and private HOME/XDG state are disposable;
the installed `.pnpm` virtual store is not. The plugin's two direct dependencies
share one SDK instance and one Zod peer/dependency instance. Two reviewed missing
SDK peers remain absent; the runtime probe, handoff and real CLI initialization
prove this version-specific choice rather than general peer compatibility.
Frozen installation can request metadata even when preparation reused bytes.
Policy, lock/configuration, integrity and network failures all stay hard through
`images/recipe-contract/run-pnpm.sh`; no receipt is written. The shared
runner/parser and isolated-layout checks also support the dsh consumer below.

## Maintained dsh consumer

dsh now uses the base launcher for image-owned installs with all five defaults
explicit in the owning subprocess and locked-resolution flags. Its workspace
retains only `chokidar@4.0.3` as a reviewed trust exception; no age or script
exception is granted. Host preparation follows the same policy. Retained graph
records are frozen, while changed root-closure selections may replace
transitives; runtime callers' documented escape routes above are unchanged. Pi,
bridge and Copilot migration remains pending. The freeze rule is
[ADR 0003](adr/0003-dsh-graph-freeze.md); the reviewed npm→pnpm delta is the
[migration ledger](standard-pnpm-migration.md).

Product limitation for **using** the installed dsh: a **noexec TMPDIR is not
supported by the dsh web surface**. `dsh web` with the default `live` reload
boots `cordis-plugin-hmr`, whose loader copies its prebuilt addon into
`$TMPDIR/node-addon-native-custom-loader-<uid>/native-cache` and `dlopen`s it
there, so a TMPDIR mounted `noexec` (for example a `--tmpfs` HOME) makes the web
surface fail to start. Point `TMPDIR` at an exec-capable directory instead. Do
not work around it with `node --expose-internals`: that skips the native-binding
path real users run.

## Chrome example consumer

The [Chrome example](../examples/layers/README.md#pinned-mcp-package) installs
`chrome-devtools-mcp` at build time from four committed inputs. The five
settings above are written explicitly by
`examples/layers/chrome-devtools/chrome-devtools-mcp.sh` instead of being
inherited from the parent image. Its environment guard is stricter than the
`pnpmenv` contract: this build rejects equal-valued and empty managed names,
which the standard guard accepts, because a Docker build of the example has no
legitimate source for them. Runtime no longer invokes a package manager by
design: the wrapper maps the launcher's `npx -y chrome-devtools-mcp@<installed>`
argv onto the installed bin and refuses other direct manager commands.

## Build-only graph primitives

The [bounded parser ADR](adr/0001-build-only-pnpm-graph-parser.md) documents
build-only validation for image-owned install migrations. It never
installs, resolves selections or weakens the five defaults. OpenCode keeps its
byte-only checker; dsh adopted the graph parser in #51, while Pi/bridge/Copilot
still use their current npm paths until their owning slices. The
[failure ADR](adr/0002-pnpm-terminal-failure-boundary.md)
records the evidence and acknowledgement gate for a proposed later bridge
transport-contract narrowing; every current pnpm failure remains hard.
