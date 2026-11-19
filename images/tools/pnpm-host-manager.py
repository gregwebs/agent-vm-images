#!/usr/bin/env python3
"""Acquire the reviewed stock manager for a host; never PATH pnpm.

Host maintenance needs pnpm 11.11.0 from the reviewed distribution. This entry
point authorizes the consumer's capability and then asks the composed
recipe-contract helper for the distribution: `pnpm-extract` downloads the pin's own
URL through `internal/fetch`, authenticates the bytes, validates the member
inventory and unpacks the package, all under the rules the image build runs. The
pin, digest, extraction and package policy live in `internal/pnpm` and nowhere
else, so a host cannot execute a distribution selected under different rules.

It is developer-only: production starts the fixed base launcher instead, so the
acquisition variables are refused there.

The consumer is a closed profile, not a caller option with a default: every
consumer names its own acquisition variables, and no consumer can claim another's
manager.
"""
import argparse
import importlib.util
import json
from pathlib import Path
import subprocess
import sys

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
    parser.add_argument('--archive',
                        help='authenticate this already-downloaded archive instead of '
                             'fetching the pinned URL')
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
    destination = prefix.parent / 'manager'
    project.no_redirect(destination, capability.root)
    project.disjoint([destination, prefix, capability.basePin])
    destination.mkdir(mode=0o700)
    arguments = ['pnpm-extract', '--destination', str(destination), '--source', str(source)]
    if options.archive is None:
        arguments.append('--download')
    else:
        arguments += ['--archive', str(project.absolute(options.archive))]
    unpacked = json.loads(project.run_contract([prefix], *arguments))
    print(json.dumps({'package': unpacked['package'], 'archive': unpacked['archive']}))


def entry(consumer=None):
    try:
        main(consumer=consumer)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print('pnpm host manager: ' + str(error), file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    entry()
