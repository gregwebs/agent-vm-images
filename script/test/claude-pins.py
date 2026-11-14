#!/usr/bin/env python3
"""Hermetic evidence for the committed Claude native payload closure (#98).

Every case here runs the production reader, `images/tools/claude/claude_pin.py`,
over a closure this suite synthesizes with its own OpenPGP key
(`script/test/fixtures/claude/claude_fixture.py`): real signatures, real ELF
headers, real layout checks. Nothing reaches the network and no committed evidence
is touched. The negative cases are the point: a rewritten manifest, a stale or
foreign signature, a substituted key, a signing subkey pinned as the primary, a
wrong-machine ELF, a group-writable directory, a launcher that points elsewhere, a
native state file that says updates are on.

The maintenance writer (`script/dev/pin-claude.py`) is exercised through
`script/test/fixtures/agent-versions/claude-writer.py`, with its one network
primitive substituted, so the fetch/verify/delta/publication order is the real
code path. Its redirect policy is checked against the *real* shared transport
handler: only `urllib.request.build_opener` in the bound transport module is
replaced.

`--write-fixture DIR --version V` writes a committed-shape closure plus the served
assets for one release, which `script/test/agent-versions.sh` installs into its
fixture repository.

The pin date is UTC on both sides, so two of the clock cases below run the real
writer and the real CLI as separate processes under `TZ=Pacific/Kiritimati` and
`TZ=Etc/GMT+12`: a host's local date only exists inside a real zone, and it is
exactly a local-date comparison that the UTC pin date replaced.

gpg homes: the fixture and the module under test both allocate theirs under /tmp,
never an inherited TMPDIR, so the suite gives the same answer whether TMPDIR is
short or long; the `gpg home` cases cover both, including the assertion that a
home is a private directory under /tmp even when TMPDIR points elsewhere. All
other scratch lives in a per-run directory and is removed by this process.
"""
from __future__ import annotations

import datetime
import email.message
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PIN_DIR = ROOT / 'images' / 'tools' / 'claude'
MODULE = PIN_DIR / 'claude_pin.py'
FIXTURE_DIR = ROOT / 'script' / 'test' / 'fixtures' / 'claude'
WRITER_ADAPTER = ROOT / 'script' / 'test' / 'fixtures' / 'agent-versions' / 'claude-writer.py'

sys.path.insert(0, str(PIN_DIR))
sys.path.insert(0, str(FIXTURE_DIR))

import claude_fixture  # noqa: E402  (the fixture closure builder)
import claude_pin  # noqa: E402  (the production reader under test)

VERSION = '9.9.9'
OLD_VERSION = '9.9.8'
PLATFORMS = ('linux-x64', 'linux-arm64')
MACHINES = {'linux-x64': 62, 'linux-arm64': 183}

failures: list[str] = []
work: list[Path] = []


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: str) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


def run_case(label: str, function) -> str:
    try:
        function()
    except AssertionError as error:
        fail(label, str(error))
        return 'failed'
    except Exception as error:  # noqa: BLE001 - every case reports, none aborts
        fail(label, f'{type(error).__name__}: {error}')
        return 'failed'
    report(label)
    return 'passed'


def scratch(label: str) -> Path:
    directory = Path(tempfile.mkdtemp(prefix=f'claude-pins.{label}.',
                                      dir=os.environ.get('TMPDIR')))
    work.append(directory)
    return directory


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def refuse(call) -> str:
    """The refusal text of `call`, which must refuse."""
    try:
        call()
    except claude_pin.Refusal as error:
        return str(error)
    raise AssertionError('expected a refusal, got success')


def requires(call, *needles: str) -> str:
    text = refuse(call)
    for needle in needles:
        if needle not in text:
            raise AssertionError(f'refusal did not mention {needle!r}: {text}')
    return text


def accepts(call) -> None:
    try:
        call()
    except claude_pin.Refusal as error:
        raise AssertionError(f'unexpected refusal: {error}') from None


# --- closure mutation helpers -------------------------------------------------

def copy_closure(source: Path, destination: Path) -> Path:
    shutil.copytree(source, destination)
    return destination


def read_pin(directory: Path) -> dict:
    return json.loads((directory / 'pin.json').read_text())


def write_pin(directory: Path, document: dict) -> None:
    (directory / 'pin.json').write_text(json.dumps(document, indent=2) + '\n')


def mutated_closure(base: Path, destination: Path, mutate) -> Path:
    copy_closure(base, destination)
    document = read_pin(destination)
    mutate(document)
    write_pin(destination, document)
    return destination


def rewrite_manifest(directory: Path, signer, mutate) -> None:
    """Change the signed manifest and re-sign it, keeping the pin consistent."""
    evidence = directory / 'evidence'
    document = json.loads((evidence / 'manifest.json').read_text())
    mutate(document)
    manifest = json.dumps(document, indent=2).encode() + b'\n'
    (evidence / 'manifest.json').write_bytes(manifest)
    signature = claude_fixture.sign(signer, evidence / 'manifest.json')
    (evidence / 'manifest.json.sig').write_bytes(signature)
    pin = read_pin(directory)
    pin['manifest']['sha256'] = digest(manifest)
    pin['signature']['sha256'] = digest(signature)
    write_pin(directory, pin)


def digest(raw: bytes) -> str:
    import hashlib
    return hashlib.sha256(raw).hexdigest()


def load_and_verify(directory: Path, version: str = VERSION):
    return claude_pin.verify(claude_pin.load(directory), version)


def verify_closure(directory: Path, version: str = VERSION) -> None:
    accepts(lambda: load_and_verify(directory, version))


def expect_verify_refusal(directory: Path, *needles: str,
                          version: str = VERSION) -> None:
    requires(lambda: load_and_verify(directory, version), *needles)


def expect_load_refusal(directory: Path, *needles: str) -> None:
    requires(lambda: claude_pin.load(directory), *needles)


# --- fixture template ---------------------------------------------------------

class Template:
    """One signer, two ELF blobs and a consistent closure, built once per run."""

    def __init__(self) -> None:
        base = scratch('template')
        self.root = base
        self.signer = claude_fixture.make_signer(base / 'signer')
        self.x64 = claude_fixture.elf(base / 'assets' / 'linux-x64', 62)
        self.arm64 = claude_fixture.elf(base / 'assets' / 'linux-arm64', 183)
        self.closure = claude_fixture.make_closure(
            base / 'closure', signer=self.signer, version=VERSION,
            amd64=self.x64, arm64=self.arm64)
        self.counter = 0

    def copy(self, label: str) -> Path:
        self.counter += 1
        return copy_closure(self.closure, scratch(f'{label}.{self.counter}') / 'source')

    def mutate(self, label: str, mutate) -> Path:
        self.counter += 1
        return mutated_closure(self.closure,
                               scratch(f'{label}.{self.counter}') / 'source', mutate)

    def resign(self, label: str, mutate) -> Path:
        directory = self.copy(label)
        rewrite_manifest(directory, self.signer, mutate)
        return directory


def run_cli(*arguments: str) -> subprocess.CompletedProcess:
    return subprocess.run([sys.executable, str(MODULE), *arguments],
                          capture_output=True, text=True, check=False)


def run_cli_in(zone: str, *arguments: str) -> subprocess.CompletedProcess:
    """The real CLI as an isolated process running under `TZ=zone`.

    A zone has to reach a real process: `date.today()` reads the C library's local
    time, which no in-process substitution of the reader's module can move.
    """
    return subprocess.run([sys.executable, '-I', str(MODULE), *arguments],
                          capture_output=True, text=True, check=False,
                          env=dict(os.environ, TZ=zone))


# --- committed closure --------------------------------------------------------

def committed_cases() -> None:
    """The real committed pin. Available once the first adoption has run."""
    if not (PIN_DIR / 'pin.json').is_file():
        print('skip: committed Claude pin group (no pin.json adopted yet)')
        return
    pin = claude_pin.load(PIN_DIR)

    def version() -> None:
        finished = run_cli('version', '--pin-dir', str(PIN_DIR))
        require(finished.returncode == 0, f'version exited {finished.returncode}: '
                                          f'{finished.stderr}')
        require(finished.stdout.strip() == pin.version,
                f'version printed {finished.stdout!r}, pin names {pin.version}')

    def digests() -> None:
        for token in PLATFORMS:
            finished = run_cli('digest', '--pin-dir', str(PIN_DIR), '--platform', token)
            require(finished.returncode == 0, f'digest {token} exited '
                                              f'{finished.returncode}: {finished.stderr}')
            require(finished.stdout.strip() == pin.executables[token].sha256,
                    f'digest {token} printed {finished.stdout!r}')

    def check() -> None:
        finished = run_cli('check', '--pin-dir', str(PIN_DIR), '--version', pin.version)
        require(finished.returncode == 0, f'check exited {finished.returncode}: '
                                          f'{finished.stderr}')
        lines = [line for line in finished.stdout.splitlines() if line.strip()]
        require(len(lines) == 1, f'check printed {len(lines)} lines: {lines}')

    def signer_policy() -> None:
        accepts(lambda: claude_pin.check_policy(pin))

    for label, function in (
            ('committed: version', version),
            ('committed: digest both tokens', digests),
            ('committed: check verifies the signature offline', check),
            ('committed: the pin names the reviewed signer', signer_policy)):
        run_case(label, function)


# --- loader -------------------------------------------------------------------

NON_CANONICAL_URLS = (
    ('other host', 'https://downloads.claude.ai.evil.example/claude-code-releases/'
                   f'{VERSION}/manifest.json'),
    ('http scheme', f'http://downloads.claude.ai/claude-code-releases/{VERSION}/manifest.json'),
    ('other version', 'https://downloads.claude.ai/claude-code-releases/9.9.8/manifest.json'),
    ('query', f'https://downloads.claude.ai/claude-code-releases/{VERSION}/manifest.json?x=1'),
    ('trailing slash',
     f'https://downloads.claude.ai/claude-code-releases/{VERSION}/manifest.json/'),
)


def loader_cases(template: Template) -> None:
    def happy_select() -> None:
        for token in PLATFORMS:
            finished = run_cli('select', '--pin-dir', str(template.closure),
                               '--version', VERSION, '--platform', token)
            require(finished.returncode == 0, f'select {token} exited '
                                              f'{finished.returncode}: {finished.stderr}')
            pinned = claude_pin.load(template.closure).executables[token]
            expected = f'{pinned.url} {pinned.sha256} {pinned.size}\n'
            require(finished.stdout == expected,
                    f'select {token} printed {finished.stdout!r}, want {expected!r}')

    def key_sets() -> None:
        mutations = {
            'root extra key': lambda pin: pin.update({'extra': 1}),
            'root missing key': lambda pin: pin.pop('authentication'),
            'manifest extra key': lambda pin: pin['manifest'].update({'note': 'x'}),
            'signature missing key': lambda pin: pin['signature'].pop('sha256'),
            'key extra key': lambda pin: pin['key'].update({'uid': 'x'}),
            'platform extra key': lambda pin: pin['platforms']['linux/amd64'].update(
                {'note': 1}),
            'authentication extra key': lambda pin: pin['authentication'].update(
                {'note': 1}),
        }
        for label, mutate in mutations.items():
            expect_load_refusal(template.mutate(label.replace(' ', '-'), mutate),
                                'must have exactly')

    def duplicate_key() -> None:
        directory = template.copy('duplicate-key')
        path = directory / 'pin.json'
        needle = f'"version": "{VERSION}"'
        text = path.read_text()
        require(text.count(needle) == 1,
                f'the fixture pin repeats {needle} {text.count(needle)} times')
        path.write_text(text.replace(needle, f'{needle}, {needle}', 1))
        expect_load_refusal(directory, 'duplicate JSON key')

    def numbers() -> None:
        expect_load_refusal(template.mutate(
            'float-size', lambda pin: pin['platforms']['linux/arm64'].update(
                {'size': 1.5})), 'fractional part')
        directory = template.copy('nan-size')
        document = read_pin(directory)
        document['platforms']['linux/arm64']['size'] = float('nan')
        (directory / 'pin.json').write_text(json.dumps(document, indent=2,
                                                       allow_nan=True) + '\n')
        expect_load_refusal(directory, 'is not a JSON number')

    def schema_identity() -> None:
        mutations = {
            'schema': lambda pin: pin.update({'schema': 2}),
            'name': lambda pin: pin.update({'name': 'codex'}),
            'kind': lambda pin: pin.update({'kind': 'release-archive'}),
            'method': lambda pin: pin['authentication'].update({'method': 'hash-only'}),
            'verified_on': lambda pin: pin['authentication'].update(
                {'verified_on': '2026-13-01'}),
        }
        needles = {'schema': 'schema must be', 'name': 'name must be',
                   'kind': 'kind must be', 'method': 'method must be',
                   'verified_on': 'not a calendar date'}
        for label, mutate in mutations.items():
            expect_load_refusal(template.mutate(f'{label}-wrong', mutate),
                                needles[label])

    def version_grammar() -> None:
        for bad in ('latest', '1.2', '01.2.3', '1.2.3+b', '1.2.3 ', ''):
            expect_load_refusal(template.mutate(
                'version-grammar', lambda pin, bad=bad: pin.update({'version': bad})),
                'version')

    def non_canonical_urls() -> None:
        for label, url in NON_CANONICAL_URLS:
            expect_load_refusal(template.mutate(
                'manifest-url', lambda pin, url=url: pin['manifest'].update(
                    {'url': url})), 'manifest evidence url must be')
        expect_load_refusal(template.mutate(
            'signature-path', lambda pin: pin['signature'].update(
                {'path': 'evidence/other.json'})), 'signature evidence path must be')
        expect_load_refusal(template.mutate(
            'key-url', lambda pin: pin['key'].update(
                {'url': 'https://downloads.claude.ai/keys/other.asc'})),
            'pin key url must be')
        for label, url in (
                ('other token', 'https://downloads.claude.ai/claude-code-releases/'
                                f'{VERSION}/linux-arm64/claude'),
                ('other version', 'https://downloads.claude.ai/claude-code-releases/'
                                  f'9.9.8/linux-x64/claude'),
                ('lookalike', 'https://downloads.claude.ai.evil.example/'
                              f'claude-code-releases/{VERSION}/linux-x64/claude'),
                ('query', 'https://downloads.claude.ai/claude-code-releases/'
                          f'{VERSION}/linux-x64/claude?x=1')):
            expect_load_refusal(template.mutate(
                'platform-url', lambda pin, url=url: pin['platforms']['linux/amd64']
                .update({'url': url})), 'platform url must be')

    def platform_fields() -> None:
        expect_load_refusal(template.mutate(
            'swapped-tokens', lambda pin: pin['platforms']['linux/amd64'].update(
                {'manifest_platform': 'linux-arm64'})), 'manifest_platform must be')
        expect_load_refusal(template.mutate(
            'platform-missing', lambda pin: pin['platforms'].pop('linux/arm64')),
            'pin platforms must have exactly')
        expect_load_refusal(template.mutate(
            'platform-extra', lambda pin: pin['platforms'].update(
                {'linux/riscv64': pin['platforms']['linux/amd64']})),
            'pin platforms must have exactly')
        expect_load_refusal(template.mutate(
            'fingerprint-case', lambda pin: pin['key'].update(
                {'fingerprint': pin['key']['fingerprint'].lower()})),
            'upper-case hex')

    def digests_and_sizes() -> None:
        mutations = {
            'upper-case sha': lambda pin: pin['platforms']['linux/amd64'].update(
                {'sha256': pin['platforms']['linux/amd64']['sha256'].upper()}),
            'short sha': lambda pin: pin['platforms']['linux/amd64'].update(
                {'sha256': 'abc'}),
            'size zero': lambda pin: pin['platforms']['linux/amd64'].update({'size': 0}),
            'size negative': lambda pin: pin['platforms']['linux/amd64'].update(
                {'size': -1}),
            'size bool': lambda pin: pin['platforms']['linux/amd64'].update(
                {'size': True}),
            'size too large': lambda pin: pin['platforms']['linux/amd64'].update(
                {'size': claude_pin.MAX_EXECUTABLE_BYTES + 1}),
        }
        for label, mutate in mutations.items():
            expect_load_refusal(template.mutate(label.replace(' ', '-'), mutate))

    def evidence_paths() -> None:
        for bad in ('../manifest.json', '/etc/passwd', 'evidence/../manifest.json'):
            expect_load_refusal(template.mutate(
                'evidence-path', lambda pin, bad=bad: pin['manifest'].update(
                    {'path': bad})), 'manifest evidence path must be')

    def symlinked_evidence() -> None:
        directory = template.copy('symlinked-evidence')
        evidence = directory / 'evidence'
        moved = directory / 'evidence-real'
        evidence.rename(moved)
        evidence.symlink_to(moved.name)
        requires(lambda: load_and_verify(directory), 'symlink')

    for label, function in (
            ('loader: select prints the canonical URL, digest and size',
             happy_select),
            ('loader: unknown or missing key at every level', key_sets),
            ('loader: duplicate JSON key', duplicate_key),
            ('loader: float and NaN numbers', numbers),
            ('loader: schema, name, kind, authentication', schema_identity),
            ('loader: version grammar', version_grammar),
            ('loader: non-canonical evidence and executable URLs',
             non_canonical_urls),
            ('loader: platform tokens, missing/extra platform, fingerprint case',
             platform_fields),
            ('loader: digest spelling and size bounds', digests_and_sizes),
            ('loader: evidence path traversal and absolute paths', evidence_paths),
            ('loader: symlinked evidence', symlinked_evidence)):
        run_case(label, function)


# --- evidence -----------------------------------------------------------------

def evidence_cases(template: Template) -> None:
    def flipped(relative: str, label: str) -> None:
        directory = template.copy(label)
        path = directory / relative
        raw = bytearray(path.read_bytes())
        raw[0] = raw[0] ^ 0xFF
        path.write_bytes(bytes(raw))
        boom = install_gpg_bomb()
        try:
            expect_verify_refusal(directory, 'does not match the pin digest')
        finally:
            restore_gpg(boom)

    run_case('evidence: flipped manifest byte refused without gpg',
             lambda: flipped('evidence/manifest.json', 'flip-manifest'))
    run_case('evidence: flipped signature byte refused without gpg',
             lambda: flipped('evidence/manifest.json.sig', 'flip-signature'))
    run_case('evidence: flipped key byte refused without gpg',
             lambda: flipped('claude-code-release-key.asc', 'flip-key'))


def install_gpg_bomb():
    """A gpg that fails loudly, to prove a case never reaches it."""
    original = claude_pin.gpg

    def bomb(*_arguments, **_keywords):
        raise AssertionError('gpg was called')

    claude_pin.gpg = bomb
    return original


def restore_gpg(original) -> None:
    claude_pin.gpg = original


# --- signature ----------------------------------------------------------------

def signature_cases(template: Template) -> None:
    def consistent_rewrite() -> None:
        """New manifest, new pin digests -- and the old signature over the old bytes."""
        directory = template.copy('stale-signature')
        evidence = directory / 'evidence'
        document = json.loads((evidence / 'manifest.json').read_text())
        document['platforms']['linux-x64']['checksum'] = 'f' * 64
        manifest = json.dumps(document, indent=2).encode() + b'\n'
        (evidence / 'manifest.json').write_bytes(manifest)
        pin = read_pin(directory)
        pin['manifest']['sha256'] = digest(manifest)
        pin['platforms']['linux/amd64']['sha256'] = 'f' * 64
        write_pin(directory, pin)
        expect_verify_refusal(directory, 'signature does not verify')

    def foreign_signature() -> None:
        attacker = claude_fixture.make_signer(scratch('attacker') / 'signer')
        directory = template.copy('foreign-signature')
        evidence = directory / 'evidence'
        signature = claude_fixture.sign(attacker, evidence / 'manifest.json')
        (evidence / 'manifest.json.sig').write_bytes(signature)
        pin = read_pin(directory)
        pin['signature']['sha256'] = digest(signature)
        write_pin(directory, pin)
        expect_verify_refusal(directory, 'signature does not verify')

    def substituted_key() -> None:
        attacker = claude_fixture.make_signer(scratch('attacker-key') / 'signer')
        directory = template.copy('substituted-key')
        key = attacker.key.read_bytes()
        (directory / 'claude-code-release-key.asc').write_bytes(key)
        pin = read_pin(directory)
        pin['key']['sha256'] = digest(key)
        write_pin(directory, pin)
        # The pin still names the fixture fingerprint, so the replaced key file is
        # refused before the signature is even considered.
        expect_verify_refusal(directory, 'exactly the primary')

    def two_primaries() -> None:
        other = claude_fixture.make_signer(scratch('second-key') / 'signer')
        directory = template.copy('two-primaries')
        combined = claude_fixture.two_primaries(
            directory / 'claude-code-release-key.asc.2', template.signer.key,
            other.key)
        key = combined.read_bytes()
        (directory / 'claude-code-release-key.asc').write_bytes(key)
        combined.unlink()
        pin = read_pin(directory)
        pin['key']['sha256'] = digest(key)
        write_pin(directory, pin)
        expect_verify_refusal(directory, 'exactly the primary')

    def gpg_absent() -> None:
        original = os.environ.get('PATH', '')
        os.environ['PATH'] = ''
        try:
            expect_verify_refusal(template.closure, 'gpg is required')
        finally:
            os.environ['PATH'] = original

    def policy() -> None:
        directory = template.copy('policy')
        pin = claude_pin.load(directory)
        requires(lambda: claude_pin.check_policy(pin), 'not the reviewed')

    def secret_key_material() -> None:
        """R2: the committed key file holds no secret key, named or not.

        `show-only` lists a secret key as `sec`, so a `pub`-only count would accept
        this file and the import would install the second signer.
        """
        other = claude_fixture.make_signer(scratch('secret-signer') / 'signer')
        for label, secret in (('own', template.signer.secret),
                              ('foreign', other.secret)):
            directory = template.copy(f'secret-key-{label}')
            key_path = directory / 'claude-code-release-key.asc'
            key = key_path.read_bytes() + secret.read_bytes()
            key_path.write_bytes(key)
            pin = read_pin(directory)
            pin['key']['sha256'] = digest(key)
            write_pin(directory, pin)
            expect_verify_refusal(directory, 'carries secret key material')

    def validsig_primary() -> None:
        """The only guard against a second signer in the keyring, exercised.

        Every other "other signer" vector is refused earlier (NO_PUBKEY, ERRSIG, or
        the single-primary key check), so nothing tested the VALIDSIG comparison. A
        gpg that reports a good signature under another primary is what an
        imported, unnamed signer would look like; accepting it would mean a
        manifest signed by a key the pin does not name is placed.
        """
        base = scratch('validsig-primary')
        binary = gpg_wrapper(base / 'bin', validsig_primary='F' * 40)
        original = os.environ.get('PATH', '')
        os.environ['PATH'] = f'{binary}{os.pathsep}{original}'
        try:
            expect_verify_refusal(template.closure, 'is not by the primary')
        finally:
            os.environ['PATH'] = original
        homes = logged_homes(binary)
        require(homes, 'the gpg wrapper logged no invocation')
        for home in homes:
            require(not Path(home).exists(), f'the gpg home {home} was left behind')

    for label, function in (
            ('signature: consistent rewrite with a stale signature refused',
             consistent_rewrite),
            ('signature: signature by another key refused', foreign_signature),
            ('signature: substituted key refused', substituted_key),
            ('signature: a key file with two primaries refused', two_primaries),
            ('signature: a key file carrying secret key material refused',
             secret_key_material),
            ('signature: VALIDSIG naming another primary refused',
             validsig_primary),
            ('signature: gpg absent from PATH refused', gpg_absent),
            ('signature: check_policy refuses a fixture fingerprint', policy)):
        run_case(label, function)


# --- signing subkey -----------------------------------------------------------

RECORDED_SUBKEY_LISTING = '\n'.join((
    'pub:u:255:22:E010800BADA0909D:1793422096:::u:::cSC:::::ed25519:::0:',
    'fpr:::::::::422E8241E320573700499D74E010800BADA0909D:',
    'uid:u::::1793422096::8719982D789267C30FA69B421197E3BE59AE39FF::'
    'fixture claude signer <fixture@example.invalid>::::::::::0:',
    'sub:u:255:22:3E91A41A314A9DD5:1793422096::::::s:::::ed25519::',
    'fpr:::::::::064F56EB3E7A7771378DDD673E91A41A314A9DD5:',
    ''))

# Recorded `--with-colons --import-options show-only --import` of a *secret* key,
# which gpg lists as `sec`; the same key exported publicly lists as `pub`. A
# `pub`-only primary count therefore misses secret material entirely.
RECORDED_SECRET_LISTING = '\n'.join((
    'sec:-:255:22:69798028BC96E24D:1793444834:::-:::scSC:::#::ed25519:::0:',
    'fpr:::::::::8C4BE8C8BE31EE86CC346C4D69798028BC96E24D:',
    'grp:::::::::1A279308478D521C325CBE7550354FC95EA1B9E1:',
    'uid:-::::1793444834::8719982D789267C30FA69B421197E3BE59AE39FF::'
    'fixture claude signer <fixture@example.invalid>::::::::::0:',
    ''))
RECORDED_SECRET_SUBKEY_LISTING = '\n'.join((
    'sec:-:255:22:49FE92192A02C925:1793444865:::-:::cSC:::#::ed25519:::0:',
    'fpr:::::::::DF4854592D167B257186AE7A49FE92192A02C925:',
    'grp:::::::::334F2A4915C41F236E4013E5663B7CF312C58291:',
    'uid:-::::1793444865::8719982D789267C30FA69B421197E3BE59AE39FF::'
    'fixture claude signer <fixture@example.invalid>::::::::::0:',
    'ssb:-:255:22:DFB3BE9DEA71B340:1793444866::::::s:::#::ed25519::',
    'fpr:::::::::C68152E062F6665D6BA0AD42DFB3BE9DEA71B340:',
    'grp:::::::::FCA3BE2AF82A79DC914987B6555CAA6C28C6935A:',
    ''))
# A *public* primary followed by a secret subkey record. `show-only` never prints
# this, and both recorded secret listings begin with `sec`, so they refuse on `sec`
# and never reach the `ssb` rule at all: a parser that refuses `sec` and ignores
# `ssb` passes over them untouched. This listing is the shortest vector that reaches
# the `ssb` branch on its own.
RECORDED_SECRET_SUBKEY_ONLY_LISTING = '\n'.join((
    'pub:u:255:22:E010800BADA0909D:1793422096:::u:::cSC:::::ed25519:::0:',
    'fpr:::::::::422E8241E320573700499D74E010800BADA0909D:',
    'uid:u::::1793422096::8719982D789267C30FA69B421197E3BE59AE39FF::'
    'fixture claude signer <fixture@example.invalid>::::::::::0:',
    'ssb:-:255:22:DFB3BE9DEA71B340:1793444866::::::s:::#::ed25519::',
    'fpr:::::::::C68152E062F6665D6BA0AD42DFB3BE9DEA71B340:',
    ''))


def subkey_cases() -> None:
    def parser() -> None:
        found = claude_pin.parse_primaries(RECORDED_SUBKEY_LISTING)
        require(found == ['422E8241E320573700499D74E010800BADA0909D'],
                f'parse_primaries returned {found}')
        requires(lambda: claude_pin.parse_primaries('pub:u:255:22:AABB:\n'),
                 'without an fpr')

    def secret_listing() -> None:
        """A `sec`/`ssb` record is a refusal, not a primary a `pub` count can miss."""
        requires(lambda: claude_pin.parse_primaries(RECORDED_SECRET_LISTING),
                 'carries secret key material')
        requires(lambda: claude_pin.parse_primaries(RECORDED_SECRET_SUBKEY_LISTING),
                 'carries secret key material')
        # Reaches `ssb` with no earlier `sec` record, so the `ssb` rule is exercised
        # on its own rather than only after the `sec` refusal has fired.
        requires(lambda: claude_pin.parse_primaries(
            RECORDED_SECRET_SUBKEY_ONLY_LISTING), 'carries secret key material')

    def signer() -> None:
        base = scratch('subkey')
        signer = claude_fixture.make_signer(base / 'signer', subkey=True)
        require(signer.fingerprint != signer.signing_fingerprint,
                'the fixture did not create a separate signing subkey')
        x64 = claude_fixture.elf(base / 'assets' / 'x64', 62)
        arm64 = claude_fixture.elf(base / 'assets' / 'arm64', 183)
        closure = claude_fixture.make_closure(
            base / 'closure', signer=signer, version=VERSION, amd64=x64, arm64=arm64)
        verified = load_and_verify(closure)
        require(verified.signature.primary_fingerprint == signer.fingerprint,
                f'the signature reports {verified.signature.primary_fingerprint}, '
                f'not the primary {signer.fingerprint}')
        # The same closure with the *subkey* as the pin's identity is refused: a
        # pin may name a primary only.
        pin = read_pin(closure)
        pin['key']['fingerprint'] = signer.signing_fingerprint
        write_pin(closure, pin)
        expect_verify_refusal(closure, 'exactly the primary')

    def validsig_fields() -> None:
        """Record what gpg itself says: [2] is the subkey, [-1] the primary."""
        base = scratch('validsig')
        signer = claude_fixture.make_signer(base / 'signer', subkey=True)
        data = base / 'manifest.json'
        data.write_text('{}\n')
        signature = claude_fixture.sign(signer, data)
        (base / 'manifest.json.sig').write_bytes(signature)
        with claude_fixture.short_gnupghome() as environment:
            subprocess.run(['gpg', '--batch', '--quiet', '--import',
                            str(signer.secret)], env=environment, check=True,
                           capture_output=True, text=True)
            finished = subprocess.run(
                ['gpg', '--batch', '--status-fd=1', '--verify',
                 str(base / 'manifest.json.sig'), str(data)],
                env=environment, check=True, capture_output=True, text=True)
        valid = [line.split() for line in finished.stdout.splitlines()
                 if line.startswith('[GNUPG:] VALIDSIG ')]
        require(len(valid) == 1, f'expected one VALIDSIG, found {len(valid)}')
        require(valid[0][2] == signer.signing_fingerprint,
                f'VALIDSIG [2] is {valid[0][2]}, not the subkey '
                f'{signer.signing_fingerprint}')
        require(valid[0][-1] == signer.fingerprint,
                f'VALIDSIG [-1] is {valid[0][-1]}, not the primary '
                f'{signer.fingerprint}')

    for label, function in (
            ('subkey: only the fpr after pub is a primary', parser),
            ('subkey: a sec or ssb record is refused', secret_listing),
            ('subkey: a closure signed by a signing subkey verifies as its primary',
             signer),
            ('subkey: VALIDSIG [2] is the subkey and [-1] the primary',
             validsig_fields)):
        run_case(label, function)


# --- gpg home -----------------------------------------------------------------

def gpg_wrapper(directory: Path, *, fail_import: bool = False,
                validsig_primary: str | None = None) -> Path:
    """A PATH `gpg` that logs its home, plus one optional hostile behaviour.

    `fail_import` fails a real (non-`show-only`) key import. `validsig_primary`
    rewrites the primary fingerprint in every VALIDSIG status line, which is how a
    signature by a signer the pin does not name would present itself once the
    keyring held it: the detached signature does verify, only the primary differs.
    """
    directory.mkdir(parents=True, exist_ok=True)
    real = shutil.which('gpg')
    log = directory / 'homes.log'
    script = directory / 'gpg'
    if validsig_primary is not None:
        body = f"""#!/bin/sh
printf '%s\\n' "$GNUPGHOME" >>'{log}'
for argument in "$@"; do
    if [ "$argument" = '--verify' ]; then
        '{real}' "$@" >"$GNUPGHOME/verify.out" 2>"$GNUPGHOME/verify.err"
        status=$?
        awk -v foreign='{validsig_primary}' 'BEGIN {{ OFS = " " }}
             $1 == "[GNUPG:]" && $2 == "VALIDSIG" {{ $NF = foreign }}
             {{ print }}' "$GNUPGHOME/verify.out"
        cat "$GNUPGHOME/verify.err" >&2
        exit $status
    fi
done
exec '{real}' "$@"
"""
    elif fail_import:
        body = f"""#!/bin/sh
printf '%s\\n' "$GNUPGHOME" >>'{log}'
for argument in "$@"; do
    if [ "$argument" = '--import' ]; then
        case " $* " in
            *' --import-options show-only '*) ;;
            *) echo 'gpg: cannot connect to the gpg-agent' >&2; exit 2 ;;
        esac
    fi
done
exec '{real}' "$@"
"""
    else:
        body = f"""#!/bin/sh
printf '%s\\n' "$GNUPGHOME" >>'{log}'
exec '{real}' "$@"
"""
    script.write_text(body)
    script.chmod(0o755)
    return directory


def logged_homes(directory: Path) -> list[str]:
    log = directory / 'homes.log'
    if not log.is_file():
        return []
    return [line for line in log.read_text().splitlines() if line]


def gpg_home_cases(template: Template) -> None:
    def long_tmpdir() -> None:
        base = scratch('long-tmpdir')
        long_dir = base / ('x' * 120)
        long_dir.mkdir()
        require(len(str(long_dir)) >= 120, 'the long TMPDIR is not long enough')
        binary = gpg_wrapper(base / 'bin')
        original = {key: os.environ.get(key) for key in ('PATH', 'TMPDIR')}
        saved_tempdir = tempfile.tempdir
        os.environ['PATH'] = f'{binary}{os.pathsep}{original["PATH"]}'
        os.environ['TMPDIR'] = str(long_dir)
        tempfile.tempdir = None
        try:
            # A scratch directory under the long TMPDIR is where the case lives,
            # but no gpg home may be created there.
            closure = copy_closure(template.closure, long_dir / 'source')
            verify_closure(closure)
        finally:
            os.environ['PATH'] = original['PATH'] or ''
            if original['TMPDIR'] is None:
                os.environ.pop('TMPDIR', None)
            else:
                os.environ['TMPDIR'] = original['TMPDIR']
            tempfile.tempdir = saved_tempdir
        homes = logged_homes(binary)
        require(homes, 'the gpg wrapper logged no invocation')
        for home in homes:
            require(home.startswith('/tmp/claude-pin-gpg.'),
                    f'a gpg home was {home}, not a private home under /tmp')
            require(not Path(home).exists(), f'the gpg home {home} was left behind')

    def unusable_roots() -> None:
        base = scratch('unusable-root')
        binary = gpg_wrapper(base / 'bin')
        original_path = os.environ.get('PATH', '')
        original_root = claude_pin.GPG_HOME_ROOT
        os.environ['PATH'] = f'{binary}{os.pathsep}{original_path}'
        try:
            for label, root in (
                    ('missing', base / 'does-not-exist'),
                    ('regular file', base / 'not-a-directory')):
                if label == 'regular file':
                    root.write_text('x')
                claude_pin.GPG_HOME_ROOT = root
                requires(lambda: load_and_verify(template.closure),
                         'cannot create a private gpg home under', str(root))
            long_root = base / ('y' * 60)
            long_root.mkdir()
            claude_pin.GPG_HOME_ROOT = long_root
            requires(lambda: load_and_verify(template.closure),
                     'too long for gpg-agent')
        finally:
            claude_pin.GPG_HOME_ROOT = original_root
            os.environ['PATH'] = original_path
        require(not logged_homes(binary),
                f'gpg ran despite an unusable home: {logged_homes(binary)}')

    def import_failure() -> None:
        base = scratch('import-failure')
        binary = gpg_wrapper(base / 'bin', fail_import=True)
        original_path = os.environ.get('PATH', '')
        os.environ['PATH'] = f'{binary}{os.pathsep}{original_path}'
        try:
            requires(lambda: load_and_verify(template.closure),
                     'cannot import the release key')
        finally:
            os.environ['PATH'] = original_path
        homes = logged_homes(binary)
        require(homes, 'the gpg wrapper logged no invocation')
        for home in homes:
            require(not Path(home).exists(), f'the gpg home {home} was left behind')

    for label, function in (
            ('gpg home: verification succeeds under a long ambient TMPDIR',
             long_tmpdir),
            ('gpg home: an unusable or too-long root is refused without gpg',
             unusable_roots),
            ('gpg home: a failing key import is refused before --verify',
             import_failure)):
        run_case(label, function)


# --- clock ---------------------------------------------------------------------

# The two extreme zones differ from UTC in opposite directions. Kiritimati is UTC+14,
# so its local date runs ahead of the UTC date from 10:00 UTC; Etc/GMT+12 is UTC-12,
# so its local date lags from 00:00 UTC. Every hour of the UTC day is therefore
# covered by at least one of the two, which is what makes the pair time-of-day
# independent instead of a coin flip.
CLOCK_ZONES = ('Pacific/Kiritimati', 'Etc/GMT+12')


def behind_utc() -> datetime.datetime:
    """A simulated host wall clock one day behind the UTC date, naive.

    The shift is measured from UTC, not from whatever local date the host running
    this suite happens to have, so the simulated local date is always exactly the
    UTC date minus one.
    """
    return (datetime.datetime.now(datetime.timezone.utc)
            - datetime.timedelta(days=1)).replace(tzinfo=None)


class WestModule:
    """The reader's `datetime` as seen by a host whose local date lags UTC by a day.

    `verified_on` used to be compared against the host's *local* date, so the local
    accessors are what an unfixed checker reaches for -- and every one of them moves
    here: `date.today()`, `datetime.now()` with no zone and `datetime.today()`.
    Overriding only some of them leaves the rest inherited and real, and a checker
    written against one of the unshifted spellings then escapes the simulation while
    the case still claims to guard it. The UTC accessors, `now(tz)` and `utcnow()`,
    stay real: UTC is the same instant everywhere, and shifting those would break
    `utc_today()` itself rather than simulate a zone.
    """

    timezone = datetime.timezone

    class date(datetime.date):  # noqa: N801 - stands in for datetime.date
        @classmethod
        def today(cls) -> datetime.date:
            return behind_utc().date()

    class datetime(datetime.datetime):  # noqa: N801
        @classmethod
        def now(cls, tz=None):
            if tz is None:
                return behind_utc()
            return datetime.datetime.now(tz)

        @classmethod
        def today(cls):
            return behind_utc()

        @classmethod
        def utcnow(cls):
            return datetime.datetime.now(datetime.timezone.utc).replace(tzinfo=None)


def at_a_stable_utc_date(call) -> None:
    """Run `call(today)` unless the UTC date rolls over mid-case, then retry once.

    Every case here derives a pin date from "the UTC date right now". A case that
    straddles UTC midnight would compare against a date the process no longer agrees
    with, so a genuine rollover is retried rather than reported -- but only when the
    date really moved, never to excuse a mutant.
    """
    for _ in range(2):
        today = claude_pin.utc_today()
        try:
            call(today)
        except AssertionError:
            if claude_pin.utc_today() == today:
                raise
            continue
        if claude_pin.utc_today() == today:
            return
    raise AssertionError('the UTC date rolled over twice while this case ran')


def clock_cases(template: Template) -> None:
    def reference_date() -> None:
        """The reference date is UTC, not the host's local date.

        One of the two zones always has a local date other than the UTC date, so the
        vector is not time-of-day dependent.
        """
        import time

        original = os.environ.get('TZ')
        try:
            for zone in CLOCK_ZONES:
                os.environ['TZ'] = zone
                time.tzset()
                local = datetime.date.today()
                utc = datetime.datetime.now(datetime.timezone.utc).date()
                if local == utc:
                    continue
                found = claude_pin.utc_today()
                require(found == utc,
                        f'under TZ={zone} the reference date is {found}, not the UTC '
                        f'date {utc} (the local date is {local})')
                return
            raise AssertionError('no candidate zone differs from the UTC date')
        finally:
            if original is None:
                os.environ.pop('TZ', None)
            else:
                os.environ['TZ'] = original
            time.tzset()

    def west_checker() -> None:
        """A pin stamped with the UTC date verifies where the local date lags.

        The writer east of UTC stamped its own local date and the reader compared
        against its own local date, so a bump merged early in the maintainer's day
        refused every builder until the two dates lined up. Here the lagging local
        date is simulated rather than waited for, so the case runs at every hour, and
        the simulation is asserted to move every local accessor the reader could
        reach for. A genuinely future date is still refused.
        """

        def case(today: datetime.date) -> None:
            behind = today - datetime.timedelta(days=1)
            for spelling, found in (
                    ('date.today()', WestModule.date.today()),
                    ('datetime.now()', WestModule.datetime.now().date()),
                    ('datetime.today()', WestModule.datetime.today().date())):
                require(found == behind,
                        f'the simulated host reports {spelling} as {found}, not the '
                        f'lagging local date {behind}')
            for spelling, found in (
                    ('datetime.now(timezone.utc)',
                     WestModule.datetime.now(datetime.timezone.utc).date()),
                    ('datetime.utcnow()', WestModule.datetime.utcnow().date())):
                require(found == today,
                        f'the simulated host reports {spelling} as {found}, not the '
                        f'UTC date {today}, so the case no longer simulates a '
                        'zone -- it moves the UTC clock instead')

            def restamp(pin: dict) -> None:
                pin['authentication']['verified_on'] = today.isoformat()

            directory = template.mutate('west-checker', restamp)
            original = claude_pin.datetime
            claude_pin.datetime = WestModule
            try:
                verify_closure(directory)
                document = read_pin(directory)
                document['authentication']['verified_on'] = (
                    today + datetime.timedelta(days=1)).isoformat()
                write_pin(directory, document)
                expect_verify_refusal(directory, 'is in the future')
            finally:
                claude_pin.datetime = original

        at_a_stable_utc_date(case)

    def displaced_cli() -> None:
        """The real CLI accepts UTC today and refuses UTC tomorrow, in both zones.

        This is the future-date half of the regression, through real processes: a
        checker that compared with a local date cannot satisfy both assertions in
        both zones at once. Where a zone's local date is a day ahead of UTC it accepts
        the pin dated UTC tomorrow; where it is a day behind it refuses the pin dated
        UTC today. At every hour of the UTC day at least one zone is in one of those
        states, so no wall clock lets that mutant through.
        """

        def case(today: datetime.date) -> None:
            for zone in CLOCK_ZONES:
                for name, day, accepted in (
                        ('utc-today', today, True),
                        ('utc-tomorrow', today + datetime.timedelta(days=1), False)):
                    directory = template.mutate(
                        f'zone-{zone.replace("/", "-")}-{name}',
                        lambda pin, day=day: pin['authentication'].update(
                            {'verified_on': day.isoformat()}))
                    finished = run_cli_in(zone, 'check', '--pin-dir', str(directory),
                                         '--version', VERSION)
                    if accepted:
                        require(finished.returncode == 0,
                                f'under TZ={zone} a pin dated the UTC date {day} '
                                f'exited {finished.returncode}: '
                                f'{finished.stderr.strip()}')
                    else:
                        require(finished.returncode != 0,
                                f'under TZ={zone} a pin dated {day} was accepted')
                        require('is in the future' in finished.stderr,
                                f'under TZ={zone} the refusal of {day} did not name '
                                f'the future: {finished.stderr.strip()}')

        at_a_stable_utc_date(case)

    def displaced_writer() -> None:
        """The real writer stamps the UTC date, and the real checker accepts it.

        This is the end-to-end half of the regression: `pin-claude.py` runs on a
        maintainer's host, east or west of UTC, and the bytes it publishes are then
        checked on builders elsewhere. Both sides are real processes here -- only the
        upstream bytes are served from the fixture -- and the pair is run in each
        extreme zone. The stamp itself is asserted on its own, so a writer that falls
        back to the host's local date is caught in whichever zone's date differs, and
        the published closure is then checked from both zones, so a stamp ahead of
        UTC cannot pass just because it was written where it looked like today.
        """

        def case(today: datetime.date) -> None:
            for zone in CLOCK_ZONES:
                fixture_repo = FixtureRepo(
                    scratch(f'zone-writer-{zone.replace("/", "-")}'), template,
                    adopted=False, dockerfile_version='0.0.0')
                finished = fixture_repo.run('--version', VERSION, extra={'TZ': zone})
                require(finished.returncode == 0,
                        f'the writer under TZ={zone} exited {finished.returncode}: '
                        f'{finished.stderr.strip()}')
                document = json.loads(fixture_repo.pin_path().read_text())
                stamped = document['authentication']['verified_on']
                require(stamped == today.isoformat(),
                        f'under TZ={zone} the writer stamped verified_on {stamped}, '
                        f'not the UTC date {today.isoformat()}')
                pin_directory = fixture_repo.root / 'images' / 'tools' / 'claude'
                for reader_zone in CLOCK_ZONES:
                    checked = run_cli_in(reader_zone, 'check', '--pin-dir',
                                         str(pin_directory), '--version', VERSION)
                    require(checked.returncode == 0,
                            f'the pin written under TZ={zone} exited '
                            f'{checked.returncode} when checked under '
                            f'TZ={reader_zone}: {checked.stderr.strip()}')

        at_a_stable_utc_date(case)

    def shifted_zones() -> None:
        """The same closure verifies with the ambient clock far east and far west."""
        for zone in CLOCK_ZONES:
            finished = run_cli_in(zone, 'version', '--pin-dir', str(template.closure))
            require(finished.returncode == 0,
                    f'under TZ={zone} version exited {finished.returncode}: '
                    f'{finished.stderr.strip()}')
            require(finished.stdout.strip() == VERSION,
                    f'under TZ={zone} version printed {finished.stdout!r}')

    for label, function in (
            ('clock: the reference date is the UTC date, not the local date',
             reference_date),
            ('clock: a far-east pin verifies on a far-west host, still refusing a '
             'future date', west_checker),
            ('clock: the real CLI accepts UTC today and refuses UTC tomorrow in '
             'either extreme zone', displaced_cli),
            ('clock: the real writer stamps the UTC date and the real checker '
             'accepts it from either zone', displaced_writer),
            ('clock: the closure verifies under a far-east and a far-west TZ',
             shifted_zones)):
        run_case(label, function)


# --- manifest agreement -------------------------------------------------------

def agreement_cases(template: Template) -> None:
    def manifest_version() -> None:
        directory = template.resign('manifest-version',
                                    lambda doc: doc.update({'version': '9.9.8'}))
        expect_verify_refusal(directory, 'signed manifest names 9.9.8')

    def manifest_checksum() -> None:
        directory = template.resign(
            'manifest-checksum',
            lambda doc: doc['platforms']['linux-x64'].update({'checksum': 'a' * 64}))
        expect_verify_refusal(directory, 'does not carry the pinned linux-x64')

    def manifest_size() -> None:
        directory = template.resign(
            'manifest-size',
            lambda doc: doc['platforms']['linux-x64'].update({'size': 4097}))
        expect_verify_refusal(directory, 'does not carry the pinned linux-x64')

    def manifest_binary() -> None:
        directory = template.resign(
            'manifest-binary',
            lambda doc: doc['platforms']['linux-x64'].update({'binary': 'claude.zst'}))
        expect_verify_refusal(directory, 'binary is')

    def manifest_missing_platform() -> None:
        directory = template.resign(
            'manifest-missing', lambda doc: doc['platforms'].pop('linux-x64'))
        expect_verify_refusal(directory, 'has no linux-x64 platform')

    def other_version() -> None:
        expect_verify_refusal(template.closure, 'not the pinned Claude Code version',
                              version='9.9.8')

    def musl_token() -> None:
        verified = load_and_verify(template.closure)
        requires(lambda: claude_pin.executable(verified, 'linux-x64-musl'),
                 'not a maintained Claude Code platform')

    for label, function in (
            ('agreement: manifest version differs from the pin', manifest_version),
            ('agreement: manifest checksum differs from the pin', manifest_checksum),
            ('agreement: manifest size differs from the pin', manifest_size),
            ('agreement: manifest binary is not claude', manifest_binary),
            ('agreement: manifest missing a maintained platform',
             manifest_missing_platform),
            ('agreement: an unpinned version refused', other_version),
            ('agreement: a musl token refused', musl_token)):
        run_case(label, function)


# --- preflight ----------------------------------------------------------------

def preflight_cases(template: Template) -> None:
    verified = load_and_verify(template.closure)

    def good() -> None:
        accepts(lambda: claude_pin.preflight(verified, 'linux-x64', template.x64))
        accepts(lambda: claude_pin.preflight(verified, 'linux-arm64', template.arm64))

    def digest_mismatch() -> None:
        other = claude_fixture.elf(scratch('preflight-hash') / 'blob', 62, size=8192)
        requires(lambda: claude_pin.preflight(verified, 'linux-x64', other),
                 'sha256')

    def size_mismatch() -> None:
        directory = template.resign(
            'preflight-size',
            lambda doc: doc['platforms']['linux-x64'].update({'size': 4097}))
        pin = read_pin(directory)
        pin['platforms']['linux/amd64']['size'] = 4097
        write_pin(directory, pin)
        other = load_and_verify(directory)
        requires(lambda: claude_pin.preflight(other, 'linux-x64', template.x64),
                 'bytes, but the pin records 4097')

    def not_elf() -> None:
        path = scratch('preflight-script') / 'claude'
        path.write_text('#!/bin/sh\n# not an ELF object, but a valid closure payload\n'
                        'exit 0\n')
        # The closure is signed over exactly these bytes, so the SHA-256, the size
        # and the pin all agree; only the header says this is not an executable.
        directory = claude_fixture.make_closure(
            scratch('preflight-script-closure') / 'source', signer=template.signer,
            version=VERSION, amd64=path, arm64=path)
        signed = load_and_verify(directory)
        requires(lambda: claude_pin.preflight(signed, 'linux-x64', path),
                 'not an ELF object')

    def elf_class() -> None:
        base = scratch('preflight-class')
        path = claude_fixture.elf(base / 'blob', 62, elf_class=1)
        directory = claude_fixture.make_closure(base / 'source',
                                                signer=template.signer, version=VERSION,
                                                amd64=path, arm64=path)
        signed = load_and_verify(directory)
        requires(lambda: claude_pin.preflight(signed, 'linux-x64', path),
                 'not a 64-bit ELF object')

    def big_endian() -> None:
        base = scratch('preflight-endian')
        path = claude_fixture.elf(base / 'blob', 62, endian=2)
        directory = claude_fixture.make_closure(base / 'source',
                                                signer=template.signer, version=VERSION,
                                                amd64=path, arm64=path)
        signed = load_and_verify(directory)
        requires(lambda: claude_pin.preflight(signed, 'linux-x64', path),
                 'not a little-endian ELF object')

    def wrong_machine_consistent_pin() -> None:
        base = scratch('preflight-machine')
        directory = claude_fixture.make_closure(
            base / 'source', signer=template.signer, version=VERSION,
            amd64=template.arm64, arm64=template.arm64)
        signed = load_and_verify(directory)
        requires(lambda: claude_pin.preflight(signed, 'linux-x64', template.arm64),
                 'targets ELF machine 183, not 62')

    def symlink() -> None:
        base = scratch('preflight-symlink')
        link = base / 'claude'
        link.symlink_to(template.x64)
        requires(lambda: claude_pin.preflight(verified, 'linux-x64', link),
                 'not a regular file')

    for label, function in (
            ('preflight: both maintained ELF blobs accepted', good),
            ('preflight: a different digest refused', digest_mismatch),
            ('preflight: a different size refused', size_mismatch),
            ('preflight: a hash-consistent shell script refused', not_elf),
            ('preflight: ELFCLASS32 refused', elf_class),
            ('preflight: big-endian ELF refused', big_endian),
            ('preflight: the other architecture under a consistent pin refused',
             wrong_machine_consistent_pin),
            ('preflight: a symlinked file refused', symlink)):
        run_case(label, function)


# --- installed layout ---------------------------------------------------------

def install_layout(directory: Path, *, payload: bytes | None = None,
                   mode: int = 0o755, launcher: str | None = None,
                   extra_versions: tuple[str, ...] = (),
                   payload_mode: int | None = None) -> Path:
    """A prefix in the native installer's shape, owned by this process."""
    prefix = directory / 'prefix'
    versions = prefix / '.local/share/claude/versions'
    versions.mkdir(parents=True)
    (prefix / '.local/bin').mkdir(parents=True)
    body = payload if payload is not None else b'#!/bin/sh\nexit 0\n'
    installed = versions / VERSION
    installed.write_bytes(body)
    installed.chmod(payload_mode if payload_mode is not None else mode)
    for extra in extra_versions:
        (versions / extra).write_bytes(b'x')
    target = launcher if launcher is not None else str(installed)
    (prefix / '.local/bin/claude').symlink_to(target)
    write_native_state(prefix)
    return prefix


def write_native_state(prefix: Path, *, install_method: object = 'native',
                       auto_updates: object = False) -> None:
    document = {'installMethod': install_method, 'autoUpdates': auto_updates,
                'firstStartVersion': VERSION, 'migrationVersion': 14}
    (prefix / '.claude.json').write_text(json.dumps(document, indent=2) + '\n')


def installed_payload(template: Template) -> bytes:
    return template.x64.read_bytes()


def installed_cases(template: Template) -> None:
    # The expected owner is the one this process can actually create: the suite
    # cannot produce a root-owned file, which is why the hook has
    # AGENT_VM_CLAUDE_EXPECTED_OWNER (same rule as verify-agy.sh).
    probe = scratch('installed-owner-probe') / 'probe'
    probe.write_bytes(b'')
    owner = (probe.stat().st_uid, probe.stat().st_gid)
    payload = installed_payload(template)
    verified = claude_pin.verify(claude_pin.load(template.closure), VERSION)

    def good() -> None:
        prefix = install_layout(scratch('installed-good'), payload=payload)
        accepts(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                   owner))

    def payload_bytes() -> None:
        prefix = install_layout(scratch('installed-bytes'), payload=payload + b'\n')
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'sha256')

    def modes() -> None:
        for mode in (0o775, 0o644):
            prefix = install_layout(scratch('installed-mode'), payload=payload,
                                    payload_mode=mode)
            requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                        owner), 'not 0755')

    def owner_mismatch() -> None:
        prefix = install_layout(scratch('installed-owner'), payload=payload)
        other = (owner[0] + 1, owner[1] + 1)
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    other), 'is owned')

    def launchers() -> None:
        prefix = install_layout(scratch('installed-relative'), payload=payload,
                                launcher=f'.local/share/claude/versions/{VERSION}')
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'points at')
        prefix = install_layout(scratch('installed-other'), payload=payload,
                                launcher=f'/opt/agent/.local/share/claude/versions/{VERSION}')
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'points at')
        prefix = install_layout(scratch('installed-regular'), payload=payload)
        launcher = prefix / '.local/bin/claude'
        launcher.unlink()
        launcher.write_bytes(b'#!/bin/sh\n')
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'not a symlink')
        prefix = install_layout(scratch('installed-dangling'), payload=payload,
                                launcher='/nonexistent/claude')
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'points at')

    def versions_entries() -> None:
        prefix = install_layout(scratch('installed-extra'), payload=payload,
                                extra_versions=('9.9.9-old',))
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'holds')
        prefix = install_layout(scratch('installed-missing'), payload=payload)
        shutil.rmtree(prefix / '.local/share/claude/versions')
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'is missing')
        prefix = install_layout(scratch('installed-symlink'), payload=payload)
        installed = prefix / f'.local/share/claude/versions/{VERSION}'
        real = prefix / 'real-payload'
        installed.rename(real)
        installed.symlink_to(real)
        requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                    owner), 'not a regular file')

    def writable_directories() -> None:
        for relative, needle in (
                ('.local/bin', '.local/bin'),
                ('.local/share/claude', '.local/share/claude'),
                ('.local/share/claude/versions', '.local/share/claude/versions')):
            prefix = install_layout(scratch('installed-writable'), payload=payload)
            (prefix / relative).chmod(0o775 if relative != '.local/share/claude'
                                      else 0o757)
            requires(lambda: claude_pin.check_installed(verified, 'linux-x64', prefix,
                                                        owner), needle,
                     'group- or world-writable')

    for label, function in (
            ('installed: the native layout is accepted', good),
            ('installed: a payload whose bytes differ refused', payload_bytes),
            ('installed: payload mode 0775 and 0644 refused', modes),
            ('installed: a different owner refused', owner_mismatch),
            ('installed: relative, other-target, regular and dangling launchers',
             launchers),
            ('installed: extra or missing versions entries and a symlinked payload',
             versions_entries),
            ('installed: each group/other-writable directory refused by name',
             writable_directories)):
        run_case(label, function)


# --- native state -------------------------------------------------------------

def native_state_cases(template: Template) -> None:
    payload = installed_payload(template)

    def good() -> None:
        prefix = install_layout(scratch('state-good'), payload=payload)
        accepts(lambda: claude_pin.check_native_state(prefix))

    def auto_updates() -> None:
        for value in (True, 'false'):
            prefix = install_layout(scratch('state-auto'), payload=payload)
            write_native_state(prefix, auto_updates=value)
            requires(lambda: claude_pin.check_native_state(prefix),
                     'autoUpdates')
        prefix = install_layout(scratch('state-auto-missing'), payload=payload)
        (prefix / '.claude.json').write_text(json.dumps({'installMethod': 'native'}))
        requires(lambda: claude_pin.check_native_state(prefix), 'autoUpdates')

    def install_method() -> None:
        for value in ('npm', None):
            prefix = install_layout(scratch('state-method'), payload=payload)
            document = {'installMethod': value, 'autoUpdates': False}
            (prefix / '.claude.json').write_text(json.dumps(document))
            requires(lambda: claude_pin.check_native_state(prefix), 'installMethod')

    def duplicate_key() -> None:
        prefix = install_layout(scratch('state-duplicate'), payload=payload)
        (prefix / '.claude.json').write_text(
            '{"installMethod": "native", "autoUpdates": false, '
            '"autoUpdates": true}\n')
        requires(lambda: claude_pin.check_native_state(prefix), 'duplicate JSON key')

    def symlink_state() -> None:
        prefix = install_layout(scratch('state-symlink'), payload=payload)
        real = prefix / 'real.json'
        (prefix / '.claude.json').rename(real)
        (prefix / '.claude.json').symlink_to(real)
        requires(lambda: claude_pin.check_native_state(prefix),
                 'not a regular file')

    def non_object() -> None:
        prefix = install_layout(scratch('state-array'), payload=payload)
        (prefix / '.claude.json').write_text('[]\n')
        requires(lambda: claude_pin.check_native_state(prefix), 'not a JSON object')

    def leftovers() -> None:
        for relative, needle in (('.claude/downloads', '.claude/downloads'),
                                 ('.cache/claude/staging', '.cache/claude/staging')):
            prefix = install_layout(scratch('state-leftover'), payload=payload)
            (prefix / relative).mkdir(parents=True)
            (prefix / relative / 'claude-9.9.9').write_bytes(b'x')
            requires(lambda: claude_pin.check_native_state(prefix), needle,
                     'not empty')
            empty = install_layout(scratch('state-empty'), payload=payload)
            (empty / relative).mkdir(parents=True)
            accepts(lambda: claude_pin.check_native_state(empty))

    for label, function in (
            ('native state: the offline install state is accepted', good),
            ('native state: autoUpdates true, "false" or missing refused',
             auto_updates),
            ('native state: installMethod npm or missing refused', install_method),
            ('native state: a duplicate JSON key refused', duplicate_key),
            ('native state: a symlinked .claude.json refused', symlink_state),
            ('native state: a non-object .claude.json refused', non_object),
            ('native state: a downloaded or staged file refused', leftovers)):
        run_case(label, function)


# --- maintenance writer -------------------------------------------------------

DOCKERFILE_ARG = 'ARG AGENT_VERSION_CLAUDE='


def proposal_for(base: Path, template: 'Template', version: str):
    """A consistent closure for one release, with its executable files."""
    x64 = claude_fixture.elf(base / 'exec' / 'linux-x64', 62,
                             marker=version.encode())
    arm64 = claude_fixture.elf(base / 'exec' / 'linux-arm64', 183, size=8192,
                               marker=version.encode())
    closure = claude_fixture.make_closure(base / 'closure', signer=template.signer,
                                          version=version, amd64=x64, arm64=arm64)
    return closure, {'linux-x64': x64, 'linux-arm64': arm64}


class FixtureRepo:
    """A repository whose Claude closure, writer and served bytes this suite owns."""

    def __init__(self, base: Path, template: 'Template', *, version: str = VERSION,
                 adopted: bool = True, dockerfile_version: str = VERSION) -> None:
        self.base = base
        self.root = base / 'repo'
        self.assets = base / 'assets'
        self.signer = template.signer
        self.version = version
        self.closure, self.executables = proposal_for(base, template, version)
        pin_dir = self.root / 'images' / 'tools' / 'claude'
        pin_dir.mkdir(parents=True)
        shutil.copyfile(self.closure / 'pin.json', pin_dir / 'pin.json')
        shutil.copytree(self.closure / 'evidence', pin_dir / 'evidence')
        shutil.copyfile(self.closure / 'claude-code-release-key.asc',
                        pin_dir / 'claude-code-release-key.asc')
        shutil.copyfile(self.closure / 'claude_pin.py', pin_dir / 'claude_pin.py')
        dockerfile = (ROOT / 'images' / 'standard' / 'Dockerfile').read_text()
        # The fixture rewrites whatever exact Claude ARG the committed Dockerfile
        # carries into its own synthetic version, so a reviewed bump does not
        # leave this suite pinned to the release it last saw.
        lines = dockerfile.splitlines(keepends=True)
        matches = [index for index, line in enumerate(lines)
                   if line.startswith(DOCKERFILE_ARG)]
        if len(matches) != 1:
            raise AssertionError('the committed Dockerfile must carry exactly one '
                                 f'{DOCKERFILE_ARG}: {matches}')
        lines[matches[0]] = f'{DOCKERFILE_ARG}{dockerfile_version}\n'
        dockerfile = ''.join(lines)
        (self.root / 'images' / 'standard').mkdir(parents=True)
        (self.root / 'images' / 'standard' / 'Dockerfile').write_text(dockerfile)
        (self.root / 'script' / 'dev').mkdir(parents=True)
        for module in ('pin-artifact.py', 'pin-codex.py'):
            shutil.copyfile(ROOT / 'script' / 'dev' / module,
                            self.root / 'script' / 'dev' / module)
        shutil.copyfile(WRITER_ADAPTER,
                        self.root / 'script' / 'dev' / 'pin-claude.py')
        (self.root / 'images' / 'artifacts').mkdir(parents=True)
        shutil.copyfile(ROOT / 'images' / 'artifacts' / 'artifact_pin.py',
                        self.root / 'images' / 'artifacts' / 'artifact_pin.py')
        if not adopted:
            (pin_dir / 'pin.json').unlink()
            shutil.rmtree(pin_dir / 'evidence')
        self.serve()

    def serve(self, closure: Path | None = None,
              executables: Mapping[str, Path] | None = None, *,
              version: str | None = None) -> None:
        closure = closure or self.closure
        executables = executables or self.executables
        version = version or self.version
        if self.assets.exists():
            shutil.rmtree(self.assets)
        self.assets.mkdir(parents=True)
        shutil.copyfile(closure / 'evidence/manifest.json',
                        self.assets / f'{version}-manifest.json')
        shutil.copyfile(closure / 'evidence/manifest.json.sig',
                        self.assets / f'{version}-manifest.json.sig')
        shutil.copyfile(closure / 'claude-code-release-key.asc',
                        self.assets / 'keys-claude-code.asc')
        for token, path in executables.items():
            shutil.copyfile(path, self.assets / f'{token}-claude')

    def snapshot(self) -> dict[str, bytes]:
        return {str(path.relative_to(self.root)): path.read_bytes()
                for path in sorted(self.root.rglob('*')) if path.is_file()}

    def environment(self, extra: Mapping[str, str] | None = None) -> dict:
        # Every case here stands in for a maintainer host: the writer refuses
        # `GITHUB_ACTIONS=true` by design, and the run this suite is normally part of
        # (contracts.sh) happens on a runner, so inheriting the runner's own marker
        # would make the write-mode cases fail for the right reason and hide the
        # behaviour they mean to test. The CI refusal has its own case below, which
        # sets the marker explicitly.
        environment = {key: value for key, value in os.environ.items()
                       if not key.startswith('FIXTURE_CLAUDE_')
                       and key != 'GITHUB_ACTIONS'}
        environment.update({
            'FIXTURE_CLAUDE_WRITER': str(ROOT / 'script' / 'dev' / 'pin-claude.py'),
            'FIXTURE_CLAUDE_ASSETS': str(self.assets),
            'FIXTURE_CLAUDE_FINGERPRINT': self.signer.fingerprint,
        })
        environment.update(extra or {})
        return environment

    def run(self, *arguments: str, extra: Mapping[str, str] | None = None,
            timeout: int = 600) -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, str(self.root / 'script' / 'dev' / 'pin-claude.py'),
             *arguments, '--repo-root', str(self.root)],
            capture_output=True, text=True, env=self.environment(extra),
            timeout=timeout, check=False)

    # --- convenience views ---------------------------------------------------

    def pin_path(self) -> Path:
        return self.root / 'images' / 'tools' / 'claude' / 'pin.json'

    def evidence(self, name: str) -> Path:
        return self.root / 'images' / 'tools' / 'claude' / 'evidence' / name

    def dockerfile(self) -> str:
        return (self.root / 'images' / 'standard' / 'Dockerfile').read_text()


def temporary_scratch() -> list[Path]:
    """The publisher and writer scratch directories a run creates under TMPDIR."""
    root = Path(os.environ.get('TMPDIR', '/tmp'))
    found: list[Path] = []
    for prefix in ('pin-artifact.', 'pin-claude.work.'):
        found.extend(root.glob(f'{prefix}*'))
    return found


def _published_files(stdout: str) -> list[str]:
    for line in stdout.splitlines():
        if line.startswith('pin-claude: published claude '):
            return sorted(line.split(': ', 2)[2].split(', '))
    return []


def retained_directories(stderr: str) -> list[Path]:
    found = []
    for line in stderr.splitlines():
        for marker in ('recovery material retained at ', 'owner scratch retained at '):
            if marker in line:
                found.append(Path(line.split(marker, 1)[1].strip()))
    return found


def writer_cases(template: 'Template') -> None:
    def repo(label: str, **keywords) -> FixtureRepo:
        return FixtureRepo(scratch(label), template, **keywords)

    def first_adoption() -> None:
        fixture_repo = repo('writer-first', adopted=False, dockerfile_version='0.0.0')
        finished = fixture_repo.run('--version', VERSION)
        require(finished.returncode == 0, f'exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        require(f'claude payload delta: (none) -> {VERSION}' in finished.stdout,
                f'the delta does not name a first adoption:\n{finished.stdout}')
        published = _published_files(finished.stdout)
        require(published == sorted([
            'images/standard/Dockerfile',
            'images/tools/claude/pin.json',
            'images/tools/claude/evidence/manifest.json',
            'images/tools/claude/evidence/manifest.json.sig']),
            f'published files are {published}')
        document = json.loads(fixture_repo.pin_path().read_text())
        require(document['version'] == VERSION,
                f'the pin names {document["version"]}')
        require(document['key']['fingerprint'] == template.signer.fingerprint,
                f'the pin names {document["key"]["fingerprint"]}')
        for platform, token in (('linux/amd64', 'linux-x64'),
                                ('linux/arm64', 'linux-arm64')):
            raw = fixture_repo.executables[token].read_bytes()
            require(document['platforms'][platform]['sha256'] == digest(raw),
                    f'{platform} digest is not the served executable')
            require(document['platforms'][platform]['size'] == len(raw),
                    f'{platform} size is not the served executable')
            require(document['platforms'][platform]['url'].endswith(
                f'/{VERSION}/{token}/claude'), f'{platform} URL is not canonical')
        dockerfile = fixture_repo.dockerfile()
        require(f'{DOCKERFILE_ARG}{VERSION}' in dockerfile,
                'the published Dockerfile does not carry the new ARG')
        require(f'{DOCKERFILE_ARG}0.0.0' not in dockerfile,
                'the published Dockerfile still carries the old ARG')

        before = fixture_repo.snapshot()
        again = fixture_repo.run('--version', VERSION)
        require(again.returncode == 0, f'repeat exit {again.returncode}: '
                                       f'{again.stderr.strip()}')
        require('unchanged' in again.stdout,
                f'the repeat did not report unchanged:\n{again.stdout}')
        require(fixture_repo.snapshot() == before,
                'a repeat run changed a committed byte')

    def bump() -> None:
        new_version = '9.9.10'
        fixture_repo = repo('writer-bump')
        new_closure, new_executables = proposal_for(fixture_repo.base / 'new',
                                                    template, new_version)
        old = {'linux-x64': digest(fixture_repo.executables['linux-x64'].read_bytes()),
               'linux-arm64': digest(fixture_repo.executables['linux-arm64'].read_bytes())}
        fixture_repo.serve(new_closure, new_executables, version=new_version)
        finished = fixture_repo.run('--version', new_version)
        require(finished.returncode == 0, f'exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        require(f'claude payload delta: {VERSION} -> {new_version}' in finished.stdout,
                f'the delta does not name the bump:\n{finished.stdout}')
        for platform, token in (('linux/amd64', 'linux-x64'),
                                ('linux/arm64', 'linux-arm64')):
            fresh = digest(new_executables[token].read_bytes())
            require(f'{old[token]}' in finished.stdout and fresh in finished.stdout,
                    f'the delta does not show {platform} old and new digests')
        document = json.loads(fixture_repo.pin_path().read_text())
        require(document['version'] == new_version,
                f'the pin names {document["version"]}')
        require(f'{DOCKERFILE_ARG}{new_version}' in fixture_repo.dockerfile(),
                'the published Dockerfile does not carry the new ARG')

    def same_release() -> None:
        def changed_manifest(fixture_repo: FixtureRepo, base: Path) -> None:
            mutant = copy_closure(fixture_repo.closure, base / 'mutant')

            def add_note(document: dict) -> None:
                document['note'] = 'upstream re-rendered'

            rewrite_manifest(mutant, template.signer, add_note)
            fixture_repo.serve(mutant, fixture_repo.executables)

        def changed_signature(fixture_repo: FixtureRepo, base: Path) -> None:
            mutant = copy_closure(fixture_repo.closure, base / 'mutant')
            import time
            signature = claude_fixture.sign(
                template.signer, mutant / 'evidence/manifest.json',
                faked_time=int(time.time()) + 3600)
            (mutant / 'evidence/manifest.json.sig').write_bytes(signature)
            pin = read_pin(mutant)
            pin['signature']['sha256'] = digest(signature)
            write_pin(mutant, pin)
            fixture_repo.serve(mutant, fixture_repo.executables)

        def changed_executable(fixture_repo: FixtureRepo, base: Path) -> None:
            replaced = {'linux-x64': base / 'replaced-x64'}
            replaced['linux-x64'].write_bytes(
                fixture_repo.executables['linux-x64'].read_bytes() + b'\x00')
            fixture_repo.serve(fixture_repo.closure,
                               {**fixture_repo.executables, **replaced})

        mutations = {
            'manifest': (changed_manifest, 'the signed manifest changed for the '
                                           'already-pinned'),
            'signature': (changed_signature, 'the detached signature changed for '
                                             'the already-pinned'),
            'executable': (changed_executable, 'but the signed manifest records'),
        }
        for label, (mutate, needle) in mutations.items():
            fixture_repo = repo(f'writer-same-release-{label}')
            base = scratch(f'writer-same-release-mutant-{label}')
            mutate(fixture_repo, base)
            before = fixture_repo.snapshot()
            finished = fixture_repo.run('--version', VERSION)
            require(finished.returncode == 1, f'{label}: exit {finished.returncode}, '
                                              f'want 1: {finished.stderr.strip()}')
            require(needle in finished.stderr,
                    f'{label}: refusal did not mention {needle!r}: '
                    f'{finished.stderr.strip()}')
            require(fixture_repo.snapshot() == before,
                    f'{label}: a refused run changed a committed byte')

    def failures() -> None:
        def published_key(fixture_repo: FixtureRepo, base: Path) -> None:
            attacker = claude_fixture.make_signer(base / 'attacker')
            shutil.copyfile(attacker.key,
                            fixture_repo.assets / 'keys-claude-code.asc')

        def other_signer(fixture_repo: FixtureRepo, base: Path) -> None:
            attacker = claude_fixture.make_signer(base / 'attacker')
            signature = claude_fixture.sign(attacker,
                                            fixture_repo.closure / 'evidence/manifest.json')
            (fixture_repo.assets / f'{VERSION}-manifest.json.sig').write_bytes(signature)

        def manifest_version(fixture_repo: FixtureRepo, base: Path) -> None:
            proposal, executables = proposal_for(base / 'proposal', template,
                                                 '9.9.10')
            fixture_repo.serve(proposal, executables, version=VERSION)

        def size_only(fixture_repo: FixtureRepo, base: Path) -> None:
            def add_one(document: dict) -> None:
                document['platforms']['linux-x64']['size'] += 1

            rewrite_manifest(fixture_repo.assets.parent / 'closure', template.signer,
                             add_one)
            fixture_repo.serve()

        def missing_platform(fixture_repo: FixtureRepo, base: Path) -> None:
            mutant = copy_closure(fixture_repo.closure, base / 'mutant')
            rewrite_manifest(mutant, template.signer,
                             lambda document: document['platforms'].pop('linux-x64'))
            fixture_repo.serve(mutant, fixture_repo.executables)

        def wrong_machine(fixture_repo: FixtureRepo, base: Path) -> None:
            arm64 = claude_fixture.elf(base / 'arm64', 183)
            closure = claude_fixture.make_closure(base / 'closure',
                                                  signer=template.signer,
                                                  version=VERSION, amd64=arm64,
                                                  arm64=arm64)
            fixture_repo.serve(closure, {'linux-x64': arm64, 'linux-arm64': arm64})

        def truncated(fixture_repo: FixtureRepo, base: Path) -> None:
            short = {'linux-x64': base / 'short'}
            short['linux-x64'].write_bytes(
                fixture_repo.executables['linux-x64'].read_bytes()[:100])
            fixture_repo.serve(fixture_repo.closure,
                               {**fixture_repo.executables, **short})

        cases = {
            'published key': (published_key, 'the published release key changed'),
            'signature by another key': (other_signer, 'signature does not verify'),
            'manifest version': (manifest_version, 'signed manifest names 9.9.10'),
            'size only': (size_only, 'but the signed manifest records'),
            'manifest missing platform': (missing_platform,
                                          'has no linux-x64 platform'),
            'wrong ELF machine': (wrong_machine, 'targets ELF machine 183, not 62'),
            'truncated executable': (truncated, 'but the signed manifest records'),
        }
        for label, (mutate, needle) in cases.items():
            fixture_repo = repo(f'writer-failure-{label.replace(" ", "-")}')
            base = scratch(f'writer-failure-mutant-{label.replace(" ", "-")}')
            mutate(fixture_repo, base)
            before = fixture_repo.snapshot()
            finished = fixture_repo.run('--version', VERSION)
            require(finished.returncode == 1, f'{label}: exit {finished.returncode}, '
                                              f'want 1: {finished.stderr.strip()}')
            require(needle in finished.stderr,
                    f'{label}: refusal did not mention {needle!r}: '
                    f'{finished.stderr.strip()}')
            require(fixture_repo.snapshot() == before,
                    f'{label}: a refused run changed a committed byte')

    def unreviewed_signer() -> None:
        fixture_repo = repo('writer-unreviewed')
        finished = fixture_repo.run('--version', VERSION,
                                    extra={'FIXTURE_CLAUDE_FINGERPRINT': ''})
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1')
        require('reviewed primary' in finished.stderr,
                f'the refusal does not name the reviewed signer: '
                f'{finished.stderr.strip()}')

    def ci() -> None:
        fixture_repo = repo('writer-ci')
        (fixture_repo.assets / 'fetches.log').unlink(missing_ok=True)
        finished = fixture_repo.run('--version', VERSION,
                                    extra={'GITHUB_ACTIONS': 'true',
                                           'FIXTURE_CLAUDE_RECORD_FETCHES': '1'})
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1')
        require('refusing GitHub Actions' in finished.stderr,
                f'the refusal does not name GitHub Actions: {finished.stderr.strip()}')
        log = fixture_repo.assets / 'fetches.log'
        require(not log.is_file() or not log.read_text().strip(),
                f'a CI run fetched {log.read_text() if log.is_file() else ""}')
        allowed = fixture_repo.run('--check', extra={'GITHUB_ACTIONS': 'true'})
        require(allowed.returncode == 0, f'--check exit {allowed.returncode}: '
                                         f'{allowed.stderr.strip()}')

    def fetch_list() -> None:
        fixture_repo = repo('writer-fetches')
        finished = fixture_repo.run('--version', VERSION,
                                    extra={'FIXTURE_CLAUDE_RECORD_FETCHES': '1'})
        require(finished.returncode == 0, f'exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        requested = (fixture_repo.assets / 'fetches.log').read_text().split()
        base = 'https://downloads.claude.ai/claude-code-releases'
        expected = [f'{base}/{VERSION}/manifest.json',
                    f'{base}/{VERSION}/manifest.json.sig',
                    'https://downloads.claude.ai/keys/claude-code.asc',
                    f'{base}/{VERSION}/linux-x64/claude',
                    f'{base}/{VERSION}/linux-arm64/claude']
        require(requested == expected,
                f'the initial requests were {requested}, want {expected}')
        require(all(url.startswith('https://downloads.claude.ai/')
                    for url in requested),
                f'a request left downloads.claude.ai: {requested}')

    def tampered_proposal() -> None:
        fixture_repo = repo('writer-tamper')
        before = fixture_repo.snapshot()
        finished = fixture_repo.run('--version', VERSION,
                                    extra={'FIXTURE_CLAUDE_TAMPER': 'manifest'})
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1: '
                                          f'{finished.stderr.strip()}')
        require('does not match the pin digest' in finished.stderr,
                f'the refusal does not name the evidence digest: '
                f'{finished.stderr.strip()}')
        require(fixture_repo.snapshot() == before,
                'a tampered proposal changed a committed byte')

    def publication_fault() -> None:
        fixture_repo = repo('writer-fault')
        before = fixture_repo.snapshot()
        finished = fixture_repo.run('--version', VERSION,
                                   extra={'FIXTURE_CLAUDE_INJECT': 'partial',
                                          'FIXTURE_CLAUDE_INJECT_AT': 'manifest.json'})
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1: '
                                          f'{finished.stderr.strip()}')
        require('restoring' in finished.stderr,
                f'the failure does not report a restore: {finished.stderr.strip()}')
        require(fixture_repo.snapshot() == before,
                'a restored publication left a changed byte')

    def incomplete_recovery() -> None:
        fixture_repo = repo('writer-recovery')
        before = fixture_repo.snapshot()
        finished = fixture_repo.run(
            '--version', VERSION,
            extra={'FIXTURE_CLAUDE_INJECT': 'partial',
                   'FIXTURE_CLAUDE_INJECT_AT': 'manifest.json',
                   'FIXTURE_CLAUDE_REFUSE_RESTORE': '1'})
        require(finished.returncode == 3, f'exit {finished.returncode}, want 3: '
                                          f'{finished.stderr.strip()}')
        retained = retained_directories(finished.stderr)
        try:
            # Both directories the recovery depends on: the publisher's own work
            # directory (which holds the numbered backups) and the owner's scratch.
            require('recovery material retained at' in finished.stderr,
                    f'the failure does not name the publisher directory: '
                    f'{finished.stderr.strip()}')
            require('owner scratch retained at' in finished.stderr,
                    f'the failure does not name the owner directory: '
                    f'{finished.stderr.strip()}')
            for directory in retained:
                require(directory.is_dir(), f'{directory} was not retained')
            backups = [directory for directory in retained
                       if (directory / 'backup').is_dir()]
            require(len(backups) == 1,
                    f'expected one publisher directory among {retained}')
            backup = backups[0] / 'backup'
            numbered = sorted(path.name for path in backup.iterdir()
                              if path.name.isdigit())
            require(numbered == ['0', '1', '2', '3'],
                    f'the numbered backups are {numbered}')
            require((backup / '.complete').is_file(),
                    'the backup is not marked complete')
            # An undecided publication may have left a destination half written;
            # what makes status 3 acceptable is that the retained material can undo
            # it. Replay it here, index by index, and require the exact tree back.
            record = json.loads((backups[0] / 'recovery.json').read_text())
            for entry in record['destinations']:
                destination = fixture_repo.root / entry['destination']
                if entry['existed']:
                    shutil.copyfile(entry['backup'], destination)
                elif destination.exists():
                    destination.unlink()
            require(fixture_repo.snapshot() == before,
                    'the retained backups do not restore the committed tree')
        finally:
            for directory in retained:
                shutil.rmtree(directory, ignore_errors=True)

    def signal() -> None:
        fixture_repo = repo('writer-signal')
        before = fixture_repo.snapshot()
        # The publisher's signal handler ends the process with `os._exit`, so the
        # two scratch directories cannot be removed by the code that owns them.
        # This suite removes exactly the ones this case creates.
        earlier = set(temporary_scratch())
        try:
            finished = fixture_repo.run(
                '--version', VERSION,
                extra={'FIXTURE_CLAUDE_INJECT': 'signal',
                       'FIXTURE_CLAUDE_INJECT_AT': 'manifest.json'})
            require(finished.returncode == 130, f'exit {finished.returncode}, want '
                                                f'130: {finished.stderr.strip()}')
            require(fixture_repo.snapshot() == before,
                    'the interrupted publication left a changed byte')
        finally:
            for directory in set(temporary_scratch()) - earlier:
                shutil.rmtree(directory, ignore_errors=True)

    def offline_check() -> None:
        fixture_repo = repo('writer-check')
        finished = fixture_repo.run('--check')
        require(finished.returncode == 0, f'--check exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        require('offline committed-input check' in finished.stdout,
                f'--check printed {finished.stdout!r}')

        def tampered() -> None:
            path = fixture_repo.evidence('manifest.json')
            raw = bytearray(path.read_bytes())
            raw[0] ^= 0xFF
            path.write_bytes(bytes(raw))

        tampered()
        refuse_check = fixture_repo.run('--check')
        require(refuse_check.returncode == 1, f'tampered --check exit '
                                              f'{refuse_check.returncode}')
        require('does not match the pin digest' in refuse_check.stderr,
                f'the refusal does not name the evidence digest: '
                f'{refuse_check.stderr.strip()}')

    def dockerfile_slots() -> None:
        fixture_repo = repo('writer-slots')
        path = fixture_repo.root / 'images' / 'standard' / 'Dockerfile'
        original = path.read_text()
        variants = {
            'two ARG lines': original + f'\n{DOCKERFILE_ARG}{VERSION}\n',
            'label missing': original.replace(
                'LABEL org.agent-vm.version.claude="${AGENT_VERSION_CLAUDE}"\n', ''),
            'ARG differs': original.replace(f'{DOCKERFILE_ARG}{VERSION}',
                                            f'{DOCKERFILE_ARG}0.0.0'),
        }
        for label, text in variants.items():
            path.write_text(text)
            finished = fixture_repo.run('--check')
            require(finished.returncode == 1, f'{label}: --check exit '
                                              f'{finished.returncode}')
            require('Dockerfile must declare' in finished.stderr,
                    f'{label}: the refusal does not name the Dockerfile: '
                    f'{finished.stderr.strip()}')
        path.write_text(original)

    def usage() -> None:
        fixture_repo = repo('writer-usage')
        for arguments in (('--version', VERSION, '--check'), ()):
            finished = fixture_repo.run(*arguments)
            require(finished.returncode == 2, f'{arguments}: exit '
                                              f'{finished.returncode}, want 2')

    for label, function in (
            ('writer: first adoption publishes the four files and a (none) delta',
             first_adoption),
            ('writer: a bump publishes and the delta lists both platforms', bump),
            ('writer: a same-release rewrite is refused with sources unchanged',
             same_release),
            ('writer: upstream and signer failures are refused with sources unchanged',
             failures),
            ('writer: an unreviewed committed signer is refused', unreviewed_signer),
            ('writer: GitHub Actions is refused before any fetch, --check allowed',
             ci),
            ('writer: every initial request is on downloads.claude.ai', fetch_list),
            ('writer: a tampered proposal is refused', tampered_proposal),
            ('writer: a publication fault restores and exits 1', publication_fault),
            ('writer: an incomplete restore exits 3 with both directories',
             incomplete_recovery),
            ('writer: SIGINT exs 130 after the sources are restored', signal),
            ('writer: --check accepts the committed closure and refuses tampering',
             offline_check),
            ('writer: --check requires one Claude ARG and its label',
             dockerfile_slots),
            ('writer: usage errors exit 2', usage)):
        run_case(label, function)


# --- maintenance redirect policy ----------------------------------------------

class _FakeResponse:
    """The minimum a `https_fetch` body read needs."""

    def __init__(self, body: bytes) -> None:
        self._body = io.BytesIO(body)

    def read1(self, size: int) -> bytes:
        return self._body.read(size)

    def read(self, size: int = -1) -> bytes:
        return self._body.read(size)

    def __enter__(self) -> '_FakeResponse':
        return self

    def __exit__(self, *_exception) -> None:
        return None


def redirect_cases() -> None:
    """The writer's own redirect policy, through the real transport handler."""
    import importlib.util
    import time
    import urllib.request

    spec = importlib.util.spec_from_file_location(
        'pin_claude_redirect_probe', ROOT / 'script' / 'dev' / 'pin-claude.py')
    writer = importlib.util.module_from_spec(spec)
    sys.modules['pin_claude_redirect_probe'] = writer
    spec.loader.exec_module(writer)
    _model, _publisher, transport = writer.bind_models(ROOT)
    start = ('https://downloads.claude.ai/claude-code-releases/'
             f'{VERSION}/linux-arm64/claude')

    def attempt(target: str, initial: str = start) -> tuple[str, list[str], bytes]:
        opened: list[str] = []

        class Opener:
            def __init__(self, *handlers) -> None:
                redirects = [handler for handler in handlers
                             if isinstance(handler, urllib.request.HTTPRedirectHandler)]
                require(len(redirects) == 1,
                        f'the transport passed {len(redirects)} redirect handlers')
                self.redirect = redirects[0]

            def open(self, request, timeout=None):
                url = request.full_url
                opened.append(url)
                if url == initial:
                    # urllib's real redirect path, with the real header type: a
                    # plain dict has no case-insensitive `get_all`.
                    headers = email.message.Message()
                    headers.add_header('Location', target)
                    return self.redirect.http_error_302(
                        request, io.BytesIO(b''), 302, 'Found', headers)
                return _FakeResponse(b'asset-bytes')

        original = urllib.request.build_opener
        urllib.request.build_opener = lambda *handlers: Opener(*handlers)
        output = scratch('redirect') / 'body'
        try:
            writer.https_fetch(initial, output, 1 << 20)
            outcome = 'followed'
        except Exception as error:  # noqa: BLE001 - the refusal is the result
            outcome = str(error)
        finally:
            urllib.request.build_opener = original
        return outcome, opened, output.read_bytes() if output.is_file() else b''

    def followed() -> None:
        for target in ('https://release-assets.githubusercontent.com/x',
                       'https://objects.githubusercontent.com/x'):
            outcome, opened, body = attempt(target)
            require(outcome == 'followed', f'{target}: {outcome}')
            require(opened[1:] == [target], f'{target}: opened {opened}')
            require(body == b'asset-bytes', f'{target}: body {body!r}')

    def refused() -> None:
        for target, needle in (
                ('https://evil.example/x', 'refusing a redirect to evil.example'),
                ('https://downloads.claude.ai.evil.example/x',
                 'refusing a redirect to downloads.claude.ai.evil.example'),
                ('https://github.com/x', 'refusing a redirect to github.com'),
                ('https://downloads.claude.ai/other',
                 'refusing a redirect to downloads.claude.ai'),
                ('http://release-assets.githubusercontent.com/x',
                 'refusing a non-HTTPS redirect'),
                ('https://release-assets.githubusercontent.com:444/x',
                 'refusing a redirect to')):
            outcome, opened, body = attempt(target)
            require(needle in outcome, f'{target}: {outcome!r} lacks {needle!r}')
            require(opened == [start], f'{target}: opened {opened}')
            require(body == b'', f'{target}: wrote {body!r}')

    def initial_origin() -> None:
        outcome, opened, _body = attempt('https://objects.githubusercontent.com/x',
                                         initial='https://objects.githubusercontent.com/x')
        require('is not on an approved origin' in outcome, outcome)
        require(opened == [], f'the refused initial URL was opened: {opened}')

    for label, function in (
            ('redirect: the two GitHub asset hosts are followed', followed),
            ('redirect: every other target is refused before it is opened', refused),
            ('redirect: an initial URL off downloads.claude.ai is refused',
             initial_origin)):
        run_case(label, function)


# --- --write-fixture ----------------------------------------------------------

def writer_reason() -> str:
    """The reason text the real writer renders, without importing it by name."""
    import importlib.util
    spec = importlib.util.spec_from_file_location(
        'pin_claude_for_reason', ROOT / 'script' / 'dev' / 'pin-claude.py')
    module = importlib.util.module_from_spec(spec)
    # Dataclasses resolve their defining module through sys.modules, which
    # module_from_spec does not populate: Python 3.12 raises AttributeError where
    # 3.13 tolerates the omission, and the runner's python3 is 3.12.
    sys.modules['pin_claude_for_reason'] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop('pin_claude_for_reason', None)
        raise
    return module.CLAUDE_REASON


def write_fixture(destination: Path, version: str, signer_dir: Path | None) -> int:
    """A committed-shape closure plus the served assets for one release.

    The pin is rendered with the production reason text and today's date, so a
    writer run over this closure at the same version is byte-identical and prints
    `unchanged` -- which is what the bumper suite asserts.
    """
    destination = Path(destination)
    assets = destination / 'assets'
    if assets.exists():
        shutil.rmtree(assets)
    assets.mkdir(parents=True)
    if signer_dir is None:
        signer_dir = destination / 'signer'
    signer = (claude_fixture.read_signer(signer_dir)
              if (signer_dir / 'secret.asc').is_file()
              else claude_fixture.make_signer(signer_dir))
    x64 = claude_fixture.elf(assets / 'linux-x64-claude', 62,
                             marker=version.encode())
    arm64 = claude_fixture.elf(assets / 'linux-arm64-claude', 183, size=8192,
                               marker=version.encode())
    closure = claude_fixture.make_closure(destination / 'closure', signer=signer,
                                          version=version, amd64=x64, arm64=arm64,
                                          reason=writer_reason())
    # The maintenance adapter names a served file after the last two URL path
    # segments joined by '-'.
    shutil.copyfile(closure / 'evidence/manifest.json',
                    assets / f'{version}-manifest.json')
    shutil.copyfile(closure / 'evidence/manifest.json.sig',
                    assets / f'{version}-manifest.json.sig')
    shutil.copyfile(closure / 'claude-code-release-key.asc',
                    assets / 'keys-claude-code.asc')
    print(signer.fingerprint)
    print(closure)
    return 0


# --- main ---------------------------------------------------------------------

def main(argv: list[str]) -> int:
    if argv[:1] == ['--write-fixture']:
        if len(argv) < 4 or argv[2] != '--version':
            print('usage: claude-pins.py --write-fixture DIR --version V [--signer DIR]',
                  file=sys.stderr)
            return 2
        signer_dir = None
        if len(argv) == 6 and argv[4] == '--signer':
            signer_dir = Path(argv[5])
        elif len(argv) != 4:
            print('usage: claude-pins.py --write-fixture DIR --version V [--signer DIR]',
                  file=sys.stderr)
            return 2
        try:
            return write_fixture(Path(argv[1]), argv[3], signer_dir)
        finally:
            for directory in work:
                shutil.rmtree(directory, ignore_errors=True)
    if argv:
        print(f'claude-pins: unknown arguments {argv}', file=sys.stderr)
        return 2
    try:
        template = Template()
        committed_cases()
        loader_cases(template)
        evidence_cases(template)
        signature_cases(template)
        subkey_cases()
        gpg_home_cases(template)
        clock_cases(template)
        agreement_cases(template)
        preflight_cases(template)
        installed_cases(template)
        native_state_cases(template)
        writer_cases(template)
        redirect_cases()
    finally:
        for directory in work:
            shutil.rmtree(directory, ignore_errors=True)
    if failures:
        print(f'\n{len(failures)} failing case(s):', file=sys.stderr)
        for failure in failures:
            print(f'  {failure}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
