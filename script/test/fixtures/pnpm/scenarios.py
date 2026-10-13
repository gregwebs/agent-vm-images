"""Validated registry input, independent of resolver implementation."""
from dataclasses import dataclass
import json
from pathlib import Path
import re


@dataclass(frozen=True)
class Version:
    version: str
    age_hours: int | None
    dependency: bool
    provenance: bool
    corrupt: bool


@dataclass(frozen=True)
class Package:
    name: str
    versions: tuple[Version, ...]


def read_scenarios(path: Path) -> tuple[Package, ...]:
    raw = json.loads(path.read_text())
    if not isinstance(raw, list) or not raw:
        raise ValueError('scenario list required')
    result = []
    names = set()
    for record in raw:
        if not isinstance(record, dict) or set(record) != {'name', 'versions'}:
            raise ValueError('invalid package fields')
        name = record['name']
        if not isinstance(name, str) or not re.fullmatch(r'(?:@[a-z-]+/)?[a-z-]+', name) or name in names:
            raise ValueError('invalid/duplicate package name')
        names.add(name)
        if not isinstance(record['versions'], list) or not record['versions']:
            raise ValueError('versions required')
        versions = []
        seen = set()
        for selection in record['versions']:
            if not isinstance(selection, dict) or not {'version', 'ageHours'} <= selection.keys() or selection.keys() - {'version', 'ageHours', 'dependency', 'provenance', 'corrupt'}:
                raise ValueError('invalid version fields')
            version = selection['version']
            age = selection['ageHours']
            if not isinstance(version, str) or not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+', version) or version in seen:
                raise ValueError('invalid/duplicate version')
            seen.add(version)
            if age is not None and (type(age) is not int or age < 0):
                raise ValueError('invalid publication age')
            flags = [selection.get(key, False) for key in ('dependency', 'provenance', 'corrupt')]
            if any(type(flag) is not bool for flag in flags):
                raise ValueError('invalid scenario flag')
            versions.append(Version(version, age, *flags))
        result.append(Package(name, tuple(versions)))
    return tuple(result)
