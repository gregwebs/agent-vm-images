#!/usr/bin/env python3
"""TEST-ONLY stand-in for the reviewed host verifier, executed as `cosign`.

`script/test/codex-pins.py` proves two things about the writer's verifier seam:
the exact argv it constructs, and the exact environment it hands the child. Both
are observed from inside the child, which is why this is a real executable rather
than a string in the test file.

  cosign version --json   prints {"gitVersion": "<version>"}
  cosign verify-blob ...  appends one JSON line to $HOME/observations.jsonl

The child's environment is the writer's own constructed one, so nothing can be
passed to it through the parent: the observation lands in the private HOME the
writer created for it, and the reported version is the reviewed one unless a
`cosign-version` file sits beside this executable (which is how a test asks for
an incompatible verifier without a second copy of this program).
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

OBSERVED = ('PATH', 'HOME', 'XDG_CACHE_HOME', 'LC_ALL')
INHERITED = ('SIGSTORE_', 'COSIGN_', 'SSL_', 'TUF_')
TOKENS = ('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY', 'GH_TOKEN',
          'GITHUB_TOKEN', 'GNUPGHOME', 'SSL_CERT_FILE', 'SSL_CERT_DIR')
DEFAULT_VERSION = 'v2.4.1'


def reported_version() -> str:
    beside = Path(os.path.realpath(sys.argv[0])).with_name('cosign-version')
    if beside.is_file():
        return beside.read_text(encoding='utf-8').strip()
    return DEFAULT_VERSION


def main(argv: list[str]) -> int:
    if argv[:1] == ['version']:
        print(json.dumps({'gitVersion': reported_version()}))
        return 0
    home = os.environ.get('HOME')
    if not home:
        print('HOME is required', file=sys.stderr)
        return 2
    extra = sorted(name for name in os.environ if name not in OBSERVED)
    observation = {'argv': argv,
                   'env': sorted(name for name in os.environ if name in OBSERVED),
                   'extra': extra,
                   'inherited_trust': [name for name in extra
                                       if name.startswith(INHERITED) or name in TOKENS]}
    with open(Path(home) / 'observations.jsonl', 'a', encoding='utf-8') as handle:
        handle.write(json.dumps(observation) + '\n')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
