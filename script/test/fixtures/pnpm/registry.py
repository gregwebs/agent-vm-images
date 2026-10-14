#!/usr/bin/env python3
"""Loopback registry, full metadata even when abbreviated metadata is requested."""
import base64
from datetime import datetime, timedelta, timezone
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
from pathlib import Path
import threading
from urllib.parse import unquote
from scenarios import read_scenarios

module = importlib.util.spec_from_file_location('make_packages', Path(__file__).with_name('make-packages.py'))
assert module and module.loader
packages = importlib.util.module_from_spec(module)
module.loader.exec_module(packages)


class Registry:
    def __init__(self) -> None:
        self.tarballs: dict[str, bytes] = {}
        self.documents: dict[str, bytes] = {}
        registry = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self) -> None:
                path = unquote(self.path.split('?', 1)[0]).lstrip('/')
                body = registry.tarballs.get(path, registry.documents.get(path))
                if body is None:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.send_header('Content-Type', 'application/octet-stream' if path.endswith('.tgz') else 'application/json')
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *_: object) -> None:
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.url = 'http://127.0.0.1:' + str(self.server.server_port)
        now = datetime.now(timezone.utc)
        for record in read_scenarios(Path(__file__).with_name('packages.json')):
            name = record.name
            versions = {}
            times = {}
            for selection in record.versions:
                version = selection.version
                archive = packages.make_package(name, version, selection.dependency)
                path = f'{name}/-/{name}-{version}.tgz'
                dist = {'tarball': self.url + '/' + path,
                        'integrity': 'sha512-' + base64.b64encode(hashlib.sha512(archive).digest()).decode()}
                if selection.provenance:
                    dist['attestations'] = {'provenance': {'predicateType': 'https://slsa.dev/provenance/v1'}}
                metadata = {'name': name, 'version': version, 'dist': dist,
                            'bin': {name.split('/')[-1]: 'bin.js'}, 'scripts': {'postinstall': 'node hook.js'}}
                if selection.dependency:
                    metadata['dependencies'] = {'fixture-dependency': '1.0.0'}
                versions[version] = metadata
                if selection.age_hours is not None:
                    times[version] = (now - timedelta(hours=selection.age_hours)).isoformat()
                self.tarballs[path] = archive + b'corrupt' if selection.corrupt else archive
            document = {'name': name, 'versions': versions, 'dist-tags': {'latest': list(versions)[-1]}}
            if times:
                document['time'] = times
            self.documents[name] = json.dumps(document).encode()
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self) -> 'Registry':
        self.thread.start()
        return self

    def __exit__(self, *_: object) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)
        assert not self.thread.is_alive(), 'fixture shutdown failed'
