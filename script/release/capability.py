"""Tiny ordinary local-base to canonical OCI/export/loader capability gate."""

import sys
sys.dont_write_bytecode = True
from pathlib import Path
import json
import os
from content import inventory_layout, normalize_root
from operations import ROOT, docker_matches, run


def check(builder: str, arch: str, scratch: Path, prefix: str) -> None:
    context = scratch / 'capability-context'
    context.mkdir()
    base, child = prefix + '-cap-base:local', prefix + '-cap-child:local'
    pins = json.loads((ROOT / 'script/release/tool-pins.json').read_text())
    (context / 'Dockerfile.base').write_text('FROM ' + pins['fixture_base'] + '\n')
    (context / 'Dockerfile').write_text(f'FROM {base}\nUSER 12345:23456\nRUN true\nRUN true\n')
    env = dict(os.environ, TMPDIR=str(scratch / 'tmp'))
    run(['docker', 'build', '--builder', builder, '--platform', 'linux/' + arch, '-t', base,
         '-f', str(context / 'Dockerfile.base'), str(context)], output=scratch / 'capability-base.log', env=env)
    layout = scratch / 'capability-layout'
    run(['docker', 'buildx', 'build', '--builder', builder, '--platform', 'linux/' + arch,
         '--provenance=false', '--sbom=false', '-t', child,
         '--output', f'type=oci,dest={layout},tar=false,compression=gzip', str(context)],
        output=scratch / 'capability-export.log', env=env)
    graph = inventory_layout(layout, arch)
    normalize_root(layout, graph)
    archive = scratch / 'capability.tar'
    run(['tar', '-C', str(layout), '-cf', str(archive), 'oci-layout', 'index.json', 'blobs'],
        env=dict(env, COPYFILE_DISABLE='1'))
    run(['docker', 'image', 'load', '--input', str(archive)], output=scratch / 'capability-load.log')
    docker_matches(layout, graph, child)
    run(['docker', 'run', '--rm', '--network', 'none', '--cap-drop', 'ALL', '--user', '12345:23456', child,
         'sh', '-c', 'set -eu; test "$(id -u)" = 12345; test "$(id -g)" = 23456; echo capability-ok'],
        output=scratch / 'capability-uid.log')
    run(['docker', 'image', 'rm', child, base])
