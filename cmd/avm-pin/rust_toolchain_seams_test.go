//go:build !example_fixture

package main

// The writer's fixture seam is a build tag, so this file's job is to show the
// seam is absent from the shipped binary rather than merely unused (ADR 0017,
// decision D1). Two builds are made from this package: the shipped one and one
// with `-tags example_fixture`. The assertions are over the compiled bytes and
// over the parser's grammar, not over a runtime branch, so a seam that leaked
// into the shipped binary fails here even if nothing calls it.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// rustToolchainSeamNames are the suite-only inputs a fixture build honours.
// Written out rather than imported, because a shipped build cannot compile the
// tagged file: this list is the contract the test holds.
var rustToolchainSeamNames = []string{"FIXTURE_DIST", "FIXTURE_FAULT", "FIXTURE_FETCH_LOG"}

func buildAVMPin(t *testing.T, tags string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "avm-pin")
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

func runAVMPin(t *testing.T, binary string, args ...string) (string, int) {
	t.Helper()
	return runAVMPinEnv(t, binary, []string{"GITHUB_ACTIONS="}, args...)
}

// runAVMPinEnv runs the binary with extra environment entries appended, so a test
// can state the variable under test instead of inheriting the runner's.
func runAVMPinEnv(t *testing.T, binary string, extra []string, args ...string) (string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Env = append(os.Environ(), extra...)
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

func TestShippedPinWriterCarriesNoFixtureSeam(t *testing.T) {
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
	// The control first: the fixture build must carry the names, or the absence
	// below would pass for a program that names no seam at all.
	for _, name := range rustToolchainSeamNames {
		if !bytes.Contains(tagged, []byte(name)) {
			t.Errorf("the example_fixture build does not carry the seam %q, so the absence check below proves nothing", name)
		}
		if bytes.Contains(shipped, []byte(name)) {
			t.Errorf("the shipped binary carries the fixture seam %q", name)
		}
	}
	// A flag is a second way in, so the grammar is held too: the shipped binary
	// must refuse a flag it has no field for, and name no seam while it does.
	output, status := runAVMPin(t, shippedPath, "rust-toolchain", "--fixture-seams")
	if status != 2 {
		t.Fatalf("the shipped binary answered --fixture-seams with status %d, want 2:\n%s",
			status, output)
	}
	if strings.Contains(output, "FIXTURE_") {
		t.Fatalf("the shipped binary's refusal names a fixture seam:\n%s", output)
	}
	// The fixture build answers the question, and every verb it lists is one the
	// suite actually sets.
	output, status = runAVMPin(t, taggedPath, "rust-toolchain", "--fixture-seams")
	if status != 0 {
		t.Fatalf("the example_fixture build refused --fixture-seams with status %d:\n%s",
			status, output)
	}
	for _, name := range rustToolchainSeamNames {
		if !strings.Contains(output, name) {
			t.Errorf("the example_fixture build did not list the seam %q:\n%s", name, output)
		}
	}
	// Without the environment the fixture build refuses rather than falling back
	// to the network: a fixture binary with no store is a suite bug, not a
	// maintainer run.
	output, status = runAVMPin(t, taggedPath, "rust-toolchain", "check", t.TempDir())
	if status != 1 || !strings.Contains(output, "rust_toolchain: the fixture build needs FIXTURE_DIST") {
		t.Fatalf("the fixture build without a store: status %d:\n%s", status, output)
	}
}
