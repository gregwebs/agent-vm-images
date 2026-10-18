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
python3 - "$tmp" "$codex" "$opencode" "$claude" "$copilot" "$dsh" "$pnpm" "$pi" <<'PY'
import re, sys
from pathlib import Path
root = Path(sys.argv[1])
codex, opencode, claude, copilot, dsh, pnpm, pi = sys.argv[2:]
expected = {'codex': 'codex-cli ' + codex.removeprefix('rust-v'),
            'opencode': 'opencode ' + opencode, 'claude': claude + ' (Claude Code)',
            'dsh': dsh, 'pnpm': pnpm, 'pi': pi}
for tool, value in expected.items():
    assert (root / (tool + '.out')).read_bytes() == (value + '\n').encode(), tool + ' exact report'
lines = (root / 'copilot.out').read_bytes().splitlines()
assert lines and lines[0] == ('GitHub Copilot CLI ' + copilot + '.').encode()
assert all(x in (b'', b"Run 'copilot update' to check for updates.") for x in lines[1:])
for tool in (*expected, 'copilot'):
    data = (root / (tool + '.err')).read_bytes()
    assert all(x in (9, 10) or 32 <= x <= 126 for x in data), tool + ' stderr'
    assert not any(re.fullmatch(rb'(codex-cli .*|GitHub Copilot CLI .*|.* \(Claude Code\)|opencode v[0-9]+\.[0-9]+\.[0-9]+.*|v?[0-9]+\.[0-9]+\.[0-9]+.*)', x)
                   for x in data.splitlines()), tool + ' conflicting stderr'
for tool in ('codex', 'opencode', 'claude', 'copilot', 'dsh', 'pi', 'pi-claude-bridge', 'opencode-claude'):
    assert Path('/opt/agent-vm/install-status', tool).read_bytes() == b'installed\n', tool + ' status'
PY
for project in pi pi-packages; do
    test -r "/opt/agent-vm/$project/package.json"
    test -r "/opt/agent-vm/$project/package-lock.json"
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
python3 - "$dsh" "$pnpm" <<'PY'
import json, os, sys
dsh, pnpm = sys.argv[1:]
prefix = '/opt/agent-vm/dsh'
manifest = json.load(open(prefix + '/package.json'))
assert manifest['name'] == 'agent-vm-dsh-layer', manifest['name']
assert manifest['private'] is True
assert manifest['packageManager'] == 'pnpm@' + pnpm, manifest['packageManager']
assert list(manifest['dependencies']) == ['@deepseek-ai/dsh'], manifest['dependencies']
assert manifest['dependencies']['@deepseek-ai/dsh'] == dsh
assert 'devDependencies' not in manifest
assert not os.path.lexists(prefix + '/node_modules/pnpm')
assert not os.path.lexists(prefix + '/node_modules/.bin/pnpm')
link = '/usr/local/bin/dsh'
target = prefix + '/node_modules/@deepseek-ai/dsh/lib/bin.js'
assert os.path.islink(link), link
assert os.readlink(link) == target, os.readlink(link)
assert os.path.samefile(link, target)
assert os.access(target, os.X_OK)
PY
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
mkdir -p "$tmp/location/nested"
timeout --kill-after=5 25 node "$tmp/check-claude-runtime.mjs" /opt/agent-vm/opencode-plugins /opt/agent/.local/bin/claude >"$tmp/runtime.json"
timeout --kill-after=5 80 python3 "$tmp/probe-opencode.py" capture --binary /opt/agent/.opencode/bin/opencode --state-root "$tmp" --directory "$tmp/location/nested" --timeout 60 >"$tmp/evidence.json"
python3 "$tmp/opencode-audit.py" --evidence "$tmp/evidence.json" --runtime "$tmp/runtime.json" --version "$opencode_claude" --status /opt/agent-vm/install-status/opencode-claude --directory "$tmp/location/nested"
printf 'released standard probe passed uid=%s gid=%s\n' "$uid" "$gid"
