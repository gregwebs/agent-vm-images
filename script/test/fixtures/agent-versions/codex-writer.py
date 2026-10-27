#!/usr/bin/env python3
"""TEST-ONLY adapter for the owner writer, used by script/test/agent-versions.sh.

`script/build/agent-versions.sh` calls `script/dev/pin-codex.py` as a subprocess
in production. That subprocess would reach GitHub and the real cosign, so the
owner suite swaps in this adapter: it loads the REAL writer module and replaces
exactly two seams -- the streaming `Fetcher` and the `BlobVerifier` -- with
fixtures read from a directory. Everything else (schema validation, evidence
hashing, archive preflight, the fixed destination list, the shared Python
publication transaction and every status code) is production code.

The publication faults the owner's rollback contract is proved with are installed
here too, because the transaction runs inside this process: a half-written
destination, a refusing restore and an unwritable recovery record. Each is opt-in
through an environment variable and none of them exists in production.

Environment:
  FIXTURE_CODEX_WRITER     absolute path to the real script/dev/pin-codex.py
  FIXTURE_CODEX_ASSETS     directory holding release.json, codex-package_SHA256SUMS,
                           codex-package-<target>.tar.gz and its .sigstore
  FIXTURE_CODEX_FAIL_URL   answer this exact URL with an HTTP error
  FIXTURE_CODEX_VERIFY     "fail" to refuse the signature check
  FIXTURE_CODEX_INJECT     "partial" | "signal" | "none"
  FIXTURE_CODEX_INJECT_AT  destination file NAME to fail publishing (e.g. release.json)
  FIXTURE_CODEX_REFUSE_RESTORE  "1" to refuse every restore
  FIXTURE_CODEX_RECORD_ENOSPC   "1" to fail writing recovery.json with ENOSPC
"""
from __future__ import annotations

import errno
import importlib.util
import os
import shutil
import signal
import sys
from pathlib import Path

WRITER = Path(os.environ['FIXTURE_CODEX_WRITER'])
ASSETS = Path(os.environ['FIXTURE_CODEX_ASSETS'])

spec = importlib.util.spec_from_file_location('pin_codex_under_test', WRITER)
module = importlib.util.module_from_spec(spec)
# Dataclasses resolve their defining module through sys.modules, which a bare
# module_from_spec does not populate (Python 3.12 raises there).
sys.modules['pin_codex_under_test'] = module
try:
    spec.loader.exec_module(module)
except BaseException:
    sys.modules.pop('pin_codex_under_test', None)
    raise


def fixture_file(url: str) -> Path:
    if '/releases/tags/' in url:
        return ASSETS / 'release.json'
    return ASSETS / url.rsplit('/', 1)[-1]


def fixture_fetch(url: str, output: Path, limit: int) -> None:
    if url == os.environ.get('FIXTURE_CODEX_FAIL_URL'):
        raise module.Refusal(f'cannot fetch {url}: HTTP 500 (fixture)')
    source = fixture_file(url)
    if not source.is_file():
        raise module.Refusal(f'cannot fetch {url}: HTTP 404 (fixture)')
    raw = source.read_bytes()
    if len(raw) > limit:
        raise module.Refusal(f'{url} is larger than the {limit} byte ceiling')
    Path(output).write_bytes(raw)


def fixture_verify(cosign):
    def verify(archive, bundle, identity, issuer, work):
        if os.environ.get('FIXTURE_CODEX_VERIFY') == 'fail':
            raise module.Refusal(f'{Path(archive).name}: cosign refused (fixture)')
    return verify


module.https_fetch = fixture_fetch
module.cosign_blob_verifier = fixture_verify

def repo_root_from_argv() -> Path:
    """The writer's own --repo-root, so the fault-injected publisher is the one
    `publish_stage` will actually use."""
    argv = sys.argv[1:]
    if '--repo-root' in argv:
        return Path(argv[argv.index('--repo-root') + 1])
    return WRITER.resolve().parents[2]


injection = os.environ.get('FIXTURE_CODEX_INJECT', 'none')
if injection != 'none':
    publisher = module.publisher_module(repo_root_from_argv())
    real_copyfile = shutil.copyfile
    armed = [True]
    target_name = os.environ.get('FIXTURE_CODEX_INJECT_AT', '')

    def copyfile(source, destination, **kwargs):
        source_path, destination_path = Path(source), Path(destination)
        if (armed[0] and destination_path.name == target_name
                and 'staged' in source_path.parts):
            armed[0] = False
            destination_path.write_text('half-written\n')
            if injection == 'signal':
                os.kill(os.getpid(), signal.SIGINT)
                raise OSError('unreachable after the signal')
            raise OSError('injected write failure')
        return real_copyfile(source, destination, **kwargs)

    publisher.shutil.copyfile = copyfile

    if os.environ.get('FIXTURE_CODEX_REFUSE_RESTORE') == '1':
        def refuse_restore(_self):
            raise OSError('injected restore failure')
        publisher.Publisher.restore = refuse_restore

    if os.environ.get('FIXTURE_CODEX_RECORD_ENOSPC') == '1':
        def enospc_record(_self, _error):
            raise OSError(errno.ENOSPC, 'no space left on device')
        publisher.Publisher.recovery_record = enospc_record

sys.exit(module.main(sys.argv[1:]))
