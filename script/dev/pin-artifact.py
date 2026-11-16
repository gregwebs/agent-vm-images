#!/usr/bin/env python3
"""The shared publication transaction the not-yet-ported pin writers still use.

The Node and GitHub CLI writer moved to Go (`avm-pin artifact`, reached through
`script/dev/pin-artifact.sh`) together with its pin model and installer, so this
module no longer resolves a release, renders a pin or parses arguments. What is
left is the one transaction every pin writer publishes through: staging, a complete
backup, publication, reverse-order restore, and a retained work directory plus
`recovery.json` when a restore fails.

`script/dev/pin-agy.py`, `pin-claude.py` and `pin-codex.py` call `publish` here, and
three suites patch `Publisher`/`shutil.copyfile` to inject publication faults. The
transaction exists twice until the last of those writers is ported (#180-#182), at
which point this file is deleted and the Go implementation is the only one.
"""
from __future__ import annotations

import json
import os
import shutil
import signal
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'images' / 'artifacts'))

import artifact_pin  # noqa: E402,F401  (the writers bind one model module)
from artifact_pin import Refusal  # noqa: E402,F401


# --- publication ------------------------------------------------------------

class Recovery(Exception):
    """Rollback was not complete: the work directory is the only way back."""


class Publisher:
    """One transaction over files that may not exist yet.

    Three explicit phases - staging, a *complete* backup, publication - because
    the order decides what a failure may undo. Staging and a failed backup touch
    no destination at all; only publication can, so only publication is restored,
    and only as far as it actually got (the destination being written counts).
    The `.complete` marker is what makes that safe: a restore never guesses from a
    half-written backup directory.

    The conventions are the shell publisher's: an invocation-owned backup
    directory, a numbered backup per destination, a `.complete` marker, restore
    in reverse order, and a retained work directory plus `recovery.json` when a
    restore fails so nothing is guessed afterwards.
    """

    def __init__(self, root: Path = ROOT) -> None:
        # `root` is the repository the destinations are relative to. It is a
        # parameter, not the module constant, because the Codex owner writer
        # publishes its own six-file closure through this same transaction; the
        # default keeps the agy and Claude call sites unchanged.
        self.root = Path(root)
        # Absolute at creation, before any publication or recovery failure: a
        # retained work directory is printed during recovery, and a relative path
        # resolved from a different cwd would point nowhere.
        self.work = Path(os.path.abspath(
            tempfile.mkdtemp(prefix='pin-artifact.', dir=os.environ.get('TMPDIR'))))
        self.backup = self.work / 'backup'
        self.backup.mkdir()
        self.staged = self.work / 'staged'
        self.staged.mkdir()
        self.destinations: list[Path] = []
        self.existed: list[bool] = []
        self.phase = 'staging'
        # Pre-rendered recovery lines. The signal handler ends the process with
        # `os._exit(3)`, which skips every buffer flush and every line the caller
        # would print after `publish` returns; these are built here, at setup, and
        # written with `os.write`, which is what a handler may safely do.
        self.work_line = (f'pin-artifact: recovery material retained at {self.work}\n'
                          .encode())
        self.owner_line = b''
        self.owner_note = ''
        # Destinations publication may have written: set *before* each copy, so
        # the file being written is included in a rollback.
        self.touched = 0

    def report_owner(self, owner: str | None) -> None:
        """Record the caller's own scratch directory in the signal-safe form.

        The publisher cannot discover it: for the Codex owner writer it is the
        owner's work or stage directory, which only the caller knows, and the
        handler that exits the process runs before the caller can say anything.
        """
        if not owner:
            return
        self.owner_note = f'pin-artifact: owner scratch retained at {owner}'
        self.owner_line = (self.owner_note + '\n').encode()

    def stage(self, relative: str, raw: bytes) -> None:
        path = self.staged / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)

    def back_up(self) -> None:
        self.phase = 'backup'
        self.existed = []
        for index, destination in enumerate(self.destinations):
            exists = destination.exists() or destination.is_symlink()
            self.existed.append(exists)
            if not exists:
                continue
            temporary = self.backup / f'.tmp{index}'
            shutil.copyfile(destination, temporary)
            shutil.copystat(destination, temporary)
            temporary.rename(self.backup / str(index))
        (self.backup / '.complete').write_text('')

    def backup_complete(self) -> bool:
        marker = (self.backup / '.complete').is_file()
        return marker and len(self.existed) == len(self.destinations)

    def publish(self) -> None:
        self.phase = 'publication'
        for index, destination in enumerate(self.destinations):
            staged = self.staged / destination.relative_to(self.root)
            destination.parent.mkdir(parents=True, exist_ok=True)
            self.touched = index + 1
            shutil.copyfile(staged, destination)

    def restore(self) -> None:
        """Reverse order over what publication could have touched.

        Nothing to do before publication, and nothing to trust without a complete
        backup: restoring from a partial backup directory would replace good bytes
        with half a file.
        """
        if self.touched == 0:
            return
        if not self.backup_complete():
            raise OSError(f'the backup in {self.backup} is incomplete')
        for index in reversed(range(self.touched)):
            destination = self.destinations[index]
            if self.existed[index]:
                shutil.copyfile(self.backup / str(index), destination)
                shutil.copystat(self.backup / str(index), destination)
            elif destination.exists() or destination.is_symlink():
                destination.unlink()

    def recovery_record(self, error: str) -> None:
        record = [{'destination': str(destination.relative_to(self.root)),
                   'existed': self.existed[index] if index < len(self.existed) else None,
                   'backup': str(self.backup / str(index))
                   if index < len(self.existed) and self.existed[index] else None}
                  for index, destination in enumerate(self.destinations)]
        (self.work / 'recovery.json').write_text(
            json.dumps({'error': error, 'phase': self.phase, 'destinations': record},
                       indent=2) + '\n')

    def record_recovery_best_effort(self, error: str) -> None:
        """Write the recovery record without letting its failure escape.

        A rollback that failed and then cannot even describe itself is still a
        status-3 outcome: the retained `backup/` directory is the way back, and
        `recovery.json` is a convenience. Catching only `OSError` would let a
        serialization or encoding failure replace the status with a traceback and
        status 1, so *every* exception is caught here and reported instead.
        """
        try:
            self.recovery_record(error)
        except Exception as record_error:  # noqa: BLE001 - the contract is "never escape"
            print(f'pin-artifact: could not write the recovery record in {self.work} '
                  f'({record_error}); the backups listed below are still authoritative',
                  file=sys.stderr)


def symlinked(destination: Path, root: Path = ROOT) -> Path | None:
    """A destination reached through a symlink is a redirection, not a layout.

    Rejecting only a symlinked *leaf* let the publisher change files through a
    symlinked parent directory, so the leaf and every ancestor between the root
    and it are checked with `lstat` semantics.
    """
    if destination.is_symlink():
        return destination
    try:
        relative = destination.relative_to(root)
    except ValueError:
        return root
    current = root
    for part in relative.parts[:-1]:
        current = current / part
        if current.is_symlink():
            return current
        if current.exists() and not current.is_dir():
            return current
    return None


def publish(staged: dict[str, bytes], *, root: Path = ROOT,
            owner: str | None = None) -> int:
    """Publish `staged` as one transaction; report the caller's own scratch too.

    `owner` is the caller's scratch directory (absolute), if it has one. It is
    reported together with this transaction's work directory *from the signal
    handler as well*, because that handler leaves through `os._exit(3)`: a caller
    that only printed its own path after `publish` returned would never say it.
    """
    publisher = Publisher(root)
    publisher.report_owner(owner)
    destinations = [root / relative for relative in staged]
    refused = sorted({str(found) for found in
                      (symlinked(destination, root) for destination in destinations)
                      if found is not None})
    if refused:
        print(f'pin-artifact: refusing a path reached through a symlink: {refused}',
              file=sys.stderr)
        shutil.rmtree(publisher.work, ignore_errors=True)
        return 1
    publisher.destinations = destinations

    def interrupted(signum, _frame):
        # Async-signal-safe only: no print, no logging, no allocation of note. The
        # two paths that make recovery possible were rendered before the handlers
        # were installed.
        if publisher.phase == 'publication' and publisher.touched:
            try:
                publisher.restore()
            except OSError as error:
                publisher.record_recovery_best_effort(str(error))
                os.write(2, b'pin-artifact: interrupted and restore incomplete\n')
                os.write(2, publisher.owner_line)
                os.write(2, publisher.work_line)
                os.write(2, b'STOP: recover both directories above before further '
                            b'edits/builds/commits\n')
                os._exit(3)
            os.write(2, b'pin-artifact: interrupted; sources restored\n')
        else:
            shutil.rmtree(publisher.work, ignore_errors=True)
            os.write(2, b'pin-artifact: interrupted before publication; sources '
                        b'unchanged\n')
        os._exit(128 + signum)

    for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(signum, interrupted)
    try:
        for relative, raw in staged.items():
            publisher.stage(relative, raw)
        publisher.back_up()
        publisher.publish()
    except OSError as error:
        if publisher.touched == 0:
            print(f'pin-artifact: {publisher.phase} failed ({error}); no destination was '
                  f'touched, sources unchanged', file=sys.stderr)
            shutil.rmtree(publisher.work, ignore_errors=True)
            return 1
        print(f'pin-artifact: publication failed ({error}); restoring', file=sys.stderr)
        try:
            publisher.restore()
        except OSError as restore_error:
            publisher.record_recovery_best_effort(str(restore_error))
            print(f'pin-artifact: restore incomplete ({restore_error}); recovery material '
                  f'retained at {publisher.work}', file=sys.stderr)
            if publisher.owner_note:
                print(publisher.owner_note, file=sys.stderr)
            print(f'STOP: recover {publisher.work}'
                  + (f' together with {owner}' if owner else '')
                  + ' before further edits/builds/commits', file=sys.stderr)
            return 3
        shutil.rmtree(publisher.work, ignore_errors=True)
        return 1
    shutil.rmtree(publisher.work, ignore_errors=True)
    return 0
