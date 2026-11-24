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
// failure rather than a clean comparison over nothing.
func TestVendoredCopyMatchesTheLayerModule(t *testing.T) {
	report, err := Check(Options{Root: repositoryRoot(t)})
	if err != nil {
		t.Fatalf("the vendored copy of %s is not the layer module's: %v", HelperModule, err)
	}
	if len(report.Imported) < MinPackages || len(report.Vendored) < MinPackages {
		t.Fatalf("derivation is implausibly small: imported %v, vendored %v", report.Imported,
			report.Vendored)
	}
	if !sameStrings(report.Imported, report.Vendored) {
		t.Fatalf("imported %v, vendored %v", report.Imported, report.Vendored)
	}
	if report.Files < MinFiles {
		t.Fatalf("compared %d files, want at least %d", report.Files, MinFiles)
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
	report, err := Check(Options{Root: root})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	layerDir := filepath.Join(root, "examples", "layers", "rust-dev", "helper", "toolchain")
	vendorDir := filepath.Join(root, "vendor", filepath.FromSlash(report.Vendored[0]))
	// Use the toolchain package by name rather than by position in the closure.
	for _, pkg := range report.Vendored {
		if strings.HasSuffix(pkg, "/toolchain") {
			vendorDir = filepath.Join(root, "vendor", filepath.FromSlash(pkg))
		}
	}
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
	modules := "# " + HelperModule + " v0.0.0 => ./examples/layers/rust-dev/helper\n" +
		"## explicit; go 1.27.0\n" + HelperModule + "/toolchain\n"
	if err := os.WriteFile(filepath.Join(root, "vendor", "modules.txt"), []byte(modules), 0o644); err != nil {
		t.Fatalf("write modules.txt: %v", err)
	}
	closure := HelperModule + "/toolchain\t" + root + "/vendor/x\n"
	for index := 0; index < 12; index++ {
		closure += fmt.Sprintf("github.com/example/dep%d\t%s/vendor/dep\n", index, root)
	}
	_, err := Check(Options{Root: root, Go: func(string, []string, ...string) ([]byte, error) {
		return []byte(closure), nil
	}})
	if err == nil || !strings.Contains(err.Error(), "want at least") {
		t.Fatalf("a one-package closure must be refused, got %v", err)
	}
}
