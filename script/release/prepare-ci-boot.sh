#!/usr/bin/env bash
# Disposable hosted amd64 runner only: reclaim the four named unused SDK paths and
# grant this job's user /dev/kvm for a real msb boot. Never run on an operator host.
set -euo pipefail
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_OS:-}" = Linux ] || { echo 'disposable Linux Actions runner only' >&2; exit 1; }
# Measured: ubuntu-24.04-arm exposes no /dev/kvm, so arm64 boot evidence stays a native-host step.
[ "$(uname -m)" = x86_64 ] || { echo 'hosted boot verification is amd64-only: ubuntu-24.04-arm has no /dev/kvm' >&2; exit 1; }
[ "$#" -eq 1 ] || { echo "usage: ${0##*/} LOG_DIR" >&2; exit 2; }
logs=$1
mkdir -p "$logs"
probe() {
    printf 'cpu virt flags: %s\n' "$(grep -cwE 'vmx|svm' /proc/cpuinfo || true)"
    lsmod | grep -E '^kvm' || echo 'no kvm modules loaded'
    ls -l /dev/kvm 2>&1 || true
}
probe > "$logs/kvm-before.log"
df -Pk / "$RUNNER_TEMP" /tmp > "$logs/capacity-before.log"
# The same bounded, explicitly named reclaim as script/test/ci-native.sh: never
# Docker data, toolcache or a prune. The verifier's own capacity checks stay authoritative.
timeout --kill-after=10s 600 sudo rm -rf /usr/share/dotnet /usr/local/lib/android /opt/ghc /usr/local/share/boost
df -Pk / "$RUNNER_TEMP" /tmp > "$logs/capacity-after.log"
[ -c /dev/kvm ] || { echo '/dev/kvm is absent on this runner' >&2; exit 1; }
echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' \
    | sudo tee /etc/udev/rules.d/99-kvm4all.rules >/dev/null
sudo udevadm control --reload-rules
sudo udevadm trigger --name-match=kvm
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then break; fi
    sleep 1
done
[ -r /dev/kvm ] && [ -w /dev/kvm ] || { echo '/dev/kvm is not readable and writable after the udev grant' >&2; exit 1; }
# Permission bits are not proof; open the device the way the runtime will.
python3 -c 'import os; os.close(os.open("/dev/kvm", os.O_RDWR))'
probe > "$logs/kvm-after.log"
cat "$logs/kvm-after.log" "$logs/capacity-after.log"
