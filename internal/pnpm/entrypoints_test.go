package pnpm

import (
	"bytes"
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

// entryResult is one entry-point run, with the streams kept apart: a diagnostic
// that belongs on stderr must not pass because it was printed to stdout.
type entryResult struct {
	stdout string
	stderr string
	status int
}

// entryPoint drives one shell entry point with a named interpreter.
func entryPoint(t *testing.T, interpreter, script string, environment []string, arguments ...string) entryResult {
	t.Helper()
	command := exec.Command(interpreter, append([]string{script}, arguments...)...) //nolint:gosec // the reviewed entry point.
	command.Env = environment
	command.Dir = repositoryRoot(t)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
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
	return entryResult{stdout: stdout.String(), stderr: stderr.String(), status: status}
}

// entryPointCase is one shell file and the interpreter the recipe invokes it with.
type entryPointCase struct {
	name        string
	interpreter string
	script      string
	// diagnostic names the file's own refusal prefix.
	diagnostic string
}

func entryPoints(t *testing.T) []entryPointCase {
	root := repositoryRoot(t)
	return []entryPointCase{
		{
			name:        "the bootstrap entry point",
			interpreter: "bash",
			script:      filepath.Join(root, "images/install-pnpm.sh"),
			diagnostic:  "install-pnpm.sh: ",
		},
		{
			name:        "the certification entry point",
			interpreter: "sh",
			script:      filepath.Join(root, "images/pnpm/verify-pnpm.sh"),
			diagnostic:  "verify-pnpm.sh: ",
		},
	}
}

// TestEntryPointsNeedTheHelper is the contract the image mount depends on: a
// missing helper is a named refusal on stderr, not a shell error, not a silent
// success, and not a line on stdout.
func TestEntryPointsNeedTheHelper(t *testing.T) {
	for _, testCase := range entryPoints(t) {
		t.Run(testCase.name, func(t *testing.T) {
			absent := filepath.Join(t.TempDir(), "absent", "recipe-contract")
			environment := []string{"RECIPE_CONTRACT_HELPER=" + absent}
			result := entryPoint(t, testCase.interpreter, testCase.script, environment)
			if result.status != 1 {
				t.Fatalf("status is %d, want 1 (stderr %q)", result.status, result.stderr)
			}
			if result.stdout != "" {
				t.Fatalf("the refusal reached stdout: %q", result.stdout)
			}
			if !strings.Contains(result.stderr, testCase.diagnostic+"no recipe-contract helper at "+absent) {
				t.Fatalf("stderr %q does not carry the named refusal", result.stderr)
			}
		})
	}
}

// TestEntryPointsHonourTheContractDirectory is the resolution order the recipes
// rely on: AGENT_VM_CONTRACT_DIR, which images/standard/verify-standard.sh has
// always exported, still names an alternate composed directory.
func TestEntryPointsHonourTheContractDirectory(t *testing.T) {
	for _, testCase := range entryPoints(t) {
		t.Run(testCase.name, func(t *testing.T) {
			contract := t.TempDir()
			helper := filepath.Join(contract, "recipe-contract")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\"\n"), 0o755); err != nil {
				t.Fatalf("cannot write the recording helper: %v", err)
			}
			environment := []string{"AGENT_VM_CONTRACT_DIR=" + contract}
			result := entryPoint(t, testCase.interpreter, testCase.script, environment)
			if result.status != 0 {
				t.Fatalf("the entry point ignored AGENT_VM_CONTRACT_DIR: %q (status %d)", result.stderr, result.status)
			}
			if !strings.HasPrefix(result.stdout, "pnpm-") {
				t.Fatalf("helper was called with %q", result.stdout)
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
			environment := []string{"RECIPE_CONTRACT_HELPER=" + helper}
			result := entryPoint(t, testCase.interpreter, testCase.script, environment, testCase.arguments...)
			if result.status != 0 {
				t.Fatalf("status is %d, want 0 (stderr %q)", result.status, result.stderr)
			}
			if result.stdout != testCase.want {
				t.Fatalf("helper was called with %q, want %q", result.stdout, testCase.want)
			}
			if result.stderr != "" {
				t.Fatalf("a successful run wrote %q to stderr", result.stderr)
			}
		})
	}
}
