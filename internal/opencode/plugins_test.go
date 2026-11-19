package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The plugin gate runs programs that are not this repository's Go code: the `.mjs`
// plugin runtime and the released image's Python probe. The fixtures stand in for
// them by argv shape, which is the whole contract this driver has with them.

const fakePayloadNode = `#!/bin/sh
printf 'node|%s\n' "$*" >>"$CALLS"
script=${1##*/}
shift
case "$script" in
    check-lock.mjs)
        if [ -n "${CHECK_FAIL:-}" ]; then echo 'check-lock: package.json: bytes differ from the reviewed pin' >&2; exit 1; fi
        printf '%s\n' "${CHECK_SELECTION:-1.4.1}"
        ;;
    check-claude-payload.mjs)
        mode=$1
        if [ -n "${PAYLOAD_FAIL:-}" ] && [ "$PAYLOAD_FAIL" = "$mode" ]; then
            echo "OpenCode payload: fixture $mode failure" >&2
            exit 1
        fi
        case "$mode" in
            files)
                find "$2" -type f -print0
                ;;
            report)
                [ -s "$2" ] || { echo 'OpenCode payload: empty report' >&2; exit 1; }
                ;;
        esac
        ;;
    check-claude-runtime.mjs)
        if [ -n "${RUNTIME_FAIL:-}" ]; then
            echo 'fixture runtime failure' >&2
            exit 1
        fi
        printf '{"schema":1}\n'
        ;;
esac
exit 0
`

const fakeProbePython = `#!/bin/sh
printf 'python3|%s\n' "$*" >>"$CALLS"
script=${1##*/}
shift
case "$script" in
    probe-opencode.py)
        case "$1" in
            capture)
                if [ -n "${CAPTURE_FAIL:-}" ]; then echo 'fixture capture failure' >&2; exit 1; fi
                printf '{"schema":1}\n'
                ;;
            check)
                if [ -n "${PROBE_CHECK_FAIL:-}" ]; then echo 'probe-opencode: fixture check failure' >&2; exit 1; fi
                printf '{"models":1}\n'
                ;;
        esac
        ;;
esac
exit 0
`

// pluginsFixture is one installed-plugin gate case.
type pluginsFixture struct {
	*fixture
	contract  string
	source    string
	statusDir string
	claude    string
	opencode  string
	config    string
	extraFile string
}

func newPluginsFixture(t *testing.T) *pluginsFixture {
	t.Helper()
	tree := newFixture(t)
	contract := filepath.Join(tree.root, "contract")
	source := filepath.Join(tree.root, "source")
	for _, directory := range []string{contract, source, filepath.Join(source, "plugins")} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	if err := os.WriteFile(filepath.Join(contract, "run-report.sh"), []byte(fakeEnvelopeReport), 0o755); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
	tree.program(t, "node", fakePayloadNode)
	tree.program(t, "python3", fakeProbePython)

	gate := &pluginsFixture{
		fixture:   tree,
		contract:  contract,
		source:    source,
		statusDir: filepath.Join(tree.root, "status"),
		claude:    filepath.Join(tree.root, "native", ".local", "bin", "claude"),
		opencode:  filepath.Join(tree.root, "native", ".opencode", "bin", "opencode"),
		config:    filepath.Join(tree.root, "opencode.json"),
		extraFile: filepath.Join(tree.prefix, "index.js"),
	}
	for _, directory := range []string{filepath.Dir(gate.claude), filepath.Dir(gate.opencode), gate.statusDir} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	for path, body := range map[string]string{
		gate.prefix + "/index.js": "export { default } from \"@openchamber/opencode-claude\";\n",
		gate.claude:               "#!/bin/sh\nprintf 'fixture\\n'\n",
		gate.opencode:             "#!/bin/sh\nexit 0\n",
		gate.config:               `{"plugins":["` + gate.prefix + `"]}` + "\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return gate
}

func (g *pluginsFixture) options() PluginsOptions {
	return PluginsOptions{
		Source:         g.source,
		Contract:       g.contract,
		Prefix:         g.prefix,
		Claude:         g.claude,
		Opencode:       g.opencode,
		Config:         g.config,
		OpenCodeConfig: g.config,
		StatusDir:      g.statusDir,
		Access:         accessEnvironment(),
	}
}

func (g *pluginsFixture) run(t *testing.T) (string, string, error) {
	t.Helper()
	options := g.options()
	out, errOut := &strings.Builder{}, &strings.Builder{}
	options.Out, options.Err = out, errOut
	err := VerifyPlugins(context.Background(), options)
	return out.String(), errOut.String(), err
}

func (g *pluginsFixture) record(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.statusDir, pluginRecord))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	return string(data), true
}

// TestPluginsHealthy is the whole gate: every child in the reviewed order, the
// quiet payload audit, the two loud audits, and the record last.
func TestPluginsHealthy(t *testing.T) {
	tree := newPluginsFixture(t)
	stdout, stderr, err := tree.run(t)
	if err != nil {
		t.Fatalf("gate = %v (%s)", err, stderr)
	}
	tree.expectInvocations(t,
		"node|",
		"check-claude-payload.mjs config",
		"run-report|20 ",
		"check-claude-runtime.mjs",
		"check-claude-payload.mjs report",
		"run-report|90 ",
		"probe-opencode.py capture",
		"probe-opencode.py check",
		"check-claude-payload.mjs files",
	)
	invocations := tree.invocations(t)
	if !strings.Contains(invocations[0], "plugins/check-lock.mjs "+tree.prefix) {
		t.Fatalf("the checker did not receive the installed prefix: %q", invocations[0])
	}
	if !strings.Contains(invocations[4], "report ") || !strings.Contains(invocations[4], " 1.4.1") {
		t.Fatalf("the runtime report was checked against the wrong selection: %q", invocations[4])
	}
	if !strings.Contains(invocations[6], "--timeout 60") {
		t.Fatalf("the probe lost its deadline: %q", invocations[6])
	}
	if !strings.Contains(invocations[7], "--plugin-entry "+filepath.Join(tree.prefix, "index.js")) {
		t.Fatalf("the probe check lost its plugin entry: %q", invocations[7])
	}
	if !strings.Contains(invocations[8], "files "+tree.prefix) {
		t.Fatalf("the inventory did not walk the installed prefix: %q", invocations[8])
	}
	// The payload inventory is audited quietly; the config and the two commands are
	// loud, so the build log names what was certified.
	if strings.Contains(stdout, "tool-access: ok "+tree.extraFile) {
		t.Fatalf("the payload inventory was audited loudly: %q", stdout)
	}
	for _, path := range []string{tree.config, tree.claude, tree.opencode} {
		if !strings.Contains(stdout, "tool-access: ok ") || !strings.Contains(stdout, path) {
			t.Fatalf("stdout = %q, want an ok line naming %s", stdout, path)
		}
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	if got, found := tree.record(t); !found || got != "installed\n" {
		t.Fatalf("record = %q, %v, want installed", got, found)
	}
}

// TestPluginsSlotMismatch keeps the layer's request an assertion: a committed
// selection that is not the requested one fails with no diagnostic, as the
// predecessor's `[ ... ]` did under `set -e`.
func TestPluginsSlotMismatch(t *testing.T) {
	tree := newPluginsFixture(t)
	options := tree.options()
	options.Slot = "1.4.2"
	out, errOut := &strings.Builder{}, &strings.Builder{}
	options.Out, options.Err = out, errOut
	err := VerifyPlugins(context.Background(), options)
	mustSilent(t, err, 1)
	if out.String() != "" || errOut.String() != "" {
		t.Fatalf("a slot mismatch wrote %q / %q", out.String(), errOut.String())
	}
	tree.expectInvocations(t, "node|")
}

// TestPluginsImageConfigMustBeSelected is the guest-ENV rule: a stale `installed`
// record does not prove the image still sets OPENCODE_CONFIG to the shipped file.
func TestPluginsImageConfigMustBeSelected(t *testing.T) {
	for _, value := range []string{"", "/image/other.json"} {
		tree := newPluginsFixture(t)
		options := tree.options()
		options.OpenCodeConfig = value
		mustRefusal(t, VerifyPlugins(context.Background(), options), "opencode-claude: image config selection missing")
	}
}

// TestPluginsPreloadGuardFirst keeps the preload rejection before the checker: a
// preloaded module can patch node:crypto and subvert the digest comparison.
func TestPluginsPreloadGuardFirst(t *testing.T) {
	for _, key := range []string{"NODE_OPTIONS", "NODE_PATH", "BUN_OPTIONS"} {
		tree := newPluginsFixture(t)
		t.Setenv(key, "/tmp/injected")
		_, _, err := tree.run(t)
		mustRefusal(t, err, "manager runtime overrides are unsupported")
		tree.expectNoChild(t)
	}
}

// TestPluginsChildFailures keeps every delegated program's status and bytes: the
// gate adds a verdict only where the predecessor captured the child's stderr into
// a file and reprinted it.
func TestPluginsChildFailures(t *testing.T) {
	for name, testCase := range map[string]struct{ key, value, stderr string }{
		"checker":        {"CHECK_FAIL", "1", "check-lock: package.json: bytes differ from the reviewed pin\n"},
		"payload config": {"PAYLOAD_FAIL", "config", "OpenCode payload: fixture config failure\n"},
		"payload report": {"PAYLOAD_FAIL", "report", "OpenCode payload: fixture report failure\n"},
		"payload files":  {"PAYLOAD_FAIL", "files", "OpenCode payload: fixture files failure\n"},
		"runtime":        {"RUNTIME_FAIL", "1", "fixture runtime failure\n"},
		"probe capture":  {"CAPTURE_FAIL", "1", "fixture capture failure\n"},
		"probe check":    {"PROBE_CHECK_FAIL", "1", "probe-opencode: fixture check failure\n"},
	} {
		t.Run(name, func(t *testing.T) {
			tree := newPluginsFixture(t)
			t.Setenv(testCase.key, testCase.value)
			_, stderr, err := tree.run(t)
			mustSilent(t, err, 1)
			if !strings.Contains(stderr, testCase.stderr) {
				t.Fatalf("%s=%s: stderr = %q, want %q", testCase.key, testCase.value, stderr, testCase.stderr)
			}
			if _, found := tree.record(t); found {
				t.Fatalf("%s=%s: a failed gate wrote the record", testCase.key, testCase.value)
			}
		})
	}
}

// TestPluginsAccessAudits keeps all three audits hard: the payload inventory, the
// config the guest loads, and the two commands the plugin hands work to.
func TestPluginsAccessAudits(t *testing.T) {
	t.Run("payload inventory", func(t *testing.T) {
		tree := newPluginsFixture(t)
		secret := filepath.Join(tree.prefix, "private.js")
		if err := os.WriteFile(secret, []byte("export default {};\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := tree.run(t)
		mustSilent(t, err, 1)
		if !strings.Contains(stderr, "tool-access: ") || !strings.Contains(stderr, "private.js") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
	t.Run("config", func(t *testing.T) {
		tree := newPluginsFixture(t)
		if err := os.Chmod(tree.config, 0o600); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := tree.run(t)
		mustSilent(t, err, 1)
		if !strings.Contains(stderr, "tool-access: ") || !strings.Contains(stderr, "opencode.json") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
	t.Run("claude command", func(t *testing.T) {
		tree := newPluginsFixture(t)
		if err := os.Chmod(tree.claude, 0o600); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := tree.run(t)
		mustSilent(t, err, 1)
		if !strings.Contains(stderr, "tool-access: ") || !strings.Contains(stderr, "claude") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
	t.Run("opencode command", func(t *testing.T) {
		tree := newPluginsFixture(t)
		if err := os.Remove(tree.opencode); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := tree.run(t)
		mustSilent(t, err, 1)
		if !strings.Contains(stderr, "tool-access: ") {
			t.Fatalf("stderr = %q", stderr)
		}
	})
}

// TestPluginsAuditsRunLast pins the order the predecessor documented: nothing is
// certified before the runtime and the probe have run.
func TestPluginsAuditsRunLast(t *testing.T) {
	tree := newPluginsFixture(t)
	if err := os.Chmod(tree.config, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := tree.run(t)
	mustSilent(t, err, 1)
	// Every child ran: the access audit is the last gate, not an early one.
	tree.expectInvocations(t, "node|", "config", "run-report|20 ", "check-claude-runtime.mjs",
		"report", "run-report|90 ", "probe-opencode.py capture", "probe-opencode.py check", "files")
}

// TestPluginsEmptyInventory documents the one case the port changed: the
// predecessor's `xargs -0` refused an empty inventory, and this driver audits
// nothing. It is unreachable for a real prefix -- `check-claude-payload.mjs config`
// asserts `index.js` -- so the difference cannot certify an empty payload.
func TestPluginsEmptyInventory(t *testing.T) {
	tree := newPluginsFixture(t)
	empty := filepath.Join(tree.root, "empty-prefix")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	options := tree.options()
	options.Prefix = empty
	options.Claude, options.Opencode = tree.claude, tree.opencode
	var out, errOut strings.Builder
	options.Out, options.Err = &out, &errOut
	if err := VerifyPlugins(context.Background(), options); err != nil {
		t.Fatalf("gate = %v (%s)", err, errOut.String())
	}
}
