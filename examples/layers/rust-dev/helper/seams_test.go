//go:build !example_fixture

package main

// The fixture seam is a build tag, so this file's job is to show the seam is
// absent from the shipped binary rather than merely unused (ADR 0017, decision
// D1). Two builds are made from this same module: the shipped one, and one with
// `-tags example_fixture`, which is the control that proves the checks can fail.
// The assertions are over the compiled bytes and over the parser's grammar, not
// over a runtime branch, so a seam that leaked into the shipped build fails here
// even if nothing calls it.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seamNames are the suite-only inputs a fixture build honours. They are written
// out rather than read from the tagged file, because a shipped build cannot
// compile that file: the list is the contract this test holds.
var seamNames = []string{"fixture-trust", "fixture-upstream", "fixture-bootstrap-exec"}

func build(t *testing.T, tags string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "rust-dev-helper")
	args := []string{"build", "-trimpath", "-buildvcs=false", "-o", binary}
	if tags != "" {
		args = append(args, "-tags", tags)
	}
	args = append(args, ".")
	command := exec.Command("go", args...)
	command.Env = append(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build (tags %q): %v\n%s", tags, err, output)
	}
	return binary
}

func runBinary(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	err := command.Run()
	if err == nil {
		return output.String(), 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return output.String(), exit.ExitCode()
	}
	t.Fatalf("running %s: %v", binary, err)
	return "", 0
}

func TestShippedBinaryCarriesNoFixtureSeam(t *testing.T) {
	if len(fixtureSeams) != 0 {
		t.Fatalf("the shipped build honours fixture seams: %v", fixtureSeams)
	}
	shippedPath := build(t, "")
	taggedPath := build(t, "example_fixture")
	shipped, err := os.ReadFile(shippedPath)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := os.ReadFile(taggedPath)
	if err != nil {
		t.Fatal(err)
	}
	// The control first: the fixture build must carry the names, or the absence
	// below would pass for a program that names no seam at all.
	for _, name := range seamNames {
		if !bytes.Contains(tagged, []byte(name)) {
			t.Errorf("the example_fixture build does not carry the seam %q, so the absence check below proves nothing", name)
		}
	}
	for _, name := range seamNames {
		if bytes.Contains(shipped, []byte(name)) {
			t.Errorf("the shipped binary carries the fixture seam %q", name)
		}
	}
	// A flag is a second way in, so the grammar is held too: the shipped binary
	// must refuse a flag it has no field for, and report no seam name while it
	// does.
	output, status := runBinary(t, shippedPath, "--fixture-seams")
	if status != 1 {
		t.Fatalf("the shipped binary answered --fixture-seams with status %d, want 1:\n%s", status, output)
	}
	if !strings.Contains(output, "usage: "+usageText) {
		t.Fatalf("the shipped binary did not report usage for --fixture-seams:\n%s", output)
	}
	if strings.Contains(output, "fixture-") {
		t.Fatalf("the shipped binary's refusal names a fixture seam:\n%s", output)
	}
	output, status = runBinary(t, taggedPath, "--fixture-seams")
	if status != 0 {
		t.Fatalf("the example_fixture build refused --fixture-seams with status %d:\n%s", status, output)
	}
	for _, name := range seamNames {
		if !strings.Contains(output, name) {
			t.Errorf("the example_fixture build did not list the seam %q:\n%s", name, output)
		}
	}
}
