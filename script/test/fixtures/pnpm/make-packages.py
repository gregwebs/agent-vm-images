#!/usr/bin/env python3
"""Harmless deterministic archives; one clock drives all publication evidence."""
import gzip
import io
import json
from pathlib import Path
import tarfile

FIXTURES = Path(__file__).resolve().parent


def make_package(name: str, version: str, dependency: bool) -> bytes:
    metadata = {'name': name, 'version': version, 'bin': {name.split('/')[-1]: 'bin.js'},
                'scripts': {'postinstall': 'node hook.js'}}
    if dependency:
        metadata['dependencies'] = {'fixture-dependency': '1.0.0'}
    files = {
        'package/package.json': json.dumps(metadata, sort_keys=True).encode(),
        'package/hook.js': (FIXTURES / 'package-hook.js').read_bytes(),
        'package/bin.js': (FIXTURES / 'package-bin.js').read_bytes(),
    }
    out = io.BytesIO()
    with gzip.GzipFile(fileobj=out, mode='wb', mtime=0, filename='') as compressed:
        with tarfile.open(fileobj=compressed, mode='w') as tar:
            for path, content in sorted(files.items()):
                entry = tarfile.TarInfo(path)
                entry.size = len(content)
                entry.mode = 0o755 if path.endswith('bin.js') else 0o644
                tar.addfile(entry, io.BytesIO(content))
    return out.getvalue()
