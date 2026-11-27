package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
)

// dataDirectory is the committed closure, resolved so a symlinked checkout cannot
// turn a test red or green (rule R-e).
func dataDirectory(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot find this file")
	}
	directory := filepath.Join(filepath.Dir(file), "..", "..", "data", "gopls-closure")
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatalf("the committed closure is missing: %v", err)
	}
	return resolved
}

// loadCommitted loads the committed closure through the shipped loader.
func loadCommitted(t *testing.T) *closure.Data {
	t.Helper()
	source, err := closure.DirSource(dataDirectory(t))
	if err != nil {
		t.Fatalf("DirSource: %v", err)
	}
	data, err := closure.Load(context.Background(), closure.Request{}, source)
	if err != nil {
		t.Fatalf("the committed closure does not load: %v", err)
	}
	return data
}

// goRoot is a Go root the tree rules accept: it only has to be absolute and hold
// bin/go, because these tests never run it.
func goRoot(t *testing.T) string {
	t.Helper()
	program, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go on PATH: %v", err)
	}
	program, err = filepath.EvalSymlinks(program)
	if err != nil {
		t.Fatalf("cannot resolve %s: %v", program, err)
	}
	return filepath.Dir(filepath.Dir(program))
}

// copyTree copies the committed closure into a writable directory, so a test can
// change one input after it was loaded.
func copyTree(t *testing.T, source string) string {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "closure")
	if err := os.MkdirAll(destination, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	return destination
}

// fixture is one invocation's destination tree and the private paths it is given,
// so a test can name the module cache the installer will use.
type fixture struct {
	target Target
	paths  gotool.Paths
}

func newFixture(t *testing.T, arch gotool.Architecture) fixture {
	t.Helper()
	base := t.TempDir()
	built, err := NewTarget(goRoot(t), filepath.Join(base, "bin"),
		filepath.Join(base, "share", "gopls"), arch)
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	return fixture{target: built, paths: gotool.Paths{Scratch: filepath.Join(base, "scratch")}}
}

// TestTargetRulesRefuse: the `--go-root` rule is the installed verb's rule, called
// here rather than restated (deleting CheckGoRoot's body fails both packages).
func TestTargetRulesRefuse(t *testing.T) {
	root := goRoot(t)
	arch := gotool.ARM64
	for _, testCase := range []struct {
		name   string
		goRoot string
		want   string
	}{
		{"a relative Go root", "opt/go", "--go-root must be an absolute Go root with bin/go"},
		{"a Go root without bin/go", filepath.Join(root, "does-not-exist"),
			"--go-root must be an absolute Go root with bin/go"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewTarget(testCase.goRoot, "/tmp/bin", "/tmp/share", arch)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, testCase.want)
			}
		})
	}
}

// TestExistingDestinationRefused: a destination is never overwritten, and the
// refusal happens before the first child runs.
func TestExistingDestinationRefused(t *testing.T) {
	data := loadCommitted(t)
	for _, testCase := range []struct {
		name  string
		write func(Target)
	}{
		{"an existing binary", func(built Target) {
			if err := os.MkdirAll(built.BinDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(built.Binary(), []byte("existing"), 0o755); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"an existing share directory", func(built Target) {
			if err := os.MkdirAll(built.ShareDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current := newFixture(t, gotool.ARM64)
			built := current.target
			testCase.write(built)
			transcript := &Transcript{}
			_, err := Install(data, built, Options{Runner: testCaseRunner(t),
				Paths: current.paths, Transcript: transcript})
			if err == nil || !strings.Contains(err.Error(),
				"refusing to install over an existing path") {
				t.Fatalf("got %v, want an existing-destination refusal", err)
			}
			if len(transcript.Steps()) != 0 {
				t.Fatalf("the refusal ran %d children, want none", len(transcript.Steps()))
			}
		})
	}
}

// TestWrongCompilerMakesNoRequest: the compiler is proved before the build, so a
// wrong compiler costs no module request at all — and the transcript names that one
// child, which is what makes "no build" an observation rather than an absence.
func TestWrongCompilerMakesNoRequest(t *testing.T) {
	data := loadCommitted(t)
	current := newFixture(t, gotool.ARM64)
	transcript := &Transcript{}
	runner := runnerFunc(func(child gotool.Child) (gotool.Result, error) {
		if strings.Join(child.Argv, " ") == "env GOVERSION" {
			return gotool.Result{Stdout: "go1.0.0\n"}, nil
		}
		return gotool.Result{Stdout: ""}, nil
	})
	_, err := Install(data, current.target, Options{Runner: runner, Paths: current.paths,
		Transcript: transcript})
	if err == nil || !strings.Contains(err.Error(), "compiler is go1.0.0, want go"+string(data.Pin.Go)) {
		t.Fatalf("got %v, want a compiler refusal", err)
	}
	steps := transcript.Steps()
	if len(steps) != 1 || strings.Join(steps[0].Argv, " ") != "env GOVERSION" {
		t.Fatalf("the refusal ran %v, want only the version read",
			argvs(steps))
	}
}

// TestChildArguments: exactly one build, with the reviewed flags, in the closed
// environment, in the scratch module directory.
func TestChildArguments(t *testing.T) {
	data := loadCommitted(t)
	current := newFixture(t, gotool.ARM64)
	transcript := &Transcript{}
	_, err := Install(data, current.target, Options{
		Runner: successfulRunner(t, data, current.target), Paths: current.paths,
		Transcript: transcript})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	steps := transcript.Steps()
	names := argvs(steps)
	if len(names) != 4 {
		t.Fatalf("ran %v, want the version read, the root read, one build and one "+
			"build-info read", names)
	}
	for index, want := range map[int]string{0: "env GOVERSION", 1: "env GOROOT"} {
		if names[index] != want {
			t.Fatalf("child %d is %q, want %q (all: %v)", index, names[index], want, names)
		}
	}
	if !strings.HasPrefix(names[2], "build ") {
		t.Fatalf("child 2 is %q, want the one build", names[2])
	}
	if !strings.HasPrefix(names[3], "version -m -json ") {
		t.Fatalf("child 3 is %q, want the build-info read", names[3])
	}
	buildStep := steps[2]
	flags := buildStep.Argv
	if len(flags) != 7 || flags[0] != "build" || flags[1] != "-mod=readonly" ||
		flags[2] != "-trimpath" || flags[3] != "-buildvcs=false" || flags[4] != "-o" ||
		flags[6] != closure.Gopls {
		t.Fatalf("build argv %v is not the reviewed invocation", flags)
	}
	if !strings.HasSuffix(flags[5], string(os.PathSeparator)+"gopls") {
		t.Fatalf("the build's -o is %q, want the scratch output path", flags[4])
	}
	environment := map[string]string{}
	for _, entry := range buildStep.Env {
		key, value, _ := strings.Cut(entry, "=")
		environment[key] = value
	}
	if len(buildStep.Env) != 21 {
		t.Fatalf("%d environment entries, want the 21 reviewed keys: %v",
			len(buildStep.Env), buildStep.Env)
	}
	for key, value := range map[string]string{
		"GOFLAGS": "", "GOTOOLCHAIN": "local", "GOPROXY": closure.Proxy,
		"GOSUMDB": closure.SumDB, "GOOS": "linux", "GOARCH": "arm64",
		"CGO_ENABLED": "0", "GOWORK": "off", "GOENV": "off", "GOVCS": "*:off",
		"GOAUTH": "off", "GOPRIVATE": "", "GONOPROXY": "", "GONOSUMDB": "",
		"GOINSECURE": "",
	} {
		if environment[key] != value {
			t.Fatalf("%s is %q, want %q", key, environment[key], value)
		}
	}
	if strings.Contains(strings.Join(buildStep.Argv, " "), ",direct") ||
		strings.Contains(environment["GOPROXY"], ",direct") {
		t.Fatal("the build origin carries a direct fallback")
	}
}

// TestCommittedBytesAreCopied: the build compiles the digest-checked copy, and an
// input changed after loading is refused before any child runs.
func TestCommittedBytesAreCopied(t *testing.T) {
	directory := copyTree(t, dataDirectory(t))
	source, err := closure.DirSource(directory)
	if err != nil {
		t.Fatalf("DirSource: %v", err)
	}
	data, err := closure.Load(context.Background(), closure.Request{}, source)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	current := newFixture(t, gotool.ARM64)
	var copies []string
	runner := successfulRunnerWith(t, data, current.target, func(child gotool.Child) {
		if len(child.Argv) > 0 && child.Argv[0] == "build" {
			raw, err := os.ReadFile(filepath.Join(child.Dir, string(closure.FileGoSum)))
			if err != nil {
				t.Errorf("the build directory has no go.sum: %v", err)
				return
			}
			copies = append(copies, string(raw))
		}
	})
	if _, err := Install(data, current.target, Options{Runner: runner,
		Paths: current.paths, Transcript: &Transcript{}}); err != nil {
		t.Fatalf("install: %v", err)
	}
	committed, err := os.ReadFile(filepath.Join(directory, string(closure.FileGoSum)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(copies) != 1 || copies[0] != string(committed) {
		t.Fatal("the build did not compile the committed go.sum bytes")
	}

	// The same input, changed on disk after loading, must be refused before the
	// build: publication writes the bytes it verified, never a fresh read.
	if err := os.WriteFile(filepath.Join(directory, string(closure.FileLicenses)),
		[]byte("UNREVIEWED\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	retry := newFixture(t, gotool.ARM64)
	_, err = Install(data, retry.target, Options{
		Runner: successfulRunner(t, data, retry.target), Paths: retry.paths,
		Transcript: &Transcript{}})
	if err == nil || !strings.Contains(err.Error(), "bytes differ from the committed digest") {
		t.Fatalf("got %v, want a digest refusal", err)
	}
	// The refusal is before publication, not before the build: the notice bytes are
	// re-read where they are used, which is after the build produced the binary.
	for _, path := range []string{retry.target.Binary(), retry.target.Notice()} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("a drifted input still published %s", path)
		}
	}
}

// TestCacheNoticeMutation: the notice bytes the build used are proved against the
// committed evidence, in both directions.
func TestCacheNoticeMutation(t *testing.T) {
	data := loadCommitted(t)
	cache := t.TempDir()
	if err := writeCache(cache, data); err != nil {
		t.Fatalf("write the cache: %v", err)
	}
	if err := verifyModuleNotices(data, cache); err != nil {
		t.Fatalf("the committed evidence does not match its own cache: %v", err)
	}
	module := data.Pin.Modules[0]
	directory := filepath.Join(cache, escapeModule(string(module.ID.Path))+"@"+
		escapeModule(string(module.ID.Version)))
	extra := filepath.Join(directory, "NOTICE")
	if err := os.WriteFile(extra, []byte("cache-only notice\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := verifyModuleNotices(data, cache); err == nil ||
		!strings.Contains(err.Error(), "license evidence does not match the build cache") {
		t.Fatalf("a cache-only notice was accepted: %v", err)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatalf("remove: %v", err)
	}
	changed := filepath.Join(directory, string(data.Notices[0].Name))
	if err := os.WriteFile(changed, []byte("different bytes\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := verifyModuleNotices(data, cache); err == nil ||
		!strings.Contains(err.Error(), "license evidence differs from the build cache") {
		t.Fatalf("changed cache bytes were accepted: %v", err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := verifyModuleNotices(data, cache); err == nil ||
		!strings.Contains(err.Error(), "license evidence missing") {
		t.Fatalf("a missing module cache directory was accepted: %v", err)
	}
}

// TestPublicationFailureWithdraws: every claimed file is withdrawn when a later
// metadata step fails.
func TestPublicationFailureWithdraws(t *testing.T) {
	built := newFixture(t, gotool.ARM64).target
	// The built bytes are the test's own, so this case is about publication.
	source := filepath.Join(t.TempDir(), "gopls")
	if err := os.WriteFile(source, []byte("\x7fELF not really\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	notices := []byte("verified notice bytes\n")
	fault := func(phase Phase) error {
		if phase == PhaseNoticeWritten {
			return errors.New("injected chmod failure")
		}
		return nil
	}
	_, err := publish(built, source, notices, fault)
	if err == nil || !strings.Contains(err.Error(), "injected chmod failure") {
		t.Fatalf("got %v, want the injected failure", err)
	}
	if _, err := os.Lstat(built.Binary()); err == nil {
		t.Fatal("the published binary survived the refusal")
	}
	// The owned directories are removed too, so the same target can be retried. This
	// is the G3 fix: Python left `share/gopls` behind and then refused the retry.
	for _, path := range []string{built.Notice(), built.ShareDir, built.BinDir} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("%s survived the refusal, so a retry would be refused", path)
		}
	}
	if _, err := publish(built, source, notices, nil); err != nil {
		t.Fatalf("the same-target retry failed: %v", err)
	}
	if info, err := os.Lstat(built.Binary()); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("the retried binary is not 0755: %v %v", info, err)
	}
	if info, err := os.Lstat(built.Notice()); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the retried notice is not 0644: %v %v", info, err)
	}
}

// TestConcurrentPublicationCannotOverwrite: the destination is created
// exclusively, so a second invocation cannot replace a publication — and a
// withdrawal therefore never removes another invocation's file.
//
// The interleaving is deterministic rather than timing-dependent: A's failure hook
// runs B's publication synchronously at the one moment both are past their own
// preflight, which is the window a separate preflight cannot close.
func TestConcurrentPublicationCannotOverwrite(t *testing.T) {
	built := newFixture(t, gotool.ARM64).target
	source := filepath.Join(t.TempDir(), "gopls")
	if err := os.WriteFile(source, []byte("\x7fELF first\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	notices := []byte("verified notice bytes\n")
	var secondErr error
	fault := func(phase Phase) error {
		if phase == PhaseBinaryPublished {
			_, secondErr = publish(built, source, notices, nil)
			return errors.New("injected: the first invocation fails after publishing")
		}
		return nil
	}
	if _, err := publish(built, source, notices, fault); err == nil ||
		!strings.Contains(err.Error(), "injected:") {
		t.Fatalf("got %v, want the injected failure", err)
	}
	if secondErr == nil {
		t.Fatal("a second invocation published over the first while it was still running")
	}
	if !strings.Contains(secondErr.Error(), "refusing to install over an existing path") {
		t.Fatalf("the second invocation was refused for another reason: %v", secondErr)
	}
	// The first invocation withdrew exactly what it created: its file (the second
	// invocation has none, and must not lose one), and the directories it made.
	for _, path := range []string{built.Binary(), built.Notice(), built.ShareDir, built.BinDir} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("%s survived a failed publication", path)
		}
	}
	// And the target is still installable afterwards.
	if _, err := publish(built, source, notices, nil); err != nil {
		t.Fatalf("the target is not installable after the interleaving: %v", err)
	}
}

// TestLateWriteRetryThroughInstall: the whole verb, including a real late failure,
// leaves no owned file or directory, so an immediate retry succeeds.
func TestLateWriteRetryThroughInstall(t *testing.T) {
	data := loadCommitted(t)
	current := newFixture(t, gotool.AMD64)
	built := current.target
	fired := false
	fault := func(phase Phase) error {
		if !fired && phase == PhaseBinaryPublished {
			fired = true
			return errors.New("injected late failure")
		}
		return nil
	}
	_, err := Install(data, built, Options{Runner: successfulRunner(t, data, built),
		Paths: current.paths, Transcript: &Transcript{}, Fault: fault})
	if err == nil || !strings.Contains(err.Error(), "injected late failure") {
		t.Fatalf("got %v, want the injected failure", err)
	}
	for _, path := range []string{built.Binary(), built.Notice(), built.ShareDir, built.BinDir} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("%s survived the failed installation", path)
		}
	}
	if _, err := Install(data, built, Options{Runner: successfulRunner(t, data, built),
		Paths: gotool.Paths{Scratch: t.TempDir()}, Transcript: &Transcript{}}); err != nil {
		t.Fatalf("the same-target retry failed: %v", err)
	}
}

// TestScratchIsRemoved: an invocation that creates its own scratch tree removes it,
// on success and after a refusal, and the path is read from this run's transcript —
// not assumed from a prefix that another process could also use.
func TestScratchIsRemoved(t *testing.T) {
	data := loadCommitted(t)
	for _, testCase := range []struct {
		name  string
		fault func(Phase) error
		fails bool
	}{
		{"on success", nil, false},
		{"after a failure", func(Phase) error { return errors.New("injected") }, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			current := newFixture(t, gotool.ARM64)
			transcript := &Transcript{}
			_, err := Install(data, current.target, Options{
				Runner: successfulRunner(t, data, current.target), Transcript: transcript,
				Fault: testCase.fault})
			if testCase.fails != (err != nil) {
				t.Fatalf("install error %v, want failure=%v", err, testCase.fails)
			}
			steps := transcript.Steps()
			if len(steps) == 0 {
				t.Fatal("the transcript is empty, so no scratch path is this run's evidence")
			}
			home := envValue(steps[0].Env, "HOME")
			if home == "" {
				t.Fatal("the first child's environment names no HOME")
			}
			scratch := filepath.Dir(home)
			t.Cleanup(func() { _ = os.RemoveAll(scratch) })
			if _, err := os.Lstat(scratch); err == nil {
				t.Fatalf("the scratch tree %s was retained", scratch)
			}
		})
	}
	// A caller-provided tree is a caller's to remove: the invocation does not delete
	// a directory it did not create.
	current := newFixture(t, gotool.ARM64)
	if _, err := Install(data, current.target, Options{
		Runner: successfulRunner(t, data, current.target), Paths: current.paths,
		Transcript: &Transcript{}}); err != nil {
		t.Fatalf("install with a caller-provided scratch: %v", err)
	}
	if _, err := os.Lstat(current.paths.Scratch); err != nil {
		t.Fatalf("the invocation removed a caller-owned scratch tree: %v", err)
	}
}

// TestWrongSelectionIsRefused: the built binary's own build info must name the
// reviewed selection, sums, compiler and target settings.
func TestWrongSelectionIsRefused(t *testing.T) {
	data := loadCommitted(t)
	current := newFixture(t, gotool.ARM64)
	runner := runnerFunc(func(child gotool.Child) (gotool.Result, error) {
		switch {
		case strings.Join(child.Argv, " ") == "env GOVERSION":
			return gotool.Result{Stdout: data.Pin.Go.Toolchain() + "\n"}, nil
		case strings.Join(child.Argv, " ") == "env GOROOT":
			return gotool.Result{Stdout: current.target.GoRoot + "\n"}, nil
		case len(child.Argv) > 0 && child.Argv[0] == "build":
			if err := writeCache(envValue(child.Env, "GOMODCACHE"), data); err != nil {
				return gotool.Result{}, err
			}
			if err := os.WriteFile(child.Argv[len(child.Argv)-2],
				[]byte("\x7fELF built by the suite\n"), 0o755); err != nil {
				return gotool.Result{}, err
			}
			return gotool.Result{}, nil
		case len(child.Argv) > 0 && child.Argv[0] == "version":
			document := buildInfoDocument(data, current.target.Arch)
			deps := document["Deps"].([]map[string]any)
			deps[0]["Version"] = "v9.9.9"
			document["Deps"] = deps
			raw, err := json.Marshal(document)
			if err != nil {
				return gotool.Result{}, err
			}
			return gotool.Result{Stdout: string(raw)}, nil
		}
		t.Fatalf("unexpected child: %v", child.Argv)
		return gotool.Result{}, nil
	})
	_, err := Install(data, current.target, Options{Runner: runner, Paths: current.paths,
		Transcript: &Transcript{}})
	if err == nil || !strings.Contains(err.Error(), "not in the reviewed closure") {
		t.Fatalf("got %v, want the build-info identity refusal", err)
	}
	for _, path := range []string{current.target.Binary(), current.target.Notice()} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("a wrong selection still published %s", path)
		}
	}
}

// TestTemporaryNameIsInvocationOwned: publication's temporary file is this
// invocation's own, not a fixed name another invocation could already hold.
//
// The witness is a pre-existing directory where a fixed `.gopls.new` would be: a
// program that wrote that name fails, and this one publishes.
func TestTemporaryNameIsInvocationOwned(t *testing.T) {
	built := newFixture(t, gotool.ARM64).target
	if err := os.MkdirAll(built.BinDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	decoy := filepath.Join(built.BinDir, ".gopls.new")
	if err := os.Mkdir(decoy, 0o755); err != nil {
		t.Fatalf("mkdir the decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "keep"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write the decoy's file: %v", err)
	}
	source := filepath.Join(t.TempDir(), "gopls")
	if err := os.WriteFile(source, []byte("\x7fELF not really\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := publish(built, source, []byte("notice\n"), nil); err != nil {
		t.Fatalf("publication used a name another invocation could hold: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(decoy, "keep")); err != nil {
		t.Fatalf("publication touched a path it does not own: %v", err)
	}
}

// TestPartialNoticeWriteIsWithdrawn: a notice write that fails partway leaves a file
// behind, and it must be withdrawn.
//
// The failure is real, not injected: the child sets RLIMIT_FSIZE below the notice
// size, so the kernel stops the write at the limit and the file exists with four
// kilobytes in it. A child process is used because the limit is per-process, and the
// parent asserts the child's own output names the failure — a child that never
// reached the write would otherwise look like a pass.
func TestPartialNoticeWriteIsWithdrawn(t *testing.T) {
	if os.Getenv(partialNoticeChildEnv) == "1" {
		partialNoticeChild(t)
		return
	}
	command := exec.Command(os.Args[0], "-test.run=TestPartialNoticeWriteIsWithdrawn",
		"-test.v")
	command.Env = append(os.Environ(), partialNoticeChildEnv+"=1")
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("the partial-write child failed: %v\n%s%s", err, stdout.String(),
			stderr.String())
	}
	if !strings.Contains(stdout.String()+stderr.String(), "file too large") {
		t.Fatalf("the child never reached the real write failure:\n%s%s", stdout.String(),
			stderr.String())
	}
}

// partialNoticeChild is the body of the child process above.
func partialNoticeChild(t *testing.T) {
	var previous syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
		t.Fatalf("Getrlimit: %v", err)
	}
	limit := syscall.Rlimit{Cur: 4096, Max: previous.Max}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatalf("Setrlimit: %v", err)
	}
	defer func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &previous); err != nil {
			t.Logf("could not restore RLIMIT_FSIZE: %v", err)
		}
	}()
	built := newFixture(t, gotool.ARM64).target
	source := filepath.Join(t.TempDir(), "gopls")
	if err := os.WriteFile(source, []byte("\x7fELF built by the suite\n"), 0o600); err != nil {
		t.Fatalf("write the built binary: %v", err)
	}
	// Larger than the limit, so the write stops partway and the file stays on disk.
	_, err := publish(built, source, bytes.Repeat([]byte("n"), 8192), nil)
	// Printed on purpose: the parent reads this line back, so a child that never
	// reached the write cannot look like a pass.
	fmt.Printf("partial-notice-write: %v\n", err)
	if err == nil || !strings.Contains(err.Error(), "file too large") {
		t.Fatalf("got %v, want the size-limit failure", err)
	}
	if info, statErr := os.Lstat(built.Notice()); statErr == nil {
		t.Fatalf("the partial notice survived the failure (size %d, mode %v)",
			info.Size(), info.Mode())
	}
	for _, path := range []string{built.Binary(), built.ShareDir, built.BinDir} {
		if _, statErr := os.Lstat(path); statErr == nil {
			t.Fatalf("%s survived the failure, so a retry would be refused", path)
		}
	}
	// The same target, with a notice that fits: the retry must work.
	if _, err := publish(built, source, []byte("small notice\n"), nil); err != nil {
		t.Fatalf("the same-target retry failed: %v", err)
	}
}

// TestCreatedDirectoriesAreWithdrawn: the directories a failed invocation created are
// withdrawn, including the ones created before a later Mkdir failed.
//
// The failure is real: a umask of 0777 makes the first level she creates mode 000, so
// the next level cannot be entered and its Mkdir fails with EACCES. A program that
// dropped the already-created list on that error would leave the first directory
// behind and its own retry would be refused.
func TestCreatedDirectoriesAreWithdrawn(t *testing.T) {
	base := t.TempDir()
	created, err := createOwnedDirs(filepath.Join(base, "a", "b"))
	if err != nil {
		t.Fatalf("the setup's first level could not be created: %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created %v, want both levels", created)
	}
	for i := len(created) - 1; i >= 0; i-- {
		if err := os.Remove(created[i]); err != nil {
			t.Fatalf("remove %s: %v", created[i], err)
		}
	}

	previous := syscall.Umask(0o777)
	defer syscall.Umask(previous)
	partial, err := createOwnedDirs(filepath.Join(base, "x", "y"))
	if err == nil {
		t.Fatalf("a mode-000 parent let the second level be created")
	}
	if len(partial) == 0 || partial[0] != filepath.Join(base, "x") {
		t.Fatalf("the created list is %v, want the first level it made: a caller that "+
			"drops it cannot withdraw that directory", partial)
	}
	syscall.Umask(previous)
	for i := len(partial) - 1; i >= 0; i-- {
		if err := os.Remove(partial[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("remove %s: %v", partial[i], err)
		}
	}
	if _, err := os.Lstat(filepath.Join(base, "x")); err == nil {
		t.Fatal("the first level survived")
	}
}

// TestEscapeModule: the module cache spelling of an uppercase component.
func TestEscapeModule(t *testing.T) {
	for input, want := range map[string]string{
		"github.com/BurntSushi/toml": "github.com/!burnt!sushi/toml",
		"example.test/dep":           "example.test/dep",
		"v1.0.0":                     "v1.0.0",
	} {
		if got := escapeModule(input); got != want {
			t.Fatalf("escapeModule(%q) = %q, want %q", input, got, want)
		}
	}
}

const partialNoticeChildEnv = "GO_DEV_INSTALL_PARTIAL_NOTICE_CHILD"

// --- fakes -------------------------------------------------------------------

// runnerFunc is a suite runner: it answers as the named child and records nothing
// itself, because the installer's transcript is what is under test.
type runnerFunc func(gotool.Child) (gotool.Result, error)

func (r runnerFunc) Run(child gotool.Child) (gotool.Result, error) { return r(child) }

// testCaseRunner answers nothing useful; a test that reaches it has already failed.
func testCaseRunner(t *testing.T) gotool.Runner {
	t.Helper()
	return runnerFunc(func(child gotool.Child) (gotool.Result, error) {
		t.Fatalf("unexpected child: %v", child.Argv)
		return gotool.Result{}, nil
	})
}

// successfulRunner answers the compiler reads and produces the built binary and a
// build-info document that describes exactly the committed selection.
func successfulRunner(t *testing.T, data *closure.Data,
	target Target) gotool.Runner {
	return successfulRunnerWith(t, data, target, nil)
}

func successfulRunnerWith(t *testing.T, data *closure.Data, target Target,
	observe func(gotool.Child)) gotool.Runner {
	return runnerFunc(func(child gotool.Child) (gotool.Result, error) {
		if observe != nil {
			observe(child)
		}
		switch {
		case strings.Join(child.Argv, " ") == "env GOVERSION":
			return gotool.Result{Stdout: data.Pin.Go.Toolchain() + "\n"}, nil
		case strings.Join(child.Argv, " ") == "env GOROOT":
			return gotool.Result{Stdout: target.GoRoot + "\n"}, nil
		case len(child.Argv) > 0 && child.Argv[0] == "build":
			// The real child downloads into its module cache during the build, so the
			// fake one materialises the committed notice layout there now: that is the
			// input the cache proof reads.
			if err := writeCache(envValue(child.Env, "GOMODCACHE"), data); err != nil {
				return gotool.Result{}, err
			}
			out := child.Argv[len(child.Argv)-2]
			if err := os.WriteFile(out, []byte("\x7fELF built by the suite\n"), 0o755); err != nil {
				return gotool.Result{}, err
			}
			return gotool.Result{}, nil
		case len(child.Argv) > 0 && child.Argv[0] == "version":
			raw, err := json.Marshal(buildInfoDocument(data, target.Arch))
			if err != nil {
				return gotool.Result{}, err
			}
			return gotool.Result{Stdout: string(raw)}, nil
		}
		t.Fatalf("unexpected child: %v", child.Argv)
		return gotool.Result{}, nil
	})
}

// buildInfoDocument is what the pinned compiler would report for the committed
// selection and this Linux target. The real thing is exercised by the pins suite's
// loopback differential; here it lets the publication path be reached.
func buildInfoDocument(data *closure.Data, arch gotool.Architecture) map[string]any {
	deps := []map[string]any{}
	root := map[string]any{}
	for _, module := range data.Pin.Modules {
		row := map[string]any{"Path": string(module.ID.Path), "Version": string(module.ID.Version),
			"Sum": string(module.H1)}
		if string(module.ID.Path) == closure.Gopls {
			root = row
			continue
		}
		deps = append(deps, row)
	}
	settings := map[string]string{
		"-buildmode": "exe", "-compiler": "gc", "-trimpath": "true",
		"DefaultGODEBUG": "tracebacklabels=0,x509sslcertoverrideplatform=0",
		"CGO_ENABLED":    "0", "GOOS": "linux", "GOARCH": arch.Value(),
	}
	if arch == gotool.AMD64 {
		settings["GOAMD64"] = "v1"
	} else {
		settings["GOARM64"] = "v8.0"
	}
	rows := []map[string]any{}
	for key, value := range settings {
		rows = append(rows, map[string]any{"Key": key, "Value": value})
	}
	return map[string]any{"GoVersion": data.Pin.Go.Toolchain(), "Path": closure.Gopls,
		"Main": root, "Deps": deps, "Settings": rows}
}

// writeCache materialises the committed notice evidence as a module cache tree.
func writeCache(cache string, data *closure.Data) error {
	for _, section := range data.Notices {
		directory := filepath.Join(cache, escapeModule(string(section.ID.Path))+"@"+
			escapeModule(string(section.ID.Version)))
		if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(string(section.Name))),
			0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(directory, string(section.Name)), section.Raw,
			0o644); err != nil {
			return err
		}
	}
	// One module directory per selected module, even when it ships no notice: a
	// missing directory is missing evidence, and that is a different rule's case.
	for _, module := range data.Pin.Modules {
		directory := filepath.Join(cache, escapeModule(string(module.ID.Path))+"@"+
			escapeModule(string(module.ID.Version)))
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// envValue is one variable's value from a child's ordered environment.
func envValue(environment []string, key string) string {
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, key+"="); found {
			return value
		}
	}
	return ""
}

// argvs renders a transcript as `program argv` lines, for a diagnostic.
func argvs(steps []Step) []string {
	rendered := make([]string, 0, len(steps))
	for _, step := range steps {
		rendered = append(rendered, strings.Join(step.Argv, " "))
	}
	return rendered
}
