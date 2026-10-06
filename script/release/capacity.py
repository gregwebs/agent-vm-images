#!/usr/bin/env python3
"""Fail-closed, host-qualified allocation accounting for standard releases."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import argparse
import json
import os
from dataclasses import dataclass
from pathlib import Path

GIB = 1024 ** 3
RESERVE = 5 * GIB
TAR_OVERHEAD = 64 * 1024 ** 2


@dataclass(frozen=True)
class Filesystem:
    host: str
    filesystem: str
    path: str
    free_bytes: int

    @classmethod
    def parse(cls, value: dict) -> Filesystem:
        if set(value) != {'host', 'filesystem', 'path', 'free_bytes'}:
            raise ValueError('filesystem record requires host/filesystem/path/free_bytes')
        for field in ('host', 'filesystem', 'path'):
            if not isinstance(value[field], str) or not value[field]:
                raise ValueError(f'invalid filesystem {field}')
        free = value['free_bytes']
        if type(free) is not int or free < 0:
            raise ValueError('invalid filesystem free bytes')
        return cls(**value)


def check(phase: str, scratch: Filesystem, daemon: tuple[Filesystem, ...],
          *, uncompressed: int, compressed: int, build: int,
          tar_bytes: int | None = None) -> dict:
    if min(uncompressed, compressed, build) <= 0:
        raise ValueError('resource caps must be positive')
    tar = compressed + TAR_OVERHEAD if tar_bytes is None else tar_bytes
    if tar <= 0 or tar > compressed + TAR_OVERHEAD:
        raise ValueError('tar size exceeds committed framing cap')
    # These are remaining allocations, not retained bytes already deducted by df.
    phases = {
        'initial': (build + uncompressed + compressed, compressed + 2 * tar),
        'build': (build, 2 * compressed),
        'load': (build + uncompressed + compressed, tar),
        'stage': (0, tar),
    }
    if phase not in phases:
        raise ValueError('unsupported release capacity phase')
    daemon_allocation, host_allocation = phases[phase]
    if not daemon:
        raise ValueError('daemon storage measurements are mandatory')
    groups: dict[tuple[str, str], dict] = {}

    def add(fs: Filesystem, amount: int) -> None:
        key = (fs.host, fs.filesystem)
        group = groups.setdefault(key, {'free_bytes': fs.free_bytes,
                                        'allocation_bytes': 0, 'paths': []})
        group['free_bytes'] = min(group['free_bytes'], fs.free_bytes)
        group['allocation_bytes'] += amount
        group['paths'].append(fs.path)

    add(scratch, host_allocation)
    # A root/content/snapshotter path on one daemon filesystem describes one
    # allowance, not three independently spendable pools. Distinct filesystems
    # each need the full bound until an actual allocation partition is known.
    seen: set[tuple[str, str]] = set()
    for fs in daemon:
        key = (fs.host, fs.filesystem)
        add(fs, 0 if key in seen else daemon_allocation)
        seen.add(key)
    result = {'phase': phase, 'filesystems': []}
    for (host, filesystem), group in groups.items():
        required = group['allocation_bytes'] + RESERVE
        record = dict(group, host=host, filesystem=filesystem, required_bytes=required)
        result['filesystems'].append(record)
        if group['free_bytes'] < required:
            raise ValueError(f'insufficient space on {host}:{filesystem}: '
                             f'{group["free_bytes"]} free; {required} required')
    return result


def check_space(path: Path, additional_bytes: int) -> None:
    if type(additional_bytes) is not int or additional_bytes < 0:
        raise ValueError('invalid remaining allocation')
    space = os.statvfs(path)
    free = space.f_bavail * space.f_frsize
    if free < additional_bytes + RESERVE:
        raise ValueError(f'insufficient capacity: {path}: {free} free; {additional_bytes + RESERVE} required')


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    cli = sub.add_parser('check')
    cli.add_argument('--phase', required=True, choices=('initial', 'build', 'load', 'stage'))
    cli.add_argument('--scratch', required=True, type=Path)
    cli.add_argument('--daemon-stats', required=True, type=Path)
    cli.add_argument('--uncompressed-cap-bytes', type=int, default=8 * GIB)
    cli.add_argument('--compressed-cap-bytes', type=int, default=4 * GIB)
    cli.add_argument('--build-cap-bytes', type=int, default=25 * GIB)
    cli.add_argument('--tar-bytes', type=int)
    args = parser.parse_args()
    try:
        stats = json.loads(args.daemon_stats.read_text())
        if set(stats) != {'scratch_host', 'daemon_filesystems'}:
            raise ValueError('unknown storage measurement schema')
        stat = os.stat(args.scratch)
        space = os.statvfs(args.scratch)
        scratch = Filesystem.parse({'host': stats['scratch_host'],
                                    'filesystem': str(stat.st_dev),
                                    'path': str(args.scratch.resolve()),
                                    'free_bytes': space.f_bavail * space.f_frsize})
        result = check(args.phase, scratch,
                       tuple(Filesystem.parse(x) for x in stats['daemon_filesystems']),
                       uncompressed=args.uncompressed_cap_bytes,
                       compressed=args.compressed_cap_bytes,
                       build=args.build_cap_bytes, tar_bytes=args.tar_bytes)
        print(json.dumps(result, indent=2))
    except (ValueError, OSError, KeyError, TypeError) as error:
        print(f'capacity: {error}', file=__import__('sys').stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
