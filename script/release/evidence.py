"""Hash-bound native evidence validation; not a substitute for maintainer review."""

import sys
sys.dont_write_bytecode = True
from pathlib import Path
from content import ArchivePart, ReleaseMetadata, hash_file, object_keys, read_json

MANDATORY_CHECKS = frozenset({
    'metadata-signature', 'platform-signatures', 'public-assets', 'anonymous-index',
    'anonymous-platform', 'registry-archive-graph', 'docker-artifact',
    'msb-registry-inspect', 'msb-archive-inspect', 'msb-registry-boot', 'msb-archive-boot',
    'corrupt-archive', 'missing-asset', 'wrong-architecture', 'fixture-corrupt-blob',
    'fixture-corrupt-archive', 'fixture-wrong-architecture', 'runtime-doctor',
})


def check_evidence(release_path: Path, evidence_dir: Path) -> None:
    release = ReleaseMetadata.parse(read_json(release_path))
    subject = hash_file(release_path)[0].value
    for arch in ('amd64', 'arm64'):
        record = read_json(evidence_dir / f'verification-{arch}.json')
        object_keys(record, {'schema_version', 'architecture', 'release_subject', 'platform',
                            'index_digest', 'host', 'tools', 'uid_gid_pairs', 'cold_cache', 'checks', 'logs'})
        if (record['schema_version'] != 1 or record['architecture'] != arch
                or record['release_subject'] != subject
                or record['platform'] != release.platform(arch).json()
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
        logs = tuple(ArchivePart.parse(x) for x in record['logs'])
        if not logs or len({x.name for x in logs}) != len(logs):
            raise ValueError('missing/duplicate evidence logs')
        names = {x.name for x in logs}
        for name, check in checks.items():
            object_keys(check, {'status', 'duration_seconds', 'logs'})
            if (type(check['status']) is not int or check['status'] != 0
                    or not isinstance(check['duration_seconds'], (int, float))
                    or check['duration_seconds'] < 0 or not check['logs']
                    or not set(check['logs']) <= names):
                raise ValueError(f'incomplete native check: {name}')
        for log in logs:
            log.verify(evidence_dir)
