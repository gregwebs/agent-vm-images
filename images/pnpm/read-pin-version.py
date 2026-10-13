#!/usr/bin/env python3
"""Print the pinned pnpm version from a pin.json document."""
import json
import sys


def main() -> None:
    with open(sys.argv[1]) as pin:
        print(json.load(pin)['version'])


if __name__ == '__main__':
    main()
