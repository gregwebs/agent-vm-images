"""Live filesystem/directory sampling, not phase-boundary peak claims."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import os
import threading
import time
from pathlib import Path
from typing import Callable
from content import atomic_json


class Sampler:
    """Sample allocated files and free-space minima while commands are running.

    Sampling is an observed lower bound, not proof of between-sample maxima.
    Daemon read-only measurements are supplied by the caller for build runs.
    """
    def __init__(self, output: Path, paths: list[Path], daemon: Callable[[], object] | None = None):
        self.output, self.paths, self.daemon = output, paths, daemon
        self.stop_event = threading.Event()
        self.thread = threading.Thread(target=self.sample, daemon=True)
        self.error: Exception | None = None
        self.samples: list[dict[str, object]] = []

    def start(self) -> None:
        self.thread.start()

    def sample(self) -> None:
        try:
            while not self.stop_event.is_set():
                records = []
                for path in list(self.paths):
                    space = os.statvfs(path)
                    logical = allocated = 0
                    for member in path.rglob('*'):
                        try:
                            if member.is_file() and not member.is_symlink():
                                stat = member.stat()
                                logical += stat.st_size
                                allocated += stat.st_blocks * 512
                        except FileNotFoundError:
                            continue  # exporter ingests may rename between samples
                    records.append({'path': str(path), 'filesystem': str(path.stat().st_dev),
                                    'free_bytes': space.f_bavail * space.f_frsize,
                                    'logical_bytes': logical, 'allocated_bytes': allocated})
                value = {'time': time.time(), 'paths': records}
                if self.daemon is not None:
                    value['daemon'] = self.daemon()
                self.samples.append(value)
                atomic_json(self.output, {'interval_seconds': 2 if self.daemon is None else 15,
                    'measurement': 'sampled lower-bound peaks; free-space deltas include other host activity',
                    'samples': self.samples})
                self.stop_event.wait(2 if self.daemon is None else 15)
        except Exception as error:
            self.error = error

    def stop(self) -> None:
        self.stop_event.set()
        self.thread.join(timeout=75)
        if self.thread.is_alive() or self.error is not None:
            raise ValueError('peak sampling failed: ' + str(self.error))
