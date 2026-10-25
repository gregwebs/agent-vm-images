#!/usr/bin/env python3
"""Print a value one committed artifact pin selects.

The runtime suites and the gate compare an image's report with the pin, so this
reads the pin through the same validator the installer uses instead of matching
JSON text. `--npm` prints the npm version the reviewed Node archive ships, which
is the only npm the image may report.
"""
import sys
from pathlib import Path

import artifact_pin


def main(argv: list[str]) -> int:
    npm = '--npm' in argv
    paths = [argument for argument in argv if argument != '--npm']
    if not argv or len(paths) != 1:
        print('usage: read-pin-version.py PIN.json [--npm]', file=sys.stderr)
        return 2
    pin = artifact_pin.load(Path(paths[0]))
    if not npm:
        print(pin.version)
        return 0
    if pin.bundled is None or 'npm' not in pin.bundled:
        print(f'read-pin-version: {paths[0]} records no bundled npm', file=sys.stderr)
        return 1
    print(pin.bundled['npm'])
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
