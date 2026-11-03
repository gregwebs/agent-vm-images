#!/usr/bin/env python3
"""Decide a `kache doctor --probe --json` report.

`kache doctor` reports findings and still exits 0, so its exit status alone
proves nothing: a broken store, wrapper or probe is a line of output. This reads
the versioned JSON document instead and refuses a report that says `success` is
false, that a non-optional check failed, or that the compile/store/restore probe
did not pass. Optional findings (a daemon not yet started, shims not installed)
are expected in the image and are not failures.

Usage: kache_probe.py REPORT
"""

import json
import sys

PROBE_LABEL = "Cache probe"


def fail(message: str) -> int:
    print(f"verify-toolchain: {message}", file=sys.stderr)
    return 1


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        return fail("usage: kache_probe.py REPORT")
    with open(argv[1], encoding="utf-8") as handle:
        report = json.load(handle)
    if not isinstance(report, dict):
        return fail("the kache doctor report is not a JSON object")
    checks = report.get("checks")
    if not isinstance(checks, list):
        return fail("the kache doctor report carries no checks")
    status = 0
    if not report.get("success"):
        status = fail("kache doctor reported failure")
    for check in checks:
        if check.get("pass") or check.get("optional"):
            continue
        print(
            f"verify-toolchain: kache doctor failed: {check.get('label')}: {check.get('detail')}",
            file=sys.stderr,
        )
        status = 1
    probes = [check for check in checks if check.get("label") == PROBE_LABEL]
    if len(probes) != 1:
        status = fail(f"expected exactly one {PROBE_LABEL} check, found {len(probes)}")
    elif not probes[0].get("pass"):
        status = fail(f"the kache compile/store/restore probe did not pass: {probes[0].get('detail')}")
    return status


if __name__ == "__main__":
    sys.exit(main(sys.argv))
