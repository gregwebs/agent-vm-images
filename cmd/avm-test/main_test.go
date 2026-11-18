package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// capture runs the command in-process and returns its status and its two streams.
func capture(t *testing.T, argv ...string) (int, string, string) {
	t.Helper()
	// The command writes to the process streams, which is what makes its status
	// meaningful; a test swaps them for pipes rather than threading a writer through
	// the flags.
	originalOut, originalErr := os.Stdout, os.Stderr
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("cannot create a pipe: %v", err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("cannot create a pipe: %v", err)
	}
	os.Stdout, os.Stderr = outWriter, errWriter
	status := run(argv)
	os.Stdout, os.Stderr = originalOut, originalErr
	if err := outWriter.Close(); err != nil {
		t.Fatalf("cannot close the stdout pipe: %v", err)
	}
	if err := errWriter.Close(); err != nil {
		t.Fatalf("cannot close the stderr pipe: %v", err)
	}
	stdout, err := io.ReadAll(outReader)
	if err != nil {
		t.Fatalf("cannot read stdout: %v", err)
	}
	stderr, err := io.ReadAll(errReader)
	if err != nil {
		t.Fatalf("cannot read stderr: %v", err)
	}
	if err := outReader.Close(); err != nil {
		t.Fatalf("cannot close the stdout pipe: %v", err)
	}
	if err := errReader.Close(); err != nil {
		t.Fatalf("cannot close the stderr pipe: %v", err)
	}
	return status, string(stdout), string(stderr)
}

// TestTheStatusIsTheContract: 0 for a run that wrote, 2 for an invocation this tool
// cannot honour, 1 only for a run that failed. A caller branching on the status must
// read a typo as a typo, and the writers say the same (CONTRIBUTING.md's statuses).
func TestTheStatusIsTheContract(t *testing.T) {
	cases := []struct {
		name   string
		argv   []string
		status int
		needle string
	}{
		{name: "the help text", argv: []string{"--help"}, status: 0, needle: "write-fixture"},
		{name: "no arguments", argv: nil, status: 2, needle: "write-fixture"},
		{name: "an unknown subcommand", argv: []string{"bogus"}, status: 2, needle: "invalid subcommand: bogus"},
		{name: "no tool", argv: []string{"write-fixture", "--dir", "/tmp/avm-test"}, status: 2, needle: "NAME"},
		{
			name:   "a tool this tool does not build",
			argv:   []string{"write-fixture", "--tool", "claude", "--dir", "/tmp/avm-test"},
			status: 2,
			needle: `"claude" is not a fixture this tool builds: agy`,
		},
		{
			// The wrapper chdirs to the checkout before exec, so a relative --dir
			// would name a different tree here than it did for the caller.
			name:   "a relative directory",
			argv:   []string{"write-fixture", "--tool", "agy", "--dir", "fixture"},
			status: 2,
			needle: "--dir must be absolute",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := capture(t, testCase.argv...)
			if status != testCase.status {
				t.Fatalf("status %d, want %d\nstdout:\n%s\nstderr:\n%s", status, testCase.status, stdout, stderr)
			}
			if !strings.Contains(stdout+stderr, testCase.needle) {
				t.Fatalf("the output never names %q\nstdout:\n%s\nstderr:\n%s", testCase.needle, stdout, stderr)
			}
		})
	}
}
