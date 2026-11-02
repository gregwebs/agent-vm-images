#!/usr/bin/env python3
"""CONNECT/SNI/Host disagreement client for the network-request egress smoke.

Runs inside the image under test. Each case is one TCP connection to the proxy
carrying exactly the CONNECT authority, the TLS SNI and the HTTP Host the case
names, so the runner can prove the proxy refused a disagreement between them
rather than a merely unlucky request. It has no verdict logic -- it prints the
raw outcome and the runner judges it, because the runner also holds the proxy
log and the declared host list.

The case grammar is deliberately one line of argv:
    --case NAME CONNECT SNI HOST

usage: destination-probe.py --proxy HOST:PORT --ca FILE --case NAME CONNECT SNI HOST ...
"""
from __future__ import annotations

import argparse
import socket
import ssl
import sys

BLOCK_MARKER = b'blocked by installer-egress allowlist'
TIMEOUT_SECONDS = 20


def _response(sock: socket.socket) -> tuple[int | None, bytes]:
    """The status code and body of one HTTP/1.1 response, best effort."""
    data = b''
    while b'\r\n\r\n' not in data:
        chunk = sock.recv(4096)
        if not chunk:
            break
        data += chunk
    head, _, body = data.partition(b'\r\n\r\n')
    status = None
    lines = head.split(b'\r\n')
    if lines and lines[0]:
        parts = lines[0].split()
        if len(parts) >= 2 and parts[1].isdigit():
            status = int(parts[1])
    length = None
    for line in lines[1:]:
        name, _, value = line.partition(b':')
        if name.strip().lower() == b'content-length' and value.strip().isdigit():
            length = int(value.strip())
    while length is not None and len(body) < length:
        chunk = sock.recv(4096)
        if not chunk:
            break
        body += chunk
    return status, body


def probe(proxy: tuple[str, int], ca: str, case: tuple[str, str, str, str]) -> str:
    name, connect_host, sni, host = case
    connect_status = 'refused'
    tls = 'failed'
    http_status = 'none'
    blocked = 'no'
    try:
        sock = socket.create_connection(proxy, timeout=TIMEOUT_SECONDS)
    except OSError as error:
        print(f'destination-probe: {name}: cannot reach the proxy: {error}', file=sys.stderr)
        raise SystemExit(1)
    try:
        sock.sendall(f'CONNECT {connect_host}:443 HTTP/1.1\r\n'
                     f'Host: {connect_host}:443\r\n\r\n'.encode())
        status, body = _response(sock)
        if status is None:
            blocked = 'yes' if BLOCK_MARKER in body else 'no'
            return _line(name, connect_status, tls, http_status, blocked)
        connect_status = str(status)
        if status != 200:
            blocked = 'yes' if BLOCK_MARKER in body else 'no'
            return _line(name, connect_status, tls, http_status, blocked)
        context = ssl.create_default_context(cafile=ca)
        try:
            tunnel = context.wrap_socket(sock, server_hostname=sni)
        except ssl.SSLError as error:
            print(f'destination-probe: {name}: TLS refused: {error}', file=sys.stderr)
            return _line(name, connect_status, tls, http_status, blocked)
        tls = 'ok'
        tunnel.sendall(b'GET / HTTP/1.1\r\n'
                       + f'Host: {host}\r\n'.encode()
                       + b'Connection: close\r\n\r\n')
        status, body = _response(tunnel)
        if status is not None:
            http_status = str(status)
        blocked = 'yes' if BLOCK_MARKER in body else 'no'
        tunnel.close()
    finally:
        sock.close()
    return _line(name, connect_status, tls, http_status, blocked)


def _line(name: str, connect_status: str, tls: str, http_status: str, blocked: str) -> str:
    line = (f'case={name} connect_status={connect_status} tls={tls} '
            f'http_status={http_status} blocked_by_proxy={blocked}')
    print(line)
    return line


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--proxy', required=True, help='HOST:PORT of the restricted proxy')
    parser.add_argument('--ca', required=True, help='PEM bundle that trusts the proxy CA')
    parser.add_argument('--case', action='append', nargs=4, required=True,
                        metavar=('NAME', 'CONNECT', 'SNI', 'HOST'))
    arguments = parser.parse_args(argv)
    host, _, port = arguments.proxy.rpartition(':')
    if not host or not port.isdigit():
        parser.error('--proxy must be HOST:PORT')
    for case in arguments.case:
        probe((host, int(port)), arguments.ca, tuple(case))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
