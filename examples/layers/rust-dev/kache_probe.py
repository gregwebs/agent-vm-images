#!/usr/bin/env python3
"""Decide a `kache doctor --probe --json` report.

`kache doctor` reports findings and still exits 0, so its exit status alone
proves nothing: a broken store, wrapper or probe is a line of output. This reads
the versioned JSON document instead and refuses a report that says `success` is
false, that a non-optional check failed, that the compile/store/restore probe did
not pass, that the C/C++ shims are not active, or that the store sits on storage
its index cannot live on. A daemon that has not been started yet is the one
finding the image does not install for itself, so it stays optional.

Usage: kache_probe.py REPORT
"""

import json
import sys

PROBE_LABEL = "Cache probe"
SHIMS_LABEL = "C/C++ shims"
STORE_FS_LABEL = "Cache FS"


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
    # `doctor` grades the shims as optional because a Rust-only setup may skip
    # them, but this image installs them and puts them first on PATH, so a report
    # that says otherwise means the shims stopped being active.
    shims = [check for check in checks if check.get("label") == SHIMS_LABEL]
    if len(shims) != 1:
        status = fail(f"expected exactly one {SHIMS_LABEL} check, found {len(shims)}")
    elif not shims[0].get("pass"):
        status = fail(f"the C/C++ shims are not active: {shims[0].get('detail')}")
    # The store is a WAL-mode SQLite index, so a report that places it on a
    # network or FUSE mount is describing the corruption case. `doctor` already
    # grades this check non-optional, so the loop above fails it too; naming it
    # here keeps the reason readable and keeps the requirement if that grading
    # ever changes.
    store_fs = [check for check in checks if check.get("label") == STORE_FS_LABEL]
    if len(store_fs) != 1:
        status = fail(f"expected exactly one {STORE_FS_LABEL} check, found {len(store_fs)}")
    elif not store_fs[0].get("pass"):
        status = fail(
            f"the cache store is on storage its index cannot live on: {store_fs[0].get('detail')}"
        )
    return status


if __name__ == "__main__":
    sys.exit(main(sys.argv))
