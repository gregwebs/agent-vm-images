#!/usr/bin/env bash
# Daemon reconfiguration is permitted only on fresh disposable CI, never operators.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1
[ "${GITHUB_ACTIONS:-}" = true ] && [ "${RUNNER_OS:-}" = Linux ] || { echo 'disposable Linux Actions runner only' >&2; exit 1; }
sudo python3 - <<'PY'
import json
from pathlib import Path
p = Path('/etc/docker/daemon.json')
value = json.loads(p.read_text()) if p.exists() else {}
value.setdefault('features', {})['containerd-snapshotter'] = True
p.write_text(json.dumps(value))
PY
timeout --kill-after=10s 120 sudo systemctl restart docker
