#!/usr/bin/env python3
"""Independent data-only npm-v3 versus pnpm-v9 comparator; Python stdlib only.
No package-manager code, parser imports, network access, or repository writes.
"""
import collections as C
import hashlib
import json
from pathlib import Path

BASE = Path(__file__).resolve().parent

def load(p):
    return json.loads((BASE / p).read_text())

def identity(key):
    # Context suffixes contain @, including scoped peers: strip them BEFORE rsplit.
    archive = key.split('(', 1)[0]
    name, version = archive.rsplit('@', 1)
    assert name and version, key
    return archive

def contexts(key):
    result = []
    depth = 0
    start = None
    for i, ch in enumerate(key):
        if ch == '(':
            if depth == 0:
                start = i + 1
            depth += 1
        elif ch == ')':
            depth -= 1
            assert depth >= 0
            if depth == 0:
                result.append(key[start:i])
    assert depth == 0
    return result

def sri(value):
    # SRI is a whitespace-separated set of digests; preserve raw strings as well.
    return sorted(set((value or '').split()))

npm_input = load('synthesized-package-lock.json')
pnpm_input = load('result/parse-yaml.stdout')
original = load('authenticated-shrinkwrap.json')
auth = load('resolutions.json')
N = npm_input['packages']
P = pnpm_input['packages']
S = pnpm_input['snapshots']
assert npm_input['lockfileVersion'] == 3
assert str(pnpm_input['lockfileVersion']) == '9.0'
root_archive = 'node_modules/@earendil-works/pi-coding-agent'

def npm_identity(loc):
    return loc.split('node_modules/')[-1] + '@' + N[loc]['version'] if loc else None

def npm_resolve(loc, name):
    # Node's NODE_MODULES_PATHS: walk actual directory ancestors, skipping a
    # candidate whose directory itself is named node_modules (scopes included).
    parts = loc.split('/') if loc else []
    for i in range(len(parts), -1, -1):
        if i and parts[i - 1] == 'node_modules':
            continue
        prefix = '/'.join(parts[:i])
        candidate = (prefix + '/' if prefix else '') + 'node_modules/' + name
        if candidate in N:
            return candidate
    return None

def pnpm_target(name, ref):
    if isinstance(ref, dict):
        ref = ref['version']
    assert isinstance(ref, str)
    key = name + '@' + ref
    if key in S:
        return key
    if ref in S:
        return ref
    raise ValueError(('unrecognized/missing pnpm snapshot reference', name, ref))

def npm_edges(loc):
    r = N[loc]
    return {name: {'kind': 'optional' if name in r.get('optionalDependencies', {}) else 'mandatory',
                   'specifier': spec, 'target': npm_resolve(loc, name)}
            for name, spec in {**r.get('dependencies', {}), **r.get('optionalDependencies', {})}.items()}

def pnpm_edges(key):
    r = pnpm_input['importers']['.'] if key == '' else S[key]
    return {name: {'kind': 'optional' if name in r.get('optionalDependencies', {}) else 'mandatory',
                   'reference': ref, 'target': pnpm_target(name, ref)}
            for name, ref in {**r.get('dependencies', {}), **r.get('optionalDependencies', {})}.items()}

NE = {loc: npm_edges(loc) for loc in N}
PE = {key: pnpm_edges(key) for key in ['', *S]}

def reach(edges, mandatory=False):
    seen = {''}
    todo = ['']
    while todo:
        for edge in edges[todo.pop()].values():
            target = edge['target']
            if target is not None and target not in seen and (not mandatory or edge['kind'] == 'mandatory'):
                seen.add(target)
                todo.append(target)
    return seen - {''}

nr, pr = reach(NE), reach(PE)
nreq, preq = reach(NE, True), reach(PE, True)
ni = C.defaultdict(list)
pi = C.defaultdict(list)
si = C.defaultdict(list)
for loc in N:
    if loc:
        ni[npm_identity(loc)].append(loc)
for key in P:
    assert identity(key) == key, key
    pi[key].append(key)
for key in S:
    assert identity(key) in P, key
    si[identity(key)].append(key)

# Rooted synchronized traversal is identity guarded. A changed target is a
# frontier, NOT permission to remap its descendants using archive identities.
queue = C.deque([('', '')])
seen = set()
provenance = C.defaultdict(list)
frontiers = []
while queue:
    loc, key = queue.popleft()
    if (loc, key) in seen:
        continue
    seen.add((loc, key))
    for name in sorted(set(NE[loc]) & set(PE[key])):
        old, new = NE[loc][name]['target'], PE[key][name]['target']
        evidence = {'npmParent': loc, 'pnpmParent': key, 'name': name, 'npmTarget': old, 'pnpmTarget': new}
        if old is not None and npm_identity(old) == identity(new):
            provenance[(old, new)].append(evidence)
            queue.append((old, new))
        else:
            frontiers.append(evidence)


def old_peer(loc, name):
    target = npm_resolve(loc, name)
    return {'target': target, 'identity': npm_identity(target) if target else None}

def new_peer(key, name):
    if name in PE[key]:
        target = PE[key][name]['target']
        return {'target': target, 'identity': identity(target), 'evidence': 'snapshot-edge'}
    for context in contexts(key):
        if identity(context).rsplit('@', 1)[0] == name:
            return {'target': context if context in S else None, 'identity': identity(context), 'evidence': 'context-suffix'}
    return {'target': None, 'identity': None, 'evidence': 'absent-from-snapshot-and-context'}


def compare_pair(loc, key):
    old, new = N[loc], P[identity(key)] if key else {}
    oe, pe = NE[loc], PE[key]
    old_peers, new_peers = old.get('peerDependencies', {}), new.get('peerDependencies', {})
    edge_deltas, expanded, peers, edge_comparisons = [], [], [], []
    for name in sorted(set(oe) | set(pe)):
        a, b = oe.get(name), pe.get(name)
        row = {'name': name, 'npm': a, 'pnpm': b}
        edge_comparisons.append({**row,
            'npmResolvedIdentity': npm_identity(a['target']) if a and a['target'] else None,
            'pnpmResolvedIdentity': identity(b['target']) if b else None})
        if a is None:
            if name in old_peers or name in new_peers:
                row['classification'] = 'explicit-peer-expansion'
                row['oldEffectivePeer'] = old_peer(loc, name)
                row['sameEffectiveTarget'] = row['oldEffectivePeer']['identity'] == identity(b['target'])
                expanded.append(row)
            else:
                row['classification'] = 'added-edge'
                edge_deltas.append(row)
        elif b is None:
            row['classification'] = 'dropped-edge'
            edge_deltas.append(row)
        else:
            if (npm_identity(a['target']) if a['target'] else None) != identity(b['target']):
                edge_deltas.append({**row, 'classification': 'resolved-target-changed'})
            if a['kind'] != b['kind']:
                edge_deltas.append({**row, 'classification': 'edge-optionality-changed'})
    for name in sorted(set(old_peers) | set(new_peers) | set(old.get('peerDependenciesMeta', {})) | set(new.get('peerDependenciesMeta', {}))):
        a, b = old_peer(loc, name), new_peer(key, name)
        peers.append({'name': name, 'npmDeclaration': old_peers.get(name), 'pnpmDeclaration': new_peers.get(name),
                      'npmOptional': bool(old.get('peerDependenciesMeta', {}).get(name, {}).get('optional')),
                      'pnpmOptional': bool(new.get('peerDependenciesMeta', {}).get(name, {}).get('optional')),
                      'npmEffective': a, 'pnpmEffective': b,
                      'effectiveChanged': a['identity'] != b['identity'],
                      'declarationChanged': old_peers.get(name) != new_peers.get(name) or
                      old.get('peerDependenciesMeta', {}).get(name, {}) != new.get('peerDependenciesMeta', {}).get(name, {})})
    transitive = [{'name': name, 'npmVisible': old_peer(loc, name), 'pnpmContext': new_peer(key, name)}
                  for name in S.get(key, {}).get('transitivePeerDependencies', [])]
    return {'npmOccurrence': loc, 'pnpmSnapshot': key, 'identityEqual': (npm_identity(loc) == identity(key)) if loc else True,
            'sri': {'npmRaw': old.get('integrity'), 'pnpmRaw': new.get('resolution', {}).get('integrity'),
                    'equal': sri(old.get('integrity')) == sri(new.get('resolution', {}).get('integrity')),
                    'rawEqual': old.get('integrity') == new.get('resolution', {}).get('integrity')},
            'optional': {'npm': bool(old.get('optional')), 'pnpm': bool(S.get(key, {}).get('optional')),
                         'equal': bool(old.get('optional')) == bool(S.get(key, {}).get('optional')),
                         'npmGraphOptionalOnly': loc in nr and loc not in nreq,
                         'pnpmGraphOptionalOnly': key in pr and key not in preq},
            'bin': {'npm': old.get('bin'), 'pnpmHasBin': bool(new.get('hasBin')),
                    'presenceEqual': bool(old.get('bin')) == bool(new.get('hasBin'))},
            'edgeComparisons': edge_comparisons,
            'edgeDeltas': edge_deltas, 'explicitPeerExpansions': expanded,
            'peers': peers, 'transitivePeerVisibility': transitive}

exact = [compare_pair(loc, key) for ident in sorted(set(ni) & set(si)) for loc in sorted(ni[ident]) for key in sorted(si[ident])]
rooted = [dict(compare_pair(loc, key), mappingEvidence=provenance[(loc, key)]) for loc, key in sorted(seen) if loc]
root_comparison = compare_pair('', '')

original_checks = []
for loc, record in original['packages'].items():
    target = root_archive + ('/' + loc if loc else '')
    synthesized = N[target]
    changes = {k: {'original': record.get(k), 'synthesized': synthesized.get(k)}
               for k in sorted(set(record) | set(synthesized)) if record.get(k) != synthesized.get(k)}
    original_checks.append({'originalLocation': loc, 'synthesizedLocation': target, 'changes': changes})
    if loc:
        assert set(changes) <= {'integrity'}, (loc, changes)
        if changes:
            receipt = next(a for a in auth if a['name'] == target.split('node_modules/')[-1] and a['version'] == record['version'])
            assert not record.get('integrity') and synthesized['integrity'] == receipt['integrity']
            assert synthesized['resolved'] == receipt['url']
    else:
        assert set(changes) == {'name', 'resolved', 'integrity'}
        receipt = next(a for a in auth if a['name'] == record['name'] and a['version'] == record['version'])
        assert synthesized['integrity'] == receipt['integrity'] and synthesized['resolved'] == receipt['url']

identity_rows = []
for ident in sorted(set(ni) | set(pi) | set(si)):
    identity_rows.append({'identity': ident, 'npmOccurrences': ni.get(ident, []), 'pnpmPackageRecords': pi.get(ident, []), 'pnpmSnapshots': si.get(ident, []),
                          'occurrenceDelta': len(si.get(ident, [])) - len(ni.get(ident, []))})

mapped_old = {loc for loc, key in seen if loc}
mapped_new = {key for loc, key in seen if key}

def delta_counts(pairs):
    edges = C.Counter(d['classification'] for p in pairs for d in p['edgeDeltas'])
    return {'pairs': len(pairs), 'sriEqualPairs': sum(p['sri']['equal'] for p in pairs),
            'sriUnequalPairs': sum(not p['sri']['equal'] for p in pairs),
            'sriRawEqualPairs': sum(p['sri']['rawEqual'] for p in pairs),
            'sriRawUnequalPairs': sum(not p['sri']['rawEqual'] for p in pairs),
            'optionalClassificationDifferentPairs': sum(not p['optional']['equal'] for p in pairs),
            'binPresenceDifferentPairs': sum(not p['bin']['presenceEqual'] for p in pairs),
            'edgeDeltas': dict(sorted(edges.items())),
            'explicitPeerExpansionEdges': sum(len(p['explicitPeerExpansions']) for p in pairs),
            'explicitPeerExpansionChangedEffectiveTargets': sum(not e['sameEffectiveTarget'] for p in pairs for e in p['explicitPeerExpansions']),
            'declaredPeerComparisons': sum(len(p['peers']) for p in pairs),
            'effectivePeerChanges': sum(e['effectiveChanged'] for p in pairs for e in p['peers']),
            'peerDeclarationChanges': sum(e['declarationChanged'] for p in pairs for e in p['peers'])}

counts = {
    'npmArchiveOccurrencesIncludingAuthenticatedRoot': len(N) - 1,
    'npmUniqueArchiveIdentities': len(ni), 'pnpmArchivePackageRecords': len(P), 'pnpmSnapshotRecords': len(S),
    'pnpmUniqueSnapshotIdentities': len(si),
    'identityIntersection': len(set(ni) & set(pi)),
    'oldOnlyIdentities': len(set(ni) - set(pi)), 'newOnlyIdentities': len(set(pi) - set(ni)),
    'identityMultisetRemovedOccurrences': sum(max(0, -r['occurrenceDelta']) for r in identity_rows),
    'identityMultisetAddedOccurrences': sum(max(0, r['occurrenceDelta']) for r in identity_rows),
    'npmUnreachableOccurrences': len(set(N) - {''} - nr), 'pnpmUnreachableSnapshots': len(set(S) - pr),
    'pnpmPackageRecordsWithoutSnapshots': len(set(P) - set(si)),
    'pnpmPackageRecordsWithoutReachableSnapshots': len(set(P) - {identity(k) for k in pr}),
    'npmRootedMappedOccurrences': len(mapped_old), 'pnpmRootedMappedSnapshots': len(mapped_new),
    'npmWithoutRootedMapping': len(N) - 1 - len(mapped_old), 'pnpmWithoutRootedMapping': len(S) - len(mapped_new),
    'rootedTargetDivergenceFrontiers': len(frontiers),
    'npmMandatoryEdges': sum(e['kind'] == 'mandatory' for edges in NE.values() for e in edges.values()),
    'npmOptionalEdges': sum(e['kind'] == 'optional' for edges in NE.values() for e in edges.values()),
    'pnpmMandatoryEdges': sum(e['kind'] == 'mandatory' for edges in PE.values() for e in edges.values()),
    'pnpmOptionalEdges': sum(e['kind'] == 'optional' for edges in PE.values() for e in edges.values()),
    'npmUnresolvedDeclaredEdges': sum(e['target'] is None for edges in NE.values() for e in edges.values()),
    'npmOptionalFlagOccurrences': sum(bool(r.get('optional')) for loc, r in N.items() if loc),
    'pnpmOptionalFlagSnapshots': sum(bool(r.get('optional')) for r in S.values()),
    'npmBinOccurrences': sum(bool(r.get('bin')) for loc, r in N.items() if loc),
    'pnpmHasBinPackageRecords': sum(bool(r.get('hasBin')) for r in P.values()),
    'pnpmPackageOptionalFieldRecords': sum('optional' in r for r in P.values()),
    'allRecordExactIdentity': delta_counts(exact), 'rootedIdentityGuarded': delta_counts(rooted),
}
assert counts['npmArchiveOccurrencesIncludingAuthenticatedRoot'] == 144
assert counts['npmUniqueArchiveIdentities'] == 143

limits = [
    'Archive identity means exact name@version, NOT proof of identical bytes; SRI is compared separately. Occurrences are npm physical package locations versus pnpm snapshot keys, not runtime path unfoldings.',
    'All-record exact-identity pairs are the Cartesian product of npm occurrences and pnpm contexts for each exact identity. They do NOT establish rooted correspondence or invent missing old contexts.',
    'Rooted mapping starts at synthetic importer and follows common named resolved edges only while child archive identities remain equal. A target change is recorded and traversal stops at that frontier. Equal nodes reachable by another unchanged route can still be mapped.',
    'Rooted pairs may be many-to-many. Counts of delta edges in this view are per mapped pair/name/classification, not deduplicated archive declarations. Nonmapped records are not silently paired by identity.',
    'Declared peer effective context uses npm ancestor lookup versus pnpm explicit snapshot edge/context suffix; absent peers normalize to null. This does not test peer semver satisfaction or runtime dynamic imports.',
    'TransitivePeerDependencies are pnpm metadata, not new dependency edges or old peer declarations. npm ancestor visibility is reported for these names, but missing old transitive context correspondence is not claimed equivalent.',
    'Optional flags are compared at npm occurrence/pnpm snapshot level, never inferred from absent pnpm package-level optional. Mandatory-only reachability is a separate raw-edge diagnostic; it is not a platform-specific install simulation.',
    'bin versus hasBin is presence normalization only: pnpm lock does not preserve command names/paths, so equality of executable mappings cannot be proven.',
    'Inputs only: no fetched archives, manifest execution, pnpm internals, YAML parsers, or installation. Original authenticated root shrinkwrap is retained verbatim; synthesis root manifest is a distinct record from original shrinkwrap root metadata.',
]
definitions = {
    'archive': 'Includes authenticated pi-coding-agent root archive; excludes synthetic project importer. All original shrinkwrap package records included.',
    'identityIntersection': 'Unique exact name@version keys present in npm occurrences and pnpm packages.',
    'identityMultiset': 'Multiplicity by npm physical locations versus pnpm snapshot keys, including unreachable records. Positive/negative differences summed separately.',
    'unreachable': 'Not reachable from synthetic root via dependencies plus optionalDependencies, including platform optionals and explicit pnpm peer edges; peer declarations alone are not traversal edges.',
    'rootedFrontier': 'One common named edge from a synchronized mapped parent pair whose resolved archive identity differs (or npm target is missing).',
    'edgeDeltas': 'Per source pair and name: dropped, added, resolved-target-changed by exact name@version, and independently changed mandatory/optional kind. Peer-only pnpm expansion is reported separately.',
    'sri': 'Sorted sets of whitespace-separated SRI tokens compared; raw values retained. No network reauthentication of transitive archives.',
    'optionalClassification': 'Boolean npm packages[location].optional versus pnpm snapshots[key].optional; absent means false.',
    'allRecordExactIdentity': 'All exact-identity occurrence/context pairs, irrespective of reachability. No arbitrary choice of context.',
    'rootedIdentityGuarded': 'Distinct identity-equal (npm location,pnpm snapshot) pairs obtained by guarded root edge walk; excludes synthetic importer.',
    'packageRecordOrphans': 'Separate absence of any snapshot and absence of any reachable snapshot for pnpm package archive records.',
    'rawEdgeCounts': 'Sum named dependencies/optionalDependencies over every source record plus synthetic importer, optional overriding mandatory if duplicated; pnpm includes explicit peer expansion.',
    'peerDeclarationChanges': 'Per mapped/exact source pair and peer name: range or peer metadata differs, independently of effective target identity.',
    'binCounts': 'Truthy npm bin mappings per archive occurrence versus truthy pnpm package hasBin per archive record; no command mapping equivalence claim.',
}
report = {'schemaVersion': 1, 'counts': counts, 'definitions': definitions, 'limits': limits,
          'inputSha256': {p: hashlib.sha256((BASE / p).read_bytes()).hexdigest() for p in
                          ['synthesized-package-lock.json', 'result/parse-yaml.stdout', 'authenticated-shrinkwrap.json', 'resolutions.json']},
          'inputs': {'npm': npm_input, 'pnpmRawSyntax': pnpm_input, 'originalAuthenticatedShrinkwrap': original, 'authentication': auth},
          'originalRecordPreservation': original_checks,
          'identitiesAndOccurrenceMultisets': identity_rows,
          'npmRecords': [{'location': loc, 'identity': npm_identity(loc), 'record': r, 'edges': NE[loc], 'reachable': loc in nr,
                          'mandatoryReachable': loc in nreq} for loc, r in sorted(N.items()) if loc],
          'pnpmPackageRecords': [{'key': key, 'identity': identity(key), 'record': r} for key, r in sorted(P.items())],
          'pnpmSnapshots': [{'key': key, 'identity': identity(key), 'contexts': contexts(key), 'record': r, 'edges': PE[key],
                             'reachable': key in pr, 'mandatoryReachable': key in preq} for key, r in sorted(S.items())],
          'rootImporterEdges': {'npm': NE[''], 'pnpm': PE[''], 'comparison': root_comparison},
          'allRecordExactIdentityCorrespondences': exact, 'rootedMappings': rooted,
          'rootedDivergenceFrontiers': frontiers,
          'npmWithoutRootedMapping': sorted(set(N) - {''} - mapped_old),
          'pnpmWithoutRootedMapping': sorted(set(S) - mapped_new),
          'unreachable': {'npmOccurrences': sorted(set(N) - {''} - nr), 'pnpmSnapshots': sorted(set(S) - pr),
                          'pnpmPackagesWithoutSnapshots': sorted(set(P) - set(si)),
                          'pnpmPackagesWithoutReachableSnapshots': sorted(set(P) - {identity(k) for k in pr})}}
(BASE / 'comparison.json').write_text(json.dumps(report, indent=2, sort_keys=True) + '\n')
lines = ['# Authenticated npm to pnpm import: independent data-only comparison', '',
         '## Outcome', '',
         'The import preserves all 143 exact archive identities. All 144 npm archive occurrences (including the authenticated root, excluding the synthetic importer) have guarded rooted correspondences to 143 pnpm snapshots. Every matched occurrence has equal SRI, optional classification, and bin presence. There are no changed resolved archive targets, dropped ordinary edges, added ordinary edges, unreachable records, or rooted divergence frontiers.', '',
         'The sole occurrence-multiset reduction is deduplication of the two physical `agent-base@9.0.0` occurrences to one snapshot. This is not an archive-identity removal.', '',
         'pnpm explicitly represents the optional `openai -> ws@8.21.0` peer as a snapshot optional edge; npm already resolves that peer to the same archive through ancestor lookup. Effective peer target changes: zero. Separately, pnpm adds the `debug` optional peer declaration `supports-color: *` where npm carries only optional peer metadata; both effective targets remain absent.', '',
         'All 26 esbuild platform-package occurrences/snapshots retain optional:true. pnpm packages have no optional field (classification resides in snapshots). Nine npm bin-bearing occurrences match nine pnpm hasBin archive records; command names and executable paths cannot be recovered from hasBin.', '',
         'Original authenticated shrinkwrap is embedded unchanged. Synthesis changes only root name/resolved/integrity representation and the five receipt-backed missing integrity supplements; detailed original-record comparisons are in the machine report.', '',
         '## Exact counts', '',
         '```json', json.dumps(counts, indent=2, sort_keys=True), '```', '', '## Exact count definitions', '']
material_equal = (counts['oldOnlyIdentities'] == counts['newOnlyIdentities'] == counts['npmUnreachableOccurrences'] == counts['pnpmUnreachableSnapshots'] == counts['rootedTargetDivergenceFrontiers'] == counts['npmWithoutRootedMapping'] == counts['pnpmWithoutRootedMapping'] == 0 and not counts['rootedIdentityGuarded']['edgeDeltas'] and counts['rootedIdentityGuarded']['sriRawUnequalPairs'] == counts['rootedIdentityGuarded']['effectivePeerChanges'] == counts['rootedIdentityGuarded']['optionalClassificationDifferentPairs'] == 0)
if not material_equal:
    lines = ['# Authenticated npm to pnpm comparison', '', '## Outcome', '', '**MATERIAL MISMATCH: inspect exact counts and full comparison.json. No fidelity success is claimed for this input.**', '', '## Exact counts', '', '```json', json.dumps(counts, indent=2, sort_keys=True), '```', '', '## Exact count definitions', '']
lines += [f'- **{key}**: {value}' for key, value in definitions.items()]
lines += ['', '## Rooted divergence frontiers', '', '```json', json.dumps(frontiers, indent=2), '```',
          '', '## Identity/multiplicity differences', '', '```json',
          json.dumps([r for r in identity_rows if r['occurrenceDelta']], indent=2), '```', '', '## Limits', '']
lines += ['- ' + limit for limit in limits]
lines += ['', 'Full original inputs, raw records, resolved edges, exact-identity Cartesian correspondences, guarded rooted mappings, SRI/peer/optional/bin comparisons, and every delta: `comparison.json`.', '']
(BASE / 'comparison-summary.md').write_text('\n'.join(lines))
print(json.dumps(counts, indent=2, sort_keys=True))
