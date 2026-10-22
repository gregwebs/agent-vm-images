// Package dshupgrade is the developer staging surface for a dsh upgrade: it
// composes the tentative/final trees, verifies the proof file against an
// independent derivation, and publishes a proposal when a reviewer asks for one.
// Production graph policy stays in pnpmlock.
package dshupgrade

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/tempname"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

// DeriveRequest is one `dsh-cordis-overrides` invocation. Empty Approved and
// Propose name no file.
type DeriveRequest struct {
	Tentative pnpmlock.Prefix
	Before    *pnpmlock.Prefix
	Approved  string
	Propose   string
	Cwd       string
}

// StageRequest is one `dsh-upgrade-stage` invocation. ExtraSet distinguishes an
// absent reviewed map from an empty path.
type StageRequest struct {
	Before   pnpmlock.Prefix
	Stage    string
	Argument string
	Extra    string
	ExtraSet bool
	Cwd      string
}

// CheckFinalReport is the check-final proof a reviewer keeps.
type CheckFinalReport struct {
	Selection    pnpmlock.Selection
	Requirements []pnpmlock.Requirement
}

// Value renders the report in the retired key order.
func (r CheckFinalReport) Value() document.Value {
	requirements := make([]document.Value, 0, len(r.Requirements))
	for _, requirement := range r.Requirements {
		requirements = append(requirements, requirement.Value())
	}
	return document.NewMapping(
		entryOf("selection", r.Selection.Value()),
		entryOf("requirements", document.NewSequence(requirements)),
	)
}

// AppendJS emits JSON.stringify(report).
func (r CheckFinalReport) AppendJS(dst []byte) []byte { return r.Value().AppendJS(dst) }

// Derive runs the derivation and, when asked, publishes the proposal file.
func Derive(request DeriveRequest) (pnpmlock.Derivation, error) {
	reviewed := pnpmlock.ReviewedMaps{}
	if request.Before != nil {
		before, err := pnpmlock.ReadDshProject(*request.Before)
		if err != nil {
			return pnpmlock.Derivation{}, err
		}
		committed, err := before.Overrides()
		if err != nil {
			return pnpmlock.Derivation{}, err
		}
		reviewed.Committed = committed
	}
	approved := pnpmlock.OverrideMap{}
	if request.Approved != "" {
		var err error
		approved, err = pnpmlock.ReadOverrideMap(request.Approved)
		if err != nil {
			return pnpmlock.Derivation{}, err
		}
		reviewed.Approved = approved
	}
	result, err := pnpmlock.DeriveCordisOverrides(request.Tentative, reviewed)
	if err != nil {
		return pnpmlock.Derivation{}, err
	}
	if request.Before != nil {
		delta := make([]pnpmlock.OverrideDelta, 0, len(result.Overrides.Entries()))
		for _, item := range result.Overrides.Entries() {
			if previous, ok := reviewed.Committed.Get(item.Selector); ok && previous == item.Version {
				continue
			}
			row := pnpmlock.OverrideDelta{Selector: item.Selector, After: item.Version}
			if previous, ok := reviewed.Committed.Get(item.Selector); ok {
				kept := previous
				row.Before = &kept
			}
			delta = append(delta, row)
		}
		result.Delta = delta
	}
	if request.Approved != "" && !approved.Equal(result.Overrides) {
		return pnpmlock.Derivation{}, errorf("reviewed map differs from independent tentative requirements")
	}
	if request.Propose != "" {
		if err := publishProposal(request, result, approved); err != nil {
			return pnpmlock.Derivation{}, err
		}
	}
	return result, nil
}

// publishProposal writes the reviewed map to a caller-designated path. The
// publication rules are the retired adapter's: an owned-or-sticky parent, a
// writable parent, no collision with an input, a completed regular file
// published by link so nothing is replaced.
func publishProposal(request DeriveRequest, result pnpmlock.Derivation, approved pnpmlock.OverrideMap) error {
	requested, err := resolveAgainst(request.Cwd, request.Propose)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(requested))
	if err != nil {
		return nodefs.Wrap("realpath", filepath.Dir(requested), err)
	}
	destination := filepath.Join(parent, filepath.Base(requested))
	info, err := os.Stat(parent)
	if err != nil {
		return nodefs.Wrap("stat", parent, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	owned := info.IsDir() && ok && (int(stat.Uid) == os.Getuid() || (stat.Uid == 0 && info.Mode().Perm()&0o1000 != 0))
	if !owned {
		return errorf("proposal parent must be owned or root-owned sticky directory")
	}
	if err := syscall.Access(parent, accessWrite|accessExecute); err != nil {
		return nodefs.Wrap("access", parent, err)
	}
	inputs := []pnpmlock.Prefix{request.Tentative}
	if request.Before != nil {
		inputs = append(inputs, *request.Before)
	}
	for _, prefix := range inputs {
		for _, file := range append(append([]string{}, pnpmlock.PinnedInputs...), "pnpm-inputs.sha256") {
			candidate, err := resolveAgainst(request.Cwd, filepath.Join(string(prefix), file))
			if err != nil {
				return err
			}
			if candidate == destination {
				return errorf("proposal collides with input")
			}
		}
	}
	identity, err := tempname.UUID4()
	if err != nil {
		return err
	}
	temporary := filepath.Join(parent, ".dsh-cordis-"+identity)
	payload := result.Overrides.Value().AppendJSIndent(nil, "  ")
	payload = append(payload, '\n')
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nodefs.Wrap("open", temporary, err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return removeThen(temporary, err)
	}
	if err := file.Close(); err != nil {
		return removeThen(temporary, err)
	}
	if err := os.Link(temporary, destination); err != nil {
		return removeThen(temporary, nodefs.Wrap("link", destination, err))
	}
	if err := os.Remove(temporary); err != nil {
		return nodefs.Wrap("unlink", temporary, err)
	}
	return nil
}

func removeThen(temporary string, cause error) error {
	if err := os.Remove(temporary); err != nil {
		return nodefs.Wrap("unlink", temporary, err)
	}
	return cause
}

// resolveAgainst is path.resolve(F): relative to the physical working directory.
func resolveAgainst(cwd, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	if cwd == "" {
		directory, err := syscall.Getwd()
		if err != nil {
			return "", err
		}
		cwd = directory
	}
	return filepath.Join(cwd, path), nil
}

// StageTentative writes the tentative tree: the target manifest and the
// workspace without overrides.
func StageTentative(request StageRequest) error {
	original, err := pnpmlock.ReadDshProject(request.Before)
	if err != nil {
		return err
	}
	if request.ExtraSet {
		return errorf("expected target exact selection")
	}
	target, err := pnpmlock.ParseExactVersion(request.Argument)
	if err != nil {
		return errorf("expected target exact selection")
	}
	if err := os.Mkdir(request.Stage, 0o777); err != nil {
		return nodefs.Wrap("mkdir", request.Stage, err)
	}
	manifest := original.Manifest().Clone()
	dependencies, err := recordOf(manifest.Get("dependencies"))
	if err != nil {
		return err
	}
	manifest = manifest.With("dependencies", dependencies.With(
		pnpmlock.KindDsh.Root().String(), document.NewString(target.String())))
	payload := append(manifest.AppendJSIndent(nil, "  "), '\n')
	if err := os.WriteFile(filepath.Join(request.Stage, "package.json"), payload, 0o666); err != nil {
		return nodefs.Wrap("open", filepath.Join(request.Stage, "package.json"), err)
	}
	workspace, err := nodefs.ReadBounded(filepath.Join(string(request.Before), "pnpm-workspace.yaml"))
	if err != nil {
		return err
	}
	staged, err := document.DeleteTopLevel(workspace, "overrides")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(request.Stage, "pnpm-workspace.yaml"), staged)
}

// StageFinal writes the final tree from the tentative tree, the committed
// workspace and the committed lock.
func StageFinal(request StageRequest) error {
	original, err := pnpmlock.ReadDshProject(request.Before)
	if err != nil {
		return err
	}
	tentative := filepath.Join(filepath.Dir(request.Argument), "tentative")
	if err := os.Mkdir(request.Stage, 0o777); err != nil {
		return nodefs.Wrap("mkdir", request.Stage, err)
	}
	if err := copyFile(filepath.Join(tentative, "package.json"), filepath.Join(request.Stage, "package.json")); err != nil {
		return err
	}
	proof, err := readProof(request.Argument)
	if err != nil {
		return err
	}
	if request.ExtraSet {
		approved, err := pnpmlock.ReadOverrideMap(request.Extra)
		if err != nil {
			return err
		}
		if !approved.Equal(proof.derivation.Overrides) {
			return errorf("reviewed map differs from independent requirements")
		}
	}
	bytes_, err := nodefs.ReadBounded(filepath.Join(string(request.Before), "pnpm-workspace.yaml"))
	if err != nil {
		return err
	}
	committed, err := original.Overrides()
	if err != nil {
		return err
	}
	if !request.ExtraSet && !proof.derivation.Overrides.Equal(committed) {
		return errorf("Cordis map changed; use --propose-cordis-overrides FILE then --cordis-overrides FILE")
	}
	workspace := bytes_
	if request.ExtraSet && !proofMatchesCommitted(proof.derivation.Overrides, committed) {
		// An explicit review may replace the map; the committed bytes are otherwise
		// the reviewed workspace.
		workspace, err = document.ReplaceTopLevel(bytes_, "overrides", proof.overrides)
		if err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(request.Stage, "pnpm-workspace.yaml"), workspace); err != nil {
		return err
	}
	return copyFile(
		filepath.Join(string(request.Before), "pnpm-lock.yaml"),
		filepath.Join(request.Stage, "pnpm-lock.yaml"))
}

// CheckFinal verifies the staged final tree against an independent derivation of
// its own recorded graph.
func CheckFinal(request StageRequest) (CheckFinalReport, error) {
	original, err := pnpmlock.ReadDshProject(request.Before)
	if err != nil {
		return CheckFinalReport{}, err
	}
	if err := pnpmlock.WritePins(pnpmlock.Prefix(request.Stage), pnpmlock.KindDsh); err != nil {
		return CheckFinalReport{}, err
	}
	project, err := pnpmlock.ReadDshProject(pnpmlock.Prefix(request.Stage))
	if err != nil {
		return CheckFinalReport{}, err
	}
	proof, err := readProof(request.Argument)
	if err != nil {
		return CheckFinalReport{}, err
	}
	check := filepath.Join(filepath.Dir(request.Stage), "final-requirements")
	if err := os.Mkdir(check, 0o777); err != nil {
		return CheckFinalReport{}, nodefs.Wrap("mkdir", check, err)
	}
	if err := copyFile(filepath.Join(request.Stage, "package.json"), filepath.Join(check, "package.json")); err != nil {
		return CheckFinalReport{}, err
	}
	workspaceSource, err := nodefs.ReadBounded(filepath.Join(request.Stage, "pnpm-workspace.yaml"))
	if err != nil {
		return CheckFinalReport{}, err
	}
	lockSource, err := nodefs.ReadBounded(filepath.Join(request.Stage, "pnpm-lock.yaml"))
	if err != nil {
		return CheckFinalReport{}, err
	}
	workspace, err := document.Parse(workspaceSource, document.YAML)
	if err != nil {
		return CheckFinalReport{}, err
	}
	lock, err := document.Parse(lockSource, document.YAML)
	if err != nil {
		return CheckFinalReport{}, err
	}
	workspace = workspace.Without("overrides")
	lock = lock.Without("overrides")
	if err := writeFile(filepath.Join(check, "pnpm-workspace.yaml"), workspace.AppendJS(nil)); err != nil {
		return CheckFinalReport{}, err
	}
	if err := writeFile(filepath.Join(check, "pnpm-lock.yaml"), lock.AppendJS(nil)); err != nil {
		return CheckFinalReport{}, err
	}
	committed, err := original.Overrides()
	if err != nil {
		return CheckFinalReport{}, err
	}
	reviewed := pnpmlock.ReviewedMaps{Committed: committed}
	if request.ExtraSet {
		approved, err := pnpmlock.ReadOverrideMap(request.Extra)
		if err != nil {
			return CheckFinalReport{}, err
		}
		reviewed.Approved = approved
	}
	final, err := pnpmlock.DeriveCordisOverrides(pnpmlock.Prefix(check), reviewed)
	if err != nil {
		return CheckFinalReport{}, err
	}
	if !proof.derivation.Overrides.Equal(final.Overrides) {
		return CheckFinalReport{}, errorf("tentative/final selected family drift")
	}
	before := requirementRows(proof.derivation.Requirements)
	after := requirementRows(final.Requirements)
	if !document.DeepStrictEqual(before, after) {
		return CheckFinalReport{}, errorf("tentative/final peer requirement drift")
	}
	stagedWorkspace, err := nodefs.ReadBounded(filepath.Join(request.Stage, "pnpm-workspace.yaml"))
	if err != nil {
		return CheckFinalReport{}, err
	}
	committedWorkspace, err := nodefs.ReadBounded(filepath.Join(string(request.Before), "pnpm-workspace.yaml"))
	if err != nil {
		return CheckFinalReport{}, err
	}
	if bytes.Equal(stagedWorkspace, committedWorkspace) {
		requested, err := pnpmlock.ParseRequestedSelection(project.Selection().Version.String())
		if err != nil {
			return CheckFinalReport{}, err
		}
		if _, _, err := pnpmlock.CompareDshUpdate(pnpmlock.UpdateRequest{
			Before:    request.Before,
			After:     pnpmlock.Prefix(request.Stage),
			Requested: requested,
			Refresh:   pnpmlock.RefreshLock,
		}); err != nil {
			return CheckFinalReport{}, err
		}
	} else {
		if !request.ExtraSet {
			return CheckFinalReport{}, errorf("changed map requires explicit review")
		}
		approved, err := pnpmlock.ReadOverrideMap(request.Extra)
		if err != nil {
			return CheckFinalReport{}, err
		}
		if !approved.Equal(final.Overrides) {
			return CheckFinalReport{}, errorf("reviewed map differs from final requirements")
		}
	}
	return CheckFinalReport{Selection: project.Selection(), Requirements: final.Requirements}, nil
}

type proofFile struct {
	overrides  document.Value
	derivation pnpmlock.Derivation
}

// readProof reads the tentative proof file. It is plain JSON, not a project
// document: the proof carries the derivation's own shape.
func readProof(file string) (proofFile, error) {
	value, err := document.ReadFile(file, document.JSON)
	if err != nil {
		return proofFile{}, err
	}
	raw := value.Get("overrides")
	overrides, err := pnpmlock.ParseOverrideMap(raw)
	if err != nil {
		return proofFile{}, err
	}
	requirements := make([]pnpmlock.Requirement, 0)
	for _, item := range value.Get("requirements").Items() {
		requirement, err := requirementOf(item)
		if err != nil {
			return proofFile{}, err
		}
		requirements = append(requirements, requirement)
	}
	return proofFile{overrides: raw, derivation: pnpmlock.Derivation{Overrides: overrides, Requirements: requirements}}, nil
}

func requirementOf(value document.Value) (pnpmlock.Requirement, error) {
	if _, err := recordOf(value); err != nil {
		return pnpmlock.Requirement{}, err
	}
	return pnpmlock.ParseRequirement(value)
}

func requirementRows(requirements []pnpmlock.Requirement) document.Value {
	items := make([]document.Value, 0, len(requirements))
	for _, requirement := range requirements {
		if requirement.Kind == pnpmlock.DependencyRequirement {
			continue
		}
		items = append(items, requirement.Value())
	}
	return document.NewSequence(items)
}

func proofMatchesCommitted(proof pnpmlock.OverrideMap, committed pnpmlock.OverrideMap) bool {
	for _, item := range proof.Entries() {
		previous, ok := committed.Get(item.Selector)
		if !ok || previous != item.Version {
			return false
		}
	}
	return true
}

func copyFile(from, to string) error {
	source, err := nodefs.ReadBounded(from)
	if err != nil {
		return err
	}
	return writeFile(to, source)
}

func writeFile(path string, source []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return nodefs.Wrap("open", path, err)
	}
	if _, err := file.Write(source); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func recordOf(value document.Value) (document.Value, error) {
	if !value.IsMapping() {
		return document.Value{}, errorf("expected mapping")
	}
	return value, nil
}

func entryOf(key string, value document.Value) document.NewMappingEntry {
	return document.NewMappingEntry{Key: document.MustStringFromUTF8(key), Value: value}
}

func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }
