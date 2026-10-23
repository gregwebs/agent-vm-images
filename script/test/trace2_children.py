#!/usr/bin/env python3
"""Children a git command started, read from a trace2 event stream.

Shared by `release-operations.py` and `ci-boot-bind.sh` so the argv predicate that guards
cleanup has one implementation instead of a shell copy that drifts (#88). The command line
form prints one matching argv per line and exits 0 whether or not it found any, so a shell
caller reports the spawns rather than a status code.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path


def children(trace: Path, subcommand: str, *more: str) -> list[list[str]]:
    # git starts its own helpers with a fixed argv, ['git', <subcommand>, ...]; matching that exactly
    # is not a git-CLI parser (#85), so a new detached helper needs its name added by a caller (#89).
    events = [json.loads(line) for line in trace.read_text(encoding='utf-8').splitlines()]
    names = (subcommand, *more)
    return [e['argv'] for e in events if e['event'] == 'child_start' and e['argv'][1:2] in [[s] for s in names]]


def main(argv: list[str]) -> int:
    if len(argv) < 3:
        print(f'usage: {argv[0]} TRACE SUBCOMMAND [SUBCOMMAND ...]', file=sys.stderr)
        return 2
    trace, *names = argv[1:]
    for spawned in children(Path(trace), *names):
        print(' '.join(spawned))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv))
