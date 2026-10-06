#!/usr/bin/env python3
"""Read-only daemon and client filesystem measurements; unknown capacity blocks."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import argparse
import json
import os
import platform
import subprocess
from pathlib import Path
from typing import TypedDict
from operations import docker_endpoint
from content import atomic_json, read_json, mapping, sequence


class FilesystemRecord(TypedDict):
    host: str
    filesystem: str
    path: str
    free_bytes: int


class StorageSnapshot(TypedDict):
    scratch_host: str
    daemon_filesystems: list[FilesystemRecord]


def measure(scratch: Path, output: Path) -> StorageSnapshot:
    info = mapping(json.loads(subprocess.check_output(['docker', 'info', '--format', '{{json .}}'])), 'daemon info')
    driver_status = [sequence(row, 'daemon driver status row')
                     for row in sequence(info.get('DriverStatus'), 'daemon driver status')]
    if ['driver-type', 'io.containerd.snapshotter.v1'] not in driver_status:
        raise ValueError('Docker containerd image store required')
    root = info.get('DockerRootDir')
    daemon_id = info.get('ID')
    if not isinstance(daemon_id, str) or not daemon_id:
        raise ValueError('daemon ID must be a nonempty string')
    address = mapping(info.get('Containerd', {}), 'daemon containerd').get('Address', '')
    if not isinstance(root, str) or not isinstance(address, str):
        raise ValueError('daemon storage root/address must be strings')
    # Docker-managed and standard distro containerd are the supported configured
    # locations. Custom roots must have reviewed evidence, not a guessed df.
    if address.startswith('/run/docker/containerd/') or address.startswith('/var/run/docker/containerd/'):
        containerd = root + '/containerd/daemon'
    elif address == '/run/containerd/containerd.sock':
        containerd = '/var/lib/containerd'
        if platform.system() == 'Linux':
            config = Path('/etc/containerd/config.toml')
            if config.exists():
                import tomllib
                containerd = tomllib.loads(config.read_text()).get('root', containerd)
    else:
        raise ValueError('unknown configured containerd storage root')
    paths = [root, containerd, root + '/buildkit']
    pins = read_json(Path(__file__).with_name('tool-pins.json'))
    argv = ['docker', 'run', '--rm', '--network', 'none', '--cap-drop', 'ALL', '--read-only']
    # Bind source existence is enforced by --mount; no creation of host paths.
    for number, path in enumerate(paths):
        if not isinstance(path, str) or not path.startswith('/') or any(x in path for x in ('..', ',', '\n')):
            raise ValueError('invalid daemon path')
        argv += ['--mount', f'type=bind,src={path},dst=/probe/{number},readonly']
    argv += [pins['fixture_base'], 'sh', '-c',
             'set -eu; for p in /probe/0 /probe/1 /probe/2; do '
             'stat -c "%d" "$p"; df -Pk "$p" | tail -n 1; done']
    result = subprocess.check_output(argv, timeout=60).decode().splitlines()
    if len(result) != 6:
        raise ValueError('malformed daemon filesystem probe')
    client_host = 'client:' + platform.node()
    daemon_host = 'daemon:' + daemon_id
    # Only an actually local Linux socket may share device-number identity.
    endpoint = docker_endpoint(json.loads(subprocess.check_output(['docker', 'context', 'inspect'])))
    if platform.system() == 'Linux' and endpoint in ('unix:///var/run/docker.sock', 'unix:///run/docker.sock'):
        daemon_host = client_host
    records: list[FilesystemRecord] = []
    for number, path in enumerate(paths):
        device = result[2 * number]
        fields = result[2 * number + 1].split()
        if not device.isdecimal() or len(fields) < 6 or not fields[3].isdigit():
            raise ValueError('invalid daemon capacity measurement')
        records.append({'host': daemon_host, 'filesystem': device, 'path': path, 'free_bytes': int(fields[3]) * 1024})
    # A thin-provisioned local VM spends physical client bytes as it grows.
    if platform.system() == 'Darwin':
        backing = Path(os.environ['RELEASE_VM_BACKING_PATH']).resolve(strict=True)
        stats, space = backing.stat(), os.statvfs(backing.parent)
        records.append({'host': client_host, 'filesystem': str(stats.st_dev), 'path': str(backing),
                        'free_bytes': space.f_bavail * space.f_frsize})
    value: StorageSnapshot = {'scratch_host': client_host, 'daemon_filesystems': records}
    atomic_json(output, value)
    return value


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--scratch', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(measure(args.scratch, args.output), indent=2))
    except (ValueError, OSError, KeyError, subprocess.SubprocessError) as error:
        print(f'storage: {error}', file=__import__('sys').stderr)
        raise SystemExit(1)
