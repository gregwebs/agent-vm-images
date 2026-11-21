package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The plugins hook's fixture children. `contract` is the composed directory's helper,
// which reduces the shared environment guard to the one variable this suite drives;
// `prepare` is the preparation driver, which refuses a leaked developer seam; `pnpm`
// is the manager and `envelope` the run-pnpm.sh it runs through.
const fakeContractHelper = `#!/bin/sh
printf 'contract|%s\n' "$*" >>"$CALLS"
[ "$1" = pnpm-environment ] || { echo "contract: unknown verb $1" >&2; exit 90; }
case "${PNPM_CONFIG_TRUST_LOCKFILE:-}" in
    "") ;;
    *) echo 'pnpm environment: unsupported package-manager environment: PNPM_CONFIG_TRUST_LOCKFILE' >&2; exit 1 ;;
esac
exit 0
`

const fakePrepareLock = `#!/bin/sh
printf 'prepare|%s\n' "$*" >>"$CALLS"
printf 'home|%s|%s\n' "$HOME" "$XDG_CONFIG_HOME" >>"$CALLS"
[ -z "${AGENT_VM_OPENCODE_LOCK_CHECK:-}" ] || { echo 'prepare-lock: the checker seam leaked' >&2; exit 93; }
[ -z "${AGENT_VM_OPENCODE_LOCK_PIN:-}" ] || { echo 'prepare-lock: the pin writer seam leaked' >&2; exit 93; }
[ -z "${AGENT_INSTALL_SOFT_FAIL:-}" ] || { echo 'prepare-lock: soft-fail mode leaked' >&2; exit 94; }
exit 0
`

const fakeComposedEnvelope = `#!/bin/sh
printf 'envelope|%s\n' "$*" >>"$CALLS"
result=$1
shift 2
"$@"
status=$?
# The real envelope publishes the manager's diagnostics and reports every failure as
# its own hard failure, which is why this hook reports that status.
[ "$status" -eq 0 ] || status=${ENVELOPE_STATUS:-1}
exit "$status"
`

const fakePluginsManager = `#!/bin/sh
printf 'pnpm|%s\n' "$*" >>"$CALLS"
if [ "$1" = --version ]; then
    printf '%s\n' "${MANAGER_VERSION-11.11.0}"
    exit "${MANAGER_STATUS:-0}"
fi
if [ -n "${STARTED:-}" ]; then
    : >"$STARTED"
    while [ ! -e "${RELEASE:-/nowhere}" ]; do sleep 0.05; done
fi
mkdir -p node_modules/.pnpm
printf 'partial\n' >node_modules/.pnpm/fixture
exit "${RESULT:-0}"
`

// installPluginsFixture is one install-plugins case: an authorized prefix, the composed
// directory's two programs and the fixture manager.
type installPluginsFixture struct {
	*fixture
	contract string
	source   string
	manager  string
}

func newPluginsInstallFixture(t *testing.T) *installPluginsFixture {
	t.Helper()
	tree := newFixture(t)
	state := &installPluginsFixture{
		fixture:  tree,
		contract: filepath.Join(tree.root, "contract"),
		source:   filepath.Join(tree.root, "opencode-source"),
		manager:  tree.program(t, "fixture-pnpm", fakePluginsManager),
	}
	for path, body := range map[string]string{
		filepath.Join(state.contract, "recipe-contract"):          fakeContractHelper,
		filepath.Join(state.contract, "run-pnpm.sh"):              fakeComposedEnvelope,
		filepath.Join(state.source, "plugins", "prepare-lock.sh"): fakePrepareLock,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

func (f *installPluginsFixture) options() InstallPluginsOptions {
	return InstallPluginsOptions{
		Contract:    f.contract,
		Source:      f.source,
		Prefix:      f.prefix,
		FixtureRoot: f.root,
		FixturePnpm: f.manager,
	}
}

// run executes one install with captured streams.
func (f *installPluginsFixture) run(t *testing.T, options InstallPluginsOptions) (string, string, error) {
	t.Helper()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	options.Out, options.Err = out, errOut
	err := InstallPlugins(context.Background(), options)
	return out.String(), errOut.String(), err
}

// TestInstallPluginsFreezesTheInstall is the whole success path: the guard runs first,
// preparation runs with a private HOME and XDG tree, the manager's identity is the pin,
// and the install runs through the composed envelope with the reviewed argv.
func TestInstallPluginsFreezesTheInstall(t *testing.T) {
	tree := newPluginsInstallFixture(t)
	options := tree.options()
	options.PluginVersion = "1.4.1"
	// A published input the read-only runtime UIDs must be able to read: `chmod -R
	// a+rX` over the prefix is the last step of a successful install.
	published := filepath.Join(tree.prefix, "index.js")
	if err := os.WriteFile(published, []byte("export {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := tree.run(t, options); err != nil {
		t.Fatalf("install = %v (stderr %q)", err, stderr)
	}
	info, err := os.Stat(published)
	if err != nil {
		t.Fatalf("stat the published input: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("published mode = %04o, want 0644", info.Mode().Perm())
	}
	calls := tree.invocations(t)
	if len(calls) < 6 {
		t.Fatalf("child invocations = %q, want the guard, preparation, identity, envelope and manager", calls)
	}
	for index, want := range []string{
		"contract|pnpm-environment " + tree.prefix,
		"prepare|" + tree.prefix + " 1.4.1",
		"home|",
		"pnpm|--version --pm-on-fail=error --config.update-notifier=false",
		"envelope|",
		"pnpm|install --config.update-notifier=false --frozen-lockfile --prod --no-optional " +
			"--ignore-scripts --ignore-pnpmfile --pm-on-fail=error --registry=https://registry.npmjs.org/ " +
			"--package-import-method=copy --store-dir ",
	} {
		if !strings.Contains(calls[index], want) {
			t.Fatalf("child invocation %d = %q, want it to contain %q (all: %q)", index, calls[index], want, calls)
		}
	}
	// The manager ran through the envelope, in the prefix, with this invocation's own
	// store: the envelope's argv is the frozen one.
	if !strings.Contains(calls[4], tree.manager+" install") {
		t.Fatalf("envelope argv = %q, want the fixture manager's install", calls[4])
	}
	if _, err := os.Stat(filepath.Join(tree.prefix, "node_modules", ".pnpm", "fixture")); err != nil {
		t.Fatalf("a successful install removed the payload: %v", err)
	}
	// The private HOME and XDG tree were invocation-owned and are gone, which is what
	// keeps a caller or image cache out of the install.
	private := strings.Split(calls[2], "|")
	if private[1] == os.Getenv("HOME") {
		t.Fatalf("preparation ran with the caller's HOME: %q", calls[2])
	}
	for _, path := range private[1:] {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invocation state %q survived the run: %v", path, err)
		}
	}
}

// TestInstallPluginsRefusesAmbientPolicy is the shared environment guard: it runs from
// the composed directory, before any manager process, and its status is the hook's.
func TestInstallPluginsRefusesAmbientPolicy(t *testing.T) {
	tree := newPluginsInstallFixture(t)
	t.Setenv("PNPM_CONFIG_TRUST_LOCKFILE", "unsafe")
	stdout, stderr, err := tree.run(t, tree.options())
	mustSilent(t, err, 1)
	if stdout != "" || !strings.Contains(stderr, "unsupported package-manager environment") {
		t.Fatalf("streams = %q / %q, want the guard's own line", stdout, stderr)
	}
	tree.expectInvocations(t, "contract|pnpm-environment")
}

// TestInstallPluginsRefusesPreload: a preloaded module can patch node:crypto and
// subvert the reviewed-bytes check, so the guard precedes every child.
func TestInstallPluginsRefusesPreload(t *testing.T) {
	for _, key := range []string{"NODE_OPTIONS", "NODE_PATH", "BUN_OPTIONS"} {
		t.Run(key, func(t *testing.T) {
			tree := newPluginsInstallFixture(t)
			t.Setenv(key, "/tmp/injected")
			// A partial tree from an earlier attempt: the predecessor's EXIT trap was
			// registered before this guard, so refusing still removed it.
			partial := filepath.Join(tree.prefix, "node_modules", ".pnpm", "fixture")
			if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(partial, []byte("partial\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			_, _, err := tree.run(t, tree.options())
			mustRefusal(t, err, "manager runtime overrides are unsupported")
			tree.expectNoChild(t)
			if _, err := os.Stat(filepath.Join(tree.prefix, "node_modules")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a refused run left a partial node_modules: %v", err)
			}
		})
	}
}

// TestInstallPluginsRemovesTheDeveloperSeams: only the developer path may re-pin, so
// the two seams are not forwarded to preparation, and the build's soft-fail mode does
// not reach it either.
func TestInstallPluginsRemovesTheDeveloperSeams(t *testing.T) {
	tree := newPluginsInstallFixture(t)
	t.Setenv("AGENT_VM_OPENCODE_LOCK_CHECK", "/bin/false")
	t.Setenv("AGENT_VM_OPENCODE_LOCK_PIN", filepath.Join(tree.root, "sentinel-pin"))
	t.Setenv("AGENT_INSTALL_SOFT_FAIL", "1")
	if _, stderr, err := tree.run(t, tree.options()); err != nil {
		t.Fatalf("install = %v (stderr %q)", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(tree.root, "sentinel-pin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a forwarded seam reached preparation: %v", err)
	}
}

// TestInstallPluginsPrefixAuthorization is the fixture-root rule: only a directory
// beneath the authorized root may be installed into, and the rule runs before the
// first child.
func TestInstallPluginsPrefixAuthorization(t *testing.T) {
	tree := newPluginsInstallFixture(t)
	elsewhere := t.TempDir()
	file := tree.write(t, "not-a-directory", "", 0o644)
	link := filepath.Join(tree.root, "link")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	for _, prefix := range []string{file, tree.root, elsewhere, link, filepath.Join(elsewhere, "prefix")} {
		options := tree.options()
		options.Prefix = prefix
		_, _, err := tree.run(t, options)
		mustSilent(t, err, 1)
	}
	tree.expectNoChild(t)
	// A non-production prefix with no root named is the predecessor's `${:?}`.
	options := tree.options()
	options.FixtureRoot = ""
	_, _, err := tree.run(t, options)
	mustUsage(t, err, "non-production prefix requires fixture root")
	tree.expectNoChild(t)
}

// TestInstallPluginsPrefixSiblings keeps the containment rule from being a string
// prefix test: a sibling of the authorized root shares its name and is not inside it.
func TestInstallPluginsPrefixSiblings(t *testing.T) {
	tree := newPluginsInstallFixture(t)
	sibling := tree.root + "-sibling"
	if err := os.MkdirAll(filepath.Join(sibling, "prefix"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	options := tree.options()
	options.Prefix = filepath.Join(sibling, "prefix")
	_, _, err := tree.run(t, options)
	mustSilent(t, err, 1)
	tree.expectNoChild(t)
}

// TestInstallPluginsManagerIdentity is `pnpm --version` against the pin: another
// manager is refused before the install, and the reviewed comparison is the report
// alone, so a manager that prints the pin and then stumbles is accepted here.
func TestInstallPluginsManagerIdentity(t *testing.T) {
	t.Run("another manager", func(t *testing.T) {
		tree := newPluginsInstallFixture(t)
		t.Setenv("MANAGER_VERSION", "12.9.1")
		_, _, err := tree.run(t, tree.options())
		mustRefusal(t, err, "install-opencode-plugins: pnpm 11.11.0 is required")
		tree.expectInvocations(t,
			"contract|pnpm-environment", "prepare|", "home|", "pnpm|--version")
	})
	t.Run("report outranks the status", func(t *testing.T) {
		tree := newPluginsInstallFixture(t)
		t.Setenv("MANAGER_STATUS", "1")
		if _, _, err := tree.run(t, tree.options()); err != nil {
			t.Fatalf("install = %v, want the pinned report accepted", err)
		}
	})
	t.Run("no report", func(t *testing.T) {
		tree := newPluginsInstallFixture(t)
		t.Setenv("MANAGER_VERSION", "")
		_, _, err := tree.run(t, tree.options())
		mustRefusal(t, err, "install-opencode-plugins: pnpm 11.11.0 is required")
	})
}

// TestInstallPluginsIsHardFailureOnly: this consumer never softens, whatever
// AGENT_INSTALL_SOFT_FAIL says, and a failed install leaves no partial payload.
func TestInstallPluginsIsHardFailureOnly(t *testing.T) {
	for _, result := range []string{"1", "75", "124"} {
		t.Run("status "+result, func(t *testing.T) {
			tree := newPluginsInstallFixture(t)
			t.Setenv("RESULT", result)
			t.Setenv("AGENT_INSTALL_SOFT_FAIL", "1")
			_, _, err := tree.run(t, tree.options())
			mustSilent(t, err, 1)
			if _, err := os.Stat(filepath.Join(tree.prefix, "node_modules")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a failed install left a partial node_modules: %v", err)
			}
		})
	}
}

// TestInstallPluginsEnvelopeStatus: a failed envelope keeps its own status, because the
// caller's trap and the build log both read it.
func TestInstallPluginsEnvelopeStatus(t *testing.T) {
	tree := newPluginsInstallFixture(t)
	t.Setenv("RESULT", "1")
	t.Setenv("ENVELOPE_STATUS", "7")
	_, _, err := tree.run(t, tree.options())
	mustSilent(t, err, 7)
	if _, err := os.Stat(filepath.Join(tree.prefix, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed install left a partial node_modules: %v", err)
	}
}

// TestInstallPluginsSignalStatus: the hook's caller has to keep recognising 130/143/129,
// so a signal outranks the failure it caused and the partial payload is still removed.
func TestInstallPluginsSignalStatus(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		signal syscall.Signal
		status int
	}{
		{"interrupt", syscall.SIGINT, 130},
		{"terminate", syscall.SIGTERM, 143},
		{"hangup", syscall.SIGHUP, 129},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			tree := newPluginsInstallFixture(t)
			started := filepath.Join(tree.root, "started")
			t.Setenv("STARTED", started)
			t.Setenv("RELEASE", filepath.Join(tree.root, "release-never-written"))
			options := tree.options()
			failures := make(chan error, 1)
			go func() {
				_, _, err := tree.run(t, options)
				failures <- err
			}()
			deadline := time.Now().Add(20 * time.Second)
			for {
				if _, err := os.Stat(started); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the manager never started")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := syscall.Kill(os.Getpid(), testCase.signal); err != nil {
				t.Fatalf("signal: %v", err)
			}
			var err error
			select {
			case err = <-failures:
			case <-time.After(30 * time.Second):
				t.Fatal("the install never returned")
			}
			var interrupted *Interrupted
			if !errors.As(err, &interrupted) || interrupted.Status != testCase.status {
				t.Fatalf("install = %v (%T), want the signal status %d", err, err, testCase.status)
			}
			if _, err := os.Stat(filepath.Join(tree.prefix, "node_modules")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("an interrupted install left a partial node_modules: %v", err)
			}
		})
	}
}

// TestManagerSelection keeps the fixture manager inside the fixture: production runs the
// fixed launcher and reads no caller value.
func TestManagerSelection(t *testing.T) {
	for _, testCase := range []struct{ name, prefix, fixture, want string }{
		{"production", productionPrefix, "/elsewhere/pnpm", launcher},
		{"production ignores the fixture", productionPrefix, "", launcher},
		{"fixture without a manager", "/tmp/root/prefix", "", launcher},
		{"fixture manager", "/tmp/root/prefix", "/tmp/root/bin/pnpm", "/tmp/root/bin/pnpm"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := managerSelection(testCase.prefix, testCase.fixture); got != testCase.want {
				t.Fatalf("managerSelection = %q, want %q", got, testCase.want)
			}
		})
	}
}
