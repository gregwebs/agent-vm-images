#!/usr/bin/env python3
"""Fake `docker`, `gh` and `curl` for the parent-pin maintenance suite (#95).

Every case drives the real resolver and the real publisher; only the three
external seams are replaced. The fakes enforce the interface instead of imitating
it: the attestation check asserts the exact composed signer workflow, source ref,
hosted-runner refusal and absence of a token, and the registry only serves bytes
that hash to the digest they are addressed by.

State is read from `<case>/state.json`, beside the `bin/` directory holding these
run names, because the public download transport deliberately runs with a
stripped environment. `state.json` is a JSON object:

    registry   reference -> fixture file, e.g. "debian:13-slim"
    images     repository@child-digest -> {"etc/apt": dir, "usr/share/keyrings": dir}
    downloads  directory served for release assets
    attest     release-json/index digest, source SHA, invocation, signer identity
    failures   {"registry": [...], "images": [...], "downloads": [...], "attestation": [...]}

Events are appended to `<case>/events.jsonl`, one JSON object per line.
"""
import hashlib
import json
import os
import shutil
import signal
import sys
from pathlib import Path

SLSA = 'https://slsa.dev/provenance/v1'
TOKEN_VARS = ('GH_TOKEN', 'GITHUB_TOKEN', 'GH_ENTERPRISE_TOKEN', 'GITHUB_ENTERPRISE_TOKEN')
CASE = Path(sys.argv[0]).resolve().parent.parent
STATE = CASE / 'state.json'
CONTAINERS = CASE / 'containers.json'
LOG = CASE / 'events.jsonl'


def state():
    return json.loads(STATE.read_text())


def save_containers(value):
    CONTAINERS.write_text(json.dumps(value, indent=1) + '\n')


def containers():
    return json.loads(CONTAINERS.read_text()) if CONTAINERS.exists() else {}


def record(kind, value):
    with LOG.open('a') as stream:
        stream.write(json.dumps({'kind': kind, 'value': value}) + '\n')
    run_hooks()


def run_hooks():
    """Choreograph a mid-run filesystem change once N events have been logged.

    The publisher must detect an ancestor, input or destination that changes
    while it resolves, so the cases need a change that happens *during* the run.
    """
    counter = CASE / 'events.count'
    seen = int(counter.read_text()) + 1 if counter.exists() else 1
    counter.write_text(str(seen))
    hooks = state().get('hooks', [])
    for hook in hooks:
        if hook['after_events'] != seen:
            continue
        if 'symlink' in hook:
            Path(hook['symlink']['path']).symlink_to(hook['symlink']['target'])
        elif 'rename' in hook:
            Path(hook['rename']['path']).rename(hook['rename']['to'])
        elif 'chmod' in hook:
            os.chmod(hook['chmod']['path'], hook['chmod']['mode'])
        elif 'write' in hook:
            Path(hook['write']['path']).write_text(hook['write']['text'])
        elif 'unlink' in hook:
            Path(hook['unlink']).unlink()
        else:
            fail('unknown hook: %r' % (hook,))


def fail(message):
    sys.stderr.write(message + '\n')
    raise SystemExit(1)


def docker(argv):
    value = state()
    failures = value.get('failures', {})
    if argv[:2] == ['buildx', 'version']:
        print('github.com/docker/buildx v0.0.0-fixture')
        return 0
    if argv[:1] == ['info']:
        print('Server Version: fixture')
        return 0
    if argv[:3] == ['buildx', 'imagetools', 'inspect']:
        reference = argv[argv.index('--raw') + 1]
        record('docker', ['inspect', reference])
        if reference in failures.get('registry', []):
            fail('manifest unknown: ' + reference)
        fixture = value['registry'].get(reference)
        if fixture is None:
            fail('no such manifest: ' + reference)
        sys.stdout.buffer.write(Path(fixture).read_bytes())
        return 0
    if argv[:1] == ['pull']:
        reference = argv[-1]
        record('docker', ['pull', argv[argv.index('--platform') + 1], reference])
        if reference in failures.get('images', []):
            fail('pull access denied: ' + reference)
        return 0
    if argv[:1] == ['create']:
        reference = argv[-1]
        record('docker', ['create', argv[argv.index('--platform') + 1], reference])
        live = containers()
        identifier = 'c%04d' % (len(live) + 1)
        live[identifier] = {'reference': reference, 'live': True}
        save_containers(live)
        print(identifier)
        return 0
    if argv[:1] == ['cp']:
        source, destination = argv[1], Path(argv[2])
        identifier, _, path = source.partition(':')
        record('docker', ['cp', source, str(destination)])
        live = containers()
        if identifier not in live:
            fail('no such container: ' + identifier)
        image = value['images'].get(live[identifier]['reference'], {})
        if path not in image:
            fail('no such path in container: ' + source)
        shutil.copytree(Path(image[path]), destination)
        return 0
    if argv[:1] == ['rm']:
        identifier = argv[-1]
        record('docker', ['rm', identifier])
        live = containers()
        if identifier not in live:
            fail('no such container: ' + identifier)
        live[identifier]['live'] = False
        save_containers(live)
        return 0
    fail('unsupported docker argv: %r' % (argv,))


def attestation_digest(subject):
    if subject.startswith('oci://'):
        reference = subject.split('@', 1)[1] if '@' in subject else subject
        return reference.split(':', 1)[1]
    path = Path(subject)
    if not path.is_file():
        fail('attestation subject is not a file: ' + subject)
    return hashlib.sha256(path.read_bytes()).hexdigest()


def gh(argv):
    if argv == ['attestation', 'verify', '--help']:
        print('USAGE fixture: gh attestation verify')
        return 0
    value = state()
    attest = value['attest']
    if argv[:2] != ['attestation', 'verify']:
        fail('unsupported gh argv: %r' % (argv,))
    for token in TOKEN_VARS:
        if os.environ.get(token):
            fail('attestation verification must run without a token: ' + token)
    if os.environ.get('GH_CONFIG_DIR'):
        fail('attestation verification must run without a gh config directory')
    subject = argv[2]
    options, index = {}, 3
    while index < len(argv):
        argument = argv[index]
        if argument == '--deny-self-hosted-runners':
            options[argument] = True
            index += 1
            continue
        if not argument.startswith('--'):
            fail('unexpected gh argument: ' + argument)
        options[argument] = argv[index + 1]
        index += 2
    bundle = Path(options['--bundle']).name
    record('gh', ['verify', subject, bundle, sorted(options)])
    if bundle in value.get('failures', {}).get('attestation', []):
        fail('no matching attestations found in ' + bundle)
    expected = {'--repo': attest['repository'], '--signer-workflow': attest['workflow'],
                '--source-ref': 'refs/heads/main', '--predicate-type': SLSA, '--format': 'json'}
    for key, wanted in expected.items():
        if options.get(key) != wanted:
            fail('wrong %s: %r, expected %r' % (key, options.get(key), wanted))
    if '--deny-self-hosted-runners' not in options:
        fail('self-hosted runners must be denied')
    if options.get('--source-digest') not in (None, attest['source_sha']):
        fail('wrong --source-digest: %r' % (options.get('--source-digest'),))
    digest = attestation_digest(subject)
    bound = {'release-metadata.sigstore.json': attest['release_json_sha256'],
             'release-index.sigstore.json': attest['index_digest']}.get(bundle)
    if bound is not None and digest != bound:
        fail('attestation subject %s does not match the signed subject' % subject)
    statement = {'verificationResult': {
        'verifiedTimestamps': [{'type': 'transparency-log', 'uri': 'fixture'}],
        'statement': {
            'predicateType': SLSA,
            'subject': [{'name': subject, 'digest': {'sha256': digest}}],
            'predicate': {'runDetails': {'metadata': {'invocationId': attest['invocation']}}}},
        'signature': {'certificate': {'runInvocationURI': attest['invocation']}}}}
    print(json.dumps([statement]))
    return 0


def curl(argv):
    value = state()
    url = argv[-1]
    name = url.rsplit('/', 1)[-1]
    record('curl', [name, [argument for argument in argv if argument.startswith('--')]])
    if '=https' not in argv:
        fail('public download must pin the https protocol')
    if name in value.get('failures', {}).get('downloads', []):
        fail('HTTP 404 for ' + url)
    path = Path(value['downloads']) / name
    if not path.is_file():
        fail('no fixture download for ' + name)
    sys.stdout.buffer.write(path.read_bytes())
    return 0


def cp_rule(marker):
    """A plain needle, or a JSON rule selecting the phase and the fault."""
    text = marker.read_text().strip()
    if not text:
        return None
    if text.startswith('{'):
        return json.loads(text)
    return {'needle': text}


def cp_matches(rule, argv):
    needle = rule['needle']
    if not any(needle in argument for argument in argv):
        return False
    # `transactional-publish.sh` writes a backup as `cp -p DEST BACKUP/.tmpN`
    # and a publication or restore as `cp SRC DEST`, so the destination is what
    # tells the backup phase from the publication phase.
    backup_copy = '/publish-backup/' in argv[-1]
    phase = rule.get('phase')
    if phase == 'backup':
        return backup_copy
    if phase == 'publish':
        return not backup_copy
    return True


def truncate(argv):
    """Leave a partial copy behind, as a failed `cp` can before it reports."""
    source, destination = Path(argv[-2]), Path(argv[-1])
    destination.write_bytes(source.read_bytes()[:8])


def cp(argv):
    """Fail, truncate at, or signal a chosen publication copy.

    `transactional-publish.sh` copies inside a command list, where a shell DEBUG
    trap cannot fire, so a copy fault or a signal is injected at the command
    boundary the publisher actually crosses. The marker holds either a plain
    needle (fail any copy whose arguments contain it) or a JSON rule:

        {"needle": "images/Dockerfile", "phase": "publish",
         "action": "fail" | "TERM", "truncate": true}

    `phase: backup` matches only the backup copy, `phase: publish` only a copy
    that writes a checkout destination (publication or restore).
    """
    marker = CASE / 'fail-copy'
    record('cp', list(argv))
    if marker.exists():
        rule = cp_rule(marker)
        if rule and cp_matches(rule, argv):
            action = rule.get('action', 'fail')
            if action == 'TERM':
                # The publisher defers the trap until this copy returns, so the
                # signal lands between the copies it is meant to interrupt.
                os.kill(os.getppid(), signal.SIGTERM)
            elif action == 'fail':
                if rule.get('truncate'):
                    truncate(argv)
                fail('injected copy failure for ' + rule['needle'])
            else:
                fail('unknown copy rule action: %r' % (action,))
    os.execv('/bin/cp', ['/bin/cp', *argv])


def main():
    name = Path(sys.argv[0]).name
    argv = sys.argv[1:]
    if name == 'docker':
        return docker(argv)
    if name == 'gh':
        return gh(argv)
    if name == 'curl':
        return curl(argv)
    if name == 'cp':
        return cp(argv)
    fail('unknown fixture tool: ' + name)


if __name__ == '__main__':
    sys.exit(main())
