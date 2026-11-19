package pnpm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	run, cancel := context.WithTimeout(ctx, ReportTimeout)
	defer cancel()
	command := exec.CommandContext(run, executable, "--version") //nolint:gosec // the published launcher, one fixed argument.
	command.Env = withEnvironment(os.Environ(), overrides)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return refuse("pnpm --version failed: %s: %s", err, strings.TrimSpace(stderr.String()))
	}
	if trimNewlines(stdout.String()) != want {
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

func defaultValue(ctx context.Context, environment []string, key string) (string, error) {
	run, cancel := context.WithTimeout(ctx, ReportTimeout)
	defer cancel()
	command := exec.CommandContext(run, "pnpm", "config", "get", key) //nolint:gosec // the reviewed default's own key.
	command.Env = environment
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", refuse("pnpm config get %s failed: %s: %s", key, err, strings.TrimSpace(stderr.String()))
	}
	return trimNewlines(stdout.String()), nil
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
