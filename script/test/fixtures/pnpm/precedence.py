"""Installation-time proof that image defaults outweigh project direct keys.

A config query only proves the displayed value. This driver runs real installs
against the conflicting project files below, so all five launcher-supplied keys
are decided by observable package-manager behavior rather than display.
"""
from dataclasses import dataclass
import json
from pathlib import Path
from typing import Callable

CONFLICT_YAML = (
    'minimumReleaseAge: 0\n'
    'minimumReleaseAgeStrict: false\n'
    'minimumReleaseAgeIgnoreMissingTime: true\n'
    'trustPolicy: off\n'
    'ignoreScripts: false\n'
)
CONFLICT_NPMRC = (
    'minimum-release-age=0\n'
    'minimum-release-age-strict=false\n'
    'minimum-release-age-ignore-missing-time=true\n'
    'trust-policy=off\n'
    'ignore-scripts=false\n'
)


@dataclass(frozen=True)
class Install:
    project: Path
    status: int
    output: str


RunInstall = Callable[[list[str], dict[str, str]], Install]


def write_conflicting_project(project: Path) -> None:
    (project / 'pnpm-workspace.yaml').write_text(CONFLICT_YAML)
    (project / '.npmrc').write_text(CONFLICT_NPMRC)


def _installed_version(outcome: Install) -> str:
    metadata = outcome.project / 'node_modules/fixture-package/package.json'
    return json.loads(metadata.read_text())['version']


def assert_defaults_win(run: RunInstall) -> None:
    """Conflicting direct project keys lose; only env/CLI safely override."""
    for args, overrides, error, label in [
            (['add', 'fixture-package@1.1.0'], {}, 'ERR_PNPM_NO_MATURE_MATCHING_VERSION', 'age'),
            (['add', 'fixture-trust@1.1.0'], {}, 'ERR_PNPM_TRUST_DOWNGRADE', 'trust')]:
        outcome = run(args, overrides)
        assert outcome.status != 0 and error in outcome.output, (label, outcome)
        assert not (outcome.project / 'markers').exists(), label
    suppressed = run(['add', 'fixture-hooks@1.0.0'], {})
    assert suppressed.status == 0 and not (suppressed.project / 'markers').exists(), suppressed

    # Missing-time is isolated from trust: stock trust checking also reports
    # ERR_PNPM_MISSING_TIME, so trust is disabled here. The only remaining cause
    # is the image's ignore-missing-time default beating the project's `true`;
    # flipping that one env key alone must flip the outcome.
    missing_time = run(['add', 'fixture-missing@1.0.0'], {'PNPM_CONFIG_TRUST_POLICY': 'off'})
    assert missing_time.status != 0 and 'ERR_PNPM_MISSING_TIME' in missing_time.output, missing_time
    assert not (missing_time.project / 'markers').exists(), missing_time
    missing_time_allowed = run(['add', 'fixture-missing@1.0.0'], {
        'PNPM_CONFIG_TRUST_POLICY': 'off', 'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'true'})
    assert missing_time_allowed.status == 0, missing_time_allowed

    young = run(['add', 'fixture-package@1.1.0'], {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '0'})
    assert young.status == 0 and _installed_version(young) == '1.1.0', young
    nonstrict = run(['add', 'fixture-package@1.1.0'], {'PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT': 'false'})
    assert nonstrict.status == 0 and _installed_version(nonstrict) == '1.1.0', nonstrict
    trust = run(['add', 'fixture-trust@1.1.0'], {'PNPM_CONFIG_TRUST_POLICY': 'off'})
    assert trust.status == 0, trust
    cli = run(['--config.minimum-release-age=0', 'add', 'fixture-package@1.1.0'], {})
    assert cli.status == 0 and _installed_version(cli) == '1.1.0', cli
    approved = run(['add', 'fixture-hooks@1.0.0', '--allow-build=fixture-hooks',
                    '--allow-build=fixture-dependency'], {'PNPM_CONFIG_IGNORE_SCRIPTS': 'false'})
    assert approved.status == 0, approved
    markers = (approved.project / 'markers').read_text().split()
    assert set(markers) == {'fixture-hooks', 'fixture-dependency'}, approved
