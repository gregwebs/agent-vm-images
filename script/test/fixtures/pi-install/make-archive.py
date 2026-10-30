#!/usr/bin/env python3
"""Build a hostile-free fixture npm archive without host metadata.

Usage: make-archive.py NAME VERSION DESTINATION

Python's tarfile is used instead of the host `tar` because macOS adds AppleDouble
`._package` members, which the real extractor refuses (correctly: an archive must
not carry names outside `package/`).
"""
import json
import os
import sys
import tarfile
import tempfile

name, version, destination = sys.argv[1:4]
with tempfile.TemporaryDirectory() as staging:
    package = os.path.join(staging, 'package')
    os.makedirs(package)
    with open(os.path.join(package, 'package.json'), 'w', encoding='utf-8') as handle:
        json.dump({'name': name, 'version': version, 'license': 'MIT'}, handle, sort_keys=True)
        handle.write('\n')
    with open(os.path.join(package, 'index.js'), 'w', encoding='utf-8') as handle:
        handle.write('export const fixture = 1;\n')
    with tarfile.open(destination, 'w:gz') as archive:
        for root, directories, files in os.walk(package):
            directories.sort()
            for entry in sorted(directories) + sorted(files):
                full = os.path.join(root, entry)
                info = archive.gettarinfo(full, os.path.relpath(full, staging))
                info.uid = info.gid = 0
                info.mtime = 0
                info.uname = info.gname = ''
                if info.isfile():
                    with open(full, 'rb') as handle:
                        archive.addfile(info, handle)
                else:
                    archive.addfile(info)
