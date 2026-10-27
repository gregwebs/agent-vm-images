#!/usr/bin/env python3
"""A loopback GOPROXY for the gopls-closure fixtures.

It speaks the small part of the proxy protocol the Go command uses
(``<module>/@v/<version>.info|.mod|.zip``) from an in-memory store, records every
request path, and can withhold or replace one module object so a checksum-mismatch
control reaches the *Go* layer.

Scope of the claim: this is detection, not isolation. The network boundary is the
OS sandbox the suites start inside; the recorder proves which paths a build asked
for, so a happy path can assert that nothing outside the pinned identities was
requested. ``/@v/list`` is served only when a test enables it, because an ordinary
build must never ask for a version list.
"""
from __future__ import annotations

import http.server
import json
import socketserver
import urllib.parse
import threading

INFO_TIME = '2024-01-01T00:00:00Z'


class Proxy:
    def __init__(self) -> None:
        self.files: dict[str, bytes] = {}
        self.requests: list[str] = []
        self.faults: dict[str, bytes | None] = {}
        self.lists: dict[str, list[str]] = {}
        recorder = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def do_GET(self) -> None:  # noqa: N802 - http.server protocol name
                # The request target is percent-encoded, and a module path with an
                # uppercase letter is escaped twice (``!`` then ``%21``). Store the
                # decoded proxy path so the fixture keys stay readable.
                path = urllib.parse.unquote(self.path)
                recorder.requests.append(path)
                if path in recorder.faults:
                    raw = recorder.faults[path]
                    if raw is None:
                        self.send_error(404)
                        return
                    self._serve(raw)
                    return
                if path.endswith('/@v/list'):
                    module = path[:-len('/@v/list')].lstrip('/')
                    versions = recorder.lists.get(module)
                    if versions is None:
                        self.send_error(404)
                        return
                    self._serve(('\n'.join(versions) + '\n').encode())
                    return
                raw = recorder.files.get(path)
                if raw is None:
                    self.send_error(404)
                    return
                self._serve(raw)

            def _serve(self, raw: bytes) -> None:
                self.send_response(200)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *_arguments) -> None:
                pass

        class Server(http.server.ThreadingHTTPServer):
            # http.server resolves the bound address with a reverse DNS lookup,
            # which a fixture child must not make: it is an off-host query, and
            # the audit hook in guards.py refuses it for exactly that reason.
            def server_bind(self) -> None:
                socketserver.TCPServer.server_bind(self)
                self.server_name = '127.0.0.1'
                self.server_port = self.server_address[1]

        self._server = Server(('127.0.0.1', 0), Handler)
        self._server.daemon_threads = True
        self.port = self._server.server_address[1]
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    # --- store ---------------------------------------------------------------
    def add_module(self, path: str, version: str, *,
                   zip_bytes: bytes, mod_bytes: bytes) -> None:
        prefix = f'/{path}/@v/{version}'
        # A synthetic module has no real VCS origin. The record's provenance block
        # is unauthenticated metadata either way (``authenticated: false``), and
        # the checker requires it to be complete, so the fixture serves a labelled
        # placeholder instead of teaching the checker to accept blanks.
        origin = {'VCS': 'git', 'URL': f'https://example.test/{path}', 'Subdir': '',
                  'Ref': f'refs/tags/{version}', 'Hash': '0' * 40}
        self.files[prefix + '.info'] = json.dumps(
            {'Version': version, 'Time': INFO_TIME, 'Origin': origin}).encode()
        self.files[prefix + '.mod'] = mod_bytes
        self.files[prefix + '.zip'] = zip_bytes

    def add_list(self, path: str, versions: list[str]) -> None:
        self.lists[path] = versions

    def replace(self, path: str, version: str, kind: str, raw: bytes) -> None:
        self.faults[f'/{path}/@v/{version}.{kind}'] = raw

    def withhold(self, path: str, version: str, kind: str) -> None:
        self.faults[f'/{path}/@v/{version}.{kind}'] = None

    # --- observations --------------------------------------------------------
    def url(self) -> str:
        return f'http://127.0.0.1:{self.port}'

    def clear_requests(self) -> None:
        self.requests.clear()

    def requested_paths(self) -> list[str]:
        return list(self.requests)

    def forbidden_requests(self) -> list[str]:
        """Paths an ordinary build may never ask for."""
        forbidden = []
        for path in self.requests:
            if '/sumdb/' in path or path.endswith('/@v/list') or path.endswith('/@latest'):
                forbidden.append(path)
            elif path.startswith('/golang.org/toolchain'):
                forbidden.append(path)
        return forbidden

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=10)
