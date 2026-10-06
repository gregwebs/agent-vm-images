#!/usr/bin/env bash
# Deterministic image-owned seeds, independent of optional marketplace success.
set -euo pipefail
[ "$#" -eq 1 ] || { echo "usage: $0 STANDARD_IMAGE" >&2; exit 2; }
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
name="agent-vm-seeds-$$"
trap 'if docker container inspect "$name" >/dev/null 2>&1; then docker rm -f "$name" >/dev/null; fi' EXIT
# shellcheck disable=SC2016 # variables belong to the disposable container
python3 "$ROOT/script/test/host-watchdog.py" 180 docker run --rm --name "$name" \
    --network none --tmpfs /home:rw,exec "$1" bash -c '
set -euo pipefail
for executable in /usr/local/bin/pi /opt/agent-vm/seed.d/10-claude-plugins /opt/agent-vm/seed.d/20-pi-claude-bridge; do
    test "$(stat -c %a "$executable")" = 755
done
test -r /opt/agent-vm/pi-extensions/guest-credential-warning.js
# Replace optional download results only in this throwaway container.
rm -rf /opt/agent-vm/claude-seed
mkdir -p /opt/agent-vm/claude-seed/plugins /agent-vm-state/{claude,pi/agent} /home/user
printf synthetic > /opt/agent-vm/claude-seed/plugins/fixture
printf "%s\n" "{\"enabledPlugins\":{\"seed\":true,\"user\":false},\"extraKnownMarketplaces\":{\"seed\":{\"source\":\"fixture\"}}}" > /opt/agent-vm/claude-seed/settings.json
printf "%s\n" "{\"enabledPlugins\":{\"user\":true},\"keep\":42}" > /agent-vm-state/claude/settings.json
chmod -R a+rX /opt/agent-vm/claude-seed
ln -s /agent-vm-state/claude /home/user/.claude
ln -s /agent-vm-state/pi /home/user/.pi
chown -R 12345:23456 /agent-vm-state /home/user
setpriv --reuid=12345 --regid=23456 --clear-groups env HOME=/home/user bash -c '\''
set -euo pipefail
/opt/agent-vm/seed.d/10-claude-plugins
cmp /opt/agent-vm/claude-seed/plugins/fixture /agent-vm-state/claude/plugins/fixture
jq -e ".keep == 42 and .enabledPlugins.user == true and .enabledPlugins.seed == true and .extraKnownMarketplaces.seed.source == \"fixture\"" /agent-vm-state/claude/settings.json
cp /agent-vm-state/claude/settings.json /tmp/claude-before
/opt/agent-vm/seed.d/10-claude-plugins
cmp /tmp/claude-before /agent-vm-state/claude/settings.json
/opt/agent-vm/seed.d/20-pi-claude-bridge
cp /agent-vm-state/pi/agent/claude-bridge.json /tmp/pi-before
/opt/agent-vm/seed.d/20-pi-claude-bridge
cmp /tmp/pi-before /agent-vm-state/pi/agent/claude-bridge.json
'\''
rm -rf /opt/agent-vm/claude-seed /agent-vm-state/claude/plugins
setpriv --reuid=12345 --regid=23456 --clear-groups env HOME=/home/user sh -ec "/opt/agent-vm/seed.d/10-claude-plugins; test ! -e /agent-vm-state/claude/plugins"
'
echo 'Claude/Pi numeric-UID seed controls passed'
