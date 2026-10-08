"""GitHub verifies cryptography/trust. Only verified statement bindings are parsed here."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import json
import subprocess
import release_trace
from pathlib import Path
from content import SOURCE, Sha256, hash_file, mapping, sequence, read_json

REPO = 'gregwebs/agent-vm-images'
WORKFLOW = REPO + '/.github/workflows/release-standard.yml'
SLSA = 'https://slsa.dev/provenance/v1'
SPDX = 'https://spdx.dev/Document'


def spdx_predicate_type(sbom: dict[str, object]) -> str:
    """The predicate type the pinned actions/attest-sbom derives from the SBOM itself.

    Its generateSPDXIntoto() splits the document's spdxVersion on '-' and appends the
    remainder, so `SPDX-2.3` is attested as `https://spdx.dev/Document/v2.3`. Deriving it
    from the same document keeps the signed handoff verifiable across a syft SPDX version
    bump, instead of pinning a string that only the third-party action knows.
    """
    parts = str(sbom.get('spdxVersion', '')).split('-')
    if len(parts) != 2 or parts[0] != 'SPDX' or not parts[1]:
        raise ValueError('SPDX document must declare spdxVersion as SPDX-<version>')
    return f'{SPDX}/v{parts[1]}'


def check_verified(value: object, digest: Sha256, *, predicate: str, invocation: str | None,
                   spdx: dict[str, object] | None = None) -> None:
    results = sequence(value, 'gh verification results')
    for raw in results:
        result = mapping(raw, 'gh verification result')
        verified = mapping(result.get('verificationResult'), 'verified result')
        for timestamp in sequence(verified.get('verifiedTimestamps', []), 'verified timestamps'):
            mapping(timestamp, 'verified timestamp')
        statement = mapping(verified.get('statement'), 'verified statement')
        if statement.get('predicateType') != predicate:
            continue
        subjects = sequence(statement.get('subject'), 'statement subjects')
        if not any(mapping(mapping(x, 'subject').get('digest'), 'subject digest').get('sha256') == digest.value[7:]
                   for x in subjects):
            continue
        claim = mapping(statement.get('predicate'), 'statement predicate')
        if invocation is not None:
            if predicate == SLSA:
                actual = mapping(mapping(claim.get('runDetails'), 'run details').get('metadata'),
                                 'run metadata').get('invocationId')
            else:
                # gh 2.97.0 renders the certificate extensions as the certificate object
                # itself; there is no nested "extensions" key.
                signature = mapping(verified.get('signature'), 'verified signature')
                certificate = mapping(signature.get('certificate'), 'verified certificate')
                actual = certificate.get('runInvocationURI')
            if actual != invocation:
                continue
        if predicate.startswith(SPDX) and (spdx is None or claim != spdx):
            continue
        return
    raise ValueError('verified attestation subject/predicate/invocation/SPDX mismatch')


def verify(subject: str | Path, bundle: Path, *, source_sha: str | None,
           digest: Sha256 | None = None, predicate: str = SLSA,
           invocation: str | None = None, output: Path | None = None, spdx_file: Path | None = None) -> None:
    expected = digest if digest is not None else hash_file(Path(subject))[0]
    spdx = read_json(spdx_file) if spdx_file is not None else None
    if spdx is not None:
        predicate = spdx_predicate_type(spdx)
    elif predicate.startswith(SPDX):
        raise ValueError('SPDX verification requires the signed SBOM document')
    argv = ['gh', 'attestation', 'verify', str(subject), '--bundle', str(bundle), '--repo', REPO,
            '--signer-workflow', WORKFLOW, '--deny-self-hosted-runners', '--source-ref', 'refs/heads/main',
            '--predicate-type', predicate, '--format', 'json']
    if source_sha is not None:
        argv += ['--source-digest', source_sha]
    proc = subprocess.run(argv, check=False, capture_output=True, timeout=1800)
    release_trace.emit(argv, proc.returncode, proc.stdout, proc.stderr)
    proc.check_returncode()
    check_verified(json.loads(proc.stdout), expected, predicate=predicate, invocation=invocation, spdx=spdx)
    if output is not None:
        output.write_bytes(proc.stdout)
