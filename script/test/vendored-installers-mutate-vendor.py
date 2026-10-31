#!/usr/bin/env python3
"""Checked, single-site mutations of a vendored installer copy for the suites.

The strict-egress and vendored-installer suites prove that a *plausible defect*
fails its oracle. Both mutations here are scratch-only and self-verifying: each
asserts exactly one injection site and that the copy really differs, so a no-op
substitution cannot masquerade as a passing control.

usage: vendored-installers-mutate-vendor.py SOURCE DESTINATION VARIANT
       variants: latest-request, substitute-before-digest,
                 claude-manifest-request, claude-drop-launcher,
                 claude-skip-preflight, claude-setup-skip-initial-check,
                 claude-setup-check-after-native

The `claude-*` variants target either the Claude vendored installer or its
offline setup hook (`images/tools/claude/setup-claude.sh`); each one asserts a
single anchor and a real change, so a no-op edit cannot pass as a control. Three
of them are the ordering controls for the before-execution checks: deleting the
installer's preflight, deleting the setup step's initial `installed` call, or
moving that call after the native invocation must each be detected by the suite.
"""
from __future__ import annotations

import sys
from pathlib import Path

# Injected immediately before the selection call, so it is a real attempt through
# the same trusted downloader rather than a fake denial.
LATEST_ANCHOR = '\nresolve_release\n'
LATEST_INJECTION = ("\ndownload_file 'https://api.github.com/repos/openai/codex/"
                    "releases/latest' \"$TMPDIR/codex-latest-control\"\n"
                    "resolve_release\n")

# Injected INSIDE `download_file_with_fallback`, between the trusted download and
# that helper's own `verify_archive_digest`: the archive is replaced with wrong
# bytes while the pin and its evidence stay as committed. The site matters --
# injecting after the call site instead would land on the wrong side of the hash
# and be refused by the later Python preflight, which would make the shell
# checksum diagnostic this control exists to exercise unreachable.
CORRUPT_ANCHOR = '  download_file "$url" "$output" || return $?\n'
CORRUPT_INJECTION = ('  download_file "$url" "$output" || return $?\n'
                     "  printf 'substituted payload\\n' >\"$output\"\n")
# The two landmarks the injection point must sit between.
HELPER_START = 'download_file_with_fallback() {'
HELPER_DIGEST = 'verify_archive_digest "$output" "$expected_digest"'


# Injected immediately before the pin selection, so it is a real attempt through
# the same classified downloader rather than a fake denial.
CLAUDE_MANIFEST_ANCHOR = 'selection=$(python3 "$CLAUDE_PIN_DIR/claude_pin.py" select'
CLAUDE_MANIFEST_INJECTION = ('download_file "$DOWNLOAD_BASE_URL/$version/manifest.json" '
                             '"$DOWNLOAD_DIR/manifest.json"\n')
# Injected immediately before the native invocation in setup-claude.sh: the
# launcher disappears, so the native installer must try its own CDN.
CLAUDE_LAUNCHER_ANCHOR = 'rc=0\nenv -u AGENT_VM_TRANSPORT_RECEIPT'
CLAUDE_LAUNCHER_INJECTION = 'rc=0\nrm -f "$launcher"\nenv -u AGENT_VM_TRANSPORT_RECEIPT'
CLAUDE_NATIVE_LINE = 'env -u AGENT_VM_TRANSPORT_RECEIPT'
CLAUDE_PREFLIGHT_ANCHOR = ('# The digest proves which bytes these are, not that they '
                           'are the expected')
CLAUDE_SETUP_CHECK_ANCHOR = ('# Verify before execute: the bytes checked here are the '
                             'bytes the native step runs.')
CLAUDE_SETUP_POST_ANCHOR = ('# The native step must have left the verified payload and '
                            'layout as they were.')


def _delete_through(lines: list[str], start: int, end: int, what: str) -> list[str]:
    if end < start:
        raise SystemExit(f'{what}: empty span')
    return lines[:start] + lines[end + 1:]


def _command_block(lines: list[str], anchor_index: int, what: str) -> tuple[int, int]:
    """The `python3 ...` command after an anchor line, through its last line."""
    index = anchor_index + 1
    while index < len(lines) and not lines[index].strip():
        index += 1
    if index >= len(lines) or not lines[index].lstrip().startswith('python3'):
        raise SystemExit(f'{what}: no command follows the anchor')
    while lines[index].rstrip().endswith('\\'):
        index += 1
        if index >= len(lines):
            raise SystemExit(f'{what}: the command never ends')
    return anchor_index + 1, index


def _delete_claude_preflight(text: str) -> str:
    lines = text.split('\n')
    anchor = [index for index, line in enumerate(lines)
              if line == CLAUDE_PREFLIGHT_ANCHOR]
    if len(anchor) != 1:
        raise SystemExit(f'claude-skip-preflight: expected one anchor, found '
                         f'{len(anchor)}')
    start = anchor[0]
    end = None
    for index in range(start + 1, len(lines)):
        if lines[index] == 'fi':
            end = index
            break
    if end is None:
        raise SystemExit('claude-skip-preflight: the anchor block does not end')
    span = '\n'.join(lines[start:end + 1])
    if 'claude_pin.py" preflight' not in span:
        raise SystemExit('claude-skip-preflight: the deleted span is not the preflight')
    mutated = '\n'.join(_delete_through(lines, start, end, 'claude-skip-preflight'))
    if 'preflight' in mutated:
        raise SystemExit('claude-skip-preflight: a preflight call remains')
    return mutated


def _delete_claude_setup_check(text: str) -> tuple[str, list[str]]:
    lines = text.split('\n')
    anchor = [index for index, line in enumerate(lines)
              if line == CLAUDE_SETUP_CHECK_ANCHOR]
    if len(anchor) != 1:
        raise SystemExit(f'claude-setup check: expected one anchor, found '
                         f'{len(anchor)}')
    start, end = _command_block(lines, anchor[0], 'claude-setup check')
    span = '\n'.join(lines[anchor[0]:end + 1])
    if 'claude_pin.py" installed' not in span:
        raise SystemExit('claude-setup check: the deleted span is not the installed call')
    removed = lines[anchor[0]:end + 1]
    return '\n'.join(_delete_through(lines, anchor[0], end, 'claude-setup check')), removed


def _move_claude_setup_check(text: str) -> str:
    lines = text.split('\n')
    native = [index for index, line in enumerate(lines) if CLAUDE_NATIVE_LINE in line]
    post = [index for index, line in enumerate(lines) if line == CLAUDE_SETUP_POST_ANCHOR]
    anchor = [index for index, line in enumerate(lines)
              if line == CLAUDE_SETUP_CHECK_ANCHOR]
    if len(native) != 1 or len(post) != 1 or len(anchor) != 1:
        raise SystemExit('claude-setup-check-after-native: an anchor is missing or '
                         f'repeated (native {len(native)}, post {len(post)}, '
                         f'check {len(anchor)})')
    if not anchor[0] < native[0] < post[0]:
        raise SystemExit('claude-setup-check-after-native: the check is not before '
                         'the native invocation and before the post-check')
    removed_text, removed = _delete_claude_setup_check(text)
    mutated_lines = removed_text.split('\n')
    insertion = [index for index, line in enumerate(mutated_lines)
                 if line == CLAUDE_SETUP_POST_ANCHOR]
    if len(insertion) != 1:
        raise SystemExit('claude-setup-check-after-native: the insertion point moved')
    mutated_lines[insertion[0]:insertion[0]] = removed
    mutated = '\n'.join(mutated_lines)
    moved_native = [index for index, line in enumerate(mutated.split('\n'))
                    if CLAUDE_NATIVE_LINE in line]
    moved_check = mutated.split('\n').index(CLAUDE_SETUP_CHECK_ANCHOR)
    if len(moved_native) != 1 or not moved_native[0] < moved_check:
        raise SystemExit('claude-setup-check-after-native: the check did not move '
                         'after the native invocation')
    return mutated


def mutate(source: Path, destination: Path, variant: str) -> None:
    text = source.read_text()
    if variant == 'claude-manifest-request':
        occurrences = text.count(CLAUDE_MANIFEST_ANCHOR)
        if occurrences != 1:
            raise SystemExit(f'{variant}: expected exactly one injection site, '
                             f'found {occurrences}')
        mutated = text.replace(CLAUDE_MANIFEST_ANCHOR,
                              CLAUDE_MANIFEST_INJECTION + CLAUDE_MANIFEST_ANCHOR, 1)
        if mutated == text:
            raise SystemExit(f'{variant}: the mutation changed no bytes')
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(mutated)
        destination.chmod(0o644)
        return
    if variant == 'claude-drop-launcher':
        occurrences = text.count(CLAUDE_LAUNCHER_ANCHOR)
        if occurrences != 1:
            raise SystemExit(f'{variant}: expected exactly one injection site, '
                             f'found {occurrences}')
        mutated = text.replace(CLAUDE_LAUNCHER_ANCHOR, CLAUDE_LAUNCHER_INJECTION, 1)
        if mutated == text:
            raise SystemExit(f'{variant}: the mutation changed no bytes')
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(mutated)
        destination.chmod(0o644)
        return
    if variant == 'claude-skip-preflight':
        mutated = _delete_claude_preflight(text)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(mutated)
        destination.chmod(0o644)
        return
    if variant == 'claude-setup-skip-initial-check':
        mutated, _removed = _delete_claude_setup_check(text)
        if 'claude_pin.py" installed' not in mutated:
            raise SystemExit(f'{variant}: the closing installed call disappeared')
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(mutated)
        destination.chmod(0o644)
        return
    if variant == 'claude-setup-check-after-native':
        mutated = _move_claude_setup_check(text)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(mutated)
        destination.chmod(0o644)
        return
    if variant == 'latest-request':
        anchor, replacement = LATEST_ANCHOR, LATEST_INJECTION
    elif variant == 'substitute-before-digest':
        anchor, replacement = CORRUPT_ANCHOR, CORRUPT_INJECTION
    else:
        raise SystemExit(f'unknown variant {variant}')
    occurrences = text.count(anchor)
    if occurrences != 1:
        raise SystemExit(f'{variant}: expected exactly one injection site, '
                         f'found {occurrences}')
    if variant == 'substitute-before-digest':
        # Checked position, not just a checked count: the corruption has to be on
        # the download side of the helper's digest check.
        start = text.find(HELPER_START)
        digest = text.find(HELPER_DIGEST, start)
        site = text.find(anchor, start)
        if start < 0 or digest < 0 or site < 0 or not start < site < digest:
            raise SystemExit(f'{variant}: the injection site is not between '
                             f'{HELPER_START!r} and {HELPER_DIGEST!r}')
    mutated = text.replace(anchor, replacement, 1)
    if mutated == text:
        raise SystemExit(f'{variant}: the mutation changed no bytes')
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(mutated)
    destination.chmod(0o644)


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    mutate(Path(argv[0]), Path(argv[1]), argv[2])
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
