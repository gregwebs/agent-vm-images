package main

// Invariant 32: the network lives in one package and child processes in two files.
// The install verb is allowed to download (it is image content, not a build
// helper), but only internal/fetch may name net, net/http, net/url or crypto/tls,
// and only the gpg verifier and the installed-state checks may start a program.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// networkImports are the packages that reach the network. They are allowed only
// under internal/fetch: the shipped Fetch and its fixture-only loopback rewriter.
var networkImports = []string{"net", "net/http", "net/url", "crypto/tls", "net/rpc"}

// execImporters are the only packages allowed to start a child process: the gpg
// package (one reviewed program, one private keyring) and the installed-state
// checks (CARGO_HOME/bin/NAME as given).
var execImporters = map[string]bool{
	"internal/gpg/gpg.go":    true,
	"toolchain/installed.go": true,
}

// TestImportsAreReviewed walks this module's own sources: no network package
// anywhere, and os/exec only where a reviewed program is started.
func TestImportsAreReviewed(t *testing.T) {
	found := 0
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
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
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		found++
		relative := filepath.ToSlash(strings.TrimPrefix(path, "./"))
		for _, imported := range file.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			for _, network := range networkImports {
				if name == network || strings.HasPrefix(name, network+"/") {
					if !strings.HasPrefix(relative, "internal/fetch/") {
						t.Errorf("%s imports %s: only internal/fetch may reach the network",
							relative, name)
					}
				}
			}
			if name == "os/exec" && !execImporters[relative] {
				t.Errorf("%s imports os/exec; only %v may start a program", relative,
					execImporters)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 10 {
		t.Fatalf("walked %d files: the check is not looking at the module", found)
	}
}

// TestForbiddenImportMutantIsDetected is the control: the same walk over a source
// string that does import net/http must report it. Without this, a green
// TestImportsAreReviewed could mean the walk looked at nothing.
func TestForbiddenImportMutantIsDetected(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "mutant.go",
		"package toolchain\n\nimport \"net/http\"\n", parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	detected := false
	for _, imported := range file.Imports {
		name, _ := strconv.Unquote(imported.Path.Value)
		for _, network := range networkImports {
			if name == network {
				detected = true
			}
		}
	}
	if !detected {
		t.Fatal("the mutant import was not detected, so the walk proves nothing")
	}
}
