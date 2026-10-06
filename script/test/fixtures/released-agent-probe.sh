#!/usr/bin/env bash
# Credential-free probe supplied verbatim to Docker and native msb guests.
set -euo pipefail
[ "$#" -eq 10 ] || { echo 'probe requires UID GID and eight source-derived selections' >&2; exit 2; }
uid=$1 gid=$2 codex=$3 opencode=$4 claude=$5 copilot=$6 dsh=$7 pnpm=$8 pi=$9 bridge=${10}
[ "$(id -u)" = "$uid" ] && [ "$(id -g)" = "$gid" ]
: "${HOME:?fresh writable HOME required}"
printf 'numeric uid probe\n' > "$HOME/agent-vm-probe"
tmp=$(mktemp -d "$HOME/probe.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
export PI_TELEMETRY=0 PI_SKIP_VERSION_CHECK=1
for tool in codex opencode claude copilot dsh pi pnpm; do
    timeout --kill-after=5s 60 "$tool" --version > "$tmp/$tool.out" 2> "$tmp/$tool.err"
done
python3 - "$tmp" "$codex" "$opencode" "$claude" "$copilot" "$dsh" "$pnpm" "$pi" <<'PY'
import re, sys
from pathlib import Path
root = Path(sys.argv[1])
codex, opencode, claude, copilot, dsh, pnpm, pi = sys.argv[2:]
expected = {'codex': 'codex-cli ' + codex.removeprefix('rust-v'),
            'opencode': opencode.removeprefix('v'), 'claude': claude + ' (Claude Code)',
            'dsh': dsh, 'pnpm': pnpm, 'pi': pi}
for tool, value in expected.items():
    assert (root / (tool + '.out')).read_bytes() == (value + '\n').encode(), tool + ' exact report'
lines = (root / 'copilot.out').read_bytes().splitlines()
assert lines and lines[0] == ('GitHub Copilot CLI ' + copilot + '.').encode()
assert all(x in (b'', b"Run 'copilot update' to check for updates.") for x in lines[1:])
for tool in (*expected, 'copilot'):
    data = (root / (tool + '.err')).read_bytes()
    assert all(x in (9, 10) or 32 <= x <= 126 for x in data), tool + ' stderr'
    assert not any(re.fullmatch(rb'(codex-cli .*|GitHub Copilot CLI .*|.* \(Claude Code\)|v?[0-9]+\.[0-9]+\.[0-9]+.*)', x)
                   for x in data.splitlines()), tool + ' conflicting stderr'
for tool in ('codex', 'opencode', 'claude', 'copilot', 'dsh', 'pi', 'pi-claude-bridge'):
    assert Path('/opt/agent-vm/install-status', tool).read_bytes() == b'installed\n', tool + ' status'
PY
for project in dsh pi pi-packages; do
    test -r "/opt/agent-vm/$project/package.json"
    test -r "/opt/agent-vm/$project/package-lock.json"
done
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
python3 - "$tmp/bridge.out" <<'PY'
import json, re, sys
lines = open(sys.argv[1]).read().splitlines()
marks = []
for line in lines:
    if line.startswith('{'):
        line = json.loads(line).get('message', '')
    if 'RELEASE-BRIDGE' in line:
        marks.append(line)
assert len(marks) == 1 and re.fullmatch(r'RELEASE-BRIDGE models=[1-9][0-9]*', marks[0])
assert any('agent-vm: signing in here' in line for line in lines)
PY
printf 'released standard probe passed uid=%s gid=%s\n' "$uid" "$gid"
