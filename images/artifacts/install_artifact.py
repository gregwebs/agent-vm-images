#!/usr/bin/env python3
"""Install one committed release-artifact pin into an image root (#96).

The installer is the only path that puts Node or the GitHub CLI into the image.
It reads nothing but the pin, so an ordinary build cannot resolve a URL, a
checksum or a version of its own: the pinned URL is the only place it fetches
from, the pinned digest is the only bytes it accepts, and there is no apt, npm,
mirror or `latest` fallback to fall back to.

Two orderings here are deliberate and load-bearing:

* The reviewed evidence is checked before the payload is fetched. For Node that
  means the detached OpenPGP signature over `SHASUMS256.txt` against the
  committed release key, and the signed line for this archive must equal the
  committed digest; only then is the tarball requested at all.
* Nothing reaches the image until the archive has been preflighted, extracted
  into a private staging tree and published from there. A hostile member, a
  duplicate path, an escaping link, a device node or a setuid bit stops the run
  with the destination untouched.

The GitHub CLI is the one artifact whose own verifier is the artifact: Sigstore
needs `gh attestation verify`, so the *authoritative* verification is the
pin-time one the maintenance tool records in the pin, and the in-build check
binds the committed bundle to the committed archive after extraction and fails
closed. It needs no token; it does need the Sigstore trusted root, which it
fetches, so a build without network fails rather than skipping the check.
"""
from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
import urllib.error
import urllib.request
from pathlib import Path, PurePosixPath

import artifact_pin
from artifact_pin import Refusal

DOWNLOAD_TIMEOUT_SECONDS = 300
# A 64 MiB ceiling is absurdly generous for Node and absurd for anything else
# this repository installs, so an unexpected payload size is a refusal. It is 64
# rather than 32 because Node amd64 is already 31,029,996 B: a 32 MiB ceiling
# leaves 3% headroom and would fail the next patch release.
MAX_EVIDENCE_BYTES = 1 << 20
MAX_ARCHIVE_BYTES = 64 << 20
# Every symlink chain is resolved through the archive's own links, so a bound is
# the cycle detector: a real payload nests a handful of links, never this many.
MAX_LINK_DEPTH = 16


def report_event(kind: str, detail: str) -> None:
    """Append one line to the `--events` log when the caller asked for one.

    The offline suite proves the ordering (evidence and digest before extraction,
    one fetch per artifact) instead of inferring it from the source: without a
    record of what the installer did, a regression that extracts before hashing
    keeps the suite green.
    """
    if EVENTS is None:
        return
    with EVENTS.open('a') as log:
        log.write(f'{kind} {detail}\n')


EVENTS: Path | None = None


def fetch(url: str, limit: int) -> bytes:
    """The one network primitive. `file://` is supported so the offline suite can
    drive every refusal through the same code path a build uses."""
    report_event('fetch', url)
    try:
        with urllib.request.urlopen(url, timeout=DOWNLOAD_TIMEOUT_SECONDS) as response:
            raw = response.read(limit + 1)
    except urllib.error.HTTPError as error:
        raise Refusal(f'cannot fetch {url}: HTTP {error.code}') from None
    except (urllib.error.URLError, OSError) as error:
        raise Refusal(f'cannot fetch {url}: {error}') from None
    if len(raw) > limit:
        raise Refusal(f'{url} is larger than the {limit} byte ceiling')
    return raw


# --- evidence --------------------------------------------------------------

def check_manifest(auth: artifact_pin.OpenPgp,
                  archive: artifact_pin.Archive) -> tuple[bytes, bytes]:
    """Fetch the reviewed manifest and signature, and require their committed bytes."""
    manifest = fetch(auth.manifest_url, MAX_EVIDENCE_BYTES)
    if artifact_pin.sha256(manifest) != auth.manifest_digest:
        raise Refusal(f'{auth.manifest_url} does not match the committed sha256')
    signature = fetch(auth.signature_url, MAX_EVIDENCE_BYTES)
    if artifact_pin.sha256(signature) != auth.signature_digest:
        raise Refusal(f'{auth.signature_url} does not match the committed sha256')
    return manifest, signature


def verify_signature(auth: artifact_pin.OpenPgp, key: Path, manifest: bytes,
                     signature: bytes, scratch: Path) -> None:
    home = scratch / 'gnupg'
    home.mkdir(mode=0o700)
    manifest_file = scratch / 'SHASUMS256.txt'
    signature_file = manifest_file.with_suffix('.txt.sig')
    manifest_file.write_bytes(manifest)
    signature_file.write_bytes(signature)
    environment = {'GNUPGHOME': str(home), 'PATH': os.environ.get('PATH', '/usr/bin:/bin'),
                   'LC_ALL': 'C'}
    run(['gpg', '--batch', '--quiet', '--import', str(key)], environment)
    status = run(['gpg', '--batch', '--status-fd=1', '--verify', str(signature_file),
                  str(manifest_file)], environment, check=False)
    if status.returncode != 0:
        raise Refusal(f'the manifest signature does not verify: {status.stdout.strip()}')
    valid = [line.split() for line in status.stdout.splitlines() if ' VALIDSIG ' in line]
    # VALIDSIG carries the signing key and, last, the primary key it belongs to:
    # a signing subkey is still the pinned identity.
    signers = {field.upper() for fields in valid for field in (fields[2], fields[-1])
               if len(field) in (40, 64)}
    if auth.key_fingerprint not in signers:
        raise Refusal('the manifest was signed by '
                      f'{sorted(signers) or "no key"}, not the pinned {auth.key_fingerprint}')


def run(argv: list[str], environment: dict, check: bool = True) -> subprocess.CompletedProcess:
    result = subprocess.run(argv, capture_output=True, text=True, env=environment, check=False)
    if check and result.returncode != 0:
        raise Refusal(f'{" ".join(argv)} failed ({result.returncode}): '
                      f'{(result.stderr or result.stdout).strip()}')
    return result


def verifier_environment() -> dict:
    """No credential may influence the attestation check: `--bundle` reads only
    the committed bytes, so the token variables are dropped rather than trusted."""
    dropped = {'GH_TOKEN', 'GITHUB_TOKEN', 'GH_ENTERPRISE_TOKEN', 'GH_HOST'}
    environment = {key: value for key, value in os.environ.items() if key not in dropped}
    environment.setdefault('HOME', tempfile.gettempdir())
    return environment


def verify_attestation(auth: artifact_pin.Sigstore, archive_path: Path, bundle: Path,
                       verifier: Path) -> None:
    policy = auth.policy
    argv = [str(verifier), 'attestation', 'verify', str(archive_path),
            '--bundle', str(bundle),
            '--repo', policy['repository'],
            '--signer-workflow', policy['signer_workflow'],
            '--cert-oidc-issuer', policy['cert_oidc_issuer']]
    if policy['deny_self_hosted_runners']:
        argv.append('--deny-self-hosted-runners')
    result = subprocess.run(argv, capture_output=True, text=True, check=False,
                            env=verifier_environment())
    if result.returncode != 0:
        raise Refusal('the committed attestation bundle does not verify the archive: '
                      f'{(result.stderr or result.stdout).strip()}')


# --- archive ---------------------------------------------------------------

def canonical_member(member: tarfile.TarInfo) -> str:
    """The member's canonical path, or a refusal.

    `PurePosixPath` collapses `.` and `//`, so a name that differs from its own
    canonical form is a *second spelling* of a path the archive may already
    carry: `prefix/bin/node` followed by `prefix/bin/./node` used to pass
    preflight and silently overwrite the first file's contents.
    """
    name = member.name
    if not name or name.startswith('/') or '\\' in name or '\n' in name:
        raise Refusal(f'the archive carries an unusable member name: {name!r}')
    canonical = PurePosixPath(name).as_posix()
    if member.isdir() and name == f'{canonical}/':
        return canonical
    if name != canonical:
        raise Refusal(f'the archive member is not a canonical path: {name!r}')
    return canonical


def preflight(archive: tarfile.TarFile, prefix: str) -> list[tarfile.TarInfo]:
    """Every member, before a byte is written. The archive is a reviewed upstream
    payload, not a trusted one: an absolute name, a `..` component, a
    non-canonical spelling, a name outside the pinned prefix, a duplicate, a
    file/directory conflict, a device/fifo member, a setuid bit, a link whose
    whole chain resolves outside the prefix or a link a later member would write
    through all stop the install."""
    members = archive.getmembers()
    if not members:
        raise Refusal('the archive is empty')
    root = prefix.rstrip('/')
    by_name: dict[str, tarfile.TarInfo] = {}
    for member in members:
        name = canonical_member(member)
        parts = PurePosixPath(name).parts
        if any(part in ('', '.', '..') for part in parts):
            raise Refusal(f'the archive member escapes its prefix: {member.name!r}')
        if name != root and not name.startswith(prefix):
            raise Refusal(f'the archive member is outside {prefix}: {member.name!r}')
        if member.isdir() or member.isfile() or member.issym() or member.islnk():
            pass
        else:
            raise Refusal(f'the archive member is not a file, directory or link: {name!r}')
        if member.mode & 0o6000:
            raise Refusal(f'the archive member carries a setuid/setgid bit: {name!r}')
        if name in by_name:
            raise Refusal(f'the archive repeats a member: {name!r}')
        by_name[name] = member
    check_conflicts(by_name)
    check_links(by_name, root)
    return members


def check_conflicts(by_name: dict[str, tarfile.TarInfo]) -> None:
    """A member may not sit under another member that is not a directory.

    That is the file/directory conflict, and it is also how a write through a
    declared link happens: `bin/b` as a symlink plus `bin/b/escaped` is a member
    whose parent is a link, not a directory.
    """
    for name in by_name:
        parent = PurePosixPath(name).parent
        while parent.as_posix() not in ('', '.'):
            ancestor = by_name.get(parent.as_posix())
            if ancestor is not None:
                if ancestor.issym() or ancestor.islnk():
                    raise Refusal(f'the archive would write through the link {ancestor.name} '
                                  f'to create {name}')
                if not ancestor.isdir():
                    raise Refusal(f'the archive carries {name} under the file {ancestor.name}')
            parent = parent.parent


def resolve_through(name: str, links: dict[str, str]) -> list[str]:
    """Resolve one archive path through the archive's own links.

    Each declared symlink is expanded where it appears as an ancestor component,
    so `bin/b -> a/../..` followed by `bin/b/escaped` resolves the way an
    extractor would see it instead of being checked lexically. A cycle and a
    resolution that leaves the archive are both refusals.
    """
    resolved: list[str] = []
    pending = list(PurePosixPath(name).parts)
    hops = 0
    while pending:
        part = pending.pop(0)
        if part in ('', '.'):
            continue
        if part == '..':
            if not resolved:
                raise Refusal(f'{name} links outside the archive')
            resolved.pop()
            continue
        candidate = '/'.join(resolved + [part])
        link = links.get(candidate)
        if link is None:
            resolved.append(part)
            continue
        if link.startswith('/') or '\\' in link or not link:
            raise Refusal(f'{candidate} links outside the archive: {link!r}')
        hops += 1
        if hops > MAX_LINK_DEPTH:
            raise Refusal(f'{name} resolves through a cycle of links')
        pending = list(PurePosixPath(link).parts) + pending
    return resolved


def check_links(by_name: dict[str, tarfile.TarInfo], root: str) -> None:
    """Resolve the whole link graph: no cycle, every target inside the archive,
    and a hard link to a regular member that really is a file."""
    local = {name: member.linkname for name, member in by_name.items() if member.issym()}
    prefix = f'{root}/'
    for name, member in by_name.items():
        if member.islnk():
            target = member.linkname
            if PurePosixPath(target).as_posix() != target:
                raise Refusal(f'{name} is a hard link to a non-canonical path: {target!r}')
            linked = by_name.get(target)
            if linked is None:
                raise Refusal(f'{name} is a hard link to an absent member: {target!r}')
            if not linked.isfile():
                raise Refusal(f'{name} is a hard link to {target}, which is not a file')
        if not (member.issym() or member.islnk()):
            continue
        target = member.linkname
        if not target or target.startswith('/') or '\\' in target:
            raise Refusal(f'{name} links outside the archive: {target!r}')
        if member.islnk():
            continue
        start = '/'.join([*PurePosixPath(name).parent.parts, target])
        final = '/'.join(resolve_through(start, local))
        if final != root and not final.startswith(prefix):
            raise Refusal(f'{name} links outside the archive: {target!r}')


def refuse_symlink_ancestors(staging: Path, target: Path) -> None:
    """Creation order alone is not a proof: check the filesystem itself, so no
    write can pass through a link created by an earlier member."""
    relative = target.relative_to(staging)
    current = staging
    for part in relative.parts[:-1]:
        current = current / part
        if current.is_symlink():
            raise Refusal(f'the archive would write through the link {current.relative_to(staging)}')


def extract(members: list[tarfile.TarInfo], archive: tarfile.TarFile, staging: Path,
            prefix: str) -> None:
    """Files and directories first, links last: a symlink created before the
    members it points at could redirect a later write outside the staging tree."""
    links: list[tarfile.TarInfo] = []
    for member in members:
        relative = PurePosixPath(canonical_member(member)).relative_to(prefix.rstrip('/'))
        target = staging.joinpath(*relative.parts)
        refuse_symlink_ancestors(staging, target)
        if member.isdir():
            target.mkdir(parents=True, exist_ok=True)
            target.chmod(member.mode & 0o777)
        elif member.isfile():
            target.parent.mkdir(parents=True, exist_ok=True)
            source = archive.extractfile(member)
            if source is None:
                raise Refusal(f'the archive member is unreadable: {member.name}')
            target.write_bytes(source.read())
            target.chmod(member.mode & 0o777)
        else:
            links.append(member)
    for member in links:
        relative = PurePosixPath(canonical_member(member)).relative_to(prefix.rstrip('/'))
        target = staging.joinpath(*relative.parts)
        refuse_symlink_ancestors(staging, target)
        target.parent.mkdir(parents=True, exist_ok=True)
        if member.islnk():
            linked = staging.joinpath(*PurePosixPath(member.linkname).relative_to(
                prefix.rstrip('/')).parts)
            os.link(linked, target)
        else:
            os.symlink(member.linkname, target)
    report_event('extract', str(len(members)))


def check_destination(root: Path, target: Path) -> None:
    """Every destination ancestor has to be a real directory.

    A symlinked `root/usr/local` used to redirect the whole publication outside
    the root the caller named, so nothing is written through a link.
    """
    try:
        relative = target.relative_to(root)
    except ValueError:
        raise Refusal(f'{target} is outside the install root {root}') from None
    current = root
    for part in relative.parts[:-1]:
        current = current / part
        if current.is_symlink():
            raise Refusal(f'refusing to publish through the symlinked directory {current}')
        if current.exists() and not current.is_dir():
            raise Refusal(f'refusing to publish through the file {current}')


def plan_publication(staging: Path, root: Path, destination: str,
                     payload: tuple[str, ...]) -> list[tuple[str, Path, Path]]:
    """The complete list of directories and files publication would create.

    Collecting it first is what makes the precheck possible: a conflict in the
    *second* published file used to leave the first one installed.
    """
    into = root / destination.lstrip('/')
    plan: list[tuple[str, Path, Path]] = []

    def walk(staged: Path, target: Path) -> None:
        if staged.is_dir() and not staged.is_symlink():
            plan.append(('dir', staged, target))
            for child in sorted(staged.iterdir()):
                walk(child, target / child.name)
            return
        plan.append(('leaf', staged, target))

    for relative in payload:
        staged = staging / relative
        if not staged.exists() and not staged.is_symlink():
            raise Refusal(f'the reviewed archive carries no {relative} to publish')
        walk(staged, into / relative)
    for kind, _staged, target in plan:
        check_destination(root, target)
        if kind == 'dir':
            if target.exists() and not target.is_dir():
                raise Refusal(f'refusing to replace the existing {target} with a directory')
            continue
        if target.exists() or target.is_symlink():
            raise Refusal(f'refusing to replace an installed path: {target}')
    return plan


def publish(plan: list[tuple[str, Path, Path]], root: Path) -> tuple[list[Path], list[Path]]:
    """Write the prechecked plan and return what was written, so a later failure
    (an attestation, a link) can take it back out again."""
    files: list[Path] = []
    directories: list[Path] = []
    try:
        for kind, staged, target in plan:
            check_destination(root, target)
            if kind == 'dir':
                if not target.exists():
                    target.mkdir(parents=True, exist_ok=True)
                    directories.append(target)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.move(str(staged), str(target))
            files.append(target)
    except BaseException:
        rollback(files, directories)
        raise
    report_event('publish', str(len(files)))
    return files, directories


def rollback(files: list[Path], directories: list[Path]) -> None:
    """Undo a partial publication. Best effort by design: the installer is failing
    anyway, and an unremovable path must not hide the original refusal."""
    for target in reversed(files):
        try:
            target.unlink()
        except OSError:
            pass
    for target in reversed(directories):
        try:
            target.rmdir()
        except OSError:
            pass


def link(root: Path, path: str, target: str) -> Path:
    destination = root / path.lstrip('/')
    resolved = root / target.lstrip('/')
    check_destination(root, destination)
    if destination.exists() or destination.is_symlink():
        raise Refusal(f'refusing to replace an installed path: {destination}')
    if not resolved.exists():
        raise Refusal(f'link target is missing: {resolved}')
    destination.parent.mkdir(parents=True, exist_ok=True)
    os.symlink(target, destination)
    return destination


# --- install ---------------------------------------------------------------

def bundled_npm(staging: Path) -> str:
    """The npm version the reviewed archive ships, read from the staged tree."""
    package = staging / artifact_pin.NPM_PACKAGE
    try:
        return str(json.loads(package.read_text())['version'])
    except (OSError, ValueError, KeyError) as error:
        raise Refusal(f'the archive carries no readable {artifact_pin.NPM_PACKAGE}: '
                      f'{error}') from None


def install(pin: artifact_pin.Pin, arch: str, root: Path, report) -> None:
    platform = artifact_pin.ARCH_PLATFORMS.get(arch)
    if platform is None:
        raise Refusal(f'{pin.name}: unsupported architecture {arch!r}')
    archive = pin.archive(platform)
    authentication = pin.authentication
    report(f'{pin.name} {pin.version}: {archive.filename} for {platform}')
    scratch = Path(tempfile.mkdtemp(prefix=f'install-{pin.name}.', dir=os.environ.get('TMPDIR')))
    try:
        staging = scratch / 'staging'
        staging.mkdir()
        if isinstance(authentication, artifact_pin.OpenPgp):
            check_evidence_digests(pin, authentication)
            manifest, signature = check_manifest(authentication, archive)
            verify_signature(authentication, pin.directory / authentication.key_path, manifest,
                             signature, scratch)
            if artifact_pin.signed_digest(manifest, archive.filename) != archive.digest:
                raise Refusal(f'the signed manifest does not carry the committed digest for '
                              f'{archive.filename}')
        if isinstance(authentication, artifact_pin.Sigstore):
            if archive.bundle is None or archive.bundle_digest is None:
                raise Refusal(f'{pin.name}: the pin carries no attestation bundle')
            bundle = artifact_pin.check_evidence(pin, archive.bundle, archive.bundle_digest)
        payload = fetch(archive.url, MAX_ARCHIVE_BYTES)
        if artifact_pin.sha256(payload) != archive.digest:
            raise Refusal(f'{archive.filename} is {artifact_pin.sha256(payload)}, '
                          f'the pin says {archive.digest}')
        archive_file = scratch / archive.filename
        archive_file.write_bytes(payload)
        with tarfile.open(archive_file, 'r:' + ('xz' if archive.archive == 'tar.xz' else 'gz')) as tar:
            members = preflight(tar, archive.member_prefix)
            extract(members, tar, staging, archive.member_prefix)
        if pin.bundled is not None:
            found = bundled_npm(staging)
            if found != pin.bundled['npm']:
                raise Refusal(f'the archive ships npm {found}, the pin records '
                              f'{pin.bundled["npm"]}')
        if isinstance(authentication, artifact_pin.Sigstore):
            # The verifier is the artifact, so it runs from the staged tree
            # *before* anything reaches the image: a failed attestation must not
            # leave a published CLI behind.
            verifier = staging / 'bin' / pin.name
            if not verifier.is_file():
                raise Refusal(f'the reviewed archive carries no bin/{pin.name} to verify')
            verify_attestation(authentication, archive_file, bundle, verifier)
            report_event('verify', 'attestation')
        plan = plan_publication(staging, root, pin.install.destination, pin.install.payload)
        files, directories = publish(plan, root)
        linked: list[Path] = []
        try:
            for path, target in pin.install.links:
                linked.append(link(root, path, target))
        except Exception:
            for path in reversed(linked):
                try:
                    path.unlink()
                except OSError:
                    pass
            rollback(files, directories)
            raise
    finally:
        shutil.rmtree(scratch, ignore_errors=True)



def check_evidence_digests(pin: artifact_pin.Pin, authentication: artifact_pin.OpenPgp) -> None:
    key = artifact_pin.check_evidence(pin, authentication.key_path, authentication.key_digest)
    if not key.read_text().startswith('-----BEGIN PGP PUBLIC KEY BLOCK-----'):
        raise Refusal(f'{pin.name}: {authentication.key_path} is not an ASCII-armoured key')


def main(argv: list[str]) -> int:
    global EVENTS
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('name', help='artifact name: node or gh')
    parser.add_argument('pin', help='the committed pin file')
    parser.add_argument('arch', help='target architecture: amd64 or arm64')
    parser.add_argument('--root', default='/', help='the image root to install into')
    parser.add_argument('--events', help='append every fetch, extraction, attestation and '
                                         'publication to this file (the offline suite reads it)')
    arguments = parser.parse_args(argv)
    try:
        pin = artifact_pin.load(Path(arguments.pin))
        if pin.name != arguments.name:
            raise Refusal(f'{arguments.pin} pins {pin.name}, not {arguments.name}')
        if arguments.events:
            EVENTS = Path(arguments.events)
            EVENTS.write_text('')
        install(pin, arguments.arch, Path(arguments.root),
                report=lambda message: print(f'install-artifact: {message}', file=sys.stderr))
    except Refusal as error:
        print(f'install-artifact: {error}', file=sys.stderr)
        return 1
    print(f'install-artifact: {arguments.name} {pin.version} installed for {arguments.arch}',
          file=sys.stderr)
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
