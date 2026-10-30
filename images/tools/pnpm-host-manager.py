#!/usr/bin/env python3
"""Acquire and authenticate a consumer's stock manager; never PATH pnpm.

Host maintenance needs pnpm 11.11.0 from the reviewed distribution. This helper
downloads that distribution, checks it against the reviewed pin and hands back
argv for the extracted copy. It is developer-only: production starts the fixed
base launcher instead, so the acquisition variables are refused there.

The consumer is a closed profile, not a caller option with a default: dsh, Pi and
the bridge each name their own acquisition variables, and no consumer can claim
another's manager.
"""
import argparse
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tarfile

sys.dont_write_bytecode = True

SHARED = Path(__file__).resolve().with_name('pnpm-project.py')


def load_project():
    """Import the shared module once, so capability types keep one identity."""
    name = 'pnpm_project'
    module = sys.modules.get(name)
    if module is not None:
        return module
    spec = importlib.util.spec_from_file_location(name, SHARED)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def main(argv=None, consumer=None):
    parser = argparse.ArgumentParser(prog='pnpm-host-manager.py')
    if consumer is None:
        parser.add_argument('--consumer', required=True,
                            choices=sorted(load_project().CONSUMERS))
    parser.add_argument('prefix')
    options = parser.parse_args(argv)
    project = load_project()
    profile = project.consumer_of(options.consumer if consumer is None else consumer)
    project.guard_environment([options.prefix], consumer=profile)
    capability = project.authorize_mutation(options.prefix, consumer=profile)
    if capability.production:
        raise ValueError('host acquisition is developer-only')
    prefix = capability.prefix
    # The reviewed pin of this checkout, never the caller-selected PNPM_SOURCE_DIR
    # mount: that seam names the base image's sources for a version check, while
    # acquisition must be the exact pin this source tree ships.
    source = Path(__file__).resolve().parents[1] / 'pnpm'
    spec = importlib.util.spec_from_file_location('pnpm_bootstrap', source / 'bootstrap.py')
    bootstrap = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bootstrap)
    pin = bootstrap.read_pin(source / 'pin.json')
    destination = prefix.parent / 'manager'
    project.no_redirect(destination, capability.root)
    project.disjoint([destination, prefix, capability.basePin])
    destination.mkdir(mode=0o700)
    archive = destination / 'pnpm.tgz'
    subprocess.run(['curl', '--fail', '--silent', '--show-error', '--location', '--proto', '=https',
                    '--connect-timeout', '15', '--max-time', '180', pin['url'], '-o', str(archive)], check=True)
    bootstrap.authenticate(archive, pin)
    package = bootstrap.extract(archive, destination, pin)
    print(json.dumps({'package': str(package), 'archive': str(archive)}))


def entry(consumer=None):
    try:
        main(consumer=consumer)
    except (ValueError, OSError, tarfile.TarError, subprocess.CalledProcessError) as error:
        print('pnpm host manager: ' + str(error), file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    entry()
