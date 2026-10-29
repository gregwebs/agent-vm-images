package pnpmlock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/candidatestage"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// mintedCandidate copies a committed fixture's three inputs into the exact
// private stage layout the preparation driver mints.
// piFixture rewrites a dsh fixture copy into the exact pi consumer schema, so the
// one mode that relaxes the retained freeze is exercised on the kind that can name
// it. The Pi workspace literal is the reviewed one; the lock's dsh name is replaced
// throughout and the override block, which only dsh has, is removed.
func piFixture(t *testing.T, name string) Prefix {
	t.Helper()
	prefix := fixture(t, name)
	for _, file := range []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"} {
		path := filepath.Join(string(prefix), file)
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rewritten := strings.ReplaceAll(string(payload), "@deepseek-ai/dsh",
			"@earendil-works/pi-coding-agent")
		if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lockPath := filepath.Join(string(prefix), "pnpm-lock.yaml")
	var lock map[string]any
	payload, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, &lock); err != nil {
		t.Fatal(err)
	}
	delete(lock, "overrides")
	lock["settings"].(map[string]any)["autoInstallPeers"] = true
	encoded, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	workspace := `{"packages":["."],"autoInstallPeers":true,"strictPeerDependencies":false,` +
		`"enableGlobalVirtualStore":false,"pmOnFail":"error","sideEffectsCache":false,"allowBuilds":{},` +
		`"extendNodePath":false,"nodeLinker":"isolated","hoist":false,"preferSymlinkedExecutables":true,` +
		`"trustPolicyExclude":["undici-types@6.21.0"]}`
	if err := os.WriteFile(filepath.Join(string(prefix), "pnpm-workspace.yaml"), []byte(workspace), 0o644); err != nil {
		t.Fatal(err)
	}
	writePinsFor(t, string(prefix))
	return prefix
}

// piCandidate mints the driver's candidate layout around the pi schema of a dsh
// fixture: the kind's own workspace and manifest root with the same graph.
func piCandidate(t *testing.T, fixtureName string) candidatestage.Stage {
	t.Helper()
	prefix := piFixture(t, fixtureName)
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
	for _, file := range PinnedInputs {
		payload, err := os.ReadFile(filepath.Join(string(prefix), file))
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

// writePinsFor rewrites the three-input digest file a prefix is read through.
func writePinsFor(t *testing.T, prefix string) {
	t.Helper()
	body := &strings.Builder{}
	for _, file := range PinnedInputs {
		payload, err := os.ReadFile(filepath.Join(prefix, file))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(payload)
		body.WriteString(hex.EncodeToString(sum[:]) + "  " + file + "\n")
	}
	if err := os.WriteFile(filepath.Join(prefix, "pnpm-inputs.sha256"), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

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

// TestShrinkwrapAuthorityIsPiOnly pins the typed boundary of the one mode that
// relaxes the retained-snapshot freeze. Only pi publishes an authenticated
// dependency shrinkwrap, so on another kind the mode would trade the freeze (a
// retained edge moving from mandatory to optional, for instance) for nothing. The
// CLI refuses the flag for those kinds too; this is the second boundary.
func TestShrinkwrapAuthorityIsPiOnly(t *testing.T) {
	for _, vector := range []struct {
		kind  Kind
		name  string
		relax func(string) string
	}{
		{kind: KindDsh, name: "tiny-dsh"},
		// The reproduced case: a changed bridge selection whose retained
		// debug -> ms edge moved from mandatory to optional. The freeze refuses it,
		// and naming the shrinkwrap authority must not be a way around that.
		{kind: KindBridge, name: "real-bridge", relax: func(payload string) string {
			return strings.Replace(payload, "  debug@4.4.3:\n    dependencies:\n      ms: 2.1.3\n",
				"  debug@4.4.3:\n    optionalDependencies:\n      ms: 2.1.3\n", 1)
		}},
	} {
		t.Run(vector.name, func(t *testing.T) {
			before, err := ReadProject(fixture(t, vector.name), vector.kind)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := ValidateCandidate(mintedCandidate(t, vector.name), vector.kind)
			if err != nil {
				t.Fatal(err)
			}
			equal, err := ParseRequestedSelection(before.Selection().Version.String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CompareSelectionCandidate(before, candidate,
				SelectionRequest{Requested: equal, Authority: RetainedSnapshotFreeze}); err != nil {
				t.Fatalf("retained freeze = %v", err)
			}
			if _, err := CompareSelectionCandidate(before, candidate,
				SelectionRequest{Requested: equal, Authority: ShrinkwrapAuthority}); err == nil ||
				!strings.Contains(err.Error(), "shrinkwrap authority is only available for the pi kind") {
				t.Fatalf("shrinkwrap authority for %s = %v, want the typed refusal", vector.name, err)
			}
			if vector.relax == nil {
				return
			}
			// A changed selection reaches the retained-snapshot freeze; an equal one
			// is refused by the byte rule before the freeze is consulted.
			relaxed := editCandidate(t, mintedCandidate(t, vector.name), "pnpm-lock.yaml",
				func(payload string) string {
					payload = vector.relax(payload)
					// The root moves to 0.1.1 as well, so this is a changed selection
					// (which reaches the retained-snapshot freeze) and not an equal one.
					payload = strings.Replace(payload,
						"        specifier: 0.1.0\n        version: 0.1.0(zod@4.6.7)",
						"        specifier: 0.1.1\n        version: 0.1.1(zod@4.6.7)", 1)
					return strings.ReplaceAll(payload, "pi-claude-bridge@0.1.0",
						"pi-claude-bridge@0.1.1")
				})
			relaxed = editCandidate(t, relaxed, "package.json", func(payload string) string {
				return strings.Replace(payload, `"pi-claude-bridge": "0.1.0"`,
					`"pi-claude-bridge": "0.1.1"`, 1)
			})
			relaxedCandidate, err := ValidateCandidate(relaxed, vector.kind)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := ParseRequestedSelection("0.1.1")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := CompareSelectionCandidate(before, relaxedCandidate,
				SelectionRequest{Requested: changed, Authority: RetainedSnapshotFreeze}); err == nil {
				t.Fatalf("a retained edge became optional under the retained freeze")
			}
			if _, err := CompareSelectionCandidate(before, relaxedCandidate,
				SelectionRequest{Requested: changed, Authority: ShrinkwrapAuthority}); err == nil ||
				!strings.Contains(err.Error(), "shrinkwrap authority is only available for the pi kind") {
				t.Fatalf("shrinkwrap authority for %s = %v, want the typed refusal", vector.name, err)
			}
		})
	}
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
	// A developer refresh may change lock bytes for the same selection; the same
	// bytes without the refresh request are not an equal selection at all. This is
	// the request the preparation driver must forward: without it an ordinary
	// comment-only lock rewrite fails as a byte difference.
	rewrittenStage := editCandidate(t, mintedCandidate(t, "update-before"), "pnpm-lock.yaml",
		func(payload string) string { return payload + "# refreshed\n" })
	rewritten, err := ValidateCandidate(rewrittenStage, KindDsh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompareSelectionCandidate(before, rewritten, SelectionRequest{Requested: requested}); err == nil ||
		!strings.Contains(err.Error(), "pnpm-lock.yaml: equal selection requires identical bytes") {
		t.Fatalf("equal bytes without refresh = %v, want the identical-bytes refusal", err)
	}
	rewrittenRefresh, err := CompareSelectionCandidate(before, rewritten,
		SelectionRequest{Requested: requested, Refresh: RefreshLock})
	if err != nil || rewrittenRefresh.Mode != ModeRefresh {
		t.Fatalf("refresh of rewritten lock = %+v, %v", rewrittenRefresh, err)
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

// TestCompareSelectionCandidateShrinkwrapAuthority pins the one authority switch
// a candidate may name. A changed Pi selection whose authenticated archive
// carries its own npm-shrinkwrap.json validates the imported graph against that
// shrinkwrap separately, so the previous selection's frozen snapshot edges must
// not be substituted for it. Everything else stays hard: the same archive SRI
// for the same name@version is immutable, and the ordinary post-pin comparison
// (`UpdateRequest`) has no way to name the authority at all.
func TestCompareSelectionCandidateShrinkwrapAuthority(t *testing.T) {
	// The authority is the pi kind's only: it names the authenticated selected
	// archive's own shrinkwrap as the graph authority instead of the reviewed
	// predecessor. The fixtures are carried into the pi schema so this exercises the
	// kind that can actually reach the mode.
	before, err := ReadProject(piFixture(t, "update-before"), KindPi)
	if err != nil {
		t.Fatal(err)
	}
	requested, err := ParseRequestedSelection("0.1.5-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	// A retained snapshot whose edge record the previous selection froze.
	reshaped := editCandidate(t, piCandidate(t, "update-after"), "pnpm-lock.yaml",
		func(source string) string {
			// The pi schema re-encodes the fixture, so the record is compact JSON.
			return strings.Replace(source, `"shared@1.0.0":{}`,
				`"shared@1.0.0":{"dependencies":{"leaf":"2.0.0"}}`, 1)
		})
	candidate, err := ValidateCandidate(reshaped, KindPi)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompareSelectionCandidate(before, candidate,
		SelectionRequest{Requested: requested}); err == nil ||
		!strings.Contains(err.Error(), "retained snapshot changed: shared@1.0.0") {
		t.Fatalf("retained freeze = %v, want the frozen snapshot rejection", err)
	}
	accepted, err := CompareSelectionCandidate(before, candidate,
		SelectionRequest{Requested: requested, Authority: ShrinkwrapAuthority})
	if err != nil {
		t.Fatalf("shrinkwrap authority = %v", err)
	}
	if accepted.Mode != ModeChanged {
		t.Errorf("mode = %v", accepted.Mode)
	}
	// The switch covers snapshot records only: a changed SRI for a retained
	// archive is still a different artifact, not a refreshed edge.
	swapped := editCandidate(t, piCandidate(t, "update-after"), "pnpm-lock.yaml",
		func(source string) string {
			return strings.Replace(source, `"sha512-AAA`, `"sha512-BAA`, 1)
		})
	substituted, err := ValidateCandidate(swapped, KindPi)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CompareSelectionCandidate(before, substituted,
		SelectionRequest{Requested: requested, Authority: ShrinkwrapAuthority}); err == nil ||
		!strings.Contains(err.Error(), "retained archive changed:") {
		t.Fatalf("shrinkwrap authority archive freeze = %v, want the retained archive rejection", err)
	}
}

// TestShrinkwrapAuthorityKeepsTheOtherFreezes pins the *boundary* of the authority
// switch: naming the authenticated selected shrinkwrap relaxes retained snapshot
// records only.
//
// The candidate-stage validator already refuses most of these shapes, so the
// vectors go through the pure comparison view instead: that is the second
// boundary, and the one that would silently widen if somebody let the mode relax
// more than retained snapshots. The manifest, the workspace bytes, the lock header
// (settings, overrides, importer) and the changed-root closure for added and
// dropped identities all still refuse, and the retained snapshot relaxation itself
// is exercised last so the vectors are a boundary and not a disabled mode.
func TestShrinkwrapAuthorityKeepsTheOtherFreezes(t *testing.T) {
	project, err := ReadProject(piFixture(t, "update-before"), KindPi)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := ValidateCandidate(piCandidate(t, "update-after"), KindPi)
	if err != nil {
		t.Fatal(err)
	}
	requested, err := ParseRequestedSelection("0.1.5-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	integrity := "sha512-" + strings.Repeat("A", 86) + "=="
	strayArchive := mappingValue(
		entry("resolution", mappingValue(entry("integrity", jsStringValue(integrity)))))
	withStray := func(lock document.Value) document.Value {
		packages, err := recordOf(lock.Get("packages"))
		if err != nil {
			t.Fatal(err)
		}
		snapshots, err := recordOf(lock.Get("snapshots"))
		if err != nil {
			t.Fatal(err)
		}
		return lock.With("packages", packages.With("stray@9.9.9", strayArchive)).
			With("snapshots", snapshots.With("stray@9.9.9", mappingValue()))
	}
	settings := func(side *comparisonSide) {
		header, err := recordOf(side.lock.Get("settings"))
		if err != nil {
			t.Fatal(err)
		}
		side.lock = side.lock.With("settings",
			header.With("excludeLinksFromLockfile", boolValue(true)))
	}
	for _, vector := range []struct {
		name  string
		edit  func(*comparisonSide)
		error string
	}{
		{
			// A second, non-root dependency change is not the selection change the
			// mode is for.
			name: "manifest", error: "only pi manifest selection may change",
			edit: func(side *comparisonSide) {
				dependencies, err := recordOf(side.manifest.Get("dependencies"))
				if err != nil {
					t.Fatal(err)
				}
				side.manifest = side.manifest.With("dependencies",
					dependencies.With("left-pad", jsStringValue("1.0.0")))
			},
		},
		{
			name: "workspace bytes", error: "workspace bytes changed",
			edit: func(side *comparisonSide) {
				side.workspace = append(append([]byte{}, side.workspace...), '\n')
			},
		},
		{
			name: "manager settings", error: "lock settings/overrides/importer changed",
			edit: settings,
		},
		{
			// An identity the changed root cannot reach is not part of this
			// selection, so the authority cannot legalize adding it.
			name:  "added identity outside the closure",
			error: "addition outside changed root closure: stray@9.9.9",
			edit:  func(side *comparisonSide) { side.lock = withStray(side.lock) },
		},
	} {
		t.Run(vector.name, func(t *testing.T) {
			side := candidateSide(candidate)
			vector.edit(&side)
			if _, err := compareSelection(KindPi, projectSide(project), side, requested,
				NoRefresh, ShrinkwrapAuthority); err == nil ||
				!strings.Contains(err.Error(), vector.error) {
				t.Fatalf("shrinkwrap authority = %v, want %q", err, vector.error)
			}
		})
	}

	// A record the predecessor carried but its graph never reached is a removal
	// outside the old closure: the authority relaxes retained *records*, not the
	// scope of the graph.
	t.Run("dropped identity outside the closure", func(t *testing.T) {
		before := projectSide(project)
		before.lock = withStray(before.lock)
		if _, err := compareSelection(KindPi, before, candidateSide(candidate), requested,
			NoRefresh, ShrinkwrapAuthority); err == nil ||
			!strings.Contains(err.Error(), "removal outside changed root closure: stray@9.9.9") {
			t.Fatalf("shrinkwrap authority = %v, want the closure removal rejection", err)
		}
	})

	// The relaxation itself still holds for the same pair, so the vectors above are
	// the boundary and not a mode that stopped working.
	t.Run("retained snapshot relaxed", func(t *testing.T) {
		side := candidateSide(candidate)
		snapshots, err := recordOf(side.lock.Get("snapshots"))
		if err != nil {
			t.Fatal(err)
		}
		side.lock = side.lock.With("snapshots", snapshots.With("shared@1.0.0", mappingValue(
			entry("dependencies", mappingValue(entry("leaf", jsStringValue("2.0.0")))))))
		if _, err := compareSelection(KindPi, projectSide(project), side, requested,
			NoRefresh, RetainedSnapshotFreeze); err == nil {
			t.Fatal("the retained freeze must refuse the reshaped record")
		}
		comparison, err := compareSelection(KindPi, projectSide(project), side, requested,
			NoRefresh, ShrinkwrapAuthority)
		if err != nil {
			t.Fatalf("the retained snapshot relaxation must still hold: %v", err)
		}
		if comparison.Mode != ModeChanged {
			t.Errorf("mode = %v", comparison.Mode)
		}
	})
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
