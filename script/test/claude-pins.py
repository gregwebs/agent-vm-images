#!/usr/bin/env python3
"""Black-box evidence for the committed Claude native payload closure (#98).

Every case drives a real program. The reader cases run
`recipe-contract claude-pin …` (`internal/claudepin` + `internal/cli`), the
writer cases run `script/dev/pin-claude.sh` (`avm-pin claude`), and the closures
they are pointed at are synthesized here by
`script/test/fixtures/claude/claude_fixture.py`: real OpenPGP keys, real
signatures, real ELF headers, real layout checks. Nothing reaches the network and
no committed evidence is touched. The negative cases are the point: a rewritten
manifest, a stale or foreign signature, a substituted key, a signing subkey pinned
as the primary, a wrong-machine ELF, a group-writable directory, a launcher that
points elsewhere, a native state file that says updates are on.

Three kinds of seam are used, and no others:

* a mutated pin or closure, for everything the reader decides from committed bytes;
* a stub `gpg`/`gpgconf` on PATH, for the two things only the keyring boundary can
  express (an unusable listing, a failed import) and for proving a refusal happened
  before gpg was reached;
* two writer flags, `--fixture-upstream DIR` (serve `https://host/path` from
  `DIR/host/path`) and `--signer FPR` (hold the pin to a throwaway primary instead
  of the reviewed one), because a fixture closure is signed by a key the review has
  never seen.

There is no monkeypatching and no import: the programs are binaries, so a case that
cannot be expressed this way does not exist here.

`--write-fixture DIR --version V` writes a committed-shape closure plus the served
upstream tree for one release, which `script/test/agent-versions.sh` installs into
its fixture repository.

The pin date is UTC on both sides, so the clock cases run the real programs as
separate processes under `TZ=Pacific/Kiritimati` and `TZ=Etc/GMT+12`: a host's local
date only exists inside a real zone, and it is exactly a local-date comparison that
the UTC pin date replaced. One of them writes a closure under each zone and checks
it from both, which is the end-to-end half of that regression.

gpg homes: the reader allocates its own under /tmp, never an inherited TMPDIR, so
the suite gives the same answer whether TMPDIR is short or long; the `gpg home`
cases cover the long TMPDIR and assert the home is a private directory under /tmp
even then. All other scratch lives in a per-run directory and is removed by this
process.
"""
from __future__ import annotations

import datetime
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PIN_DIR = ROOT / 'images' / 'tools' / 'claude'
WRITER = ROOT / 'script' / 'dev' / 'pin-claude.sh'
CONTRACT_BUILD = ROOT / 'script' / 'build' / 'recipe-contract.sh'
FIXTURE_DIR = ROOT / 'script' / 'test' / 'fixtures' / 'claude'

sys.path.insert(0, str(FIXTURE_DIR))

import claude_fixture  # noqa: E402  (the fixture closure builder)

VERSION = '9.9.9'
PLATFORMS = ('linux-x64', 'linux-arm64')
# The pin's platform keys, in the order the pin and the writer list them.
PLATFORM_KEYS = (('linux/amd64', 'linux-x64'), ('linux/arm64', 'linux-arm64'))
TOKEN_PLATFORM = {token: platform for platform, token in PLATFORM_KEYS}
# The `authentication.reason` the writer stamps. A fixture closure is written with
# it so a writer run over that closure at the same version is byte-identical and
# prints `unchanged`; the text is the writer's own, and this copy is what makes the
# fixture a committed-shape pin.
WRITER_REASON = (
    'The manifest is the selection authority: the build verifies the detached '
    'OpenPGP signature over the committed manifest.json with the committed release '
    'key, requires the pinned primary fingerprint, and refuses unless the signed '
    'checksum and size for the platform equal the committed ones before the '
    'executable is fetched. The executable is placed in the native layout and '
    '`claude install` runs only with no network, so the native installer\'s own '
    'downloads never happen. See ADR 0013.')
DOCKERFILE_ARG = 'ARG AGENT_VERSION_CLAUDE='

failures: list[str] = []
cases: list[str] = []
work: list[Path] = []
_reader: Path | None = None


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
    cases.append(label)
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


# --- the real programs --------------------------------------------------------

def reader_binary() -> Path:
    """The reader, compiled once per run.

    `script/build/recipe-contract.sh --native` prints the composed directory it
    built (its own build log goes to stderr), which is the same helper the image
    receives through a named build context.
    """
    global _reader
    if _reader is None:
        built = subprocess.run(['bash', str(CONTRACT_BUILD), '--native'],
                               capture_output=True, text=True, check=False)
        require(built.returncode == 0,
                f'cannot build recipe-contract: {built.stderr.strip()}')
        context = built.stdout.strip().splitlines()[-1]
        _reader = Path(context) / 'out' / 'recipe-contract'
        require(_reader.is_file(), f'the composed context holds no reader: {_reader}')
    return _reader


def run_reader(arguments: list[str], *, env: dict | None = None,
               timeout: int = 600) -> subprocess.CompletedProcess:
    return subprocess.run([str(reader_binary()), 'claude-pin', *arguments],
                          capture_output=True, text=True, check=False, env=env,
                          timeout=timeout)


REFUSAL_PREFIX = 'claude_pin: '


def refuses(arguments: list[str], needles=(), *, env: dict | None = None,
            timeout: int = 600) -> str:
    """The message of a refused invocation, which exits 1 and names its own reason."""
    finished = run_reader(arguments, env=env, timeout=timeout)
    require(finished.returncode == 1,
            f'expected a refusal, got {finished.returncode}: '
            f'{finished.stdout.strip()} {finished.stderr.strip()}')
    require(finished.stderr.startswith(REFUSAL_PREFIX),
            f'the refusal is not prefixed with {REFUSAL_PREFIX!r}: {finished.stderr!r}')
    text = finished.stderr[len(REFUSAL_PREFIX):].strip()
    for needle in needles:
        require(needle in text, f'the refusal did not mention {needle!r}: {text}')
    return text


def accepts(arguments: list[str], *, env: dict | None = None,
            timeout: int = 600) -> str:
    finished = run_reader(arguments, env=env, timeout=timeout)
    require(finished.returncode == 0,
            f'unexpected refusal ({finished.returncode}): {finished.stderr.strip()}')
    return finished.stdout


def load_refusal(directory: Path, *needles: str) -> str:
    """A closure refused by its own structure, before any evidence is read."""
    return refuses(['version', '--pin-dir', str(directory)], needles)


def check_refusal(directory: Path, *needles: str, version: str = VERSION,
                  env: dict | None = None) -> str:
    return refuses(['check', '--pin-dir', str(directory), '--version', version],
                   needles, env=env)


def check_ok(directory: Path, version: str = VERSION,
             env: dict | None = None) -> str:
    return accepts(['check', '--pin-dir', str(directory), '--version', version],
                   env=env)


def run_writer(arguments: list[str], *, root: Path, env: dict | None = None,
               timeout: int = 1800) -> subprocess.CompletedProcess:
    """The real maintainer wrapper: it compiles `avm-pin` and execs `avm-pin claude`.

    `GITHUB_ACTIONS` is dropped from the inherited environment because every case
    here stands in for a maintainer host, and the run this suite is normally part of
    (contracts.sh) happens on a runner; the CI refusal has its own case, which sets
    the marker explicitly.
    """
    environment = {key: value for key, value in os.environ.items()
                   if key != 'GITHUB_ACTIONS'}
    environment.update(env or {})
    return subprocess.run(['bash', str(WRITER), '--root', str(root), *arguments],
                          capture_output=True, text=True, check=False,
                          env=environment, timeout=timeout)


# --- closure mutation helpers -------------------------------------------------

def digest(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


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


# --- committed closure --------------------------------------------------------

def committed_cases() -> None:
    """The real committed pin, read by the real reader."""
    if not (PIN_DIR / 'pin.json').is_file():
        print('skip: committed Claude pin group (no pin.json adopted yet)')
        return
    document = read_pin(PIN_DIR)

    def version() -> None:
        printed = accepts(['version', '--pin-dir', str(PIN_DIR)]).strip()
        require(printed == document['version'],
                f'version printed {printed!r}, pin names {document["version"]}')

    def digests() -> None:
        for token in PLATFORMS:
            printed = accepts(['digest', '--pin-dir', str(PIN_DIR),
                               '--platform', token]).strip()
            expected = document['platforms'][TOKEN_PLATFORM[token]]['sha256']
            require(printed == expected, f'digest {token} printed {printed!r}')

    def check() -> None:
        printed = check_ok(PIN_DIR, document['version'])
        lines = [line for line in printed.splitlines() if line.strip()]
        require(len(lines) == 1, f'check printed {len(lines)} lines: {lines}')
        require('committed closure verified offline' in lines[0],
                f'check did not report the offline verification: {lines[0]}')

    for label, function in (
            ('committed: version', version),
            ('committed: digest both tokens', digests),
            ('committed: check verifies the signature offline', check)):
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
        document = read_pin(template.closure)
        for token in PLATFORMS:
            printed = accepts(['select', '--pin-dir', str(template.closure),
                               '--version', VERSION, '--platform', token])
            pinned = document['platforms'][TOKEN_PLATFORM[token]]
            expected = f'{pinned["url"]} {pinned["sha256"]} {pinned["size"]}\n'
            require(printed == expected,
                    f'select {token} printed {printed!r}, want {expected!r}')

    def key_sets() -> None:
        mutations = {
            'root extra key': (lambda pin: pin.update({'extra': 1}),
                               ('pin must have exactly', 'unexpected [extra]')),
            'root missing key': (lambda pin: pin.pop('authentication'),
                                 ('pin must have exactly', 'missing [authentication]')),
            'manifest extra key': (lambda pin: pin['manifest'].update({'note': 'x'}),
                                   ('manifest evidence must have exactly',
                                    'unexpected [note]')),
            'signature missing key': (lambda pin: pin['signature'].pop('sha256'),
                                      ('signature evidence must have exactly',
                                       'missing [sha256]')),
            'key extra key': (lambda pin: pin['key'].update({'uid': 'x'}),
                              ('pin key must have exactly', 'unexpected [uid]')),
            'platform extra key': (lambda pin: pin['platforms']['linux/amd64'].update(
                {'note': 1}), ('linux/amd64 platform must have exactly',
                               'unexpected [note]')),
            'authentication extra key': (lambda pin: pin['authentication'].update(
                {'note': 1}), ('pin authentication must have exactly',
                               'unexpected [note]')),
        }
        for label, (mutate, needles) in mutations.items():
            load_refusal(template.mutate(label.replace(' ', '-'), mutate), *needles)

    def duplicate_key() -> None:
        directory = template.copy('duplicate-key')
        path = directory / 'pin.json'
        needle = f'"version": "{VERSION}"'
        text = path.read_text()
        require(text.count(needle) == 1,
                f'the fixture pin repeats {needle} {text.count(needle)} times')
        path.write_text(text.replace(needle, f'{needle}, {needle}', 1))
        load_refusal(directory, 'duplicate JSON key')

    def numbers() -> None:
        load_refusal(template.mutate(
            'float-size', lambda pin: pin['platforms']['linux/arm64'].update(
                {'size': 1.5})), 'has a fractional part')
        directory = template.copy('nan-size')
        document = read_pin(directory)
        document['platforms']['linux/arm64']['size'] = float('nan')
        (directory / 'pin.json').write_text(json.dumps(document, indent=2,
                                                       allow_nan=True) + '\n')
        load_refusal(directory, 'is not a JSON number')

    def schema_identity() -> None:
        mutations = {
            'schema': (lambda pin: pin.update({'schema': 2}), 'pin schema must be 1'),
            'name': (lambda pin: pin.update({'name': 'codex'}), 'pin name must be claude'),
            'kind': (lambda pin: pin.update({'kind': 'release-archive'}),
                     'pin kind must be claude-native-release'),
            'method': (lambda pin: pin['authentication'].update(
                {'method': 'hash-only'}),
                'method must be openpgp-signed-manifest'),
            'verified_on': (lambda pin: pin['authentication'].update(
                {'verified_on': '2026-13-01'}), 'is not a calendar date'),
        }
        for label, (mutate, needle) in mutations.items():
            load_refusal(template.mutate(f'{label}-wrong', mutate), needle)

    def version_grammar() -> None:
        for bad in ('latest', '1.2', '01.2.3', '1.2.3+b', '1.2.3 ', ''):
            load_refusal(template.mutate(
                'version-grammar', lambda pin, bad=bad: pin.update({'version': bad})),
                'version')

    def non_canonical_urls() -> None:
        for label, url in NON_CANONICAL_URLS:
            load_refusal(template.mutate(
                'manifest-url', lambda pin, url=url: pin['manifest'].update(
                    {'url': url})), 'manifest evidence url must be')
        load_refusal(template.mutate(
            'signature-path', lambda pin: pin['signature'].update(
                {'path': 'evidence/other.json'})), 'signature evidence path must be')
        load_refusal(template.mutate(
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
            load_refusal(template.mutate(
                'platform-url', lambda pin, url=url: pin['platforms']['linux/amd64']
                .update({'url': url})), 'platform url must be')

    def platform_fields() -> None:
        load_refusal(template.mutate(
            'swapped-tokens', lambda pin: pin['platforms']['linux/amd64'].update(
                {'manifest_platform': 'linux-arm64'})), 'manifest_platform must be')
        load_refusal(template.mutate(
            'platform-missing', lambda pin: pin['platforms'].pop('linux/arm64')),
            'pin platforms must have exactly')
        load_refusal(template.mutate(
            'platform-extra', lambda pin: pin['platforms'].update(
                {'linux/riscv64': pin['platforms']['linux/amd64']})),
            'pin platforms must have exactly')
        load_refusal(template.mutate(
            'fingerprint-case', lambda pin: pin['key'].update(
                {'fingerprint': pin['key']['fingerprint'].lower()})),
            'upper-case hex')

    def digests_and_sizes() -> None:
        mutations = {
            'upper-case sha': (lambda pin: pin['platforms']['linux/amd64'].update(
                {'sha256': pin['platforms']['linux/amd64']['sha256'].upper()}),
                'lower-case hex'),
            'short sha': (lambda pin: pin['platforms']['linux/amd64'].update(
                {'sha256': 'abc'}), 'lower-case hex'),
            'size zero': (lambda pin: pin['platforms']['linux/amd64'].update(
                {'size': 0}), 'size must be between'),
            'size negative': (lambda pin: pin['platforms']['linux/amd64'].update(
                {'size': -1}), 'size must be between'),
            'size bool': (lambda pin: pin['platforms']['linux/amd64'].update(
                {'size': True}), 'expected an integer'),
            'size too large': (lambda pin: pin['platforms']['linux/amd64'].update(
                {'size': 512 * 1024 * 1024 + 1}), 'size must be between'),
        }
        for label, (mutate, needle) in mutations.items():
            load_refusal(template.mutate(label.replace(' ', '-'), mutate), needle)

    def evidence_paths() -> None:
        for bad in ('../manifest.json', '/etc/passwd', 'evidence/../manifest.json'):
            load_refusal(template.mutate(
                'evidence-path', lambda pin, bad=bad: pin['manifest'].update(
                    {'path': bad})), 'manifest evidence path must be')

    def symlinked_evidence() -> None:
        directory = template.copy('symlinked-evidence')
        evidence = directory / 'evidence'
        moved = directory / 'evidence-real'
        evidence.rename(moved)
        evidence.symlink_to(moved.name)
        check_refusal(directory, 'symlink')

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
        # A gpg that records its own invocation: the digest refusal must come first,
        # so an empty log is the evidence that nothing was asked of the keyring.
        probe = gpg_stub(scratch(f'{label}-bin'), mode='bomb')
        check_refusal(directory, 'does not match the pin digest', env=gpg_env(probe))
        require(not logged_homes(probe),
                f'{label}: gpg ran for a digest refusal: {logged_homes(probe)}')

    run_case('evidence: flipped manifest byte refused without gpg',
             lambda: flipped('evidence/manifest.json', 'flip-manifest'))
    run_case('evidence: flipped signature byte refused without gpg',
             lambda: flipped('evidence/manifest.json.sig', 'flip-signature'))
    run_case('evidence: flipped key byte refused without gpg',
             lambda: flipped('claude-code-release-key.asc', 'flip-key'))


# --- gpg boundary -------------------------------------------------------------

def gpg_stub(directory: Path, *, mode: str = 'log',
             validsig_primary: str | None = None,
             listing: Path | None = None) -> Path:
    """A PATH `gpg` that logs its GNUPGHOME and then does one hostile thing.

    The reader hands every gpg call its own private home in the environment, so the
    log is also the record of which homes were used -- which is how a case proves a
    home lived under /tmp and was removed, or that gpg was never reached at all.

    modes:
      log           delegate to the real gpg
      bomb          fail loudly, for a case that must never reach gpg
      fail-import   fail a real (non-`show-only`) key import
      validsig      rewrite the primary fingerprint in every VALIDSIG status line
      listing       answer the `show-only --import` listing from `listing`
    """
    directory.mkdir(parents=True, exist_ok=True)
    real = shutil.which('gpg')
    require(real is not None, 'gpg is required to run this suite')
    log = directory / 'homes.log'
    script = directory / 'gpg'
    header = f"""#!/bin/sh
printf '%s\\n' "$GNUPGHOME" >>'{log}'
"""
    if mode == 'bomb':
        body = header + "echo 'gpg: called when it must not be' >&2\nexit 99\n"
    elif mode == 'fail-import':
        body = header + f"""for argument in "$@"; do
    if [ "$argument" = '--import' ]; then
        case " $* " in
            *' --import-options show-only '*) ;;
            *) echo 'gpg: cannot connect to the gpg-agent' >&2; exit 2 ;;
        esac
    fi
done
exec '{real}' "$@"
"""
    elif mode == 'validsig':
        body = header + f"""for argument in "$@"; do
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
    elif mode == 'listing':
        require(listing is not None, 'the listing mode needs a listing file')
        body = header + f"""for argument in "$@"; do
    if [ "$argument" = '--import-options' ]; then
        cat '{listing}'
        exit 0
    fi
done
exec '{real}' "$@"
"""
    else:
        body = header + f"exec '{real}' \"$@\"\n"
    script.write_text(body)
    script.chmod(0o755)
    return directory


def gpg_env(directory: Path) -> dict:
    """The inherited environment with the stub's directory first on PATH."""
    return dict(os.environ, PATH=f'{directory}{os.pathsep}{os.environ.get("PATH", "")}')


def logged_homes(directory: Path) -> list[str]:
    log = directory / 'homes.log'
    if not log.is_file():
        return []
    return [line for line in log.read_text().splitlines() if line]


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
        check_refusal(directory, 'signature does not verify')

    def foreign_signature() -> None:
        attacker = claude_fixture.make_signer(scratch('attacker') / 'signer')
        directory = template.copy('foreign-signature')
        evidence = directory / 'evidence'
        signature = claude_fixture.sign(attacker, evidence / 'manifest.json')
        (evidence / 'manifest.json.sig').write_bytes(signature)
        pin = read_pin(directory)
        pin['signature']['sha256'] = digest(signature)
        write_pin(directory, pin)
        check_refusal(directory, 'signature does not verify')

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
        check_refusal(directory, 'exactly the primary')

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
        check_refusal(directory, 'exactly the primary')

    def gpg_absent() -> None:
        # A PATH with nothing in it: the reader falls back to /usr/bin:/bin only
        # when PATH is empty, so the directory has to exist and stay barren.
        empty = scratch('gpg-absent-bin')
        text = check_refusal(template.closure, 'gpg is required',
                             env=dict(os.environ, PATH=str(empty)))
        require('gpg is required' in text, text)

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
            check_refusal(directory, 'carries secret key material')

    def validsig_primary() -> None:
        """The only guard against a second signer in the keyring, exercised.

        Every other "other signer" vector is refused earlier (NO_PUBKEY, ERRSIG, or
        the single-primary key check), so nothing else tests the VALIDSIG
        comparison. A gpg that reports a good signature under another primary is
        what an imported, unnamed signer would look like; accepting it would mean a
        manifest signed by a key the pin does not name is placed.
        """
        probe = gpg_stub(scratch('validsig-primary'), mode='validsig',
                         validsig_primary='F' * 40)
        check_refusal(template.closure, 'is not by the primary', env=gpg_env(probe))
        homes = logged_homes(probe)
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
            ('signature: gpg absent from PATH refused', gpg_absent)):
        run_case(label, function)


# --- signing subkey -----------------------------------------------------------

def subkey_cases(template: Template) -> None:
    def secret_subkey_listing() -> None:
        """A `ssb` record in the key listing is a refusal, not a primary.

        `show-only` never prints this shape -- both a secret primary and a secret
        subkey begin with `sec`, which is refused on its own -- so this is the short
        vector that reaches the `ssb` rule with no earlier `sec` record. The `sec`
        half is the `signature: a key file carrying secret key material` case above,
        which is a real gpg import.
        """
        listing = scratch('secret-subkey-listing') / 'listing.txt'
        listing.write_text('\n'.join((
            'pub:u:255:22:E010800BADA0909D:1793422096:::u:::cSC:::::ed25519:::0:',
            f'fpr:::::::::{template.signer.fingerprint}:',
            'uid:u::::1793422096::8719982D789267C30FA69B421197E3BE59AE39FF::'
            'fixture claude signer <fixture@example.invalid>::::::::::0:',
            'ssb:-:255:22:DFB3BE9DEA71B340:1793444866::::::s:::#::ed25519::',
            'fpr:::::::::C68152E062F6665D6BA0AD42DFB3BE9DEA71B340:',
            '')) + '\n')
        probe = gpg_stub(scratch('secret-subkey-listing-bin'), mode='listing',
                         listing=listing)
        check_refusal(template.closure, 'carries secret key material',
                      env=gpg_env(probe))

    def signer() -> None:
        base = scratch('subkey')
        signer = claude_fixture.make_signer(base / 'signer', subkey=True)
        require(signer.fingerprint != signer.signing_fingerprint,
                'the fixture did not create a separate signing subkey')
        x64 = claude_fixture.elf(base / 'assets' / 'x64', 62)
        arm64 = claude_fixture.elf(base / 'assets' / 'arm64', 183)
        closure = claude_fixture.make_closure(
            base / 'closure', signer=signer, version=VERSION, amd64=x64, arm64=arm64)
        printed = check_ok(closure)
        require(signer.fingerprint in printed,
                f'check reports {printed!r}, not the primary {signer.fingerprint}')
        # The same closure with the *subkey* as the pin's identity is refused: a pin
        # may name a primary only.
        pin = read_pin(closure)
        pin['key']['fingerprint'] = signer.signing_fingerprint
        write_pin(closure, pin)
        check_refusal(closure, 'exactly the primary')

    for label, function in (
            ('subkey: a secret subkey record in the key listing is refused',
             secret_subkey_listing),
            ('subkey: a closure signed by a signing subkey verifies as its primary',
             signer)):
        run_case(label, function)


# --- gpg home -----------------------------------------------------------------

def gpg_home_cases(template: Template) -> None:
    def long_tmpdir() -> None:
        base = scratch('long-tmpdir')
        long_dir = base / ('x' * 120)
        long_dir.mkdir()
        require(len(str(long_dir)) >= 120, 'the long TMPDIR is not long enough')
        probe = gpg_stub(base / 'bin')
        # A scratch directory under the long TMPDIR is where the case lives, but no
        # gpg home may be created there.
        closure = copy_closure(template.closure, long_dir / 'source')
        check_ok(closure, env=dict(os.environ, TMPDIR=str(long_dir),
                                   PATH=gpg_env(probe)['PATH']))
        homes = logged_homes(probe)
        require(homes, 'the gpg wrapper logged no invocation')
        for home in homes:
            require(home.startswith('/tmp/claude-pin-gpg.'),
                    f'a gpg home was {home}, not a private home under /tmp')
            require(not Path(home).exists(), f'the gpg home {home} was left behind')

    def import_failure() -> None:
        probe = gpg_stub(scratch('import-failure'), mode='fail-import')
        check_refusal(template.closure, 'cannot import the release key',
                      env=gpg_env(probe))
        homes = logged_homes(probe)
        require(homes, 'the gpg wrapper logged no invocation')
        for home in homes:
            require(not Path(home).exists(), f'the gpg home {home} was left behind')

    for label, function in (
            ('gpg home: verification succeeds under a long ambient TMPDIR',
             long_tmpdir),
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


def utc_today() -> datetime.date:
    return datetime.datetime.now(datetime.timezone.utc).date()


def in_zone(zone: str) -> dict:
    return dict(os.environ, TZ=zone)


def at_a_stable_utc_date(call) -> None:
    """Run `call(today)` unless the UTC date rolls over mid-case, then retry once.

    Every case here derives a pin date from "the UTC date right now". A case that
    straddles UTC midnight would compare against a date the process no longer agrees
    with, so a genuine rollover is retried rather than reported -- but only when the
    date really moved, never to excuse a mutant.
    """
    for _ in range(2):
        today = utc_today()
        try:
            call(today)
        except AssertionError:
            if utc_today() == today:
                raise
            continue
        if utc_today() == today:
            return
    raise AssertionError('the UTC date rolled over twice while this case ran')


def clock_cases(template: Template) -> None:
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
                    if accepted:
                        check_ok(directory, env=in_zone(zone))
                    else:
                        text = check_refusal(directory, 'is in the future',
                                             env=in_zone(zone))
                        require(day.isoformat() in text,
                                f'under TZ={zone} the refusal of {day} did not name '
                                f'the date: {text}')

        at_a_stable_utc_date(case)

    def displaced_writer() -> None:
        """The real writer stamps the UTC date, and the real reader accepts it.

        This is the end-to-end half of the regression: the writer runs on a
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
                fixture = WriterRepo(
                    scratch(f'zone-writer-{zone.replace("/", "-")}'), template,
                    adopted=False, dockerfile_version='0.0.0')
                finished = fixture.run('--version', VERSION,
                                       extra={'TZ': zone})
                require(finished.returncode == 0,
                        f'the writer under TZ={zone} exited {finished.returncode}: '
                        f'{finished.stderr.strip()}')
                stamped = fixture.pin()['authentication']['verified_on']
                require(stamped == today.isoformat(),
                        f'under TZ={zone} the writer stamped verified_on {stamped}, '
                        f'not the UTC date {today.isoformat()}')
                for reader_zone in CLOCK_ZONES:
                    check_ok(fixture.root / 'images' / 'tools' / 'claude',
                             env=in_zone(reader_zone))

        at_a_stable_utc_date(case)

    def shifted_zones() -> None:
        """The same closure verifies with the ambient clock far east and far west."""
        for zone in CLOCK_ZONES:
            printed = accepts(['version', '--pin-dir', str(template.closure)],
                              env=in_zone(zone)).strip()
            require(printed == VERSION,
                    f'under TZ={zone} version printed {printed!r}')

    for label, function in (
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
        check_refusal(directory, 'signed manifest names 9.9.8')

    def manifest_checksum() -> None:
        directory = template.resign(
            'manifest-checksum',
            lambda doc: doc['platforms']['linux-x64'].update({'checksum': 'a' * 64}))
        check_refusal(directory, 'does not carry the pinned linux-x64')

    def manifest_size() -> None:
        directory = template.resign(
            'manifest-size',
            lambda doc: doc['platforms']['linux-x64'].update({'size': 4097}))
        check_refusal(directory, 'does not carry the pinned linux-x64')

    def manifest_binary() -> None:
        directory = template.resign(
            'manifest-binary',
            lambda doc: doc['platforms']['linux-x64'].update({'binary': 'claude.zst'}))
        check_refusal(directory, 'binary is')

    def manifest_missing_platform() -> None:
        directory = template.resign(
            'manifest-missing', lambda doc: doc['platforms'].pop('linux-x64'))
        check_refusal(directory, 'has no linux-x64 platform')

    def other_version() -> None:
        check_refusal(template.closure, 'not the pinned Claude Code version',
                      version='9.9.8')

    def musl_token() -> None:
        refuses(['select', '--pin-dir', str(template.closure), '--version', VERSION,
                 '--platform', 'linux-x64-musl'], ['not a maintained Claude Code platform'])

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
    def preflight(directory: Path, token: str, path: Path, *needles: str,
                  version: str = VERSION) -> str:
        return refuses(['preflight', '--pin-dir', str(directory), '--version', version,
                        '--platform', token, str(path)], needles)

    def good() -> None:
        for token, path in (('linux-x64', template.x64), ('linux-arm64', template.arm64)):
            printed = accepts(['preflight', '--pin-dir', str(template.closure),
                               '--version', VERSION, '--platform', token, str(path)])
            require('matches the committed pin' in printed, printed)

    def digest_mismatch() -> None:
        other = claude_fixture.elf(scratch('preflight-hash') / 'blob', 62, size=8192)
        preflight(template.closure, 'linux-x64', other, 'sha256')

    def size_mismatch() -> None:
        directory = template.resign(
            'preflight-size',
            lambda doc: doc['platforms']['linux-x64'].update({'size': 4097}))
        pin = read_pin(directory)
        pin['platforms']['linux/amd64']['size'] = 4097
        write_pin(directory, pin)
        preflight(directory, 'linux-x64', template.x64,
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
        preflight(directory, 'linux-x64', path, 'not an ELF object')

    def elf_class() -> None:
        base = scratch('preflight-class')
        path = claude_fixture.elf(base / 'blob', 62, elf_class=1)
        directory = claude_fixture.make_closure(base / 'source',
                                                signer=template.signer, version=VERSION,
                                                amd64=path, arm64=path)
        preflight(directory, 'linux-x64', path, 'not a 64-bit ELF object')

    def big_endian() -> None:
        base = scratch('preflight-endian')
        path = claude_fixture.elf(base / 'blob', 62, endian=2)
        directory = claude_fixture.make_closure(base / 'source',
                                                signer=template.signer, version=VERSION,
                                                amd64=path, arm64=path)
        preflight(directory, 'linux-x64', path, 'not a little-endian ELF object')

    def wrong_machine_consistent_pin() -> None:
        base = scratch('preflight-machine')
        directory = claude_fixture.make_closure(
            base / 'source', signer=template.signer, version=VERSION,
            amd64=template.arm64, arm64=template.arm64)
        preflight(directory, 'linux-x64', template.arm64,
                  'targets ELF machine 183, not 62')

    def symlink() -> None:
        base = scratch('preflight-symlink')
        link = base / 'claude'
        link.symlink_to(template.x64)
        preflight(template.closure, 'linux-x64', link, 'not a regular file')

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


def installed_cases(template: Template) -> None:
    # The expected owner is the one this process can actually create: the suite
    # cannot produce a root-owned file, which is why the hook has
    # AGENT_VM_CLAUDE_EXPECTED_OWNER (same rule as verify-agy.sh).
    probe = scratch('installed-owner-probe') / 'probe'
    probe.write_bytes(b'')
    owner = (probe.stat().st_uid, probe.stat().st_gid)
    owner_text = f'{owner[0]}:{owner[1]}'
    payload = template.x64.read_bytes()

    def installed(prefix: Path, *needles: str, token: str = 'linux-x64') -> str:
        return refuses(['installed', '--pin-dir', str(template.closure), '--version',
                        VERSION, '--platform', token, '--prefix', str(prefix),
                        '--owner', owner_text], needles)

    def good() -> None:
        prefix = install_layout(scratch('installed-good'), payload=payload)
        printed = accepts(['installed', '--pin-dir', str(template.closure),
                           '--version', VERSION, '--platform', 'linux-x64',
                           '--prefix', str(prefix), '--owner', owner_text])
        require('match the committed pin' in printed, printed)

    def payload_bytes() -> None:
        prefix = install_layout(scratch('installed-bytes'), payload=payload + b'\n')
        installed(prefix, 'sha256')

    def modes() -> None:
        for mode in (0o775, 0o644):
            prefix = install_layout(scratch('installed-mode'), payload=payload,
                                    payload_mode=mode)
            installed(prefix, f'{mode:04o}', 'not 0755')

    def owner_mismatch() -> None:
        prefix = install_layout(scratch('installed-owner'), payload=payload)
        other = f'{owner[0] + 1}:{owner[1] + 1}'
        refuses(['installed', '--pin-dir', str(template.closure), '--version', VERSION,
                 '--platform', 'linux-x64', '--prefix', str(prefix),
                 '--owner', other], ['is owned'])

    def launchers() -> None:
        prefix = install_layout(scratch('installed-relative'), payload=payload,
                                launcher=f'.local/share/claude/versions/{VERSION}')
        installed(prefix, 'points at')
        prefix = install_layout(scratch('installed-other'), payload=payload,
                                launcher=f'/opt/agent/.local/share/claude/versions/{VERSION}')
        installed(prefix, 'points at')
        prefix = install_layout(scratch('installed-regular'), payload=payload)
        launcher = prefix / '.local/bin/claude'
        launcher.unlink()
        launcher.write_bytes(b'#!/bin/sh\n')
        installed(prefix, 'not a symlink')
        prefix = install_layout(scratch('installed-dangling'), payload=payload,
                                launcher='/nonexistent/claude')
        installed(prefix, 'points at')

    def versions_entries() -> None:
        prefix = install_layout(scratch('installed-extra'), payload=payload,
                                extra_versions=('9.9.9-old',))
        installed(prefix, 'holds')
        prefix = install_layout(scratch('installed-missing'), payload=payload)
        shutil.rmtree(prefix / '.local/share/claude/versions')
        installed(prefix, 'is missing')
        prefix = install_layout(scratch('installed-symlink'), payload=payload)
        entry = prefix / f'.local/share/claude/versions/{VERSION}'
        real = prefix / 'real-payload'
        entry.rename(real)
        entry.symlink_to(real)
        installed(prefix, 'not a regular file')

    def writable_directories() -> None:
        for relative, needle in (
                ('.local/bin', '.local/bin'),
                ('.local/share/claude', '.local/share/claude'),
                ('.local/share/claude/versions', '.local/share/claude/versions')):
            prefix = install_layout(scratch('installed-writable'), payload=payload)
            (prefix / relative).chmod(0o775 if relative != '.local/share/claude'
                                      else 0o757)
            installed(prefix, needle, 'group- or world-writable')

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
    payload = template.x64.read_bytes()

    def state(prefix: Path, *needles: str) -> str:
        return refuses(['native-state', '--prefix', str(prefix)], needles)

    def good() -> None:
        prefix = install_layout(scratch('state-good'), payload=payload)
        printed = accepts(['native-state', '--prefix', str(prefix)])
        require('matches the offline install' in printed, printed)

    def auto_updates() -> None:
        for value in (True, 'false'):
            prefix = install_layout(scratch('state-auto'), payload=payload)
            write_native_state(prefix, auto_updates=value)
            state(prefix, 'autoUpdates')
        prefix = install_layout(scratch('state-auto-missing'), payload=payload)
        (prefix / '.claude.json').write_text(json.dumps({'installMethod': 'native'}))
        state(prefix, 'autoUpdates')

    def install_method() -> None:
        for value in ('npm', None):
            prefix = install_layout(scratch('state-method'), payload=payload)
            document = {'installMethod': value, 'autoUpdates': False}
            (prefix / '.claude.json').write_text(json.dumps(document))
            state(prefix, 'installMethod')

    def duplicate_key() -> None:
        prefix = install_layout(scratch('state-duplicate'), payload=payload)
        (prefix / '.claude.json').write_text(
            '{"installMethod": "native", "autoUpdates": false, '
            '"autoUpdates": true}\n')
        state(prefix, 'duplicate JSON key')

    def symlink_state() -> None:
        prefix = install_layout(scratch('state-symlink'), payload=payload)
        real = prefix / 'real.json'
        (prefix / '.claude.json').rename(real)
        (prefix / '.claude.json').symlink_to(real)
        state(prefix, 'not a regular file')

    def non_object() -> None:
        prefix = install_layout(scratch('state-array'), payload=payload)
        (prefix / '.claude.json').write_text('[]\n')
        state(prefix, 'not a JSON object')

    def leftovers() -> None:
        for relative, needle in (('.claude/downloads', '.claude/downloads'),
                                 ('.cache/claude/staging', '.cache/claude/staging')):
            prefix = install_layout(scratch('state-leftover'), payload=payload)
            (prefix / relative).mkdir(parents=True)
            (prefix / relative / 'claude-9.9.9').write_bytes(b'x')
            state(prefix, needle, 'not empty')
            empty = install_layout(scratch('state-empty'), payload=payload)
            (empty / relative).mkdir(parents=True)
            accepts(['native-state', '--prefix', str(empty)])

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

def snapshot(root: Path) -> dict[str, bytes]:
    """Every file in a tree, for `a refused run changed no byte`."""
    return {str(path.relative_to(root)): path.read_bytes()
            for path in sorted(root.rglob('*')) if path.is_file()}


def proposal_for(base: Path, template: 'Template', version: str):
    """A consistent closure for one release, with its executable files."""
    x64 = claude_fixture.elf(base / 'exec' / 'linux-x64', 62,
                             marker=version.encode())
    arm64 = claude_fixture.elf(base / 'exec' / 'linux-arm64', 183, size=8192,
                               marker=version.encode())
    closure = claude_fixture.make_closure(base / 'closure', signer=template.signer,
                                          version=version, amd64=x64, arm64=arm64,
                                          reason=WRITER_REASON)
    return closure, {'linux-x64': x64, 'linux-arm64': arm64}


class WriterRepo:
    """A repository whose Claude closure, served upstream and Dockerfile this owns.

    Everything the writer reads is here: the committed closure under
    `images/tools/claude`, the single Claude ARG of `images/standard/Dockerfile`, and
    a tree laid out the way `--fixture-upstream` serves it, so a case reaches the
    writer through flags alone -- the bytes it fetches, the signer it holds the pin
    to, and the tree it publishes into.
    """

    def __init__(self, base: Path, template: 'Template', *, version: str = VERSION,
                 adopted: bool = True, dockerfile_version: str = VERSION,
                 signed: bool = True, key: bool = True) -> None:
        self.base = base
        self.root = base / 'repo'
        self.upstream = base / 'upstream'
        self.signer = template.signer.fingerprint if signed else None
        self.version = version
        self.closure, self.executables = proposal_for(base, template, version)
        pin_dir = self.root / 'images' / 'tools' / 'claude'
        pin_dir.mkdir(parents=True)
        shutil.copyfile(self.closure / 'pin.json', pin_dir / 'pin.json')
        shutil.copytree(self.closure / 'evidence', pin_dir / 'evidence')
        shutil.copyfile(self.closure / 'claude-code-release-key.asc',
                        pin_dir / 'claude-code-release-key.asc')
        self.write_dockerfile(dockerfile_version)
        if not key:
            (pin_dir / 'claude-code-release-key.asc').unlink()
        if not adopted:
            (pin_dir / 'pin.json').unlink()
            shutil.rmtree(pin_dir / 'evidence')
        self.serve()

    # --- the repository -------------------------------------------------------

    def write_dockerfile(self, version: str) -> None:
        """The committed Dockerfile, with its one Claude ARG set to `version`.

        The fixture rewrites whatever exact Claude ARG the committed Dockerfile
        carries, so a reviewed bump does not leave this suite pinned to the release
        it last saw.
        """
        dockerfile = (ROOT / 'images' / 'standard' / 'Dockerfile').read_text()
        lines = dockerfile.splitlines(keepends=True)
        matches = [index for index, line in enumerate(lines)
                   if line.startswith(DOCKERFILE_ARG)]
        require(len(matches) == 1,
                f'the committed Dockerfile must carry exactly one {DOCKERFILE_ARG}: '
                f'{matches}')
        lines[matches[0]] = f'{DOCKERFILE_ARG}{version}\n'
        (self.root / 'images' / 'standard').mkdir(parents=True, exist_ok=True)
        (self.root / 'images' / 'standard' / 'Dockerfile').write_text(''.join(lines))

    def serve(self, closure: Path | None = None,
              executables: dict | None = None, *,
              version: str | None = None) -> None:
        """Write the `--fixture-upstream` tree: `https://host/path` from `host/path`."""
        closure = closure or self.closure
        executables = executables or self.executables
        version = version or self.version
        if self.upstream.exists():
            shutil.rmtree(self.upstream)
        base = 'downloads.claude.ai/claude-code-releases'
        files = {
            f'{base}/{version}/manifest.json': closure / 'evidence/manifest.json',
            f'{base}/{version}/manifest.json.sig':
                closure / 'evidence/manifest.json.sig',
            'downloads.claude.ai/keys/claude-code.asc':
                closure / 'claude-code-release-key.asc',
        }
        for token, path in executables.items():
            files[f'{base}/{version}/{token}/claude'] = path
        for relative, source in files.items():
            path = self.upstream / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, path)

    # --- paths ----------------------------------------------------------------

    def pin_path(self) -> Path:
        return self.root / 'images' / 'tools' / 'claude' / 'pin.json'

    def evidence(self, name: str) -> Path:
        return self.root / 'images' / 'tools' / 'claude' / 'evidence' / name

    def dockerfile(self) -> str:
        return (self.root / 'images' / 'standard' / 'Dockerfile').read_text()

    def pin(self) -> dict:
        return read_pin(self.root / 'images' / 'tools' / 'claude')

    def published(self, path: str) -> Path:
        return self.upstream / path

    def snapshot(self) -> dict[str, bytes]:
        return snapshot(self.root)

    # --- the run --------------------------------------------------------------

    def environment(self, extra: dict | None = None) -> dict:
        # Every case here stands in for a maintainer host: the writer refuses
        # `GITHUB_ACTIONS=true` by design, and the run this suite is normally part of
        # (contracts.sh) happens on a runner, so inheriting the runner's own marker
        # would make the write-mode cases fail for the right reason and hide the
        # behaviour they mean to test. The CI refusal has its own case, which sets
        # the marker explicitly.
        environment = {key: value for key, value in os.environ.items()
                       if key != 'GITHUB_ACTIONS'}
        environment.update(extra or {})
        return environment

    def arguments(self, *arguments: str) -> list[str]:
        served = ['--fixture-upstream', str(self.upstream)]
        if self.signer is not None:
            served += ['--signer', self.signer]
        return [*served, *arguments]

    def run(self, *arguments: str, extra: dict | None = None,
            timeout: int = 1800) -> subprocess.CompletedProcess:
        return run_writer(self.arguments(*arguments), root=self.root,
                          env=self.environment(extra), timeout=timeout)


def _published_files(stdout: str) -> list[str]:
    for line in stdout.splitlines():
        if line.startswith('pin-claude: published claude '):
            return sorted(line.split(': ', 2)[2].split(', '))
    return []


def writer_cases(template: 'Template') -> None:
    def repo(label: str, **keywords) -> WriterRepo:
        return WriterRepo(scratch(label), template, **keywords)

    def first_adoption() -> None:
        fixture = repo('writer-first', adopted=False, dockerfile_version='0.0.0')
        finished = fixture.run('--version', VERSION)
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
        document = fixture.pin()
        require(document['version'] == VERSION,
                f'the pin names {document["version"]}')
        require(document['key']['fingerprint'] == template.signer.fingerprint,
                f'the pin names {document["key"]["fingerprint"]}')
        for platform, token in PLATFORM_KEYS:
            raw = fixture.executables[token].read_bytes()
            require(document['platforms'][platform]['sha256'] == digest(raw),
                    f'{platform} digest is not the served executable')
            require(document['platforms'][platform]['size'] == len(raw),
                    f'{platform} size is not the served executable')
            require(document['platforms'][platform]['url'].endswith(
                f'/{VERSION}/{token}/claude'), f'{platform} URL is not canonical')
        dockerfile = fixture.dockerfile()
        require(f'{DOCKERFILE_ARG}{VERSION}' in dockerfile,
                'the published Dockerfile does not carry the new ARG')
        require(f'{DOCKERFILE_ARG}0.0.0' not in dockerfile,
                'the published Dockerfile still carries the old ARG')

        before = fixture.snapshot()
        again = fixture.run('--version', VERSION)
        require(again.returncode == 0, f'repeat exit {again.returncode}: '
                                       f'{again.stderr.strip()}')
        require('unchanged' in again.stdout,
                f'the repeat did not report unchanged:\n{again.stdout}')
        require(fixture.snapshot() == before,
                'a repeat run changed a committed byte')

    def bump() -> None:
        new_version = '9.9.10'
        fixture = repo('writer-bump')
        new_closure, new_executables = proposal_for(fixture.base / 'new',
                                                    template, new_version)
        old = {token: digest(fixture.executables[token].read_bytes())
               for token in ('linux-x64', 'linux-arm64')}
        fixture.serve(new_closure, new_executables, version=new_version)
        finished = fixture.run('--version', new_version)
        require(finished.returncode == 0, f'exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        require(f'claude payload delta: {VERSION} -> {new_version}' in finished.stdout,
                f'the delta does not name the bump:\n{finished.stdout}')
        for platform, token in PLATFORM_KEYS:
            fresh = digest(new_executables[token].read_bytes())
            require(old[token] in finished.stdout and fresh in finished.stdout,
                    f'the delta does not show {platform} old and new digests')
        document = fixture.pin()
        require(document['version'] == new_version,
                f'the pin names {document["version"]}')
        require(f'{DOCKERFILE_ARG}{new_version}' in fixture.dockerfile(),
                'the published Dockerfile does not carry the new ARG')

    def same_release() -> None:
        def changed_manifest(fixture: WriterRepo, base: Path) -> None:
            mutant = copy_closure(fixture.closure, base / 'mutant')

            def add_note(document: dict) -> None:
                document['note'] = 'upstream re-rendered'

            rewrite_manifest(mutant, template.signer, add_note)
            fixture.serve(mutant, fixture.executables)

        def changed_signature(fixture: WriterRepo, base: Path) -> None:
            mutant = copy_closure(fixture.closure, base / 'mutant')
            signature = claude_fixture.sign(
                template.signer, mutant / 'evidence/manifest.json',
                faked_time=int(time.time()) + 3600)
            (mutant / 'evidence/manifest.json.sig').write_bytes(signature)
            pin = read_pin(mutant)
            pin['signature']['sha256'] = digest(signature)
            write_pin(mutant, pin)
            fixture.serve(mutant, fixture.executables)

        def changed_executable(fixture: WriterRepo, base: Path) -> None:
            replaced = base / 'replaced-x64'
            replaced.write_bytes(
                fixture.executables['linux-x64'].read_bytes() + b'\x00')
            fixture.serve(fixture.closure,
                          {**fixture.executables, 'linux-x64': replaced})

        mutations = {
            'manifest': (changed_manifest, 'the signed manifest changed for the '
                                           'already-pinned'),
            'signature': (changed_signature, 'the detached signature changed for '
                                             'the already-pinned'),
            'executable': (changed_executable, 'but the signed manifest records'),
        }
        for label, (mutate, needle) in mutations.items():
            fixture = repo(f'writer-same-release-{label}')
            base = scratch(f'writer-same-release-mutant-{label}')
            mutate(fixture, base)
            before = fixture.snapshot()
            finished = fixture.run('--version', VERSION)
            require(finished.returncode == 1, f'{label}: exit {finished.returncode}, '
                                              f'want 1: {finished.stderr.strip()}')
            require(needle in finished.stderr,
                    f'{label}: refusal did not mention {needle!r}: '
                    f'{finished.stderr.strip()}')
            require(fixture.snapshot() == before,
                    f'{label}: a refused run changed a committed byte')

    def failures() -> None:
        def published_key(fixture: WriterRepo, base: Path) -> None:
            attacker = claude_fixture.make_signer(base / 'attacker')
            fixture.published('downloads.claude.ai/keys/claude-code.asc').write_bytes(
                attacker.key.read_bytes())

        def other_signer(fixture: WriterRepo, base: Path) -> None:
            attacker = claude_fixture.make_signer(base / 'attacker')
            signature = claude_fixture.sign(
                attacker, fixture.closure / 'evidence/manifest.json')
            fixture.published(f'downloads.claude.ai/claude-code-releases/{VERSION}/'
                              'manifest.json.sig').write_bytes(signature)

        def manifest_version(fixture: WriterRepo, base: Path) -> None:
            proposal, executables = proposal_for(base / 'proposal', template, '9.9.10')
            fixture.serve(proposal, executables, version=VERSION)

        def size_only(fixture: WriterRepo, base: Path) -> None:
            mutant = copy_closure(fixture.closure, base / 'mutant')

            def add_one(document: dict) -> None:
                document['platforms']['linux-x64']['size'] += 1

            rewrite_manifest(mutant, template.signer, add_one)
            fixture.serve(mutant, fixture.executables)

        def missing_platform(fixture: WriterRepo, base: Path) -> None:
            mutant = copy_closure(fixture.closure, base / 'mutant')
            rewrite_manifest(mutant, template.signer,
                             lambda document: document['platforms'].pop('linux-x64'))
            fixture.serve(mutant, fixture.executables)

        def wrong_machine(fixture: WriterRepo, base: Path) -> None:
            arm64 = claude_fixture.elf(base / 'arm64', 183)
            closure = claude_fixture.make_closure(base / 'closure',
                                                 signer=template.signer,
                                                 version=VERSION, amd64=arm64,
                                                 arm64=arm64)
            fixture.serve(closure, {'linux-x64': arm64, 'linux-arm64': arm64})

        def truncated(fixture: WriterRepo, base: Path) -> None:
            short = base / 'short'
            short.write_bytes(fixture.executables['linux-x64'].read_bytes()[:100])
            fixture.serve(fixture.closure,
                          {**fixture.executables, 'linux-x64': short})

        def missing_executable(fixture: WriterRepo, base: Path) -> None:
            fixture.published(f'downloads.claude.ai/claude-code-releases/{VERSION}/'
                              'linux-arm64/claude').unlink()

        cases = {
            'published key': (published_key, 'the published release key changed'),
            'signature by another key': (other_signer, 'signature does not verify'),
            'manifest version': (manifest_version, 'signed manifest names 9.9.10'),
            'size only': (size_only, 'but the signed manifest records'),
            'manifest missing platform': (missing_platform,
                                          'has no linux-x64 platform'),
            'wrong ELF machine': (wrong_machine, 'targets ELF machine 183, not 62'),
            'truncated executable': (truncated, 'but the signed manifest records'),
            'missing executable': (missing_executable,
                                   f'/{VERSION}/linux-arm64/claude'),
        }
        for label, (mutate, needle) in cases.items():
            fixture = repo(f'writer-failure-{label.replace(" ", "-")}')
            base = scratch(f'writer-failure-mutant-{label.replace(" ", "-")}')
            mutate(fixture, base)
            before = fixture.snapshot()
            finished = fixture.run('--version', VERSION)
            require(finished.returncode == 1, f'{label}: exit {finished.returncode}, '
                                              f'want 1: {finished.stderr.strip()}')
            require(needle in finished.stderr,
                    f'{label}: refusal did not mention {needle!r}: '
                    f'{finished.stderr.strip()}')
            require(fixture.snapshot() == before,
                    f'{label}: a refused run changed a committed byte')

    def unreviewed_signer() -> None:
        """Without the suite's `--signer`, a fixture closure is an unreviewed signer."""
        fixture = repo('writer-unreviewed', signed=False)
        before = fixture.snapshot()
        finished = fixture.run('--version', VERSION)
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1: '
                                          f'{finished.stderr.strip()}')
        require('reviewed primary' in finished.stderr,
                f'the refusal does not name the reviewed signer: '
                f'{finished.stderr.strip()}')
        require(fixture.snapshot() == before,
                'a refused run changed a committed byte')

    def ci() -> None:
        fixture = repo('writer-ci')
        before = fixture.snapshot()
        finished = fixture.run('--version', VERSION,
                               extra={'GITHUB_ACTIONS': 'true'})
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1: '
                                          f'{finished.stderr.strip()}')
        require('refusing GitHub Actions' in finished.stderr,
                f'the refusal does not name GitHub Actions: {finished.stderr.strip()}')
        require(fixture.snapshot() == before, 'a CI run changed a committed byte')
        allowed = fixture.run('--check', extra={'GITHUB_ACTIONS': 'true'})
        require(allowed.returncode == 0, f'--check under CI exit '
                                         f'{allowed.returncode}: '
                                         f'{allowed.stderr.strip()}')

    def fetch_list() -> None:
        """Every fetch is one of the five canonical URLs on downloads.claude.ai.

        The served tree holds exactly those five files, so a run that reaches any
        other URL has nothing to read and refuses; a lookalike host that *would*
        serve a request is placed with different bytes, so reading it would be
        caught by the digests. What the run placed is therefore the canonical
        bytes from the canonical origin, whatever order it asked in.
        """
        base_url = 'downloads.claude.ai/claude-code-releases'
        expected = [f'{base_url}/{VERSION}/manifest.json',
                    f'{base_url}/{VERSION}/manifest.json.sig',
                    'downloads.claude.ai/keys/claude-code.asc',
                    f'{base_url}/{VERSION}/linux-x64/claude',
                    f'{base_url}/{VERSION}/linux-arm64/claude']
        fixture = repo('writer-fetches')
        require(sorted(str(path.relative_to(fixture.upstream))
                       for path in fixture.upstream.rglob('*') if path.is_file())
                == sorted(expected),
                'the served tree is not exactly the canonical request set')
        lookalike = scratch('writer-lookalike') / 'blob'
        lookalike.write_bytes(b'#!/bin/sh\n# not the pinned release, from a lookalike\n')
        for relative in expected:
            path = fixture.upstream / relative.replace(
                'downloads.claude.ai', 'downloads.claude.ai.evil.example', 1)
            path.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(lookalike, path)
        finished = fixture.run('--version', VERSION)
        require(finished.returncode == 0, f'exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        document = fixture.pin()
        for platform, token in PLATFORM_KEYS:
            require(document['platforms'][platform]['url'].startswith(
                f'https://{base_url}/{VERSION}/'),
                f'{platform} URL left the canonical host: '
                f'{document["platforms"][platform]["url"]}')
            require(document['platforms'][platform]['sha256'] == digest(
                fixture.executables[token].read_bytes()),
                f'{platform} is not the executable served on the canonical host')
        # The initial evidence requests come before any payload: a run whose
        # manifest is missing refuses without publishing a byte.
        broken = repo('writer-fetch-order')
        broken.published(f'{base_url}/{VERSION}/manifest.json').unlink()
        refused = broken.run('--version', VERSION)
        require(refused.returncode == 1, f'exit {refused.returncode}, want 1')
        require(f'{base_url}/{VERSION}/manifest.json' in refused.stderr,
                f'the refusal does not name the missing manifest: '
                f'{refused.stderr.strip()}')
        require(_published_files(refused.stdout) == [],
                'a run with no manifest published something')

    def offline_check() -> None:
        fixture = repo('writer-check')
        finished = fixture.run('--check')
        require(finished.returncode == 0, f'--check exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        require('offline committed-input check' in finished.stdout,
                f'--check printed {finished.stdout!r}')
        require(template.signer.fingerprint in finished.stdout,
                f'--check does not report the signer: {finished.stdout!r}')

        def tampered() -> None:
            path = fixture.evidence('manifest.json')
            raw = bytearray(path.read_bytes())
            raw[0] ^= 0xFF
            path.write_bytes(bytes(raw))

        tampered()
        refused = fixture.run('--check')
        require(refused.returncode == 1, f'tampered --check exit '
                                         f'{refused.returncode}')
        require('does not match the pin digest' in refused.stderr,
                f'the refusal does not name the evidence digest: '
                f'{refused.stderr.strip()}')

    def committed_check() -> None:
        """`--check` over the real committed closure, with the reviewed signer."""
        root = scratch('writer-committed-check') / 'repo'
        pin_dir = root / 'images' / 'tools' / 'claude'
        (pin_dir / 'evidence').mkdir(parents=True)
        for relative in ('pin.json', 'evidence/manifest.json',
                         'evidence/manifest.json.sig', 'claude-code-release-key.asc'):
            shutil.copyfile(PIN_DIR / relative, pin_dir / relative)
        (root / 'images' / 'standard').mkdir(parents=True)
        shutil.copyfile(ROOT / 'images' / 'standard' / 'Dockerfile',
                        root / 'images' / 'standard' / 'Dockerfile')
        before = snapshot(root)
        finished = run_writer(['--check'], root=root)
        require(finished.returncode == 0, f'--check exit {finished.returncode}: '
                                          f'{finished.stderr.strip()}')
        require('offline committed-input check' in finished.stdout,
                f'--check printed {finished.stdout!r}')
        require('not a fresh fetch' in finished.stdout,
                f'--check does not state its scope: {finished.stdout!r}')
        require(snapshot(root) == before, '--check changed a committed byte')

    def dockerfile_slots() -> None:
        fixture = repo('writer-slots')
        path = fixture.root / 'images' / 'standard' / 'Dockerfile'
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
            finished = fixture.run('--check')
            require(finished.returncode == 1, f'{label}: --check exit '
                                              f'{finished.returncode}')
            require('Dockerfile must declare' in finished.stderr,
                    f'{label}: the refusal does not name the Dockerfile: '
                    f'{finished.stderr.strip()}')
        path.write_text(original)

    def missing_key() -> None:
        """The release key is the trust anchor and is committed by hand."""
        fixture = repo('writer-no-key', key=False)
        finished = fixture.run('--version', VERSION)
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1: '
                                          f'{finished.stderr.strip()}')
        require('committed by hand' in finished.stderr,
                f'the refusal does not name the hand-committed key: '
                f'{finished.stderr.strip()}')

    def leftover_evidence() -> None:
        """A closure with evidence and no pin.json is a partial state, not a start."""
        fixture = repo('writer-partial', adopted=False)
        evidence = fixture.root / 'images' / 'tools' / 'claude' / 'evidence'
        evidence.mkdir(parents=True)
        (evidence / 'manifest.json').write_bytes(b'{}\n')
        finished = fixture.run('--version', VERSION)
        require(finished.returncode == 1, f'exit {finished.returncode}, want 1: '
                                          f'{finished.stderr.strip()}')
        require('refusing a partial old state' in finished.stderr,
                f'the refusal does not name the partial state: '
                f'{finished.stderr.strip()}')

    def usage() -> None:
        fixture = repo('writer-usage')
        for arguments in (('--version', VERSION, '--check'), ()):
            finished = fixture.run(*arguments)
            require(finished.returncode == 2, f'{arguments}: exit '
                                              f'{finished.returncode}, want 2: '
                                              f'{finished.stderr.strip()}')

    for label, function in (
            ('writer: first adoption publishes the four files and a (none) delta',
             first_adoption),
            ('writer: a bump publishes and the delta lists both platforms', bump),
            ('writer: a same-release rewrite is refused with sources unchanged',
             same_release),
            ('writer: upstream and signer failures are refused with sources unchanged',
             failures),
            ('writer: a closure under an unreviewed signer is refused',
             unreviewed_signer),
            ('writer: GitHub Actions is refused before any fetch, --check allowed',
             ci),
            ('writer: every request is a canonical URL on downloads.claude.ai',
             fetch_list),
            ('writer: --check accepts a committed closure and refuses tampering',
             offline_check),
            ('writer: --check accepts the real committed closure offline',
             committed_check),
            ('writer: --check requires one Claude ARG and its label',
             dockerfile_slots),
            ('writer: a missing committed release key is refused', missing_key),
            ('writer: evidence without a pin.json is refused', leftover_evidence),
            ('writer: usage errors exit 2', usage)):
        run_case(label, function)


# --- --write-fixture ----------------------------------------------------------

def write_fixture(destination: Path, version: str, signer_dir: Path | None) -> int:
    """A committed-shape closure plus the upstream tree agent-versions.sh serves.

    The pin is rendered with the writer's own reason text and today's UTC date, so a
    writer run over this closure at the same version is byte-identical and prints
    `unchanged` -- which is what the bumper suite asserts.
    """
    destination = Path(destination)
    upstream = destination / 'upstream'
    if upstream.exists():
        shutil.rmtree(upstream)
    if signer_dir is None:
        signer_dir = destination / 'signer'
    signer = (claude_fixture.read_signer(signer_dir)
              if (signer_dir / 'secret.asc').is_file()
              else claude_fixture.make_signer(signer_dir))
    x64 = claude_fixture.elf(destination / 'exec' / 'linux-x64', 62,
                             marker=version.encode())
    arm64 = claude_fixture.elf(destination / 'exec' / 'linux-arm64', 183, size=8192,
                               marker=version.encode())
    closure = claude_fixture.make_closure(destination / 'closure', signer=signer,
                                          version=version, amd64=x64, arm64=arm64,
                                          reason=WRITER_REASON)
    base = 'downloads.claude.ai/claude-code-releases'
    files = {
        f'{base}/{version}/manifest.json': closure / 'evidence/manifest.json',
        f'{base}/{version}/manifest.json.sig': closure / 'evidence/manifest.json.sig',
        'downloads.claude.ai/keys/claude-code.asc':
            closure / 'claude-code-release-key.asc',
        f'{base}/{version}/linux-x64/claude': x64,
        f'{base}/{version}/linux-arm64/claude': arm64,
    }
    for relative, source in files.items():
        path = upstream / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, path)
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
    started = time.monotonic()
    try:
        template = Template()
        committed_cases()
        loader_cases(template)
        evidence_cases(template)
        signature_cases(template)
        subkey_cases(template)
        gpg_home_cases(template)
        clock_cases(template)
        agreement_cases(template)
        preflight_cases(template)
        installed_cases(template)
        native_state_cases(template)
        writer_cases(template)
    finally:
        for directory in work:
            shutil.rmtree(directory, ignore_errors=True)
    elapsed = time.monotonic() - started
    if failures:
        print(f'\n{len(failures)} of {len(cases) + len(failures)} case(s) failed:',
              file=sys.stderr)
        for failure in failures:
            print(f'  {failure}', file=sys.stderr)
        return 1
    print(f'\nclaude-pins: {len(cases)} cases passed ({elapsed:.1f}s)')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
