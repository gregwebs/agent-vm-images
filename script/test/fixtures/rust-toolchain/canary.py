#!/usr/bin/env python3
"""HTTP recorder for the first-use legs: it logs every request it receives.

The container under test is given RUSTUP_DIST_SERVER=http://canary:PORT so any
attempted distribution request is observed instead of reaching the internet.
"""
from __future__ import annotations

import http.server
import sys


class Recorder(http.server.BaseHTTPRequestHandler):
    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler protocol
        print(f'request {self.path}', flush=True)
        self.send_response(404)
        self.end_headers()

    def log_message(self, *arguments) -> None:
        pass


def main() -> int:
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8765
    server = http.server.HTTPServer(('0.0.0.0', port), Recorder)
    print(f'canary listening on {port}', flush=True)
    server.serve_forever()
    return 0


if __name__ == '__main__':
    sys.exit(main())
