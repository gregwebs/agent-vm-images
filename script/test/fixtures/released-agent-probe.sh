#!/usr/bin/env bash
# Credential-free probe supplied verbatim to Docker and native msb guests.
set -euo pipefail
[ "$#" -eq 11 ] || { echo 'probe requires UID GID and nine source-derived selections' >&2; exit 2; }
uid=$1 gid=$2 codex=$3 opencode=$4 claude=$5 copilot=$6 dsh=$7 pnpm=$8 pi=$9 bridge=${10} opencode_claude=${11}
[ "$(id -u)" = "$uid" ] && [ "$(id -g)" = "$gid" ]
: "${HOME:?fresh writable HOME required}"
printf 'numeric uid probe\n' > "$HOME/agent-vm-probe"
tmp=$(mktemp -d "$HOME/probe.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
# OPENCODE_PROBE_SOURCES
export PI_TELEMETRY=0 PI_SKIP_VERSION_CHECK=1
for tool in codex opencode claude copilot dsh pi pnpm; do
    timeout --kill-after=5s 60 "$tool" --version > "$tmp/$tool.out" 2> "$tmp/$tool.err"
done
python3 "$tmp/released-agent-report.py" "$tmp" "$codex" "$opencode" "$claude" "$copilot" "$dsh" "$pnpm" "$pi"
# Pi and the bridge are reviewed pnpm projects too: four regular inputs and no
# npm lock, not even a dangling link to one.
for project in pi pi-packages; do
    for input in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do
        test -f "/opt/agent-vm/$project/$input"
        test ! -L "/opt/agent-vm/$project/$input"
    done
    test ! -e "/opt/agent-vm/$project/package-lock.json"
    test ! -L "/opt/agent-vm/$project/package-lock.json"
done
# dsh is a reviewed pnpm project installed by the base manager: four regular
# inputs, no npm lock (nor a dangling link to one), and a direct link at the
# installed lib entrypoint instead of a generated .bin wrapper.
for input in package.json pnpm-lock.yaml pnpm-workspace.yaml pnpm-inputs.sha256; do
    test -f "/opt/agent-vm/dsh/$input"
    test ! -L "/opt/agent-vm/dsh/$input"
done
test ! -e /opt/agent-vm/dsh/package-lock.json
test ! -L /opt/agent-vm/dsh/package-lock.json
python3 "$tmp/released-agent-dsh-layout.py" "$dsh" "$pnpm"
test -x /usr/local/bin/pi
test -r /opt/agent-vm/pi-extensions/guest-credential-warning.js
test -r /opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src/index.ts
[ "$(jq -r .version /opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/package.json)" = "$bridge" ]
for seed in /opt/agent-vm/seed.d/*; do test -r "$seed"; test -x "$seed"; "$seed"; done
# Registration is exercised without a provider turn or credentials.
cat > "$tmp/bridge.js" <<'JS'
export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    const p = ctx.modelRegistry.getProvider("claude-bridge");
    const n = p && typeof p.getModels === "function" ? p.getModels().length
            : p && Array.isArray(p.models) ? p.models.length : 0;
    ctx.ui.notify(`RELEASE-BRIDGE models=${n}`, "warning");
  });
}
JS
timeout --kill-after=5s 120 pi -e "$tmp/bridge.js" --mode rpc --no-session --no-approve < /dev/null > "$tmp/bridge.out" 2> "$tmp/bridge.err"
python3 "$tmp/released-agent-bridge-report.py" "$tmp/bridge.out"
mkdir -p "$tmp/location/nested"
timeout --kill-after=5 25 node "$tmp/check-claude-runtime.mjs" /opt/agent-vm/opencode-plugins /opt/agent/.local/bin/claude >"$tmp/runtime.json"
timeout --kill-after=5 80 python3 "$tmp/probe-opencode.py" capture --binary /opt/agent/.opencode/bin/opencode --state-root "$tmp" --directory "$tmp/location/nested" --timeout 60 >"$tmp/evidence.json"
python3 "$tmp/opencode-audit.py" --evidence "$tmp/evidence.json" --runtime "$tmp/runtime.json" --version "$opencode_claude" --status /opt/agent-vm/install-status/opencode-claude --directory "$tmp/location/nested"
printf 'released standard probe passed uid=%s gid=%s\n' "$uid" "$gid"
