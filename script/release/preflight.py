#!/usr/bin/env python3
"""Authenticated create-once checks; absence needs independent owner authority."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import argparse
import base64
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from urllib.parse import urlparse, quote

import public_http

OWNER = 'gregwebs'
REPO = 'gregwebs/agent-vm-images'
PACKAGE = 'agent-vm-standard'
SOURCE_URL = f'https://github.com/{REPO}'
ACCEPT = ', '.join(('application/vnd.oci.image.manifest.v1+json',
                    'application/vnd.oci.image.index.v1+json',
                    'application/vnd.docker.distribution.manifest.v2+json',
                    'application/vnd.docker.distribution.manifest.list.v2+json'))


class Outcome(str, Enum):
    PACKAGE_NOT_CREATED = 'PACKAGE_NOT_CREATED'
    REF_ABSENT = 'REF_ABSENT'
    REF_PRESENT = 'REF_PRESENT'
    UNAUTHORIZED = 'UNAUTHORIZED'
    UNKNOWN = 'UNKNOWN'
    TRANSIENT_FAILURE = 'TRANSIENT_FAILURE'


@dataclass(frozen=True)
class Response:
    status: int
    headers: dict[str, str]
    body: bytes

    def json(self) -> object:
        return json.loads(self.body)


def error_codes(response: Response) -> list[str]:
    body = response.json()
    if not isinstance(body, dict) or set(body) != {'errors'}:
        raise ValueError('malformed registry error response')
    errors = body['errors']
    if not isinstance(errors, list) or not errors:
        raise ValueError('missing registry error codes')
    codes = []
    for error in errors:
        if not isinstance(error, dict) or not isinstance(error.get('code'), str):
            raise ValueError('malformed registry error code')
        codes.append(error['code'])
    return codes


def classify_manifest(response: Response, *, package_exists: bool) -> Outcome:
    if response.status in (401, 403):
        return Outcome.UNAUTHORIZED
    if response.status in (429, 500, 502, 503, 504):
        return Outcome.TRANSIENT_FAILURE
    if response.status == 200:
        body = response.json()
        if not isinstance(body, dict) or body.get('schemaVersion') != 2:
            return Outcome.UNKNOWN
        digest = 'sha256:' + hashlib.sha256(response.body).hexdigest()
        if response.headers.get('docker-content-digest') != digest:
            return Outcome.UNKNOWN
        if body.get('mediaType') not in ACCEPT.split(', '):
            return Outcome.UNKNOWN
        return Outcome.REF_PRESENT
    if response.status == 404:
        codes = error_codes(response)
        if package_exists and codes == ['MANIFEST_UNKNOWN']:
            return Outcome.REF_ABSENT
    return Outcome.UNKNOWN


def package_record(value: object) -> dict:
    if not isinstance(value, dict):
        raise ValueError('malformed owner package inventory record')
    if (value.get('package_type') != 'container'
            or not isinstance(value.get('name'), str)
            or value.get('visibility') not in ('public', 'private', 'internal')):
        raise ValueError('malformed owner container package record')
    return value


def next_page(link: str) -> str | None:
    links = re.findall(r'<([^>]+)>;\s*rel="([^"]+)"', link)
    if link and not links:
        raise ValueError('malformed pagination Link header')
    found = [url for url, rel in links if rel == 'next']
    if len(found) > 1:
        raise ValueError('multiple next inventory pages')
    if not found:
        return None
    url = found[0]
    parsed = urlparse(url)
    if (parsed.scheme != 'https' or parsed.netloc != 'api.github.com'
            or parsed.path != f'/users/{OWNER}/packages'
            or parsed.username or parsed.fragment):
        raise ValueError('owner pagination left approved API endpoint')
    return url


class Curl:
    protocol = '=https'

    def request_url(self, url: str) -> str:
        return url

    def __init__(self, private: Path):
        self.private = private
        self.sequence = 0
        self.snapshot = False

    def get(self, url: str, *, authorization: str, accept: str = 'application/vnd.github+json') -> Response:
        self.sequence += 1
        prefix = self.private / str(self.sequence)
        config, headers, body = (prefix.with_suffix(x) for x in ('.curl', '.headers', '.body'))
        if any(c in url + authorization + accept for c in '\r\n"\\'):
            raise ValueError('invalid HTTP config input')
        config.write_text(f'url = "{self.request_url(url)}"\nheader = "Authorization: {authorization}"\n'
                          f'header = "Accept: {accept}"\n'
                          'header = "X-GitHub-Api-Version: 2022-11-28"\n')
        config.chmod(0o600)
        for attempt in range(1 if getattr(self, 'snapshot', False) else 3):
            try:
                public_http.transfer(['--silent', '--show-error', '--proto', self.protocol,
                    '--connect-timeout', '10', '--max-time', '45', '--config', str(config),
                    '--dump-header', str(headers)], body, 8 * 1024 * 1024)
                statuses = re.findall(r'^HTTP/\S+ (\d{3})', headers.read_text(), re.M)
                status = int(statuses[-1]) if statuses else 0
            except ValueError:
                status = 0
            if status not in (0, 429, 500, 502, 503, 504):
                break
            body.unlink(missing_ok=True)
            if not getattr(self, 'snapshot', False) and attempt < 2:
                time.sleep(2 ** attempt)
        if status in (0, 429, 500, 502, 503, 504):
            raise ValueError(Outcome.TRANSIENT_FAILURE.value)
        parsed_headers = {}
        for line in headers.read_text().splitlines():
            if ':' in line:
                name, value = line.split(':', 1)
                parsed_headers[name.lower()] = value.strip()
        # These are small metadata requests, not image transport.
        if body.stat().st_size > 8 * 1024 * 1024:
            raise ValueError('oversized preflight metadata response')
        return Response(status, parsed_headers, body.read_bytes())


def require_status(response: Response, expected: int) -> None:
    if response.status != expected:
        outcome = Outcome.UNAUTHORIZED if response.status in (401, 403) else Outcome.UNKNOWN
        raise ValueError(f'{outcome.value}: expected HTTP {expected}, got {response.status}')


def owner_inventory(client: Curl, token: str) -> tuple[bool, str]:
    auth = 'Bearer ' + token
    user = client.get('https://api.github.com/user', authorization=auth)
    require_status(user, 200)
    if not isinstance(user.json(), dict) or user.json().get('login') != OWNER:
        raise ValueError('UNAUTHORIZED: inventory token is not the package owner')
    scopes = {x.strip() for x in user.headers.get('x-oauth-scopes', '').split(',')}
    if 'read:packages' not in scopes or scopes & {'write:packages', 'delete:packages', 'repo', 'admin:org'}:
        raise ValueError('UNAUTHORIZED: require read-only owner read:packages token')
    url = f'https://api.github.com/users/{OWNER}/packages?package_type=container&per_page=100'
    visited = set()
    matches = []
    while url:
        if url in visited:
            raise ValueError('cyclic owner inventory pagination')
        visited.add(url)
        response = client.get(url, authorization=auth)
        require_status(response, 200)
        records = response.json()
        if not isinstance(records, list):
            raise ValueError('malformed owner package inventory')
        for raw in records:
            record = package_record(raw)
            if record['name'] == PACKAGE:
                matches.append(record)
        url = next_page(response.headers.get('link', ''))
    if len(matches) > 1:
        raise ValueError('duplicate target package inventory')
    metadata = client.get(f'https://api.github.com/users/{OWNER}/packages/container/{PACKAGE}',
                          authorization=auth)
    if matches:
        require_status(metadata, 200)
        target = package_record(metadata.json())
        if target['name'] != PACKAGE or target['visibility'] != matches[0]['visibility']:
            raise ValueError('owner inventory/metadata disagreement')
        evidence = {'target': PACKAGE, 'present': True, 'visibility': target['visibility']}
    else:
        require_status(metadata, 404)
        # The package endpoint's authoritative absence message is not the
        # releases/git-ref "Not Found"; GitHub returns "Package not found.".
        if (not isinstance(metadata.json(), dict)
                or metadata.json().get('message') != 'Package not found.'):
            raise ValueError('malformed authoritative package absence')
        evidence = {'target': PACKAGE, 'present': False}
    # Never persist unrelated private package names.
    return bool(matches), hashlib.sha256(json.dumps(evidence, sort_keys=True).encode()).hexdigest()


def check_refs(client: Curl, *, workflow_token: str, actor: str,
               version: str, package_exists: bool) -> dict[str, Outcome]:
    basic = base64.b64encode(f'{actor}:{workflow_token}'.encode()).decode()
    token = client.get(f'https://ghcr.io/token?service=ghcr.io&scope=repository:{OWNER}/{PACKAGE}:pull,push',
                       authorization='Basic ' + basic)
    if token.status == 403 and not package_exists and error_codes(token) == ['DENIED']:
        return {ref: Outcome.PACKAGE_NOT_CREATED
                for ref in (f'v{version}', f'v{version}-amd64', f'v{version}-arm64')}
    require_status(token, 200)
    value = token.json()
    if not isinstance(value, dict) or not isinstance(value.get('token'), str) or not value['token']:
        raise ValueError('malformed registry bearer token')
    results = {}
    for ref in (f'v{version}', f'v{version}-amd64', f'v{version}-arm64'):
        response = client.get(f'https://ghcr.io/v2/{OWNER}/{PACKAGE}/manifests/{ref}',
                              authorization='Bearer ' + value['token'], accept=ACCEPT)
        outcome = classify_manifest(response, package_exists=package_exists)
        # A registry 404 is never absence on its own: GHCR reports a manifest
        # GET for a never-published package as MANIFEST_UNKNOWN, which equally
        # means "tag absent" for a package that does exist. The independent
        # owner inventory is the sole authority for first-package bootstrap.
        if (not package_exists and response.status == 404
                and error_codes(response) == ['MANIFEST_UNKNOWN']):
            outcome = Outcome.PACKAGE_NOT_CREATED
        results[ref] = outcome
    return results


class ReleaseOutcome(str, Enum):
    ABSENT = 'ABSENT'
    OCCUPIED_BY_DRAFT = 'OCCUPIED_BY_DRAFT'
    OCCUPIED_BY_PUBLISHED = 'OCCUPIED_BY_PUBLISHED'
    OCCUPIED_BY_TAG = 'OCCUPIED_BY_TAG'
    UNAUTHORIZED_OR_CONCEALED = 'UNAUTHORIZED_OR_CONCEALED'
    TRANSIENT_FAILURE = 'TRANSIENT_FAILURE'


@dataclass(frozen=True)
class PushAuthority:
    repository: str
    run_id: str
    run_attempt: str
    effective_contents: str
    evidence_url: str

    def established(self) -> bool:
        prefix = f'{SOURCE_URL}/actions/runs/{self.run_id}/attempts/{self.run_attempt}'
        return (self.repository == REPO and self.effective_contents == 'write'
                and self.evidence_url.startswith(prefix + '#')
                and len(self.evidence_url) > len(prefix) + 1)


def workflow_authority() -> PushAuthority:
    # The protected environment supplies the maintainer's current-run link to
    # the effective permission report; requested YAML permissions alone are not
    # authority. This is a deployment approval, not a write probe.
    if (os.environ.get('GITHUB_REPOSITORY') != REPO
            or os.environ.get('GITHUB_REF') != 'refs/heads/main'
            or os.environ.get('GITHUB_ACTIONS') != 'true'):
        raise ValueError(ReleaseOutcome.UNAUTHORIZED_OR_CONCEALED.value)
    return PushAuthority(REPO, os.environ['GITHUB_RUN_ID'], os.environ['GITHUB_RUN_ATTEMPT'],
                         os.environ.get('RELEASE_EFFECTIVE_CONTENTS', ''),
                         os.environ.get('RELEASE_PERMISSION_EVIDENCE_URL', ''))


def release_record(value: object) -> dict:
    if (not isinstance(value, dict) or type(value.get('id')) is not int or value['id'] <= 0
            or not isinstance(value.get('tag_name'), str) or type(value.get('draft')) is not bool):
        raise ValueError('malformed Release record')
    return value


def _release_tag_snapshot(client: Curl, *, token: str, version: str,
                        authority: PushAuthority) -> ReleaseOutcome:
    if not authority.established():
        return ReleaseOutcome.UNAUTHORIZED_OR_CONCEALED
    auth = 'Bearer ' + token
    try:
        repository = client.get(f'https://api.github.com/repos/{REPO}', authorization=auth)
        require_status(repository, 200)
        if repository.json().get('full_name') != REPO:
            raise ValueError('wrong repository')
        tag = 'v' + version
        url = f'https://api.github.com/repos/{REPO}/releases?per_page=100&page=1'
        visited, matches = set(), []
        while url:
            if url in visited:
                raise ValueError('cyclic Release pagination')
            visited.add(url)
            response = client.get(url, authorization=auth)
            require_status(response, 200)
            records = response.json()
            if not isinstance(records, list):
                raise ValueError('Release list required')
            for raw in records:
                record = release_record(raw)
                if record['tag_name'] == tag:
                    matches.append(record)
            link = response.headers.get('link', '')
            links = re.findall(r'<([^>]+)>;\s*rel="([^"]+)"', link)
            if link and not links:
                raise ValueError('malformed Release pagination')
            found = [x for x, rel in links if rel == 'next']
            if len(found) > 1:
                raise ValueError('ambiguous Release pagination')
            url = found[0] if found else None
            if url:
                parsed = urlparse(url)
                if (parsed.scheme != 'https' or parsed.netloc != 'api.github.com'
                        or parsed.path != f'/repos/{REPO}/releases' or parsed.fragment
                        or parsed.username or not parsed.query):
                    raise ValueError('Release pagination left approved endpoint')
        if len(matches) > 1:
            raise ValueError('duplicate exact-version Releases')
        published = client.get(f'https://api.github.com/repos/{REPO}/releases/tags/{quote(tag, safe="")}',
                               authorization=auth)
        ref = client.get(f'https://api.github.com/repos/{REPO}/git/ref/tags/{quote(tag, safe="")}',
                         authorization=auth)
        for response in (published, ref):
            if response.status not in (200, 404):
                raise ValueError('unauthorized/unknown tag response')
            if response.status == 404 and response.json().get('message') != 'Not Found':
                raise ValueError('malformed authorized absence')
        if published.status == 200:
            record = release_record(published.json())
            if record['tag_name'] != tag or record['draft'] or not matches or matches[0] != record:
                raise ValueError('list/published evidence disagreement')
        elif matches and not matches[0]['draft']:
            raise ValueError('published Release concealed by tag probe')
        if ref.status == 200:
            value = ref.json()
            obj = value.get('object', {})
            if (value.get('ref') != 'refs/tags/' + tag or obj.get('type') not in ('commit', 'tag')
                    or not re.fullmatch(r'[0-9a-f]{40}', obj.get('sha', ''))):
                raise ValueError('malformed exact git ref')
        if matches:
            return ReleaseOutcome.OCCUPIED_BY_DRAFT if matches[0]['draft'] else ReleaseOutcome.OCCUPIED_BY_PUBLISHED
        return ReleaseOutcome.OCCUPIED_BY_TAG if ref.status == 200 else ReleaseOutcome.ABSENT
    except (ValueError, KeyError, TypeError, AttributeError) as error:
        if str(error) == Outcome.TRANSIENT_FAILURE.value:
            return ReleaseOutcome.TRANSIENT_FAILURE
        return ReleaseOutcome.UNAUTHORIZED_OR_CONCEALED


def validate_repository(client: Curl, token: str) -> None:
    repository = client.get(f'https://api.github.com/repos/{REPO}', authorization='Bearer ' + token)
    require_status(repository, 200)
    value = repository.json()
    if not isinstance(value, dict) or value.get('full_name') != REPO:
        raise ValueError('workflow repository authority mismatch')


def release_tag_absence(client: Curl, *, token: str, version: str,
                        authority: PushAuthority) -> ReleaseOutcome:
    # A retry discards the entire snapshot, including every previous list page.
    client.snapshot = True
    try:
        for attempt in range(3):
            outcome = _release_tag_snapshot(client, token=token, version=version, authority=authority)
            if outcome != ReleaseOutcome.TRANSIENT_FAILURE:
                return outcome
            if attempt < 2:
                time.sleep(2 ** attempt)
        return ReleaseOutcome.TRANSIENT_FAILURE
    finally:
        client.snapshot = False


def require_release_absent(client: Curl, *, token: str, version: str) -> None:
    outcome = release_tag_absence(client, token=token, version=version, authority=workflow_authority())
    if outcome != ReleaseOutcome.ABSENT:
        raise ValueError('Release/tag collision check: ' + outcome.value)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ('version', 'source-sha', 'run-id', 'run-attempt'):
        parser.add_argument('--' + flag, required=True)
    parser.add_argument('--out', required=True, type=Path)
    args = parser.parse_args()
    try:
        if not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', args.version):
            raise ValueError('invalid stable image version')
        if not re.fullmatch(r'[0-9a-f]{40}', args.source_sha):
            raise ValueError('invalid source SHA')
        if not all(re.fullmatch(r'[1-9][0-9]*', x) for x in (args.run_id, args.run_attempt)):
            raise ValueError('invalid workflow run binding')
        if (os.environ.get('GITHUB_REPOSITORY') != REPO
                or os.environ.get('GITHUB_REF') != 'refs/heads/main'
                or os.environ.get('GITHUB_SHA') != args.source_sha
                or os.environ.get('GITHUB_RUN_ID') != args.run_id
                or os.environ.get('GITHUB_RUN_ATTEMPT') != args.run_attempt):
            raise ValueError('preflight requires the exact trusted main workflow invocation')
        os.umask(0o077)
        args.out.mkdir(mode=0o700)
        # Authentication state is separate from redacted evidence. Temporary
        # paths belong to this invocation; no operator credential file is read.
        with tempfile.TemporaryDirectory(prefix='release-private-auth-') as directory:
            client = Curl(Path(directory))
            workflow = os.environ['GITHUB_TOKEN']
            owner = os.environ['GHCR_PACKAGE_INVENTORY_TOKEN']
            actor = os.environ['GITHUB_ACTOR']
            validate_repository(client, workflow)
            exists, inventory_hash = owner_inventory(client, owner)
            require_release_absent(client, token=workflow, version=args.version)
            refs = check_refs(client, workflow_token=workflow, actor=actor,
                              version=args.version, package_exists=exists)
            allowed = {Outcome.PACKAGE_NOT_CREATED, Outcome.REF_ABSENT}
            if any(outcome not in allowed for outcome in refs.values()):
                raise ValueError('create-once preflight refused: ' + ', '.join(
                    f'{ref}={outcome.value}' for ref, outcome in refs.items()))
            evidence = {'schema_version': 1, 'product': 'standard',
                        'version': args.version, 'source_sha': args.source_sha,
                        'build_run_id': args.run_id, 'build_run_attempt': args.run_attempt,
                        'inventory_timestamp': int(time.time()), 'maximum_age_seconds': 900,
                        'owner_inventory_sha256': inventory_hash,
                        'refs': {ref: outcome.value for ref, outcome in refs.items()}}
            (args.out / 'preflight.json').write_text(json.dumps(evidence, indent=2) + '\n')
            print(json.dumps(evidence))
    except (ValueError, OSError, KeyError, TypeError) as error:
        print(f'preflight: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
