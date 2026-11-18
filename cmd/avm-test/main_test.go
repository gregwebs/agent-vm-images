package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/claude"
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

// requireStatus runs one invocation and holds it to its status and to the text that
// names the reason.
func requireStatus(t *testing.T, status, want int, needle, stdout, stderr string) {
	t.Helper()
	if status != want {
		t.Fatalf("status %d, want %d\nstdout:\n%s\nstderr:\n%s", status, want, stdout, stderr)
	}
	if !strings.Contains(stdout+stderr, needle) {
		t.Fatalf("the output never names %q\nstdout:\n%s\nstderr:\n%s", needle, stdout, stderr)
	}
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
			argv:   []string{"write-fixture", "--tool", "codex", "--dir", "/tmp/avm-test"},
			status: 2,
			needle: `"codex" is not a fixture this tool builds: agy, claude`,
		},
		{
			// The wrapper chdirs to the checkout before exec, so a relative --dir
			// would name a different tree here than it did for the caller.
			name:   "a relative directory",
			argv:   []string{"write-fixture", "--tool", "agy", "--dir", "fixture"},
			status: 2,
			needle: "--dir must be absolute",
		},
		{
			// A flag parsed for another tool is malformed for this one, rather than
			// silently ignored.
			name: "a flag the tool does not honour",
			argv: []string{"write-fixture", "--tool", "agy", "--dir", "/tmp/avm-test",
				"--signer", "/tmp/signer"},
			status: 2,
			needle: "--signer is not a flag --tool agy honours",
		},
		{name: "keygen without a directory", argv: []string{"keygen"}, status: 2, needle: "OUT"},
		{name: "elf without a machine", argv: []string{"elf", "/tmp/avm-test/blob"}, status: 2,
			needle: "machine"},
		{
			name: "closure without a signer",
			argv: []string{"closure", "/tmp/avm-test/closure", "--version", "1.2.3",
				"--amd64", "/tmp/a", "--arm64", "/tmp/b"},
			status: 2,
			needle: "signer",
		},
		{
			name:   "two-primaries with one key",
			argv:   []string{"two-primaries", "/tmp/avm-test/keys.asc", "/tmp/one.asc"},
			status: 2,
			needle: "SECOND",
		},
		{
			// Every path these verbs take is one the wrapper's `cd` would reinterpret,
			// not just write-fixture's --dir.
			name:   "a relative blob",
			argv:   []string{"elf", "blob", "--machine", "62"},
			status: 2,
			needle: "OUT must be absolute",
		},
		{
			name:   "a relative signer directory",
			argv:   []string{"keygen", "signer"},
			status: 2,
			needle: "OUT must be absolute",
		},
		{
			name: "a relative executable in a closure",
			argv: []string{"closure", "/tmp/avm-test/closure", "--signer", "/tmp/signer",
				"--version", "1.2.3", "--amd64", "amd64", "--arm64", "/tmp/b"},
			status: 2,
			needle: "--amd64 must be absolute",
		},
		{
			name:   "a relative key file",
			argv:   []string{"two-primaries", "/tmp/keys.asc", "/tmp/one.asc", "two.asc"},
			status: 2,
			needle: "SECOND must be absolute",
		},
		{
			name: "a relative signer for a fixture",
			argv: []string{"write-fixture", "--tool", "claude", "--dir", "/tmp/avm-test",
				"--signer", "signer"},
			status: 2,
			needle: "--signer must be absolute",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := capture(t, testCase.argv...)
			requireStatus(t, status, testCase.status, testCase.needle, stdout, stderr)
		})
	}
}

// TestTheClosureBuilderVerbs holds each fixture builder to its status, because the
// three shell suites call them instead of the Python files they replaced: a verb that
// answered 0 for a failed build, or 2 for one, would make those suites branch wrongly.
func TestTheClosureBuilderVerbs(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg is required to build a signer")
	}
	work := t.TempDir()
	signer := filepath.Join(work, "signer")
	amd64 := filepath.Join(work, "amd64")
	arm64 := filepath.Join(work, "arm64")

	t.Run("keygen prints the fingerprint", func(t *testing.T) {
		status, stdout, stderr := capture(t, "keygen", signer)
		requireStatus(t, status, 0, "", stdout, stderr)
		if len(strings.TrimSpace(stdout)) != 40 {
			t.Fatalf("keygen printed %q, want one 40-character fingerprint", stdout)
		}
	})
	for _, blob := range []struct {
		path    string
		machine string
	}{{amd64, "62"}, {arm64, "183"}} {
		if status, stdout, stderr := capture(t, "elf", blob.path, "--machine",
			blob.machine); status != 0 {
			t.Fatalf("elf exited %d\nstdout:\n%s\nstderr:\n%s", status, stdout, stderr)
		}
	}

	t.Run("closure writes a consistent closure", func(t *testing.T) {
		out := filepath.Join(work, "closure")
		status, stdout, stderr := capture(t, "closure", out, "--signer", signer,
			"--version", claude.Version, "--amd64", amd64, "--arm64", arm64)
		requireStatus(t, status, 0, "", stdout, stderr)
		for _, name := range []string{"pin.json", "evidence/manifest.json",
			"evidence/manifest.json.sig", "claude-code-release-key.asc"} {
			if _, err := os.Stat(filepath.Join(out, filepath.FromSlash(name))); err != nil {
				t.Fatalf("the closure is missing %s: %v", name, err)
			}
		}
	})

	t.Run("two-primaries writes both keys", func(t *testing.T) {
		other := filepath.Join(work, "other")
		if status, _, stderr := capture(t, "keygen", other); status != 0 {
			t.Fatalf("the second keygen exited %d: %s", status, stderr)
		}
		combined := filepath.Join(work, "combined.asc")
		status, stdout, stderr := capture(t, "two-primaries", combined,
			filepath.Join(signer, "key.asc"), filepath.Join(other, "key.asc"))
		requireStatus(t, status, 0, "", stdout, stderr)
		raw, err := os.ReadFile(combined)
		if err != nil {
			t.Fatalf("cannot read the combined key file: %v", err)
		}
		if count := strings.Count(string(raw), "BEGIN PGP PUBLIC KEY BLOCK"); count != 2 {
			t.Fatalf("the combined key file holds %d keys, want 2", count)
		}
	})

	t.Run("write-fixture claude writes the bumper's layout", func(t *testing.T) {
		dir := filepath.Join(work, "fixture")
		status, stdout, stderr := capture(t, "write-fixture", "--tool", "claude", "--dir",
			dir, "--version", claude.Version)
		// Both families say what they wrote the same way, because nothing parses it.
		requireStatus(t, status, 0,
			"avm-test: claude "+claude.Version+" written to "+dir, stdout, stderr)
		for _, name := range []string{"exec/linux-x64", "exec/linux-arm64",
			"upstream/downloads.claude.ai/claude-code-releases/" + claude.Version + "/manifest.json"} {
			if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
				t.Fatalf("the fixture is missing %s: %v", name, err)
			}
		}
	})

	// A file where a directory would have to be, so the write fails whatever the
	// process can create.
	blocker := filepath.Join(work, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("cannot write the blocker file: %v", err)
	}
	failures := []struct {
		name string
		argv []string
	}{
		{"a blob that cannot be written", []string{"elf",
			filepath.Join(blocker, "blob"), "--machine", "62"}},
		{"a key that cannot be generated", []string{"keygen",
			filepath.Join("/nonexistent-directory", "signer")}},
		{"a closure whose signer directory does not exist", []string{"closure",
			filepath.Join(work, "orphan"), "--signer", filepath.Join(work, "no-signer"),
			"--version", claude.Version, "--amd64", amd64, "--arm64", arm64}},
		{"a key file that does not exist", []string{"two-primaries",
			filepath.Join(work, "nothing.asc"), filepath.Join(work, "no.asc"),
			filepath.Join(work, "none.asc")}},
		{"a fixture whose signer cannot be created", []string{"write-fixture",
			"--tool", "claude", "--dir", filepath.Join(work, "broken-fixture"),
			"--version", claude.Version, "--signer", filepath.Join(blocker, "signer")}},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			status, stdout, stderr := capture(t, failure.argv...)
			requireStatus(t, status, 1, "avm-test: ", stdout, stderr)
		})
	}
}
