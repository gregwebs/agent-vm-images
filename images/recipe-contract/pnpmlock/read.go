package pnpmlock

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
)

const (
	// pinFile is the reviewed-input digest list the readers verify.
	pinFile = "pnpm-inputs.sha256"
	// maxDepth bounds the YAML walk and the peer-context parse.
	maxDepth = 100
)

// PinnedInputs is the reviewed input order the pin file records.
var PinnedInputs = []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"}

// manifestKeys are the only manifest keys a reviewed project may carry.
var manifestKeys = []string{"name", "version", "private", "type", "description", "license", "packageManager", "dependencies"}

var (
	pinLinePattern   = regexp.MustCompile(`^([a-f0-9]{64}) {2}([a-z.-]+)$`)
	integrityPattern = regexp.MustCompile(`^sha512-[A-Za-z0-9+/]{86}==$`)
	peerForbidden    = regexp.MustCompile("[:/@\\\\\r\n]")
)

// pinCheck replaces the retired verifyPins boolean.
type pinCheck uint8

const (
	verifyPins pinCheck = iota
	skipPins
)

// readMode replaces the retired tentative/report booleans.
type readMode uint8

const (
	readLocked readMode = iota
	readTentative
	readReport
)

// ReadProject reads and fully validates a locked project.
func ReadProject(prefix Prefix, kind Kind) (*Project, error) {
	return readProjectInputs(prefix, kind, verifyPins, readLocked)
}

// ReadDshProject reads and validates a locked dsh project, the only form the
// peer-sensitive and comparison operations accept.
func ReadDshProject(prefix Prefix) (*DshProject, error) {
	project, err := readProjectInputs(prefix, KindDsh, verifyPins, readLocked)
	if err != nil {
		return nil, err
	}
	return &DshProject{project: project}, nil
}

func readProjectInputs(prefix Prefix, kind Kind, pins pinCheck, mode readMode) (*Project, error) {
	root := kind.Root()
	if pins == verifyPins {
		if err := verifyPinnedInputs(prefix); err != nil {
			return nil, err
		}
	}
	for _, file := range []string{".npmrc", ".pnpmfile.cjs", ".pnpmfile.js", ".pnpmfile.mjs", "pnpmfile.cjs", "package-lock.json"} {
		present, err := nodefs.Present(filepath.Join(string(prefix), file))
		if err != nil {
			return nil, err
		}
		if present {
			return nil, errorf("unreviewed input: %s", file)
		}
	}
	manifest, err := document.ReadFile(filepath.Join(string(prefix), "package.json"), document.JSON)
	if err != nil {
		return nil, err
	}
	if _, err := recordOf(manifest); err != nil {
		return nil, err
	}
	if err := checkKeys(manifest, manifestKeys, []string{"name", "private", "packageManager", "dependencies"}); err != nil {
		return nil, err
	}
	if _, err := packageNameOf(manifest.Get("name")); err != nil {
		return nil, err
	}
	if version := manifest.Get("version"); !isUndefined(version) {
		if _, err := exactVersionOf(version); err != nil {
			return nil, err
		}
	}
	if manifestType := manifest.Get("type"); !isUndefined(manifestType) {
		if text, ok := manifestType.Text(); !ok || text.Display() != "module" {
			return nil, errorf("manifest.type: expected \"module\", got %s", manifestType.Describe())
		}
	}
	for _, field := range []string{"description", "license"} {
		if value := manifest.Get(field); !isUndefined(value) {
			if _, err := textOf(value); err != nil {
				return nil, err
			}
		}
	}
	if !isTrue(manifest.Get("private")) {
		return nil, errorf("manifest.private: expected true, got %s", manifest.Get("private").Describe())
	}
	if text, ok := manifest.Get("packageManager").Text(); !ok || text.Display() != "pnpm@11.11.0" {
		return nil, errorf("manifest.packageManager: expected \"pnpm@11.11.0\", got %s", manifest.Get("packageManager").Describe())
	}
	dependencies, err := recordOf(manifest.Get("dependencies"))
	if err != nil {
		return nil, err
	}
	if err := checkKeys(dependencies, []string{root.String()}, []string{root.String()}); err != nil {
		return nil, err
	}
	selected, err := exactVersionOf(dependencies.Get(root.String()))
	if err != nil {
		return nil, err
	}
	selection := Selection{Kind: kind, Version: selected}
	workspace, err := document.ReadFile(filepath.Join(string(prefix), "pnpm-workspace.yaml"), document.YAML)
	if err != nil {
		return nil, err
	}
	if err := validateWorkspace(workspace, kind, mode); err != nil {
		return nil, err
	}
	lock, err := document.ReadFile(filepath.Join(string(prefix), "pnpm-lock.yaml"), document.YAML)
	if err != nil {
		return nil, err
	}
	if _, err := recordOf(lock); err != nil {
		return nil, err
	}
	if err := checkKeys(lock,
		[]string{"lockfileVersion", "settings", "overrides", "ignoredOptionalDependencies", "importers", "packages", "snapshots"},
		[]string{"lockfileVersion", "settings", "importers", "packages", "snapshots"}); err != nil {
		return nil, err
	}
	if jsStringOf(lock.Get("lockfileVersion")) != "9.0" {
		return nil, errorf("requires v9 lock")
	}
	settings, err := recordOf(lock.Get("settings"))
	if err != nil {
		return nil, err
	}
	if err := checkKeys(settings, []string{"autoInstallPeers", "excludeLinksFromLockfile"}, []string{"autoInstallPeers", "excludeLinksFromLockfile"}); err != nil {
		return nil, err
	}
	if !strictEqual(settings.Get("autoInstallPeers"), workspace.Get("autoInstallPeers")) {
		return nil, errorf("settings.autoInstallPeers: expected %s, got %s",
			workspace.Get("autoInstallPeers").Describe(), settings.Get("autoInstallPeers").Describe())
	}
	if !strictEqual(settings.Get("excludeLinksFromLockfile"), boolValue(false)) {
		return nil, errorf("settings.excludeLinksFromLockfile: expected false, got %s",
			settings.Get("excludeLinksFromLockfile").Describe())
	}
	if kind == KindDsh {
		if !deepEqual(lock.Get("overrides"), workspace.Get("overrides")) {
			return nil, errorf("lock overrides differ")
		}
	} else if !isUndefined(lock.Get("overrides")) {
		return nil, errorf("lock.overrides: expected undefined, got %s", lock.Get("overrides").Describe())
	}
	if ignored := lock.Get("ignoredOptionalDependencies"); !isUndefined(ignored) {
		if !deepEqual(ignored, workspace.Get("ignoredOptionalDependencies")) {
			return nil, errorf("lock.ignoredOptionalDependencies: expected %s, got %s",
				workspace.Get("ignoredOptionalDependencies").Describe(), ignored.Describe())
		}
	}
	importers, err := recordOf(lock.Get("importers"))
	if err != nil {
		return nil, err
	}
	if err := checkKeys(importers, []string{"."}, []string{"."}); err != nil {
		return nil, err
	}
	importer, err := recordOf(importers.Get("."))
	if err != nil {
		return nil, err
	}
	if err := checkKeys(importer, []string{"dependencies"}, []string{"dependencies"}); err != nil {
		return nil, err
	}
	imported, err := recordOf(importer.Get("dependencies"))
	if err != nil {
		return nil, err
	}
	if err := checkKeys(imported, []string{root.String()}, []string{root.String()}); err != nil {
		return nil, err
	}
	resolved, err := recordOf(imported.Get(root.String()))
	if err != nil {
		return nil, err
	}
	if err := checkKeys(resolved, []string{"specifier", "version"}, []string{"specifier", "version"}); err != nil {
		return nil, err
	}
	if specifier, ok := resolved.Get("specifier").Text(); !ok || !specifier.Equal(document.MustStringFromUTF8(selection.Version.String())) {
		return nil, errorf("importer specifier mismatch")
	}
	resolvedText, err := textOf(resolved.Get("version"))
	if err != nil {
		return nil, err
	}
	rootID, err := parseSnapshotID(root.String() + "@" + displayOf(resolvedText))
	if err != nil {
		return nil, err
	}
	if rootID.Package.Version != selection.Version {
		return nil, errorf("importer resolution mismatch")
	}
	archives, err := recordOf(lock.Get("packages"))
	if err != nil {
		return nil, err
	}
	packages, err := readArchives(archives, rootID)
	if err != nil {
		return nil, err
	}
	snapshotsValue, err := recordOf(lock.Get("snapshots"))
	if err != nil {
		return nil, err
	}
	snapshots, err := readSnapshots(snapshotsValue, archives)
	if err != nil {
		return nil, err
	}
	project := &Project{
		prefix: prefix, selection: selection, mode: mode,
		manifest: manifest, workspace: workspace, lock: lock,
		packages: packages, snapshots: snapshots,
		importer: []Edge{{Name: root, Target: rootID.Key}},
	}
	switch mode {
	case readTentative:
		if kind != KindDsh {
			return nil, errorf("unknown consumer kind")
		}
		if err := validateGraph(project, selection); err != nil {
			return nil, err
		}
		if err := validateContextEdges(project); err != nil {
			return nil, err
		}
		return project, nil
	case readReport:
		if err := validateGraph(project, selection); err != nil {
			return nil, err
		}
		if err := validateContextEdges(project); err != nil {
			return nil, err
		}
		return project, nil
	case readLocked:
		if err := validateProject(project, selection); err != nil {
			return nil, err
		}
		return project, nil
	default:
		return nil, errorf("unknown read mode")
	}
}

func readArchives(archives document.Value, rootID SnapshotID) ([]Archive, error) {
	keys := sortedKeys(archives)
	packages := make([]Archive, 0, len(keys))
	for _, key := range keys {
		archiveKey := key.Display()
		id, err := parseSnapshotID(archiveKey)
		if err != nil {
			return nil, err
		}
		if len(id.Context) != 0 {
			return nil, errorf("archive cannot have peer context")
		}
		record, err := recordOf(archives.GetJS(key))
		if err != nil {
			return nil, err
		}
		if err := checkKeys(record,
			[]string{"resolution", "engines", "cpu", "os", "libc", "deprecated", "hasBin", "peerDependencies", "peerDependenciesMeta", "optional", "bundledDependencies"},
			[]string{"resolution"}); err != nil {
			return nil, err
		}
		resolution, err := recordOf(record.Get("resolution"))
		if err != nil {
			return nil, err
		}
		if err := checkKeys(resolution, []string{"integrity", "tarball"}, []string{"integrity"}); err != nil {
			return nil, err
		}
		integrityText, err := textOf(resolution.Get("integrity"))
		if err != nil {
			return nil, err
		}
		integrity, err := parseIntegrity(displayOf(integrityText))
		if err != nil {
			return nil, errorf("%s: full sha512 required", archiveKey)
		}
		tarball := canonicalTarball(id.Package)
		if declared := resolution.Get("tarball"); !isUndefined(declared) {
			if text, ok := declared.Text(); !ok || text.Display() != tarball {
				return nil, errorf("only canonical npmjs tarball allowed")
			}
		}
		peers, err := readPeers(record, archiveKey)
		if err != nil {
			return nil, err
		}
		optionalPeers, err := readOptionalPeers(record, peers, archiveKey)
		if err != nil {
			return nil, err
		}
		packages = append(packages, Archive{
			ID: id.Package, Integrity: integrity, Tarball: tarball,
			Peers: peers, OptionalPeers: optionalPeers,
		})
	}
	return packages, nil
}

func readPeers(record document.Value, archiveKey string) (PeerDeclarations, error) {
	declared := emptyForNull(record.Get("peerDependencies"))
	if _, err := recordOf(declared); err != nil {
		return PeerDeclarations{}, err
	}
	var declarations []PeerDeclaration
	for _, key := range sortedKeys(declared) {
		wanted, err := textOf(declared.GetJS(key))
		if err != nil {
			return PeerDeclarations{}, err
		}
		specification := displayOf(wanted)
		if specification == "" || peerForbidden.MatchString(specification) {
			return PeerDeclarations{}, errorf("%s: non-registry peer specification", archiveKey)
		}
		name, err := ParsePackageName(key.Display())
		if err != nil {
			return PeerDeclarations{}, err
		}
		declarations = append(declarations, PeerDeclaration{Peer: name, Specification: specification})
	}
	return PeerDeclarations{declarations: declarations}, nil
}

func readOptionalPeers(record document.Value, peers PeerDeclarations, archiveKey string) ([]PackageName, error) {
	metaValue := emptyForNull(record.Get("peerDependenciesMeta"))
	if _, err := recordOf(metaValue); err != nil {
		return nil, err
	}
	var optional []PackageName
	for _, key := range metaValue.Keys() {
		name, err := ParsePackageName(key.Display())
		if err != nil {
			return nil, err
		}
		if _, declared := peers.Get(name); !declared {
			return nil, errorf("peer metadata without declaration")
		}
		meta, err := recordOf(metaValue.GetJS(key))
		if err != nil {
			return nil, err
		}
		if err := checkKeys(meta, []string{"optional"}, []string{"optional"}); err != nil {
			return nil, err
		}
		if !isTrue(meta.Get("optional")) {
			return nil, errorf("%s: peerDependenciesMeta.%s.optional: expected true, got %s",
				archiveKey, name.String(), meta.Get("optional").Describe())
		}
		optional = append(optional, name)
	}
	sortPackageNames(optional)
	return optional, nil
}

func readSnapshots(snapshots document.Value, archives document.Value) ([]Snapshot, error) {
	keys := sortedKeys(snapshots)
	result := make([]Snapshot, 0, len(keys))
	for _, key := range keys {
		snapshotKey := key.Display()
		id, err := parseSnapshotID(snapshotKey)
		if err != nil {
			return nil, err
		}
		if !archives.Has(id.Package.Key()) {
			return nil, errorf("%s: missing archive", snapshotKey)
		}
		entry, err := recordOf(snapshots.GetJS(key))
		if err != nil {
			return nil, err
		}
		if err := checkKeys(entry, []string{"dependencies", "optionalDependencies", "transitivePeerDependencies", "optional"}, nil); err != nil {
			return nil, err
		}
		if optional := entry.Get("optional"); !isUndefined(optional) {
			if _, ok := optional.Bool(); !ok {
				return nil, errorf("%s: optional: expected boolean, got %s", snapshotKey, optional.Describe())
			}
		}
		if transitive := entry.Get("transitivePeerDependencies"); !isUndefined(transitive) {
			if transitive.Kind() != document.Sequence {
				return nil, errorf("%s: transitivePeerDependencies: expected sequence, got %s", snapshotKey, transitive.Describe())
			}
			for _, item := range transitive.Items() {
				if _, err := packageNameOf(item); err != nil {
					return nil, err
				}
			}
		}
		var edges []Edge
		seen := map[string]bool{}
		for _, field := range []struct {
			name     string
			optional bool
		}{{"dependencies", false}, {"optionalDependencies", true}} {
			fieldValue := emptyForNull(entry.Get(field.name))
			if _, err := recordOf(fieldValue); err != nil {
				return nil, err
			}
			for _, dependencyKey := range fieldValue.Keys() {
				dependency, err := ParsePackageName(dependencyKey.Display())
				if err != nil {
					return nil, err
				}
				if seen[dependency.String()] {
					return nil, errorf("%s: ambiguous dependency %s", snapshotKey, dependency.String())
				}
				seen[dependency.String()] = true
				targetText, err := textOf(fieldValue.GetJS(dependencyKey))
				if err != nil {
					return nil, err
				}
				targetID, err := parseSnapshotID(dependency.String() + "@" + displayOf(targetText))
				if err != nil {
					return nil, err
				}
				edges = append(edges, Edge{Name: dependency, Target: targetID.Key, Optional: field.optional})
			}
		}
		sortEdges(edges)
		result = append(result, Snapshot{ID: id, Edges: edges})
	}
	return result, nil
}

// emptyForNull implements `value ?? {}`: an absent or null value reads as empty.
func emptyForNull(value document.Value) document.Value {
	if isUndefined(value) || value.Kind() == document.Null {
		return mappingValue()
	}
	return value
}

func parseIntegrity(text string) (Integrity, error) {
	if !integrityPattern.MatchString(text) {
		return Integrity{}, errorf("malformed integrity")
	}
	decoded, err := base64.StdEncoding.DecodeString(text[len("sha512-"):])
	if err != nil || len(decoded) != 64 {
		return Integrity{}, errorf("malformed integrity")
	}
	if base64.StdEncoding.EncodeToString(decoded) != text[len("sha512-"):] {
		return Integrity{}, errorf("malformed integrity")
	}
	return Integrity{value: text}, nil
}

func canonicalTarball(id PackageID) string {
	parts := strings.Split(id.Name.String(), "/")
	return "https://registry.npmjs.org/" + id.Name.String() + "/-/" + parts[len(parts)-1] + "-" + id.Version.String() + ".tgz"
}

func sortPackageNames(names []PackageName) {
	for index := 1; index < len(names); index++ {
		for cursor := index; cursor > 0 && names[cursor].String() < names[cursor-1].String(); cursor-- {
			names[cursor], names[cursor-1] = names[cursor-1], names[cursor]
		}
	}
}

// sortEdges is canonicalEdges: by name, then non-optional before optional, and
// stable for equal keys.
func sortEdges(edges []Edge) {
	for index := 1; index < len(edges); index++ {
		for cursor := index; cursor > 0 && edgeLess(edges[cursor], edges[cursor-1]); cursor-- {
			edges[cursor], edges[cursor-1] = edges[cursor-1], edges[cursor]
		}
	}
}

func edgeLess(a, b Edge) bool {
	if a.Name.String() != b.Name.String() {
		return a.Name.String() < b.Name.String()
	}
	if a.Optional != b.Optional {
		return !a.Optional
	}
	return false
}

func verifyPinnedInputs(prefix Prefix) error {
	source, err := nodefs.ReadBounded(filepath.Join(string(prefix), pinFile))
	if err != nil {
		return err
	}
	entries := map[string]string{}
	for _, line := range strings.Split(string(source), "\n") {
		if line == "" {
			continue
		}
		match := pinLinePattern.FindStringSubmatch(line)
		if match == nil {
			return errorf("malformed input pin")
		}
		if _, exists := entries[match[2]]; exists {
			return errorf("duplicate input pin")
		}
		entries[match[2]] = match[1]
	}
	recorded := make([]string, 0, len(entries))
	for name := range entries {
		recorded = append(recorded, name)
	}
	sortStrings(recorded)
	expected := append([]string(nil), PinnedInputs...)
	sortStrings(expected)
	if strings.Join(recorded, "\x00") != strings.Join(expected, "\x00") {
		return errorf("pins must cover exactly three input files")
	}
	for _, file := range PinnedInputs {
		digest, err := inputDigest(prefix, file)
		if err != nil {
			return err
		}
		if digest != entries[file] {
			return errorf("%s: bytes differ from reviewed pin", file)
		}
	}
	return nil
}

func inputDigest(prefix Prefix, file string) (string, error) {
	source, err := nodefs.ReadBounded(filepath.Join(string(prefix), file))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:]), nil
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

// jsStringOf is JavaScript `String(value)` for the lockfile-version comparison.
func jsStringOf(value document.Value) string {
	switch value.Kind() {
	case document.String:
		text, _ := value.Text()
		return text.Display()
	case document.Number:
		number, _ := value.Number()
		return string(appendNumber(nil, number))
	case document.Bool:
		boolean, _ := value.Bool()
		if boolean {
			return "true"
		}
		return "false"
	case document.Null:
		return "null"
	case document.Undefined:
		return "undefined"
	case document.Sequence:
		parts := make([]string, 0, len(value.Items()))
		for _, item := range value.Items() {
			parts = append(parts, jsStringOf(item))
		}
		return strings.Join(parts, ",")
	case document.Mapping:
		return "[object Object]"
	default:
		panic("pnpmlock: unhandled value kind")
	}
}

func appendNumber(dst []byte, number float64) []byte {
	return document.NewNumber(number).AppendJS(dst)
}
