package pnpm

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repositoryRoot is the checkout the entry points live in. The two shell files are
// the image's mount points, so the test drives the published bytes rather than a
// copy.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// entryPoint drives one shell entry point with a named interpreter.
func entryPoint(t *testing.T, interpreter, script string, environment []string, arguments ...string) (string, int) {
	t.Helper()
	command := exec.Command(interpreter, append([]string{script}, arguments...)...) //nolint:gosec // the reviewed entry point.
	command.Env = environment
	command.Dir = repositoryRoot(t)
	var stderr strings.Builder
	command.Stdout = &stderr
	command.Stderr = &stderr
	err := command.Run()
	status := 0
	if err != nil {
		exit, ok := err.(*exec.ExitError) //nolint:errorlint // exec returns the concrete type.
		if !ok {
			t.Fatalf("cannot run %s: %v", script, err)
		}
		status = exit.ExitCode()
	}
	return stderr.String(), status
}

// TestEntryPointsNeedTheHelper is the contract the image mount depends on: a
// missing helper is a named refusal, not a shell error or a silent success.
func TestEntryPointsNeedTheHelper(t *testing.T) {
	root := repositoryRoot(t)
	absent := filepath.Join(t.TempDir(), "absent", "recipe-contract")
	cases := []struct {
		name        string
		interpreter string
		script      string
		diagnostic  string
	}{
		{
			name:        "the bootstrap entry point",
			interpreter: "bash",
			script:      filepath.Join(root, "images/install-pnpm.sh"),
			diagnostic:  "install-pnpm.sh: no recipe-contract helper at " + absent,
		},
		{
			name:        "the certification entry point",
			interpreter: "sh",
			script:      filepath.Join(root, "images/pnpm/verify-pnpm.sh"),
			diagnostic:  "verify-pnpm.sh: no recipe-contract helper at " + absent,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := append([]string{"RECIPE_CONTRACT_HELPER=" + absent}, os.Environ()...)
			message, status := entryPoint(t, testCase.interpreter, testCase.script, environment)
			if status != 1 {
				t.Fatalf("status is %d, want 1 (output %q)", status, message)
			}
			if !strings.Contains(message, testCase.diagnostic) {
				t.Fatalf("output %q does not carry %q", message, testCase.diagnostic)
			}
		})
	}
}

// TestEntryPointsForwardArgv proves the wrappers are only entry points: the
// subcommand and the caller's arguments reach the helper unchanged.
func TestEntryPointsForwardArgv(t *testing.T) {
	root := repositoryRoot(t)
	helper := filepath.Join(t.TempDir(), "recipe-contract")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o755); err != nil {
		t.Fatalf("cannot write the recording helper: %v", err)
	}
	cases := []struct {
		name        string
		interpreter string
		script      string
		arguments   []string
		want        string
	}{
		{
			name:        "the bootstrap forwards its flags",
			interpreter: "bash",
			script:      filepath.Join(root, "images/install-pnpm.sh"),
			arguments:   []string{"--fixture-archive", "/tmp/pnpm.tgz"},
			want:        "pnpm-bootstrap --fixture-archive /tmp/pnpm.tgz\n",
		},
		{
			name:        "the certification forwards to the certified verb",
			interpreter: "sh",
			script:      filepath.Join(root, "images/pnpm/verify-pnpm.sh"),
			want:        "pnpm-verify --certify\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			environment := append([]string{"RECIPE_CONTRACT_HELPER=" + helper}, os.Environ()...)
			message, status := entryPoint(t, testCase.interpreter, testCase.script, environment, testCase.arguments...)
			if status != 0 {
				t.Fatalf("status is %d, want 0 (output %q)", status, message)
			}
			if message != testCase.want {
				t.Fatalf("helper was called with %q, want %q", message, testCase.want)
			}
		})
	}
}
