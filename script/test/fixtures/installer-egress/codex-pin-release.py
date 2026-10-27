#!/usr/bin/env python3
"""Print the exact release a committed Codex pin selects.

The restricted-egress gate has to prove that `selections.tsv`, the Dockerfile ARG
default and the committed pin all name the same release before it starts a proxy
or a container. Parsing JSON belongs in a file, not in a shell one-liner.

usage: codex-pin-release.py [--digests] PIN_JSON

`--digests` prints the release followed by one `<platform>\t<sha256>` line per
maintained platform, which is the identity projection two pins must share.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path


def main(argv: list[str]) -> int:
    digests = '--digests' in argv
    paths = [argument for argument in argv if argument != '--digests']
    if len(paths) != 1:
        print(__doc__, file=sys.stderr)
        return 2
    pin = json.loads(Path(paths[0]).read_text())
    release = pin.get('release')
    if not isinstance(release, str) or not release.startswith('rust-v'):
        print(f'{argv[0]}: no exact rust-v release', file=sys.stderr)
        return 1
    print(release)
    if digests:
        platforms = pin.get('platforms')
        if not isinstance(platforms, dict):
            print(f'{paths[0]}: no platforms', file=sys.stderr)
            return 1
        for platform in sorted(platforms):
            digest = platforms[platform].get('sha256')
            if not isinstance(digest, str):
                print(f'{paths[0]}: {platform} has no digest', file=sys.stderr)
                return 1
            print(f'{platform}\t{digest}')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
