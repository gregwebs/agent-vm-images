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

const (
	pollInterval = 20 * time.Millisecond

	// notFoundStatus and cannotExecuteStatus are what the shell reported for a
	// program it could not find or could not execute; `set -eu` propagated them.
	notFoundStatus      = 127
	cannotExecuteStatus = 126
	genericFailure      = 1

	// classifiedTransportStatus is the one status `run-report.sh` remapped. It is
	// kept because the reviewed envelope's rule was that a tool which happens to
	// exit 75 can never look like a classified download failure.
	classifiedTransportStatus = 75
)

// The reviewed bounds. They are variables only so a test can drive them; nothing
// outside this package can set one, so no caller can lengthen a gate.
var (
	// identityBound is `timeout --kill-after=5 20` around the manager identity
	// check, which is the one manager call the reviewed shell bounded itself: the
	// composed `run-pnpm.sh` does not run it.
	identityBound = 20 * time.Second
	// killGrace is the reviewed `--kill-after=5`: the grace a TERMed child gets
	// before it is killed.
	killGrace = 5 * time.Second
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
	// bound is the child's own deadline. Zero leaves the bound to the composed
	// envelope the child runs under.
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
// before it is killed. It is derived from the reviewed backstop rather than
// chosen: `run-pnpm.sh` runs the manager under `timeout --kill-after=5`, so the
// envelope's TERM handling fires five seconds after the TERM it forwards. Waiting
// less would return while a resolver was still running, and the caller's cleanup
// would remove the scratch underneath it.
func cancelGrace() time.Duration { return killGrace + time.Second }

// signalGroup sends one signal to a process group. It is a variable so the teardown
// table can drive a refusal the kernel will not produce on demand.
var signalGroup = func(pgid int, signal syscall.Signal) error {
	return syscall.Kill(-pgid, signal)
}

// endSession stops a child and everything it started: TERM to the session, then
// KILL once the grace is spent, and only then does it report success.
//
// The TERM-first order is load-bearing for `run-pnpm.sh`, whose own TERM trap has to
// run before the caller removes the scratch the resolver is using.
//
// Success is only ever *proof*: `kill -0` on the group answered ESRCH. Anything else
// -- a member that exists, or EPERM, which is the kernel refusing to say -- is "not
// proven empty" and escalates, and if the KILL cannot prove it either the call
// returns the error. A teardown that cannot show the session is gone must fail
// loudly: passing there is how a resolver outlives the removal of the tree it is
// writing to.
//
// The caller has to be reaping the leader while this runs. Go's `CommandContext`
// does exactly that -- `Wait` is reaping while `Cancel` runs -- and a reaped leader
// stops being a group member, which is what turns the common case into an honest
// ESRCH. A caller that only blocks on endSession leaves the leader a zombie and gets
// the refusal it deserves. `TestEndSessionTermsBeforeItKills` is that difference on
// the platform that showed it (Darwin answers EPERM for a group of unwaited
// zombies).
func endSession(command *exec.Cmd) error {
	if command.Process == nil {
		return nil
	}
	pgid := command.Process.Pid
	_ = signalGroup(pgid, syscall.SIGTERM)
	if empty := waitForEmptyGroup(pgid); empty {
		return nil
	}
	_ = signalGroup(pgid, syscall.SIGKILL)
	if empty := waitForEmptyGroup(pgid); empty {
		return nil
	}
	return fmt.Errorf("process group %d is not provably gone after TERM and KILL", pgid)
}

// waitForEmptyGroup polls until the group has no members left, and reports whether
// it proved that before the grace ran out.
func waitForEmptyGroup(pgid int) bool {
	until := time.Now().Add(cancelGrace())
	for time.Now().Before(until) {
		if errors.Is(signalGroup(pgid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// resolveCommand is the composed envelope's own program resolution, kept because
// what it does *not* do is the rule: the first PATH candidate that exists is the one
// run, whether or not it can be executed. Go's `exec.LookPath` would skip a
// present-but-unusable candidate and run a later one instead, so a gate could
// certify a command whose first PATH entry the runtime cannot start. `run-report.sh`
// refused that case and so does this, with the same status: the child never starts.
func resolveCommand(name string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		return name, nil
	}
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, name)
		// lstat, not stat: a symlink whose target is gone is still a candidate, and
		// the envelope reported it rather than searching on.
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s not found on PATH", name)
}

// reportSpec is one bounded child whose two streams are published to files.
type reportSpec struct {
	argv       []string
	bound      time.Duration
	stdoutFile string
	stderrFile string
}

// reportOutcome is a finished report run: the status the caller saw, or the
// reason it never started.
type reportOutcome struct {
	status int
	// start is set when the program could not be started at all. An absent
	// interpreter is a broken image, and the caller reports it at status 1.
	start error
}

// runReport runs one bounded child with a private HOME and both streams captured
// in the caller's files, which is what the composed `run-report.sh` did.
//
// This program owns the run instead of execing that envelope. The envelope runs
// its child through `timeout`, and GNU timeout puts the managed command in a
// process group of its own, so a TERM to the envelope's group kills `timeout` and
// leaves the managed child running with nobody left to forward anything -- which
// is exactly what this port's signal test observed. Owning the run keeps the child
// in this session, so `endSession` reaches it and KILL follows when TERM is
// ignored. The rest of the envelope's contract is kept: both files always exist,
// the child gets a private HOME and XDG tree, and 75 becomes 1.
func runReport(ctx context.Context, spec reportSpec) reportOutcome {
	stdout, err := os.Create(spec.stdoutFile)
	if err != nil {
		return reportOutcome{start: err}
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := os.Create(spec.stderrFile)
	if err != nil {
		return reportOutcome{start: err}
	}
	defer func() { _ = stderr.Close() }()
	home, err := os.MkdirTemp("", "opencode-report-")
	if err != nil {
		return reportOutcome{start: err}
	}
	defer func() { _ = os.RemoveAll(home) }()
	if err := createReportState(home); err != nil {
		return reportOutcome{start: err}
	}

	program, err := resolveCommand(spec.argv[0])
	if err != nil {
		return reportOutcome{start: err}
	}
	run := ctx
	if spec.bound > 0 {
		var cancel context.CancelFunc
		run, cancel = context.WithTimeout(ctx, spec.bound)
		defer cancel()
	}
	command := exec.CommandContext(run, program, spec.argv[1:]...) //nolint:gosec // the argv is this program's own reviewed call.
	command.Env = reportEnvironment(home)
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = killGrace
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return endSession(command) }
	if err := command.Start(); err != nil {
		return reportOutcome{start: err}
	}
	if err := command.Wait(); err != nil {
		status := childStatus(err)
		if status == classifiedTransportStatus {
			status = genericFailure
		}
		return reportOutcome{status: status}
	}
	return reportOutcome{}
}

// createReportState is the envelope's `mkdir -p` of the private HOME and the four
// XDG roots.
func createReportState(home string) error {
	for _, directory := range []string{
		home,
		filepath.Join(home, ".config"),
		filepath.Join(home, ".cache"),
		filepath.Join(home, ".local", "share"),
		filepath.Join(home, ".local", "state"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// reportEnvironment is this process's environment with the report's own HOME and
// XDG roots applied, as the envelope exported them.
func reportEnvironment(home string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME":
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"),
		"XDG_DATA_HOME="+filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(home, ".local", "state"),
	)
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
