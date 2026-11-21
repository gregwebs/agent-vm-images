package opencode

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// InstallPluginsOptions is one installed-plugin install invocation.
type InstallPluginsOptions struct {
	// Contract is the composed helper directory: the environment guard and the
	// manager envelope run from it and nowhere else.
	Contract string
	// Source is the read-only build mount carrying plugins/prepare-lock.sh.
	Source string
	// Prefix is the installed plugin directory.
	Prefix string
	// FixtureRoot authorizes a non-production prefix; FixturePnpm replaces the fixed
	// launcher beneath that root and is ignored everywhere else.
	FixtureRoot string
	FixturePnpm string
	// PluginVersion is the layer's requested Claude plugin slot; empty keeps the
	// committed selection.
	PluginVersion string
	Out, Err      io.Writer
}

const (
	// pluginsHook names this hook in its diagnostics, as the predecessor did.
	pluginsHook = "install-opencode-plugins"
	// launcher is the only production manager. The base image installs the reviewed
	// 11.11.0 launcher here, and this hook never resolves `pnpm` through PATH: a build
	// must not run whichever manager a mount happened to put there.
	launcher = "/usr/local/bin/pnpm"
	// pluginsSlotRefusal is the predecessor's manager-identity diagnostic.
	pluginsSlotRefusal = pluginsHook + ": pnpm " + requiredManager + " is required"
)

// InstallPlugins installs the reviewed plugin inputs from the committed lock.
//
// The hook is hard-failure-only: certification owns `installed`, so nothing here
// writes a record or softens a failure, whatever AGENT_INSTALL_SOFT_FAIL says.
//
// The order is the predecessor's and every step in it is load-bearing:
//
//  1. the prefix is authorized, so an unauthorized directory never reaches a child;
//  2. the preload guard runs before any Node process, because a preloaded module can
//     patch node:crypto and subvert the reviewed-bytes check that preparation runs;
//  3. the two developer-only seams are removed, so a build cannot point preparation
//     at another checker or at the developer-only pin writer;
//  4. the shared environment guard runs as the composed subcommand, so the build and
//     the developer path apply one copy of the policy defaults;
//  5. preparation validates the committed bytes, and only then does the manager run,
//     under a private HOME and XDG tree, through the composed envelope; and
//  6. a failed run removes the partial node_modules, because half an installed plugin
//     is neither a working one nor an absent one.
func InstallPlugins(ctx context.Context, options InstallPluginsOptions) (failure error) {
	prefix, err := authorizePrefix(options.Prefix, options.FixtureRoot)
	if err != nil {
		return err
	}
	if err := refuseRuntimeInjection(); err != nil {
		return err
	}
	manager := managerSelection(prefix, options.FixturePnpm)
	// The empty name prefix is the predecessor's `mktemp -d`: the invocation's own
	// scratch, named by the system and removed below.
	work, err := os.MkdirTemp("", "")
	if err != nil {
		return refusal(pluginsHook + ": cannot create invocation scratch: " + err.Error())
	}
	// published is the predecessor's `success=true`, which it set only after the
	// payload was published. Anything else -- a refusal, a failed child, a signal --
	// leaves a partial tree this hook must not let look installed.
	published := false
	defer func() {
		if !published {
			if err := os.RemoveAll(filepath.Join(prefix, "node_modules")); err != nil {
				say(options.Err, "%s: partial node_modules cleanup failed; recovery required\n", pluginsHook)
			}
		}
		_ = os.RemoveAll(work)
	}()

	watcher, stopWatching := watchInterrupts()
	defer stopWatching()
	abort, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-watcher.done
		cancel()
	}()
	// A signal carries 130/143/129 out of this hook, and it is applied after the
	// cleanup above, exactly as the EXIT trap reported it.
	defer func() { failure = interruptedOr(watcher, failure) }()

	// The guard runs from the composed directory, which is why the entry point names
	// it: the build, the CI leg and the developer path must refuse the same ambient
	// policy, and this subcommand is that rule's one owner.
	if _, status := runChild(abort, childSpec{
		argv:        []string{filepath.Join(options.Contract, "recipe-contract"), "pnpm-environment", prefix},
		stdin:       os.Stdin,
		stdout:      options.Out,
		stderr:      options.Err,
		environment: childEnvironment(""),
	}); status != 0 {
		return &Silent{Status: status}
	}
	if err := createInvocationState(work); err != nil {
		return refusal(pluginsHook + ": cannot create invocation scratch: " + err.Error())
	}
	environment := childEnvironment(work)
	// Preparation owns the reviewed bytes and the manifest policy, and its stderr is
	// the caller's; this hook adds no verdict on top of it.
	if _, status := runChild(abort, childSpec{
		argv: []string{"sh", filepath.Join(options.Source, "plugins", "prepare-lock.sh"),
			prefix, options.PluginVersion},
		stdin:       os.Stdin,
		stdout:      options.Out,
		stderr:      options.Err,
		environment: environment,
	}); status != 0 {
		return &Silent{Status: status}
	}
	// The identity check is the one manager call this hook bounds itself, because the
	// composed envelope does not run it. Its own status is not the question -- the
	// reviewed comparison was the report and nothing else -- so a manager that prints
	// the pin and then stumbles is accepted here and refused by the run that follows.
	identity, _ := runChild(abort, childSpec{
		argv:        identityArgv(manager),
		bound:       identityBound,
		capture:     true,
		stdin:       os.Stdin,
		stderr:      options.Err,
		environment: environment,
	})
	if trimCommandSubstitution(identity) != requiredManager {
		return refusal(pluginsSlotRefusal)
	}
	// The envelope owns the bounded run, the NDJSON transport and the credential
	// redaction; a caller that ran the manager itself would own a second copy.
	if _, status := runChild(abort, childSpec{
		argv:        installArgv(options.Contract, work, manager),
		directory:   prefix,
		stdin:       os.Stdin,
		stdout:      options.Out,
		stderr:      options.Err,
		environment: environment,
	}); status != 0 {
		return &Silent{Status: status}
	}
	if err := publishPermissions(prefix); err != nil {
		return refusal(pluginsHook + ": cannot publish the installed payload a+rX")
	}
	published = true
	return nil
}

// managerSelection is the manager this invocation runs. The fixture manager replaces
// the fixed launcher only outside production, which authorization has already proved
// is inside a fixture root; inside production no caller value is read at all.
func managerSelection(physicalPrefix, fixturePnpm string) string {
	if isProductionPrefix(physicalPrefix) || fixturePnpm == "" {
		return launcher
	}
	return fixturePnpm
}

// identityArgv is the reviewed identity call. Every manager invocation suppresses the
// update notifier: a build must not spend a registry round trip on a "newer pnpm is
// available" notice, and the strict-egress profile has to describe exactly the traffic
// the install needs.
func identityArgv(manager string) []string {
	return []string{manager, "--version", "--pm-on-fail=error", "--config.update-notifier=false"}
}

// installArgv is the frozen install command, in the predecessor's order. run-pnpm.sh
// appends its own `--reporter=ndjson`.
func installArgv(contract, work, manager string) []string {
	return []string{
		"sh", filepath.Join(contract, "run-pnpm.sh"), filepath.Join(work, "install.ndjson"), "--",
		manager, "install",
		"--config.update-notifier=false",
		"--frozen-lockfile", "--prod", "--no-optional",
		"--ignore-scripts", "--ignore-pnpmfile", "--pm-on-fail=error",
		"--registry=" + registryURL,
		"--package-import-method=copy", "--store-dir", filepath.Join(work, "store"),
	}
}

// childEnvironment is this process's environment as every child of this hook sees it.
//
// The two developer-only seams are removed rather than forwarded: only the developer
// path may re-pin, and no image build can reach it. The build's own soft-fail mode is
// cleared for the children the way `export AGENT_INSTALL_SOFT_FAIL=` did, because
// nothing below this hook may decide to soften.
//
// The private HOME and the four XDG roots are applied only once an invocation has
// created them: the environment guard runs before that, so it sees the caller's HOME,
// exactly as the shell's did before it exported the redirect.
func childEnvironment(work string) []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "AGENT_VM_OPENCODE_LOCK_CHECK", "AGENT_VM_OPENCODE_LOCK_PIN", "AGENT_INSTALL_SOFT_FAIL":
			continue
		case "HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME":
			if work != "" {
				continue
			}
		}
		environment = append(environment, entry)
	}
	environment = append(environment, "AGENT_INSTALL_SOFT_FAIL=")
	if work == "" {
		return environment
	}
	return append(environment,
		"HOME="+filepath.Join(work, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(work, "config"),
		"XDG_CACHE_HOME="+filepath.Join(work, "cache"),
		"XDG_DATA_HOME="+filepath.Join(work, "data"),
		"XDG_STATE_HOME="+filepath.Join(work, "state"),
	)
}

// createInvocationState is the predecessor's `mkdir -p` of the private HOME and the
// four XDG roots. The manager and the preparation see no caller or image cache,
// because every path they would write is invocation-owned and gone afterwards.
func createInvocationState(work string) error {
	for _, name := range []string{"home", "config", "cache", "data", "state"} {
		if err := os.MkdirAll(filepath.Join(work, name), 0o700); err != nil {
			return err
		}
	}
	return nil
}
