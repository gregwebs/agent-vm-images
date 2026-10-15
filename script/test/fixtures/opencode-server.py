#!/usr/bin/env python3
"""Credential-free HTTP fixture for the native probe's transport boundaries."""
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import sys
import threading
import time

state = {"model_calls": 0}
mode = os.environ.get("TEST_PROBE_MODE", "ok")
password = os.environ["OPENCODE_PASSWORD"]
auth = "Basic " + base64.b64encode(f"opencode:{password}".encode()).decode()
entry = os.environ["TEST_PLUGIN_ENTRY"]
if os.environ.get("TEST_PID_FILE"):
    Path(os.environ["TEST_PID_FILE"]).write_text(str(os.getpid()))
if os.environ.get("TEST_SECRET_FILE"):
    Path(os.environ["TEST_SECRET_FILE"]).write_text(json.dumps([password, auth]))
if mode == "diagnostics":
    print(f"private-password={password} authority={auth}", file=sys.stderr, flush=True)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        is_health = self.path == "/v1/health"
        if (is_health and self.headers.get("Authorization")) or (not is_health and self.headers.get("Authorization") != auth):
            self.send_error(401)
            return
        directory = self.headers.get("x-opencode-directory")
        if not directory:
            self.send_error(400)
            return
        if self.path == "/api/model":
            state["model_calls"] += 1
            if (mode == "activation" and state["model_calls"] == 1) or mode == "activation-timeout":
                time.sleep(5.2)
                self.send_response(503)
                self.end_headers()
                self.wfile.write(b'{"message":"Location plugins did not finish activating","service":"plugin"}')
                return
            data = [{"id": "claude-fixture", "providerID": "claude-code"}]
        elif self.path == "/api/plugin":
            if not state["model_calls"]:
                self.send_error(409)
                return
            data = [{"id": "openchamber.claude-code", "state": {"status": "active"},
                     "source": {"type": "local", "path": entry}}]
        elif self.path == "/api/provider":
            data = [{"id": "claude-code", "activation": "enabled", "settings": {"baseURL": url + "/v1"}}]
        elif is_health:
            data = {"ok": True, "provider": "claude-code"}
        else:
            self.send_error(404)
            return
        if mode == "redirect":
            self.send_response(302)
            self.send_header("Location", url + "/stolen")
            self.end_headers()
            return
        self.send_response(200)
        self.end_headers()
        if mode == "duplicate":
            self.wfile.write(b'{"data":[],"data":[]}')
        elif mode == "oversized":
            try:
                self.wfile.write(b" " * (4 * 1024 * 1024 + 1))
            except (BrokenPipeError, ConnectionResetError):
                pass
        else:
            record = data if is_health else {"location": {"directory": "/wrong" if mode == "location" else directory}, "data": data}
            self.wfile.write(json.dumps(record).encode())


server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
url = f"http://127.0.0.1:{server.server_port}"
if mode == "slow":
    time.sleep(0.2)
print(json.dumps({"url": "http://example.invalid:80" if mode == "unsafe" else url}), flush=True)
threading.Thread(target=server.serve_forever, daemon=True).start()
sys.stdin.read()
server.shutdown()
server.server_close()
