package main

// Status and stream contract, the environment rule, and the shipped binary over
// the committed data. Both builds of this module are made here: the shipped one
// is what the image mounts, and the fixture one is the control that proves these
// checks can fail (seams_test.go).

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// runBuilt runs a binary with exactly env (plus nothing inherited) and returns
// stdout, stderr and the status.
func runBuilt(t *testing.T, binary string, env []string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command(binary, args...)
	if env != nil {
		// The variables named are added to the host's environment: these rows are
		// about one variable's effect, not about running without a PATH.
		command.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	status := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			status = exit.ExitCode()
		} else {
			t.Fatalf("running %s: %v", binary, err)
		}
	}
	return stdout.String(), stderr.String(), status
}

// TestStatusMatrix is invariant 27: every invocation's status, stdout and stderr
// follows the source reader's table.
func TestStatusMatrix(t *testing.T) {
	binary := build(t, "")
	committed := committedPinDir(t)
	cases := []struct {
		label  string
		args   []string
		env    []string
		status int
		stdout string
		stderr string
	}{
		{"no verb", nil, nil, 1, "", "usage: " + usageText},
		{"unknown flag before a verb", []string{"--bogus"}, nil, 1, "",
			"usage: " + usageText},
		{"check without a directory", []string{"check"}, nil, 2, "", "usage: " + usageText},
		{"check with an extra positional",
			[]string{"check", committed, "extra"}, nil, 2, "", "usage: " + usageText},
		{"check with a bad flag", []string{"check", committed, "--bogus"}, nil, 2, "",
			"usage: " + usageText},
		{"verify-installed without --rustup-home",
			[]string{"verify-installed", committed, "--arch", "amd64", "--expect-channel",
				"1.98.1", "--cargo-home", "/tmp"}, nil, 2, "", "usage: " + usageText},
		{"verify-installed with --arch riscv64",
			[]string{"verify-installed", committed, "--arch", "riscv64", "--expect-channel",
				"1.98.1", "--rustup-home", "/tmp", "--cargo-home", "/tmp"}, nil, 2, "",
			"usage: " + usageText},
		{"install without --arch", []string{"install", committed, "--expect-channel", "1.98.1",
			"--rustup-home", "/tmp", "--cargo-home", "/tmp"}, nil, 2, "", "usage: " + usageText},
		{"install with --arch riscv64", []string{"install", committed, "--arch", "riscv64",
			"--expect-channel", "1.98.1", "--rustup-home", "/tmp", "--cargo-home", "/tmp"},
			nil, 2, "", "usage: " + usageText},
		{"install over an existing home", []string{"install", committed, "--arch", "amd64",
			"--expect-channel", "1.98.1", "--rustup-home", "/tmp", "--cargo-home",
			filepath.Join(t.TempDir(), "cargo")}, nil, 1, "",
			"rust_toolchain: refusing to install over an existing path: /tmp"},
		{"install with a missing work dir", []string{"install", committed, "--arch", "amd64",
			"--expect-channel", "1.98.1", "--rustup-home", filepath.Join(t.TempDir(), "r"),
			"--cargo-home", filepath.Join(t.TempDir(), "c"), "--work-dir",
			filepath.Join(t.TempDir(), "absent")}, nil, 1, "",
			"rust_toolchain: --work-dir must be an existing directory"},
		{"check over an empty directory", []string{"check", t.TempDir()}, nil, 1, "",
			"rust_toolchain: missing input: pin.json"},
		{"asserted channel", []string{"check", committed, "--expect-channel", "9.9.9"}, nil, 1,
			"", "rust_toolchain: pin channel 1.98.1 != asserted channel 9.9.9"},
		{"empty asserted channel", []string{"check", committed, "--expect-channel", ""}, nil, 1,
			"", "rust_toolchain: pin channel 1.98.1 != asserted channel "},
		{"the committed pin", []string{"check", committed}, nil, 0, "1.98.1\n", ""},
		{"an unset verb with the environment set", []string{"check", committed},
			[]string{"RUST_TOOLCHAIN=1.98.1"}, 0, "1.98.1\n", ""},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			stdout, stderr, status := runBuilt(t, binary, test.env, test.args...)
			if status != test.status {
				t.Errorf("status %d, want %d (stderr %q)", status, test.status, stderr)
			}
			if stdout != test.stdout {
				t.Errorf("stdout %q, want %q", stdout, test.stdout)
			}
			if !strings.Contains(stderr, test.stderr) {
				t.Errorf("stderr %q, want it to contain %q", stderr, test.stderr)
			}
		})
	}
	t.Run("env does not supply a flag", func(t *testing.T) {
		// `--expect-channel` is required by verify-installed; an ambient
		// RUST_TOOLCHAIN must not stand in for it.
		_, stderr, status := runBuilt(t, binary, []string{"RUST_TOOLCHAIN=1.98.1"},
			"verify-installed", committed, "--arch", "amd64", "--rustup-home", "/tmp",
			"--cargo-home", "/tmp")
		if status != 2 {
			t.Fatalf("status %d, want 2: %s", status, stderr)
		}
	})
}

// TestNoArgumentReadsTheEnvironment is invariant 29: no field of the grammar
// carries an `env` tag, and the parser is configured not to read one.
func TestNoArgumentReadsTheEnvironment(t *testing.T) {
	types := []reflect.Type{reflect.TypeOf(arguments{}),
		reflect.TypeOf(checkCommand{}), reflect.TypeOf(verifyInstalledCommand{}),
		reflect.TypeOf(kacheProbeCommand{}), reflect.TypeOf(fixtureArguments{})}
	for _, structure := range types {
		for index := 0; index < structure.NumField(); index++ {
			field := structure.Field(index)
			if tag, present := field.Tag.Lookup("arg"); present && strings.Contains(tag, "env:") {
				t.Errorf("%s.%s carries an env tag: %s", structure.Name(), field.Name, tag)
			}
		}
	}
	// The grammar reading the environment is the other half: a verb that needs a
	// flag must still need it with the matching variable set.
	t.Setenv("EXPECT_CHANNEL", "1.98.1")
	binary := build(t, "")
	_, stderr, status := runBuilt(t, binary, nil, "verify-installed", committedPinDir(t),
		"--arch", "amd64", "--rustup-home", "/tmp", "--cargo-home", "/tmp")
	if status != 2 || !strings.Contains(stderr, "usage: ") {
		t.Fatalf("status %d: %s", status, stderr)
	}
}

// TestCheckCommittedData is the shipped binary over the committed data: status 0,
// the channel on stdout, nothing on stderr and nothing left in TMPDIR.
func TestCheckCommittedData(t *testing.T) {
	binary := build(t, "")
	scratch := t.TempDir()
	stdout, stderr, status := runBuilt(t, binary, []string{"TMPDIR=" + scratch},
		"check", committedPinDir(t))
	if status != 0 || stdout != "1.98.1\n" || stderr != "" {
		t.Fatalf("status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("TMPDIR was not left empty: %v", names)
	}
}

// TestCheckRefusesWithNoScratch is row 5's other half: a refusal also removes the
// keyring, so a failed build leaves no `rust-verify-*` directory.
func TestCheckRefusesWithNoScratch(t *testing.T) {
	binary := build(t, "")
	scratch := t.TempDir()
	directory := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(committedPinDir(t), "pin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "pin.json"),
		bytes.Replace(raw, []byte(`"schema": 1`), []byte(`"schema": 2`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, status := runBuilt(t, binary, []string{"TMPDIR=" + scratch},
		"check", directory)
	if status != 1 || stdout != "" || !strings.Contains(stderr, "unsupported schema") {
		t.Fatalf("status %d, stdout %q, stderr %q", status, stdout, stderr)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refusal left scratch behind: %v", entries)
	}
}

// committedPinDir is the reviewed pin directory. `go test` runs this package from
// its own directory, so the path is stable however the caller started it.
func committedPinDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.Abs("../data/rust-toolchain")
	if err != nil {
		t.Fatal(err)
	}
	return directory
}
