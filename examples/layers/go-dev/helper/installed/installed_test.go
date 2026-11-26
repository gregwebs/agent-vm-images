package installed

// The witnesses for the installed-state rules. Each test names one rule; deleting
// the rule makes that test fail. The runner is injected, so the assertions are about
// what the verifier *asked to run* (program, argv, environment, directory) rather
// than about a status it reported.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/buildinfo"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
)

const (
	testRelease = "v0.0.1"
	testGo      = "1.27.3"
	depPath     = "example.test/dep"
)

func h1Of(label string) closure.H1 {
	sum := sha256.Sum256([]byte(label))
	return closure.H1("h1:" + base64.StdEncoding.EncodeToString(sum[:]))
}

// loaded is the closure data the verifier needs, built directly: these tests are
// about the installed-state rules, not about loading the committed five files.
func loaded(t *testing.T, noticeDigest closure.SHA256) *closure.Data {
	t.Helper()
	return &closure.Data{Pin: closure.Pin{
		Gopls: testRelease,
		Go:    testGo,
		Files: map[closure.Filename]closure.SHA256{closure.FileLicenses: noticeDigest},
		Modules: []closure.PinnedModule{
			{ID: closure.ModuleID{Path: depPath, Version: "v1.0.0"}, H1: h1Of("dep")},
			{ID: closure.ModuleID{Path: closure.Gopls, Version: testRelease},
				H1: h1Of("root")},
		},
	}}
}

// buildInfoDocument is a coherent document for one target. It exists so the
// installed-state tests reach their rule instead of a shape refusal; the shape and
// comparison rules have their own table in package buildinfo.
func buildInfoDocument(t *testing.T, arch gotool.Architecture) []byte {
	t.Helper()
	settings := []map[string]string{}
	expected := buildinfo.ExpectedSettings(arch)
	for _, key := range sortedKeys(expected) {
		settings = append(settings, map[string]string{"Key": key, "Value": expected[key]})
	}
	document := map[string]any{
		"GoVersion": "go" + testGo,
		"Path":      closure.Gopls,
		"Main": map[string]string{"Path": closure.Gopls, "Version": testRelease,
			"Sum": string(h1Of("root"))},
		"Deps": []map[string]string{{"Path": depPath, "Version": "v1.0.0",
			"Sum": string(h1Of("dep"))}},
		"Settings": settings,
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func sortedKeys(entries map[string]string) []string {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// installedState is a directory tree that satisfies every rule.
type installedState struct {
	root   string
	target gotool.Target
	data   *closure.Data
	notice []byte
}

func newInstalledState(t *testing.T, arch gotool.Architecture) installedState {
	t.Helper()
	root := t.TempDir()
	goRoot := filepath.Join(root, "go")
	toolDir := filepath.Join(goRoot, "bin")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	binDir := filepath.Join(root, "go-tools", "bin")
	shareDir := filepath.Join(root, "go-tools", "share", "gopls")
	for _, directory := range []string{binDir, shareDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	binary := filepath.Join(binDir, "gopls")
	if err := os.WriteFile(binary, []byte("\x7fELF the reviewed binary\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	notice := []byte("# Third-party license files\n")
	noticePath := filepath.Join(shareDir, string(closure.FileLicenses))
	if err := os.WriteFile(noticePath, notice, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(noticePath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	digest := sha256.Sum256(notice)
	target := gotool.Target{GoRoot: goRoot, ShareDir: shareDir, Arch: arch, Binary: binary}
	return installedState{root: root, target: target,
		data:   loaded(t, closure.SHA256(hex.EncodeToString(digest[:]))),
		notice: notice}
}

// recordingRunner answers the three closed commands from one document and records
// exactly what it was asked to run.
type recordingRunner struct {
	target    gotool.Target
	document  []byte
	goroot    string
	goVersion string
	status    map[string]int
	stderr    string
	children  []gotool.Child
	missing   []string
}

func (r *recordingRunner) Run(child gotool.Child) (gotool.Result, error) {
	r.children = append(r.children, child)
	if _, err := os.Stat(child.Dir); err != nil {
		r.missing = append(r.missing, child.Dir)
	}
	command := strings.Join(child.Argv, " ")
	goroot := r.goroot
	if goroot == "" {
		goroot = r.target.GoRoot
	}
	version := r.goVersion
	if version == "" {
		version = "go" + testGo
	}
	if status := r.status[command]; status != 0 {
		return gotool.Result{Status: status, Stderr: r.stderr}, nil
	}
	switch command {
	case "env GOVERSION":
		return gotool.Result{Stdout: version + "\n"}, nil
	case "env GOROOT":
		return gotool.Result{Stdout: goroot + "\n"}, nil
	}
	if strings.HasPrefix(command, "version -m -json ") {
		return gotool.Result{Stdout: string(r.document)}, nil
	}
	return gotool.Result{Status: 1, Stderr: "unexpected command " + command}, nil
}

// TestTargetRulesRefuse: the CLI's two path rules and their positive control.
func TestTargetRulesRefuse(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	for _, testCase := range []struct {
		name     string
		goRoot   string
		binary   string
		fragment string
	}{
		{"a relative Go root", "go", state.target.Binary,
			"--go-root must be an absolute Go root with bin/go: go"},
		{"a Go root without bin/go", filepath.Join(state.root, "absent"), state.target.Binary,
			"--go-root must be an absolute Go root with bin/go: "},
		{"a relative binary", state.target.GoRoot, "gopls", "--binary must be absolute: gopls"},
		{"a binary with another name", state.target.GoRoot,
			filepath.Join(filepath.Dir(state.target.Binary), "not-gopls"),
			"--binary must name the installed tool gopls, not 'not-gopls'"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewTarget(testCase.goRoot, testCase.binary, state.target.ShareDir,
				gotool.ARM64)
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
	target, err := NewTarget(state.target.GoRoot, state.target.Binary, state.target.ShareDir,
		gotool.ARM64)
	if err != nil {
		t.Fatalf("the reviewed paths were refused: %v", err)
	}
	if target != state.target {
		t.Fatalf("target %+v, want %+v", target, state.target)
	}
}

// TestBinaryRulesRefuse: present, not a symlink, exactly 0755.
func TestBinaryRulesRefuse(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	runner := &recordingRunner{target: state.target, document: buildInfoDocument(t,
		gotool.ARM64)}
	if _, err := Verify(state.data, state.target, runner); err != nil {
		t.Fatalf("the positive control was refused: %v", err)
	}
	if len(runner.children) != 3 {
		t.Fatalf("%d children, want 3", len(runner.children))
	}
	binary := state.target.Binary
	for _, testCase := range []struct {
		name     string
		apply    func(t *testing.T)
		fragment string
	}{
		{"a missing binary", func(t *testing.T) {
			if err := os.Remove(binary); err != nil {
				t.Fatal(err)
			}
		}, "installed: missing " + binary},
		{"a symlinked binary", func(t *testing.T) {
			target := filepath.Join(state.root, "elsewhere")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			copied := filepath.Join(target, "gopls")
			if err := os.WriteFile(copied, []byte("binary"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(binary); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copied, binary); err != nil {
				t.Fatal(err)
			}
		}, "installed: missing " + binary},
		{"a directory where the binary belongs", func(t *testing.T) {
			if err := os.Remove(binary); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(binary, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "installed: missing " + binary},
		{"a non-executable mode", func(t *testing.T) {
			if err := os.Chmod(binary, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + binary + " has mode 644, want 755"},
		{"a group-writable mode", func(t *testing.T) {
			if err := os.Chmod(binary, 0o775); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + binary + " has mode 775, want 755"},
		{"a setuid mode", func(t *testing.T) {
			if err := os.Chmod(binary, 0o755|os.ModeSetuid); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + binary + " has mode 4755, want 755"},
		{"a setgid mode", func(t *testing.T) {
			// The kernel keeps a setgid bit only when the file's group is one the
			// process belongs to, so the file is moved to this process's own group
			// first. A host that still refuses the bit cannot express this state, and
			// that is a red test rather than a silent acceptance.
			if err := os.Chown(binary, -1, os.Getgid()); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(binary, 0o755|os.ModeSetgid); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Lstat(binary); err != nil ||
				info.Mode()&os.ModeSetgid == 0 {
				t.Fatalf("this host will not keep a setgid bit: %v %v", info.Mode(), err)
			}
		}, "installed: " + binary + " has mode 2755, want 755"},
		{"a sticky mode", func(t *testing.T) {
			if err := os.Chmod(binary, 0o755|os.ModeSticky); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + binary + " has mode 1755, want 755"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			restore := replace(t, binary)
			defer restore()
			testCase.apply(t)
			_, err := Verify(state.data, state.target, &recordingRunner{target: state.target,
				document: buildInfoDocument(t, gotool.ARM64)})
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
}

// replace snapshots one path's bytes, mode and kind so a case can destroy it.
func replace(t *testing.T, path string) func() {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	mode := info.Mode()
	return func() {
		if err := os.RemoveAll(path); err != nil {
			t.Fatalf("remove %s: %v", path, err)
		}
		if mode.IsDir() {
			if err := os.Mkdir(path, mode.Perm()); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
			return
		}
		if err := os.WriteFile(path, raw, mode.Perm()); err != nil {
			t.Fatalf("restore %s: %v", path, err)
		}
		if err := os.Chmod(path, mode.Perm()); err != nil {
			t.Fatalf("chmod %s: %v", path, err)
		}
	}
}

// TestSiblingIsNotSubstituted: the verifier inspects exactly the path it was given.
// A correct sibling in the installation directory is not the requested file.
func TestSiblingIsNotSubstituted(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	elsewhere := filepath.Join(state.root, "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	requested := filepath.Join(elsewhere, "gopls")
	target := state.target
	target.Binary = requested
	runner := &recordingRunner{target: target, document: buildInfoDocument(t, gotool.ARM64)}
	_, err := Verify(state.data, target, runner)
	if err == nil || !strings.Contains(err.Error(), "installed: missing "+requested) {
		t.Fatalf("a sibling binary was substituted for the requested path: %v", err)
	}
	if len(runner.children) != 0 {
		t.Fatalf("the verifier ran %v before proving the requested path exists",
			runner.children)
	}
	// The positive control for this fixture: the installation's own file at its own
	// path verifies, so the refusal above is the path rule and not a broken fixture.
	if _, err := Verify(state.data, state.target, &recordingRunner{target: state.target,
		document: buildInfoDocument(t, gotool.ARM64)}); err != nil {
		t.Fatalf("the sibling's own path was refused: %v", err)
	}
}

// TestNoticeRulesRefuse: present, exactly 0644, and the pinned digest.
func TestNoticeRulesRefuse(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	notice := filepath.Join(state.target.ShareDir, string(closure.FileLicenses))
	for _, testCase := range []struct {
		name     string
		apply    func(t *testing.T)
		fragment string
	}{
		{"a missing notice file", func(t *testing.T) {
			if err := os.Remove(notice); err != nil {
				t.Fatal(err)
			}
		}, "installed: missing " + notice},
		{"a symlinked notice file", func(t *testing.T) {
			copied := filepath.Join(state.root, "committed-notice")
			if err := os.WriteFile(copied, state.notice, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(notice); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(copied, notice); err != nil {
				t.Fatal(err)
			}
		}, "installed: missing " + notice},
		{"a group-writable notice mode", func(t *testing.T) {
			if err := os.Chmod(notice, 0o664); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + notice + " has mode 664, want 644"},
		{"a non-readable notice mode", func(t *testing.T) {
			if err := os.Chmod(notice, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + notice + " has mode 600, want 644"},
		{"edited notice bytes", func(t *testing.T) {
			if err := os.WriteFile(notice, append(state.notice, "\n# extra\n"...), 0o644); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + notice + " does not match the pinned license digest"},
		{"a setuid notice mode", func(t *testing.T) {
			if err := os.Chmod(notice, 0o644|os.ModeSetuid); err != nil {
				t.Fatal(err)
			}
		}, "installed: " + notice + " has mode 4644, want 644"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			restore := replace(t, notice)
			defer restore()
			testCase.apply(t)
			_, err := Verify(state.data, state.target, &recordingRunner{target: state.target,
				document: buildInfoDocument(t, gotool.ARM64)})
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
	// The digest is the pin's, not the file's: a notice whose bytes are unchanged but
	// whose pinned digest names other bytes is refused.
	other := sha256.Sum256([]byte("other bytes"))
	_, err := Verify(loaded(t, closure.SHA256(hex.EncodeToString(other[:]))), state.target,
		&recordingRunner{target: state.target, document: buildInfoDocument(t, gotool.ARM64)})
	if err == nil || !strings.Contains(err.Error(), "does not match the pinned license digest") {
		t.Fatalf("a notice was accepted against another digest: %v", err)
	}
}

// TestChildArguments is the observation this verb exists to make: the candidate is
// never executed, the argv and environment are the closed ones, and the three
// commands run in the reviewed order.
func TestChildArguments(t *testing.T) {
	for _, arch := range gotool.Architectures() {
		state := newInstalledState(t, arch)
		runner := &recordingRunner{target: state.target, document: buildInfoDocument(t, arch)}
		release, err := Verify(state.data, state.target, runner)
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if release != testRelease {
			t.Fatalf("%s: release %q", arch, release)
		}
		if len(runner.missing) != 0 {
			t.Fatalf("%s: a child ran in a directory that does not exist: %v", arch,
				runner.missing)
		}
		want := []string{"env GOVERSION", "env GOROOT", "version -m -json " +
			state.target.Binary}
		got := []string{}
		for _, child := range runner.children {
			got = append(got, strings.Join(child.Argv, " "))
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s: ran %v, want %v", arch, got, want)
		}
		for _, child := range runner.children {
			if child.Program == state.target.Binary ||
				strings.HasPrefix(strings.Join(child.Argv, " "), state.target.Binary) {
				t.Fatalf("%s: the candidate binary was executed: %s %v", arch, child.Program,
					child.Argv)
			}
			if child.Program != filepath.Join(state.target.GoRoot, "bin", "go") {
				t.Fatalf("%s: program %q is not the pinned Go", arch, child.Program)
			}
			environment := strings.Join(child.Env, "\n")
			for _, required := range []string{"GOPROXY=off", "GOTOOLCHAIN=local", "GOFLAGS=",
				"GOENV=off", "GOWORK=off", "GOOS=linux", "GOARCH=" + arch.Value(),
				"CGO_ENABLED=0", "GOAUTH=off", "GOVCS=*:off"} {
				found := false
				for _, entry := range child.Env {
					if entry == required {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s: %q is missing from %s", arch, required, environment)
				}
			}
			// Every path the child can write is inside the invocation's own tree.
			for _, entry := range child.Env {
				for _, key := range []string{"HOME=", "GOPATH=", "GOMODCACHE=", "GOCACHE=",
					"TMPDIR="} {
					if value, found := strings.CutPrefix(entry, key); found &&
						!strings.HasPrefix(value, os.TempDir()) {
						t.Fatalf("%s: %s%s is outside the invocation scratch tree", arch, key,
							value)
					}
				}
			}
		}
		// The candidate is the last argument of the inspection and of nothing else.
		occurrences := 0
		for _, entry := range runner.children[2].Argv {
			if entry == state.target.Binary {
				occurrences++
			}
		}
		if occurrences != 1 {
			t.Fatalf("%s: the binary is named %d times in one argv", arch, occurrences)
		}
	}
}

// TestScratchIsRemoved: the invocation's tree is gone after the run, so a verifier
// leaving a module cache in the image fails here.
func TestScratchIsRemoved(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	runner := &recordingRunner{target: state.target, document: buildInfoDocument(t,
		gotool.ARM64)}
	if _, err := Verify(state.data, state.target, runner); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	scratch := filepath.Dir(runner.children[0].Dir)
	if filepath.Base(runner.children[0].Dir) != "module" {
		t.Fatalf("the child ran in %q, not the scratch module directory",
			runner.children[0].Dir)
	}
	if !strings.HasPrefix(filepath.Base(scratch), "gopls-verify-") {
		t.Fatalf("the scratch tree is %q, not an invocation-owned gopls-verify-* tree",
			scratch)
	}
	if _, err := os.Lstat(scratch); err == nil {
		t.Fatalf("%s survived the invocation", scratch)
	}
}

// TestPathSemanticsAreTheKernels: the paths the CLI is given are used in the
// spelling the caller wrote, because `..` after a symlink resolves at the kernel and
// not lexically.
//
// `a/link` is a symlink to `b/sub`, so `a/link/../gopls` is `b/gopls`. A reader that
// cleaned the path would inspect `a/gopls` instead — accepting a candidate the caller
// did not name, or refusing one they did. The same holds for the notice directory.
func TestPathSemanticsAreTheKernels(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	a := filepath.Join(state.root, "a")
	b := filepath.Join(state.root, "b")
	sub := filepath.Join(b, "sub")
	for _, directory := range []string{a, sub} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	if err := os.Symlink(sub, filepath.Join(a, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// Built by concatenation: filepath.Join would clean the `..` this case is about.
	requested := a + "/link/../gopls"
	candidate := func() {
		// The kernel's target of the requested spelling. The bytes are a script: the
		// build info is answered by the runner, so only the path rule is under test.
		if err := os.WriteFile(filepath.Join(b, "gopls"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := os.Chmod(filepath.Join(b, "gopls"), 0o755); err != nil {
			t.Fatalf("chmod: %v", err)
		}
	}
	runner := func() *recordingRunner {
		return &recordingRunner{target: state.target, document: buildInfoDocument(t,
			gotool.ARM64)}
	}

	// Only the lexically cleaned location carries a file: the requested path has no
	// candidate, so the verifier must refuse rather than verify `a/gopls`.
	if err := os.WriteFile(filepath.Join(a, "gopls"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	target, err := NewTarget(state.target.GoRoot, requested, state.target.ShareDir,
		gotool.ARM64)
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	if target.Binary != requested {
		t.Fatalf("the target names %q, not the requested spelling %q", target.Binary, requested)
	}
	_, err = Verify(state.data, target, runner())
	if err == nil || !strings.Contains(err.Error(), "installed: missing "+requested) {
		t.Fatalf("the lexically cleaned location was verified: %v", err)
	}
	// The kernel's target is the one that verifies. The positive control is the same
	// seam: a reader that refused every `..` would fail here too.
	candidate()
	if _, err := Verify(state.data, target, runner()); err != nil {
		t.Fatalf("the kernel-resolved candidate was refused: %v", err)
	}

	// The notice directory has the same semantics: `outer/link/..` is `real` when
	// `outer/link` links to `real/nested`, and a reader that cleaned would look in
	// `outer` instead.
	outer := filepath.Join(state.root, "outer")
	real := filepath.Join(state.root, "real")
	nested := filepath.Join(real, "nested")
	for _, directory := range []string{outer, nested} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	if err := os.Symlink(nested, filepath.Join(outer, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	wanted := filepath.Join(real, string(closure.FileLicenses))
	if err := os.WriteFile(wanted, state.notice, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(wanted, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	noticeTarget, err := NewTarget(state.target.GoRoot, state.target.Binary,
		outer+"/link/..", gotool.ARM64)
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	if _, err := Verify(state.data, noticeTarget, runner()); err != nil {
		t.Fatalf("the kernel-resolved notice directory was refused: %v", err)
	}
	// Now the kernel's directory has nothing, and the cleaned spelling has wrong
	// bytes. A reader that cleaned reads the wrong bytes and refuses with the digest
	// rule; the shipped reader reports the missing file at the requested spelling.
	if err := os.Remove(wanted); err != nil {
		t.Fatalf("remove: %v", err)
	}
	wrong := filepath.Join(outer, string(closure.FileLicenses))
	if err := os.WriteFile(wrong, []byte("another notice\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(wrong, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err = Verify(state.data, noticeTarget, runner())
	requestedNotice := outer + "/link/../" + string(closure.FileLicenses)
	if err == nil || !strings.Contains(err.Error(), "installed: missing "+requestedNotice) {
		t.Fatalf("the lexically cleaned notice directory was read: %v", err)
	}
}

// TestCandidateIsNeverExecuted: the observation is the candidate's own side effect,
// not a log from a child the verifier chose to run. The candidate writes a file next
// to itself when it starts; nothing else does, so the file appearing means something
// started it — including an `os.StartProcess` the injected runner cannot see.
func TestCandidateIsNeverExecuted(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	marker := filepath.Join(filepath.Dir(state.target.Binary), "executed")
	script := "#!/bin/sh\n: > '" + marker + "'\n"
	if err := os.WriteFile(state.target.Binary, []byte(script), 0o755); err != nil {
		t.Fatalf("write the candidate: %v", err)
	}
	if err := os.Chmod(state.target.Binary, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	runner := &recordingRunner{target: state.target, document: buildInfoDocument(t,
		gotool.ARM64)}
	if _, err := Verify(state.data, state.target, runner); err != nil {
		t.Fatalf("the fixture was refused: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the candidate was executed: %s exists", marker)
	}
	// The control that the marker would appear: run the same file the way a direct
	// execution would, and require its side effect. Without this the absence above
	// could be a script that never writes anything.
	control := exec.Command(state.target.Binary)
	if err := control.Run(); err != nil {
		t.Fatalf("the marker control could not run the candidate: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the candidate's own side effect did not appear under direct execution: %v",
			err)
	}
}

// TestCompilerRulesRefuse: `go env GOVERSION` and `go env GOROOT` are the proof that
// the child which answers is the pinned compiler in the pinned root.
func TestCompilerRulesRefuse(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	for _, testCase := range []struct {
		name     string
		runner   *recordingRunner
		fragment string
	}{
		{"a different compiler", &recordingRunner{goVersion: "go1.26.5"},
			"compiler is go1.26.5, want go1.27.3"},
		{"a compiler that does not answer",
			&recordingRunner{status: map[string]int{"env GOVERSION": 1}, stderr: "boom"},
			"go env GOVERSION failed (status 1): boom"},
		{"another Go root", &recordingRunner{goroot: "/opt/go-other"},
			"GOROOT is /opt/go-other, want the pinned "},
		{"a Go root that does not answer",
			&recordingRunner{status: map[string]int{"env GOROOT": 2}, stderr: ""},
			"go env GOROOT failed (status 2)"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.runner.target = state.target
			testCase.runner.document = buildInfoDocument(t, gotool.ARM64)
			_, err := Verify(state.data, state.target, testCase.runner)
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
			// The compiler and root rules fire before the build-info read, so the
			// inspected binary is never named.
			for _, child := range testCase.runner.children {
				if strings.HasPrefix(strings.Join(child.Argv, " "), "version -m") {
					t.Fatalf("the build-info read ran before the compiler proof")
				}
			}
		})
	}
	// A Go root reached through a symlink is the same root: the comparison resolves
	// both sides, so a symlinked checkout or /opt layout is not a false red.
	link := filepath.Join(state.root, "linked-go")
	if err := os.Symlink(state.target.GoRoot, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	target := state.target
	target.GoRoot = link
	if _, err := Verify(state.data, target, &recordingRunner{target: target,
		document: buildInfoDocument(t, gotool.ARM64)}); err != nil {
		t.Fatalf("a symlinked Go root was refused: %v", err)
	}
}

// TestBuildInfoWiringRefuses: the wiring from the child's stdout through the strict
// decoder and the comparison. The malformed case is the G3 fix reaching the verb.
func TestBuildInfoWiringRefuses(t *testing.T) {
	state := newInstalledState(t, gotool.ARM64)
	coherent := string(buildInfoDocument(t, gotool.ARM64))
	for _, testCase := range []struct {
		name     string
		document string
		status   map[string]int
		fragment string
	}{
		{"unparseable build info", "not a document", nil, "build info: not valid JSON"},
		{"a duplicate build-info key",
			strings.Replace(coherent, `"GoVersion":"go1.27.3"`,
				`"GoVersion":"go1.27.3","GoVersion":"go9.9.9"`, 1), nil, "duplicate key"},
		{"the wrong compiler in the build info",
			strings.Replace(coherent, `"GoVersion":"go1.27.3"`, `"GoVersion":"go1.26.5"`, 1),
			nil, "compiler is go1.26.5, want go1.27.3"},
		{"another closure",
			strings.Replace(coherent, `"example.test/dep"`, `"example.test/evil"`, 1),
			nil, "not in the reviewed closure"},
		{"another target",
			strings.Replace(coherent, `{"Key":"GOARCH","Value":"arm64"}`,
				`{"Key":"GOARCH","Value":"amd64"}`, 1),
			nil, "build settings differ"},
		{"a child that does not answer",
			coherent, map[string]int{"version -m -json PLACEHOLDER": 3},
			"go version -m -json failed (status 3): the pinned Go refused"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status := map[string]int{}
			for command, value := range testCase.status {
				status[strings.Replace(command, "PLACEHOLDER", state.target.Binary, 1)] = value
			}
			runner := &recordingRunner{target: state.target, document: []byte(testCase.document),
				status: status, stderr: "the pinned Go refused"}
			_, err := Verify(state.data, state.target, runner)
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
}
