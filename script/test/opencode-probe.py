#!/usr/bin/env python3
"""Exercise raw response mutations and a private, authenticated HTTP server."""
import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from unittest.mock import patch

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
PROBE = ROOT / "images/tools/opencode/probe-opencode.py"
spec = importlib.util.spec_from_file_location("opencode_probe", PROBE)
probe = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = probe
spec.loader.exec_module(probe)
fixture = ROOT / "script/test/fixtures/opencode-server.py"
entry = "/opt/agent-vm/opencode-plugins/index.js"

with tempfile.TemporaryDirectory() as work:
    work = Path(work)
    env = dict(os.environ, TEST_PID_FILE=str(work / "server-pid"), TEST_SECRET_FILE=str(work / "secrets"), TEST_PLUGIN_ENTRY=entry, OPENCODE_CONFIG="/image/config.json",
               ANTHROPIC_API_KEY="secret", CLAUDE_CONFIG_DIR="/credentials", OPENCODE_CONFIG_CONTENT='{"plugins":[]}',
               HTTPS_PROXY="http://127.0.0.1:1", HTTP_PROXY="http://127.0.0.1:1")
    with patch.dict(os.environ, env, clear=True):
        sanitized = probe.sanitized_environment(work, "private-password")
    assert sanitized["OPENCODE_CONFIG"] == "/image/config.json"
    for key in ("ANTHROPIC_API_KEY", "CLAUDE_CONFIG_DIR", "OPENCODE_CONFIG_CONTENT", "HTTPS_PROXY"):
        assert key not in sanitized
    for mode in ("ok", "activation", "activation-timeout", "slow", "diagnostics", "unsafe", "redirect", "oversized", "duplicate", "location"):
        result = subprocess.run([sys.executable, str(PROBE), "capture", "--binary", str(fixture),
                                 "--state-root", str(work), "--timeout", "10", "--directory", str(work)],
                                env=dict(env, TEST_PROBE_MODE=mode), capture_output=True, timeout=25)
        if mode in ("ok", "activation", "slow", "diagnostics"):
            assert result.returncode == 0, (mode, result.stderr)
            evidence = work / "evidence.json"
            evidence.write_bytes(result.stdout)
            probe.check(evidence, entry, str(work.resolve()))
            good = json.loads(result.stdout)
        else:
            assert result.returncode != 0, (mode, result.stdout)
        if (work / "secrets").exists():
            for authority in json.loads((work / "secrets").read_bytes()):
                assert authority.encode() not in result.stderr, 'probe authority leaked in diagnostics'
        if (work / "server-pid").exists():
            try:
                os.kill(int((work / "server-pid").read_text()), 0)
            except ProcessLookupError:
                pass
            else:
                raise AssertionError('invocation-owned server survived capture cleanup')
        assert b"secret" not in result.stderr
        assert b"Authorization" not in result.stderr
    # Independently mutate every load-bearing predicate, retaining raw-wire bytes.
    mutations = [
        ("model_b64", lambda r: r.update(data=[])),
        ("model_b64", lambda r: r["data"].append(copy.deepcopy(r["data"][0]))),
        ("model_b64", lambda r: r["data"][0].update(providerID="other")),
        ("plugin_b64", lambda r: r["data"][0]["state"].update(status="failed")),
        ("plugin_b64", lambda r: r["data"][0]["source"].update(path="/wrong")),
        ("plugin_b64", lambda r: r["data"].append(copy.deepcopy(r["data"][0]))),
        ("provider_b64", lambda r: r["data"][0].update(activation="disabled")),
        ("provider_b64", lambda r: r["data"][0]["settings"].update(baseURL="http://127.0.0.1:0/v1")),
        ("provider_b64", lambda r: r["data"][0]["settings"].update(baseURL="https://example.invalid:80/v1")),
        ("health_b64", lambda r: r.update(ok=False)),
        ("health_b64", lambda r: r.update(provider="other")),
    ]
    for key in ("model_b64", "plugin_b64", "provider_b64"):
        mutations.append((key, lambda r: r["location"].update(directory="/wrong")))
    for key, mutate in mutations:
        bad = copy.deepcopy(good)
        record = json.loads(base64.b64decode(bad[key]))
        mutate(record)
        bad[key] = base64.b64encode(json.dumps(record).encode()).decode()
        evidence.write_text(json.dumps(bad))
        try:
            probe.check(evidence, entry, str(work.resolve()))
        except (ValueError, KeyError, TypeError):
            pass
        else:
            raise AssertionError(f"mutation accepted: {key} {record}")
print("opencode probe: private transport, activation retry, raw-wire and location mutations passed")
