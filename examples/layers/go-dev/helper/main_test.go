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
		{"verify-installed without arguments", []string{"verify-installed"}, 2, "",
			"gopls-closure: usage: " + usageText},
		{"install without arguments", []string{"install"}, 2, "",
			"gopls-closure: usage: " + usageText},
		{"install with an unsupported architecture",
			[]string{"install", committed, "--expect-gopls", "v0.23.0", "--expect-go",
				"1.27.3", "--go-root", "/opt/go", "--arch", "386",
				"--bin-dir", "/tmp/bin", "--share-dir", "/tmp/share"},
			2, "", "gopls-closure: usage: " + usageText},
		{"install with a wrong gopls assertion",
			[]string{"install", committed, "--expect-gopls", "v0.24.0", "--expect-go",
				"1.27.3", "--go-root", "/opt/go", "--arch", "amd64",
				"--bin-dir", "/tmp/bin", "--share-dir", "/tmp/share"},
			1, "", "gopls-closure: asserted gopls version v0.24.0 != pinned v0.23.0\n"},
		{"install with a Go root that has no compiler",
			[]string{"install", committed, "--expect-gopls", "v0.23.0", "--expect-go",
				"1.27.3", "--go-root", filepath.Join(committed, "absent"), "--arch", "amd64",
				"--bin-dir", t.TempDir(), "--share-dir", t.TempDir()},
			1, "", "gopls-closure: --go-root must be an absolute Go root with bin/go: "},
		{"verify-installed with an unsupported architecture",
			[]string{"verify-installed", committed, "--expect-gopls", "v0.23.0",
				"--expect-go", "1.27.3", "--go-root", "/opt/go", "--arch", "386",
				"--binary", "/opt/go-tools/bin/gopls", "--share-dir", "/opt/go-tools/share/gopls"},
			2, "", "gopls-closure: usage: " + usageText},
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

// TestCheckRunsWithoutAToolchainOrEnvironment proves two things about `check` and
// stops there.
//
// It proves that the reader needs no inherited GO* policy and no usable toolchain:
// with an empty PATH, and with GOFLAGS/GOPROXY/GOTOOLCHAIN/GOOS/GOARCH set to
// values the image would never accept, it still prints the selected release. So a
// reader that read any of those variables, or that needed a `go` on PATH to
// succeed, fails here.
//
// It also observes that no `go` is *run*: the PATH holds one executable, a shim
// that records being invoked, and the record must stay empty. That catches a
// reader which exec's a toolchain and ignores the error, which a merely failing
// PATH would not.
//
// It does not claim more. The environment and PATH assertions cannot see a network
// request, and this module's static import register is not a veto either: it is one
// review-time list over the files we compile, and its header states what it cannot
// see (a socket made through `syscall`, `os.StartProcess`, cgo, `plugin`, a
// dependency's internals, anything in a child). The boundary that forbids a request
// being made or succeeding at build time is the recipe's `--network=none` on that
// RUN, which no host-side test can exercise.
func TestCheckRunsWithoutAToolchainOrEnvironment(t *testing.T) {
	binary := build(t)
	committed := committedData(t)
	check := func(t *testing.T, environment []string, wantShim bool) {
		t.Helper()
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
		if wantShim {
			return
		}
		if invoked, err := os.Stat(shimRecord(t, environment)); err == nil {
			t.Fatalf("check ran a `go` from PATH (%v bytes recorded); it must run no "+
				"toolchain at all", invoked.Size())
		}
	}
	t.Run("no toolchain on PATH", func(t *testing.T) {
		shim := goShim(t)
		check(t, []string{"PATH=" + shim}, false)
	})
	t.Run("hostile GO environment", func(t *testing.T) {
		shim := goShim(t)
		check(t, []string{"PATH=" + shim, "GOFLAGS=-mod=mod", "GOPROXY=direct",
			"GOTOOLCHAIN=auto", "GOOS=windows", "GOARCH=386", "GOENV=off"}, false)
	})
}

// goShim is a directory whose only entry is a `go` that records being run. It is
// what makes "the reader exec'd nothing" observable rather than assumed.
func goShim(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	record := filepath.Join(directory, "invoked")
	body := "#!/bin/sh\n: > '" + record + "'\n"
	if err := os.WriteFile(filepath.Join(directory, "go"), []byte(body), 0o755); err != nil {
		t.Fatalf("write the go shim: %v", err)
	}
	return directory
}

// shimRecord is the file the shim writes when it runs.
func shimRecord(t *testing.T, environment []string) string {
	t.Helper()
	for _, entry := range environment {
		if path, found := strings.CutPrefix(entry, "PATH="); found {
			return filepath.Join(path, "invoked")
		}
	}
	t.Fatal("the environment names no PATH")
	return ""
}

// installedTree is a directory layout that satisfies every rule except the one a
// case changes. Its `go` records being started, so "the refusal fired before the
// child" is an observation rather than a reading of the status.
type installedTree struct {
	root     string
	goRoot   string
	binary   string
	shareDir string
	record   string
	notice   string
	// noticeBytes is the committed notice file, so a case that edits or removes it
	// can be followed by a case that needs it back.
	noticeBytes []byte
}

func newInstalledTree(t *testing.T, committed string) installedTree {
	t.Helper()
	root := t.TempDir()
	goRoot := filepath.Join(root, "go")
	if err := os.MkdirAll(filepath.Join(goRoot, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	record := filepath.Join(root, "go-invoked")
	shim := "#!/bin/sh\n: > '" + record + "'\nprintf 'go1.27.3\\n'\n"
	if err := os.WriteFile(filepath.Join(goRoot, "bin", "go"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write the go shim: %v", err)
	}
	binDir := filepath.Join(root, "go-tools", "bin")
	shareDir := filepath.Join(root, "go-tools", "share", "gopls")
	for _, directory := range []string{binDir, shareDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	binary := filepath.Join(binDir, "gopls")
	if err := os.WriteFile(binary, []byte("\x7fELF installed binary\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	notice := filepath.Join(shareDir, "THIRD_PARTY_LICENSES")
	raw, err := os.ReadFile(filepath.Join(committed, "THIRD_PARTY_LICENSES"))
	if err != nil {
		t.Fatalf("read the committed notice file: %v", err)
	}
	if err := os.WriteFile(notice, raw, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return installedTree{root: root, goRoot: goRoot, binary: binary, shareDir: shareDir,
		record: record, notice: notice, noticeBytes: raw}
}

// args is the whole invocation for this tree, with the binary path replaced.
func (tree installedTree) args(committed, binary string) []string {
	return []string{"verify-installed", committed, "--expect-gopls", "v0.23.0",
		"--expect-go", "1.27.3", "--go-root", tree.goRoot, "--arch", "arm64",
		"--binary", binary, "--share-dir", tree.shareDir}
}

// restore puts the tree back between cases and reports a failure to do so: a case
// that left the fixture changed would make the next case's verdict meaningless. A
// retained `go` record is removed too, because each case asserts it is absent.
func (tree installedTree) restore(t *testing.T) {
	t.Helper()
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove %s: %v", path, err)
		}
	}
	remove(tree.record)
	if err := os.Chmod(tree.binary, 0o755); err != nil {
		t.Errorf("restore %s: %v", tree.binary, err)
	}
	if err := os.WriteFile(tree.notice, tree.noticeBytes, 0o644); err != nil {
		t.Errorf("restore %s: %v", tree.notice, err)
	}
	if err := os.Chmod(tree.notice, 0o644); err != nil {
		t.Errorf("restore %s: %v", tree.notice, err)
	}
}

// TestVerifyInstalledRefusesBeforeTheGoChild covers the installed-state rules that
// fire before the build-info read, each with the committed closure loaded. Every
// case must leave the `go` shim unstarted: a verifier that ran the child and then
// refused for another reason would still be a wrong answer.
func TestVerifyInstalledRefusesBeforeTheGoChild(t *testing.T) {
	binary := build(t)
	committed := committedData(t)
	tree := newInstalledTree(t, committed)
	base := filepath.Join(tree.root, "elsewhere")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, testCase := range []struct {
		name     string
		args     []string
		status   int
		fragment string
		apply    func(t *testing.T)
	}{
		{"a relative binary path", tree.args(committed, "gopls"), 1, "--binary must be absolute", nil},
		{"a binary with another name", tree.args(committed, filepath.Join(tree.root, "not-gopls")), 1,
			"--binary must name the installed tool gopls, not 'not-gopls'", nil},
		{"a missing requested binary", tree.args(committed, filepath.Join(base, "gopls")), 1,
			"installed: missing " + filepath.Join(base, "gopls"), nil},
		{"a non-executable installed binary", tree.args(committed, tree.binary), 1,
			"has mode 644, want 755", func(t *testing.T) {
				if err := os.Chmod(tree.binary, 0o644); err != nil {
					t.Fatal(err)
				}
			}},
		{"a setuid installed binary", tree.args(committed, tree.binary), 1, "has mode 4755, want 755",
			func(t *testing.T) {
				if err := os.Chmod(tree.binary, 0o755|os.ModeSetuid); err != nil {
					t.Fatal(err)
				}
			}},
		{"a group-writable notice file", tree.args(committed, tree.binary), 1, "has mode 664, want 644",
			func(t *testing.T) {
				if err := os.Chmod(tree.notice, 0o664); err != nil {
					t.Fatal(err)
				}
			}},
		{"edited notice bytes", tree.args(committed, tree.binary), 1,
			"does not match the pinned license digest", func(t *testing.T) {
				if err := os.WriteFile(tree.notice, []byte("# edited\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}},
		{"a missing notice file", tree.args(committed, tree.binary), 1, "installed: missing " + tree.notice,
			func(t *testing.T) {
				if err := os.Remove(tree.notice); err != nil {
					t.Fatal(err)
				}
			}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tree.restore(t)
			t.Cleanup(func() { tree.restore(t) })
			if testCase.apply != nil {
				testCase.apply(t)
			}
			stdout, stderr, status := runBinary(t, binary, testCase.args...)
			if status != testCase.status {
				t.Fatalf("status %d, want %d (stderr %q)", status, testCase.status, stderr)
			}
			if stdout != "" {
				t.Fatalf("a refusal wrote stdout %q", stdout)
			}
			if !strings.Contains(stderr, testCase.fragment) {
				t.Fatalf("stderr %q does not name %q", stderr, testCase.fragment)
			}
			if _, err := os.Stat(tree.record); err == nil {
				t.Fatalf("the verifier started a `go` child before refusing")
			}
		})
	}
}

// TestCheckUsesTheRequestedSpelling: the loader's half of the same rule. The five
// committed files are reached through the directory the caller named, in that
// spelling, so `a/link/../closure` is the kernel's `b/closure` and not a cleaned
// `a/closure`.
func TestCheckUsesTheRequestedSpelling(t *testing.T) {
	binary := build(t)
	committed := committedData(t)
	root := t.TempDir()
	a := filepath.Join(root, "a")
	elsewhere := filepath.Join(root, "b")
	for _, directory := range []string{a, filepath.Join(elsewhere, "sub")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join(elsewhere, "sub"), filepath.Join(a, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	copyClosure := func(directory string) {
		t.Helper()
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
		for _, name := range []string{"go.mod", "go.sum", "gopls.go.mod", "pin.json",
			"THIRD_PARTY_LICENSES"} {
			raw, err := os.ReadFile(filepath.Join(committed, name))
			if err != nil {
				t.Fatalf("read the committed %s: %v", name, err)
			}
			if err := os.WriteFile(filepath.Join(directory, name), raw, 0o644); err != nil {
				t.Fatalf("write %s: %v", filepath.Join(directory, name), err)
			}
		}
	}
	requested := a + "/link/../closure"
	// The kernel's target carries the closure: the requested spelling must be read.
	copyClosure(filepath.Join(elsewhere, "closure"))
	stdout, stderr, status := runBinary(t, binary, "check", requested)
	if status != 0 || stdout != "v0.23.0\n" {
		t.Fatalf("the kernel-resolved closure was not checked: status %d, %q, %q", status,
			stdout, stderr)
	}
	// Only the cleaned spelling carries it now. A loader that cleaned would print the
	// release; the shipped one must refuse, naming the spelling it was given.
	if err := os.RemoveAll(filepath.Join(elsewhere, "closure")); err != nil {
		t.Fatalf("remove the kernel-resolved closure: %v", err)
	}
	copyClosure(filepath.Join(a, "closure"))
	_, stderr, status = runBinary(t, binary, "check", requested)
	if status != 1 || !strings.Contains(stderr, "not a directory: "+requested) {
		t.Fatalf("the lexically cleaned closure was read: status %d, stderr %q", status, stderr)
	}
}

// TestVerifyInstalledUsesTheRequestedSpelling: the `..`-after-a-symlink case at the
// CLI seam. The unit tests call NewTarget and Verify directly; this one runs the
// binary, because the cleaning this row forbids lived in the argument handling.
//
// `b/link` links to `elsewhere/sub`, so `b/link/../gopls` is `elsewhere/gopls` to the
// kernel and `b/gopls` to a lexical clean. The two spellings name different files,
// and the row requires the kernel's.
func TestVerifyInstalledUsesTheRequestedSpelling(t *testing.T) {
	binary := build(t)
	committed := committedData(t)
	tree := newInstalledTree(t, committed)
	b := filepath.Join(tree.root, "b")
	elsewhere := filepath.Join(tree.root, "elsewhere")
	for _, directory := range []string{b, filepath.Join(elsewhere, "sub")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.Symlink(filepath.Join(elsewhere, "sub"), filepath.Join(b, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	requested := b + "/link/../gopls"
	writeCandidate := func(directory string) {
		path := filepath.Join(directory, "gopls")
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", path, err)
		}
	}
	// The kernel's target carries the candidate, so the path rule must pass and the
	// pinned go must be reached (its shim records being started).
	writeCandidate(elsewhere)
	_, stderr, status := runBinary(t, binary, tree.args(committed, requested)...)
	if status == 1 && strings.Contains(stderr, "installed: missing "+requested) {
		t.Fatalf("the requested spelling was not found although the kernel resolves it: %v",
			stderr)
	}
	if _, err := os.Stat(tree.record); err != nil {
		t.Fatalf("the verifier did not reach the pinned go for the kernel-resolved path: "+
			"status %d, stderr %q", status, stderr)
	}
	// Only the cleaned spelling carries a file now. A reader that cleaned would verify
	// `b/gopls`; the shipped one must refuse, naming the spelling it was given.
	if err := os.Remove(filepath.Join(elsewhere, "gopls")); err != nil {
		t.Fatalf("remove the kernel-resolved candidate: %v", err)
	}
	writeCandidate(b)
	_, stderr, status = runBinary(t, binary, tree.args(committed, requested)...)
	if status != 1 || !strings.Contains(stderr, "installed: missing "+requested) {
		t.Fatalf("the lexically cleaned spelling was verified: status %d, stderr %q", status,
			stderr)
	}
}

// TestVerifyInstalledRejectsAMissingGoRoot: the `--go-root` rule is the last path
// check, and it is a refusal rather than a fallback to whichever `go` answers.
func TestVerifyInstalledRejectsAMissingGoRoot(t *testing.T) {
	binary := build(t)
	committed := committedData(t)
	tree := newInstalledTree(t, committed)
	args := tree.args(committed, tree.binary)
	for index, argument := range args {
		if argument == "--go-root" {
			args[index+1] = filepath.Join(tree.root, "absent")
		}
	}
	_, stderr, status := runBinary(t, binary, args...)
	if status != 1 || !strings.Contains(stderr,
		"--go-root must be an absolute Go root with bin/go") {
		t.Fatalf("status %d, stderr %q", status, stderr)
	}
	if _, err := os.Stat(tree.record); err == nil {
		t.Fatalf("the verifier started a `go` child from an unproved root")
	}
}
