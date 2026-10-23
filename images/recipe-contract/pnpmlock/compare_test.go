package pnpmlock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

func TestCompareDshUpdate(t *testing.T) {
	before := fixture(t, "update-before")
	after := fixture(t, "update-after")
	requested, err := ParseRequestedSelection("0.1.5-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	comparison, project, err := CompareDshUpdate(UpdateRequest{Before: before, After: after, Requested: requested})
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Mode != ModeChanged || comparison.BeforeVersion.String() != "0.1.5-rc.2" || comparison.AfterVersion.String() != "0.1.5-rc.1" {
		t.Fatalf("comparison = %+v", comparison)
	}
	if len(comparison.AddedSnapshots) != 2 || !containsString(comparison.AddedSnapshots, "leaf@2.0.0") {
		t.Errorf("added snapshots = %v", comparison.AddedSnapshots)
	}
	if len(comparison.RemovedSnapshots) != 2 || !containsString(comparison.RemovedSnapshots, "leaf@1.0.0") {
		t.Errorf("removed snapshots = %v", comparison.RemovedSnapshots)
	}
	if len(comparison.RetainedSnapshots) == 0 {
		t.Errorf("retained snapshots = %v", comparison.RetainedSnapshots)
	}
	if _, err := project.ExactPeers(); err != nil {
		t.Errorf("after peers: %v", err)
	}
	// The emitted JSON carries the retired key order.
	printed := string(comparison.AppendJS(nil))
	previous := -1
	for _, key := range []string{"mode", "beforeVersion", "afterVersion", "addedArchives", "removedArchives",
		"retainedArchives", "addedSnapshots", "removedSnapshots", "retainedSnapshots"} {
		position := strings.Index(printed, `"`+key+`":`)
		if position <= previous {
			t.Errorf("comparison JSON key %q is missing or out of order: %s", key, printed)
		}
		previous = position
	}
	keep, err := ParseRequestedSelection("")
	if err != nil {
		t.Fatal(err)
	}
	if equal, _, err := CompareDshUpdate(UpdateRequest{Before: before, After: before, Requested: keep}); err != nil || equal.Mode != ModeEqual {
		t.Errorf("equal = %+v, %v", equal, err)
	}
	if refreshed, _, err := CompareDshUpdate(UpdateRequest{Before: before, After: before, Requested: keep, Refresh: RefreshLock}); err != nil || refreshed.Mode != ModeRefresh {
		t.Errorf("refresh = %+v, %v", refreshed, err)
	}
}

func TestCompareRejects(t *testing.T) {
	mutate := func(t *testing.T, prefix Prefix, file string, edit func(string) string) {
		t.Helper()
		path := filepath.Join(string(prefix), file)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := edit(string(source))
		if edited == string(source) {
			t.Fatalf("%s: edit did not apply", file)
		}
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		repin(t, prefix)
	}
	vector := func(t *testing.T) (Prefix, Prefix) {
		return fixture(t, "update-before"), fixture(t, "update-after")
	}
	requested, err := ParseRequestedSelection("0.1.5-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		build func(t *testing.T) (Prefix, Prefix)
		match string
	}{
		{"retained-archive", func(t *testing.T) (Prefix, Prefix) {
			before, after := vector(t)
			mutate(t, after, "pnpm-lock.yaml", func(source string) string {
				return strings.Replace(source, `"integrity": "sha512-AAAA`,
					`"integrity": "sha512-AQAA`, strings.Count(source, `"integrity": "sha512-AAAA`)-1)
			})
			return before, after
		}, "retained archive changed"},
		{"workspace-bytes", func(t *testing.T) (Prefix, Prefix) {
			before, after := vector(t)
			mutate(t, after, "pnpm-workspace.yaml", func(source string) string { return source + "\n" })
			return before, after
		}, "workspace bytes changed"},
		{"manifest", func(t *testing.T) (Prefix, Prefix) {
			before, after := vector(t)
			mutate(t, after, "package.json", func(source string) string {
				return strings.Replace(source, `"private": true`, `"private": true, "description": "new metadata"`, 1)
			})
			return before, after
		}, "only dsh manifest selection may change"},
		{"equal-pin-bytes", func(t *testing.T) (Prefix, Prefix) {
			before := fixture(t, "update-before")
			after := fixture(t, "update-before")
			path := filepath.Join(string(after), pinFile)
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(source, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			return before, after
		}, "identical bytes"},
	} {
		before, after := test.build(t)
		selection := requested
		if test.name == "equal-pin-bytes" {
			selection = RequestedSelection{}
		}
		_, _, err := CompareDshUpdate(UpdateRequest{Before: before, After: after, Requested: selection})
		if test.match == "" {
			if err != nil && !strings.Contains(err.Error(), "identical bytes") {
				t.Errorf("%s = %v", test.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.match) {
			t.Errorf("%s = %v, want %q", test.name, err, test.match)
		}
	}
	before, after := vector(t)
	equalBefore := fixture(t, "update-before")
	if _, _, err := CompareDshUpdate(UpdateRequest{Before: before, After: equalBefore, Requested: RequestedSelection{}}); err != nil {
		t.Errorf("keeping the committed selection = %v", err)
	}
	latest, ok := parseVersionForTest(t, "latest")
	if ok {
		if _, _, err := CompareDshUpdate(UpdateRequest{Before: before, After: after, Requested: latest}); err == nil {
			t.Error("an invalid requested version was accepted")
		}
	}
}

func parseVersionForTest(t *testing.T, text string) (RequestedSelection, bool) {
	t.Helper()
	requested, err := ParseRequestedSelection(text)
	if err != nil {
		return RequestedSelection{}, false
	}
	return requested, true
}

func TestDeriveVectors(t *testing.T) {
	vectorsSource, err := os.ReadFile(filepath.Join(fixtureRoot, "dsh-derivation-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	vectors, err := document.Parse(vectorsSource, document.JSON)
	if err != nil {
		t.Fatal(err)
	}
	wantOverrides, err := ParseOverrideMap(vectors.Get("overrides"))
	if err != nil {
		t.Fatal(err)
	}
	derivation, err := DeriveCordisOverrides(fixture(t, "derivation"), ReviewedMaps{})
	if err != nil {
		t.Fatal(err)
	}
	if !derivation.Overrides.Equal(wantOverrides) {
		t.Fatalf("overrides = %s\nwant %s", derivation.Overrides.Value().AppendJS(nil), wantOverrides.Value().AppendJS(nil))
	}
	if len(derivation.Requirements) == 0 || len(derivation.Delta) != 4 {
		t.Errorf("derivation = %+v", derivation)
	}
	if derivation.Delta[0].Before != nil {
		t.Errorf("a fresh derivation has no committed predecessor: %+v", derivation.Delta[0])
	}
	for _, requirement := range derivation.Requirements {
		switch requirement.Kind {
		case ExactPeerRequirement, RangePeerRequirement, DependencyRequirement:
		default:
			t.Errorf("requirement kind %v", requirement.Kind)
		}
	}
	// Two tentative selections of one family derive the value the surviving
	// declarations support; a dependency target is never read as an exact pin.
	twoSelections := vectors.Get("twoSelections")
	twoWant, err := ParseOverrideMap(twoSelections.Get("overrides"))
	if err != nil {
		t.Fatal(err)
	}
	two, err := DeriveCordisOverrides(fixture(t, "derivation-two-selections"), ReviewedMaps{})
	if err != nil {
		t.Fatalf("two selections: %v", err)
	}
	if !two.Overrides.Equal(twoWant) {
		t.Errorf("two-selection overrides = %s", two.Overrides.Value().AppendJS(nil))
	}
	derivedTimer, _ := two.Overrides.Get(SelectorTimer)
	wantTimer, _ := twoSelections.Get("derivedTimer").Text()
	if derivedTimer.String() != wantTimer.Display() {
		t.Errorf("derived timer = %s, want %s", derivedTimer, wantTimer.Display())
	}
}

func TestDeriveCommittedAndApproved(t *testing.T) {
	locked, err := ReadDshProject(fixture(t, "update-before"))
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := locked.Overrides()
	if err != nil {
		t.Fatal(err)
	}
	prefix := fixture(t, "derivation")
	withCommitted, err := DeriveCordisOverrides(prefix, ReviewedMaps{Committed: overrides})
	if err != nil || !withCommitted.Overrides.Equal(overrides) {
		t.Errorf("committed re-pin = %v, %v", withCommitted.Overrides, err)
	}
	// A reviewed value that is not a candidate cannot decide a unique family: the
	// derivation keeps the graph's own selection, and the adapter is what rejects a
	// reviewed map that differs from it.
	wrong := OverrideMap{entries: []OverrideEntry{{Selector: SelectorTimer, Version: ExactVersion{value: "9.9.9"}}}}
	decided, err := DeriveCordisOverrides(prefix, ReviewedMaps{Approved: wrong})
	if err != nil {
		t.Fatalf("a unique family ignores a non-candidate review: %v", err)
	}
	if timer, _ := decided.Overrides.Get(SelectorTimer); timer.String() != "1.1.4" {
		t.Errorf("timer = %s", timer)
	}
}

func TestReadOverrideMapFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "map.json")
	if err := os.WriteFile(path, []byte(`{"@deepseek-ai/dsh>@deepseek-ai/cordis":"4.0.2"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOverrideMap(path); err == nil || !strings.Contains(err.Error(), "missing key") {
		t.Errorf("incomplete map = %v", err)
	}
	complete := `{"@deepseek-ai/dsh>@deepseek-ai/cordis":"4.0.2","@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader":"1.0.3",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer":"1.1.4","@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include":"1.0.7"}`
	if err := os.WriteFile(path, []byte(complete), 0o644); err != nil {
		t.Fatal(err)
	}
	overrides, err := ReadOverrideMap(path)
	if err != nil || len(overrides.Entries()) != 4 {
		t.Errorf("complete map = %v, %v", overrides, err)
	}
	if err := os.WriteFile(path, []byte(`{"duplicate":"1.0.0","duplicate":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOverrideMap(path); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Errorf("duplicate keys = %v", err)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
