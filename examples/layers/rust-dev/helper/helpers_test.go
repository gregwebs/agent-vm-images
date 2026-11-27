package main

// Both builds of this module are made by these helpers: the shipped one is what
// the image mounts, and the `-tags example_fixture` control proves the checks can
// fail. They live in an untagged file so the fixture build's own test run can
// build both.

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// build compiles this module through script/build/example-helper.sh, the one host
// build path, never with `go build` of its own.
//
// The wrapper runs the guarded-import register before it compiles anything (owner
// ruling G2 of #188), so a suite cannot build a tree the register refuses; the
// returned path is held to the wrapper's own output directory, which a hand-rolled
// `-o $TMPDIR/...` cannot satisfy. `tags` is the fixture variant or none, exactly as
// the wrapper names them.
func build(t *testing.T, tags string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	args := []string{filepath.Join(root, "script", "build", "example-helper.sh"),
		"rust-dev", "--root", root, "--os", runtime.GOOS}
	switch tags {
	case "":
	case "example_fixture":
		args = append(args, "--fixture")
	default:
		t.Fatalf("no build path for tags %q", tags)
	}
	command := exec.Command("bash", args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("the helper build refused this tree (tags %q): %v\n%s%s", tags, err,
			stdout.String(), stderr.String())
	}
	binary := strings.TrimSpace(stdout.String())
	if !filepath.IsAbs(binary) {
		t.Fatalf("the builder printed %q, which is not an absolute path", binary)
	}
	wanted := filepath.Join(root, "target", "example-helper", "rust-dev",
		runtime.GOOS+"-"+runtime.GOARCH)
	if filepath.Dir(binary) != wanted {
		t.Fatalf("the test built %s, which is not the reviewed build path %s", binary, wanted)
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
