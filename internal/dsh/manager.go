package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// ManagerPolicy is the argv every manager call carries, in the order the reviewed
// commands state it. A caller pnpmfile or an ambient registry must never influence
// even `--version`, so the identity check is not exempt.
var (
	identityFlags = []string{
		"--version",
		"--pm-on-fail=error",
		"--config.update-notifier=false",
		"--ignore-pnpmfile",
		"--registry=https://registry.npmjs.org/",
	}
	policyFlags = []string{
		"--config.minimum-release-age=1440",
		"--config.minimum-release-age-strict=true",
		"--config.minimum-release-age-ignore-missing-time=false",
		"--config.trust-policy=no-downgrade",
		"--ignore-scripts",
		"--ignore-pnpmfile",
		"--pm-on-fail=error",
		"--config.update-notifier=false",
		"--registry=https://registry.npmjs.org/",
		"--package-import-method=copy",
	}
	// frozenInstall is the production install: the reviewed four inputs, resolved by
	// the committed lock, with no registry metadata consultation.
	frozenInstall = append([]string{"install", "--prod", "--frozen-lockfile"}, policyFlags...)
	// stagedResolution is the lock-only resolution of a changed selection. It is
	// deliberately unseeded: a seeded resolver keeps every locked version together
	// with the registry metadata it sees now, so upstream could turn a retained
	// record into a refusal no selection can clear (ADR 0003).
	stagedResolution = append([]string{"install", "--lockfile-only", "--no-frozen-lockfile"}, policyFlags...)
)

// withStore binds the policy flags to this invocation's private store, which is what
// keeps a caller or image cache out of the resolution entirely.
func withStore(flags []string, work string) []string {
	return append(append([]string{}, flags...), "--store-dir", filepath.Join(work, "store"))
}

// managerPolicy is the five policy defaults, exported as the manager's environment.
// They are explicit rather than inherited so a caller's config cannot weaken them.
var managerPolicy = []string{
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440",
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=true",
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=false",
	"PNPM_CONFIG_TRUST_POLICY=no-downgrade",
	"PNPM_CONFIG_IGNORE_SCRIPTS=true",
}

// The two bounds the reviewed shell runners applied, kept here as the values the Go
// side uses when it runs a child itself.
const (
	identityTimeout = 20 * time.Second
	killAfter       = 5 * time.Second
	reportTimeout   = 20 * time.Second
	runtimeTimeout  = 60 * time.Second
	webTimeout      = 70 * time.Second
)

// runBounded runs one child with a deadline and a TERM-to-KILL escalation, the way
// `timeout --kill-after=5 N` did.
func runBounded(ctx context.Context, limit time.Duration, directory string, environment []string, stdout, stderr io.Writer, argv ...string) error {
	deadline, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	command := exec.CommandContext(deadline, argv[0], argv[1:]...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = killAfter
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return endSession(command) }
	return command.Run()
}

// processEnvironment is this process's environment with the redirect applied. The
// manager never sees the caller's HOME or XDG, so its caches are invocation-owned.
func processEnvironment(work string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR":
			continue
		}
		// An ambient policy value would be the weaker default the five reviewed ones
		// exist to replace, so it is replaced rather than overridden later.
		if strings.HasPrefix(key, "PNPM_CONFIG_") {
			continue
		}
		environment = append(environment, entry)
	}
	environment = append(environment, managerPolicy...)
	return append(environment,
		"HOME="+filepath.Join(work, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(work, "config"),
		"XDG_CACHE_HOME="+filepath.Join(work, "cache"),
		"XDG_DATA_HOME="+filepath.Join(work, "data"),
		"XDG_STATE_HOME="+filepath.Join(work, "state"),
		"TMPDIR="+filepath.Join(work, "tmp"),
	)
}

// privateState creates the private HOME/XDG/TMPDIR tree the manager runs with.
func privateState(work string) error {
	for _, name := range []string{"home", "config", "cache", "data", "state", "tmp"} {
		if err := os.MkdirAll(filepath.Join(work, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// managerIdentity runs the identity check and returns the manager's own report,
// which the caller compares with the base pin. The policy flags are part of the
// check, not only of the resolution.
func managerIdentity(ctx context.Context, work string, manager []string) (string, error) {
	var stdout, stderr strings.Builder
	err := runBounded(ctx, identityTimeout, work, processEnvironment(work), &stdout, &stderr,
		append(append([]string{}, manager...), identityFlags...)...)
	if err != nil {
		return "", err
	}
	return stdout.String(), nil
}

// runPnpm runs one manager call through the composed envelope, which owns the
// bounded run, the NDJSON diagnostic transport and the credential redaction. A
// caller that reimplemented that would own a second copy of it.
func runPnpm(ctx context.Context, contract, resultFile, directory string, environment []string, manager []string, flags []string) error {
	argv := append([]string{"sh", filepath.Join(contract, "run-pnpm.sh"), resultFile, "--"}, manager...)
	argv = append(argv, flags...)
	var stderr strings.Builder
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = environment
	command.Stderr = &stderr
	command.Stdout = io.Discard
	command.WaitDelay = killAfter
	// A cancelled run takes the manager's whole session down: run-pnpm.sh ends with
	// its own TERM trap, and a manager that outlives its envelope would rebuild the
	// payload after the caller removed it.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return endSession(command) }
	if err := command.Run(); err != nil {
		return &ChildFailure{Status: exitStatusOf(err), Stderr: stderr.String()}
	}
	return nil
}

// childCommand runs a child with the caller's streams, in its own group so a
// cancelling caller can take the whole session down.
func childCommand(ctx context.Context, directory string, argv []string) *exec.Cmd {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = directory
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Stdin = os.Stdin
	command.WaitDelay = killAfter
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return endSession(command) }
	return command
}

// cancelGrace is how long a cancelled child's session gets to unwind on TERM before it
// is killed.
const cancelGrace = 3 * time.Second

// endSession stops a child and everything it started.
//
// TERM comes first, and that order is load-bearing. The envelope runs the manager
// through `timeout`, which puts the manager in a group of its own, so a KILL of the
// envelope's group alone leaves a resolver running against the payload with nobody to
// stop it. TERM reaches `timeout`, which forwards it to the manager's group; the KILL
// is only the backstop for a child that ignores it.
func endSession(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	pgid := command.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	until := time.Now().Add(cancelGrace)
	for time.Now().Before(until) {
		if err := syscall.Kill(-pgid, 0); err != nil && errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(pollInterval)
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// dshManifest is the reviewed input's own assertions: the manager selection and the
// exact dsh dependency the graph must resolve.
type dshManifest struct {
	PackageManager string            `json:"packageManager"`
	Dependencies   map[string]string `json:"dependencies"`
}

func readManifest(path string) (dshManifest, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return dshManifest{}, err
	}
	var manifest dshManifest
	if err := json.Unmarshal(source, &manifest); err != nil {
		return dshManifest{}, err
	}
	return manifest, nil
}

// publishPermissions is `chmod -R a+rX`: the payload is readable by the read-only
// runtime UIDs, and executable exactly where it already was.
func publishPermissions(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		mode |= 0o444
		if info.IsDir() || mode&0o111 != 0 {
			mode |= 0o111
		}
		if mode == info.Mode().Perm() {
			return nil
		}
		return os.Chmod(path, mode)
	})
}

// exactVersionPattern is the reviewed selection grammar: canonical semver only.
var exactVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// exactVersion is the canonical-semver test: a multi-line range or a leading-zero
// component must not reach the manager as if it were an exact selection.
func exactVersion(text string) bool {
	if strings.Contains(text, "\n") {
		return false
	}
	return exactVersionPattern.MatchString(text)
}

// interruption turns a signal into the status the owning shell exited with, so a
// caller's own trap sees 130/143/129 instead of a generic failure.
type interruption struct {
	status int
	done   chan struct{}
}

// watchInterrupts reports the three signals the dsh hooks handled.
func watchInterrupts() (*interruption, func()) {
	watcher := &interruption{status: 0, done: make(chan struct{})}
	signals := make(chan os.Signal, 3)
	signalNotify(signals)
	go func() {
		defer close(watcher.done)
		for received := range signals {
			switch received {
			case syscall.SIGINT:
				watcher.status = 130
			case syscall.SIGTERM:
				watcher.status = 143
			case syscall.SIGHUP:
				watcher.status = 129
			default:
				continue
			}
			return
		}
	}()
	return watcher, func() { stopSignals(signals) }
}

// exited reports the signal status once one arrived.
func (i *interruption) exited() (int, bool) {
	select {
	case <-i.done:
		if i.status == 0 {
			return 0, false
		}
		return i.status, true
	default:
		return 0, false
	}
}

// dshVersionSlot is one exact-or-empty version argument. Empty keeps the committed
// selection.
func dshVersionSlot(text string) error {
	if text == "" {
		return nil
	}
	if !exactVersion(text) {
		return refusalf("not an exact version: '%s'", text)
	}
	return nil
}

// basePinSupport is the one base manager selection the dsh layer supports: the base
// image owns the manager, so a dsh layer never names another.
const basePinSupport = "11.11.0"

// redirectHome performs the redirect the retired hooks did by exporting HOME, the
// four XDG roots and TMPDIR: from here on every child sees this invocation's private
// state, so no caller or image cache is read or written. The returned function
// restores the caller's values, which only a test that outlives one invocation needs.
func redirectHome(work string) func() {
	values := map[string]string{
		"HOME":            filepath.Join(work, "home"),
		"XDG_CONFIG_HOME": filepath.Join(work, "config"),
		"XDG_CACHE_HOME":  filepath.Join(work, "cache"),
		"XDG_DATA_HOME":   filepath.Join(work, "data"),
		"XDG_STATE_HOME":  filepath.Join(work, "state"),
		"TMPDIR":          filepath.Join(work, "tmp"),
	}
	saved := make(map[string]string, len(values))
	present := make(map[string]bool, len(values))
	for key := range values {
		saved[key], present[key] = os.LookupEnv(key)
		_ = os.Setenv(key, values[key])
	}
	return func() {
		for key, value := range saved {
			if present[key] {
				_ = os.Setenv(key, value)
				continue
			}
			_ = os.Unsetenv(key)
		}
	}
}
