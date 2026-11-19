package dsh

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/pnpm"
	"github.com/gregwebs/agent-vm-images/internal/pnpmlock"
)

// InstallOptions is one owning install hook invocation.
type InstallOptions struct {
	// Source is the read-only mount carrying project.py, prepare-lock.sh and
	// check-runtime.mjs.
	Source string
	// Contract is the authorized composed helper directory: the preparation
	// envelope and run-pnpm.sh execute from it and nowhere else.
	Contract string
	// Prefix is the payload destination; Command is the link target the image
	// definition will point at it.
	Prefix  string
	Command string
	// DshVersion and PnpmVersion are the layer's requested slots. Empty keeps the
	// committed selection.
	DshVersion  string
	PnpmVersion string
	Out, Err    io.Writer
}

// Install runs the owning install hook.
//
// dsh is never soft-failable: a partial node_modules is a broken agent, not a
// missing one, so every failure here is hard regardless of AGENT_INSTALL_SOFT_FAIL.
//
// Order is load-bearing. Authorization covers the prefix, its four inputs, the base
// pin and the command destination before any mutation exists; a changed selection is
// prepared first, which resolves and validates a new graph; an empty or equal slot
// starts no package manager during preparation. Only after the exact report, the
// reviewed-input validation and the native layout check is the payload published and
// the slot recorded.
func Install(ctx context.Context, options InstallOptions) (failure error) {
	if err := refuseRuntimeInjection(); err != nil {
		return err
	}
	for _, helper := range []struct{ path, message string }{
		{filepath.Join(options.Source, "project.py"), "dsh project helper is missing at "},
		{filepath.Join(options.Source, "prepare-lock.sh"), "prepare-lock.sh is missing at "},
		{filepath.Join(options.Source, "check-runtime.mjs"), "layout checker is missing at "},
	} {
		if !fileExists(helper.path) {
			return refusal(helper.message + helper.path)
		}
	}
	if !executableFile(filepath.Join(options.Contract, "recipe-contract")) {
		return refusal("no executable recipe-contract in " + options.Contract)
	}
	if !fileExists(filepath.Join(options.Contract, "run-pnpm.sh")) {
		return refusal("hard runner is missing in " + options.Contract)
	}
	project := Project{Source: options.Source}
	facts, err := project.Authorize(ctx, options.Prefix, options.Command, "")
	if err != nil {
		return err
	}
	if err := project.Guard(ctx, options.Prefix); err != nil {
		return err
	}
	basePnpm, err := pnpm.PinVersion(facts.BasePin)
	if err != nil {
		return refusal("cannot read the base pin")
	}
	if basePnpm != basePinSupport {
		return refusal("unsupported base manager selection: " + basePnpm)
	}
	if options.PnpmVersion != "" && options.PnpmVersion != basePnpm {
		return refusal("pnpm compatibility slot must equal base pin")
	}
	manifestPath := filepath.Join(options.Prefix, "package.json")
	manifest, err := readManifest(manifestPath)
	if err != nil || manifest.PackageManager == "" {
		return refusal("committed manifest has no packageManager")
	}
	if manifest.PackageManager != "pnpm@"+basePnpm {
		return refusalf("manifest packageManager %s disagrees with base pin %s", manifest.PackageManager, basePnpm)
	}

	installer, stopWatching := watchInterrupts()
	defer stopWatching()
	abort, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-installer.done
		cancel()
	}()
	preparation := []string{"sh", filepath.Join(options.Source, "prepare-lock.sh"),
		options.Prefix, options.DshVersion, options.PnpmVersion}
	if status, ok := runInherited(abort, "", preparation...); !ok {
		if signal, interrupted := installer.exited(); interrupted {
			return &Interrupted{Status: signal}
		}
		// The preparation driver reported its own refusal; a signal keeps its status.
		return &Silent{Status: status}
	}

	committed, err := readManifest(manifestPath)
	if err != nil || committed.Dependencies["@deepseek-ai/dsh"] == "" {
		return refusal("committed manifest selects no dsh version")
	}
	selection := committed.Dependencies["@deepseek-ai/dsh"]
	if options.DshVersion != "" && options.DshVersion != selection {
		return refusalf("prepared selection %s does not match the requested %s", selection, options.DshVersion)
	}
	if _, err := pnpmlock.ReadProject(pnpmlock.Prefix(options.Prefix), pnpmlock.KindDsh); err != nil {
		say(options.Err, "pnpm lock: %s\n", err)
		return refusal("reviewed inputs failed pinned-input validation")
	}

	work, err := project.Scratch(ctx, options.Prefix, "")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if cleanupErr := project.Cleanup(context.WithoutCancel(ctx), work, options.Prefix); cleanupErr != nil {
			say(options.Err, "%s\n", `install-dsh.sh: invocation scratch cleanup failed`)
			if failure == nil {
				failure = &Silent{Status: 1}
			}
		}
		if failure == nil || published {
			return
		}
		// Hard failure: no half-installed agent may survive, and no caller cache is
		// touched because the manager never saw it.
		if removeErr := project.RemoveInstall(context.WithoutCancel(ctx), options.Prefix); removeErr != nil {
			say(options.Err, "%s\n", `install-dsh.sh: partial node_modules cleanup failed; recovery required`)
		}
	}()
	if err := privateState(work); err != nil {
		return refusal(err.Error())
	}
	defer redirectHome(work)()
	manager, err := project.Manager(ctx, options.Prefix)
	if err != nil {
		return err
	}
	identity, err := managerIdentity(abort, work, manager)
	if err != nil {
		return interruptedOr(installer, refusal("base manager identity check failed"))
	}
	if strings.TrimSpace(identity) != basePnpm {
		return refusalf("base manager is not the pin %s", basePnpm)
	}
	installErr := runPnpm(abort, options.Contract, filepath.Join(work, "install.ndjson"),
		options.Prefix, processEnvironment(work), manager, withStore(frozenInstall, work))
	if installErr != nil {
		return interruptedOr(installer, refusal("frozen install failed"))
	}
	layout, err := checkLayout(abort, options.Source, options.Prefix)
	if err != nil {
		return err
	}
	entry, err := layout.resolvedBin()
	if err != nil {
		return err
	}
	if err := os.Chmod(entry, 0o755); err != nil {
		return refusal("cannot make the dsh bin executable")
	}
	if err := publishPermissions(options.Prefix); err != nil {
		return refusal("cannot publish the installed payload a+rX")
	}
	published = true
	say(options.Out, "install-dsh.sh: installed dsh %s from the reviewed inputs\n", selection)
	return nil
}

// refuseRuntimeInjection rejects a Node preload before any Node process starts: a
// preload would make every later check untrustworthy.
func refuseRuntimeInjection() error {
	for _, key := range []string{"NODE_OPTIONS", "NODE_PATH", "BUN_OPTIONS"} {
		if os.Getenv(key) != "" {
			return refusal(key + " is not supported")
		}
	}
	return nil
}

// interruptedOr prefers a signal over the failure it caused: the caller's own trap
// must see 130/143/129, not a generic 1.
func interruptedOr(watcher *interruption, otherwise error) error {
	if status, interrupted := watcher.exited(); interrupted {
		return &Interrupted{Status: status}
	}
	return otherwise
}

// runInherited runs one child with the caller's streams and reports its status.
func runInherited(ctx context.Context, directory string, argv ...string) (int, bool) {
	command := childCommand(ctx, directory, argv)
	if err := command.Run(); err != nil {
		return exitStatusOf(err), false
	}
	return 0, true
}

// layoutReport is what the native layout checker publishes: the single bin target
// the image definition links, plus the packaging facts it asserts.
type layoutReport struct {
	ResolvedBin *string `json:"resolvedBin"`
}

func (r layoutReport) resolvedBin() (string, error) {
	if r.ResolvedBin == nil || *r.ResolvedBin == "" {
		return "", refusal("layout check returned no bin target")
	}
	return *r.ResolvedBin, nil
}

// checkLayout runs the Node layout checker: the direct link target, the reviewed bin
// mapping and the absence of a second package manager are packaging facts, so native
// code owns them.
func checkLayout(ctx context.Context, source, prefix string) (layoutReport, error) {
	var stdout, stderr strings.Builder
	err := runBounded(ctx, runtimeTimeout, "", nil, &stdout, &stderr,
		"node", filepath.Join(source, "check-runtime.mjs"), prefix, "--layout")
	if err != nil {
		if stderr.Len() > 0 {
			return layoutReport{}, &ChildFailure{Status: exitStatusOf(err), Stderr: stderr.String()}
		}
		return layoutReport{}, refusal("installed layout failed validation")
	}
	var report layoutReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		return layoutReport{}, refusal("installed layout failed validation")
	}
	return report, nil
}
