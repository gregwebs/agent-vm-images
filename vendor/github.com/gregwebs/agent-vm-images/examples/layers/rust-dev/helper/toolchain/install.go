package toolchain

// The execution phase: run the pinned bootstrap and rustup against the private
// dist layout, verify what they installed, and leave nothing behind on any
// unsuccessful exit.
//
// Q2 of #231 (binding): the child environment is a reviewed literal list, not a
// per-variable comparison. Go runs no Python in this leg, so there is no
// PYTHONDONTWRITEBYTECODE to carry and no divergence to declare: an environment
// this side builds itself is a specification, not a measured disagreement. The
// environment is built key by key from four pass-through variables, so the
// source reader's post-copy AMBIENT_TOOLCHAIN_VARIABLES pop loop is deleted rather
// than tested -- it has no input that could exercise it. The reviewed ledger below
// sits above ambientToolchainVariables on purpose: a future variable must be
// added to the built list, and the test holds the exact names and values.

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/gpg"
)

// InstallRequest is one `install` invocation.
type InstallRequest struct {
	InputDir      string
	Arch          Architecture
	ExpectChannel string
	Homes         Homes
	WorkDir       string
	WorkDirSet    bool
	EventsPath    string
	Trust         TrustPolicy
	GPG           gpg.Program
	Transport     Transport
	// TransportFactory takes precedence over Transport when set: the fixture
	// seam needs the pin's exact URL closure before it can build one.
	TransportFactory TransportFactory
	Runner           Runner
	// ScratchParent overrides the system temp directory. Tests use it; a
	// production run leaves it empty.
	ScratchParent string
}

// SignalError reports the signal that ended an install after cleanup. The exit
// status is 128+signal, which is what a shell expects for an interrupted child.
type SignalError struct{ Signal syscall.Signal }

func (s *SignalError) Error() string { return "interrupted by " + s.Signal.String() }

// Install runs the whole leg and returns the pinned channel on success.
func Install(ctx context.Context, request InstallRequest) (Channel, error) {
	directory := filepath.Clean(request.InputDir)
	if request.WorkDirSet {
		info, err := os.Stat(request.WorkDir)
		if err != nil || !info.IsDir() {
			return "", refuse("--work-dir must be an existing directory: %s", request.WorkDir)
		}
	}
	// Preconditions before any fetch: a build must not download the closure and
	// only then discover that it would install over an existing toolchain.
	if err := RequireAbsentHomes(request.Homes); err != nil {
		return "", err
	}
	events := NewEvents(request.EventsPath)
	work, err := gpg.WorkDirectory()
	if err != nil {
		return "", err
	}
	defer removeKeyring(work)
	pin, err := LoadPin(directory, request.Trust)
	if err != nil {
		return "", err
	}
	if string(pin.Channel) != request.ExpectChannel {
		return "", refuse("pin channel %s != asserted channel %s", pin.Channel,
			request.ExpectChannel)
	}
	verifier := gpg.NewVerifier(request.GPG, filepath.Join(directory, KeyPath),
		work, PolicyFor(request.Trust))

	transport := request.Transport
	if request.TransportFactory != nil {
		transport, err = request.TransportFactory(PinnedURLs(pin, request.Arch))
		if err != nil {
			return "", adopt(err)
		}
	}
	if transport == nil {
		return "", refuse("install: no transport")
	}
	runner := request.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	parent := request.ScratchParent
	if request.WorkDirSet {
		parent = request.WorkDir
	}
	scratch, err := os.MkdirTemp(parent, "rust-prepare-")
	if err != nil {
		return "", adopt(err)
	}
	if err := os.Chmod(scratch, 0o700); err != nil {
		_ = os.RemoveAll(scratch)
		return "", adopt(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	layout, err := Prepare(ctx, directory, pin, request.Arch, scratch, transport, verifier, events)
	if err != nil {
		return "", err
	}
	if err := execute(ctx, layout, request, pin, runner, events); err != nil {
		return "", err
	}
	return pin.Channel, nil
}

// execute is phase 4: homes, environment, the exact rustup transcript, the
// installed-state check, and cleanup on every unsuccessful exit.
func execute(parent context.Context, layout *VerifiedLayout, request InstallRequest, pin Pin,
	runner Runner, events *Events) error {
	if err := RequireAbsentHomes(request.Homes); err != nil {
		return err
	}
	scratch, err := os.MkdirTemp(request.ScratchParent, "rust-install-")
	if err != nil {
		return adopt(err)
	}
	created := make([]string, 0, 2)
	installed := false
	defer func() {
		if !installed {
			removeCreatedHomes(created)
		}
		_ = os.RemoveAll(scratch)
	}()
	if err := os.Chmod(scratch, 0o700); err != nil {
		return adopt(err)
	}
	environment, err := installEnvironment(layout, request.Homes, scratch)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	received := make(chan os.Signal, 1)
	signal.Notify(received, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(received)

	run := func(argv []string, cwd, what string) error {
		result, err := runner.Run(ctx, argv, environment, cwd)
		if err != nil {
			return refuse("%s failed: %v", what, err)
		}
		if result.Status != 0 {
			return refuse("%s failed with status %d: %s", what, result.Status,
				outputTail(result))
		}
		return nil
	}

	for _, home := range []string{request.Homes.Rustup, request.Homes.Cargo} {
		if err := os.Mkdir(home, 0o755); err != nil {
			return adopt(err)
		}
		if err := os.Chmod(home, 0o755); err != nil {
			return adopt(err)
		}
		// Appended immediately, so a failure creating cargo removes rustup.
		created = append(created, home)
	}
	rustup := filepath.Join(request.Homes.Cargo, "bin", "rustup")
	root := filepath.Dir(layout.Root)
	if err := events.Record("execute bootstrap"); err != nil {
		return err
	}
	if err := run([]string{layout.Bootstrap, "-y", "--no-modify-path", "--default-host",
		string(request.Arch.Host()), "--default-toolchain", "none", "--profile", Profile},
		root, "rustup-init"); err != nil {
		return interrupted(ctx, received, err)
	}
	for _, setting := range []string{"auto-self-update", "auto-install"} {
		if err := events.Record("execute rustup set " + setting); err != nil {
			return err
		}
		if err := run([]string{rustup, "set", setting, "disable"}, "", "rustup set "+
			setting); err != nil {
			return interrupted(ctx, received, err)
		}
	}
	if err := events.Record("execute rustup toolchain install"); err != nil {
		return err
	}
	if err := run([]string{rustup, "toolchain", "install", string(pin.Channel), "--profile",
		Profile, "--component", strings.Join(CLIComponents, ","), "--target",
		string(request.Arch.Musl()), "--no-self-update"}, root,
		"rustup toolchain install"); err != nil {
		return interrupted(ctx, received, err)
	}
	if err := events.Record("execute rustup default"); err != nil {
		return err
	}
	if err := run([]string{rustup, "default", string(pin.Channel)}, "", "rustup default"); err != nil {
		return interrupted(ctx, received, err)
	}
	if err := CheckInstalled(ctx, pin, request.Arch, request.Homes, runner); err != nil {
		return interrupted(ctx, received, err)
	}
	if err := events.Record("install-ok " + request.Arch.Value()); err != nil {
		return err
	}
	installed = true
	return nil
}

// interrupted turns a cancelled context into the signal that caused it, so the
// CLI can exit 130/143 after cleanup instead of a generic refusal.
func interrupted(ctx context.Context, received chan os.Signal, err error) error {
	if ctx.Err() == nil {
		return err
	}
	select {
	case got := <-received:
		if signal, ok := got.(syscall.Signal); ok {
			return &SignalError{Signal: signal}
		}
	default:
	}
	return err
}

// installEnvironment is the reviewed literal child environment. Names and values
// are the specification: TestInstallEnvironmentIsBuilt fails on an extra name or
// a changed value.
func installEnvironment(layout *VerifiedLayout, homes Homes, scratch string) ([]string, error) {
	environment := make([]string, 0, 16)
	for _, key := range []string{"PATH", "LANG", "LC_ALL", "TZ"} {
		if value, present := os.LookupEnv(key); present {
			environment = append(environment, key+"="+value)
		}
	}
	if _, present := os.LookupEnv("PATH"); !present {
		environment = append(environment, "PATH="+imagesPath)
	}
	root, err := resolvePath(layout.Root)
	if err != nil {
		return nil, err
	}
	noUpdates, err := resolvePath(filepath.Join(filepath.Dir(layout.Root), NoUpdatesDirectory))
	if err != nil {
		return nil, err
	}
	for _, directory := range []string{"home", "tmp", "home/.cache", "home/.local/share",
		"home/.config"} {
		if err := makePrivate(filepath.Join(scratch, directory)); err != nil {
			return nil, err
		}
	}
	environment = append(environment,
		"HOME="+filepath.Join(scratch, "home"),
		"XDG_CACHE_HOME="+filepath.Join(scratch, "home", ".cache"),
		"XDG_DATA_HOME="+filepath.Join(scratch, "home", ".local", "share"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "home", ".config"),
		"TMPDIR="+filepath.Join(scratch, "tmp"),
		"RUSTUP_HOME="+homes.Rustup,
		"CARGO_HOME="+homes.Cargo,
		"RUSTUP_DIST_SERVER="+fileURI(root),
		"RUSTUP_UPDATE_ROOT="+fileURI(noUpdates),
		"RUSTUP_AUTO_INSTALL=0")
	return environment, nil
}

// resolvePath is `Path.resolve()` for an absolute path that exists.
func resolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	absolute, absErr := filepath.Abs(path)
	if absErr != nil {
		return "", adopt(absErr)
	}
	return absolute, nil
}

// fileURI is `Path.as_uri()`: a `file://` URI whose path is percent-encoded
// outside the unreserved set and "/". net/url is deliberately confined to
// internal/fetch (imports_test.go), so this is spelled here.
func fileURI(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	var builder strings.Builder
	builder.WriteString("file://")
	for _, b := range []byte(path) {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
			b == '-' || b == '.' || b == '_' || b == '~' || b == '/' {
			builder.WriteByte(b)
			continue
		}
		const hex = "0123456789ABCDEF"
		builder.WriteByte('%')
		builder.WriteByte(hex[b>>4])
		builder.WriteByte(hex[b&0xf])
	}
	return builder.String()
}

// RequireAbsentHomes is the source reader's precondition: both homes absolute,
// absent even as dangling symlinks, and every existing ancestor a real directory.
func RequireAbsentHomes(homes Homes) error {
	for _, home := range []string{homes.Rustup, homes.Cargo} {
		if !filepath.IsAbs(home) {
			return refuse("refusing a relative install home: %s", home)
		}
		if _, err := os.Lstat(home); err == nil {
			return refuse("refusing to install over an existing path: %s", home)
		} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			// ENOTDIR means an ancestor is a file: absent as a path, and the
			// ancestor walk below names that file.
			return adopt(err)
		}
		if err := requireRealAncestors(home); err != nil {
			return err
		}
	}
	return nil
}

func requireRealAncestors(path string) error {
	for ancestor := filepath.Dir(path); ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Lstat(ancestor)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return refuse("refusing a symlinked install-home ancestor: %s", ancestor)
			}
			if !info.IsDir() {
				return refuse("refusing a non-directory install-home ancestor: %s", ancestor)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return adopt(err)
		}
		if ancestor == filepath.Dir(ancestor) {
			return nil
		}
	}
}

// removeCreatedHomes removes only homes this invocation created, in reverse
// order, best effort: a removal failure must not replace the refusal being
// handled.
func removeCreatedHomes(created []string) {
	for index := len(created) - 1; index >= 0; index-- {
		_ = os.RemoveAll(created[index])
	}
}
