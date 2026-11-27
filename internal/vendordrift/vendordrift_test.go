package vendordrift

// The gate's own tests: the real tree passes, the derivation is not empty, and
// each kind of drift is reported by name. The last part is the one that keeps the
// gate from being a `git status` in disguise: a comparison that cannot name a
// perturbed file proves nothing when it reports a clean tree.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repositoryRoot is the checkout this package's tests run in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root: %v", err)
	}
	return root
}

// TestVendoredCopyMatchesTheLayerModule is the gate. It also holds the derivation
// it depends on: a closure that came back empty, or short of the floor, is a
// failure rather than a clean comparison over nothing. Both linked example layers are
// held at once (#252 extends the rust-dev slice's gate), each with its own floor.
func TestVendoredCopyMatchesTheLayerModule(t *testing.T) {
	report, err := Check(Options{Root: repositoryRoot(t)})
	if err != nil {
		t.Fatalf("a vendored copy is not its layer module's: %v", err)
	}
	if len(report.Modules) != len(Modules) {
		t.Fatalf("%d module derivations, want %d: one of them is not running", len(report.Modules),
			len(Modules))
	}
	for _, entry := range report.Modules {
		floor := MinPackages[entry.Module.Path]
		if len(entry.Imported) < floor || len(entry.Vendored) < floor {
			t.Fatalf("%s: derivation is implausibly small: imported %v, vendored %v",
				entry.Module.Path, entry.Imported, entry.Vendored)
		}
		if !sameStrings(entry.Imported, entry.Vendored) {
			t.Fatalf("%s: imported %v, vendored %v", entry.Module.Path, entry.Imported,
				entry.Vendored)
		}
		if entry.Files == 0 {
			t.Fatalf("%s: no file was compared", entry.Module.Path)
		}
	}
	if report.Files < MinFiles {
		t.Fatalf("compared %d files, want at least %d", report.Files, MinFiles)
	}
	if report.Shared < MinSharedRequirements {
		t.Fatalf("compared %d shared requirements, want at least %d", report.Shared,
			MinSharedRequirements)
	}
	// The gate claims it ran offline. Prove the claim here: the recorded proxy is
	// off, and the module cache it was given is still empty, so nothing was
	// downloaded into it.
	if report.Proxy != "off" {
		t.Fatalf("the derivation did not run with GOPROXY=off: %q", report.Proxy)
	}
	if !report.VendorOnly {
		t.Fatal("the closure is not entirely under vendor/")
	}
}

// TestComparePackageDirectoriesNamesEachDifference is the gate's control on real
// bytes: a copy of a vendored package with one file edited, one file missing and
// one file added must report all three, by name.
func TestComparePackageDirectoriesNamesEachDifference(t *testing.T) {
	root := repositoryRoot(t)
	if _, err := Check(Options{Root: root}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	layerDir := filepath.Join(root, "examples", "layers", "rust-dev", "helper", "toolchain")
	vendorDir := filepath.Join(root, "vendor", filepath.FromSlash(HelperModule+"/toolchain"))
	perturbed := t.TempDir()
	entries, err := os.ReadDir(vendorDir)
	if err != nil {
		t.Fatalf("read the vendored package: %v", err)
	}
	edited := ""
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(vendorDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if entry.Name() == "policy.go" {
			edited = "policy.go"
			raw = append(raw, []byte("\n// a hand edit in the vendor tree\n")...)
		}
		if err := os.WriteFile(filepath.Join(perturbed, entry.Name()), raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	if edited == "" {
		t.Fatal("policy.go is not in the vendored toolchain package: this control needs a file " +
			"that exists in both trees")
	}
	if err := os.Remove(filepath.Join(perturbed, "stage.go")); err != nil {
		t.Fatalf("remove stage.go from the copy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(perturbed, "invented.go"), []byte("package toolchain\n"), 0o644); err != nil {
		t.Fatalf("write invented.go: %v", err)
	}
	differences, _, err := ComparePackageDirectories(layerDir, perturbed)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(differences) != 3 {
		t.Fatalf("want three differences, got %d: %v", len(differences), differences)
	}
	for _, want := range []string{"policy.go: bytes differ", "stage.go: present in the layer " +
		"module, absent from vendor", "invented.go: present in vendor, absent from the layer " +
		"module"} {
		found := false
		for _, difference := range differences {
			if strings.Contains(difference, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no difference reported for %q: %v", want, differences)
		}
	}
}

// TestComparePackageDirectoriesIgnoresTestFiles holds the one exclusion: vendor
// never carries `_test.go`, so a test file on the layer side is not drift.
func TestComparePackageDirectoriesIgnoresTestFiles(t *testing.T) {
	layer := t.TempDir()
	vendored := t.TempDir()
	for _, directory := range []string{layer, vendored} {
		if err := os.WriteFile(filepath.Join(directory, "a.go"), []byte("package a\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(layer, "a_test.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatalf("write the test file: %v", err)
	}
	differences, files, err := ComparePackageDirectories(layer, vendored)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(differences) != 0 || files != 1 {
		t.Fatalf("a layer-side test file is drift: %v (files %d)", differences, files)
	}
}

// TestCheckRefusesAnEmptyClosure keeps the session's failure mode out of this
// gate: a derivation that finds one package (or none) must be red, not a clean
// comparison over an almost-empty set.
func TestCheckRefusesAnEmptyClosure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "vendor"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	syntheticTree(t, root)
	// The go-dev module is satisfied and the rust-dev module's vendor record carries one
	// package: the per-module floor is the rule under test.
	record := readFile(t, filepath.Join(root, "vendor", "modules.txt"))
	for index := 1; index < 10; index++ {
		record = strings.ReplaceAll(record,
			fmt.Sprintf("%s/pkg%d\n", Modules[1].Path, index), "")
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "modules.txt"), []byte(record),
		0o644); err != nil {
		t.Fatalf("rewrite modules.txt: %v", err)
	}
	resolved := canonical(root)
	closure := ""
	for index := 0; index < 10; index++ {
		closure += fmt.Sprintf("%s/pkg%d\t%s/vendor/%s/pkg%d\n", Modules[0].Path, index,
			resolved, Modules[0].Path, index)
	}
	closure += HelperModule + "/toolchain\t" + resolved + "/vendor/x\n"
	for index := 0; index < 20; index++ {
		closure += fmt.Sprintf("github.com/example/dep%d\t%s/vendor/dep\n", index, resolved)
	}
	_, err := Check(Options{Root: root, Go: fakeGo(closure)})
	if err == nil || !strings.Contains(err.Error(), "want at least") {
		t.Fatalf("a one-package closure must be refused, got %v", err)
	}
}

// syntheticTree writes a minimal repository the gate can read: two helper go.mod files,
// a root go.mod that links both, and a vendor record naming the same packages. A case
// then changes one thing and asserts the rule that fires.
func syntheticTree(t *testing.T, root string) {
	t.Helper()
	write := func(relative, body string) {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", relative, err)
		}
	}
	write("go.mod", "module "+MainModule+"\n\ngo 1.27.0\n\nrequire (\n"+
		"\t"+Modules[0].Path+" v0.0.0\n\t"+Modules[1].Path+" v0.0.0\n"+
		"\tgithub.com/example/shared v1.2.3\n)\n")
	for _, module := range Modules {
		write(module.Directory+"/go.mod", "module "+module.Path+"\n\ngo 1.27.0\n\nrequire "+
			"github.com/example/shared v1.2.3\n")
	}
	modules := ""
	for _, module := range Modules {
		modules += "# " + module.Path + " v0.0.0 => ./" + module.Directory + "\n" +
			"## explicit; go 1.27.0\n"
		for index := 0; index < 10; index++ {
			modules += fmt.Sprintf("%s/pkg%d\n", module.Path, index)
			// The same file in the layer and in vendor: the gate compares bytes, so a
			// synthetic tree without package directories would be testing the walk.
			source := fmt.Sprintf("package pkg%d\n", index)
			write(fmt.Sprintf("%s/pkg%d/pkg.go", module.Directory, index), source)
			write(fmt.Sprintf("vendor/%s/pkg%d/pkg.go", module.Path, index), source)
		}
	}
	write("vendor/modules.txt", modules)
}

// syntheticClosure is the import graph that matches syntheticTree: every package the
// vendor record names, resolved under vendor/, plus enough third-party packages to pass
// the whole-closure floor.
func syntheticClosure(root string) string {
	closure := ""
	for _, module := range Modules {
		for index := 0; index < 10; index++ {
			closure += fmt.Sprintf("%s/pkg%d\t%s/vendor/%s/pkg%d\n", module.Path, index, root,
				module.Path, index)
		}
	}
	for index := 0; index < 20; index++ {
		closure += fmt.Sprintf("github.com/example/dep%d\t%s/vendor/dep\n", index, root)
	}
	return closure
}

// TestCheckRefusesAStaleVendoredRecord: a package the writer imports and vendor does not
// carry is drift in the direction a file comparison cannot see.
func TestCheckRefusesAStaleVendoredRecord(t *testing.T) {
	root := t.TempDir()
	syntheticTree(t, root)
	resolved := canonical(root)
	closure := syntheticClosure(resolved) + Modules[0].Path + "/extra\t" + resolved +
		"/vendor/" + Modules[0].Path + "/extra\n"
	_, err := Check(Options{Root: root, Go: fakeGo(closure)})
	if err == nil || !strings.Contains(err.Error(), "the writer imports") {
		t.Fatalf("a stale vendor record was accepted: %v", err)
	}
}

// TestCheckRefusesAReplacementOutsideTheLayer: the commit that puts the layer's source in
// vendor/ is a local replace at the layer's own directory. A replacement pointing
// anywhere else means the gate would compare two copies that are not the pair it claims.
func TestCheckRefusesAReplacementOutsideTheLayer(t *testing.T) {
	root := t.TempDir()
	syntheticTree(t, root)
	modules := strings.Replace(readFile(t, filepath.Join(root, "vendor", "modules.txt")),
		"=> ./"+Modules[0].Directory, "=> ./somewhere/else", 1)
	if err := os.WriteFile(filepath.Join(root, "vendor", "modules.txt"), []byte(modules),
		0o644); err != nil {
		t.Fatalf("rewrite modules.txt: %v", err)
	}
	_, err := Check(Options{Root: root, Go: fakeGo(syntheticClosure(canonical(root)))})
	if err == nil || !strings.Contains(err.Error(), "is replaced with") {
		t.Fatalf("a foreign replacement was accepted: %v", err)
	}
}

// TestCheckRefusesADriftedSharedRequirement is ADR 0017's link 5 (#252): the root may not
// require a different version of a module the helper's own build selects.
func TestCheckRefusesADriftedSharedRequirement(t *testing.T) {
	root := t.TempDir()
	syntheticTree(t, root)
	path := filepath.Join(root, "go.mod")
	raw := readFile(t, path)
	drifted := strings.Replace(raw, "github.com/example/shared v1.2.3",
		"github.com/example/shared v1.2.4", 1)
	if drifted == raw {
		t.Fatalf("the shared requirement is not in the synthetic root go.mod")
	}
	if err := os.WriteFile(path, []byte(drifted), 0o644); err != nil {
		t.Fatalf("rewrite go.mod: %v", err)
	}
	_, err := Check(Options{Root: root, Go: fakeGo(syntheticClosure(canonical(root)))})
	if err == nil || !strings.Contains(err.Error(), "require different versions of a shared "+
		"module") {
		t.Fatalf("a version that would raise the layer's build was accepted: %v", err)
	}
}

// TestCheckRefusesAModuleTheRootDoesNotRequire: the link itself. Without a `require` at
// v0.0.0 there is no vendored copy for the writer to link.
func TestCheckRefusesAModuleTheRootDoesNotRequire(t *testing.T) {
	root := t.TempDir()
	syntheticTree(t, root)
	path := filepath.Join(root, "go.mod")
	raw := readFile(t, path)
	unlinked := strings.Replace(raw, "\t"+Modules[0].Path+" v0.0.0\n", "", 1)
	if unlinked == raw {
		t.Fatalf("the require line is not in the synthetic root go.mod")
	}
	if err := os.WriteFile(path, []byte(unlinked), 0o644); err != nil {
		t.Fatalf("rewrite go.mod: %v", err)
	}
	_, err := Check(Options{Root: root, Go: fakeGo(syntheticClosure(canonical(root)))})
	if err == nil || !strings.Contains(err.Error(), "must require") {
		t.Fatalf("an unlinked helper module was accepted: %v", err)
	}
}

// fakeGo answers the two questions the gate asks: the proxy in effect, and the import
// graph. A fake that answered the closure for `go env` would be refused as "not
// offline", which is a different rule than any case below is testing.
func fakeGo(closure string) func(string, []string, ...string) ([]byte, error) {
	return func(_ string, _ []string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == "env" && args[1] == "GOPROXY" {
			return []byte("off\n"), nil
		}
		return []byte(closure), nil
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// TestCheckRefusesAnOnlineDerivation: the gate reads the committed tree and nothing
// else. A proxy in effect is refused, because the derivation would then be a download
// rather than a comparison of the bytes this checkout carries.
func TestCheckRefusesAnOnlineDerivation(t *testing.T) {
	root := t.TempDir()
	syntheticTree(t, root)
	online := func(_ string, _ []string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == "env" {
			return []byte("https://proxy.golang.org\n"), nil
		}
		return []byte(syntheticClosure(canonical(root))), nil
	}
	_, err := Check(Options{Root: root, Go: online})
	if err == nil || !strings.Contains(err.Error(), "the run is not offline") {
		t.Fatalf("an online derivation was accepted: %v", err)
	}
}

// TestCheckRefusesAClosureOutsideVendor: a package resolved from a module cache is not a
// vendored copy, so residency is asserted for every module dependency in the closure.
func TestCheckRefusesAClosureOutsideVendor(t *testing.T) {
	root := t.TempDir()
	syntheticTree(t, root)
	closure := syntheticClosure(canonical(root)) + "github.com/example/elsewhere\t" +
		filepath.Join(t.TempDir(), "pkg", "mod", "github.com/example/elsewhere") + "\n"
	_, err := Check(Options{Root: root, Go: fakeGo(closure)})
	if err == nil || !strings.Contains(err.Error(), "not entirely under vendor/") {
		t.Fatalf("a module-cache package was accepted: %v", err)
	}
}

// TestCheckRefusesTooFewComparedFiles: the file floor is the other half of the empty
// derivation guard. A tree whose packages hold almost nothing is not this repository.
func TestCheckRefusesTooFewComparedFiles(t *testing.T) {
	root := t.TempDir()
	syntheticTree(t, root)
	for _, module := range Modules {
		for index := 5; index < 10; index++ {
			directory := filepath.Join(root, filepath.FromSlash(module.Directory),
				fmt.Sprintf("pkg%d", index))
			if err := os.RemoveAll(directory); err != nil {
				t.Fatalf("remove %s: %v", directory, err)
			}
			if err := os.RemoveAll(filepath.Join(root, "vendor",
				filepath.FromSlash(module.Path), fmt.Sprintf("pkg%d", index))); err != nil {
				t.Fatalf("remove the vendored package: %v", err)
			}
		}
	}
	record := strings.ReplaceAll(readFile(t, filepath.Join(root, "vendor", "modules.txt")),
		"pkg5\npkg6\npkg7\npkg8\npkg9\n", "")
	record = strings.ReplaceAll(record, "pkg5\npkg6\npkg7\npkg8\npkg9\n", "")
	for _, module := range Modules {
		for index := 5; index < 10; index++ {
			record = strings.ReplaceAll(record, fmt.Sprintf("%s/pkg%d\n", module.Path, index), "")
		}
	}
	if err := os.WriteFile(filepath.Join(root, "vendor", "modules.txt"), []byte(record),
		0o644); err != nil {
		t.Fatalf("rewrite modules.txt: %v", err)
	}
	closure := ""
	for _, module := range Modules {
		for index := 0; index < 5; index++ {
			closure += fmt.Sprintf("%s/pkg%d\t%s/vendor/%s/pkg%d\n", module.Path, index,
				canonical(root), module.Path, index)
		}
	}
	for index := 0; index < 20; index++ {
		closure += fmt.Sprintf("github.com/example/dep%d\t%s/vendor/dep\n", index,
			canonical(root))
	}
	_, err := Check(Options{Root: root, Go: fakeGo(closure)})
	if err == nil || !strings.Contains(err.Error(), "want at least "+fmt.Sprint(MinFiles)) {
		t.Fatalf("a ten-file tree was accepted: %v", err)
	}
}
