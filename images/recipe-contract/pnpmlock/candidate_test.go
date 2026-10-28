package pnpmlock

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/candidatestage"
)

// mintedCandidate copies a committed fixture's three inputs into the exact
// private stage layout the preparation driver mints.
func mintedCandidate(t *testing.T, fixtureName string) candidatestage.Stage {
	t.Helper()
	scratch, err := os.MkdirTemp(candidatestage.SystemTempDir(), "pnpm-project-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	fd, err := os.Open(scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fd.Close() })
	candidate := filepath.Join(scratch, "candidate")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(fixtureRoot, fixtureName)
	for _, file := range PinnedInputs {
		payload, err := os.ReadFile(filepath.Join(source, file))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(candidate, file), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stage, err := candidatestage.Open(scratch, int(fd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return stage
}

// editCandidate rewrites one captured file after capture and re-captures the
// stage, the way a preparation driver would after generating import output.
func editCandidate(t *testing.T, stage candidatestage.Stage, file string, edit func(string) string) candidatestage.Stage {
	t.Helper()
	path := filepath.Join(stage.Scratch(), "candidate", file)
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := edit(string(payload))
	if edited == string(payload) {
		t.Fatalf("%s: edit did not apply", file)
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(stage.Scratch())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fd.Close() })
	recaptured, err := candidatestage.Open(stage.Scratch(), int(fd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	return recaptured
}

func TestValidateCandidate(t *testing.T) {
	stage := mintedCandidate(t, "tiny-dsh")
	candidate, err := ValidateCandidate(stage, KindDsh)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Kind() != KindDsh {
		t.Errorf("kind = %v", candidate.Kind())
	}
	for _, file := range PinnedInputs {
		payload, err := os.ReadFile(filepath.Join(stage.Scratch(), "candidate", file))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		digest, ok := candidate.InputDigests().Get(file)
		if !ok || digest != hex.EncodeToString(sum[:]) {
			t.Errorf("%s digest = %q, %v", file, digest, ok)
		}
	}
	// Emission order is the pin-file order, not map order.
	printed := string(candidate.InputDigests().Value().AppendJS(nil))
	previous := -1
	for _, file := range PinnedInputs {
		position := strings.Index(printed, `"`+file+`"`)
		if position <= previous {
			t.Fatalf("digest key %q out of order: %s", file, printed)
		}
		previous = position
	}
	if graph := candidate.Graph(); graph.Selection.Version.String() != "0.1.5-rc.2" || len(graph.Packages) == 0 {
		t.Errorf("graph = %+v", graph)
	}
}

func TestValidateCandidateRejectsUnpinnedPolicy(t *testing.T) {
	for _, vector := range []struct {
		name  string
		file  string
		edit  func(string) string
		match string
	}{
		{"workspace", "pnpm-workspace.yaml", func(source string) string {
			return strings.Replace(source, `"hoist": true`, `"hoist": true, "trustLockfile": true`, 1)
		}, "unexpected key"},
		{"manifest", "package.json", func(source string) string {
			return strings.Replace(source, `"private": true,`, `"private": true, "scripts": {},`, 1)
		}, "unexpected key"},
		{"lock", "pnpm-lock.yaml", func(source string) string {
			return strings.Replace(source, `"lockfileVersion": "9.0"`, `"lockfileVersion": "6.0"`, 1)
		}, "requires v9 lock"},
	} {
		t.Run(vector.name, func(t *testing.T) {
			stage := editCandidate(t, mintedCandidate(t, "tiny-dsh"), vector.file, vector.edit)
			if _, err := ValidateCandidate(stage, KindDsh); err == nil || !strings.Contains(err.Error(), vector.match) {
				t.Fatalf("%s = %v, want %q", vector.name, err, vector.match)
			}
		})
	}
}

func TestCompareSelectionCandidate(t *testing.T) {
	before, err := ReadProject(fixture(t, "update-before"), KindDsh)
	if err != nil {
		t.Fatal(err)
	}
	equal, err := ValidateCandidate(mintedCandidate(t, "update-before"), KindDsh)
	if err != nil {
		t.Fatal(err)
	}
	requested, err := ParseRequestedSelection("0.1.5-rc.2")
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := CompareSelectionCandidate(before, equal, SelectionRequest{Requested: requested})
	if err != nil || comparison.Mode != ModeEqual {
		t.Fatalf("equal = %+v, %v", comparison, err)
	}
	refresh, err := CompareSelectionCandidate(before, equal, SelectionRequest{Requested: requested, Refresh: RefreshLock})
	if err != nil || refresh.Mode != ModeRefresh {
		t.Fatalf("refresh = %+v, %v", refresh, err)
	}

	afterVersion, err := ParseRequestedSelection("0.1.5-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ValidateCandidate(mintedCandidate(t, "update-after"), KindDsh)
	if err != nil {
		t.Fatal(err)
	}
	changedComparison, err := CompareSelectionCandidate(before, changed, SelectionRequest{Requested: afterVersion})
	if err != nil || changedComparison.Mode != ModeChanged {
		t.Fatalf("changed = %+v, %v", changedComparison, err)
	}
	if !containsString(changedComparison.AddedSnapshots, "leaf@2.0.0") ||
		!containsString(changedComparison.RemovedSnapshots, "leaf@1.0.0") {
		t.Errorf("delta = %+v", changedComparison)
	}
}

func TestCompareSelectionCandidateRejectsWeakerInput(t *testing.T) {
	before, err := ReadProject(fixture(t, "update-before"), KindDsh)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := ParseRequestedSelection("")
	if err != nil {
		t.Fatal(err)
	}
	for _, vector := range []struct {
		name    string
		fixture string
		file    string
		edit    func(string) string
		version string
		match   string
	}{
		{"workspace-bytes", "update-before", "pnpm-workspace.yaml", func(source string) string { return source + "\n" }, "", "workspace bytes changed"},
		{"equal-input-bytes", "update-before", "package.json", func(source string) string { return source + " " }, "", "identical bytes"},
		{"manifest", "update-before", "package.json", func(source string) string {
			return strings.Replace(source, `"private": true`, `"private": true, "description": "new metadata"`, 1)
		}, "", "only dsh manifest selection may change"},
		{"retained-archive", "update-after", "pnpm-lock.yaml", func(source string) string {
			return strings.Replace(source, `"integrity": "sha512-AAAA`,
				`"integrity": "sha512-AQAA`, strings.Count(source, `"integrity": "sha512-AAAA`)-1)
		}, "0.1.5-rc.1", "retained archive changed"},
	} {
		t.Run(vector.name, func(t *testing.T) {
			stage := editCandidate(t, mintedCandidate(t, vector.fixture), vector.file, vector.edit)
			candidate, err := ValidateCandidate(stage, KindDsh)
			if err != nil {
				t.Fatal(err)
			}
			requested := keep
			if vector.version != "" {
				requested, err = ParseRequestedSelection(vector.version)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = CompareSelectionCandidate(before, candidate, SelectionRequest{Requested: requested})
			if err == nil || !strings.Contains(err.Error(), vector.match) {
				t.Fatalf("%s = %v, want %q", vector.name, err, vector.match)
			}
		})
	}
}

func TestCandidateReportShape(t *testing.T) {
	candidate, err := ValidateCandidate(mintedCandidate(t, "tiny-dsh"), KindDsh)
	if err != nil {
		t.Fatal(err)
	}
	adoption := string(NewCandidateReport(candidate, nil).AppendJS(nil))
	if strings.Contains(adoption, "selectionDelta") {
		t.Errorf("adoption report carries selectionDelta: %s", adoption)
	}
	previous := -1
	for _, key := range []string{"inputState", "inputDigests", "graph"} {
		position := strings.Index(adoption, `"`+key+`":`)
		if position <= previous {
			t.Fatalf("adoption report key %q missing or out of order: %s", key, adoption)
		}
		previous = position
	}
	delta := UpdateComparison{Mode: ModeEqual}
	selected := string(NewCandidateReport(candidate, &delta).AppendJS(nil))
	previous = -1
	for _, key := range []string{"inputState", "inputDigests", "selectionDelta", "graph"} {
		position := strings.Index(selected, `"`+key+`":`)
		if position <= previous {
			t.Fatalf("selection report key %q missing or out of order: %s", key, selected)
		}
		previous = position
	}
}
