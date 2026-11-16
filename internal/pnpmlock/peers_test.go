package pnpmlock

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	dshRootArchive        = "@deepseek-ai/dsh@0.1.5-rc.2"
	cordisLoaderArchive   = "@deepseek-ai/cordis-plugin-loader@1.0.3"
	timerArchive          = "@deepseek-ai/cordis-plugin-timer@1.1.4"
	timerNextArchive      = "@deepseek-ai/cordis-plugin-timer@1.1.7"
	timerPeer             = "@deepseek-ai/cordis-plugin-timer"
	includeArchive        = "@deepseek-ai/cordis-plugin-include@1.0.7"
	timerSelector         = "@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer"
	syntheticIntegrity    = "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
	coverExactCordisPeers = "dsh must cover declared exact Cordis peers"
)

// editFixtureJSON rewrites one committed fixture file through a decoded object so
// a test can express "this graph changed shape" without a hand-edited string.
func editFixtureJSON(t *testing.T, prefix Prefix, file string, edit func(root map[string]any)) {
	t.Helper()
	path := filepath.Join(string(prefix), file)
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(source, &root); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	edit(root)
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func objectAt(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object", key)
	}
	return value
}

// dropOverrides rewrites both the workspace and the lock to the override-free
// graph a re-resolution without the reviewed map produces.
func dropOverrides(t *testing.T, prefix Prefix) {
	t.Helper()
	editFixtureJSON(t, prefix, "pnpm-workspace.yaml", func(workspace map[string]any) {
		delete(workspace, "overrides")
	})
	editFixtureJSON(t, prefix, "pnpm-lock.yaml", func(lock map[string]any) {
		delete(lock, "overrides")
	})
	repin(t, prefix)
}

// repinOverride rewrites the reviewed map in both inputs, which must stay equal.
func repinOverride(t *testing.T, prefix Prefix, selector, version string) {
	t.Helper()
	for _, file := range []string{"pnpm-workspace.yaml", "pnpm-lock.yaml"} {
		editFixtureJSON(t, prefix, file, func(root map[string]any) {
			objectAt(t, root, "overrides")[selector] = version
		})
	}
	repin(t, prefix)
}

// dshRootDependencies is the dsh root snapshot's dependency map, the edges that
// make a package reachable.
func dshRootDependencies(t *testing.T, lock map[string]any) map[string]any {
	t.Helper()
	root := objectAt(t, objectAt(t, lock, "snapshots"), dshRootArchive)
	return objectAt(t, root, "dependencies")
}

// addFamilyInstance adds one more resolved version of a family, reached from the
// `via` snapshot: an archive, its snapshot, and the edge that keeps it reachable.
func addFamilyInstance(t *testing.T, prefix Prefix, name, version, via string) {
	t.Helper()
	editFixtureJSON(t, prefix, "pnpm-lock.yaml", func(lock map[string]any) {
		archive := name + "@" + version
		objectAt(t, lock, "packages")[archive] = map[string]any{
			"resolution": map[string]any{"integrity": syntheticIntegrity},
		}
		snapshots := objectAt(t, lock, "snapshots")
		snapshots[archive] = map[string]any{}
		from := objectAt(t, snapshots, via)
		if _, present := from["dependencies"]; !present {
			from["dependencies"] = map[string]any{}
		}
		objectAt(t, from, "dependencies")[name] = version
	})
	repin(t, prefix)
}

// dropFamily removes every trace of one resolved version while the reviewed map
// keeps pinning the family.
func dropFamily(t *testing.T, prefix Prefix, name, version string) {
	t.Helper()
	editFixtureJSON(t, prefix, "pnpm-lock.yaml", func(lock map[string]any) {
		archive := name + "@" + version
		delete(objectAt(t, lock, "packages"), archive)
		delete(objectAt(t, lock, "snapshots"), archive)
		delete(dshRootDependencies(t, lock), name)
	})
	repin(t, prefix)
}

// rangePeerRoot is the shape upstream dsh 0.2.x publishes: the Cordis family's
// declarations are ranges, so no declaration is an exact pin.
func rangePeerRoot(t *testing.T) Prefix {
	t.Helper()
	prefix := fixture(t, "tiny-dsh")
	editFixtureJSON(t, prefix, "pnpm-lock.yaml", func(lock map[string]any) {
		packages := objectAt(t, lock, "packages")
		objectAt(t, packages, cordisLoaderArchive)["peerDependencies"] = map[string]any{
			"@deepseek-ai/cordis": "~4.0.2",
		}
	})
	repin(t, prefix)
	return prefix
}

func exactPeerReportFor(t *testing.T, prefix Prefix) PeerReport {
	t.Helper()
	report, err := ReportDshPeers(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// TestExactSubPackagePeersWithoutOverrides pins the dsh 0.1.x behaviour this rule
// must not disturb. A locked dsh project always carries the complete reviewed map
// -- the workspace policy refuses an absent one -- so the override-free shape is
// the tentative graph a re-resolution produces, read through the report reader.
func TestExactSubPackagePeersWithoutOverrides(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	dropOverrides(t, prefix)
	report := exactPeerReportFor(t, prefix)
	if len(report.Unsatisfied) != 0 {
		t.Fatalf("unsatisfied = %v", report.Unsatisfied)
	}
	if report.Exact != 1 || report.CordisExact != 1 {
		t.Fatalf("exact = %d, cordisExact = %d, want the one declared pin", report.Exact, report.CordisExact)
	}
	locked, err := ReadDshProject(fixture(t, "tiny-dsh"))
	if err != nil {
		t.Fatalf("exact sub-package peers are a locked project: %v", err)
	}
	exact, err := locked.ExactPeers()
	if err != nil || exact != report.Exact {
		t.Fatalf("exact peers = %d, %v", exact, err)
	}
}

// TestReviewedOverridesAreVerifiedAgainstTheGraph is the dsh 0.2.x adoption: no
// declaration is a pin, the reviewed map pins all four families, and the graph
// resolves each of them to exactly that version.
func TestReviewedOverridesAreVerifiedAgainstTheGraph(t *testing.T) {
	prefix := rangePeerRoot(t)
	report := exactPeerReportFor(t, prefix)
	if len(report.Unsatisfied) != 0 {
		t.Fatalf("unsatisfied = %v", report.Unsatisfied)
	}
	if report.Exact != 0 {
		t.Fatalf("exact = %d, want no exact declaration", report.Exact)
	}
	if report.CordisExact != 4 {
		t.Fatalf("cordisExact = %d, want the four verified families", report.CordisExact)
	}
	if _, err := ReadDshProject(prefix); err != nil {
		t.Fatalf("a graph honouring its reviewed map is a locked project: %v", err)
	}
}

// TestOverrideResolvedElsewhereIsUnsatisfied proves the rule verifies rather than
// counts: a reviewed pin the graph does not honour is refused.
func TestOverrideResolvedElsewhereIsUnsatisfied(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	repinOverride(t, prefix, timerSelector, "1.1.7")
	report := exactPeerReportFor(t, prefix)
	want := timerPeer + ": override 1.1.7, got " + timerArchive
	if len(report.Unsatisfied) != 1 || report.Unsatisfied[0] != want {
		t.Fatalf("unsatisfied = %v, want [%s]", report.Unsatisfied, want)
	}
	// Three families are honoured; the loader's exact Cordis declaration is the
	// fourth counted row.
	if report.CordisExact != 4 {
		t.Errorf("cordisExact = %d, want the three honoured families plus the declared pin", report.CordisExact)
	}
	if _, err := ReadProject(prefix, KindDsh); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("override resolved elsewhere = %v", err)
	}
}

// TestOverrideResolvedTwiceIsUnsatisfied refuses a graph that carries two
// versions of a family the reviewed map pins, because one pin cannot control both.
func TestOverrideResolvedTwiceIsUnsatisfied(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	// Two versions of one family is what an unpinned closure looks like: the dsh
	// root resolves 1.1.4 and a plugin pulls its own 1.1.7.
	addFamilyInstance(t, prefix, timerPeer, "1.1.7", includeArchive)
	report := exactPeerReportFor(t, prefix)
	want := timerPeer + ": override 1.1.4, got " + timerArchive + " and " + timerNextArchive
	if len(report.Unsatisfied) != 1 || report.Unsatisfied[0] != want {
		t.Fatalf("unsatisfied = %v, want [%s]", report.Unsatisfied, want)
	}
	if _, err := ReadProject(prefix, KindDsh); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("two resolved versions = %v", err)
	}
}

// TestOverrideFamilyAbsentIsUnsatisfied refuses a reviewed pin for a family the
// graph does not resolve at all: a pin nothing honours is not evidence.
func TestOverrideFamilyAbsentIsUnsatisfied(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	dropFamily(t, prefix, timerPeer, "1.1.4")
	report := exactPeerReportFor(t, prefix)
	want := timerPeer + ": override 1.1.4, got missing"
	if len(report.Unsatisfied) != 1 || report.Unsatisfied[0] != want {
		t.Fatalf("unsatisfied = %v, want [%s]", report.Unsatisfied, want)
	}
	if _, err := ReadProject(prefix, KindDsh); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("absent family = %v", err)
	}
}

// TestNoOverridesAndNoExactPeersIsRefused keeps the old refusal for the shape
// that has neither instrument: range declarations and no reviewed map. Only a
// non-locked graph can reach it, so the guard is called on the report reader's
// project -- the same function every publication path calls.
func TestNoOverridesAndNoExactPeersIsRefused(t *testing.T) {
	prefix := rangePeerRoot(t)
	dropOverrides(t, prefix)
	report := exactPeerReportFor(t, prefix)
	if report.CordisExact != 0 || len(report.Unsatisfied) != 0 {
		t.Fatalf("override-free range graph = %+v", report)
	}
	project, err := readProjectInputs(prefix, KindDsh, skipPins, readReport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkExactPeers(project); err == nil || !strings.Contains(err.Error(), coverExactCordisPeers) {
		t.Errorf("override-free range graph = %v", err)
	}
}
