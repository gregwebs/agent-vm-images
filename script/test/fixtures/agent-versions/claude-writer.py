#!/usr/bin/env python3
"""TEST-ONLY adapter for the Claude owner writer, used by the hermetic suites.

`script/dev/pin-claude.py` fetches from downloads.claude.ai in production. This
adapter loads the REAL writer module, replaces exactly the streaming `Fetcher`
with fixture files, points the reviewed-signer constant at the fixture key and,
when asked, installs publication faults into the shared transaction. Everything
else (URL derivation, key-primary policy, signature verification, ELF preflight,
proposal validation, the payload delta, the shared publication transaction and
every status code) is production code.

It deliberately does not touch `sys.path`: the writer's own `bind_models` must be
what binds the model, the publisher and the transport, or the fixture repository's
copies are not the code under test.

Environment:
  FIXTURE_CLAUDE_WRITER        absolute path to the real script/dev/pin-claude.py
  FIXTURE_CLAUDE_ASSETS        directory holding the served files; a file is named
                               after the last two URL path segments joined by '-'
  FIXTURE_CLAUDE_FINGERPRINT   the fixture key's primary, installed as the reviewed
                               signer constant; unset leaves the production one
  FIXTURE_CLAUDE_FAIL_URL      answer this exact URL with an HTTP error
  FIXTURE_CLAUDE_INJECT        "partial" | "signal" | "none"
  FIXTURE_CLAUDE_INJECT_AT     destination file NAME to fail publishing
  FIXTURE_CLAUDE_REFUSE_RESTORE  "1" to refuse every restore
  FIXTURE_CLAUDE_RECORD_FETCHES  "1" to append every requested URL to
                               $FIXTURE_CLAUDE_ASSETS/fetches.log
  FIXTURE_CLAUDE_TAMPER        "manifest" to render a proposal pin whose manifest
                               sha256 is wrong, so only the writer's own validation
                               of the staged proposal can refuse it

Everything below the module load runs only when this file IS the process. It is
imported as a fixture repository's `script/dev/pin-claude.py`, so an import must
never parse arguments or install faults.
"""
from __future__ import annotations

import importlib.util
import json
import os
import shutil
import signal
import sys
from pathlib import Path

WRITER = Path(os.environ['FIXTURE_CLAUDE_WRITER'])
# `--check` needs no upstream bytes at all, so the served directory is optional
# and any fetch without it is a refusal.
ASSETS = Path(os.environ.get('FIXTURE_CLAUDE_ASSETS', ''))

spec = importlib.util.spec_from_file_location('pin_claude_under_test', WRITER)
module = importlib.util.module_from_spec(spec)
# Dataclasses resolve their defining module through sys.modules, which a bare
# module_from_spec does not populate (Python 3.12 raises there).
sys.modules['pin_claude_under_test'] = module
try:
    spec.loader.exec_module(module)
except BaseException:
    sys.modules.pop('pin_claude_under_test', None)
    raise


def fixture_file(url: str) -> Path:
    return ASSETS / '-'.join(url.rsplit('/', 2)[-2:])


def fixture_fetch(url: str, output: Path, limit: int) -> None:
    if not str(ASSETS):
        raise module.Refusal(f'cannot fetch {url}: no fixture assets are configured')
    if os.environ.get('FIXTURE_CLAUDE_RECORD_FETCHES') == '1':
        with (ASSETS / 'fetches.log').open('a') as log:
            log.write(f'{url}\n')
    if url == os.environ.get('FIXTURE_CLAUDE_FAIL_URL'):
        raise module.Refusal(f'cannot fetch {url}: HTTP 500 (fixture)')
    source = fixture_file(url)
    if not source.is_file():
        raise module.Refusal(f'cannot fetch {url}: HTTP 404 (fixture)')
    raw = source.read_bytes()
    if len(raw) > limit:
        raise module.Refusal(f'{url} is larger than the {limit} byte ceiling')
    Path(output).write_bytes(raw)


module.https_fetch = fixture_fetch


_real_bind_models = module.bind_models


def bind_models(repo_root):
    """Bind the real modules, then aim the reviewed-signer policy at the fixture."""
    model, publisher, transport = _real_bind_models(repo_root)
    fingerprint = os.environ.get('FIXTURE_CLAUDE_FINGERPRINT')
    if fingerprint:
        model.REVIEWED_SIGNER_FINGERPRINT = fingerprint.upper()
    return model, publisher, transport


module.bind_models = bind_models


def repo_root_from_argv() -> Path:
    """The writer's own --repo-root, so the fault-injected publisher is the one
    the writer will actually use."""
    argv = sys.argv[1:]
    if '--repo-root' in argv:
        return Path(argv[argv.index('--repo-root') + 1])
    return WRITER.resolve().parents[2]


injection = os.environ.get('FIXTURE_CLAUDE_INJECT', 'none')
if __name__ == '__main__' and injection != 'none':
    _model, publisher, _transport = module.bind_models(repo_root_from_argv())
    real_copyfile = shutil.copyfile
    armed = [True]
    target_name = os.environ.get('FIXTURE_CLAUDE_INJECT_AT', '')

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

    if os.environ.get('FIXTURE_CLAUDE_REFUSE_RESTORE') == '1':
        def refuse_restore(_self):
            raise OSError('injected restore failure')
        publisher.Publisher.restore = refuse_restore

if os.environ.get('FIXTURE_CLAUDE_TAMPER') == 'manifest':
    # A proposal whose own pin records the wrong manifest hash still parses and has
    # the right executable digests; the only check that can refuse it is the
    # writer's validation of the bytes it is about to publish.
    real_render_pin = module.render_pin

    def render_pin(model, version, manifest, signature, key_bytes, executables,
                   verified_on):
        document = json.loads(real_render_pin(model, version, manifest, signature,
                                              key_bytes, executables, verified_on))
        document['manifest']['sha256'] = '0' * 64
        return json.dumps(document, indent=2) + '\n'

    module.render_pin = render_pin

if __name__ == '__main__':
    sys.exit(module.main(sys.argv[1:]))
