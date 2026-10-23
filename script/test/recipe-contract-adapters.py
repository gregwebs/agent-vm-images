#!/usr/bin/env python3
"""Keep native-helper build failures visible at shell adapter boundaries."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def main():
    with tempfile.TemporaryDirectory(prefix='recipe-contract-adapter-') as directory:
        root = Path(directory)
        (root / 'script/test').mkdir(parents=True)
        (root / 'script/build').mkdir()
        (root / 'bin').mkdir()
        entry = root / 'script/test/dsh-prepare-lock.sh'
        shutil.copyfile(ROOT / 'script/test/dsh-prepare-lock.sh', entry)
        shutil.copyfile(ROOT / 'script/test/fixtures/recipe-contract/build-failure.sh',
                        root / 'script/build/recipe-contract.sh')
        # Only command discovery runs before the injected adapter fails.
        for name in ['jq', 'node', 'python3', 'timeout', 'sha256sum']:
            (root / 'bin' / name).symlink_to(shutil.which('true'))
        environment = {**os.environ, 'PATH': str(root / 'bin') + ':/usr/bin:/bin'}
        result = subprocess.run(['/bin/bash', str(entry)], env=environment,
                                capture_output=True, text=True)
        assert result.returncode == 23, (result.returncode, result.stdout, result.stderr)
        assert result.stderr == 'recipe-contract build: injected failure\n', result.stderr
    print('recipe-contract-adapters: build status and diagnostic preserved')


if __name__ == '__main__':
    main()
