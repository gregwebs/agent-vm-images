#!/usr/bin/env python3
"""Focused dsh orchestration; primitive accounting remains a separate contract."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import sys
import tempfile

spec = importlib.util.spec_from_file_location('consumers', Path(__file__).with_name('standard-node-consumers.py'))
base = importlib.util.module_from_spec(spec)
spec.loader.exec_module(base)
ROOT = base.ROOT
INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
INSTALL_BUDGET_KIB = 1048576
RESERVE_KIB = 524288
BODY = '/source/script/test/fixtures/standard-node-dsh.py'
PREFIX = '/fixtures/image-root/opt/agent-vm/dsh'
COMMAND = '/fixtures/image-root/usr/local/bin/dsh'
EXACT = re.compile(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?')


def arguments(args):
    if not args or args[0].startswith('-'):
        raise ValueError('expected BASE_IMAGE --consumer dsh [--version EXACT] [--inputs PREFIX] '
                         '[--cordis-negative] [--platform linux/ARCH]')
    flags = {}
    index = 1
    while index < len(args):
        flag = args[index]
        if flag == '--cordis-negative':
            if flag in flags:
                raise ValueError('unknown/duplicate/missing option: ' + flag)
            flags[flag] = True
            index += 1
            continue
        if flag not in ('--consumer', '--version', '--inputs', '--platform') or flag in flags or index + 1 == len(args):
            raise ValueError('unknown/duplicate/missing option: ' + flag)
        flags[flag] = args[index + 1]
        index += 2
    if flags.get('--consumer') != 'dsh':
        raise ValueError('expected --consumer dsh')
    version = flags.get('--version')
    if version is not None and not EXACT.fullmatch(version):
        raise ValueError('expected canonical exact dsh version')
    platform = flags.get('--platform')
    if platform not in (None, 'linux/amd64', 'linux/arm64'):
        raise ValueError('expected native Linux platform')
    inputs = ROOT / 'images/tools/dsh'
    if '--inputs' in flags:
        # Use the owning filesystem authorization, never turn an arbitrary host
        # directory into a privileged Docker mount.
        helper_spec = importlib.util.spec_from_file_location('dsh_project', ROOT / 'images/tools/dsh/project.py')
        helper = importlib.util.module_from_spec(helper_spec)
        sys.modules[helper_spec.name] = helper
        helper_spec.loader.exec_module(helper)
        helper.guard_environment([Path(flags['--inputs'])])
        inputs = helper.authorize_mutation(flags['--inputs']).prefix
    for name in INPUTS:
        if (inputs / name).is_symlink() or not (inputs / name).is_file():
            raise ValueError('expected four regular dsh inputs')
    selected = version or json.loads((inputs / 'package.json').read_text())['dependencies']['@deepseek-ai/dsh']
    if not EXACT.fullmatch(selected):
        raise ValueError('invalid input selection')
    return args[0], platform, inputs, selected, bool(flags.get('--cordis-negative'))


class Invocation(base.Invocation):
    def __init__(self, image, requested, inputs, version, cordis_negative=False):
        super().__init__(image, requested)
        self.inputs, self.version = inputs, version
        self.source = ''
        self.deadline = 190
        self.cordis_negative = cordis_negative

    def run(self, args):
        previous = base.COMMAND_TIMEOUT_SECONDS
        base.COMMAND_TIMEOUT_SECONDS = self.deadline
        try:
            return super().run(args)
        finally:
            base.COMMAND_TIMEOUT_SECONDS = previous

    def check_budget(self):
        retained = self.retained_kib()
        if retained > INSTALL_BUDGET_KIB:
            raise base.Failure(f'dsh install budget exceeded: {retained} > {INSTALL_BUDGET_KIB} KiB')
        return retained

    def evidence_allowance_bytes(self):
        return max(INSTALL_BUDGET_KIB * 1024 - (self.rw_kib + self.fixture_kib) * 1024
                   - self.evidence_allocated_bytes() - base.EVIDENCE_BLOCK_BYTES, 0)

    def mounts(self):
        return ['--mount', f'type=bind,src={ROOT},dst=/source,readonly',
                '--mount', f'type=volume,src={self.volume},dst=/fixtures',
                '--mount', f'type=bind,src={ROOT / "images/recipe-contract"},dst=/fixtures/contract,readonly']

    def action(self, main, action, *args):
        seconds = 630 if action in ('prepare', 'install') else 240 if action == 'cordis-negative' else 180
        self.deadline = seconds + 15
        try:
            return self.docker('exec', main, 'timeout', '--kill-after=5', str(seconds),
                               'python3', '-B', BODY, action, *args)
        finally:
            self.deadline = 190

    def negative_leg(self, main):
        """Remove the four reviewed overrides, re-resolve, and require a failure.

        Runs before the committed install: the rest of the flow is then the repair
        and the readiness proof. Fixture-authorized: the disposable copy lives in
        this invocation's own scratch and is uploaded, never written into sources.
        """
        negative = self.scratch / 'cordis-negative'
        negative.mkdir()
        for name in INPUTS:
            (negative / name).write_bytes((self.inputs / name).read_bytes())
        removed = json.loads(self.command('node', str(ROOT / 'script/test/fixtures/dsh/strip-cordis-overrides.mjs'),
                                          str(negative)))
        (negative / 'reviewed-overrides.json').write_text(json.dumps(removed['overrides']))
        self.docker('cp', str(negative), main + ':/invocation/cordis-negative')
        output = self.action(main, 'cordis-negative', '/invocation/cordis-negative')
        record = self.json_line(output, 'cordisNegative')
        if record.get('cordisNegative') not in ('exact-peer rejection', 'runtime startup failure'):
            raise base.Failure('the overrides-removed graph produced no failure evidence')
        print(output, end='', flush=True)

    def execute(self):
        self.command('python3', '-B', str(ROOT / 'images/tools/dsh/project.py'), 'guard', str(self.inputs))
        self.command('node', str(ROOT / 'images/recipe-contract/pnpm-lock.mjs'), 'check', 'dsh', str(self.inputs))
        self.source = self.command('git', '-C', str(ROOT), 'rev-parse', 'HEAD').strip()
        files = [*(self.inputs / name for name in INPUTS), *(ROOT / 'images/pnpm').rglob('*'),
                 *(ROOT / 'images/tools/dsh').glob('*'), *(ROOT / 'images/recipe-contract').rglob('*'),
                 *(ROOT / 'script/test').glob('standard-node-*.py'), ROOT / 'script/test/fixtures/standard-node-dsh.py',
                 ROOT / 'script/test/fixtures/dsh/oracle.mjs']
        hashes = {str(file): hashlib.sha256(file.read_bytes()).hexdigest() for file in files
                  if file.is_file() and '__pycache__' not in file.parts}
        (self.logs / 'source.json').write_text(json.dumps({'source': self.source, 'selection': self.version,
                                                        'inputs': hashes, 'dirty': self.command('git', '-C', str(ROOT), 'status', '--short')}, indent=2))
        context = self.docker('context', 'show').strip()
        driver = self.docker('buildx', 'inspect', context)
        if not any(line.split() == ['Driver:', 'docker'] for line in driver.splitlines()):
            raise base.Failure('daemon-backed docker driver required')
        daemon = json.loads(self.docker('info', '--format', '{{json .}}'))
        image = json.loads(self.docker('image', 'inspect', self.image))[0]
        self.docker('history', '--no-trunc', self.image)
        self.platform = self.requested or 'linux/' + base.arch(daemon['Architecture'])
        if daemon['OSType'] != 'linux' or 'linux/' + base.arch(daemon['Architecture']) != self.platform:
            raise base.Failure('daemon platform mismatch/emulation')
        if image['Os'] != 'linux' or 'linux/' + base.arch(image['Architecture']) != self.platform:
            raise base.Failure('base-image platform mismatch/emulation')
        print(f'dsh: predeclared install budget {INSTALL_BUDGET_KIB} KiB + reserve {RESERVE_KIB} KiB; measured prior install 307 MB/9 s', flush=True)
        self.volume = self.token + '-fixtures'
        self.docker('volume', 'create', self.volume)
        self.capacity(INSTALL_BUDGET_KIB + RESERVE_KIB)
        main = self.create(self.token + '-probe', '--platform', self.platform, '--network', 'bridge',
                           '--cap-drop', 'ALL', '--cap-add', 'SETUID', '--cap-add', 'SETGID', '--cap-add', 'SETPCAP',
                           '--tmpfs', '/root:rw,mode=0770,uid=0,gid=23456',
                           '--tmpfs', '/runtime-homes:rw,mode=1777', *self.mounts(), self.image, 'sleep', 'infinity')
        self.docker('start', main)
        self.action(main, 'initialize', self.token, self.source, self.version)
        if self.cordis_negative:
            self.negative_leg(main)
        for name in INPUTS:
            self.docker('cp', str(self.inputs / name), main + ':' + PREFIX + '/' + name)
        self.action(main, 'prepare', self.version)
        self.action(main, 'install')
        self.action(main, 'verify')
        self.action(main, 'clean')
        info = json.loads(self.docker('inspect', '--size', main))[0]
        self.rw_kib = (self.setup_rw + int(info['SizeRw']) + 1023) // 1024
        self.measure(main)
        self.capacity(RESERVE_KIB)
        self.docker('network', 'disconnect', 'bridge', main)
        if json.loads(self.docker('inspect', '--format', '{{json .NetworkSettings.Networks}}', main)):
            raise base.Failure('network attachments remain before offline runtime')
        # The oracle reads a disposable extraction, not evidence: it holds a link
        # to `/` that mirrors the container's fixture alias, and an evidence
        # uploader that follows links would walk the host root. Keep it outside
        # the evidence root and drop it as soon as the oracle has reported.
        image_root = Path(tempfile.mkdtemp(prefix='dsh-image-root-'))
        try:
            (image_root / 'opt/agent-vm').mkdir(parents=True)
            (image_root / 'usr/local/bin').mkdir(parents=True)
            # Preserve the absolute fixture link verbatim. This virtual-only alias
            # maps its invocation prefix to the image root, never to host /opt.
            (image_root / 'fixtures').mkdir()
            (image_root / 'fixtures/image-root').symlink_to('/')
            self.docker('cp', main + ':' + PREFIX, str(image_root / 'opt/agent-vm/dsh'))
            self.docker('cp', main + ':' + COMMAND, str(image_root / 'usr/local/bin/dsh'))
            self.command('python3', str(ROOT / 'script/test/standard-node-layout.py'), '--consumer', 'dsh',
                         '--root', str(image_root), '--platform', self.platform, '--expect-version', self.version)
        finally:
            shutil.rmtree(image_root, ignore_errors=True)
        seen = set()
        for uid, gid in base.PAIRS:
            home = '/root' if uid == 0 else f'/runtime-homes/u{uid}g{gid}'
            output = self.docker('exec', '-e', 'HOME=' + home, main, 'timeout', '--kill-after=5', '180',
                                 'setpriv', '--bounding-set=-all', '--inh-caps=-all', '--ambient-caps=-all',
                                 '--reuid', str(uid), '--regid', str(gid), '--clear-groups', '--no-new-privs',
                                 'python3', '-B', BODY, 'probe', self.token, self.source, self.version, str(uid), str(gid))
            expected = {'dshProbe': self.token, 'source': self.source, 'selection': self.version,
                        'pair': f'{uid}:{gid}', 'caps': 'zero', 'offline': True}
            details = self.json_line(output, 'dshRuntime')
            if any(details[key] != expected[{'dshRuntime': 'dshProbe'}.get(key, key)]
                   for key in ('dshRuntime', 'source', 'selection', 'pair')):
                raise base.Failure('runtime details do not bind to invocation/source/selection/pair')
            if (details['web']['http']['status'] != 200 or not details['web']['cleanup']['execTmpRemoved']
                    or details['web']['cleanup']['survivors'] != 0
                    or details['independent']['subprocess'] != {'stdout': 'independent-dsh\n', 'exitCode': 7, 'disposed': True}):
                raise base.Failure('runtime detail health/cleanup evidence invalid')
            report = self.json_line(output, 'dshProbe')
            if report != expected or report['pair'] in seen:
                raise base.Failure('stale/duplicate/invalid dsh evidence')
            seen.add(report['pair'])
            print(output, end='', flush=True)
        self.measure(main)
        print('provenance: diagnostic local base; no whole-base revision attestation; clean native CI remains required', flush=True)


def main(args):
    try:
        invocation = Invocation(*arguments(args))
    except (ValueError, OSError) as error:
        print('standard-node-consumers: ' + str(error), file=sys.stderr)
        return 2
    status = 0
    def interrupted(number, _frame):
        raise base.Failure('signal ' + str(number), 128 + number)
    for sig in (base.signal.SIGINT, base.signal.SIGTERM, base.signal.SIGHUP):
        base.signal.signal(sig, interrupted)
    try:
        invocation.execute()
    except (base.Failure, OSError, ValueError, KeyError) as error:
        print('standard-node-consumers: ' + str(error), file=sys.stderr)
        status = error.status if isinstance(error, base.Failure) else 1
    finally:
        try:
            invocation.cleanup()
        except base.Failure as error:
            print(str(error), file=sys.stderr)
            status = status or error.status
    if not status:
        print('standard-node-consumers: dsh PASS (native focused diagnostic)')
    return status


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
