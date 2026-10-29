#!/bin/sh
# Build-time verification gate for the pinned Pi layer and its image-owned
# Claude bridge, run from the layer's final `RUN --mount=type=bind` (see
# images/standard/Dockerfile). Bind-mounted, never COPYed, so the script stays
# out of the shipped image and needs no execute bit.
#
# The gate has exactly ONE absence decision, and it happens before ordinary
# runtime authorization, before HOME/Node/manager state and before any status
# write: `pnpm-project.py pi-gate-entry` validates the whole Pi feature tuple,
# recognises a genuine curl absence (and only that), and otherwise hands back a
# present command. A dangling, non-executable or indeterminate command, a
# surviving prefix without its bin, or an unreadable/malformed/retired record is
# a hard failure -- never an absence. A present command wins even over a stale
# absence record, and then the ordinary four-input/runtime gates decide.
#
# In the absent case the same invocation retires exactly the finite inventory the
# absence authority names (warning wrapper, seed hook, extensions and bridge
# roots) and publishes the derivative `absent-pi` bridge record; it preserves the
# Pi curl record, because that record is the evidence. It starts no Node process
# and no manager.
#
# In the present case every Pi invocation is bounded and its status is checked
# BEFORE its output is parsed. Results AGGREGATE per tool: a failed command, RPC
# registration or seed audit is not erased by a later passing content/tree walk,
# and each slot's `installed` record is written only by its own successful gate.
set -eu

fail() {
    echo "  pi: $*" >&2
    exit 1
}

CONTRACT="${AGENT_VM_CONTRACT_DIR:-/tmp/recipe-contract}"
PI_PREFIX="${AGENT_VM_PI_PREFIX:-/opt/agent-vm/pi}"
WRAPPER="${AGENT_VM_PI_WRAPPER:-/usr/local/bin/pi}"
PACKAGES_PREFIX="${AGENT_VM_PI_PACKAGES_PREFIX:-/opt/agent-vm/pi-packages}"
EXTENSION_DIR="${AGENT_VM_PI_EXTENSION_DIR:-/opt/agent-vm/pi-extensions}"
CODEX_STATE_DIR="${AGENT_VM_PI_CODEX_STATE_DIR:-/agent-vm-state/codex}"
GATE_WORK_DIR="${AGENT_VM_PI_GATE_WORK_DIR:-/tmp/pi-gate}"
TARGET_PLATFORM="${AGENT_VM_PI_TARGET_PLATFORM:-}"
SEED_HOOK="${AGENT_VM_PI_SEED_HOOK:-/opt/agent-vm/seed.d/20-pi-claude-bridge}"
STATUS_DIR="${AGENT_VM_INSTALL_STATUS_DIR:-/opt/agent-vm/install-status}"
PI_VERSION="${AGENT_VM_VERSION_PI:-}"
BRIDGE_VERSION="${AGENT_VM_VERSION_PI_CLAUDE_BRIDGE:-}"
SELF_DIR=$(cd "$(dirname "$0")" && pwd -P)
PROJECT="${AGENT_VM_PI_SHARED:-$SELF_DIR/../pnpm-project.py}"
# The mounted-helper seam, pinned by the standard image's verify RUN for the same
# reason as `AGENT_VM_PI_ENTRY` in pi.sh: the checker is the gate's own identity
# proof, so a build must not be able to inherit a different one from an image ENV.
# `script/test/pi-verify.sh` fails if that pin disappears.
RUNTIME_CHECK="${AGENT_VM_PI_RUNTIME_CHECK:-$SELF_DIR/check-runtime.mjs}"
SUBCOMMAND_FILE="${AGENT_VM_PI_SUBCOMMANDS:-$PI_PREFIX/agent-vm-subcommands}"
DISPATCH_DIR="${AGENT_VM_PI_DISPATCH_DIR:-$SELF_DIR/dispatch}"
SKIP_BRIDGE="${AGENT_VM_PI_GATE_SKIP_BRIDGE:-}"

# A Node preload or an injected module path would make every later probe, RPC
# registration and content walk report what the caller chose rather than what the
# image contains, so it is refused before the presence decision and before any
# Node process exists. `pnpm-project.py pi-gate-entry` starts none either, so this
# is the first thing the gate decides.
[ -z "${NODE_OPTIONS-}" ] || fail 'NODE_OPTIONS is not supported'
[ -z "${NODE_PATH-}" ] || fail 'NODE_PATH is not supported'
[ -z "${BUN_OPTIONS-}" ] || fail 'BUN_OPTIONS is not supported'

pi_bin="$PI_PREFIX/node_modules/.bin/pi"
pi_failed=0
bridge_failed=0
pi_fail() {
    echo "  pi: $*" >&2
    pi_failed=$((pi_failed + 1))
}
bridge_fail() {
    echo "  pi: $*" >&2
    bridge_failed=$((bridge_failed + 1))
}

# --- the single absence decision ---------------------------------------------
command -v python3 >/dev/null 2>&1 || fail 'python3 is required'
[ -f "$PROJECT" ] || fail "the shared capability module is missing at $PROJECT"
[ -f "$RUNTIME_CHECK" ] || fail "the runtime checker is missing at $RUNTIME_CHECK"
gate=$(python3 "$PROJECT" --consumer pi pi-gate-entry \
    --pi-prefix "$PI_PREFIX" --bridge-prefix "$PACKAGES_PREFIX" \
    --extension-root "$EXTENSION_DIR" --wrapper "$WRAPPER" --seed "$SEED_HOOK" \
    --status-dir "$STATUS_DIR" --contract-dir "$CONTRACT" --retire) ||
    fail 'the Pi gate entry rejected this image (see the diagnostic above)'
case "$gate" in
    present) ;;
    absent-retired)
        echo "  pi: ABSENT ($(cat "$STATUS_DIR/pi")); retired the feature, bridge record is derivative"
        exit 0
        ;;
    *) fail "the Pi gate entry returned an unknown answer '$gate'" ;;
esac

# --- ordinary authorization (present branch only) ----------------------------
# PresentPi implies nothing about health, so the ordinary runtime capability is
# acquired explicitly: the four reviewed inputs, the base pin and a payload-owned
# readable/executable command must all hold before any probe runs.
export HOME="$GATE_WORK_DIR" XDG_CONFIG_HOME="$GATE_WORK_DIR/.config" PI_TELEMETRY=0
mkdir -p "$HOME"
python3 "$PROJECT" --consumer pi authorize-runtime "$PI_PREFIX" \
    --command "$pi_bin" --scratch-root "$HOME" >/dev/null ||
    fail 'the installed Pi payload failed runtime authorization'

# bound EXE [ARGS...] -> PI_RUN_STATUS, stdout/stderr in $OUT/$ERR. stdin is
# /dev/null so an rpc-mode run sees EOF instead of the build's terminal.
OUT=
ERR=
PI_RUN_STATUS=0
pi_run() { # $1 = seconds, $2 = out, $3 = err, rest = executable + args
    seconds=$1
    OUT=$2
    ERR=$3
    shift 3
    PI_RUN_STATUS=0
    sh "$CONTRACT/run-report.sh" "$seconds" "$OUT" "$ERR" "$@" </dev/null || PI_RUN_STATUS=$?
}

require_registered_provider() { # label, marker prefix, output file
    # The label is part of the call shape; the caller prefixes its own
    # diagnostics with it, so it is only consumed here to keep the signature
    # explicit for the two call sites.
    : "$1"
    marker=$2
    probe_out=$3
    # The probe result may be a complete plain marker line OR a valid single-line
    # JSON notification whose `.message` is the marker (real Pi wraps the
    # `ctx.ui.notify` text). Require EXACTLY ONE unambiguous registration result
    # and a strictly positive canonical model count: a bare prefix match used to
    # accept `models=00` (a zero), a non-JSON line like `models=12"garbage`, or a
    # positive marker contradicted by a later `${marker}-MISSING`.
    registration_error="did not register its provider"
    registered_count=""
    result_count=0
    malformed=0
    while IFS= read -r line || [ -n "$line" ]; do
        [ -z "$line" ] && continue
        msg="$line"
        registration_bearing=0
        case "$line" in
            *"${marker}-REGISTERED"* | *"${marker}-MISSING"*)
                registration_bearing=1 ;;
        esac
        case "$line" in
            '{'*)
                # A registration marker may arrive inside a single-line JSON
                # notification, so decode `.message`. The WHOLE line must parse;
                # a truncated or invalid notification that carries a `-MISSING`
                # marker must fail closed instead of being discarded.
                if msg="$(printf '%s\n' "$line" | jq -r \
                    'if type == "object" and (.message | type) == "string" then .message else empty end' \
                    2>/dev/null)"; then
                    :
                else
                    if [ "$registration_bearing" -eq 1 ]; then malformed=1; fi
                    continue
                fi ;;
        esac
        if [ "$registration_bearing" -eq 1 ]; then
            case "$msg" in
                "${marker}-MISSING")
                    result_count=$((result_count + 1)) ;;
                "${marker}-REGISTERED models="*)
                    result_count=$((result_count + 1))
                    registered_count="${msg#${marker}-REGISTERED models=}" ;;
                *)
                    malformed=1 ;;
            esac
        fi
    done <"$probe_out"

    if [ "$malformed" -eq 1 ]; then
        registration_error="emitted a malformed registration line"
        return 1
    fi
    if [ "$result_count" -ne 1 ]; then
        if [ "$result_count" -eq 0 ]; then
            registration_error="did not register its provider"
        else
            registration_error="emitted $result_count conflicting registration results"
        fi
        return 1
    fi
    case "$registered_count" in
        '') registration_error="did not report a model count"; return 1 ;;
        0 | 0*) registration_error="registered but its model catalog is empty"; return 1 ;;
        *[!0-9]*) registration_error="reported a non-numeric model count '$registered_count'"; return 1 ;;
    esac
    return 0
}

# --- Pi gates ----------------------------------------------------------------
want=$(jq -er '.dependencies["@earendil-works/pi-coding-agent"]' "$PI_PREFIX/package.json") ||
    fail 'the installed manifest pins no Pi version'
if [ -n "$PI_VERSION" ] && [ "$PI_VERSION" != "$want" ]; then
    fail "layer asked for Pi $PI_VERSION but the manifest pins $want"
fi

# The installed tree must be the selection the lock describes *before* it runs at
# all: the root command is a direct link to the locked bin target, the N6 lifecycle
# packages provide their shipped runtime, and the isolated layout reaches the copy
# the lock's edges resolved. This is therefore the first thing the gate does with
# the payload -- before the wrapper's own `--version`/`--help`, which exec the
# installed CLI -- because a tree whose root command is a shim or a copy must not
# get to run once, let alone be certified. A failed proof therefore ends the gate:
# withholding the records is not enough when the payload it could not identify is
# what the next four probes would exec. The byte proof (the archives the lock
# names, rehashed and compared with the installed tree) belongs to the installer,
# which owns the authenticated downloads and their retained evidence; repeating it
# here would need the network this verifier deliberately does not use.
[ -n "$TARGET_PLATFORM" ] ||
    fail 'AGENT_VM_PI_TARGET_PLATFORM must name the image platform for the runtime proof'
if ! timeout --kill-after=5 600 env AGENT_VM_PI_TARGET_PLATFORM="$TARGET_PLATFORM" \
    AGENT_VM_CONTRACT_DIR="$CONTRACT" node "$RUNTIME_CHECK" "$PI_PREFIX" \
    >"$GATE_WORK_DIR/runtime.json" 2>"$GATE_WORK_DIR/runtime.err"; then
    sed -n '1,5p' "$GATE_WORK_DIR/runtime.err" >&2
    fail 'the installed Pi runtime failed its identity and N6 probes'
fi
grep -q '^pi runtime: ' "$GATE_WORK_DIR/runtime.json" ||
    fail 'the runtime checker returned no report'
echo "  pi: installed runtime and N6 probes passed"

pi_run 60 "$GATE_WORK_DIR/v.out" "$GATE_WORK_DIR/v.err" "$WRAPPER" --version
if [ "$PI_RUN_STATUS" -ne 0 ]; then
    pi_fail "--version exited $PI_RUN_STATUS; refusing to accept the report"
    sed -n '1,10p' "$GATE_WORK_DIR/v.err" >&2
else
    got=$(sed -n '1p' "$GATE_WORK_DIR/v.out")
    if [ "$got" != "$want" ]; then
        pi_fail "the wrapper --version reported '$got', the manifest pins $want"
    fi
    lineno=0
    while IFS= read -r line || [ -n "$line" ]; do
        lineno=$((lineno + 1))
        [ "$lineno" -eq 1 ] && continue
        [ -z "$line" ] && continue
        pi_fail "unexpected --version output on line $lineno: '$line'"
    done <"$GATE_WORK_DIR/v.out"
fi

# The subcommand set belongs to the selected Pi: it is published as data by the
# installer, must equal the reviewed expectation for this version, and must equal
# the installed CLI's own `--help`. A pin bump that adds or renames a command
# therefore fails here instead of silently mis-dispatching.
if [ -L "$SUBCOMMAND_FILE" ] || [ ! -f "$SUBCOMMAND_FILE" ] || [ ! -r "$SUBCOMMAND_FILE" ]; then
    pi_fail "the image-owned subcommand list is missing or unreadable at $SUBCOMMAND_FILE"
else
    expectation="$DISPATCH_DIR/$want.txt"
    if [ ! -f "$expectation" ]; then
        pi_fail "no reviewed dispatch expectation for Pi $want at $expectation"
    elif ! cmp -s "$SUBCOMMAND_FILE" "$expectation"; then
        pi_fail "the published subcommand list [$(tr '\n' ' ' <"$SUBCOMMAND_FILE")] is not the reviewed $want list [$(tr '\n' ' ' <"$expectation")]"
    fi
    pi_run 60 "$GATE_WORK_DIR/h.out" "$GATE_WORK_DIR/h.err" "$WRAPPER" --help
    if [ "$PI_RUN_STATUS" -ne 0 ]; then
        pi_fail "--help exited $PI_RUN_STATUS; refusing to accept the output"
    else
        sed -n 's/^  pi \([a-z][a-z-]*\) .*/\1/p' "$GATE_WORK_DIR/h.out" | LC_ALL=C sort -u \
            >"$GATE_WORK_DIR/h.list"
        if [ ! -s "$GATE_WORK_DIR/h.list" ]; then
            pi_fail 'the installed CLI advertises no subcommands'
        elif ! cmp -s "$GATE_WORK_DIR/h.list" "$SUBCOMMAND_FILE"; then
            pi_fail "the installed CLI advertises [$(tr '\n' ' ' <"$GATE_WORK_DIR/h.list")], the published list is [$(tr '\n' ' ' <"$SUBCOMMAND_FILE")]"
        else
            echo "  pi: subcommand list matches the installed CLI and the reviewed $want expectation"
        fi
    fi
fi

# The extension is image-owned and mandatory: a real invocation must load it and
# emit the credential warning. Both halves are checked -- it is present, and it
# actually runs -- with the process status checked before the output is scanned.
[ -r "$EXTENSION_DIR/guest-credential-warning.js" ] || pi_fail 'the mandatory extension is missing'
[ -r "$EXTENSION_DIR/agent-vm-codex.js" ] || pi_fail 'the agent-vm-codex extension is missing'

codex_parent=$(dirname "$CODEX_STATE_DIR")
codex_created_parent=
codex_created_dir=
codex_cleanup_failed=
codex_trap_status=0
# Both are cleared here, before the trap and before any early exit can run it: the
# cleanup decides ownership by comparing with the fixture source, and a `fixture_src`
# inherited from the caller's environment names a file this gate never published.
# `scratch` has the same reason -- it is what the trap removes.
scratch=
fixture_src=
codex_cleanup() {
    # Ownership is decided by inode identity with the fixture source, which stays in
    # $scratch until this invocation ends. `ln` may publish before the shell can see
    # its status, and a credential another writer put in place afterwards is a
    # different inode; a flag set after publication would only say that *something*
    # was published, never that what sits at that path now is still the gate's, so it
    # takes no part in this decision. `-ef` is the comparison (POSIX.1-2024; dash and
    # bash 3.2 both have it). A symlink is never deleted here: `-ef` would dereference
    # it and `rm` would unlink a different writer's link.
    #
    # Residual: between that comparison and `rm` a writer could replace the path with
    # its own inode, and `rm` would then unlink a credential this gate never
    # published. Sh cannot close it -- there is no compare-and-unlink, and the rename
    # substitute (`mv` the entry aside, compare, restore) moves the same race into the
    # restore and can clobber a newer file -- so the bound is environmental instead:
    # nothing else in the image runs while the gate holds this file. Closing it in
    # code means an `unlinkat`-with-identity helper, not a shell gate.
    if [ -n "$fixture_src" ] && [ ! -L "$CODEX_STATE_DIR/auth.json" ] &&
        [ -e "$fixture_src" ] && [ "$CODEX_STATE_DIR/auth.json" -ef "$fixture_src" ]; then
        rm -f "$CODEX_STATE_DIR/auth.json" || codex_cleanup_failed=1
    fi
    [ -z "$codex_created_dir" ] || { if rmdir "$CODEX_STATE_DIR"; then codex_created_dir=; else codex_cleanup_failed=1; fi; }
    [ -z "$codex_created_parent" ] || { if rmdir "$codex_parent"; then codex_created_parent=; else codex_cleanup_failed=1; fi; }
    return 0
}
# Ownership flags precede every creation: early exits must leave existing state alone.
# The invocation scratch is removed here, once, so an intermediate codex cleanup
# cannot take the later bridge probe's capture files with it.
trap 'codex_trap_status=$?; codex_cleanup || :; [ -z "$scratch" ] || rm -rf "$scratch" || :; exit "$codex_trap_status"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

scratch=$(mktemp -d)
if [ -L "$codex_parent" ] || [ -L "$CODEX_STATE_DIR" ]; then
    pi_fail 'refusing symlinked Codex fixture ancestry'
else
    if [ ! -e "$codex_parent" ]; then
        mkdir "$codex_parent" || pi_fail 'cannot create the Codex fixture parent'
        codex_created_parent=1
    fi
    if [ ! -e "$CODEX_STATE_DIR" ]; then
        mkdir "$CODEX_STATE_DIR" || pi_fail 'cannot create the Codex fixture state directory'
        codex_created_dir=1
    fi
    if [ -e "$CODEX_STATE_DIR/auth.json" ] || [ -L "$CODEX_STATE_DIR/auth.json" ]; then
        pi_fail "refusing to overwrite existing $CODEX_STATE_DIR/auth.json"
    else
        fixture_src="$scratch/codex-auth.json"
        printf '%s\n' '{"tokens":{"access_token":"msb-openai-placeholder-a-v2","refresh_token":"msb-openai-placeholder-r-v2","account_id":"acct-~~~?"}}' >"$fixture_src"
        # A hardlink publishes complete bytes exclusively; mv would overwrite a racing writer.
        # The source is deliberately kept: it is the inode the cleanup compares against,
        # so a replacement written after this point is preserved rather than deleted.
        ln "$fixture_src" "$CODEX_STATE_DIR/auth.json" ||
            pi_fail 'cannot plant the Codex fixture credential'
        pi_run 60 "$scratch/w.out" "$scratch/w.err" "$WRAPPER" --mode rpc --no-session --no-approve
        if [ "$PI_RUN_STATUS" -ne 0 ]; then
            pi_fail "a real invocation exited $PI_RUN_STATUS; refusing to scan its output"
            sed -n '1,10p' "$scratch/w.err" >&2
        else
            grep -q 'agent-vm: signing in here' "$scratch/w.out" ||
                pi_fail 'a real invocation does not load the mandatory extension'
            grep -qF 'Credentials imported from your host stay on the host and never appear in guest files.' \
                "$scratch/w.out" ||
                pi_fail 'agent-vm-codex is not registered, or the warning omits its host-import clause'
        fi
    fi
fi

cat > "$GATE_WORK_DIR/codex-probe.js" <<'PROBE'
export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    const p = ctx.modelRegistry.getProvider("agent-vm-codex");
    const n = p && typeof p.getModels === "function" ? p.getModels().length
            : p && Array.isArray(p.models) ? p.models.length : 0;
    ctx.ui.notify(p ? `AGENT-VM-CODEX-REGISTERED models=${n}` : "AGENT-VM-CODEX-MISSING", "warning");
  });
}
PROBE
pi_run 120 "$scratch/c.out" "$scratch/c.err" \
    "$WRAPPER" -e "$GATE_WORK_DIR/codex-probe.js" --mode rpc --no-session --no-approve
if [ "$PI_RUN_STATUS" -ne 0 ]; then
    pi_fail "the agent-vm-codex probe exited $PI_RUN_STATUS; refusing to scan its output"
else
    require_registered_provider agent-vm-codex AGENT-VM-CODEX "$scratch/c.out" ||
        pi_fail "agent-vm-codex ${registration_error}"
fi
codex_cleanup
[ -z "$codex_cleanup_failed" ] || pi_fail 'the agent-vm-codex fixture could not be removed'
if [ -n "$fixture_src" ] && [ -e "$fixture_src" ] &&
    [ "$CODEX_STATE_DIR/auth.json" -ef "$fixture_src" ]; then
    pi_fail 'the agent-vm-codex fixture survived the gate'
elif [ -e "$CODEX_STATE_DIR/auth.json" ] || [ -L "$CODEX_STATE_DIR/auth.json" ]; then
    # Not ours, and not deleted: but the image would ship whatever is there, so this
    # gate cannot certify the Codex state directory either.
    pi_fail 'a credential replaced the agent-vm-codex fixture during the gate'
fi

# --- bridge gates ------------------------------------------------------------
BRIDGE_MANIFEST="$PACKAGES_PREFIX/package.json"
BRIDGE_PKG="$PACKAGES_PREFIX/node_modules/pi-claude-bridge"
BRIDGE_SRC="$BRIDGE_PKG/src/index.ts"
SDK_PKG="$PACKAGES_PREFIX/node_modules/@anthropic-ai/claude-agent-sdk"
bridge_shipped=0
if [ -n "$SKIP_BRIDGE" ]; then
    bridge_shipped=2 # explicitly opted out for this invocation only
elif [ ! -d "$BRIDGE_PKG" ] || [ -L "$BRIDGE_PKG" ]; then
    # There is no bridge-side absence producer any more. A missing bridge payload
    # is only ever the derivative of a genuine Pi curl absence, and that branch
    # exited above; a Pi-present image with no bridge is a hard failure.
    bridge_fail 'pi-claude-bridge is missing while Pi is present -- there is no bridge absence producer'
elif [ ! -r "$BRIDGE_SRC" ]; then
    bridge_fail 'pi-claude-bridge is present but its source is unreadable'
elif [ ! -f "$BRIDGE_MANIFEST" ]; then
    bridge_fail 'the bridge prefix has no manifest'
else
    bridge_shipped=1
    # A version bump that regenerated the lock but installed nothing would leave
    # metadata at the new pin while the tree stayed stale; the installed
    # package.json is the tree's own version.
    bridge_want=$(jq -er '.dependencies["pi-claude-bridge"]' "$BRIDGE_MANIFEST") ||
        bridge_fail 'the bridge manifest pins no version'
    bridge_got=$(jq -er '.version' "$BRIDGE_PKG/package.json" 2>/dev/null) ||
        bridge_got=
    if [ -z "${bridge_want:-}" ] || [ "$bridge_got" != "$bridge_want" ]; then
        bridge_fail "pi-claude-bridge installed '$bridge_got' but the manifest pins '${bridge_want:-nothing}'"
    fi
    if [ -n "$BRIDGE_VERSION" ] && [ "$BRIDGE_VERSION" != "${bridge_want:-}" ]; then
        bridge_fail "layer asked for bridge $BRIDGE_VERSION but the manifest pins ${bridge_want:-nothing}"
    fi
    # The reviewed bridge workspace is hoisted, keeps the Agent SDK JavaScript and
    # omits only its platform packages, and installs no second Pi/typebox.
    # Hoisted means real package directories. pnpm 11.11.0 keeps a metadata-only
    # `node_modules/.pnpm/lock.yaml` in a hoisted tree, so any *directory* below
    # that store is the payload signal, not the directory's existence.
    if [ -L "$BRIDGE_PKG" ] || [ ! -d "$BRIDGE_PKG" ]; then
        bridge_fail 'the bridge tree is not hoisted: the bridge package is not a real directory'
    elif [ -n "$(find "$PACKAGES_PREFIX/node_modules/.pnpm" -mindepth 1 -type d -print 2>/dev/null | head -n 1)" ]; then
        bridge_fail 'the bridge tree carries private-store package payload; the reviewed workspace is hoisted'
    fi
    if [ ! -f "$SDK_PKG/package.json" ] || [ -L "$SDK_PKG" ]; then
        bridge_fail 'the Claude Agent SDK JavaScript is missing from the hoisted bridge tree'
    fi
    platform_native=$(find "$PACKAGES_PREFIX/node_modules/@anthropic-ai" -maxdepth 1 \
        -name 'claude-agent-sdk-*' -print 2>/dev/null | head -n 1)
    if [ -n "$platform_native" ]; then
        bridge_fail "the bridge tree ships the omitted SDK platform payload $platform_native"
    fi
    for aliased in '@earendil-works' 'typebox' '@sinclair/typebox' 'pi-agent-core' 'pi-ai' 'pi-tui'; do
        if [ -e "$PACKAGES_PREFIX/node_modules/$aliased" ] ||
            [ -L "$PACKAGES_PREFIX/node_modules/$aliased" ]; then
            bridge_fail "the bridge tree installs $aliased, which Pi's loader aliases to its own copy"
        fi
    done
fi

if [ "$pi_failed" -eq 0 ] && [ "$bridge_shipped" -eq 1 ]; then
    # A --extension Pi cannot load is fatal before session startup, so a run that
    # exits 0 already proves every top-level import resolved (the Agent SDK, the
    # MCP SDK, cc-session-io, change-case, and the loader-aliased peers). The
    # probe adds the half that matters: the provider actually REGISTERED, and the
    # model catalog is non-empty. Keep this body in sync with the non-root copy in
    # script/test/pi-runtime.sh.
    cat > "$GATE_WORK_DIR/probe.js" <<'PROBE'
export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    const p = ctx.modelRegistry.getProvider("claude-bridge");
    const n = p && typeof p.getModels === "function" ? p.getModels().length
            : p && Array.isArray(p.models) ? p.models.length : 0;
    ctx.ui.notify(p ? `AGENT-VM-BRIDGE-REGISTERED models=${n}` : "AGENT-VM-BRIDGE-MISSING",
                  "warning");
  });
}
PROBE
    pi_run 120 "$scratch/p.out" "$scratch/p.err" \
        "$WRAPPER" -e "$GATE_WORK_DIR/probe.js" --mode rpc --no-session --no-approve
    if [ "$PI_RUN_STATUS" -ne 0 ]; then
        bridge_fail "the bridge probe exited $PI_RUN_STATUS; refusing to scan its output"
        sed -n '1,10p' "$scratch/p.err" >&2
    elif ! require_registered_provider pi-claude-bridge AGENT-VM-BRIDGE "$scratch/p.out"; then
        bridge_fail "pi-claude-bridge ${registration_error}"
    fi
fi

# --- access and content gates (aggregating) ----------------------------------
# Leavings from the gate runs above (Pi's own caches) are removed so they cannot
# be baked into the shipped image.
rm -rf "$GATE_WORK_DIR" /tmp/jiti /tmp/node-compile-cache || pi_fail 'cannot remove gate leavings'

# Every shipped tree must be usable by an arbitrary uid, and the reachability of
# each entry through its ancestor links matters as much as its own mode, so the
# whole-tree walk is the Go access auditor's job -- not `find`'s `-perm`, which
# inspects a symlink's own 0777 mode and never follows it.
"$CONTRACT/recipe-contract" tool-access --quiet --tree "$PI_PREFIX" ||
    pi_fail "the Pi payload is not readable/searchable by any uid"
"$CONTRACT/recipe-contract" tool-access --quiet --tree "$EXTENSION_DIR" ||
    pi_fail "the extension tree is not readable/searchable by any uid"
for executable in "$WRAPPER" "$pi_bin"; do
    if [ ! -r "$executable" ] || [ ! -x "$executable" ]; then
        pi_fail "$executable is not a+rx"
    fi
done
"$CONTRACT/recipe-contract" tool-access --quiet "$WRAPPER" "$pi_bin" ||
    pi_fail 'the wrapper or the linked Pi entry point is not executable by any uid'
# The wrapper loads required extension content on every invocation, so that
# content must be readable through its link ancestry too.
"$CONTRACT/recipe-contract" tool-access --quiet --content \
    "$EXTENSION_DIR/guest-credential-warning.js" "$EXTENSION_DIR/agent-vm-codex.js" ||
    pi_fail 'a mandatory or image-owned extension is not readable by any uid'
# The seed hook is executed by the guest's boot sequence: it needs a command bit,
# readable content, and reachable ancestry. It seeds the *bridge* configuration, so
# its failures are bridge failures: a Pi that installed and verified correctly is
# still a healthy Pi, and only the bridge record can be withheld for this.
if [ ! -x "$SEED_HOOK" ]; then
    bridge_fail "the bridge seed hook is missing or not executable at $SEED_HOOK"
else
    "$CONTRACT/recipe-contract" tool-access --quiet "$SEED_HOOK" ||
        bridge_fail 'the bridge seed hook is not executable by any uid'
    "$CONTRACT/recipe-contract" tool-access --quiet --content "$SEED_HOOK" ||
        bridge_fail 'the bridge seed hook content is not readable by any uid'
    grep -qF 'pathToClaudeCodeExecutable' "$SEED_HOOK" ||
        bridge_fail 'the bridge seed hook no longer seeds the SDK executable path'
    grep -qF '/opt/agent/.local/bin/claude' "$SEED_HOOK" ||
        bridge_fail 'the bridge seed hook no longer names the image-owned Claude binary'
fi

if [ "$bridge_shipped" -eq 1 ]; then
    "$CONTRACT/recipe-contract" tool-access --quiet --tree "$PACKAGES_PREFIX" ||
        bridge_fail 'the bridge payload is not readable/searchable by any uid'
    "$CONTRACT/recipe-contract" tool-access --quiet --content "$BRIDGE_SRC" ||
        bridge_fail 'the bridge source is not readable by any uid'
fi

# --- publication -------------------------------------------------------------
# Only a slot whose OWN gates all passed is published. A failed command, probe or
# content audit can never be erased by a later passing walk, and a Pi failure
# also leaves the bridge unpublished because an unproven Pi cannot prove it.
if [ "$pi_failed" -ne 0 ]; then
    echo "  pi: $pi_failed Pi gate(s) failed; no record published" >&2
    exit 1
fi
printf 'installed\n' >"$STATUS_DIR/pi"
if [ "$bridge_failed" -ne 0 ]; then
    echo "  pi: $bridge_failed bridge gate(s) failed; only Pi is recorded" >&2
    exit 1
fi
if [ "$bridge_shipped" -eq 1 ]; then
    printf 'installed\n' >"$STATUS_DIR/pi-claude-bridge"
fi
echo "  pi: readable and executable by any uid"
