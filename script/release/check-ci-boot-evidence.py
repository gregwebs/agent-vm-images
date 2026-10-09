#!/usr/bin/env python3
"""Fail-closed retention check for amd64 CI; promotion still requires both arches."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True
from pathlib import Path
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    import content


def validate_amd64(release: content.ReleaseMetadata, release_path: Path, original_assets: Path,
                   verification_dir: Path) -> None:
    # These imports resolve from the authenticated signed-source tree in main.
    import content
    from evidence import MANDATORY_CHECKS

    originals = list(original_assets.glob('verify-amd64-*/verification-amd64.json'))
    staged = verification_dir / 'verification-amd64.json'
    if len(originals) != 1 or not originals[0].is_file() or not staged.is_file():
        raise ValueError('exactly one original and a regular staged success record required')
    if content.hash_file(originals[0]) != content.hash_file(staged):
        raise ValueError('original/staged success record hash or size mismatch')
    record = content.read_json(staged)
    content.object_keys(record, {'schema_version', 'architecture', 'release_subject', 'platform',
                                'index_digest', 'host', 'tools', 'uid_gid_pairs', 'cold_cache', 'checks', 'logs'})
    if (record['schema_version'] != 1 or record['architecture'] != 'amd64'
            or record['release_subject'] != content.hash_file(release_path)[0].value
            or record['platform'] != release.platform('amd64').json()
            or record['index_digest'] != release.index_digest.value
            or record['cold_cache'] is not True):
        raise ValueError('evidence release/platform/cold-cache binding mismatch')
    if not isinstance(record['host'], str) or not record['host']:
        raise ValueError('missing native host identity')
    tools = record['tools']
    required_tools = {'docker', 'buildx', 'skopeo', 'msb', 'firmware', 'libkrun'}
    if not isinstance(tools, dict) or not required_tools <= set(tools):
        raise ValueError('missing runtime/tool evidence')
    for tool in required_tools:
        if not isinstance(tools[tool], dict) or not tools[tool].get('version') or not tools[tool].get('sha256'):
            raise ValueError('missing tool version/binary hash')
    pairs = record['uid_gid_pairs']
    if not isinstance(pairs, list) or len(set(pairs)) < 3 or '12345:23456' not in pairs:
        raise ValueError('missing native numeric UID/GID cases')
    checks = record['checks']
    if not isinstance(checks, dict) or set(checks) != MANDATORY_CHECKS:
        raise ValueError('missing/extra mandatory native checks')
    if not isinstance(record['logs'], list) or not record['logs']:
        raise ValueError('missing evidence log inventory')
    logs = tuple(content.ArchivePart.parse(item) for item in record['logs'])
    if len({part.name for part in logs}) != len(logs):
        raise ValueError('duplicate evidence logs')
    names = {part.name for part in logs}
    for name, check in checks.items():
        content.object_keys(check, {'status', 'duration_seconds', 'logs'})
        if (type(check['status']) is not int or check['status'] != 0
                or not isinstance(check['duration_seconds'], (int, float))
                or not check['duration_seconds'] >= 0 or not check['logs']
                or not set(check['logs']) <= names):
            raise ValueError(f'incomplete native check: {name}')
    for part in logs:
        part.verify(verification_dir)


def main() -> None:
    if len(sys.argv) != 6:
        raise ValueError('usage: check-ci-boot-evidence.py SOURCE_ROOT ORIGINAL_ASSETS EVIDENCE_DIR VERSION SOURCE_SHA')
    source, assets, evidence, version, sha = sys.argv[1:]
    sys.path.insert(0, str(Path(source) / 'script/release'))
    import operations
    release_path = Path(evidence) / 'release/release.json'
    release = operations.authenticate_metadata(release_path.parent, version)
    if release.source_sha != sha:
        raise ValueError('staged metadata signed source mismatch')
    validate_amd64(release, release_path, Path(assets), Path(evidence) / 'verification')
    print('staged amd64 success record and every inventoried log verified')


if __name__ == '__main__':
    main()
