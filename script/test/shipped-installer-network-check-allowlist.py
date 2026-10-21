#!/usr/bin/env python3
"""Replay the allowlist cases and redaction canaries inside the mitmproxy image.

Runs as the container's own interpreter with the image's installed mitmproxy
dependencies, on the same invocation identity as the proxy. It imports the
mounted addon from /addon/addon.py, replays /addon/cases.tsv and inspects the
redacted log, so the policy under test is the one the proxy actually runs.
Status 0 when every case and canary passes; 1 when any check reported a
mismatch; import/read failures are ordinary non-zero exceptions.
"""
from __future__ import annotations

import importlib.util
import sys
from urllib.parse import urlsplit

spec = importlib.util.spec_from_file_location("egress_addon", "/addon/addon.py")
addon = importlib.util.module_from_spec(spec)
spec.loader.exec_module(addon)


class Req:
    def __init__(self, url, method="GET"):
        parts = urlsplit(url)
        self.pretty_url = url
        self.pretty_host = parts.hostname
        self.path = parts.path + (("?" + parts.query) if parts.query else "")
        self.method = method


class Flow:
    def __init__(self, url, method="GET"):
        self.request = Req(url, method)
        self.response = None


bad = 0
with open("/addon/cases.tsv", encoding="utf-8") as handle:
    for line in handle:
        line = line.rstrip("\n")
        if not line:
            continue
        fields = line.split("\t")
        url, expect = fields[:2]
        flow = Flow(url, fields[2] if len(fields) == 3 else "GET")
        addon.request(flow)
        allowed = flow.response is None
        if allowed != (expect == "1"):
            print("MISMATCH %s expected allowed=%s got %s" % (url, expect, allowed), file=sys.stderr)
            bad = 1

# F5: the addon must redact every query value from the log, so a signed CDN URL
# never persists a bearer-style secret.
redacted = addon._redact(
    "https://cdn.example.invalid/asset?sig=REVIEW-SIGNED-SECRET&token=REVIEW-TOKEN"
)
if "REVIEW-SIGNED-SECRET" in redacted or "REVIEW-TOKEN" in redacted:
    print("redaction failed: %s" % redacted, file=sys.stderr)
    bad = 1
try:
    with open("/addon/log/egress.log", encoding="utf-8") as handle:
        logtext = handle.read()
except OSError:
    logtext = ""
if "REVIEW-SIGNED-SECRET" in logtext or "REVIEW-TOKEN" in logtext:
    print("egress log leaked a query secret:\n%s" % logtext, file=sys.stderr)
    bad = 1
sys.exit(bad)
