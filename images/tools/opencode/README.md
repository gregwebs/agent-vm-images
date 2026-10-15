# OpenCode layer

`vendor/` holds the reviewed installer snapshot and patch; `install-opencode.sh`
and `verify-opencode.sh` are the owning hook and build gate. The
[installer documentation](../README.md#trust-limits-and-reviewed-locks) is
canonical for that contract. This file covers runtime shape.

## One shared background server per HOME

Upstream reference: <https://opencode.ai/v2/docs/cli> (Background service).

OpenCode 2 is a server plus local clients. A plain `opencode`, `opencode run` or
TUI invocation discovers or starts **one shared background server for the HOME**,
and every local client connects to it. That server owns sessions, configuration,
plugins, permissions and tool execution, so a client's own environment is not
what decides which plugins load.

| Invocation | Effect |
|---|---|
| `opencode --standalone` | Private server instead of the shared one |
| `opencode --server URL` | Connect to a specific server; the registration file is not consulted |
| `opencode service set disabled true` | Private servers become the default; stops the shared server |
| `opencode serve --stdio` | Embed a server on stdio instead of TCP |

There is **no unix socket**. The server listens on TCP loopback. The default port
is 49374 (`0xC0DE`) on release channels, `0xC0DF` on local builds, otherwise
`10000 + hash(channel) % 50000`.

### Discovery is one file

`$XDG_STATE_HOME/opencode/service.json` — `~/.local/state/opencode/service.json`
by default, mode `0600` — is the entire discovery contract:

```json
{
  "id": "f424367d-9dfa-495d-a7b7-0c845a92baec",
  "version": "2.0.29",
  "url": "http://127.0.0.1:49374",
  "pid": 19978,
  "password": "<32 random bytes, base64url>"
}
```

Verified on 2.0.29: a fresh HOME containing only this file discovered a running
server; the same command without it reported `stopped`.

`$XDG_CONFIG_HOME/opencode/service.json` — `~/.config/opencode/service.json` — is
the service's own configuration (`port`, `hostname`, `password`, `cors`, `env`,
`disabled`), written by `opencode service set`/`unset`. `OPENCODE_CONFIG_DIR`
relocates it, because it is the global config directory.

### Reusing a server across containers

| Path | Holds | Needed to reuse |
|---|---|---|
| `~/.local/state/opencode/` | `service.json` registration, `locks/` | **Required** — this is discovery |
| TCP loopback port (49374) | The transport | **Required** — shared netns, or a published port |
| `~/.config/opencode/service.json` | `port`, `password`, `disabled`, `env` | Only for a client that may start the server, or to force `disabled: true` |
| `~/.local/state/opencode/service.json.pty-handoff` | Persistent-terminal ticket | Only to keep PTYs across a server restart |
| `~/.local/share/opencode/` | `opencode.db`, `log/`, `repos/` | Optional for a client; the server is the only writer |
| `~/.cache/opencode/` | Plugin tree, `bin/` | Only for the process that runs the server |

Four traps, in order of how much they cost:

1. **The registration file re-checks itself every 5 seconds.** A daemon that
   finds its registration replaced shuts itself down. A second container that
   starts its own service, or runs `opencode service stop`, can therefore kill
   the first one. Sharing the state directory shares that hazard.
2. **The registration file is a bearer credential** to a server that edits files
   and runs commands. Moving it between containers moves that authority.
3. **Two servers must never share one data directory.** `opencode.db` is
   SQLite and server-owned; two writers is corruption, and a 2.x→1.x downgrade
   on that database is an already-reported corruption path.
4. **`--server` bypasses the version gate.** A discovered service whose
   `version` differs is replaced or rejected; a `--server` target only warns,
   and needs `OPENCODE_PASSWORD` (legacy: `OPENCODE_SERVER_PASSWORD`) matching
   the server's configured password.

Reaching a server on another host needs it started with a non-loopback
`--hostname` and the port published; clients then use
`opencode --server http://HOST:PORT`.

## Position

Single isolated containers are the supported and certified shape: each guest
HOME gets its own server, and the image's certification probe spawns a private
`opencode serve --stdio` rather than reusing an ambient one. A centralised,
sandboxed OpenCode server is documented above because there is plausible
interest in it; it is not supported, certified or security-reviewed here.

## Image-owned Claude plugin

OpenCode is pinned to v2.0.29 and `@openchamber/opencode-claude` to 1.4.1.
This is the named exception in the [installer policy](../README.md); credential
replay is prohibited, and no general community-plugin exemption is implied.
Registration is not account eligibility, quota, authentication or a successful
Claude turn. The gate performs no authenticated turn.

```text
/opt/agent-vm/opencode/opencode.json (OPENCODE_CONFIG)
  → /opt/agent-vm/opencode-plugins/index.js
  → locked plugin → Agent SDK → PATH's /opt/agent/.local/bin/claude
```

The adapter re-exports the installed plugin. v2 loads local **directories** via
`server`/`index`, not a package's main or a bare configured JS file. npm plugin
specifications can instead populate the guest cache; the image integration does
not invoke a runtime package manager or preseed that cache. HOME overmounts
cannot hide the adapter, dependencies or supplemental config.

`OPENCODE_CONFIG` adds a file after global configuration and before project-root
and `.opencode` configuration. Inline/managed settings have higher precedence.
Plugin entries compose as ordered operations: a project can disable this plugin
with `{"plugins":["-openchamber.claude-code"]}`. Replacing or emptying
`OPENCODE_CONFIG` is a caller opt-out; there is no wrapper enforcing it. Do not
set `OPENCODE_CONFIG_DIR` merely to load this plugin.

No seed hook or new `/agent-vm-state` mapping is installed. Runtime owns writable
HOME/XDG and Claude login provisioning. OpenCode keeps global configuration under
`~/.config/opencode`, cache under `~/.cache/opencode`, data/database under
`~/.local/share/opencode`, and service registration under `~/.local/state/opencode`.
The plugin uses `~/.local/share/opencode-claude` for models.json, sessions.json,
logs and rate-limit state, following XDG_DATA_HOME. Project configuration remains
in `opencode.json(c)` and `.opencode/opencode.json(c)`.

### Locked installation and upgrades

The private project has exactly two direct dependencies: plugin 1.4.1 and
`@anthropic-ai/claude-agent-sdk` 0.3.281. A workspace override selects Zod 4.6.7.
The v9 pnpm lock contains eleven package and eleven snapshot records, including
eight optional SDK platform packages; only three package instances are installed.
`--no-optional` avoids a duplicate Claude executable. The plugin resolves Claude
on PATH and hands the SDK an explicit `pathToClaudeCodeExecutable`; there is no
supported `CLAUDE_CODE_EXECUTABLE` override or seed repair requirement here.

Peer auto-installation and strict missing-peer failure are disabled deliberately.
The omitted `@anthropic-ai/sdk` and `@modelcontextprotocol/sdk` peers are not runtime
imports in these reviewed versions. Zod remains available in plugin and SDK
contexts as the same canonical instance, not as an unhoisted root dependency.
The runtime gate imports the SDK/adapter, tests the plugin's own query handoff,
and initializes the real SDK/CLI without a prompt. Future dependency/peer changes
require review. The upstream OpenCode host is development-only and excluded.
Plugin/Zod are MIT; SDK/platform packages have Anthropic terms, not MIT.

```sh
bash images/tools/opencode/plugins/upgrade-plugins.sh VERSION
```

Host maintenance requires **pnpm 11.11.0**, not host pnpm 12. The upgrade stages
manifest/lock/workspace/Dockerfile and publishes transactionally; rollback status
3 retains and reports recovery material. Omitted VERSION uses a 60-second private-
state pnpm metadata query; lock preparation and installation use the 600-second
hard-only runner. Builds never query latest. Empty/equal preparation invokes no
manager/network and preserves committed bytes; frozen installation still verifies
registry age/provenance metadata. There is no npm execution/fallback or lock
conversion in this path, no lifecycle exception and no safety-default relaxation.

Installation uses the base launcher, frozen production flags, copied package
files and invocation-private HOME/XDG/store, and suppresses pnpm's update
notifier on every invocation so no build spends a registry round trip on a
version notice. The strict-egress profile admits the resulting traffic exactly:
metadata, provenance bundles and the three locked tarballs. The external store is removed on
success/failure; `.pnpm` remains required runtime content. Its generated metadata
may name the removed store; that is not a symlink dependency. Certification runs
after removal and rejects escaped/dangling/cyclic links, physical SDK shadows,
optional payloads and inaccessible lexical/resolved ancestors.

The gate starts a private loopback server, activates models first, then checks
one active local plugin, one enabled provider, positive unique associated models
and a live nonzero-port proxy health response. It preserves the real image ENV
rather than repairing a missing config. Eight records/seven agent verifiers
exclude separate pnpm verification calls. The independent finished-image oracle
checks raw evidence under four numeric UID/GID pairs and `--network none`.
Both native architectures and the separate frozen-policy egress profile remain
acceptance requirements; host fixtures are not that evidence.

### Migration risk

The 2.x line is young and has reported regressions. Back up OpenCode state before
migration. Opening a v2 session database with v1 as an informal rollback can
corrupt it; do not downgrade against the same database.
