#!/usr/bin/env python3
"""Acquire and authenticate the developer's stock manager; never PATH pnpm."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tarfile

sys.dont_write_bytecode = True


def main():
    helper_spec = importlib.util.spec_from_file_location('dsh_project', Path(__file__).with_name('project.py'))
    project = importlib.util.module_from_spec(helper_spec)
    sys.modules[helper_spec.name] = project
    helper_spec.loader.exec_module(project)
    project.guard_environment([sys.argv[1]])
    capability = project.authorize_mutation(sys.argv[1])
    if capability.production:
        raise ValueError('host acquisition is developer-only')
    prefix = capability.prefix
    source = Path(__file__).resolve().parents[2] / 'pnpm'
    spec = importlib.util.spec_from_file_location('bootstrap', source / 'bootstrap.py')
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


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, tarfile.TarError, subprocess.CalledProcessError) as error:
        print('dsh host manager: ' + str(error), file=sys.stderr)
        sys.exit(1)
