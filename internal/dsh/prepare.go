package dsh

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/document"
	"github.com/gregwebs/agent-vm-images/internal/pnpm"
	"github.com/gregwebs/agent-vm-images/internal/pnpmlock"
)

// PrepareOptions is one preparation invocation.
type PrepareOptions struct {
	// Source is the read-only mount carrying project.py and check-runtime.mjs.
	Source string
	// Contract is the authorized composed helper directory.
	Contract string
	// Arguments is the caller's argv: PREFIX DSH_VERSION PNPM_VERSION
	// [--refresh-lock]. The grammar is this program's, so the entry point passes it
	// through untouched.
	Arguments []string
	Out, Err  io.Writer
}

// prepareUsage is the documented interface, quoted by callers.
const prepareUsage = "usage: prepare-lock.sh PREFIX DSH_VERSION PNPM_VERSION [--refresh-lock]"

// prepareRequest is the parsed argv.
type prepareRequest struct {
	prefix      string
	dshVersion  string
	pnpmVersion string
	refresh     bool
}

// ParsePrepareArguments is the argv grammar: three positional slots, at most one
// `--refresh-lock`, nothing else. A caller that names a slot twice or an option this
// driver does not have made a usage mistake (status 2), not a refused request.
func ParsePrepareArguments(args []string) (prepareRequest, error) {
	if len(args) < 3 {
		return prepareRequest{}, &Usage{Text: prepareUsage, Bare: true}
	}
	request := prepareRequest{prefix: args[0], dshVersion: args[1], pnpmVersion: args[2]}
	for _, option := range args[3:] {
		if option != "--refresh-lock" || request.refresh {
			return prepareRequest{}, &Usage{Text: prepareUsage, Bare: true}
		}
		request.refresh = true
	}
	return request, nil
}

// PrepareLock prepares and publishes the four reviewed inputs.
//
// Empty and equal selections are immutable: they start no package manager and
// re-check the committed bytes, structure and effective peers. A changed selection
// stages the changed manifest only, so the resolver cannot keep a retained version or
// its current upstream metadata; the comparison is then a refresh, because that is
// how the graph was produced. One rule for the image build, the CI changed leg and
// every developer upgrade.
func PrepareLock(ctx context.Context, options PrepareOptions) (failure error) {
	// The environment guard runs before the argv grammar, as the predecessor's did: a
	// hostile environment is a refusal whatever the arguments look like, and the shell
	// entry point only guards its own `$1`.
	if err := refuseRuntimeInjection(""); err != nil {
		return err
	}
	request, err := ParsePrepareArguments(options.Arguments)
	if err != nil {
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
	// A signal outranks whatever the run was about to report, and it is applied after
	// the cleanup defer below, exactly as the shell's EXIT trap reported 130/143/129.
	defer func() { failure = interruptedOr(watcher, failure) }()
	for _, slot := range []string{request.dshVersion, request.pnpmVersion} {
		if err := dshVersionSlot(slot); err != nil {
			return err
		}
	}
	project := Project{Source: options.Source}
	facts, err := project.Authorize(abort, request.prefix, "", "")
	if err != nil {
		return err
	}
	if err := project.Guard(abort, request.prefix); err != nil {
		return err
	}
	if !executableFile(filepath.Join(options.Contract, "recipe-contract")) {
		return refusal("no executable recipe-contract in " + options.Contract)
	}
	basePnpm, err := pnpm.PinVersion(facts.BasePin)
	if err != nil {
		return refusal("cannot read the base pin")
	}
	if basePnpm != basePinSupport {
		return refusal("unsupported base manager selection: " + basePnpm)
	}
	if request.pnpmVersion != "" && request.pnpmVersion != basePnpm {
		return refusal("pnpm compatibility selection disagrees with base pin")
	}
	manifestPath := filepath.Join(request.prefix, "package.json")
	manifest, err := readManifest(manifestPath)
	if err != nil || manifest.PackageManager == "" {
		return refusal("committed manifest has no packageManager")
	}
	if manifest.PackageManager != "pnpm@"+basePnpm {
		return refusalf("manifest packageManager %s disagrees with base pin %s", manifest.PackageManager, basePnpm)
	}
	committed := manifest.Dependencies["@deepseek-ai/dsh"]
	if committed == "" {
		return refusal("committed manifest selects no dsh version")
	}
	effective := committed
	if request.dshVersion != "" {
		effective = request.dshVersion
	}
	if effective == committed && !request.refresh {
		// Comparing the committed project with itself proves all four bytes, the
		// structure, the effective peers and the base assertions without a manager.
		mode := pnpmlock.NoRefresh
		line, err := pnpmlock.CompareDshUpdateDiagnostic(pnpmlock.UpdateRequest{
			Before:  pnpmlock.Prefix(request.prefix),
			After:   pnpmlock.Prefix(request.prefix),
			Refresh: mode,
		}, "")
		if err != nil {
			say(options.Err, "pnpm lock: %s\n", err)
			return &Silent{Status: 1}
		}
		say(options.Out, "%s", line)
		say(options.Out, "prepare-lock.sh: dsh %s unchanged; reviewed inputs reused\n", effective)
		return nil
	}

	work, err := project.Scratch(abort, request.prefix, "")
	if err != nil {
		return err
	}
	stage := filepath.Join(work, "prefix")
	// Status 3 (incomplete rollback) must retain the whole scratch: removing the
	// staged graph or the recovery material would destroy the evidence.
	retain := false
	defer func() {
		if retain {
			return
		}
		if cleanupErr := project.Cleanup(context.WithoutCancel(abort), work, ""); cleanupErr != nil {
			say(options.Err, "%s\n", `prepare-lock.sh: invocation scratch cleanup failed`)
			if failure == nil {
				failure = &Silent{Status: 1}
			}
		}
	}()
	if err := os.Mkdir(stage, 0o700); err != nil {
		return refusal(err.Error())
	}
	for _, name := range []string{"pnpm-workspace.yaml", "pnpm-inputs.sha256"} {
		if err := copyFile(filepath.Join(request.prefix, name), filepath.Join(stage, name)); err != nil {
			return refusal("cannot stage the changed manifest")
		}
	}
	// Every resolution resolves from the manifest and the reviewed workspace alone.
	// The workspace is staged even when byte-identical: resolution must run against
	// the reviewed workspace, never an ambient one.
	if err := stageManifest(manifestPath, filepath.Join(stage, "package.json"), effective); err != nil {
		return refusal("cannot stage the changed manifest")
	}
	if err := privateState(work); err != nil {
		return refusal(err.Error())
	}
	manager, err := project.Manager(abort, request.prefix)
	if err != nil {
		return err
	}
	identity, err := managerIdentity(abort, work, manager)
	if err != nil {
		return refusal("manager version check failed")
	}
	if strings.TrimSpace(identity) != basePnpm {
		return refusalf("manager is not the base pin %s", basePnpm)
	}
	say(options.Out, "prepare-lock.sh: dsh %s -> %s via %s\n",
		committed, effective, strings.Join(manager, " "))
	if err := runPnpm(abort, options.Contract, filepath.Join(work, "resolve.ndjson"), stage,
		processEnvironment(work), manager, withStore(stagedResolution, work)); err != nil {
		// The manager's own line and the envelope's hard-failure line are the evidence;
		// this driver only adds its verdict.
		reportChildFailure(options.Err, err)
		return refusal("staged resolution failed")
	}
	// Lock-only generation must not create an installed tree in the staged project.
	if pathExists(filepath.Join(stage, "node_modules")) {
		return refusal("lock-only generation created node_modules")
	}
	// Pins are written only after the staged structure and every effective exact peer
	// validate, and writing them is that validation: `WritePins` reads the staged graph,
	// checks its structure and peers, and publishes `pnpm-inputs.sha256` from the bytes
	// it just wrote. The comparison below then re-reads both sides, so a prepared graph
	// is validated again on its way out rather than by a second call to the same reader
	// on the same path in this process.
	if err := pnpmlock.WritePins(pnpmlock.Prefix(stage), pnpmlock.KindDsh); err != nil {
		say(options.Err, "pnpm lock: %s\n", err)
		return refusal("staged graph failed structural validation")
	}
	// The comparison matches how the graph was produced: an unseeded resolution is
	// free to move a retained snapshot, and only a refresh comparison says so.
	line, err := pnpmlock.CompareDshUpdateDiagnostic(pnpmlock.UpdateRequest{
		Before:  pnpmlock.Prefix(request.prefix),
		After:   pnpmlock.Prefix(stage),
		Refresh: pnpmlock.RefreshLock,
	}, request.dshVersion)
	if err != nil {
		say(options.Err, "pnpm lock: %s\n", err)
		return &Silent{Status: 1}
	}
	say(options.Out, "%s", line)
	if err := project.Publish(ctx, stage, request.prefix); err != nil {
		if child, ok := asChild(err); ok && child.Status == 3 {
			retain = true
			say(options.Err,
				"prepare-lock.sh: publication rollback is incomplete; staged inputs and recovery retained\n")
			if child.Stderr != "" {
				say(options.Err, "%s", child.Stderr)
			}
			return &Silent{Status: 3}
		}
		reportChildFailure(options.Err, err)
		return refusal("publication failed; committed inputs were restored and staged graph discarded")
	}
	say(options.Out, "prepare-lock.sh: published four dsh inputs for %s\n", effective)
	return nil
}

// asChild is the child's own status, with its stderr already carried.
func asChild(err error) (*ChildFailure, bool) {
	child, ok := err.(*ChildFailure)
	return child, ok
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

// stageManifest writes the committed manifest with one dependency replaced, keeping
// every other field and every key's position: the staged bytes are compared with the
// committed ones, so the rewrite must be the document with one selection changed.
func stageManifest(manifestPath, destination, selection string) error {
	source, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	manifest, err := document.Parse(source, document.JSON)
	if err != nil {
		return err
	}
	dependencies := manifest.Get("dependencies")
	if !dependencies.IsMapping() {
		return refusal("committed manifest selects no dsh version")
	}
	selectionValue, err := document.StringFromUTF8(selection)
	if err != nil {
		return err
	}
	staged := manifest.With("dependencies", dependencies.With("@deepseek-ai/dsh", document.NewJSString(selectionValue)))
	return os.WriteFile(destination, append(staged.AppendJSIndent(nil, "  "), '\n'), 0o644)
}
