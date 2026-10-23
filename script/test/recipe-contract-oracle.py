#!/usr/bin/env python3
"""Keep oracle replay mandatory and its missing-history failure actionable."""
import pathlib
import shutil
import subprocess
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[2]
ORACLE = pathlib.Path('script/test/recipe-contract-oracle.mjs')


def main():
    contracts = (ROOT / 'script/test/contracts.sh').read_text()
    assert 'node script/test/recipe-contract-oracle.mjs' in contracts, 'contracts must replay the JS oracle'
    with tempfile.TemporaryDirectory(prefix='recipe-contract-oracle-test-') as directory:
        root = pathlib.Path(directory)
        subprocess.run(['git', 'init', '-q', str(root)], check=True)
        target = root / ORACLE
        target.parent.mkdir(parents=True)
        shutil.copyfile(ROOT / ORACLE, target)
        result = subprocess.run(['node', str(target)], capture_output=True, text=True, check=False)
        assert result.returncode != 0, 'oracle accepted missing history'
        assert '3b21103' in result.stderr and 'fetch' in result.stderr and 'history' in result.stderr, result.stderr
    print('recipe-contract-oracle: wiring and missing-history controls passed')


if __name__ == '__main__':
    main()
