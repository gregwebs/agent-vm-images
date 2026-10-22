"""Native composed-directory adapter for Python verifier fixtures."""
import subprocess
from pathlib import Path


def native_contract_directory(root: Path) -> Path:
    return Path(subprocess.check_output(
        ['bash', str(root / 'script/build/recipe-contract.sh'), '--native'], text=True).strip()) / 'out'
