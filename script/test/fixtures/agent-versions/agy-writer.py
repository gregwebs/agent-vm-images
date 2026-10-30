#!/usr/bin/env python3
"""TEST-ONLY adapter for the agy owner writer, used by the hermetic suites.

`script/dev/pin-agy.py` fetches from GitHub and Google's manifest host in
production. This adapter loads the REAL writer module, replaces exactly the
streaming `Fetcher` with fixture files and, when asked, installs publication
faults into the shared transaction. Everything else (manifest parsing, the
two-channel corroboration, archive preflight, the fixed destination list, the
shared Python publication transaction and every status code) is production code.

It deliberately does not import `artifact_pin` or `install_artifact` and does not
touch `sys.path`: the writer's own `bind_models` must be what binds them, or the
clean-process case in `script/test/pin-agy-maintenance.py` proves nothing.

Environment:
  FIXTURE_AGY_WRITER        absolute path to the real script/dev/pin-agy.py
  FIXTURE_AGY_ASSETS        directory holding release.json, manifest-linux-*.json
                            and the two agy_cli_linux_*.tar.gz assets
  FIXTURE_AGY_FAIL_URL      answer this exact URL with an HTTP error
  FIXTURE_AGY_INJECT        "partial" | "signal" | "none"
  FIXTURE_AGY_INJECT_AT     destination file NAME to fail publishing
                            (e.g. manifest-linux-amd64.json)
  FIXTURE_AGY_REFUSE_RESTORE  "1" to refuse every restore
  FIXTURE_AGY_RECORD_ENOSPC   "1" to fail writing recovery.json with ENOSPC
  FIXTURE_AGY_RECORD_FETCHES  "1" to append every requested URL to
                            $FIXTURE_AGY_ASSETS/fetches.log
  FIXTURE_AGY_TAMPER_EVIDENCE "release" to render a pin whose release-evidence
                            sha256 is wrong, so only the writer's own validation
                            of the staged proposal can refuse it

Everything below the module load runs only when this file IS the process. It is
imported as `script/dev/pin-agy.py`'s transport by a fixture repository, exactly
as the real `pin-codex.py` is, so an import must never parse arguments or install
faults.
"""
from __future__ import annotations

import errno
import importlib.util
import json
import os
import shutil
import signal
import sys
from pathlib import Path

WRITER = Path(os.environ['FIXTURE_AGY_WRITER'])
# `--check` needs no upstream bytes at all, so the served directory is
# optional and any fetch without it is a refusal.
ASSETS = Path(os.environ.get('FIXTURE_AGY_ASSETS', ''))

spec = importlib.util.spec_from_file_location('pin_agy_under_test', WRITER)
module = importlib.util.module_from_spec(spec)
# Dataclasses resolve their defining module through sys.modules, which a bare
# module_from_spec does not populate (Python 3.12 raises there).
sys.modules['pin_agy_under_test'] = module
try:
    spec.loader.exec_module(module)
except BaseException:
    sys.modules.pop('pin_agy_under_test', None)
    raise


def fixture_file(url: str) -> Path:
    if '/releases/tags/' in url:
        return ASSETS / 'release.json'
    return ASSETS / url.rsplit('/', 1)[-1]


def fixture_fetch(url: str, output: Path, limit: int) -> None:
    if not str(ASSETS):
        raise module.Refusal(f'cannot fetch {url}: no fixture assets are configured')
    if os.environ.get('FIXTURE_AGY_RECORD_FETCHES') == '1':
        with (ASSETS / 'fetches.log').open('a') as log:
            log.write(f'{url}\n')
    if url == os.environ.get('FIXTURE_AGY_FAIL_URL'):
        raise module.Refusal(f'cannot fetch {url}: HTTP 500 (fixture)')
    source = fixture_file(url)
    if not source.is_file():
        raise module.Refusal(f'cannot fetch {url}: HTTP 404 (fixture)')
    raw = source.read_bytes()
    if len(raw) > limit:
        raise module.Refusal(f'{url} is larger than the {limit} byte ceiling')
    Path(output).write_bytes(raw)


module.https_fetch = fixture_fetch


def repo_root_from_argv() -> Path:
    """The writer's own --repo-root, so the fault-injected publisher is the one
    the writer will actually use."""
    argv = sys.argv[1:]
    if '--repo-root' in argv:
        return Path(argv[argv.index('--repo-root') + 1])
    return WRITER.resolve().parents[2]


injection = os.environ.get('FIXTURE_AGY_INJECT', 'none')
if __name__ == '__main__' and injection != 'none':
    _model, _installer, publisher, _transport = module.bind_models(repo_root_from_argv())
    real_copyfile = shutil.copyfile
    armed = [True]
    target_name = os.environ.get('FIXTURE_AGY_INJECT_AT', '')

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

    if os.environ.get('FIXTURE_AGY_REFUSE_RESTORE') == '1':
        def refuse_restore(_self):
            raise OSError('injected restore failure')
        publisher.Publisher.restore = refuse_restore

    if os.environ.get('FIXTURE_AGY_RECORD_ENOSPC') == '1':
        def enospc_record(_self, _error):
            raise OSError(errno.ENOSPC, 'no space left on device')
        publisher.Publisher.recovery_record = enospc_record

if os.environ.get('FIXTURE_AGY_TAMPER_EVIDENCE') == 'release':
    # A proposal whose own pin records the wrong evidence hash parses and has the
    # right archive digests; the only check that can refuse it is the writer's
    # validation of the bytes it is about to publish.
    real_render_pin = module.render_pin

    def render_pin(model, version, release, archives, manifest_bytes, verified_on):
        document = json.loads(real_render_pin(model, version, release, archives,
                                              manifest_bytes, verified_on))
        document['authentication']['release']['sha256'] = '0' * 64
        return json.dumps(document, indent=2) + '\n'

    module.render_pin = render_pin

if __name__ == '__main__':
    sys.exit(module.main(sys.argv[1:]))
