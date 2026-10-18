#!/usr/bin/env python3
"""Web fixture for the bounded readiness probe. Control flow only.

Each case reproduces one adversarial shape the real `dsh web` can take (early
exit, HMR/sandbox failure, delayed startup, wrong body, bare valid page, hang,
ignored TERM, surviving descendant, changed origin, double redirect). The
fixture never pretends to be the shipped frontend: the probe's own invariant is
that a response is accepted only when this invocation's token and the reviewed
markers both match.
"""
import http.server
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import threading
import time

MARKERS = ('<!doctype html>', '__DSH_BOOT__', '<title>DeepSeek Harness</title>')
PAGE = ('<!doctype html><html><head><title>DeepSeek Harness</title></head>'
        '<body><div id="root"></div><script>window.__DSH_BOOT__ = {}</script></body></html>')
TOKEN = secrets.token_urlsafe(32)


class Handler(http.server.BaseHTTPRequestHandler):
    case = 'valid'
    token = TOKEN
    port = 0

    def log_message(self, *_args):
        pass

    def reply(self, status, body=b'', headers=()):
        self.send_response(status)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Connection', 'close')
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self):
        path = self.path
        token_path = f'/?token={self.token}'
        if self.case == 'wrong-body':
            self.reply(200, b'<!doctype html><html><body>other page</body></html>')
        elif self.case == 'wrong-status':
            self.reply(500, b'internal error')
        elif self.case == 'bare-200':
            if path == '/':
                self.reply(200, PAGE.encode())      # a foreign server that rejects our token
            else:
                self.reply(404, b'no such session')
        elif self.case == 'changed-origin':
            other = self.port + 1 if self.port < 65535 else self.port - 1
            self.reply(303, headers=(('Location', f'http://127.0.0.1:{other}/?token={self.token}'),))
        elif self.case == 'double-redirect':
            if path == token_path:
                self.reply(303, headers=(('Location', '/'),))
            elif path == '/':
                self.reply(302, headers=(('Location', '/final'),))
            else:
                self.reply(200, PAGE.encode())
        elif path == token_path:
            # The shipped server accepts the token and only then serves the app;
            # the cookie jar proves the redirect chain is same-origin.
            self.reply(303, headers=(('Location', '/'), ('Set-Cookie', 'session=fixture; Path=/')))
        elif path == '/':
            if 'session=fixture' in (self.headers.get('Cookie') or ''):
                self.reply(200, PAGE.encode())
            else:
                self.reply(303, headers=(('Location', token_path),))
        else:
            self.reply(404, b'not found')


def serve(case, port):
    Handler.case = case
    Handler.port = port
    server = http.server.ThreadingHTTPServer(('127.0.0.1', port), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def announce(port):
    print(f'dsh web: http://127.0.0.1:{port}/?token={TOKEN}', flush=True)


def hold():
    # A detached descendant: it leaves the probe's process group but keeps the
    # inherited listening socket, so only the port check can notice it survived.
    pidfile = os.environ.get('DSH_WEB_FIXTURE_PIDFILE')
    if pidfile:
        Path(pidfile).write_text(str(os.getpid()))
    while True:
        time.sleep(1)


def main(argv):
    case = os.environ.get('DSH_WEB_FIXTURE_CASE', 'valid')
    if '--hold' in argv:
        hold()
        return 0
    port = int(argv[argv.index('--port') + 1]) if '--port' in argv else 0
    if case == 'exec-temp':
        directory = Path(os.environ['TMPDIR'])
        assert str(directory) == os.environ['TMP'] == os.environ['TEMP']
        assert directory.name.startswith('dsh-probe-')
        assert directory.stat().st_uid == os.getuid()
        assert directory.stat().st_mode & 0o777 == 0o700
        assert not directory.is_relative_to(Path(os.environ['HOME']))
        executable = directory / 'exec-check'
        executable.write_text('#!/bin/sh\nexit 0\n')
        executable.chmod(0o700)
        subprocess.run([str(executable)], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        Path(os.environ['DSH_WEB_FIXTURE_TMP_RECORD']).write_text(str(directory))
    if case == 'early-exit':
        print('fixture: provider credentials are unavailable', file=sys.stderr, flush=True)
        return 1
    if case == 'hmr-error':
        print('Error: dsh web requires the Cordis HMR service', file=sys.stderr, flush=True)
        return 1
    if case == 'sandbox-error':
        print("Error: Cannot find module '@deepseek-ai/dsh-sandbox-local'", file=sys.stderr, flush=True)
        return 1
    if case == 'hang':
        while True:
            time.sleep(1)
    if case == 'delayed-valid':
        time.sleep(float(os.environ.get('DSH_WEB_FIXTURE_DELAY', '0.3')))
    if case == 'ignore-term':
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    server = serve(case, port)
    if case == 'descendant':
        subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])
    if case == 'escaping-descendant':
        subprocess.Popen([sys.executable, os.path.abspath(__file__), '--hold'],
                         pass_fds=(server.socket.fileno(),), start_new_session=True)
    announce(server.server_address[1])
    while True:
        time.sleep(1)


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
