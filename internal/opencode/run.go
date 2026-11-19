package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/toolaccess"
)

// The reviewed shell bounded the manager identity check with
// `timeout --kill-after=5 20`; every other child was bounded by its own composed
// envelope (`run-report.sh`, `run-pnpm.sh`), which is why most calls here pass no
// bound of their own.
const (
	identityBound = 20 * time.Second
	killGrace     = 5 * time.Second
	pollInterval  = 20 * time.Millisecond

	// notFoundStatus and cannotExecuteStatus are the shell's statuses for a child
	// it could not start, kept because `set -eu` propagated them verbatim.
	notFoundStatus      = 127
	cannotExecuteStatus = 126
	genericFailure      = 1
)

// childSpec is one program this driver runs and the streams the predecessor gave
// it.
type childSpec struct {
	argv []string
	// directory is the child's working directory; empty keeps this process's.
	directory string
	// capture takes the child's stdout into a buffer instead of a stream, which is
	// what the predecessor's `$(...)` and `>file` spellings did.
	capture bool
	// stdout and stderr are where the child's streams go when they are not
	// captured. Nil discards them, which is what `>/dev/null` did.
	stdout io.Writer
	stderr io.Writer
	// bound is the child's own deadline. Zero leaves the bound to the child's
	// composed envelope, exactly as the reviewed hooks relied on it.
	bound time.Duration
}

// runChild runs one child in its own session and reports its status the way the
// shell propagated it: the child's own code, 128+N for a signal, or 126/127 for a
// program the shell could not start.
func runChild(ctx context.Context, spec childSpec) (string, int) {
	run := ctx
	if spec.bound > 0 {
		var cancel context.CancelFunc
		run, cancel = context.WithTimeout(ctx, spec.bound)
		defer cancel()
	}
	var captured strings.Builder
	stdout := spec.stdout
	if spec.capture {
		stdout = &captured
	}
	command := exec.CommandContext(run, spec.argv[0], spec.argv[1:]...)
	command.Dir = spec.directory
	command.Stdout = stdout
	command.Stderr = spec.stderr
	command.WaitDelay = killGrace
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Go's default cancellation is SIGKILL. The reviewed envelopes give the child
	// TERM and five seconds first, and a resolver killed rather than asked leaves
	// its own scratch behind.
	command.Cancel = func() error { return endSession(command) }
	if err := command.Run(); err != nil {
		return captured.String(), childStatus(err)
	}
	return captured.String(), 0
}

// childStatus is the status the shell would have propagated.
func childStatus(err error) int {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return notFoundStatus
		}
		return cannotExecuteStatus
	}
	if wait, ok := exit.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
		return 128 + int(wait.Signal())
	}
	if code := exit.ExitCode(); code >= 0 {
		return code
	}
	return genericFailure
}

// cancelGrace is how long a cancelled child's session gets to unwind on TERM
// before it is killed. It is derived from the envelope's own backstop rather than
// chosen: `run-pnpm.sh` runs the manager under `timeout --kill-after=5`, so the
// envelope's TERM handling fires five seconds after the TERM it forwards. Waiting
// less would return while a resolver was still running, and the caller's cleanup
// would remove the scratch underneath it.
const cancelGrace = killGrace + time.Second

// endSession stops a child and everything it started. TERM comes first: the
// composed envelopes run their own children through `timeout`, which puts them in
// a group of their own, so a KILL of the envelope's group alone would leave a
// resolver running with nobody to stop it.
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

// interruption turns a signal into the status the owning shell exited with, so a
// caller's own trap sees 130/143/129 instead of a generic failure.
type interruption struct {
	status int
	done   chan struct{}
}

// watchInterrupts reports the three signals all three predecessors trapped.
func watchInterrupts() (*interruption, func()) {
	watcher := &interruption{done: make(chan struct{})}
	signals := make(chan os.Signal, 3)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
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
	return watcher, func() {
		signal.Stop(signals)
		close(signals)
	}
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

// gateStreams is what an access audit needs from a gate invocation: the process
// view the resolver reads and the two streams the subcommand wrote to.
type gateStreams struct {
	Access toolaccess.Environment
	Out    io.Writer
	Err    io.Writer
}

// privateState creates the private HOME and XDG tree the manager runs with.
func privateState(work string) error {
	for _, name := range []string{"project", "home", "config", "cache", "data", "state"} {
		if err := os.MkdirAll(filepath.Join(work, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// redirectHome performs the redirect the preparation driver did by exporting HOME
// and the four XDG roots: from here on every child sees this invocation's private
// state, so no caller or image cache is read or written. The returned function
// restores the caller's values, which only a test that outlives one invocation
// needs.
func redirectHome(work string) func() {
	values := map[string]string{
		"HOME":            filepath.Join(work, "home"),
		"XDG_CONFIG_HOME": filepath.Join(work, "config"),
		"XDG_CACHE_HOME":  filepath.Join(work, "cache"),
		"XDG_DATA_HOME":   filepath.Join(work, "data"),
		"XDG_STATE_HOME":  filepath.Join(work, "state"),
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

// refuseRuntimeInjection rejects a Node preload or an injected module path before
// any Node or pnpm process starts: a preloaded module can patch node:crypto and
// subvert the digest comparison the reviewed-bytes checker performs.
func refuseRuntimeInjection() error {
	for _, key := range []string{"NODE_OPTIONS", "NODE_PATH", "BUN_OPTIONS"} {
		if os.Getenv(key) != "" {
			return refusal("manager runtime overrides are unsupported")
		}
	}
	return nil
}

// copyFile keeps the source bytes and mode, which is what `cp` did.
func copyFile(from, to string) error {
	source, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, source, info.Mode().Perm())
}

// writeFile publishes one record. The bytes are the contract, so the mode is fixed
// rather than inherited from the caller's umask.
func writeFile(path, text string) error {
	return os.WriteFile(path, []byte(text), 0o644)
}

// say writes one progress or diagnostic line. A write failure on the caller's own
// stream is reported by the stream layer above -- the CLI turns an unusable stderr
// into the legacy 120 -- so it is not an error this program can act on.
func say(destination io.Writer, format string, args ...any) {
	if destination == nil {
		return
	}
	_, _ = fmt.Fprintf(destination, format, args...)
}

// directoryExists is `[ -d ]`: a directory, reached through any symlink chain.
func directoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// trimCommandSubstitution removes the trailing newlines `$(...)` removed, and
// nothing else: the reviewed comparison rejected a value with leading space.
func trimCommandSubstitution(text string) string {
	return strings.TrimRight(text, "\n")
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

// printFilePrefix reproduces `sed -n '1,10p' FILE`: at most limit lines, each
// terminated, and nothing when the file is unreadable.
func printFilePrefix(destination io.Writer, path string, limit int) {
	source, err := os.ReadFile(path)
	if err != nil {
		return
	}
	printPrefix(destination, string(source), limit)
}

// printPrefix reproduces `sed -n '1,10p'` over captured text: at most limit
// terminated lines, and nothing at all for an empty file.
func printPrefix(destination io.Writer, text string, limit int) {
	if text == "" {
		return
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > limit {
		lines = lines[:limit]
	}
	for _, line := range lines {
		say(destination, "%s\n", line)
	}
}

// printable is the reviewed report's byte rule: tab, newline and printable ASCII,
// nothing else. A NUL would be erased by command substitution and could
// canonicalise a corrupted report.
func printable(data []byte) bool {
	for _, b := range data {
		switch {
		case b == '\t' || b == '\n':
		case b >= 0x20 && b <= 0x7e:
		default:
			return false
		}
	}
	return true
}
