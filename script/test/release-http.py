#!/usr/bin/env python3
"""Real curl pagination/status controls, confined to a sanitized loopback server."""
import importlib.util
import json
import os
import subprocess
from unittest import mock
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import urlparse

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'script/release'))
import preflight
import public_http


class HTTPControls(unittest.TestCase):
    def setUp(self):
        self.responses, self.paths = [], []
        outer = self
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                outer.paths.append(self.path)
                status, body, headers = outer.responses.pop(0)
                self.send_response(status)
                for name, value in headers.items():
                    self.send_header(name, value)
                self.end_headers()
                self.wfile.write(json.dumps(body).encode())
            def log_message(self, *_):
                pass
        self.server = HTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        endpoint = f'http://127.0.0.1:{self.server.server_port}'
        class FixtureCurl(preflight.Curl):
            protocol = '=http'
            def request_url(self, url):
                parsed = urlparse(url)
                if parsed.netloc != 'api.github.com' or parsed.scheme != 'https':
                    raise ValueError('unapproved fixture source')
                return endpoint + parsed.path + ('?' + parsed.query if parsed.query else '')
        self.client = FixtureCurl(Path(self.directory.name))
    def check(self):
        return preflight.release_tag_absence(self.client, token='nonsecret-fixture-token',
                                             version='0.1.0').value

    def response(self, status, body, headers=None):
        self.responses.append((status, body, headers or {}))

    def setup_absence(self, draft=False, page2=False):
        self.response(200, {'full_name': preflight.REPO})
        record = {'id': 264, 'tag_name': 'v0.1.0', 'draft': True}
        link = {'Link': '<https://api.github.com/repos/gregwebs/agent-vm-images/releases?per_page=100&page=2>; rel="next"'}
        self.response(200, [] if page2 or not draft else [record], link if page2 else {})
        if page2:
            self.response(200, [record] if draft else [])
        self.response(404, {'message': 'Not Found'})
        self.response(404, {'message': 'Not Found'})

    def test_absence_then_intervening_draft_and_page_two(self):
        self.setup_absence()
        self.assertEqual(self.check(), 'ABSENT')
        self.setup_absence(draft=True, page2=True)
        self.assertEqual(self.check(), 'OCCUPIED_BY_DRAFT')
        self.assertTrue(any('page=2' in path for path in self.paths))
        self.setup_absence(draft=True)
        self.assertEqual(self.check(), 'OCCUPIED_BY_DRAFT')

    def test_page_two_retry_restarts_snapshot_and_sees_intervening_draft(self):
        self.response(200, {'full_name': preflight.REPO})
        self.response(200, [], {'Link': '<https://api.github.com/repos/gregwebs/agent-vm-images/releases?per_page=100&page=2>; rel="next"'})
        self.response(503, {'message': 'transient'})
        self.setup_absence(draft=True)
        self.assertEqual(self.check(), 'OCCUPIED_BY_DRAFT')
        self.assertEqual(sum(path.endswith('&page=1') for path in self.paths), 2)

    def test_errors_are_unauthorized_or_concealed(self):
        for status in (401, 403, 404):
            self.response(status, {'message': 'Not Found'})
            self.assertEqual(self.check(), 'UNAUTHORIZED_OR_CONCEALED')

    def test_transient_exhaustion_never_authorizes_partial_pages(self):
        for _ in range(3):
            self.response(503, {'message': 'fixture unavailable'})
        self.assertEqual(self.check(), 'TRANSIENT_FAILURE')
        self.assertEqual(len(self.paths), 3)


class PublicHTTPControls(unittest.TestCase):
    def test_curlrc_authorization_not_sent_and_stream_is_bounded(self):
        headers_seen = []
        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                headers_seen.append(dict(self.headers))
                self.send_response(200)
                # Deliberately no Content-Length.
                self.end_headers()
                try:
                    for _ in range(100):
                        self.wfile.write(b'x' * 4096)
                        self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    pass
            def log_message(self, *_):
                pass
        server = HTTPServer(('127.0.0.1', 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / '.curlrc').write_text('header = "Authorization: Bearer synthetic-secret"\ninsecure\n')
                with mock.patch.dict(os.environ, {'HOME': directory, 'CURL_HOME': directory}):
                    with self.assertRaises(ValueError):
                        public_http.transfer(['--silent', '--max-time', '10',
                            f'http://127.0.0.1:{server.server_port}/stream'], root / 'out', 1024)
                self.assertFalse((root / 'out').exists())
                self.assertEqual(len(headers_seen), 1)
                self.assertFalse(any(k.lower() == 'authorization' for k in headers_seen[0]))
        finally:
            server.shutdown()
            server.server_close()


if __name__ == '__main__':
    unittest.main()
