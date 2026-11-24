package main

// Invariant 32, as one committed list: the network lives in one package, and a
// child process may be started from two files.
//
// This test is the replacement for the Python audit hook (`guards.install`) that
// the Rust suites installed in every fixture child (slice 4a of #187). The hook
// watched `socket` and `subprocess` calls at run time in one interpreter; this
// watches the *sources* of the module the shipped binary is built from, which is
// the boundary that matters now: the writer and the installer are Go, so the
// question "what can this program reach?" is answered by its import graph, not by
// a runtime hook. It parses with go/parser, starts no child process, and runs on
// every PR.
//
// One list, not two: a file is either reviewed for a guarded import or it may
// import none of them, so there is no second table to keep in step. Test files are
// skipped because they are not in the shipped binary -- and because the fixture
// build's own tests then cannot widen the shipped surface.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// guardedImports are the packages that reach the network or start a program. Any
// file not named in reviewedImporters may import none of them.
var guardedImports = []string{"os/exec", "net", "net/http", "net/url", "crypto/tls",
	"net/rpc"}

// reviewedImporters is the one committed list: every source file of this module
// that may import a guarded package, and exactly which ones it may import.
//
//   - internal/fetch is the module's single byte source, and the fixture build's
//     loopback rewriter is the one change a suite gets to it (Q1 of #231).
//   - internal/gpg starts one reviewed program with a private keyring.
//   - toolchain/installed.go starts the tools the pin names, from the install
//     homes the caller gave.
//
// The writer's staging code is not a fourth entry: it downloads through
// internal/fetch and verifies through internal/gpg, so it names neither family.
var reviewedImporters = map[string][]string{
	"internal/fetch/fetch.go":            {"crypto/tls", "net", "net/http"},
	"internal/fetch/loopback_fixture.go": {"net/http", "net/url"},
	"internal/gpg/gpg.go":                {"os/exec"},
	"toolchain/installed.go":             {"os/exec"},
}

// TestImportsAreReviewed holds this module to the list above.
func TestImportsAreReviewed(t *testing.T) {
	files, err := moduleSources(".")
	if err != nil {
		t.Fatal(err)
	}
	// A walk that found nothing, or almost nothing, must be red: a green result
	// over an empty set is the failure this assertion exists to prevent.
	if len(files) < 10 {
		t.Fatalf("walked %d source files: the check is not looking at the module", len(files))
	}
	for _, violation := range importViolations(files) {
		t.Error(violation)
	}
}

// moduleSources parses every non-test Go file under root into file -> imports.
func moduleSources(root string) (map[string][]string, error) {
	files := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "vendor" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		imports := make([]string, 0, len(parsed.Imports))
		for _, imported := range parsed.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			imports = append(imports, name)
		}
		files[filepath.ToSlash(relative)] = imports
		return nil
	})
	return files, err
}

// importViolations is the rule: a guarded import in a file that is not reviewed
// for it, or a guarded import a reviewed file did not list. Each message names the
// file and the import, because that is what a failure has to be actionable for.
func importViolations(files map[string][]string) []string {
	violations := []string{}
	names := append([]string{}, keysOf(files)...)
	sort.Strings(names)
	for _, name := range names {
		allowed := reviewedImporters[name]
		permitted := map[string]bool{}
		for _, entry := range allowed {
			permitted[entry] = true
		}
		for _, imported := range files[name] {
			if !isGuarded(imported) {
				continue
			}
			if !permitted[imported] {
				violations = append(violations, violationFor(name, imported, allowed))
			}
		}
	}
	return violations
}

func violationFor(name, imported string, allowed []string) string {
	if len(allowed) == 0 {
		return name + " imports " + imported + "; no file may import it unless it is in " +
			"reviewedImporters"
	}
	return name + " imports " + imported + "; its reviewed list is " +
		strings.Join(allowed, ", ")
}

func isGuarded(imported string) bool {
	for _, guarded := range guardedImports {
		if imported == guarded {
			return true
		}
	}
	return false
}

func keysOf(files map[string][]string) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}

// TestImportBoundaryControls holds both sides of the rule: a synthetic file that
// adds a guarded import fails and is named, and a comment or a string that spells
// the same path is not an import and passes. Without the second control the check
// could be a substring search; without the first it could be looking at nothing.
func TestImportBoundaryControls(t *testing.T) {
	directory := t.TempDir()
	write := func(name, body string) {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("mutant.go", "package toolchain\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n")
	files, err := moduleSources(directory)
	if err != nil {
		t.Fatal(err)
	}
	violations := importViolations(files)
	if len(violations) != 1 || !strings.Contains(violations[0], "mutant.go") ||
		!strings.Contains(violations[0], "os/exec") {
		t.Fatalf("the synthetic os/exec import: %v", violations)
	}

	// The control that proves this is a parsed import list and not a text search:
	// the same path inside a comment and inside a string is not an import.
	write("mutant.go", "package toolchain\n\n// import \"os/exec\" is reviewed elsewhere.\n\n"+
		"var example = \"import \\\"os/exec\\\"\"\n\nvar _ = example\n")
	files, err = moduleSources(directory)
	if err != nil {
		t.Fatal(err)
	}
	if violations := importViolations(files); len(violations) != 0 {
		t.Fatalf("a comment or string mentioning os/exec was reported as an import: %v",
			violations)
	}
}
