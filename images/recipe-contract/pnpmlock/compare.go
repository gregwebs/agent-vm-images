package pnpmlock

import (
	"bytes"
	"path/filepath"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
)

// RequestedSelection is the version an update asks for; an empty argument keeps
// the committed selection instead of naming one.
type RequestedSelection struct{ version ExactVersion }

// ParseRequestedSelection reads the CLI argument.
func ParseRequestedSelection(text string) (RequestedSelection, error) {
	if text == "" {
		return RequestedSelection{}, nil
	}
	version, err := ParseExactVersion(text)
	if err != nil {
		return RequestedSelection{}, err
	}
	return RequestedSelection{version: version}, nil
}

// Version reports the requested version, and false when the committed one is kept.
func (r RequestedSelection) Version() (ExactVersion, bool) { return r.version, r.version.value != "" }

// RefreshMode replaces the retired refresh boolean.
type RefreshMode uint8

const (
	NoRefresh RefreshMode = iota
	RefreshLock
)

// UpdateMode is the comparison outcome.
type UpdateMode uint8

const (
	ModeEqual UpdateMode = iota
	ModeChanged
	ModeRefresh
)

func (m UpdateMode) String() string {
	switch m {
	case ModeEqual:
		return "equal"
	case ModeChanged:
		return "changed"
	case ModeRefresh:
		return "refresh"
	default:
		panic("pnpmlock: unhandled update mode")
	}
}

// UpdateRequest names the two projects, the requested version and the mode.
type UpdateRequest struct {
	Kind      Kind
	Before    Prefix
	After     Prefix
	Requested RequestedSelection
	Refresh   RefreshMode
}

// UpdateComparison is the ADR 0003 comparison result.
type UpdateComparison struct {
	Mode              UpdateMode
	BeforeVersion     ExactVersion
	AfterVersion      ExactVersion
	AddedArchives     []string
	RemovedArchives   []string
	RetainedArchives  []string
	AddedSnapshots    []string
	RemovedSnapshots  []string
	RetainedSnapshots []string
}

// Value renders the comparison in the retired key order.
func (c UpdateComparison) Value() document.Value {
	return mappingValue(
		entry("mode", jsStringValue(c.Mode.String())),
		entry("beforeVersion", jsStringValue(c.BeforeVersion.String())),
		entry("afterVersion", jsStringValue(c.AfterVersion.String())),
		entry("addedArchives", stringList(c.AddedArchives)),
		entry("removedArchives", stringList(c.RemovedArchives)),
		entry("retainedArchives", stringList(c.RetainedArchives)),
		entry("addedSnapshots", stringList(c.AddedSnapshots)),
		entry("removedSnapshots", stringList(c.RemovedSnapshots)),
		entry("retainedSnapshots", stringList(c.RetainedSnapshots)),
	)
}

// AppendJS emits JSON.stringify(comparison).
func (c UpdateComparison) AppendJS(dst []byte) []byte { return c.Value().AppendJS(dst) }

func stringList(values []string) document.Value {
	items := make([]document.Value, 0, len(values))
	for _, value := range values {
		items = append(items, jsStringValue(value))
	}
	return sequenceValue(items)
}

// CompareDshUpdate applies the ADR 0003 update rules to two independently
// validated dsh projects and returns the comparison plus the validated after
// side.
func CompareDshUpdate(request UpdateRequest) (UpdateComparison, *DshProject, error) {
	before, err := ReadDshProject(request.Before)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	after, err := ReadDshProject(request.After)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	return compareDshProjects(request, before, after)
}

// CompareDshUpdateArgument preserves the retired CLI's read-before-version-check
// order without allowing unvalidated selections into the comparator.
func CompareDshUpdateArgument(request UpdateRequest, argument string) (UpdateComparison, *DshProject, error) {
	before, err := ReadDshProject(request.Before)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	after, err := ReadDshProject(request.After)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	request.Requested, err = ParseRequestedSelection(argument)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	return compareDshProjects(request, before, after)
}

func compareDshProjects(request UpdateRequest, before, after *DshProject) (UpdateComparison, *DshProject, error) {
	comparison, err := compareSelection(KindDsh, projectSide(before.project), projectSide(after.project),
		request.Requested, request.Refresh, RetainedSnapshotFreeze)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	return comparison, after, nil
}

// CompareSelectionUpdate applies the ADR 0003 rules to two independently
// validated reviewed projects of one consumer kind. It is the post-pin entry; a
// private candidate enters through CompareSelectionCandidate instead.
func CompareSelectionUpdate(request UpdateRequest) (UpdateComparison, *Project, error) {
	return readAndCompareSelection(request, nil)
}

// CompareSelectionUpdateArgument preserves the retired read-before-version-check
// order: a broken project wins over an invalid version argument.
func CompareSelectionUpdateArgument(request UpdateRequest, argument string) (UpdateComparison, *Project, error) {
	return readAndCompareSelection(request, &argument)
}

func readAndCompareSelection(request UpdateRequest, argument *string) (UpdateComparison, *Project, error) {
	before, err := ReadProject(request.Before, request.Kind)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	after, err := ReadProject(request.After, request.Kind)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	if argument != nil {
		if request.Requested, err = ParseRequestedSelection(*argument); err != nil {
			return UpdateComparison{}, nil, err
		}
	}
	comparison, err := compareSelection(request.Kind, projectSide(before), projectSide(after),
		request.Requested, request.Refresh, RetainedSnapshotFreeze)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	return comparison, after, nil
}

// SelectionRequest is the closed comparison input: the requested exact version
// (empty keeps the committed one), whether a developer asked to refresh, and
// which snapshot authority the request names. It carries no caller-supplied
// policy switch: the authority is a typed value, and its zero value is the
// retained freeze every existing path keeps using.
type SelectionRequest struct {
	Requested RequestedSelection
	Refresh   RefreshMode
	Authority SnapshotAuthority
}

// SnapshotAuthority names who decides what a retained snapshot record may say.
//
// The default is the retained freeze: once a project is reviewed, the same
// name@version snapshot may not acquire, lose or rewrite edges, because the old
// selection is the authority for those edges. That is right for an ordinary
// update and for an initial adoption.
//
// A changed Pi selection whose authenticated archive carries its own
// npm-shrinkwrap.json is different: the upstream shrinkwrap, not the previous
// selection, is the authority for the graph. The preparation driver converts
// that shrinkwrap and compares the imported graph against it separately, so the
// frozen old edges must not be substituted for it. Archives stay frozen in both
// modes (an SRI for the same name@version is immutable), as do the manifest,
// workspace and manager invariants and the closure rules for additions and
// removals.
type SnapshotAuthority uint8

const (
	// RetainedSnapshotFreeze is the ordinary comparator: reviewed edges are frozen.
	RetainedSnapshotFreeze SnapshotAuthority = iota
	// ShrinkwrapAuthority is only reachable through the authenticated
	// selected-shrinkwrap route, which imposes its own upstream comparator.
	ShrinkwrapAuthority
)

// comparisonSide is the pure comparison view shared by a reviewed Project and a
// captured candidate. readInput exists only for the equal/no-refresh byte rule,
// and inputNames names the files that rule compares.
type comparisonSide struct {
	selection Selection
	manifest  document.Value
	workspace []byte
	lock      document.Value
	snapshots []Snapshot
	importer  []Edge
	readInput func(name string) ([]byte, error)
	// inputNames is the side's own file set; the equal rule compares over it.
	inputNames []string
}

func projectSide(project *Project) comparisonSide {
	prefix := project.prefix
	names := append(append([]string{}, PinnedInputs...), pinFile)
	return comparisonSide{
		selection: project.selection,
		manifest:  project.manifest,
		workspace: project.workspaceBytes,
		lock:      project.lock,
		snapshots: project.snapshots,
		importer:  project.importer,
		readInput: func(name string) ([]byte, error) {
			return nodefs.ReadBounded(filepath.Join(string(prefix), name))
		},
		inputNames: names,
	}
}

func candidateSide(candidate *ValidatedCandidate) comparisonSide {
	graph := candidate.Graph()
	return comparisonSide{
		selection:  graph.Selection,
		manifest:   candidate.manifest,
		workspace:  candidate.workspace,
		lock:       candidate.lock,
		snapshots:  graph.Snapshots,
		importer:   graph.Importer,
		readInput:  candidate.inputBytesFor,
		inputNames: PinnedInputs,
	}
}

func compareSelection(kind Kind, before, after comparisonSide, requested RequestedSelection, refresh RefreshMode, authority SnapshotAuthority) (UpdateComparison, error) {
	if before.selection.Kind != kind || after.selection.Kind != kind {
		return UpdateComparison{}, errorf("selection kind differs from the requested kind")
	}
	// Only pi publishes an authenticated dependency shrinkwrap, so only pi can
	// name that shrinkwrap as the authority for its graph. For another kind the
	// mode would silently replace the retained-edge freeze with nothing.
	if authority == ShrinkwrapAuthority && kind != KindPi {
		return UpdateComparison{}, errorf("shrinkwrap authority is only available for the %s kind", KindPi)
	}
	version := before.selection.Version
	if explicit, ok := requested.Version(); ok {
		version = explicit
	}
	if after.selection.Version != version {
		return UpdateComparison{}, errorf("after selection differs from request")
	}
	changed := before.selection.Version != version
	requestedRefresh := refresh == RefreshLock
	root := kind.Root().String()

	beforeManifest, err := normalizedManifest(before.manifest, changed, root)
	if err != nil {
		return UpdateComparison{}, err
	}
	afterManifest, err := normalizedManifest(after.manifest, changed, root)
	if err != nil {
		return UpdateComparison{}, err
	}
	if !deepEqual(afterManifest, beforeManifest) {
		return UpdateComparison{}, errorf("only %s manifest selection may change", kind.String())
	}
	if !bytes.Equal(before.workspace, after.workspace) {
		return UpdateComparison{}, errorf("workspace bytes changed")
	}
	beforeHeader := lockHeader(before.lock, changed || requestedRefresh, root)
	afterHeader := lockHeader(after.lock, changed || requestedRefresh, root)
	if !deepEqual(afterHeader, beforeHeader) {
		return UpdateComparison{}, errorf("lock settings/overrides/importer changed")
	}
	if !changed && !requestedRefresh {
		for _, file := range after.inputNames {
			beforeBytes, err := before.readInput(file)
			if err != nil {
				return UpdateComparison{}, err
			}
			afterBytes, err := after.readInput(file)
			if err != nil {
				return UpdateComparison{}, err
			}
			if !bytes.Equal(beforeBytes, afterBytes) {
				return UpdateComparison{}, errorf("%s: equal selection requires identical bytes", file)
			}
		}
	}
	oldClosure, err := closure(before.snapshots, before.importer)
	if err != nil {
		return UpdateComparison{}, err
	}
	newClosure, err := closure(after.snapshots, after.importer)
	if err != nil {
		return UpdateComparison{}, err
	}
	mode := ModeEqual
	switch {
	case requestedRefresh:
		mode = ModeRefresh
	case changed:
		mode = ModeChanged
	}
	result := UpdateComparison{
		Mode:          mode,
		BeforeVersion: before.selection.Version,
		AfterVersion:  after.selection.Version,
	}
	for _, field := range []struct {
		name         string
		label        string
		added        *[]string
		removed      *[]string
		retained     *[]string
		oldRecords   document.Value
		newRecords   document.Value
		closure      func(string) bool
		oldClosure   func(string) bool
		retainedWord string
	}{
		{
			name: "packages", label: "Archives",
			added: &result.AddedArchives, removed: &result.RemovedArchives, retained: &result.RetainedArchives,
			oldRecords: before.lock.Get("packages"), newRecords: after.lock.Get("packages"),
			closure: newClosure.archives.contains, oldClosure: oldClosure.archives.contains,
			retainedWord: "archive",
		},
		{
			name: "snapshots", label: "Snapshots",
			added: &result.AddedSnapshots, removed: &result.RemovedSnapshots, retained: &result.RetainedSnapshots,
			oldRecords: before.lock.Get("snapshots"), newRecords: after.lock.Get("snapshots"),
			closure: newClosure.snapshots.contains, oldClosure: oldClosure.snapshots.contains,
			retainedWord: "snapshot",
		},
	} {
		oldKeys := keyTexts(field.oldRecords)
		newKeys := keyTexts(field.newRecords)
		*field.added = missingFrom(newKeys, oldKeys)
		*field.removed = missingFrom(oldKeys, newKeys)
		*field.retained = presentInBoth(oldKeys, newKeys)
		for _, key := range *field.added {
			if !field.closure(key) {
				return UpdateComparison{}, errorf("addition outside changed root closure: %s", key)
			}
		}
		for _, key := range *field.removed {
			if !field.oldClosure(key) {
				return UpdateComparison{}, errorf("removal outside changed root closure: %s", key)
			}
		}
		for _, key := range *field.retained {
			// Refresh may re-select transitive snapshots, never substitute a retained
			// archive: npm versions are immutable, so a different SRI/tarball/metadata
			// for the same name@version is a different artifact, not a refresh.
			if requestedRefresh && field.name == "snapshots" {
				continue
			}
			// The authenticated selected shrinkwrap is the authority for a changed
			// Pi graph, so its snapshot records are compared by the upstream
			// comparator instead of this freeze. Archive records are not relaxed:
			// a changed SRI for the same name@version is still hard here.
			if authority == ShrinkwrapAuthority && field.name == "snapshots" {
				continue
			}
			oldRecord := field.oldRecords.Get(key).Clone()
			newRecord := field.newRecords.Get(key).Clone()
			if field.name == "snapshots" && changed &&
				key == before.importer[0].Target && key == after.importer[0].Target {
				oldRecord = oldRecord.Without("dependencies", "optionalDependencies")
				newRecord = newRecord.Without("dependencies", "optionalDependencies")
			}
			if !deepEqual(newRecord, oldRecord) {
				return UpdateComparison{}, errorf("retained %s changed: %s", field.retainedWord, key)
			}
		}
	}
	return result, nil
}

func normalizedManifest(manifest document.Value, changed bool, root string) (document.Value, error) {
	normalized := manifest.Clone()
	if changed {
		dependencies, err := recordOf(normalized.Get("dependencies"))
		if err != nil {
			return document.Value{}, err
		}
		normalized = normalized.With("dependencies", dependencies.With(root, jsStringValue("<selected>")))
	}
	return normalized, nil
}

func lockHeader(lock document.Value, selected bool, root string) document.Value {
	header := lock.Without("packages", "snapshots")
	if !selected {
		return header
	}
	importers, _ := recordOf(header.Get("importers"))
	dot, _ := recordOf(importers.Get("."))
	dependencies, _ := recordOf(dot.Get("dependencies"))
	dependencies = dependencies.With(root, mappingValue(
		entry("specifier", jsStringValue("<selected>")),
		entry("version", jsStringValue("<selected>")),
	))
	return header.With("importers", importers.With(".", dot.With("dependencies", dependencies)))
}

type closureSet map[string]bool

func (c closureSet) contains(key string) bool { return c[key] }

type projectClosure struct {
	snapshots closureSet
	archives  closureSet
}

func closure(snapshots []Snapshot, importer []Edge) (projectClosure, error) {
	index := indexSnapshots(snapshots)
	visited := closureSet{}
	pending := make([]string, 0, len(importer))
	for _, edge := range importer {
		pending = append(pending, edge.Target)
	}
	for len(pending) > 0 {
		key := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[key] {
			continue
		}
		snapshot, ok := index.byKey[key]
		if !ok {
			return projectClosure{}, errorf("dangling closure: %s", key)
		}
		visited[key] = true
		for _, edge := range snapshot.Edges {
			pending = append(pending, edge.Target)
		}
	}
	archives := closureSet{}
	for key := range visited {
		archives[index.byKey[key].ID.Package.Key()] = true
	}
	return projectClosure{snapshots: visited, archives: archives}, nil
}

func keyTexts(value document.Value) []string {
	keys := value.Keys()
	texts := make([]string, 0, len(keys))
	for _, key := range keys {
		texts = append(texts, key.Display())
	}
	return texts
}

func missingFrom(absentIn, presentIn []string) []string {
	present := make(map[string]bool, len(presentIn))
	for _, key := range presentIn {
		present[key] = true
	}
	result := make([]string, 0, len(absentIn))
	for _, key := range absentIn {
		if !present[key] {
			result = append(result, key)
		}
	}
	sortStrings(result)
	return result
}

func presentInBoth(a, b []string) []string {
	other := make(map[string]bool, len(b))
	for _, key := range b {
		other[key] = true
	}
	result := make([]string, 0, len(a))
	for _, key := range a {
		if other[key] {
			result = append(result, key)
		}
	}
	sortStrings(result)
	return result
}
