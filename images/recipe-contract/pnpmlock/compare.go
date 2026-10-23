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
	requested := before.project.selection.Version
	if version, ok := request.Requested.Version(); ok {
		requested = version
	}
	if after.project.selection.Version != requested {
		return UpdateComparison{}, nil, errorf("after selection differs from request")
	}
	changed := before.project.selection.Version != requested
	refresh := request.Refresh == RefreshLock
	root := KindDsh.Root().String()

	beforeManifest, err := normalizedManifest(before, changed, root)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	afterManifest, err := normalizedManifest(after, changed, root)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	if !deepEqual(afterManifest, beforeManifest) {
		return UpdateComparison{}, nil, errorf("only dsh manifest selection may change")
	}
	beforeWorkspace, err := nodefs.ReadBounded(filepath.Join(string(request.Before), "pnpm-workspace.yaml"))
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	afterWorkspace, err := nodefs.ReadBounded(filepath.Join(string(request.After), "pnpm-workspace.yaml"))
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	if !bytes.Equal(beforeWorkspace, afterWorkspace) {
		return UpdateComparison{}, nil, errorf("workspace bytes changed")
	}
	beforeHeader := lockHeader(before, changed || refresh, root)
	afterHeader := lockHeader(after, changed || refresh, root)
	if !deepEqual(afterHeader, beforeHeader) {
		return UpdateComparison{}, nil, errorf("lock settings/overrides/importer changed")
	}
	if !changed && !refresh {
		for _, file := range append(append([]string{}, PinnedInputs...), pinFile) {
			beforeBytes, err := nodefs.ReadBounded(filepath.Join(string(request.Before), file))
			if err != nil {
				return UpdateComparison{}, nil, err
			}
			afterBytes, err := nodefs.ReadBounded(filepath.Join(string(request.After), file))
			if err != nil {
				return UpdateComparison{}, nil, err
			}
			if !bytes.Equal(beforeBytes, afterBytes) {
				return UpdateComparison{}, nil, errorf("%s: equal selection requires identical bytes", file)
			}
		}
	}
	oldClosure, err := closure(before.project)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	newClosure, err := closure(after.project)
	if err != nil {
		return UpdateComparison{}, nil, err
	}
	mode := ModeEqual
	switch {
	case refresh:
		mode = ModeRefresh
	case changed:
		mode = ModeChanged
	}
	result := UpdateComparison{
		Mode:          mode,
		BeforeVersion: before.project.selection.Version,
		AfterVersion:  after.project.selection.Version,
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
			oldRecords: before.project.lock.Get("packages"), newRecords: after.project.lock.Get("packages"),
			closure: newClosure.archives.contains, oldClosure: oldClosure.archives.contains,
			retainedWord: "archive",
		},
		{
			name: "snapshots", label: "Snapshots",
			added: &result.AddedSnapshots, removed: &result.RemovedSnapshots, retained: &result.RetainedSnapshots,
			oldRecords: before.project.lock.Get("snapshots"), newRecords: after.project.lock.Get("snapshots"),
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
				return UpdateComparison{}, nil, errorf("addition outside changed root closure: %s", key)
			}
		}
		for _, key := range *field.removed {
			if !field.oldClosure(key) {
				return UpdateComparison{}, nil, errorf("removal outside changed root closure: %s", key)
			}
		}
		for _, key := range *field.retained {
			// Refresh may re-select transitive snapshots, never substitute a retained
			// archive: npm versions are immutable, so a different SRI/tarball/metadata
			// for the same name@version is a different artifact, not a refresh.
			if refresh && field.name == "snapshots" {
				continue
			}
			oldRecord := field.oldRecords.Get(key).Clone()
			newRecord := field.newRecords.Get(key).Clone()
			if field.name == "snapshots" && changed &&
				key == before.project.importer[0].Target && key == after.project.importer[0].Target {
				oldRecord = oldRecord.Without("dependencies", "optionalDependencies")
				newRecord = newRecord.Without("dependencies", "optionalDependencies")
			}
			if !deepEqual(newRecord, oldRecord) {
				return UpdateComparison{}, nil, errorf("retained %s changed: %s", field.retainedWord, key)
			}
		}
	}
	return result, after, nil
}

func normalizedManifest(project *DshProject, changed bool, root string) (document.Value, error) {
	manifest := project.project.manifest.Clone()
	if changed {
		dependencies, err := recordOf(manifest.Get("dependencies"))
		if err != nil {
			return document.Value{}, err
		}
		manifest = manifest.With("dependencies", dependencies.With(root, jsStringValue("<selected>")))
	}
	return manifest, nil
}

func lockHeader(project *DshProject, selected bool, root string) document.Value {
	header := project.project.lock.Without("packages", "snapshots")
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

func closure(project *Project) (projectClosure, error) {
	index := indexSnapshots(project.snapshots)
	visited := closureSet{}
	pending := make([]string, 0, len(project.importer))
	for _, edge := range project.importer {
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
