package opencode

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/document"
	"github.com/gregwebs/agent-vm-images/internal/pnpmenv"
	"github.com/gregwebs/agent-vm-images/internal/pnpmlock"
)

// PrepareOptions is one reviewed-input preparation invocation.
type PrepareOptions struct {
	// Contract is the composed helper directory: the manager envelope runs from it
	// and nowhere else.
	Contract string
	// Check is the reviewed-bytes checker and Pin the developer-only writer. They
	// are flags because a fixture replaces them; a build names neither, and the
	// install hook deliberately cannot override them.
	Check string
	Pin   string
	// FixtureRoot authorizes a prefix outside production.
	FixtureRoot string
	// Arguments is the caller's argv: PREFIX PLUGIN_VERSION [--refresh-lock]. The
	// grammar is this program's, so the entry point passes it through untouched.
	Arguments []string
	Out, Err  io.Writer
}

// EmptyPathUsage is the refusal for a required path flag that carried no value. The
// flag parser's `required` records only that the flag appeared, so `--contract ""`
// parses and would compose `run-pnpm.sh` relative to the caller's working directory;
// the caller cannot tell which flag was empty from the bare usage line, so this one
// names it. It is usage (2), not a refusal of a well-formed request.
func EmptyPathUsage(flag string) string {
	return prepareUsage + " (" + flag + " must not be empty)"
}

// productionPrefix is the only prefix a build may prepare without a fixture root:
// it is the image's own plugin directory. It is a variable so a test can point the
// exemption at a directory it can create; no caller can set it, and
// isProductionPrefix is the decision whatever path it is given.
var productionPrefix = "/opt/agent-vm/opencode-plugins"

const (
	// prepareUsage is the documented interface, quoted by callers.
	prepareUsage = "usage: prepare-lock.sh PREFIX PLUGIN_VERSION [--refresh-lock]"

	// pluginName is the one selection the reviewed manifest carries.
	pluginName = "@openchamber/opencode-claude"
	// requiredManager is the only manager that may resolve this lock: the reviewed
	// lock is a pnpm 11 artefact and regeneration under another major is not a
	// reviewed diff.
	requiredManager = "11.11.0"
	registryURL     = "https://registry.npmjs.org/"
)

// prepareRequest is the parsed argv.
type prepareRequest struct {
	prefix  string
	version string
	refresh bool
}

// parsePrepareArguments is the argv grammar: two positional slots and at most one
// `--refresh-lock`. A caller that names an option this driver does not have made a
// usage mistake (status 2), not a refused request.
func parsePrepareArguments(args []string) (prepareRequest, error) {
	if len(args) < 2 || len(args) > 3 {
		return prepareRequest{}, &Usage{Text: prepareUsage}
	}
	request := prepareRequest{prefix: args[0], version: args[1]}
	if len(args) == 3 {
		switch args[2] {
		case "":
		case "--refresh-lock":
			request.refresh = true
		default:
			// The predecessor's `case''|--refresh-lock) ;; *) exit 2` refused an
			// unknown third word with no diagnostic at all, so this usage carries
			// no text.
			return prepareRequest{}, &Usage{}
		}
	}
	return request, nil
}

// PrepareLock re-pins the three reviewed inputs after a changed selection.
//
// Empty and equal selections are immutable: they validate the committed bytes and
// start no package manager. A changed selection is a deliberate developer re-pin,
// because it rewrites bytes a build is meant to verify; `--refresh-lock` is the
// caller saying so.
func PrepareLock(ctx context.Context, options PrepareOptions) (failure error) {
	request, err := parsePrepareArguments(options.Arguments)
	if err != nil {
		return err
	}
	// The grammar runs before the prefix guard, as the predecessor's did: a range,
	// a dist-tag or a multi-line value is a refused selection whatever the prefix
	// looks like.
	if request.version != "" && !exactVersion(request.version) {
		return refusal("prepare-lock: selection must be canonical exact semver")
	}
	prefix, err := authorizePrefix(request.prefix, options.FixtureRoot)
	if err != nil {
		return err
	}
	// Two guards, in the predecessor's order, and both are load-bearing.
	//
	// The fixture-root authorization above runs before the first child, so an
	// unauthorized prefix never reaches the checker or the resolution envelope
	// mounted in the composed directory. This preload guard runs before any Node or
	// pnpm process starts: a preloaded module can patch node:crypto and subvert the
	// digest comparison the checker below performs. TestPrepareGuardOrder fails if
	// either moves.
	if err := refuseRuntimeInjection(); err != nil {
		return err
	}
	// The environment guard is the composed helper's own rule rather than a second
	// copy of it: the same policy defaults, the same case-sensitive variable set and
	// the same unreviewed-input list the `pnpm-environment` subcommand applies.
	if err := pnpmenv.Check(os.Environ(), []pnpmlock.Prefix{pnpmlock.Prefix(prefix)}); err != nil {
		return refusal("pnpm environment: " + err.Error())
	}
	committed, err := runChecker(ctx, options, prefix)
	if err != nil {
		return err
	}
	if request.version == "" || request.version == committed {
		say(options.Out, "prepare-lock: %s unchanged; committed inputs reused\n", committed)
		return nil
	}
	if !request.refresh {
		return refusalf("prepare-lock: committed inputs pin %s, not %s; re-pin deliberately with upgrade-plugins.sh",
			committed, request.version)
	}
	return rePin(ctx, options, prefix, request, committed)
}

// runChecker runs the reviewed-bytes checker and returns the selection it reports.
// Its stderr is the caller's: the checker owns those bytes, so this driver adds no
// verdict of its own.
func runChecker(ctx context.Context, options PrepareOptions, prefix string) (string, error) {
	stdout, status := runChild(ctx, childSpec{
		argv:    []string{"node", options.Check, prefix},
		capture: true,
		stderr:  options.Err,
	})
	if status != 0 {
		return "", &Silent{Status: status}
	}
	return trimCommandSubstitution(stdout), nil
}

// rePin is the developer path: stage the changed manifest in private scratch,
// resolve the lock, write the pins and publish the three inputs.
func rePin(ctx context.Context, options PrepareOptions, prefix string, request prepareRequest, committed string) (failure error) {
	watcher, stopWatching := watchInterrupts()
	defer stopWatching()
	abort, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-watcher.done
		cancel()
	}()
	// A signal outranks whatever the run was about to report, and it is applied
	// after the scratch cleanup below, exactly as the shell's EXIT trap reported
	// 130/143/129.
	defer func() { failure = interruptedOr(watcher, failure) }()

	work, err := os.MkdirTemp("", "opencode-prepare-")
	if err != nil {
		return refusal("prepare-lock: cannot create invocation scratch: " + err.Error())
	}
	defer func() { _ = os.RemoveAll(work) }()
	if err := privateState(work); err != nil {
		return refusal("prepare-lock: cannot create invocation scratch: " + err.Error())
	}
	defer redirectHome(work)()

	// The reviewed comparison was the manager's own report and nothing else: a
	// failed `--version` that still printed the pin was accepted, and the resolution
	// behind it is what a real failure surfaces in. Keeping that comparison is what
	// makes the refusal mean "another manager", not "a manager that stumbled". A
	// manager that prints nothing -- which is what a bound that fires produces -- is
	// refused here.
	//
	// The nonzero status is accepted on purpose: the differential measured the
	// predecessor doing exactly that (`manager exits after printing the pin`, 0 to
	// 0), so refusing it here would be an invented refusal with no oracle behind it.
	identity, _ := runChild(abort, childSpec{
		argv: []string{"pnpm", "--version", "--pm-on-fail=error", "--config.update-notifier=false"},
		// The identity check is the one manager call the reviewed shell bounded
		// itself, because run-pnpm.sh does not run it.
		bound:   identityBound,
		capture: true,
		stderr:  options.Err,
	})
	if trimCommandSubstitution(identity) != requiredManager {
		return refusalf("prepare-lock: pnpm %s is required; refusing another manager version", requiredManager)
	}

	project := filepath.Join(work, "project")
	// The reviewed workspace and lock are staged even though a re-pin moves one
	// selection: the resolution must run against them, never an ambient copy. The
	// predecessor guarded the lock copy with `if [ "$refresh" = --refresh-lock ]`,
	// which is always true here -- only a refresh reaches this point.
	for _, staged := range []struct{ from, name string }{
		{filepath.Join(prefix, "pnpm-workspace.yaml"), "pnpm-workspace.yaml"},
		{filepath.Join(prefix, "pnpm-lock.yaml"), "pnpm-lock.yaml"},
	} {
		if err := copyFile(staged.from, filepath.Join(project, staged.name)); err != nil {
			return refusalf("prepare-lock: cannot stage %s", staged.name)
		}
	}
	if err := stageManifest(filepath.Join(prefix, "package.json"), filepath.Join(project, "package.json"), request.version); err != nil {
		return err
	}

	resolution := []string{
		"sh", filepath.Join(options.Contract, "run-pnpm.sh"), filepath.Join(work, "prepare.ndjson"), "--",
		"pnpm", "install", "--lockfile-only", "--no-frozen-lockfile",
		"--config.update-notifier=false",
		"--ignore-scripts", "--ignore-pnpmfile", "--pm-on-fail=error",
		"--registry=" + registryURL, "--store-dir", filepath.Join(work, "store"),
	}
	if _, status := runChild(abort, childSpec{
		argv:      resolution,
		directory: project,
		stdout:    options.Out,
		stderr:    options.Err,
	}); status != 0 {
		// run-pnpm.sh's own envelope line and the manager's diagnostics are the
		// evidence; this driver adds no verdict on top of them.
		return &Silent{Status: status}
	}
	// The pin writer runs only after the resolution produced a project, and the
	// checker re-reads the staged bytes it just wrote: a preparation is validated
	// on its way out, not only on its way in.
	if _, status := runChild(abort, childSpec{argv: []string{"node", options.Pin, project}, stderr: options.Err}); status != 0 {
		return &Silent{Status: status}
	}
	if _, status := runChild(abort, childSpec{
		argv:   []string{"node", options.Check, project},
		stdout: io.Discard,
		stderr: options.Err,
	}); status != 0 {
		return &Silent{Status: status}
	}
	// The caller stages a disposable project; developer publication is transactional
	// across the manifest, lock, workspace, pins and central Dockerfile. Everything
	// after this point is publication, so a failure mid-way is reported rather than
	// left to exit with `cp`'s status and no explanation.
	for _, name := range []string{"package.json", "pnpm-lock.yaml", "pnpm-inputs.sha256"} {
		if err := copyFile(filepath.Join(project, name), filepath.Join(prefix, name)); err != nil {
			return refusalf("prepare-lock: cannot publish %s", name)
		}
	}
	return nil
}

// isProductionPrefix is the exemption's whole decision, and it is equality.
//
// A sibling named `/opt/agent-vm/opencode-plugins-x` or a child of the production
// directory is not the image's plugin directory and has to bring a fixture root
// like any other prefix, so a prefix test would exempt a directory no build owns.
// It is a separate function because the production path is the one input a host
// test cannot create.
func isProductionPrefix(physical string) bool { return physical == productionPrefix }

// authorizePrefix resolves the prefix physically and requires it to be the
// production one or to live inside the authorized fixture root.
//
// The physical path is what the rest of the run uses, exactly as
// `cd "$PREFIX" && pwd -P` did, so a fixture cannot be prepared through a symlink
// whose lexical name merely looks authorized. `filepath.Abs` first is that `cd`:
// `EvalSymlinks` on its own returns a relative path for a relative argument, and
// a relative path can never be shown to sit under an absolute root.
func authorizePrefix(prefix, fixtureRoot string) (string, error) {
	if !directoryExists(prefix) {
		// The predecessor's `[ -d "$PREFIX" ] || exit 1`: a refusal with no text.
		return "", &Silent{Status: 1}
	}
	physical, err := physicalPath(prefix)
	if err != nil {
		// `cd` failing under `set -e` is a usage error: the shell exited 2 with its
		// own `cd: can't cd to …` line.
		return "", &Usage{Text: "cd: can't cd to " + prefix}
	}
	if isProductionPrefix(physical) {
		return physical, nil
	}
	if fixtureRoot == "" {
		// `${AGENT_VM_FIXTURE_ROOT:?…}` is a missing required variable, which the
		// predecessor's shell reported at status 2.
		return "", &Usage{Text: "non-production prefix requires fixture root"}
	}
	root, err := physicalPath(fixtureRoot)
	if err != nil {
		return "", &Usage{Text: "cd: can't cd to " + fixtureRoot}
	}
	// The predecessor's `case "$prefix_phys" in "$root_phys"/*)`: the root itself is
	// not an authorized prefix, only something beneath it.
	if !strings.HasPrefix(physical, root+string(filepath.Separator)) {
		return "", &Silent{Status: 1}
	}
	return physical, nil
}

// physicalPath is `cd PATH && pwd -P`.
func physicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// stageManifest writes the committed manifest with one dependency replaced, keeping
// every other field and every key's position: the staged bytes are compared with
// the committed ones, so the rewrite must be the document with one selection
// changed. jq's `--arg` filter did this, and its output is two-space indentation
// with one trailing newline, which is what the encoder produces.
func stageManifest(manifestPath, destination, selection string) error {
	source, err := os.ReadFile(manifestPath)
	if err != nil {
		return refusalf("prepare-lock: cannot stage package.json")
	}
	manifest, err := document.Parse(source, document.JSON)
	if err != nil {
		return refusalf("prepare-lock: cannot stage package.json")
	}
	dependencies := manifest.Get("dependencies")
	if !dependencies.IsMapping() {
		return refusalf("prepare-lock: cannot stage package.json")
	}
	selectionValue, err := document.StringFromUTF8(selection)
	if err != nil {
		return refusalf("prepare-lock: cannot stage package.json")
	}
	staged := manifest.With("dependencies", dependencies.With(pluginName, document.NewJSString(selectionValue)))
	if err := os.WriteFile(destination, append(staged.AppendJSIndent(nil, "  "), '\n'), 0o644); err != nil {
		return refusalf("prepare-lock: cannot stage package.json")
	}
	return nil
}
