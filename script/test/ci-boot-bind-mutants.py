#!/usr/bin/env python3
"""Prove ci-boot-bind.sh's maintenance self-check still bites (#92).

Every case below edits a copy of `ci-boot-bind.sh` or of its trace predicate and requires the
diagnostic that edit must raise. Without them the self-check is only tested by itself: a future
change could drop the control commit, narrow the predicate, or delete a pin assertion and the
suite would stay green, which is the failure #88 was filed to close one level up.
"""
from __future__ import annotations

import os
import shlex
import shutil
import subprocess
import tempfile
import unittest
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = Path('script/test/ci-boot-bind.sh')
PREDICATE = Path('script/test/trace2_children.py')
# Suites the script drives but does not own: linked into the scratch root, never copied, because a
# mutant edits only the two files it names.
SHARED = (Path('script/test/ci-boot-bind-extract.py'), Path('script/test/ci-boot-go-setup.py'))

MAINTENANCE_PIN = '    git -C "$work/$repo" config maintenance.auto false\n'
GC_PIN = '    git -C "$work/$repo" config gc.auto 0\n'
MAINTENANCE_ASSERTION = (
    '[ "$(git -C "$work/harness" config --local --get maintenance.auto)" = false ] \\\n'
    "    || fail \"harness repository is missing 'maintenance.auto false'\"\n"
)
CONTROL_COMMIT = (
    'GIT_TRACE2_EVENT="$work/control-trace.json" git -C "$work/control" \\\n'
    '    -c maintenance.auto=true -c maintenance.autoDetach=false \\\n'
    '    -c user.name=Test -c user.email=test@example.invalid commit -qm control --allow-empty\n'
)
HARNESS_COMMIT_PREFIX = 'GIT_TRACE2_EVENT="$trace" git -C "$work/harness"'
TRACE_PREDICATE = "e['argv'][1:2] in [[s] for s in names]"


@dataclass(frozen=True)
class Edit:
    path: Path
    anchor: str
    replacement: str = ''


@dataclass(frozen=True)
class Mutation:
    """An edit that must break the self-check, and the message that must name the break."""

    name: str
    expected: str
    edits: tuple[Edit, ...] = ()
    # Set to make `git version` report this instead, so the version gate can be watched without an
    # old git binary; every other invocation is forwarded to the real git.
    reported_version: str = ''


MUTATIONS = (
    # #88 acceptance: deleting either pin fails, naming the setting git was told to ignore.
    Mutation(
        name='delete-maintenance-pin',
        expected="missing 'maintenance.auto false'",
        edits=(Edit(SCRIPT, MAINTENANCE_PIN),),
    ),
    Mutation(
        name='delete-gc-pin',
        expected="missing 'gc.auto 0'",
        edits=(Edit(SCRIPT, GC_PIN),),
    ),
    # Deleting the maintenance pin alone fails its value assertion, so the trace check is reached
    # only once that assertion goes too; gc.auto 0 alone does not stop `maintenance run --auto` on
    # git >= 2.30, which is why the child itself has to fail this run.
    Mutation(
        name='delete-pin-and-assertion',
        expected="harness commit spawned 'git maintenance run --auto",
        edits=(Edit(SCRIPT, MAINTENANCE_PIN), Edit(SCRIPT, MAINTENANCE_ASSERTION)),
    ),
    # The predicate is the check; a predicate that never matches has to be caught by the control.
    Mutation(
        name='weaken-predicate',
        expected='control commit spawned no maintenance child',
        edits=(Edit(PREDICATE, TRACE_PREDICATE, 'False'),),
    ),
    # The control is only a control while it forces maintenance past the repository pin.
    Mutation(
        name='drop-control-force',
        expected='control commit spawned no maintenance child',
        edits=(Edit(SCRIPT, '-c maintenance.auto=true '),),
    ),
    # Removing the control must fail rather than assert on a stream nobody wrote.
    Mutation(
        name='drop-control-commit',
        expected='control commit wrote no trace2 stream',
        edits=(Edit(SCRIPT, CONTROL_COMMIT),),
    ),
    # Tracing silently off would make the harness check vacuous, which is what its stream guard is
    # for: an empty or never-written stream must not read as "no child spawned".
    Mutation(
        name='untraced-harness-commit',
        expected='harness commit wrote no trace2 stream',
        edits=(Edit(SCRIPT, HARNESS_COMMIT_PREFIX, 'git -C "$work/harness"'),),
    ),
    # #92 acceptance: below git 2.30 the harness commit spawns gc --auto whatever the settings say,
    # so the gate must name the version instead of reporting a spawn nothing could prevent.
    Mutation(
        name='old-git-version',
        expected='needs git >=2.30 to observe',
        reported_version='git version 2.25.1',
    ),
    # The gate reads major.minor out of that string; a version it cannot parse must say so rather
    # than let `[ -lt ]` reject the operand and skip the gate.
    Mutation(
        name='unparseable-version',
        expected="cannot parse git version 'banana'",
        reported_version='git version banana',
    ),
)


def scratch_root(directory: Path) -> Path:
    """A root that resolves this checkout's shared files but owns mutable copies of the rest."""
    (directory / 'script/test').mkdir(parents=True)
    (directory / '.github').symlink_to(ROOT / '.github', target_is_directory=True)
    for shared in SHARED:
        (directory / shared).symlink_to(ROOT / shared)
    for owned in (SCRIPT, PREDICATE):
        shutil.copyfile(ROOT / owned, directory / owned)
    return directory


def environment() -> dict[str, str]:
    # A developer's global config changes what the fixture repositories do, and an inherited
    # trace2 session would seed the streams these cases read; neither belongs in the result.
    env = {key: value for key, value in os.environ.items() if not key.startswith('GIT_')}
    env['GIT_CONFIG_GLOBAL'] = os.devnull
    env['GIT_CONFIG_NOSYSTEM'] = '1'
    return env


def apply_mutation(root: Path, mutation: Mutation) -> dict[str, str]:
    """Apply the edits and any version shim; return the environment overrides a run needs."""
    for edit in mutation.edits:
        target = root / edit.path
        text = target.read_text(encoding='utf-8')
        # An anchor that is gone would leave the file unmodified, and the case would then pass for
        # the wrong reason: mutation cases rot silently unless the anchor has to be found.
        if text.count(edit.anchor) != 1:
            raise AssertionError(
                f'{mutation.name}: {edit.path} holds the mutation anchor {edit.anchor!r} '
                f'{text.count(edit.anchor)} times, not once; update ci-boot-bind-mutants.py'
            )
        target.write_text(text.replace(edit.anchor, edit.replacement), encoding='utf-8')
    if not mutation.reported_version:
        return {}
    shim = root / 'shim'
    shim.mkdir()
    fake_git = shim / 'git'
    fake_git.write_text(
        '#!/bin/sh\n'
        '# Only `version` is faked: everything else runs the real git, so the fixture repositories\n'
        '# behave normally and only the gate sees the reported version.\n'
        'if [ "$1" = version ]; then\n'
        f"    printf '%s\\n' {shlex.quote(mutation.reported_version)}\n"
        '    exit 0\n'
        'fi\n'
        f'exec {shlex.quote(shutil.which("git") or "git")} "$@"\n',
        encoding='utf-8',
    )
    fake_git.chmod(0o755)
    return {'PATH': f'{shim}{os.pathsep}{os.environ["PATH"]}'}


class SelfCheckMutationTest(unittest.TestCase):
    def run_self_check(
        self, directory: Path, extra_env: dict[str, str] | None = None
    ) -> subprocess.CompletedProcess[str]:
        env = environment()
        env.update(extra_env or {})
        return subprocess.run(
            ['bash', str(directory / SCRIPT)],
            cwd=directory,
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )

    def scratch(self) -> Path:
        directory = Path(tempfile.mkdtemp(prefix='ci-boot-bind-mutants-'))
        self.addCleanup(shutil.rmtree, directory)
        return scratch_root(directory)

    def test_unmutated_copy_passes(self) -> None:
        """The mutants only mean something while the same scratch copy passes untouched."""
        result = self.run_self_check(self.scratch())
        self.assertEqual(result.returncode, 0, result.stdout[-2000:] + result.stderr[-2000:])
        self.assertIn('ci boot bind controls passed', result.stdout)

    def test_mutations_break_the_self_check(self) -> None:
        for mutation in MUTATIONS:
            with self.subTest(mutation=mutation.name):
                root = self.scratch()
                extra_env = apply_mutation(root, mutation)
                result = self.run_self_check(root, extra_env)
                output = result.stdout + result.stderr
                self.assertNotEqual(
                    result.returncode,
                    0,
                    f'{mutation.name} still passed: the self-check does not bite',
                )
                self.assertIn(mutation.expected, output, f'{mutation.name} failed for another reason')

    def test_anchor_drift_is_refused(self) -> None:
        """The cases above are text edits, so prove a stale anchor fails instead of no-opping."""
        stale = Edit(SCRIPT, 'an anchor nobody wrote')
        with self.assertRaisesRegex(AssertionError, 'holds the mutation anchor'):
            apply_mutation(self.scratch(), Mutation(name='drifted', expected='', edits=(stale,)))


if __name__ == '__main__':
    unittest.main()
