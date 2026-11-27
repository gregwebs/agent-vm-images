package main

// Invariant 32, as one committed list over *everything the pin writer is built
// from*: `avm-pin rust-toolchain` does not only link this module. The root module
// vendors this one and links `internal/fetch`, `internal/artifactinstall`,
// `internal/artifactwriter`, `internal/claudepin` and `internal/codewriter` beside
// it, so a walk over this module alone would classify the wrong tree and could be
// escaped from `cmd/avm-pin` alone (a reviewer demonstrated exactly that: an
// `os/exec` plus an `init` in `cmd/avm-pin` started a program while this test
// passed).
//
// So the classified set is derived three times and unioned: every non-test Go file
// of this module, every non-test Go file of the go-dev layer's helper module
// (#249, owner ruling G2: one register over the owned Go source of *both* layer
// helpers), and every non-test Go file of every main-module package in
// `go list -deps ./cmd/avm-pin`'s closure, all offline. Every file of the shipped
// writer's own sources and of the go-dev reader is therefore in exactly one of
// those lists, and the one table below says which guarded imports each may have.
// Test files are excluded because they are not in the shipped binary; vendor and
// testdata are excluded because they are not sources this repository owns. Each
// derivation carries its own floor, so a walk that stops seeing one of the three
// trees is red rather than quietly green.
//
// This is the register the #188 ruling means by "runs in guard-only and before any
// fixture child is built": script/test/contracts.sh runs it with `-count=1` (it scans
// other modules' sources, so Go's test cache would answer from a cached pass) in
// --guard-only mode before it builds the recipe-contract helper and before any suite
// starts a child process, and script/build/example-helper.sh — the one host build path
// every suite now uses — runs it before it compiles anything.
//
// One caller cannot inherit that: a tree with no `cmd/avm-pin` (the example-layers
// self-test's synthetic copies, which cannot contain a root module) has no root
// closure to classify, so the wrapper runs the helper-only part of nothing and says
// on stderr that it skipped the register. That is deliberate and it is the only such
// path: a build from a checkout always runs it, including every fixture build a suite
// asks for. A test that bypassed the wrapper with its own `go build` would not, which
// is why both helpers' test builders go through the wrapper and hold the returned
// binary to the wrapper's own output directory.
//
// What an import-level guard cannot see, and why the Python audit hook it replaced
// (deleted with its interpreter, #245) was a different kind of statement (R2 of
// #232): a socket opened through `syscall` rather than `net`, `os.StartProcess`,
// code reached through cgo or `plugin`, any of the above inside a dependency
// someone adds later, and anything a child process does once it is running. The
// hook vetoed those at run time in one interpreter; this rejects them at review
// time in the sources we compile. It is not equivalent, and the difference is
// deliberate: it is static, it needs no child process, and it runs on every PR.

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	mainModulePath      = "github.com/gregwebs/agent-vm-images"
	helperModulePath    = mainModulePath + "/examples/layers/rust-dev/helper"
	goDevHelperPath     = mainModulePath + "/examples/layers/go-dev/helper"
	goDevHelperRelative = "../../go-dev/helper"
)

// guardedImports are the packages that reach the network or start a program.
var guardedImports = []string{"os/exec", "net", "net/http", "net/url", "crypto/tls",
	"net/rpc"}

// reviewedImporters is the one committed list, keyed by import path and file name
// so the same relative path in the two modules cannot be confused. A file compiled
// into `avm-pin` that is not named here may import none of guardedImports.
//
//   - the layer's fetch is the module's single byte source, its gpg package starts
//     one reviewed program with a private keyring, and its installed-state checks
//     start the tools the pin names;
//   - the root's fetch is the same idea for the root writers, its gpg and cosign
//     packages start one reviewed verifier each, `artifactinstall` runs the Node
//     and GitHub CLI installers, `artifactwriter` starts one reviewed publisher,
//     and `artifactpin` parses URLs (not a network capability, listed because the
//     guarded set is conservative on purpose).
var reviewedImporters = map[string][]string{
	helperModulePath + "/internal/fetch/fetch.go":            {"crypto/tls", "net", "net/http"},
	helperModulePath + "/internal/fetch/loopback_fixture.go": {"net/http", "net/url"},
	helperModulePath + "/internal/gpg/gpg.go":                {"os/exec"},
	helperModulePath + "/toolchain/installed.go":             {"os/exec"},
	// The go-dev reader parses the provenance and evidence URLs one pin field at a
	// time. `net/url` is not a network capability (the guarded set is conservative
	// on purpose, as it is for the root's artifactpin), and the reader opens no
	// socket: `check` runs under --network=none with no Go child at all.
	goDevHelperPath + "/closure/pin.go": {"net/url"},
	// `verify-installed` starts exactly one program, the pinned go, with the argv and
	// environment package gotool builds (#250). The binary under test is never a
	// program: it is an argument to `go version -m -json`. The boundary around that
	// child is the recipe's --network=none RUN, not this list.
	goDevHelperPath + "/gotool/exec.go": {"os/exec"},
	// The fixture builds — the layer installer's and, from #252, the maintainer
	// stager's — name a loopback origin on their command line, and this file parses
	// that origin and refuses anything that is not `http://<loopback IP>:<port>`.
	// `net/url` is a parser here, not a client: no shipped build can reach it.
	goDevHelperPath + "/gotool/loopback.go": {"net/url"},
	// The `example_fixture` seam parses the loopback origin it was given, and refuses
	// anything that is not `http://<loopback IP>:<port>` before the installer sees it
	// (#251). `net/url` is a parser, not a client, in the same sense as the root's
	// artifactpin and the go-dev loader. The file exists only in a `-tags
	// example_fixture` build (ADR 0017, decision D1), and the shipped binary's bytes
	// carry none of it — `seams_test.go` holds both halves.
	goDevHelperPath + "/seams_fixture.go":                   {"net/url"},
	mainModulePath + "/internal/artifactpin/rules.go":       {"net/url"},
	mainModulePath + "/internal/artifactinstall/archive.go": {"os/exec"},
	mainModulePath + "/internal/artifactinstall/install.go": {"os/exec"},
	mainModulePath + "/internal/artifactwriter/exec.go":     {"os/exec"},
	mainModulePath + "/internal/claudepin/gpg.go":           {"os/exec"},
	mainModulePath + "/internal/codewriter/cosign.go":       {"os/exec"},
	mainModulePath + "/internal/fetch/fetch.go":             {"net/http", "net/url"},
}

// TestImportsAreReviewed holds the writer's and both helpers' whole compile
// closures to the list.
func TestImportsAreReviewed(t *testing.T) {
	files, err := writerSources()
	if err != nil {
		t.Fatal(err)
	}
	// A walk that found almost nothing must be red: a green result over an empty
	// set is the failure this assertion exists to prevent.
	if len(files) < 40 {
		t.Fatalf("classified %d source files, want at least 40: the walk is not looking at "+
			"the writer", len(files))
	}
	// All three derivations must be present, or one of them is not running.
	layer, goDev, main := 0, 0, 0
	for name := range files {
		switch {
		case strings.HasPrefix(name, helperModulePath+"/"):
			layer++
		case strings.HasPrefix(name, goDevHelperPath+"/"):
			goDev++
		default:
			main++
		}
	}
	if layer < 10 || goDev < 8 || main < 10 {
		t.Fatalf("classified %d rust-dev, %d go-dev and %d main-module files: all three "+
			"derivations must run", layer, goDev, main)
	}
	for _, violation := range importViolations(files) {
		t.Error(violation)
	}
}

// writerSources is the union of this module's sources, the go-dev helper module's
// sources and the main-module sources in `avm-pin`'s import graph.
func writerSources() (map[string][]string, error) {
	files := map[string][]string{}
	layer, err := filesOf(".", helperModulePath)
	if err != nil {
		return nil, err
	}
	for name, imports := range layer {
		files[name] = imports
	}
	goDev, err := filesOf(goDevHelperRelative, goDevHelperPath)
	if err != nil {
		return nil, err
	}
	for name, imports := range goDev {
		files[name] = imports
	}
	root, err := repositoryRoot()
	if err != nil {
		return nil, err
	}
	environment := []string{"GOFLAGS=-mod=vendor", "GOPROXY=off"}
	command := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}\t{{.Dir}}",
		"./cmd/avm-pin")
	command.Dir = root
	command.Env = append(os.Environ(), environment...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, errors.New("go list ./cmd/avm-pin: " + err.Error() + ": " +
			strings.TrimSpace(stderr.String()))
	}
	for _, line := range strings.Split(string(output), "\n") {
		path, directory, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || path == "" || !strings.HasPrefix(path, mainModulePath+"/") ||
			strings.HasPrefix(path, helperModulePath) {
			continue
		}
		packageFiles, err := packageImports(directory)
		if err != nil {
			return nil, err
		}
		for name, imports := range packageFiles {
			files[path+"/"+name] = imports
		}
	}
	return files, nil
}

// filesOf walks a module directory and keys each file by module path and name.
func filesOf(root, modulePath string) (map[string][]string, error) {
	files := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "vendor" || info.Name() == "testdata" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		imports, err := parseImports(path)
		if err != nil {
			return err
		}
		files[modulePath+"/"+filepath.ToSlash(relative)] = imports
		return nil
	})
	return files, err
}

// packageImports parses the non-test Go files directly inside one package
// directory, keyed by file name.
func packageImports(directory string) (map[string][]string, error) {
	files := map[string][]string{}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") ||
			strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		imports, err := parseImports(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		files[entry.Name()] = imports
	}
	return files, nil
}

func parseImports(path string) ([]string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	imports := make([]string, 0, len(parsed.Imports))
	for _, imported := range parsed.Imports {
		name, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, err
		}
		imports = append(imports, name)
	}
	return imports, nil
}

// repositoryRoot is the module that builds `avm-pin`, found by walking up from this
// module's directory. It fails rather than skips: a repo checkout always has it,
// and a case that silently does not run is the failure mode this file guards.
func repositoryRoot() (string, error) {
	directory, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "cmd", "avm-pin", "main.go")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("cannot find the repository that builds cmd/avm-pin: this " +
				"boundary covers the writer's whole compile closure and will not pass over a " +
				"tree it could not find")
		}
		directory = parent
	}
}

// importViolations is the rule: a guarded import in a file that is not reviewed for
// it, or a guarded import a reviewed file did not list. Each message names the file
// and the import, because that is what a failure has to be actionable for.
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

// TestImportBoundaryControls holds both sides of the rule. A synthetic file that
// adds a guarded import fails and is named -- in either module, because a file in
// `cmd/avm-pin` is compiled into the writer too -- and a comment or a string that
// spells the same path is not an import and passes. Without the second control the
// check could be a substring search; without the first it could be looking at
// nothing.
func TestImportBoundaryControls(t *testing.T) {
	directory := t.TempDir()
	write := func(name, body string) {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("mutant.go", "package toolchain\n\nimport \"os/exec\"\n\nvar _ = exec.Command\n")
	files, err := filesOf(directory, helperModulePath)
	if err != nil {
		t.Fatal(err)
	}
	// The escape a reviewer demonstrated, keyed the way the closure keys it: a file
	// in the root binary, which the old walk could not see at all.
	files[mainModulePath+"/cmd/avm-pin/escape.go"] = []string{"os/exec", "net/http"}
	// The same escape in the other layer's helper, which the register must also
	// classify (#249): a go-dev reader that grew `net/http` is not reviewed for it.
	files[goDevHelperPath+"/closure/socket.go"] = []string{"net", "net/http"}
	violations := importViolations(files)
	// Five: the layer mutant's one import, both of the root escape's, and both of
	// the go-dev escape's.
	if len(violations) != 5 {
		t.Fatalf("want five violations (one rust-dev, two root, two go-dev), got %v",
			violations)
	}
	for _, want := range []string{"mutant.go imports os/exec",
		"cmd/avm-pin/escape.go imports os/exec", "cmd/avm-pin/escape.go imports net/http",
		"closure/socket.go imports net", "closure/socket.go imports net/http"} {
		found := false
		for _, violation := range violations {
			if strings.Contains(violation, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no violation for %q: %v", want, violations)
		}
	}

	// The control that proves this is a parsed import list and not a text search:
	// the same path inside a comment and inside a string is not an import.
	write("mutant.go", "package toolchain\n\n// import \"os/exec\" is reviewed elsewhere.\n\n"+
		"var example = \"import \\\"os/exec\\\"\"\n\nvar _ = example\n")
	files, err = filesOf(directory, helperModulePath)
	if err != nil {
		t.Fatal(err)
	}
	if violations := importViolations(files); len(violations) != 0 {
		t.Fatalf("a comment or string mentioning os/exec was reported as an import: %v",
			violations)
	}
}
