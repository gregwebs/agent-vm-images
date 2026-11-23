package toolchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// installRunner is the fixture bootstrap and the installed tools in one double.
// The hook runs when the bootstrap starts, which is where a real rustup-init
// writes the homes the post-install check reads.
type installRunner struct {
	inner      *fakeRunner
	hook       func()
	block      bool
	started    chan struct{}
	startOnce  sync.Once
	failStatus map[string]int
	failError  map[string]error
}

func (r *installRunner) Run(ctx context.Context, argv, env []string, cwd string) (Result, error) {
	key := filepath.Base(argv[0]) + " " + strings.Join(argv[1:], " ")
	if status, present := r.failStatus[key]; present {
		return Result{Status: status, Stderr: "boom"}, nil
	}
	if err, present := r.failError[key]; present {
		return Result{}, err
	}
	if filepath.Base(argv[0]) == "rustup" && isInstallVerb(argv) {
		return Result{}, nil
	}
	if filepath.Base(argv[0]) == "rustup-init" {
		if r.started != nil {
			r.startOnce.Do(func() { close(r.started) })
		}
		if r.block {
			<-ctx.Done()
			return Result{}, ctx.Err()
		}
		if r.hook != nil {
			r.hook()
		}
		return Result{}, nil
	}
	return r.inner.Run(ctx, argv, env, cwd)
}

func resolvedTemp(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func installFixture(t *testing.T, arch Architecture) (Pin, *VerifiedLayout, Homes, *fakeRunner) {
	t.Helper()
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	root := resolvedTemp(t)
	if err := os.MkdirAll(filepath.Join(root, "dist-root", "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(root, "rustup-init")
	if err := os.WriteFile(bootstrap, elfbootstrap(arch), 0o755); err != nil {
		t.Fatal(err)
	}
	homes := Homes{Rustup: filepath.Join(root, "rustup"), Cargo: filepath.Join(root, "cargo")}
	layout := &VerifiedLayout{Root: filepath.Join(root, "dist-root"), Bootstrap: bootstrap,
		Closure: pin.Closure(arch), Channel: pin.Channel}
	outputs := cleanOutputs(arch)
	return pin, layout, homes, &fakeRunner{outputs: outputs, errors: map[string]error{}}
}

func executeRequest(homes Homes, arch Architecture, events string) InstallRequest {
	return InstallRequest{Arch: arch, Homes: homes, EventsPath: events}
}

func mustResolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// isInstallVerb reports the four rustup calls the installer itself makes, which
// the fixture answers without the installed-state outputs.
func isInstallVerb(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	if argv[1] == "set" || argv[1] == "default" {
		return true
	}
	return argv[1] == "toolchain" && len(argv) > 2 && argv[2] == "install"
}

func requireAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			t.Errorf("%s survived a failed install", path)
		}
	}
}

func requireNoScratch(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "rust-install-") ||
			strings.HasPrefix(entry.Name(), "rust-prepare-") ||
			strings.HasPrefix(entry.Name(), "rust-verify-") {
			t.Errorf("scratch %s survived", entry.Name())
		}
	}
}

// TestInstallTranscript is invariant 30: the exact argv, cwd and order, followed
// by the installed-state check.
func TestInstallTranscript(t *testing.T) {
	arch := AMD64
	pin, layout, homes, inner := installFixture(t, arch)
	var calls [][]string
	var cwds []string
	recording := &recordingInstallRunner{inner: inner, hook: func() {
		populateInstalledTree(t, homes, arch)
	}, calls: &calls, cwds: &cwds}
	eventsPath := filepath.Join(t.TempDir(), "events")
	if err := execute(context.Background(), layout, executeRequest(homes, arch, eventsPath),
		pin, recording, NewEvents(eventsPath)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	expected := [][]string{
		{layout.Bootstrap, "-y", "--no-modify-path", "--default-host",
			string(arch.Host()), "--default-toolchain", "none", "--profile", "minimal"},
		{filepath.Join(homes.Cargo, "bin", "rustup"), "set", "auto-self-update", "disable"},
		{filepath.Join(homes.Cargo, "bin", "rustup"), "set", "auto-install", "disable"},
		{filepath.Join(homes.Cargo, "bin", "rustup"), "toolchain", "install", "1.98.1",
			"--profile", "minimal", "--component", "clippy,rustfmt", "--target",
			string(arch.Musl()), "--no-self-update"},
		{filepath.Join(homes.Cargo, "bin", "rustup"), "default", "1.98.1"},
	}
	for index, want := range expected {
		if strings.Join(calls[index], "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("call %d is\n%q\nwant\n%q", index, calls[index], want)
		}
	}
	if cwds[0] != filepath.Dir(layout.Root) {
		t.Errorf("bootstrap cwd is %q, want %q", cwds[0], filepath.Dir(layout.Root))
	}
	if cwds[3] != filepath.Dir(layout.Root) {
		t.Errorf("toolchain install cwd is %q, want %q", cwds[3], filepath.Dir(layout.Root))
	}
	raw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"execute bootstrap", "execute rustup set auto-self-update",
		"execute rustup set auto-install", "execute rustup toolchain install",
		"execute rustup default", "install-ok amd64"} {
		if !strings.Contains(string(raw), line+"\n") {
			t.Errorf("events lack %q:\n%s", line, raw)
		}
	}
	if !strings.Contains(strings.Join(calls[len(calls)-1], " "), "rustfmt --version") {
		t.Errorf("the installed-state check did not run: %v", calls[len(calls)-1])
	}
}

type recordingInstallRunner struct {
	inner *fakeRunner
	hook  func()
	calls *[][]string
	cwds  *[]string
}

func (r *recordingInstallRunner) Run(ctx context.Context, argv, env []string,
	cwd string) (Result, error) {
	*r.calls = append(*r.calls, argv)
	*r.cwds = append(*r.cwds, cwd)
	if filepath.Base(argv[0]) == "rustup" && isInstallVerb(argv) {
		return Result{}, nil
	}
	if filepath.Base(argv[0]) == "rustup-init" {
		if r.hook != nil {
			r.hook()
		}
		return Result{}, nil
	}
	return r.inner.Run(ctx, argv, env, cwd)
}

// TestInstallEnvironmentIsBuilt is invariant 28 and Q2: the child environment is
// the reviewed literal list. An extra name, a missing name or a changed value
// fails here.
func TestInstallEnvironmentIsBuilt(t *testing.T) {
	t.Setenv("PATH", "/reviewed/bin")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("LC_ALL", "C")
	t.Setenv("TZ", "UTC")
	for _, banned := range ambientToolchainVariables {
		t.Setenv(banned, "ambient")
	}
	root := t.TempDir()
	homes := Homes{Rustup: "/opt/rustup", Cargo: "/opt/cargo"}
	scratch := filepath.Join(t.TempDir(), "rust-install-x")
	layout := &VerifiedLayout{Root: filepath.Join(root, "dist-root")}
	if err := os.MkdirAll(filepath.Join(filepath.Dir(layout.Root), "no-updates"), 0o700); err != nil {
		t.Fatal(err)
	}
	environment, err := installEnvironment(layout, homes, scratch)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PATH=/reviewed/bin", "LANG=C.UTF-8", "LC_ALL=C", "TZ=UTC",
		"HOME=" + filepath.Join(scratch, "home"),
		"XDG_CACHE_HOME=" + filepath.Join(scratch, "home", ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(scratch, "home", ".local", "share"),
		"XDG_CONFIG_HOME=" + filepath.Join(scratch, "home", ".config"),
		"TMPDIR=" + filepath.Join(scratch, "tmp"),
		"RUSTUP_HOME=/opt/rustup", "CARGO_HOME=/opt/cargo",
		"RUSTUP_DIST_SERVER=" + fileURI(layout.Root),
		"RUSTUP_UPDATE_ROOT=" + fileURI(mustResolve(t, filepath.Join(filepath.Dir(layout.Root),
			"no-updates"))),
		"RUSTUP_AUTO_INSTALL=0",
	}
	if strings.Join(environment, "\n") != strings.Join(want, "\n") {
		t.Fatalf("environment is\n%s\nwant\n%s", strings.Join(environment, "\n"),
			strings.Join(want, "\n"))
	}
	for _, entry := range environment {
		if strings.HasPrefix(entry, "PYTHONDONTWRITEBYTECODE=") {
			t.Fatal("PYTHONDONTWRITEBYTECODE is in the child environment; Go runs no Python")
		}
		for _, banned := range ambientToolchainVariables {
			if entry == banned+"=ambient" {
				t.Errorf("ambient %s reached the child", banned)
			}
		}
	}
	for _, directory := range []string{"home", "tmp", "home/.cache", "home/.local/share",
		"home/.config"} {
		info, err := os.Stat(filepath.Join(scratch, directory))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s is %o, want 0700", directory, info.Mode().Perm())
		}
	}
}

// TestRequireAbsentHomes is invariant 26.
func TestRequireAbsentHomes(t *testing.T) {
	root := resolvedTemp(t)
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "file")
	write(t, file, "x")
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "absent"), dangling); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		homes    Homes
		fragment string
	}{
		{"clean", Homes{Rustup: filepath.Join(root, "r"), Cargo: filepath.Join(root, "c")}, ""},
		{"relative", Homes{Rustup: "relative", Cargo: filepath.Join(root, "c")},
			"refusing a relative install home"},
		{"existing_empty", Homes{Rustup: real, Cargo: filepath.Join(root, "c")},
			"refusing to install over an existing path"},
		{"existing_populated", Homes{Rustup: filepath.Join(root, "r"), Cargo: real},
			"refusing to install over an existing path"},
		{"dangling_symlink", Homes{Rustup: dangling, Cargo: filepath.Join(root, "c")},
			"refusing to install over an existing path"},
		{"symlink_ancestor", Homes{Rustup: filepath.Join(link, "rustup"), Cargo: filepath.Join(root, "c")},
			"refusing a symlinked install-home ancestor"},
		{"file_ancestor", Homes{Rustup: filepath.Join(file, "rustup"), Cargo: filepath.Join(root, "c")},
			"refusing a non-directory install-home ancestor"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := RequireAbsentHomes(test.homes)
			if test.fragment == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.fragment) {
				t.Fatalf("got %v, want %q", err, test.fragment)
			}
		})
	}
}

// TestInstallPreconditionsBeforeFetch is invariant 26's ordering: a refusal that
// precedes any fetch makes zero transport calls, even when the pin directory
// does not exist.
func TestInstallPreconditionsBeforeFetch(t *testing.T) {
	root := resolvedTemp(t)
	existing := filepath.Join(root, "rustup")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	transport := &recordingTransport{files: map[string][]byte{}}
	channel, err := Install(context.Background(), InstallRequest{
		InputDir: filepath.Join(root, "absent-pin"), Arch: AMD64, ExpectChannel: "1.98.1",
		Homes: Homes{Rustup: existing, Cargo: filepath.Join(root, "cargo")},
		Trust: ReviewedTrust, Transport: transport})
	if err == nil || !strings.Contains(err.Error(), "refusing to install over an existing path") {
		t.Fatalf("got %v, want the home precondition", err)
	}
	if channel != "" {
		t.Fatalf("channel is %q", channel)
	}
	if len(transport.calls) != 0 {
		t.Fatalf("the precondition ran after a fetch: %v", transport.calls)
	}
	notDirectory := filepath.Join(root, "work-file")
	write(t, notDirectory, "x")
	_, err = Install(context.Background(), InstallRequest{
		InputDir: filepath.Join(root, "absent-pin"), Arch: AMD64, ExpectChannel: "1.98.1",
		Homes:   Homes{Rustup: filepath.Join(root, "r2"), Cargo: filepath.Join(root, "c2")},
		WorkDir: notDirectory, WorkDirSet: true,
		Trust: ReviewedTrust, Transport: transport})
	if err == nil || !strings.Contains(err.Error(), "--work-dir must be an existing directory") {
		t.Fatalf("got %v, want the work-dir refusal", err)
	}
}

func TestInstallCleanup(t *testing.T) {
	arch := AMD64
	cases := []struct {
		name   string
		runner func(*fakeRunner, Homes) *installRunner
		homes  func(root string) Homes
	}{
		{"bootstrap_fail", func(inner *fakeRunner, homes Homes) *installRunner {
			return &installRunner{inner: inner, failStatus: map[string]int{"rustup-init -y --no-modify-path --default-host " + string(arch.Host()) + " --default-toolchain none --profile minimal": 1}}
		}, nil},
		{"toolchain_install_fail", func(inner *fakeRunner, homes Homes) *installRunner {
			return &installRunner{inner: inner, failStatus: map[string]int{
				"rustup toolchain install 1.98.1 --profile minimal --component clippy,rustfmt --target " +
					string(arch.Musl()) + " --no-self-update": 1}}
		}, nil},
		{"check_installed_fail", func(inner *fakeRunner, homes Homes) *installRunner {
			inner.outputs["rustup --version"] = Result{Stdout: "rustup 1.27.0\n"}
			return &installRunner{inner: inner, hook: func() { populateInstalledTree(t, homes, arch) }}
		}, nil},
		{"exec_error", func(inner *fakeRunner, homes Homes) *installRunner {
			return &installRunner{inner: inner, failError: map[string]error{
				"rustup-init -y --no-modify-path --default-host " + string(arch.Host()) +
					" --default-toolchain none --profile minimal": errors.New("exec: no such file")}}
		}, nil},
		{"partial_home_creation", func(inner *fakeRunner, homes Homes) *installRunner {
			return &installRunner{inner: inner}
		}, func(root string) Homes {
			return Homes{Rustup: filepath.Join(root, "rustup"),
				Cargo: filepath.Join(root, "missing", "cargo")}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			pin, layout, homes, inner := installFixture(t, arch)
			if test.homes != nil {
				homes = test.homes(resolvedTemp(t))
			}
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			runner := test.runner(inner, homes)
			events := filepath.Join(t.TempDir(), "events")
			err := execute(context.Background(), layout, executeRequest(homes, arch, events),
				pin, runner, NewEvents(events))
			if err == nil {
				t.Fatal("the failed install reported success")
			}
			requireAbsent(t, homes.Rustup, homes.Cargo)
			requireNoScratch(t, temporary)
		})
	}
}

func TestInstallSignals(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal syscall.Signal
	}{
		{"sigint_sigterm", syscall.SIGINT},
		{"sigterm", syscall.SIGTERM},
	} {
		t.Run(test.name, func(t *testing.T) {
			arch := AMD64
			pin, layout, homes, inner := installFixture(t, arch)
			started := make(chan struct{})
			runner := &installRunner{inner: inner, block: true, started: started}
			temporary := t.TempDir()
			t.Setenv("TMPDIR", temporary)
			done := make(chan error, 1)
			go func() {
				done <- execute(context.Background(), layout, executeRequest(homes, arch, ""),
					pin, runner, NewEvents(""))
			}()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("the bootstrap never started")
			}
			if err := syscall.Kill(os.Getpid(), test.signal); err != nil {
				t.Fatal(err)
			}
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("execute did not return after the signal")
			}
			var signaled *SignalError
			if !errors.As(err, &signaled) || signaled.Signal != test.signal {
				t.Fatalf("error is %v, want %v", err, test.signal)
			}
			requireAbsent(t, homes.Rustup, homes.Cargo)
			requireNoScratch(t, temporary)
		})
	}
}
