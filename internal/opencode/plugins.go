package opencode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/toolaccess"
)

// PluginsOptions is one installed-plugin gate invocation.
type PluginsOptions struct {
	// Source is the read-only build mount: the payload checks, the probe and the
	// reviewed-bytes checker live beneath it.
	Source string
	// Contract is the composed helper directory the report envelopes run from.
	Contract string
	Prefix   string
	// Claude and Opencode are the two commands the plugin hands work to.
	Claude   string
	Opencode string
	Config   string
	// OpenCodeConfig is the guest's own OPENCODE_CONFIG. A stale `installed` record
	// does not prove the image still selects this config, so the two must agree.
	OpenCodeConfig string
	// Slot is the layer's requested plugin version; empty accepts the committed
	// one.
	Slot      string
	StatusDir string
	Access    toolaccess.Environment
	Out, Err  io.Writer
}

const (
	// pluginRecord is the record this gate owns.
	pluginRecord = "opencode-claude"
	// runtimeSeconds and probeSeconds are the reviewed bounds, in seconds, of the
	// two report envelopes.
	runtimeSeconds = "20"
	probeSeconds   = "90"
	// probeTimeout is the value the probe's own deadline takes, in seconds.
	probeTimeout = "60"
)

// VerifyPlugins is the installed-plugin gate.
//
// A stale record cannot certify activation or the SDK/Claude handoff, so every
// assertion is re-derived here: the reviewed bytes, the guest configuration, the
// payload shape, the real runtime handoff, a live loopback server and the access
// audit. Anything that fails is hard, whatever AGENT_INSTALL_SOFT_FAIL says.
func VerifyPlugins(ctx context.Context, options PluginsOptions) (failure error) {
	// Both guards run before the first Node process, in the predecessor's order: the
	// fixture authorization is not this gate's, but the preload rejection is, and it
	// must precede the checker whose digest comparison a preloaded module could
	// subvert.
	if err := refuseRuntimeInjection(); err != nil {
		return err
	}
	watcher, stopWatching := watchInterrupts()
	defer stopWatching()
	abort, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-watcher.done
		cancel()
	}()
	defer func() { failure = interruptedOr(watcher, failure) }()
	streams := gateStreams{Access: options.Access, Out: options.Out, Err: options.Err}

	work, err := os.MkdirTemp("", "opencode-plugins-")
	if err != nil {
		return refusal("opencode-claude: cannot create invocation scratch: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(work) }()

	// The checker owns the reviewed bytes and the manifest policy; its stderr is the
	// caller's, so this driver adds no verdict on top of it.
	reported, status := runChild(abort, childSpec{
		argv:    []string{"node", filepath.Join(options.Source, "plugins", "check-lock.mjs"), options.Prefix},
		capture: true,
		stderr:  options.Err,
	})
	if status != 0 {
		return &Silent{Status: status}
	}
	selected := trimCommandSubstitution(reported)
	// The layer's slot is the assertion: a different committed selection is a build
	// that installed something other than what it asked for.
	if options.Slot != "" && options.Slot != selected {
		return &Silent{Status: 1}
	}
	if options.OpenCodeConfig != options.Config {
		return refusal("opencode-claude: image config selection missing")
	}
	payload := filepath.Join(options.Source, "check-claude-payload.mjs")
	if _, status := runChild(abort, childSpec{
		argv:   []string{"node", payload, "config", options.Config, options.Prefix},
		stdout: options.Out,
		stderr: options.Err,
	}); status != 0 {
		return &Silent{Status: status}
	}
	runtimeFile := filepath.Join(work, "runtime.json")
	runtimeErr := filepath.Join(work, "runtime.err")
	if !reportEnvelope(abort, options, childSpec{
		argv: []string{"node", filepath.Join(options.Source, "check-claude-runtime.mjs"), options.Prefix, options.Claude},
	}, runtimeSeconds, runtimeFile, runtimeErr) {
		printFile(options.Err, runtimeErr)
		return &Silent{Status: 1}
	}
	// The runtime report is checked against the selection the reviewed bytes named,
	// not against the layer's slot: those two were proven equal above.
	if _, status := runChild(abort, childSpec{
		argv:   []string{"node", payload, "report", runtimeFile, selected},
		stdout: options.Out,
		stderr: options.Err,
	}); status != 0 {
		return &Silent{Status: status}
	}
	evidence := filepath.Join(work, "evidence.json")
	probeErr := filepath.Join(work, "probe.err")
	if !reportEnvelope(abort, options, childSpec{
		argv: []string{"python3", filepath.Join(options.Source, "probe-opencode.py"), "capture",
			"--binary", options.Opencode, "--state-root", work, "--timeout", probeTimeout},
	}, probeSeconds, evidence, probeErr) {
		printFile(options.Err, probeErr)
		return &Silent{Status: 1}
	}
	// The probe's own checker is the second half of its protocol; it reads the
	// evidence rather than the wire, so it is not a second implementation.
	if _, status := runChild(abort, childSpec{
		argv: []string{"python3", filepath.Join(options.Source, "probe-opencode.py"), "check",
			"--evidence", evidence, "--plugin-entry", filepath.Join(options.Prefix, "index.js")},
		stdout: options.Out,
		stderr: options.Err,
	}); status != 0 {
		return &Silent{Status: status}
	}

	// Supply physical and logical regular files; the access auditor checks
	// ancestors. The inventory is the payload checker's own walk, so the audit
	// covers exactly what the runtime would load.
	//
	// Order is load-bearing: every audit runs after the probes, because access is
	// only part of the claim and a certificate written before the runtime ran would
	// be the stale record this gate exists to replace. TestPluginsAuditsRunLast
	// fails if the audits move earlier.
	inventory, status := runChild(abort, childSpec{
		argv:    []string{"node", payload, "files", options.Prefix},
		capture: true,
		stderr:  options.Err,
	})
	if status != 0 {
		return &Silent{Status: status}
	}
	// The predecessor fed this list to `xargs -0`, which ran the audit once with no
	// arguments when the list was empty and so refused. That state is unreachable: a
	// prefix whose inventory is empty has no `index.js`, and the `config` check above
	// asserts that file's bytes.
	for _, file := range strings.Split(inventory, "\x00") {
		if file == "" {
			continue
		}
		if err := audit(streams, file, toolaccess.Content, true); err != nil {
			return err
		}
	}
	if err := audit(streams, options.Config, toolaccess.Content, false); err != nil {
		return err
	}
	for _, command := range []string{options.Claude, options.Opencode} {
		if err := audit(streams, command, toolaccess.Executable, false); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(options.StatusDir, 0o755); err != nil {
		return refusal("opencode-claude: cannot write the install record: " + err.Error())
	}
	if err := writeFile(filepath.Join(options.StatusDir, pluginRecord), recordInstalled); err != nil {
		return refusal("opencode-claude: cannot write the install record: " + err.Error())
	}
	return nil
}

// reportEnvelope runs one child under the composed `run-report.sh` and reports
// whether it succeeded. The envelope owns the bound, the private HOME and the
// 75-to-1 mapping, and it always creates both capture files.
func reportEnvelope(ctx context.Context, options PluginsOptions, spec childSpec, seconds, stdoutFile, stderrFile string) bool {
	argv := append([]string{"sh", filepath.Join(options.Contract, "run-report.sh"), seconds, stdoutFile, stderrFile}, spec.argv...)
	_, status := runChild(ctx, childSpec{argv: argv, stderr: options.Err})
	return status == 0
}

// printFile is `cat FILE >&2`: the envelope's capture is the diagnostic, so it is
// reprinted whole.
func printFile(destination io.Writer, path string) {
	source, err := os.ReadFile(path)
	if err != nil {
		return
	}
	say(destination, "%s", string(source))
}

// audit is the `tool-access` subcommand's own rule, run in process so the two
// gates and the subcommand cannot drift. It prints what the subcommand printed:
// the resolved path on stdout, or the violation on stderr, and it keeps its
// status.
func audit(streams gateStreams, target string, mode toolaccess.Mode, quiet bool) error {
	auditor := toolaccess.NewAuditor(streams.Access)
	final, err := auditor.Audit(toolaccess.Target(target), mode)
	if err != nil {
		var violation *toolaccess.Violation
		if errors.As(err, &violation) {
			say(streams.Err, "tool-access: %s: %s\n", violation.Path, violation.Reason())
			return &Silent{Status: 1}
		}
		// An internal resolver failure (an unreadable cwd, a readlink race). The
		// subcommand reports it through the legacy exception line; that path is not
		// reachable from a composed tree, so this driver reports the error text at the
		// same status instead of reproducing the interpreter's frame.
		return refusal(err.Error())
	}
	if !quiet {
		say(streams.Out, "tool-access: ok %s\n", final)
	}
	return nil
}
