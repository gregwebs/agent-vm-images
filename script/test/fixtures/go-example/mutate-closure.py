#!/usr/bin/env python3
"""Rewrite one committed gopls-closure record so a build must reach Go's checksum.

The native harness's hard negative is only evidence if the build fails for the
committed-content reason and not for something earlier: an incoherent record
(with a stale file digest, or a pin/go.sum disagreement) would be refused by the
offline checker before a single byte was fetched, and a generic build failure
would prove nothing about module authentication.

So this tool performs one *coherent* mutation: the selected module keeps its
identity, and only its zip content hash changes -- in ``go.sum`` and in
``pin.modules[].h1`` at once, followed by the two digests that cover those files
(``files["go.sum"]`` and ``authentication.scope["go.sum"]``). Everything else is
left alone: ``go.mod``, the ``/go.mod`` hashes, the notice sections, the
reviewer/date and the policy text.

Usage: mutate-closure.py --data DIR --wrong-zip-h1 MODULE@VERSION
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import sys
from pathlib import Path


class MutationError(Exception):
    pass


def wrong_hash(module: str) -> str:
    """A syntactically valid h1 that no module zip can have."""
    digest = hashlib.sha256(f'wrong content for {module}'.encode()).digest()
    return 'h1:' + base64.b64encode(digest).decode('ascii')


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--data', required=True)
    parser.add_argument('--wrong-zip-h1', required=True, metavar='MODULE@VERSION')
    arguments = parser.parse_args(argv)
    try:
        module, _, version = arguments.wrong_zip_h1.partition('@')
        if not module or not version:
            raise MutationError('--wrong-zip-h1 takes MODULE@VERSION')
        data = Path(arguments.data)
        pin_path = data / 'pin.json'
        go_sum_path = data / 'go.sum'
        document = json.loads(pin_path.read_text(encoding='utf-8'))
        selected = [entry for entry in document['modules']
                    if entry['path'] == module and entry['version'] == version]
        if len(selected) != 1:
            raise MutationError(f'{module}@{version} is not selected exactly once')
        replacement = wrong_hash(arguments.wrong_zip_h1)
        if selected[0]['h1'] == replacement:
            raise MutationError('the replacement hash equals the committed one')

        lines = go_sum_path.read_text(encoding='utf-8').splitlines(keepends=True)
        matches = [index for index, line in enumerate(lines)
                   if line.startswith(f'{module} {version} h1:')]
        if len(matches) != 1:
            raise MutationError(f'go.sum has {len(matches)} zip h1 lines for '
                                f'{module}@{version}')
        lines[matches[0]] = f'{module} {version} {replacement}\n'
        rendered = ''.join(lines)
        go_sum_path.write_text(rendered, encoding='utf-8')

        selected[0]['h1'] = replacement
        digest = hashlib.sha256(rendered.encode('utf-8')).hexdigest()
        document['files']['go.sum'] = digest
        document['authentication']['scope']['go.sum'] = digest
        pin_path.write_text(json.dumps(document, indent=2) + '\n', encoding='utf-8')
    except MutationError as error:
        print(f'mutate-closure: {error}', file=sys.stderr)
        return 1
    print(f'mutate-closure: {module}@{version} zip h1 -> {replacement}')
    print(f'mutate-closure: files["go.sum"] and authentication.scope["go.sum"] -> {digest}')
    return 0


if __name__ == '__main__':
    sys.exit(main())
