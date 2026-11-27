//go:build !example_fixture

package main

// `avm-pin gopls`: the shipped build's grammar, and the fixture seam's absence from it
// (ADR 0017, decision D1). Two builds are made from this package and the assertions are
// over the compiled bytes and the parser's grammar, not over a runtime branch, so a
// seam that leaked into the shipped binary fails here even if nothing calls it. The
// staging runs themselves are held by internal/goplsstage's tests and by
// script/test/gopls-stage-parity.py.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goplsSeamNames are the suite-only inputs a fixture build honours. Written out rather
// than imported, because a shipped build cannot compile the tagged file: this list is
// the contract the test holds.
var goplsSeamNames = []string{"fixture-seams", "fixture-proxy", "fixture-sumdb"}

func TestShippedGoplsWriterCarriesNoFixtureSeam(t *testing.T) {
	shippedPath := buildAVMPin(t, "")
	taggedPath := buildAVMPin(t, "example_fixture")
	shipped, err := os.ReadFile(shippedPath)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := os.ReadFile(taggedPath)
	if err != nil {
		t.Fatal(err)
	}
	// The control first: the fixture build must carry the names, or the absence below
	// would pass for a program that names no seam at all.
	for _, name := range goplsSeamNames {
		if !bytes.Contains(tagged, []byte(name)) {
			t.Errorf("the example_fixture build does not carry the seam %q, so the absence "+
				"check below proves nothing", name)
		}
		if bytes.Contains(shipped, []byte(name)) {
			t.Errorf("the shipped binary carries the fixture seam %q", name)
		}
	}
	output, status := runAVMPin(t, shippedPath, "gopls", "--fixture-seams")
	if status != 2 {
		t.Fatalf("the shipped binary answered --fixture-seams with status %d, want 2:\n%s",
			status, output)
	}
	// The refusal is the parser's own unknown-argument answer, not a seam list: the
	// flag is echoed because the argument was typed, and nothing else about it exists.
	if !strings.Contains(output, "unknown argument --fixture-seams") ||
		strings.Contains(output, "--fixture-proxy") {
		t.Fatalf("the shipped binary's refusal is not the parser's unknown-argument answer:\n%s",
			output)
	}
	output, status = runAVMPin(t, taggedPath, "gopls", "--fixture-seams")
	if status != 0 {
		t.Fatalf("the example_fixture build refused --fixture-seams with status %d:\n%s",
			status, output)
	}
	for _, name := range goplsSeamNames {
		if !strings.Contains(output, name) {
			t.Errorf("the example_fixture build did not list the seam %q:\n%s", name, output)
		}
	}
	// A fixture origin that is not loopback is refused rather than served: the seam
	// accepts the suite's recorder and nothing else.
	output, status = runAVMPin(t, taggedPath, "gopls", "stage", "--gopls", "v1.2.3", "--go",
		"1.27.3", "--current", filepath.Join(t.TempDir(), "none"), "--out",
		filepath.Join(t.TempDir(), "out"), "--dockerfile", filepath.Join(t.TempDir(), "Dockerfile"),
		"--fixture-proxy", "https://proxy.golang.org", "--fixture-sumdb", "off")
	if status != 2 || !strings.Contains(output, "must be http://IP:PORT") {
		t.Fatalf("a non-loopback fixture origin: status %d:\n%s", status, output)
	}
}

// TestGoplsStageRefusesInGitHubActions is the outward contract: status 2 and one line,
// before any path is inspected or any compiler resolved.
func TestGoplsStageRefusesInGitHubActions(t *testing.T) {
	work := t.TempDir()
	dockerfile := filepath.Join(work, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("ARG GOPLS_VERSION=v1.2.3\n"), 0o644); err != nil {
		t.Fatalf("write the Dockerfile: %v", err)
	}
	shippedPath := buildAVMPin(t, "")
	out := filepath.Join(work, "out")
	output, status := runAVMPinEnv(t, shippedPath, []string{"GITHUB_ACTIONS=true"},
		"gopls", "stage", "--gopls", "v1.2.3", "--go", "1.27.3", "--current",
		filepath.Join(work, "none"), "--out", out, "--dockerfile", dockerfile)
	if status != 2 {
		t.Fatalf("CI was not refused with status 2: status %d:\n%s", status, output)
	}
	if !strings.Contains(output, "gopls-closure-update: refusing stage in GitHub Actions") {
		t.Fatalf("the CI refusal does not carry the frozen prefix:\n%s", output)
	}
	if _, err := os.Lstat(out); err == nil {
		t.Fatal("the CI refusal created its --out directory")
	}
}

// TestGoplsStageRequiresAbsolutePaths: every path the writer acts on is absolute, so a
// relative one is reported as the malformed invocation rather than resolved against
// whatever directory the maintainer happened to be in.
func TestGoplsStageRequiresAbsolutePaths(t *testing.T) {
	shippedPath := buildAVMPin(t, "")
	output, status := runAVMPin(t, shippedPath, "gopls", "stage", "--gopls", "v1.2.3", "--go",
		"1.27.3", "--current", "relative", "--out", "/tmp/out", "--dockerfile",
		"/tmp/Dockerfile")
	if status != 2 || !strings.Contains(output, "every path must be absolute: relative") {
		t.Fatalf("a relative path: status %d:\n%s", status, output)
	}
}
