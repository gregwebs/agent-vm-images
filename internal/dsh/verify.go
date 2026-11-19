package dsh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/pnpm"
	"github.com/gregwebs/agent-vm-images/internal/toolaccess"
)

// VerifyOptions is one installed-payload gate invocation.
type VerifyOptions struct {
	Source string
	// Contract is the authorized composed helper directory the gate executes from.
	Contract  string
	Prefix    string
	Command   string
	StatusDir string
	// DshSlot and PnpmSlot are the layer's requested versions. Non-empty must equal
	// the effective selection, or the gate would verify the wrong thing.
	DshSlot  string
	PnpmSlot string
	Access   toolaccess.Environment
	Out, Err io.Writer
	// HTTPProbe is the readiness driver. Nil runs the real one; the package tests
	// substitute a recorder so the gate's ordering is provable without a browser.
	HTTPProbe func(ctx context.Context, options ProbeOptions) ([]byte, error)
}

// verifyRecord is the status record the gate owns: `pending` before it measures and
// `installed` only after every check passes, so a failed rerun cannot leave a stale
// success behind.
const (
	recordPending   = "pending\n"
	recordInstalled = "installed\n"
)

// Verify is the installed-payload gate.
//
// The version *string* is the assertion, not the exit code: dsh dispatches on
// `import.meta.main`, which Node only defines from 22.19, so an older Node loads
// bin.js, prints nothing and exits 0. Checking only the exit status would certify an
// image that ships no working dsh.
//
// dsh is never soft-failable: a partial or unhealthy agent is a broken image, so
// every failure here is hard regardless of AGENT_INSTALL_SOFT_FAIL (this program
// deliberately never consults it).
func Verify(ctx context.Context, options VerifyOptions) (failure error) {
	if err := refuseRuntimeInjection("dsh: "); err != nil {
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
	// The status a signal carries outranks the run's own verdict and is applied after
	// the scratch cleanup below, as the retired EXIT trap reported 130/143/129.
	defer func() { failure = interruptedOr(watcher, failure) }()
	for _, helper := range []string{
		filepath.Join(options.Source, "project.py"),
		filepath.Join(options.Source, "check-runtime.mjs"),
	} {
		if !regularFile(helper) {
			return refusal("dsh: build helper is missing at " + helper)
		}
	}
	project := Project{Source: options.Source}
	if err := project.Guard(abort, options.Prefix); err != nil {
		reportChildFailure(options.Err, err)
		return refusal("dsh: the environment guard rejected this invocation")
	}
	// An authorized directory is not necessarily a composed one: a host checkout
	// authorizes its own source-only `images/recipe-contract`, which holds no binary,
	// so exec would fail with 127 instead of a reason.
	if !executableFile(filepath.Join(options.Contract, "recipe-contract")) {
		return refusalf("dsh: no executable recipe-contract in %s (set AGENT_VM_CONTRACT_DIR"+
			" to a composed helper directory; a source checkout carries no binary)", options.Contract)
	}
	facts, err := project.Authorize(abort, options.Prefix, options.Command, options.StatusDir)
	if err != nil {
		reportChildFailure(options.Err, err)
		return refusal("dsh: prefix, command or status destination is not authorized")
	}
	basePnpm, err := pnpm.PinVersion(facts.BasePin)
	if err != nil {
		return refusal("dsh: cannot read the base pin")
	}
	manifest, err := readManifest(filepath.Join(options.Prefix, "package.json"))
	if err != nil || manifest.Dependencies["@deepseek-ai/dsh"] == "" {
		return refusal("dsh: the committed manifest selects no dsh version")
	}
	want := manifest.Dependencies["@deepseek-ai/dsh"]
	if manifest.PackageManager == "" {
		return refusal("dsh: the committed manifest has no packageManager assertion")
	}
	if manifest.PackageManager != "pnpm@"+basePnpm {
		return refusalf("dsh: manifest packageManager %s disagrees with base pin %s",
			manifest.PackageManager, basePnpm)
	}
	if options.DshSlot != "" && options.DshSlot != want {
		return refusalf("dsh: layer asked for %s but the manifest pins %s", options.DshSlot, want)
	}
	if options.PnpmSlot != "" && options.PnpmSlot != basePnpm {
		return refusalf("pnpm: layer asked for %s but the base pin is %s", options.PnpmSlot, basePnpm)
	}

	// Reset the record before measuring so a rerun that fails cannot leave a
	// misleading `installed` from an earlier image state.
	if err := os.MkdirAll(options.StatusDir, 0o755); err != nil {
		return refusal(err.Error())
	}
	record := filepath.Join(options.StatusDir, "dsh")
	if err := writeFile(record, recordPending); err != nil {
		return refusal(err.Error())
	}

	for _, command := range []struct{ name, message string }{
		{"dsh", "dsh: MISSING — hard sanity failure"},
		{"pnpm", "pnpm: MISSING — hard sanity failure ('dsh plugin' would fail)"},
	} {
		if _, found := pathCandidate(command.name); !found {
			return refusal(command.message)
		}
	}
	// The first lexical PATH candidate must be the installed file itself: a
	// same-version copy is a different program, while a symlink to the same file is
	// the normal image layout and passes.
	if err := project.CommandIdentity(abort, "dsh", options.Command); err != nil {
		reportChildFailure(options.Err, err)
		return refusal("dsh: the first PATH candidate is not the installed command")
	}
	if err := verifyDistribution(); err != nil {
		reportDistribution(options.Err, err)
		return refusal("pnpm: base launcher/distribution identity failed")
	}

	work, err := project.Scratch(abort, options.Prefix, "")
	if err != nil {
		return refusal("dsh: cannot create invocation scratch")
	}
	defer func() {
		if cleanupErr := project.Cleanup(context.WithoutCancel(abort), work, options.Prefix); cleanupErr != nil {
			say(options.Err, "%s\n", `  dsh: invocation scratch cleanup failed`)
			if failure == nil {
				failure = &Silent{Status: 1}
			}
		}
	}()
	if err := privateState(work); err != nil {
		return refusal(err.Error())
	}
	defer redirectHome(work)()
	gotDsh, err := versionReport(abort, options, work, "dsh", "dsh")
	if err != nil {
		return err
	}
	if gotDsh != want {
		return refusalf("dsh: installed %s but the manifest pins %s", gotDsh, want)
	}
	gotPnpm, err := versionReport(abort, options, work, "pnpm", "pnpm")
	if err != nil {
		return err
	}
	if gotPnpm != basePnpm {
		return refusalf("pnpm: installed %s but the base pin is %s", gotPnpm, basePnpm)
	}
	say(options.Out, "  dsh: %s (pnpm %s)\n", gotDsh, gotPnpm)

	// Real behaviour, not a version report. The JavaScript probes stay bounded
	// internally; the outer bound is belt-and-braces.
	runtimeFile := filepath.Join(work, "runtime.json")
	runtimeErr := filepath.Join(work, "runtime.err")
	if err := runRuntimeProbes(abort, options, runtimeFile, runtimeErr); err != nil {
		reportPrefix(options.Err, runtimeErr, 20)
		return refusal("dsh: installed runtime probes failed")
	}
	if err := runWebProbe(abort, options, work); err != nil {
		return refusal("dsh: credential-free web readiness failed")
	}

	// Health must not have rewritten the identity assumptions it just proved.
	if err := project.CommandIdentity(abort, "dsh", options.Command); err != nil {
		reportChildFailure(options.Err, err)
		return refusal("dsh: PATH identity changed during the health gate")
	}
	if err := verifyDistribution(); err != nil {
		reportDistribution(options.Err, err)
		return refusal("pnpm: base identity changed during the health gate")
	}

	// Tree access covers the whole payload; a tree audit alone does not certify
	// executables or the interpreter chain, so the commands are audited explicitly,
	// and every native module the probes actually loaded must be readable by all.
	if err := auditAccess(options, options.Prefix, toolaccess.Tree); err != nil {
		return refusal("dsh: installed tree is not readable/traversable by an arbitrary uid")
	}
	for _, target := range []string{"dsh", "pnpm"} {
		if err := auditAccess(options, target, toolaccess.Executable); err != nil {
			return refusal("dsh: a command or its interpreter is not usable by an arbitrary uid")
		}
	}
	native, err := runtimeNative(runtimeFile)
	if err != nil {
		return refusal("dsh: runtime report is unreadable")
	}
	for _, file := range native {
		if err := auditAccess(options, file, toolaccess.Content); err != nil {
			return refusal("dsh: a loaded native module is not readable by an arbitrary uid")
		}
	}
	return writeFile(record, recordInstalled)
}

// verifyDistribution runs the distribution audit the base image owns. It is the same
// audit `pnpm-verify` performs, so a caller cannot get a weaker answer from the gate.
func verifyDistribution() error {
	target, err := pnpm.ResolveTarget()
	if err != nil {
		return err
	}
	return pnpm.Verify(pnpm.VerifyOptions{Target: target, PathIdentity: true})
}

// versionReport runs one bounded `--version` and returns its single non-blank line.
// The mapping is exact: a second non-blank line, an empty report or a nonzero status
// is a failure, never a partially trusted answer.
func versionReport(ctx context.Context, options VerifyOptions, work, exe, label string) (string, error) {
	outFile := filepath.Join(work, label+".out")
	errFile := filepath.Join(work, label+".err")
	// The runner captures the child itself, so the report is read back from the file
	// it wrote rather than from this process's own pipe.
	err := runBounded(ctx, reportTimeout, "", nil, io.Discard, io.Discard,
		"sh", filepath.Join(options.Contract, "run-report.sh"), "20", outFile, errFile, exe, "--version")
	if err != nil {
		say(options.Err, "  %s: --version exited %d; refusing to accept the report\n", exe, exitStatusOf(err))
		reportPrefix(options.Err, errFile, 10)
		return "", &Silent{Status: 1}
	}
	report, readErr := os.ReadFile(outFile)
	if readErr != nil {
		return "", refusal(readErr.Error())
	}
	first := firstLine(string(report))
	if strings.TrimSpace(first) == "" {
		if exe == "dsh" {
			say(options.Err, "%s\n", `  dsh: empty --version output — hard sanity failure`)
			say(options.Err, "%s\n", `  dsh needs Node >=22.19 ('import.meta.main'); an older Node makes`)
			say(options.Err, "%s\n", `  its CLI exit 0 with no output, so this must fail the build.`)
		} else {
			say(options.Err, "  %s: empty --version output — hard sanity failure\n", exe)
		}
		return "", &Silent{Status: 1}
	}
	for index, line := range strings.Split(string(report), "\n") {
		if index == 0 || line == "" {
			continue
		}
		say(options.Err, "  %s: unexpected --version output on line %d: '%s'\n", exe, index+1, line)
		return "", &Silent{Status: 1}
	}
	return first, nil
}

// runRuntimeProbes runs the JavaScript probes under their own bound.
func runRuntimeProbes(ctx context.Context, options VerifyOptions, stdoutFile, stderrFile string) error {
	var stdout, stderr strings.Builder
	err := runBounded(ctx, runtimeTimeout, "", nil, &stdout, &stderr,
		"node", filepath.Join(options.Source, "check-runtime.mjs"), options.Prefix, "--runtime")
	if writeErr := writeFile(stdoutFile, stdout.String()); writeErr != nil {
		return writeErr
	}
	if writeErr := writeFile(stderrFile, stderr.String()); writeErr != nil {
		return writeErr
	}
	return err
}

// runWebProbe runs the bounded readiness driver. The child environment the fixture
// needs is named per invocation: this is the one place the dsh layer's existing
// `DSH_WEB_FIXTURE_` boundary crosses into the probe, and it crosses as flags so the
// probe itself has no environment seam.
func runWebProbe(ctx context.Context, options VerifyOptions, work string) error {
	probe := options.HTTPProbe
	if probe == nil {
		probe = Probe
	}
	scratch := filepath.Join(work, "home", "runtime")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return refusal(err.Error())
	}
	bounded, cancel := context.WithTimeout(ctx, webTimeout)
	defer cancel()
	report, err := probe(bounded, ProbeOptions{
		Source:            options.Source,
		Prefix:            options.Prefix,
		Command:           options.Command,
		ScratchRoot:       scratch,
		FixtureExecTmpdir: os.Getenv("AGENT_VM_FIXTURE_EXEC_TMPDIR"),
		FixtureChildEnv:   fixtureChildEnvironment(),
	})
	if writeErr := writeFile(filepath.Join(work, "web.json"), string(report)); writeErr != nil {
		return writeErr
	}
	if err != nil {
		// The gate prints the driver's own diagnostics once, exactly as the retired
		// `sed -n 1,20p web.err` did: the delegated child's stderr is the diagnostic, so
		// its summary line is not printed on top of it.
		if child, ok := err.(*ChildFailure); ok && child.Stderr != "" {
			say(options.Err, "%s\n", firstLines(child.Stderr, 20))
			return err
		}
		say(options.Err, "%s\n", firstLines(err.Error(), 20))
		return err
	}
	return nil
}

// fixtureChildEnvironment is every `DSH_WEB_FIXTURE_` entry of this process's
// environment, handed to the probe as one flag per entry.
func fixtureChildEnvironment() []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "DSH_WEB_FIXTURE_") {
			environment = append(environment, entry)
		}
	}
	return environment
}

// auditAccess runs one tool-access audit and prints the ok line the build log reads.
func auditAccess(options VerifyOptions, target string, mode toolaccess.Mode) error {
	auditor := toolaccess.NewAuditor(options.Access)
	final, err := auditor.Audit(toolaccess.Target(target), mode)
	if err != nil {
		var failure *toolaccess.Violation
		if errors.As(err, &failure) {
			say(options.Err, "tool-access: %s: %s\n", failure.Path, failure.Reason())
			return err
		}
		return err
	}
	say(options.Out, "tool-access: ok %s\n", final)
	return nil
}

// runtimeNative reads the native modules the probes actually loaded.
func runtimeNative(path string) ([]string, error) {
	fields, err := readJSONField(path, "native")
	if err != nil {
		return nil, err
	}
	return fields, nil
}

// reportChildFailure prints what a delegated child already said, so the two-line
// diagnostic order the hooks produced is preserved.
func reportChildFailure(destination io.Writer, err error) {
	child, ok := err.(*ChildFailure)
	if !ok {
		return
	}
	if child.Stderr != "" {
		say(destination, "%s", child.Stderr)
	}
}

// reportDistribution prints what the distribution audit said, under the prefix the
// `pnpm-verify` subcommand itself uses: a build log reads one diagnostic either way.
func reportDistribution(destination io.Writer, err error) {
	var frozen *pnpm.Frozen
	if errors.As(err, &frozen) {
		say(destination, "%s\n", frozen.Text)
		return
	}
	var child *pnpm.ChildFailure
	if errors.As(err, &child) && child.Stderr != "" {
		say(destination, "%s", child.Stderr)
		return
	}
	say(destination, "pnpm verification: %s\n", err)
}

// reportPrefix prints at most limit lines of a captured file.
func reportPrefix(destination io.Writer, path string, limit int) {
	source, err := os.ReadFile(path)
	if err != nil {
		return
	}
	say(destination, "%s\n", firstLines(string(source), limit))
}

// firstLines keeps at most limit lines of text.
func firstLines(text string, limit int) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > limit {
		lines = lines[:limit]
	}
	return strings.Join(lines, "\n")
}

// readJSONField reads one array-of-strings field from a captured JSON document.
func readJSONField(path, field string) ([]string, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document map[string]any
	if err := json.Unmarshal(source, &document); err != nil {
		return nil, err
	}
	values := []string{}
	raw, ok := document[field]
	if !ok {
		return values, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, &Refusal{Text: "field is not a list: " + field}
	}
	for _, item := range list {
		text, ok := item.(string)
		if !ok {
			return nil, &Refusal{Text: "field is not a list of strings: " + field}
		}
		values = append(values, text)
	}
	return values, nil
}
