#!/usr/bin/env python3
"""Enable the containerd snapshotter in the CI daemon configuration.

Runs only through script/release/prepare-ci.sh, which refuses non-disposable
hosts before reaching this privileged write. Unrelated keys are preserved and
no newline is added: the daemon reads this file as JSON, and the surrounding
restart is the effect that matters.
"""
from __future__ import annotations

import json
from pathlib import Path

p = Path('/etc/docker/daemon.json')
value = json.loads(p.read_text()) if p.exists() else {}
value.setdefault('features', {})['containerd-snapshotter'] = True
p.write_text(json.dumps(value))
