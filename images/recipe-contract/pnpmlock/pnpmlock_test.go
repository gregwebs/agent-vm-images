package pnpmlock

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

const fixtureRoot = "../../../script/test/fixtures/pnpm-lock"

// fixture copies one committed fixture so a test can mutate it.
func fixture(t *testing.T, name string) Prefix {
	t.Helper()
	destination := t.TempDir()
	source := filepath.Join(fixtureRoot, name)
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	for _, entry := range entries {
		copyFixtureFile(t, filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()))
	}
	return Prefix(destination)
}

func copyFixtureFile(t *testing.T, from, to string) {
	t.Helper()
	source, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, source, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPeerSpecGrammar(t *testing.T) {
	accepted := []struct {
		specification string
		class         specClass
	}{
		{"=4.0.2", specExact}, {"= 4.0.2", specExact}, {"=  4.0.2", specExact},
		{"v4.0.2", specExact}, {" v4.0.2 ", specExact}, {" 4.0.2 ", specExact},
		{"^4.0.2", specRange}, {"~4.0.2", specRange}, {">=4.0.0 <5.0.0", specRange},
		{"4.x", specRange}, {"4", specRange}, {"*", specRange}, {"x", specRange},
		{"^4.0.2 || ^5.0.0", specRange}, {"4.0.0 - 4.1.0", specRange},
	}
	for _, vector := range accepted {
		class, err := peerSpecClass(vector.specification)
		if err != nil {
			t.Errorf("%q: %v", vector.specification, err)
			continue
		}
		if class != vector.class {
			t.Errorf("%q classified %v, want %v", vector.specification, class, vector.class)
		}
	}
	for _, specification := range []string{"nonsense", "4.0.2 ||", ">=", "v4", "=", "~>4.0.2", "4.0.2, 4.0.3"} {
		if _, err := peerSpecClass(specification); err == nil || !strings.Contains(err.Error(), "unknown peer specification") {
			t.Errorf("%q = %v", specification, err)
		}
	}
	// The spaced comparator must remain an exact declaration, not fall through to
	// the range grammar and escape the exact-peer oracle.
	for _, specification := range []string{"=4.0.3", "= 4.0.3", "=  4.0.3", "v4.0.3", " = 4.0.3 "} {
		pinned, ok := exactDeclaration(specification)
		if !ok || pinned != "4.0.3" {
			t.Errorf("%q pinned %q, %v", specification, pinned, ok)
		}
	}
}

func TestUnicodeWhitespaceInSpecifications(t *testing.T) {
	for _, space := range []string{"\u00a0", "\ufeff", "\u2028", "\u2003"} {
		if _, ok := exactDeclaration("=" + space + "4.0.3"); !ok {
			t.Errorf("U+%04X is not treated as JavaScript whitespace", space)
		}
		if !rangeDeclaration("^4.0.2" + space + "||" + space + "^5.0.0") {
			t.Errorf("U+%04X is not treated as JavaScript whitespace in a range", space)
		}
	}
	if splitJSSpace("")[0] != "" || len(splitJSSpace("")) != 1 {
		t.Error(`"".split(/\s+/) must be [""]`)
	}
}

func TestFamilyRange(t *testing.T) {
	for _, vector := range []struct {
		selected string
		wanted   string
		want     bool
	}{
		{"4.0.2", "4.0.2", true},
		{"4.0.2", "=4.0.2", true},
		{"4.0.2", "^4.0.0", true},
		{"4.0.2", "~4.0.0", true},
		{"4.0.2", ">=4.0.0 <5.0.0", true},
		{"5.0.0", "^4.0.0", false},
		{"0.2.3", "^0.2.0", true},
		{"0.3.0", "^0.2.0", false},
		{"4.0.2", "^4.0.0 || ^5.0.0", true},
	} {
		got, err := satisfiesFamilyRange(ExactVersion{value: vector.selected}, vector.wanted)
		if err != nil || got != vector.want {
			t.Errorf("%s in %s = %v, %v", vector.selected, vector.wanted, got, err)
		}
	}
	if _, err := satisfiesFamilyRange(ExactVersion{value: "4.0.2"}, "4.x"); err == nil {
		t.Error("a wildcard range is unprovable and must stop")
	}
	if _, err := satisfiesFamilyRange(ExactVersion{value: "4.0.2-rc.1"}, "^4.0.0"); err == nil {
		t.Error("a prerelease selection cannot prove a family range")
	}
	if _, err := satisfiesFamilyRange(ExactVersion{value: "4.0.2"}, "9007199254740993"); err == nil {
		t.Error("an unsafe integer must stop the range check")
	}
}

func TestSnapshotID(t *testing.T) {
	id, err := parseSnapshotID("@deepseek-ai/dsh@0.1.5-rc.2(@deepseek-ai/cordis@4.0.2)")
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Context) != 1 || id.Key != "@deepseek-ai/dsh@0.1.5-rc.2(@deepseek-ai/cordis@4.0.2)" {
		t.Fatalf("id = %+v", id)
	}
	if _, ok := id.Context[0].(PeerContextID); !ok {
		t.Fatalf("context = %T", id.Context[0])
	}
	opaque, err := parseSnapshotID("@deepseek-ai/dsh@0.1.5-rc.2(" + strings.Repeat("a", 32) + ")")
	if err != nil {
		t.Fatal(err)
	}
	if context, ok := opaque.Context[0].(OpaqueContext); !ok || context.Key() != strings.Repeat("a", 32) {
		t.Fatalf("opaque = %T", opaque.Context[0])
	}
	for _, suffix := range []string{"(", ")", "()", "(bad)", "(a@1.0.0)(a@2.0.0)", "(a@file:bad)", "(a@1.0.0))"} {
		key := "@deepseek-ai/dsh@0.1.5-rc.2" + suffix
		if _, err := parseSnapshotID(key); err == nil {
			t.Errorf("%q was accepted", key)
		}
	}
	deep := "a@1.0.0"
	for index := 0; index < 101; index++ {
		deep = "a@1.0.0(" + deep + ")"
	}
	if _, err := parseSnapshotID(deep); err == nil || !strings.Contains(err.Error(), "peer context too deep") {
		t.Errorf("deep context = %v", err)
	}
}

func TestGraphGolden(t *testing.T) {
	for _, fixtureName := range []string{"tiny-dsh", "tiny-dsh-previous"} {
		prefix := fixture(t, fixtureName)
		project, err := ReadProject(prefix, KindDsh)
		if err != nil {
			t.Fatalf("%s: %v", fixtureName, err)
		}
		expectedSource, err := os.ReadFile(filepath.Join(fixtureRoot, fixtureName+"-expected.json"))
		if err != nil {
			t.Fatal(err)
		}
		// The committed golden is a formatted document; the contract is
		// JSON.stringify(parsed golden), which is what the CLI prints.
		expected, err := document.Parse(expectedSource, document.JSON)
		if err != nil {
			t.Fatal(err)
		}
		graph := project.Graph()
		if !document.DeepStrictEqual(graph.Value(), expected) {
			t.Errorf("%s graph value differs from the golden", fixtureName)
		}
		if got, want := string(graph.AppendJS(nil)), string(expected.AppendJS(nil)); got != want {
			t.Errorf("%s graph bytes differ\n got: %s\nwant: %s", fixtureName, got, want)
		}
	}
}

func TestPeerReport(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	report, err := ReportDshPeers(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unsatisfied) != 0 || report.Exact == 0 || report.CordisExact == 0 {
		t.Fatalf("report = %+v", report)
	}
	project, err := ReadDshProject(prefix)
	if err != nil {
		t.Fatal(err)
	}
	exact, err := project.ExactPeers()
	if err != nil {
		t.Fatal(err)
	}
	if exact != report.Exact {
		t.Errorf("summary exact = %d, report exact = %d", exact, report.Exact)
	}
	if _, err := ReportDshPeers(fixture(t, "derivation")); err != nil {
		t.Errorf("an override-free report = %v", err)
	}
	for _, kind := range []Kind{KindPi, KindBridge} {
		if _, err := ReadProject(fixture(t, "tiny-dsh"), kind); err == nil || !strings.Contains(err.Error(), "unexpected key") {
			t.Errorf("%v: %v", kind, err)
		}
	}
}

func TestReadRejections(t *testing.T) {
	for _, vector := range []struct {
		name  string
		file  string
		edit  func(string) string
		match string
	}{
		{"lockfile-version", "pnpm-lock.yaml", func(source string) string {
			return strings.Replace(source, `"lockfileVersion": "9.0"`, `"lockfileVersion": "6.0"`, 1)
		}, "requires v9 lock"},
		{"unexpected-manifest-key", "package.json", func(source string) string {
			return strings.Replace(source, `"private": true,`, `"private": true, "scripts": {},`, 1)
		}, "unexpected key: scripts"},
		{"integrity", "pnpm-lock.yaml", func(source string) string {
			return strings.Replace(source, `"integrity": "sha512-AAAA`, `"integrity": "sha512-AAAAA`, 1)
		}, "full sha512 required"},
		{"merge-key", "pnpm-workspace.yaml", func(source string) string {
			return strings.Replace(source, `"hoist": true`, `"<<": {"hoist": true}, "hoist": true`, 1)
		}, "merge key forbidden"},
		{"tag", "pnpm-workspace.yaml", func(source string) string {
			return strings.Replace(source, `"hoist": true`, `"hoist": !!bool true`, 1)
		}, "anchors, aliases and explicit tags forbidden"},
		{"anchor", "pnpm-workspace.yaml", func(source string) string {
			return strings.Replace(source, `"hoist": true`, `"hoist": &a true`, 1)
		}, "anchors, aliases and explicit tags forbidden"},
		{"duplicate", "pnpm-workspace.yaml", func(source string) string {
			return strings.Replace(source, `"hoist": true`, `"hoist": true, "hoist": true`, 1)
		}, "unique"},
		{"depth", "pnpm-workspace.yaml", func(source string) string {
			return strings.Replace(source, `"allowBuilds": {}`, `"allowBuilds": `+strings.Repeat("[", 101)+"0"+strings.Repeat("]", 101), 1)
		}, "depth100"},
		{"non-string-key", "pnpm-workspace.yaml", func(source string) string {
			return strings.Replace(source, `"hoist": true`, `1: true, "hoist": true`, 1)
		}, "mapping keys must be strings"},
	} {
		prefix := fixture(t, "tiny-dsh")
		path := filepath.Join(string(prefix), vector.file)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		edited := vector.edit(string(source))
		if edited == string(source) {
			t.Fatalf("%s: edit did not apply", vector.name)
		}
		if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
			t.Fatal(err)
		}
		repin(t, prefix)
		if _, err := ReadProject(prefix, KindDsh); err == nil || !strings.Contains(err.Error(), vector.match) {
			t.Errorf("%s = %v, want %q", vector.name, err, vector.match)
		}
	}
}

func TestUnreviewedInputs(t *testing.T) {
	for _, file := range []string{".npmrc", ".pnpmfile.cjs", ".pnpmfile.js", ".pnpmfile.mjs", "pnpmfile.cjs", "package-lock.json"} {
		prefix := fixture(t, "tiny-dsh")
		if err := os.WriteFile(filepath.Join(string(prefix), file), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadProject(prefix, KindDsh); err == nil || !strings.Contains(err.Error(), "unreviewed input: "+file) {
			t.Errorf("%s = %v", file, err)
		}
	}
}

func TestPinsRejectSubstitution(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	if err := WritePins(prefix, KindDsh); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(string(prefix), "pnpm-workspace.yaml")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(source, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProject(prefix, KindDsh); err == nil || !strings.Contains(err.Error(), "bytes differ from reviewed pin") {
		t.Errorf("substituted workspace = %v", err)
	}
}

// repin writes the digest list directly, the way the independent oracle does, so
// a negative test can pin input the writer itself would refuse.
func repin(t *testing.T, prefix Prefix) {
	t.Helper()
	body := &strings.Builder{}
	for _, file := range PinnedInputs {
		digest, err := inputDigest(prefix, file)
		if err != nil {
			t.Fatal(err)
		}
		body.WriteString(digest + "  " + file + "\n")
	}
	if err := os.WriteFile(filepath.Join(string(prefix), pinFile), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
