#!/usr/bin/env python3
"""Hermetic reproduction of the issue #52 Phase 0 selected-shrinkwrap vectors.

The fixture is data only: an authenticated 0.87.0 npm-shrinkwrap.json, the
npm-shaped project lock synthesized from it, the pnpm v9 graph the real import
emitted (captured as syntax-only JSON), and the reviewed aggregate comparison.
`compare.py` is the same independent stdlib comparator the feasibility gate ran;
it is an evidence tool, not production code, and the C3 converter must not import
it. Every run copies the fixture into a scratch directory because the comparator
writes its report beside itself.

The five mutants recorded in mutants.json are the ones the gate demonstrated.
Each is applied to the committed graph in memory; the comparator must still exit
0 and report exactly the counter the mutant claims to move. A fixture that no
longer kills a mutant is a review stop, not a stale-vector warning.
"""
from __future__ import annotations

import json
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

FIXTURE = Path(__file__).resolve().parent / "fixtures" / "pi-shrinkwrap-0.87.0"
DATA = [
    "compare.py",
    "synthesized-package-lock.json",
    "authenticated-shrinkwrap.json",
    "resolutions.json",
]
GRAPH = Path("result") / "parse-yaml.stdout"
ROOT = "@earendil-works/pi-coding-agent@0.87.0"
FAIL = []


def fail(message: str) -> None:
    FAIL.append(message)


def check(condition: bool, message: str) -> None:
    if not condition:
        fail(message)


def load(path: Path):
    return json.loads(path.read_text())


def copy_fixture(destination: Path, graph=None) -> None:
    for name in DATA:
        shutil.copyfile(FIXTURE / name, destination / name)
    (destination / "result").mkdir()
    source = graph if graph is not None else (FIXTURE / GRAPH).read_text()
    (destination / GRAPH).write_text(source if isinstance(source, str) else json.dumps(source))


def run_comparator(directory: Path):
    result = subprocess.run(
        [sys.executable, "-B", "compare.py"],
        cwd=directory,
        capture_output=True,
        text=True,
        timeout=120,
    )
    if result.returncode != 0:
        fail(f"comparator status {result.returncode}: {result.stderr.strip()}")
        return None
    return load(directory / "comparison.json")["counts"]


def lookup(counts, counter: str):
    value = counts
    for part in counter.split("."):
        if not isinstance(value, dict) or part not in value:
            return None
        value = value[part]
    return value


def apply_edit(graph, edit) -> None:
    path = edit["path"]
    value = graph
    for part in path[:-1]:
        if part not in value:
            raise SystemExit(f"mutant path does not exist: {'/'.join(path)}")
        value = value[part]
    value[path[-1]] = edit["value"]


def check_shape() -> None:
    entries = sorted(entry.name for entry in FIXTURE.iterdir())
    # `archive-manifest.json` is the selected archive's own package.json, added
    # with the C3 converter so synthesis can be re-run from authenticated inputs.
    check(entries == sorted(DATA + ["README.md", "archive-manifest.json",
                                    "expected-delta.json", "mutants.json", "result"]),
          f"fixture carries unexpected entries: {entries}")
    check(not (FIXTURE / "pnpm-lock.yaml").exists(), "fixture must not commit a 0.87.0 pnpm lock")
    archive_manifest = load(FIXTURE / "archive-manifest.json")
    check(archive_manifest["name"] == "@earendil-works/pi-coding-agent", "archive manifest name")
    check(archive_manifest["version"] == "0.87.0", "archive manifest version")
    shrinkwrap = load(FIXTURE / "authenticated-shrinkwrap.json")
    check(shrinkwrap["lockfileVersion"] == 3, "shrinkwrap must be lockfileVersion 3")
    packages = shrinkwrap["packages"]
    check(packages[""]["name"] == "@earendil-works/pi-coding-agent", "shrinkwrap root name")
    check(packages[""]["version"] == "0.87.0", "shrinkwrap root version")
    resolutions = load(FIXTURE / "resolutions.json")
    check(resolutions[0]["name"] == "@earendil-works/pi-coding-agent", "resolutions root first")
    root = next(entry for entry in resolutions if entry["name"] == "@earendil-works/pi-coding-agent")
    expected = load(FIXTURE / "expected-delta.json")["provenance"]["root"]
    check(root["version"] == expected["version"] and root["integrity"] == expected["integrity"],
          "root resolution disagrees with the reviewed provenance")
    missing = sorted(
        location.split("node_modules/")[-1]
        for location, record in packages.items()
        if location and not record.get("integrity")
    )
    supplements = sorted(entry["name"] for entry in resolutions[1:])
    check(missing == supplements and len(supplements) == 5,
          f"missing-integrity records must be exactly the five legacy siblings: {missing}")
    synthesized = load(FIXTURE / "synthesized-package-lock.json")
    check(synthesized["lockfileVersion"] == 3, "synthesized npm lock must be lockfileVersion 3")
    check(len(synthesized["packages"]) == 145, "synthesized lock keeps the importer plus 144 occurrences")
    graph = load(FIXTURE / GRAPH)
    check(str(graph["lockfileVersion"]) == "9.0", "graph capture must be a v9 lock")
    check(len(graph["packages"]) == 143 and len(graph["snapshots"]) == 143,
          "graph capture must carry 143 archives and 143 snapshots")
    reviewed = load(FIXTURE / "expected-delta.json")["reviewedNormalizations"]
    kinds = sorted(entry["kind"] for entry in reviewed)
    check(kinds == ["explicit-optional-peer-edge", "occurrence-dedup", "peer-declaration-completion"],
          f"reviewed normalizations must be exactly the three approved ones: {kinds}")


def main() -> int:
    check_shape()
    expected = load(FIXTURE / "expected-delta.json")["counts"]
    with tempfile.TemporaryDirectory(prefix="pi-shrinkwrap-") as scratch:
        baseline = Path(scratch) / "baseline"
        baseline.mkdir()
        copy_fixture(baseline)
        counts = run_comparator(baseline)
        if counts is not None:
            check(counts == expected, f"baseline counts differ: {counts!r}")
            check(counts["npmArchiveOccurrencesIncludingAuthenticatedRoot"] == 144
                  and counts["npmUniqueArchiveIdentities"] == 143,
                  "baseline must preserve 144 occurrences of 143 identities")
            check(counts["npmMandatoryEdges"] == 248 and counts["npmOptionalEdges"] == 26
                  and counts["pnpmMandatoryEdges"] == 248 and counts["pnpmOptionalEdges"] == 27,
                  "baseline edge counts")
            check(counts["rootedIdentityGuarded"]["sriEqualPairs"] == 144
                  and counts["rootedIdentityGuarded"]["sriRawEqualPairs"] == 144,
                  "baseline SRI equality")
            check(counts["rootedTargetDivergenceFrontiers"] == 0
                  and counts["npmWithoutRootedMapping"] == 0
                  and counts["pnpmWithoutRootedMapping"] == 0,
                  "baseline rooted mapping")
        for index, mutant in enumerate(load(FIXTURE / "mutants.json")):
            directory = Path(scratch) / f"mutant-{index}"
            directory.mkdir()
            graph = load(FIXTURE / GRAPH)
            for edit in mutant["edits"]:
                apply_edit(graph, edit)
            copy_fixture(directory, graph)
            mutated = run_comparator(directory)
            if mutated is None:
                continue
            for expected_counter in mutant["expect"]:
                actual = lookup(mutated, expected_counter["counter"])
                check(actual == expected_counter["value"],
                      f"mutant {mutant['mutant']}: {expected_counter['counter']} = {actual!r}, "
                      f"expected {expected_counter['value']!r}")
    for message in FAIL:
        print(f"FAIL: {message}", file=sys.stderr)
    if FAIL:
        return 1
    print("pi-shrinkwrap-vectors: 0.87.0 identity/occurrence/SRI/edge/optional/peer vectors and five mutants PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
