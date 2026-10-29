#!/bin/sh
# agent-vm's stable `pi` wrapper (https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0012-stable-pi-image-customization-seam.md).
#
# The Pi installation under /opt/agent-vm/pi and the extensions under
# /opt/agent-vm/pi-extensions may both be replaced or extended by a later image
# layer. THIS FILE MAY NOT BE: it is the only thing that guarantees the guest
# credential warning is loaded for every normal Pi invocation, including a bare
# `pi` typed into `agent-vm shell`.
#
# It makes four decisions and then execs:
#   1. the Pi env enforcement (PI_SKIP_VERSION_CHECK);
#   2. subcommand dispatch (forwarded verbatim);
#   3. whether the optional image-owned bridge extension is loadable and not
#      opted out;
#   4. whether the image-owned Codex extension is loadable (ADR-0044);
# plus the mandatory extension below.
#
# agent-vm intervenes in Pi's behaviour only where agent-vm introduced the
# condition (the parity principle; see ADR-0021). It deliberately injects NO
# project-trust default and NO telemetry default: --approve / --no-approve /
# --extension and PI_TELEMETRY are forwarded untouched, so a user gets Pi's own
# policy. Pi's project-trust prompt appears on its own terms, and the answer a
# user gives is remembered in the now-persistent ~/.pi/agent/trust.json. ADR-0012's
# wrapper counted its one decision as the mandatory extension; counting the
# extensions the same way, this wrapper makes five.
set -eu

# agent-vm owns the Pi binary: it is a root-owned image layer, `pi update self`
# cannot write to it, and the pin lives in images/tools/pi/package.json. The
# startup "a newer pi is available" fetch can therefore only ever be noise and a
# network call the guest did not ask for. Enforced, not defaulted: Pi treats any
# non-empty value as "skip" (dist/utils/version-check.js).
export PI_SKIP_VERSION_CHECK=1

# Overridable only so the black-box test in script/test/pi-wrapper.sh can point at a
# fake. It is not a protection: the warning is advisory (the microVM is the
# boundary), and a guest can reach the entry point directly anyway -- which is also
# why the seam is *pinned* rather than removed: the standard image's verify RUN sets
# AGENT_VM_PI_ENTRY to exactly this path, so the build-time gate cannot inherit a
# different binary from an image ENV. `script/test/pi-verify.sh` fails if that pin
# disappears.
PI_ENTRY="${AGENT_VM_PI_ENTRY:-/opt/agent-vm/pi/node_modules/.bin/pi}"

MANDATORY_EXTENSION=/opt/agent-vm/pi-extensions/guest-credential-warning.js

# Pi dispatches a subcommand only when it is the FIRST argument: `pi -e X list`
# treats `list` as a prompt and runs an agent turn, and `pi list -e X` rejects
# the option. So a subcommand invocation is forwarded verbatim.
#
# WHICH words are subcommands belongs to the installed Pi, not to this file: Pi
# 0.87.0 has no `mcp` command, and forwarding `pi mcp ...` for it would let an
# upstream prompt turn skip the mandatory extension. The installer therefore
# generates `/opt/agent-vm/pi/agent-vm-subcommands` from the installed CLI's own
# bounded `--help`, after checking it against a reviewed per-version expectation,
# and publishes it as data only: one canonical sorted word per line, root-owned
# 0644. This file owns the closed *vocabulary* below and refuses anything else, so
# the data can only select words a reviewed wrapper already understands. An image
# that ships no data file (or a replacement Pi in a later layer) keeps the literal
# default list, exactly as before.
PI_SUBCOMMANDS="auth config install list mcp remove uninstall update"
PI_SUBCOMMAND_VOCABULARY=$PI_SUBCOMMANDS
SUBCOMMAND_FILE="${AGENT_VM_PI_SUBCOMMANDS:-/opt/agent-vm/pi/agent-vm-subcommands}"

if [ -e "$SUBCOMMAND_FILE" ] || [ -L "$SUBCOMMAND_FILE" ]; then
    # A malformed list is refused before dispatch rather than silently falling
    # back: a file that exists is an assertion about this Pi, not a hint.
    if [ -L "$SUBCOMMAND_FILE" ] || [ ! -f "$SUBCOMMAND_FILE" ] || [ ! -r "$SUBCOMMAND_FILE" ]; then
        echo "pi: refusing the subcommand list at $SUBCOMMAND_FILE (not a readable regular file)" >&2
        exit 2
    fi
    if [ -s "$SUBCOMMAND_FILE" ]; then
        LC_ALL=C awk -v vocabulary="$PI_SUBCOMMAND_VOCABULARY" '
            BEGIN { count = 0; split(vocabulary, allowed, " ");
                    for (slot in allowed) known[allowed[slot]] = 1; previous = "" }
            { if (NF != 1 || $0 != $1 || !($1 in known)) exit 1
              if (previous != "" && !(previous < $1)) exit 1
              previous = $1
              count++ }
            END { if (count == 0) exit 1 }
        ' "$SUBCOMMAND_FILE" >/dev/null 2>&1 || {
            echo "pi: refusing the malformed subcommand list at $SUBCOMMAND_FILE" >&2
            exit 2
        }
        if [ "$(tail -c 1 "$SUBCOMMAND_FILE" | wc -l | tr -d ' ')" != 1 ]; then
            echo "pi: refusing the malformed subcommand list at $SUBCOMMAND_FILE" >&2
            exit 2
        fi
        selected=
        while IFS= read -r word || [ -n "$word" ]; do
            selected="$selected $word"
        done <"$SUBCOMMAND_FILE"
        PI_SUBCOMMANDS=$selected
    else
        echo "pi: refusing the empty subcommand list at $SUBCOMMAND_FILE" >&2
        exit 2
    fi
fi

if [ "$#" -gt 0 ]; then
    for subcommand in $PI_SUBCOMMANDS; do
        if [ "$1" = "${subcommand}" ]; then
            exec "${PI_ENTRY}" "$@"
        fi
    done
fi

# Pi already fails closed when an explicit --extension cannot be loaded (missing
# path, throw, or syntax error all exit 1 before session startup, verified in
# every mode), so there is deliberately no existence check here to duplicate --
# and diverge from -- that message.
# The extension is agent-vm-introduced: agent-vm persists guest credentials in
# project-scoped state readable by any guest process, so agent-vm is the one
# that must warn about it. Nothing else is injected: no --approve default (Pi's
# project-trust prompt is Pi's own, and a user's answer now sticks because
# ~/.pi/agent/trust.json is persistent), and no PI_TELEMETRY default (Pi's
# telemetry policy is Pi's own).
#
# The image-owned pi-claude-bridge
# (https://github.com/gregwebs/agent-vm/blob/3ec78eebb7bdf743c93942a6ed2e5510de15c52d/docs/adr/0023-image-owned-pi-extension-packages.md). Unlike
# MANDATORY_EXTENSION, this one IS existence-checked. Pi treats an --extension
# it cannot load as fatal before session startup (and a settings-listed or
# discovered extension that fails is fatal too -- dist/main.js turns any
# "Failed to load extension" diagnostic into exit 1), so an
# AGENT_INSTALL_SOFT_FAIL build that deleted the tree would otherwise break
# every `pi` invocation. An absent bridge must cost the bridge, not pi.
#
# AGENT_VM_PI_NO_BRIDGE is the recovery hatch: pinned third-party code runs
# in-process in every non-subcommand invocation, so a bridge that throws -- or a
# user-installed second copy (see the ADR's consequence) -- must be escapable
# without knowing the internal entry point. The path override exists only so
# script/test/pi-wrapper.sh can point at a fake, exactly like AGENT_VM_PI_ENTRY
# above. It is not a protection.
BRIDGE_EXTENSION="${AGENT_VM_PI_BRIDGE_EXTENSION:-/opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src/index.ts}"

# An image that drops this file must cost the provider, not pi. The override is
# test-only, not a protection, like the bridge override above. See ADR-0044:
# https://github.com/gregwebs/agent-vm/blob/7438927/docs/adr/0044-pi-reaches-host-codex-through-an-image-owned-provider.md
CODEX_EXTENSION="${AGENT_VM_PI_CODEX_EXTENSION:-/opt/agent-vm/pi-extensions/agent-vm-codex.js}"

# POSIX sh has no arrays; reverse prepending preserves mandatory/bridge/codex order.
if [ -r "$CODEX_EXTENSION" ]; then
    set -- --extension "$CODEX_EXTENSION" "$@"
fi
if [ -z "${AGENT_VM_PI_NO_BRIDGE:-}" ] && [ -r "${BRIDGE_EXTENSION}" ]; then
    set -- --extension "$BRIDGE_EXTENSION" "$@"
fi
exec "${PI_ENTRY}" --extension "${MANDATORY_EXTENSION}" "$@"
