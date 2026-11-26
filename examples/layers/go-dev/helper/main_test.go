package main

// The command's contract: status, stdout and stderr. The Python reader's table is
// the authority until #254, and both readers print the same release on stdout and
// the same `gopls-closure: ` line on stderr.

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// build compiles the shipped binary the way the layer's stage does.
func build(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "go-dev-helper")
	command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binary, ".")
	command.Env = append(os.Environ(), "GOFLAGS=-mod=vendor", "GOPROXY=off")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	return binary
}

func runBinary(t *testing.T, binary string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running %s: %v", binary, err)
		}
		status = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), status
}

// committedData is the real reviewed directory, absolute.
func committedData(t *testing.T) string {
	t.Helper()
	directory, err := filepath.Abs(filepath.Join("..", "data", "gopls-closure"))
	if err != nil {
		t.Fatalf("resolve the committed closure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "pin.json")); err != nil {
		t.Fatalf("the committed closure is not where this test expects it (%s): %v",
			directory, err)
	}
	return directory
}

// TestStatusMatrix holds the whole invocation table.
func TestStatusMatrix(t *testing.T) {
	binary := build(t)
	committed := committedData(t)
	for _, testCase := range []struct {
		label  string
		args   []string
		status int
		stdout string
		stderr string
	}{
		{"no verb", nil, 1, "", "gopls-closure: usage: " + usageText},
		{"an unknown flag before a verb", []string{"--bogus"}, 1, "", "gopls-closure: usage: " + usageText},
		{"check without a directory", []string{"check"}, 2, "", "gopls-closure: usage: " + usageText},
		{"check with an extra positional", []string{"check", committed, "extra"}, 2, "",
			"gopls-closure: usage: " + usageText},
		{"check with an unknown flag", []string{"check", committed, "--bogus"}, 2, "",
			"gopls-closure: usage: " + usageText},
		{"check the committed closure", []string{"check", committed}, 0, "v0.23.0\n", ""},
		{"check with the Dockerfile assertions",
			[]string{"check", committed, "--expect-gopls", "v0.23.0", "--expect-go", "1.27.3"},
			0, "v0.23.0\n", ""},
		{"check with a wrong gopls assertion",
			[]string{"check", committed, "--expect-gopls", "v0.24.0"}, 1, "",
			"gopls-closure: asserted gopls version v0.24.0 != pinned v0.23.0\n"},
		{"check with a wrong Go assertion",
			[]string{"check", committed, "--expect-go", "1.27.4"}, 1, "",
			"gopls-closure: pin generated with go1.27.3; the layer installs go1.27.4\n"},
		{"check with an empty gopls assertion",
			[]string{"check", committed, "--expect-gopls", ""}, 1, "",
			"gopls-closure: --expect-gopls: expected an exact vX.Y.Z release version\n"},
		{"check a missing directory", []string{"check", filepath.Join(committed, "absent")}, 1,
			"", "gopls-closure: not a directory: "},
		{"check a malformed directory", []string{"check", t.TempDir()}, 1, "",
			"gopls-closure: missing input: go.mod\n"},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			stdout, stderr, status := runBinary(t, binary, testCase.args...)
			if status != testCase.status {
				t.Fatalf("status %d, want %d (stderr %q)", status, testCase.status, stderr)
			}
			if stdout != testCase.stdout {
				t.Fatalf("stdout %q, want %q", stdout, testCase.stdout)
			}
			if !strings.HasPrefix(stderr, testCase.stderr) {
				t.Fatalf("stderr %q, want it to start with %q", stderr, testCase.stderr)
			}
		})
	}
}

// TestCheckRunsWithoutAToolchainOrEnvironment is the observable half of the claim
// the recipe rests on: `check` needs no Go toolchain and no inherited module
// policy, so it runs with only the binary and the data mounted. An empty PATH and a
// hostile GO* environment are what makes that observable — a reader that exec'd
// `go`, or read GOFLAGS/GOPROXY/GOTOOLCHAIN, cannot produce the selected release
// under them.
//
// It deliberately does not claim to observe a *network request*: no host-side test
// can, because nothing is mounted to observe with. What forbids the request is the
// recipe's `--network=none` for that RUN, and what forbids the code from making one
// is this module's import register (no `net`, `net/http` or `os/exec` in any
// shipped file — see `TestImportsAreReviewed` in the rust-dev helper, which
// classifies this module).
func TestCheckRunsWithoutAToolchainOrEnvironment(t *testing.T) {
	binary := build(t)
	committed := committedData(t)
	emptyPath := "PATH=" + t.TempDir()
	for _, environment := range [][]string{
		{emptyPath},
		{emptyPath, "GOFLAGS=-mod=mod", "GOPROXY=direct", "GOTOOLCHAIN=auto",
			"GOOS=windows", "GOARCH=386", "GOENV=off", "GOFLAGS=-tags=example_fixture"},
	} {
		command := exec.Command(binary, "check", committed)
		// Later entries win for a duplicated variable, so these replace the host's.
		command.Env = append(os.Environ(), environment...)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("check with %v: %v (stderr %q)", environment, err, stderr.String())
		}
		if stdout.String() != "v0.23.0\n" || stderr.String() != "" {
			t.Fatalf("check with %v printed %q / %q", environment, stdout.String(),
				stderr.String())
		}
	}
}
