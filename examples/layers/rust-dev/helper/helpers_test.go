package main

// Both builds of this module are made by these helpers: the shipped one is what
// the image mounts, and the `-tags example_fixture` control proves the checks can
// fail. They live in an untagged file so the fixture build's own test run can
// build both.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

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
