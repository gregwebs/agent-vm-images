#!/usr/bin/env python3
"""Checked, single-site mutations of a vendored installer copy for the suites.

The strict-egress and vendored-installer suites prove that a *plausible defect*
fails its oracle. Both mutations here are scratch-only and self-verifying: each
asserts exactly one injection site and that the copy really differs, so a no-op
substitution cannot masquerade as a passing control.

usage: vendored-installers-mutate-vendor.py SOURCE DESTINATION VARIANT
       variants: latest-request, substitute-before-digest
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


def mutate(source: Path, destination: Path, variant: str) -> None:
    text = source.read_text()
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
