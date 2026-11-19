package pnpm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/toolaccess"
)

// Frozen is a diagnostic a consumer parses, so it is printed without this
// program's own prefix and cannot be reworded.
type Frozen struct{ Text string }

func (f *Frozen) Error() string { return f.Text }

// ReportTimeout bounds each manager call the certification makes. The image's own
// runner (images/recipe-contract/run-report.sh) allowed 60 seconds for the report;
// a manager that hangs while being certified must fail the build instead of
// holding it open.
const ReportTimeout = 60 * time.Second

// killGrace is run-report.sh's `timeout --kill-after=5`: a manager that ignores
// TERM is killed five seconds later instead of holding the build open.
const killGrace = 5 * time.Second

// The runner's own statuses, which the shell entry point also produced: GNU
// timeout reports 124 once it had to stop the child, and a command the runner
// could not execute at all is 126 (present but unusable) or 127 (absent).
const (
	timeoutStatus       = 124
	cannotExecuteStatus = 126
	notFoundStatus      = 127
)

// ChildFailure is a program the certification ran that failed on its own. Parity
// here is the status, not just the wording: `set -e` in the shell entry point
// exited with the child's status (or 128+N after a signal), so a manager that
// exits 42 still exits 42 — the only mapped status is the report runner's
// documented 75-to-1, which keeps a tool that happens to exit 75 from looking like
// a classified transport failure.
type ChildFailure struct {
	Status int
	// Stderr is what the caller would have seen on the build log. It is empty when
	// the predecessor captured the child's stderr to a file instead of inheriting
	// it, so this does not invent output the old entry point never printed.
	Stderr string
}

func (f *ChildFailure) Error() string {
	return fmt.Sprintf("exit status %d", f.Status)
}

// DefaultKey is one policy default the launcher exports and the installed manager
// must therefore report.
type DefaultKey struct{ Key, Value string }

// Defaults are the five reviewed defaults, in the order the certification reports
// them: the first mismatch names the key a reader needs.
var Defaults = []DefaultKey{
	{Key: "minimum-release-age", Value: "1440"},
	{Key: "minimum-release-age-strict", Value: "true"},
	{Key: "minimum-release-age-ignore-missing-time", Value: "false"},
	{Key: "trust-policy", Value: "no-downgrade"},
	{Key: "ignore-scripts", Value: "true"},
}

// DefaultVariables are the manager's own overrides for those defaults. The
// certification clears them, so an inherited value cannot stand in for what the
// image actually ships.
var DefaultVariables = []string{
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE",
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT",
	"PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME",
	"PNPM_CONFIG_TRUST_POLICY",
	"PNPM_CONFIG_IGNORE_SCRIPTS",
	"XDG_CONFIG_HOME",
}

// CertifyOptions is one certification run.
type CertifyOptions struct {
	Target Target
	Source string
	// Out receives the per-target access lines, which are the tool-access verb's
	// own output and read the same way in a build log.
	Out   io.Writer
	Audit toolaccess.Environment
}

// Certify proves the installed distribution answers as the reviewed one: the
// exact report, the audited exec targets and the five manager defaults.
//
// Two diagnostics are frozen because a consumer reads them. `pnpm: exact report
// mismatch` and `pnpm: default <key> mismatch: <got>` were the shell entry point's
// own lines (images/pnpm/verify-pnpm.sh), and the base image's certification path
// -- images/standard/verify-standard.sh, driven by script/test/
// standard-certification.py -- watched that entry point. The fixtures match only
// the exit status, so the wording is kept for the build log and for the documents
// that quote it.
func Certify(ctx context.Context, options CertifyOptions) error {
	source := options.Source
	if source == "" {
		source = SourceDir()
	}
	pin, err := LoadPin(filepath.Join(source, "pin.json"))
	if err != nil {
		return err
	}
	if err := certifyReport(ctx, pin.Version); err != nil {
		return err
	}
	if err := certifyAccess(options); err != nil {
		return err
	}
	return certifyDefaults(ctx)
}

// certifyReport runs the published launcher's `--version` under a private
// HOME/XDG, so a config in the caller's home cannot answer for the image.
func certifyReport(ctx context.Context, want string) error {
	executable, err := resolveReported("pnpm")
	if err != nil {
		return err
	}
	scratch, err := os.MkdirTemp("", "pnpm-verify-")
	if err != nil {
		return refuseCause(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	home := filepath.Join(scratch, "home")
	directories := map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local/share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local/state"),
	}
	overrides := make(map[string]*string, len(directories))
	for name, path := range directories {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return refuseCause(err)
		}
		overrides[name] = textValue(path)
	}
	result := runChild(ctx, childSpec{
		path:        executable,
		arguments:   []string{"--version"},
		environment: withEnvironment(os.Environ(), overrides),
		// run-report.sh captured the child's streams to files of its own, so its
		// stderr never reached the build log; only the runner's exec diagnostic did.
		stderrReported: false,
		mapSeventyFive: true,
	})
	if result.status != 0 {
		return &ChildFailure{Status: result.status, Stderr: result.diagnostic}
	}
	if trimNewlines(result.stdout) != want {
		return &Frozen{Text: "pnpm: exact report mismatch"}
	}
	return nil
}

// certifyAccess audits the commands and the two entrypoints the launcher execs
// with the same reader the recipe-contract tool-access verb uses: a launcher audit
// alone sees only /bin/sh, and an unusable interpreter is invisible to it. The
// violation text is that verb's, because a reader has seen it before.
func certifyAccess(options CertifyOptions) error {
	out := options.Out
	if out == nil {
		out = io.Discard
	}
	auditor := toolaccess.NewAuditor(options.Audit)
	targets := []string{
		"pnpm", "pnx", "pnpx", options.Target.Node,
		filepath.Join(options.Target.Prefix, "bin/pnpm.mjs"),
		filepath.Join(options.Target.Prefix, "bin/pnpx.mjs"),
	}
	for _, target := range targets {
		final, err := auditor.Audit(toolaccess.Target(target), toolaccess.Executable)
		if err != nil {
			var violation *toolaccess.Violation
			if !errors.As(err, &violation) {
				return refuseCause(err)
			}
			return &Frozen{Text: "tool-access: " + violation.Path + ": " + violation.Reason()}
		}
		if _, err := fmt.Fprintf(out, "tool-access: ok %s\n", final); err != nil {
			return refuseCause(err)
		}
	}
	return nil
}

// certifyDefaults asks the installed manager for each reviewed default with the
// override variables cleared, so the answer comes from the launcher the image
// ships rather than from the caller's environment.
func certifyDefaults(ctx context.Context) error {
	scratch, err := os.MkdirTemp("", "pnpm-defaults-")
	if err != nil {
		return refuseCause(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	home := filepath.Join(scratch, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return refuseCause(err)
	}
	overrides := map[string]*string{"HOME": textValue(home)}
	for _, name := range DefaultVariables {
		overrides[name] = nil
	}
	environment := withEnvironment(os.Environ(), overrides)
	for _, entry := range Defaults {
		got, err := defaultValue(ctx, environment, entry.Key)
		if err != nil {
			return err
		}
		if got != entry.Value {
			return &Frozen{Text: "pnpm: default " + entry.Key + " mismatch: " + got}
		}
	}
	return nil
}

// childSpec is one program the certification runs, and what the shell entry point
// used to do with its outcome.
type childSpec struct {
	path        string
	arguments   []string
	environment []string
	// stderrReported says whether the child inherited the build log's stderr. The
	// report runner redirected it to a file; the defaults loop inherited it.
	stderrReported bool
	// mapSeventyFive is run-report.sh's rule for the report: 75 becomes 1.
	mapSeventyFive bool
}

// childResult is one finished child: its streams and the status the caller saw.
type childResult struct {
	stdout string
	stderr string
	// diagnostic is what the runner itself printed about a child it could not
	// start, which run-report.sh also left on the build log.
	diagnostic string
	status     int
}

// runChild runs one manager call the way run-report.sh did: bounded by the report
// timeout, TERM then KILL after the grace, and reporting the status the shell
// entry point would have exited with.
func runChild(ctx context.Context, spec childSpec) childResult {
	run, cancel := context.WithTimeout(ctx, ReportTimeout)
	defer cancel()
	command := exec.CommandContext(run, spec.path, spec.arguments...)
	command.Env = spec.environment
	// Go's Cancel sends SIGKILL immediately; the reviewed runner gives the child
	// TERM and five seconds first, so the child can clean up its own scratch.
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = killGrace
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := childResult{stdout: stdout.String(), stderr: stderr.String()}
	if err == nil {
		return result
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		result.diagnostic = spec.path + ": " + err.Error()
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			result.status = notFoundStatus
		} else {
			result.status = cannotExecuteStatus
		}
		return result
	}
	if run.Err() != nil {
		result.status = timeoutStatus
		return result
	}
	if wait, ok := exit.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
		result.status = 128 + int(wait.Signal())
		return result
	}
	result.status = exit.ExitCode()
	if spec.mapSeventyFive && result.status == 75 {
		result.status = 1
	}
	if !spec.stderrReported {
		// The predecessor's capture file swallowed the child's own output.
		result.stderr = ""
	}
	return result
}

func defaultValue(ctx context.Context, environment []string, key string) (string, error) {
	result := runChild(ctx, childSpec{
		path:           "pnpm",
		arguments:      []string{"config", "get", key},
		environment:    environment,
		stderrReported: true,
	})
	if result.status != 0 {
		diagnostic := result.stderr
		if result.diagnostic != "" {
			diagnostic = result.diagnostic
		}
		return "", &ChildFailure{Status: result.status, Stderr: diagnostic}
	}
	return trimNewlines(result.stdout), nil
}

// resolveReported is the runner's own lookup: the first PATH candidate that exists
// is the one run, and a dangling link is reported rather than skipped for a later
// executable.
func resolveReported(name string) (string, error) {
	candidate, found := firstCandidate(os.Getenv("PATH"), name)
	if !found {
		return "", refuseText(name + " is not on PATH")
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", refuseCause(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if _, err := os.Stat(candidate); err != nil {
			return "", refuseText(candidate + " is a dangling symlink")
		}
	}
	return candidate, nil
}

// trimNewlines is what a shell's command substitution did to the reported line.
func trimNewlines(text string) string {
	return strings.TrimRight(text, "\n")
}
