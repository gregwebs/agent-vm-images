package opencode

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/installstatus"
	"github.com/gregwebs/agent-vm-images/internal/toolaccess"
)

// VerifyOptions is one installed-command gate invocation.
type VerifyOptions struct {
	// Prefix is the installer's $HOME; the command lives beneath it.
	Prefix string
	// Slot is the layer's requested `v` version. It is the assertion, not the exit
	// code: the report must name it exactly.
	Slot      string
	StatusDir string
	Access    toolaccess.Environment
	Out, Err  io.Writer
}

// versionBound is the reviewed `opencode --version` bound. A variable only so a
// test can drive it.
var versionBound = 60 * time.Second

const (
	// reportLines is how much of the child's stderr the gate reprinted.
	reportLines = 10
	// recordName is the record this gate owns.
	recordName = "opencode"
	// recordInstalled is written only after every check passed, so a failed rerun
	// cannot leave a stale success behind.
	recordInstalled = "installed\n"
	// slotRefusal is the predecessor's `${AGENT_VM_VERSION_OPENCODE:?…}` text. An
	// unset slot is a missing required argument: usage, status 2.
	slotRefusal = "verify-opencode.sh needs AGENT_VM_VERSION_OPENCODE"
)

// stderrVersionPattern is the report form that may not also appear on stderr:
// either old and new banner contradicts the exact stdout report. The `m` flag is
// the reviewed `grep -E` line semantics -- a bare `^`/`$` would only see the whole
// file, and every captured diagnostic ends with a newline.
var stderrVersionPattern = regexp.MustCompile(`(?m)^(opencode v|v)?[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*$`)

// Verify is the installed-command gate for the opencode layer.
//
// It is reached after install-opencode.sh: a hard failure already aborted the
// build, and a softened transport failure exited 0 having left an
// `absent-transport CODE` record and no command. The gate accepts that degraded
// state only when the record is present; otherwise a missing command is a hard
// failure regardless of AGENT_INSTALL_SOFT_FAIL. On a real install it runs a
// bounded, status-checked `opencode --version`, requires the exact report, audits
// the command, and only then records `installed` -- the selection label is not
// health.
func Verify(ctx context.Context, options VerifyOptions) (failure error) {
	if options.Slot == "" {
		// The entry point's `${:?}` refuses this before the program runs; a direct
		// caller gets the same status and the same text, because an unset required
		// variable is a usage error under the shell that ran the predecessor.
		return &Usage{Text: slotRefusal}
	}
	command := filepath.Join(options.Prefix, ".opencode", "bin", "opencode")
	// Path state via lstat: only a genuinely nonexistent command may be waved away
	// by an absence record. A dangling symlink, a present-but-unusable file or a
	// permission error on an ancestor is a contract violation, not an absence.
	switch state := installstatus.ClassifyPath(command); state {
	case installstatus.Absent:
		record, err := installstatus.ReadRecord(filepath.Join(options.StatusDir, recordName))
		if _, isAbsent := record.Code(); err != nil || !isAbsent {
			return refusal("  opencode: command missing with no valid absence record -- hard failure")
		}
		// The record's own bytes are what the build log quotes, as `cat "$status"` did.
		say(options.Out, "  opencode: ABSENT (%s); raw developer build only\n", record.Text())
		return nil
	case installstatus.Present:
	case installstatus.Indeterminate:
		return refusalf("  opencode: cannot stat %s (permission/loop) -- hard failure", command)
	}

	watcher, stopWatching := watchInterrupts()
	defer stopWatching()
	abort, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-watcher.done
		cancel()
	}()
	// The status a signal carries outranks the run's own verdict and is applied
	// after the scratch cleanup below, as the retired EXIT trap reported 130/143/129.
	defer func() { failure = interruptedOr(watcher, failure) }()

	work, err := os.MkdirTemp("", "opencode-verify-")
	if err != nil {
		return refusal("  opencode: cannot create invocation scratch: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(work) }()
	stdoutFile := filepath.Join(work, "out")
	stderrFile := filepath.Join(work, "err")

	outcome := runReport(abort, reportSpec{
		argv:       []string{command, "--version"},
		bound:      versionBound,
		stdoutFile: stdoutFile,
		stderrFile: stderrFile,
	})
	if outcome.start != nil {
		return refusal("  opencode: cannot run " + command + ": " + outcome.start.Error())
	}
	if outcome.status != 0 {
		say(options.Err, "  opencode: --version exited %d; refusing to accept the report\n", outcome.status)
		printFilePrefix(options.Err, stderrFile, reportLines)
		return &Silent{Status: 1}
	}
	report, _ := os.ReadFile(stdoutFile)
	diagnostics, _ := os.ReadFile(stderrFile)
	// The report is exact bytes; reject non-printable bytes, because a NUL would be
	// erased by command substitution and canonicalise a corrupted report.
	if !printable(report) || !printable(diagnostics) {
		return refusal("  opencode: --version report contains non-printable bytes -- hard failure")
	}
	// Both old and new report forms on stderr contradict the exact stdout report.
	if stderrVersionPattern.MatchString(string(diagnostics)) {
		return refusal("  opencode: version report on stderr -- hard failure")
	}
	if string(report) != "opencode "+options.Slot+"\n" {
		return refusalf("  opencode: report does not match %s exactly", options.Slot)
	}
	say(options.Out, "  opencode: %s (pinned)\n", options.Slot)

	// The audit target is the bare command name, not the path this gate derived:
	// what the runtime runs is the first `opencode` on PATH, and a same-version copy
	// there is a different program.
	if err := audit(gateStreams{Access: options.Access, Out: options.Out, Err: options.Err},
		"opencode", toolaccess.Executable, false); err != nil {
		return err
	}
	if err := os.MkdirAll(options.StatusDir, 0o755); err != nil {
		return refusal("  opencode: cannot write the install record: " + err.Error())
	}
	if err := writeFile(filepath.Join(options.StatusDir, recordName), recordInstalled); err != nil {
		return refusal("  opencode: cannot write the install record: " + err.Error())
	}
	return nil
}
