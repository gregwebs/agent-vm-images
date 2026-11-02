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

// The reviewed map pins a newer timer than the committed one; every other
// selector keeps the committed version.
const (
	committedMapJSON = `{"@deepseek-ai/dsh>@deepseek-ai/cordis":"4.0.2",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader":"1.0.3",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer":"1.1.4",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include":"1.0.7"}`
	reviewedMapJSON = `{"@deepseek-ai/dsh>@deepseek-ai/cordis":"4.0.2",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader":"1.0.3",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer":"1.1.5",` +
		`"@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include":"1.0.7"}`
	reviewedTimerArchive = "@deepseek-ai/cordis-plugin-timer@1.1.5"
)

func overrideMapOf(t *testing.T, source string) OverrideMap {
	t.Helper()
	value, err := document.Parse([]byte(source), document.JSON)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := ParseOverrideMap(value)
	if err != nil {
		t.Fatal(err)
	}
	return reviewed
}

// reviewedTimerPair is a staged update whose workspace was written by the
// reviewed rewrite, exactly as the developer path writes it.
func reviewedTimerPair(t *testing.T, reviewed OverrideMap) (Prefix, Prefix) {
	t.Helper()
	before := fixture(t, "update-before")
	after := fixture(t, "update-before")
	editFixtureJSON(t, after, "pnpm-lock.yaml", func(lock map[string]any) {
		for _, field := range []string{"packages", "snapshots"} {
			records := objectAt(t, lock, field)
			records[reviewedTimerArchive] = records[timerArchive]
			delete(records, timerArchive)
		}
		dshRootDependencies(t, lock)[timerPeer] = "1.1.5"
		objectAt(t, lock, "overrides")[timerSelector] = "1.1.5"
	})
	committed, err := os.ReadFile(filepath.Join(string(before), "pnpm-workspace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := WorkspaceWithOverrides(committed, reviewed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(string(after), "pnpm-workspace.yaml"), workspace, 0o644); err != nil {
		t.Fatal(err)
	}
	repin(t, after)
	return before, after
}

// stagedComparison applies a case's post-validation staged edit and compares the
// two parsed projects the comparator is given. Both sides are read by the
// production reader first, so a case can only reach a freeze the reader does not
// already close; the edit represents what the comparator would be handed if that
// freeze were not live.
func stagedComparison(t *testing.T, request UpdateRequest, staged func(*testing.T, *DshProject, *DshProject)) (UpdateComparison, error) {
	t.Helper()
	before, err := ReadDshProject(request.Before)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ReadDshProject(request.After)
	if err != nil {
		t.Fatal(err)
	}
	staged(t, before, after)
	comparison, _, err := compareDshProjects(request, before, after)
	return comparison, err
}

// TestCompareDshReviewedOverrides proves the reviewed-map seam keeps every rule
// 1 freeze except the override key the review decided.
func TestCompareDshReviewedOverrides(t *testing.T) {
	reviewed := overrideMapOf(t, reviewedMapJSON)
	for _, test := range []struct {
		name      string
		authority func() OverrideAuthority
		refresh   RefreshMode
		upstream  func(t *testing.T, after Prefix)
		// staged changes the already-validated staged project. It exists for the
		// freezes ReadDshProject closes first: a valid staged project cannot carry
		// a header edit at all, so the comparator's own header freeze is only
		// reachable on the two sides the comparator is actually given.
		staged func(t *testing.T, before, after *DshProject)
		match  string
		check  func(t *testing.T, comparison UpdateComparison)
	}{
		{
			name:    "accepted",
			refresh: RefreshLock,
			check: func(t *testing.T, comparison UpdateComparison) {
				t.Helper()
				if comparison.Mode != ModeRefresh {
					t.Errorf("mode = %s", comparison.Mode)
				}
				if !containsString(comparison.AddedArchives, reviewedTimerArchive) ||
					!containsString(comparison.RemovedArchives, timerArchive) {
					t.Errorf("added = %v, removed = %v", comparison.AddedArchives, comparison.RemovedArchives)
				}
			},
		},
		{
			name:      "committed-authority",
			refresh:   RefreshLock,
			authority: func() OverrideAuthority { return OverrideAuthority{} },
			match:     "workspace bytes changed",
		},
		{
			name:      "map-mismatch",
			refresh:   RefreshLock,
			authority: func() OverrideAuthority { return ReviewedOverrides(overrideMapOf(t, committedMapJSON)) },
			match:     "staged overrides differ from the reviewed map",
		},
		{
			name:    "manifest-outside-root",
			refresh: RefreshLock,
			// The root dependency is the only manifest field a reviewed update may
			// touch; `description` is ordinary metadata the reader accepts, so only
			// the manifest freeze can refuse it on the reviewed-map path.
			upstream: func(t *testing.T, after Prefix) {
				editFixtureJSON(t, after, "package.json", func(manifest map[string]any) {
					manifest["description"] = "edited outside the root selection"
				})
				repin(t, after)
			},
			match: "only dsh manifest selection may change",
		},
		{
			name:    "header-outside-overrides",
			refresh: RefreshLock,
			// The reviewed substitution may replace lock.overrides and nothing else in
			// the header. Settings, importers, lockfileVersion and the override map
			// itself are all closed by ReadDshProject, so the staged header is changed
			// after validation; a substitution that copied the whole staged header
			// would accept this graph.
			staged: func(t *testing.T, before, after *DshProject) {
				t.Helper()
				if !document.DeepStrictEqual(before.project.lock.Get("settings"), after.project.lock.Get("settings")) {
					t.Fatal("the control must differ from the committed header only by what it injects")
				}
				settings := after.project.lock.Get("settings").With("excludeLinksFromLockfile", boolValue(true))
				after.project.lock = after.project.lock.With("settings", settings)
			},
			match: "lock settings/overrides/importer changed",
		},
		{
			name:    "workspace-outside-map",
			refresh: RefreshLock,
			// The staged workspace is the reviewed rewrite's YAML encoding. This case
			// re-emits exactly those values as JSON: the workspace freeze is a byte
			// rule, so semantically equal output in another encoding is what a
			// hand-edited workspace looks like to it.
			upstream: func(t *testing.T, after Prefix) {
				path := filepath.Join(string(after), "pnpm-workspace.yaml")
				source, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				workspace, err := document.Parse(source, document.YAML)
				if err != nil {
					t.Fatal(err)
				}
				body := append(workspace.AppendJSIndent(nil, "  "), '\n')
				if err := os.WriteFile(path, body, 0o644); err != nil {
					t.Fatal(err)
				}
				repin(t, after)
			},
			match: "workspace bytes changed outside the reviewed overrides",
		},
		{
			name:    "retained-metadata",
			refresh: RefreshLock,
			upstream: func(t *testing.T, after Prefix) {
				editFixtureJSON(t, after, "pnpm-lock.yaml", func(lock map[string]any) {
					objectAt(t, objectAt(t, lock, "packages"), "shared@1.0.0")["deprecated"] = "upstream notice"
				})
				repin(t, after)
			},
			match: "retained archive changed: shared@1.0.0",
		},
		{
			// A plain replacement: the old identity leaves inside the closure and the
			// new one enters it. Nothing adopts moved metadata — a retained record
			// whose metadata moved is the case above, and it is refused.
			name:    "drops-retained-identity",
			refresh: RefreshLock,
			upstream: func(t *testing.T, after Prefix) {
				editFixtureJSON(t, after, "pnpm-lock.yaml", func(lock map[string]any) {
					for _, field := range []string{"packages", "snapshots"} {
						records := objectAt(t, lock, field)
						records["leaf@1.0.1"] = records["leaf@1.0.0"]
						delete(records, "leaf@1.0.0")
					}
					dshRootDependencies(t, lock)["leaf"] = "1.0.1"
				})
				repin(t, after)
			},
			check: func(t *testing.T, comparison UpdateComparison) {
				t.Helper()
				if !containsString(comparison.RemovedArchives, "leaf@1.0.0") {
					t.Errorf("removed = %v", comparison.RemovedArchives)
				}
			},
		},
		{
			name:    "orphan",
			refresh: RefreshLock,
			upstream: func(t *testing.T, after Prefix) {
				editFixtureJSON(t, after, "pnpm-lock.yaml", func(lock map[string]any) {
					objectAt(t, lock, "packages")["orphan@1.0.0"] = map[string]any{
						"resolution": map[string]any{"integrity": syntheticIntegrity},
					}
				})
				repin(t, after)
			},
			// `orphan@1.0.0` is this fixture's own key, so the assertion names the
			// comparator's closure rule instead of matching a substring of it.
			match: "orphan archive",
		},
		{
			name:      "no-refresh-equal",
			authority: func() OverrideAuthority { return ReviewedOverrides(reviewed) },
			match:     "identical bytes",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			before, after := reviewedTimerPair(t, reviewed)
			if test.upstream != nil {
				test.upstream(t, after)
			}
			authority := ReviewedOverrides(reviewed)
			if test.authority != nil {
				authority = test.authority()
			}
			request := UpdateRequest{
				Before:    before,
				After:     after,
				Refresh:   test.refresh,
				Overrides: authority,
			}
			var comparison UpdateComparison
			var err error
			if test.staged == nil {
				comparison, _, err = CompareDshUpdate(request)
			} else {
				comparison, err = stagedComparison(t, request, test.staged)
			}
			if test.match != "" {
				if err == nil || !strings.Contains(err.Error(), test.match) {
					t.Fatalf("error = %v, want %q", err, test.match)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.check != nil {
				test.check(t, comparison)
			}
		})
	}
}

// The authority cannot reach the generic entry: an image build or a Pi/bridge
// comparison never carries a reviewed dsh map.
func TestCompareSelectionRefusesReviewedOverrides(t *testing.T) {
	before := fixture(t, "update-before")
	after := fixture(t, "update-before")
	_, _, err := CompareSelectionUpdate(UpdateRequest{
		Kind:      KindDsh,
		Before:    before,
		After:     after,
		Overrides: ReviewedOverrides(overrideMapOf(t, reviewedMapJSON)),
	})
	if err == nil || !strings.Contains(err.Error(),
		"reviewed overrides are only available through the dsh comparison") {
		t.Errorf("generic entry = %v", err)
	}
}
