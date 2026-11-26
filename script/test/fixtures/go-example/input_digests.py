#!/usr/bin/env python3
"""Print the digest of every input the go-dev gopls steps build from.

The native harness records these in its log so acceptance evidence binds the
working-copy bytes, not just the source commit and a count of dirty tracked
files. Kept as a sibling script rather than an inline heredoc: shell sources in
this repository may not embed Python (script/test/contracts.sh enforces it).

Usage: input_digests.py ROOT EXAMPLE_RELATIVE
Prints one `input <sha256>  <path>` line per input, then
`inputs-set <sha256>` over the newline-joined lines, in sorted path order.
"""
from __future__ import annotations

import hashlib
import pathlib
import sys


def main() -> int:
    if len(sys.argv) != 3:
        print('usage: input_digests.py ROOT EXAMPLE_RELATIVE', file=sys.stderr)
        return 2
    root = pathlib.Path(sys.argv[1])
    layer = root / sys.argv[2]
    # Exactly the inputs the Dockerfile binds for the gopls steps, in the two sets
    # the recipe uses: the final stage's mounts (recipe, three adapters, the Python
    # reader, the five committed closure files) and the builder stage's bind
    # (`helper`, the whole module including its vendor tree, which is what the
    # stage compiles). The helper tree is walked rather than listed so a new source
    # file cannot be built without being digested; the count is floored by the
    # self-test control, which is what makes a short walk fail rather than pass.
    paths = [layer / name for name in ('Dockerfile', 'install-gopls.sh',
                                       'verify-toolchain.sh', 'gopls_closure.py')]
    paths += sorted(path for path in (layer / 'data' / 'gopls-closure').iterdir()
                    if path.is_file())
    helper = layer / 'helper'
    paths += sorted(path for path in helper.rglob('*') if path.is_file())
    if len(paths) < 30:
        print(f'input_digests: found {len(paths)} inputs, want at least 30: the walk is '
              f'not looking at the helper module', file=sys.stderr)
        return 1
    lines = [f'{hashlib.sha256(path.read_bytes()).hexdigest()}  '
             f'{path.relative_to(root)}' for path in paths]
    for line in lines:
        print('input', line)
    print('inputs-set',
          hashlib.sha256(('\n'.join(lines) + '\n').encode()).hexdigest())
    return 0


if __name__ == '__main__':
    sys.exit(main())
