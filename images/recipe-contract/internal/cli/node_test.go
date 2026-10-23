package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

// nodeFixture copies the committed dsh fixture so a case can mutate or pin it.
func nodeFixture(t *testing.T) string {
	t.Helper()
	source := "../../../../script/test/fixtures/pnpm-lock/tiny-dsh"
	destination := t.TempDir()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		payload, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return destination
}

func runNode(t *testing.T, environ []string, args ...string) result {
	t.Helper()
	return runInProcess(t, Environment{Variables: environ}, args...)
}

func TestPnpmLockArgv(t *testing.T) {
	prefix := nodeFixture(t)
	for _, test := range []struct {
		name   string
		args   []string
		status int
		stdout string
		stderr string
	}{
		{"check", []string{"pnpm-lock", "check", "dsh", prefix}, 0, "0.1.5-rc.2\n", ""},
		{"no-verb", []string{"pnpm-lock"}, 2, "", "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"},
		{"no-kind", []string{"pnpm-lock", "check"}, 2, "", "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"},
		{"bad-verb", []string{"pnpm-lock", "wat", "dsh", prefix}, 2, "", "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"},
		{"bad-kind", []string{"pnpm-lock", "check", "copilot", prefix}, 2, "", "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"},
		{"empty-prefix", []string{"pnpm-lock", "graph", "dsh", ""}, 2, "", "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"},
		{"extra", []string{"pnpm-lock", "check", "dsh", prefix, "extra"}, 2, "", "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"},
		{"help", []string{"pnpm-lock", "--help"}, 2, "", "usage: recipe-contract pnpm-lock check|graph|pin|peers dsh|pi|bridge PREFIX\n"},
		{"peers-non-dsh", []string{"pnpm-lock", "peers", "pi", prefix}, 1, "", "pnpm-lock: " + "peer reporting is dsh-only\n"},
		{"absent", []string{"pnpm-lock", "check", "dsh", filepath.Join(prefix, "absent")}, 1, "", ""},
	} {
		got := runNode(t, nil, test.args...)
		if got.status != test.status {
			t.Errorf("%s status = %d, want %d (%s)", test.name, got.status, test.status, got.stderr)
		}
		if test.stdout != "" && string(got.stdout) != test.stdout {
			t.Errorf("%s stdout = %q, want %q", test.name, got.stdout, test.stdout)
		}
		if test.stderr != "" && string(got.stderr) != test.stderr {
			t.Errorf("%s stderr = %q, want %q", test.name, got.stderr, test.stderr)
		}
		if test.status == 1 && test.stderr == "" {
			if len(got.stdout) != 0 {
				t.Errorf("%s leaked stdout on failure", test.name)
			}
			if !strings.HasPrefix(string(got.stderr), "pnpm-lock: ") {
				t.Errorf("%s stderr = %q", test.name, got.stderr)
			}
		}
		if test.status == 0 && len(got.stderr) != 0 {
			t.Errorf("%s leaked stderr on success: %q", test.name, got.stderr)
		}
	}
	peers := runNode(t, nil, "pnpm-lock", "peers", "dsh", prefix)
	if peers.status != 0 || !strings.HasPrefix(string(peers.stdout), `{"exact":`) {
		t.Errorf("peers = %d %q", peers.status, peers.stdout)
	}
	if !strings.HasSuffix(string(peers.stdout), "\n") || strings.Count(string(peers.stdout), "\n") != 1 {
		t.Errorf("peers stdout is not one line: %q", peers.stdout)
	}
	if pin := runNode(t, nil, "pnpm-lock", "pin", "dsh", prefix); pin.status != 0 || len(pin.stdout) != 0 {
		t.Errorf("pin = %d %q", pin.status, pin.stdout)
	}
}

func TestPnpmEnvironmentArgv(t *testing.T) {
	clean := t.TempDir()
	if got := runNode(t, []string{"PATH=/usr/bin"}, "pnpm-environment"); got.status != 0 || len(got.stdout) != 0 || len(got.stderr) != 0 {
		t.Errorf("clean = %d %q %q", got.status, got.stdout, got.stderr)
	}
	if got := runNode(t, []string{"PATH=/usr/bin"}, "pnpm-environment", clean); got.status != 0 {
		t.Errorf("prefix = %d %q", got.status, got.stderr)
	}
	got := runNode(t, []string{"NODE_OPTIONS=--x"}, "pnpm-environment")
	if got.status != 1 || string(got.stderr) != "pnpm environment: "+"unsupported manager runtime override: NODE_OPTIONS\n" {
		t.Errorf("node options = %d %q", got.status, got.stderr)
	}
}

func TestPnpmYAMLArgv(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "value.yaml")
	if err := os.WriteFile(path, []byte("x: 1\ny: [a]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runNode(t, nil, "pnpm-yaml", path)
	if got.status != 0 || string(got.stdout) != "{\"x\":1,\"y\":[\"a\"]}\n" {
		t.Errorf("pnpm-yaml = %d %q", got.status, got.stdout)
	}
	if got := runNode(t, nil, "pnpm-yaml"); got.status != 2 || string(got.stderr) != "usage: recipe-contract pnpm-yaml FILE\n" {
		t.Errorf("usage = %d %q", got.status, got.stderr)
	}
	if err := os.WriteFile(path, []byte("x: ! 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runNode(t, nil, "pnpm-yaml", path); got.status != 1 || !strings.HasPrefix(string(got.stderr), "pnpm-yaml: ") {
		t.Errorf("tagged = %d %q", got.status, got.stderr)
	}
}

func TestDshLockUpdateArgv(t *testing.T) {
	before := nodeFixture(t)
	for _, test := range []struct {
		name   string
		args   []string
		status int
		stdout string
	}{
		{"equal", []string{"dsh-lock-update", before, before, ""}, 0, "dsh lock update: equal 0.1.5-rc.2 -> 0.1.5-rc.2; 1 exact peers satisfied\n"},
		{"json", []string{"dsh-lock-update", before, before, "", "--json"}, 0, ""},
		{"short", []string{"dsh-lock-update", before, before}, 2, "usage: recipe-contract dsh-lock-update OLD_PREFIX NEW_PREFIX DSH_VERSION [--refresh-lock] [--json]\n"},
		{"unknown-option", []string{"dsh-lock-update", before, before, "", "--unknown"}, 2, "usage: recipe-contract dsh-lock-update OLD_PREFIX NEW_PREFIX DSH_VERSION [--refresh-lock] [--json]\n"},
		{"repeated-option", []string{"dsh-lock-update", before, before, "", "--refresh-lock", "--refresh-lock"}, 2, "usage: recipe-contract dsh-lock-update OLD_PREFIX NEW_PREFIX DSH_VERSION [--refresh-lock] [--json]\n"},
		{"bad-version", []string{"dsh-lock-update", before, before, "latest"}, 1, ""},
	} {
		got := runNode(t, nil, test.args...)
		if got.status != test.status {
			t.Errorf("%s status = %d, want %d (%s)", test.name, got.status, test.status, got.stderr)
		}
		if test.stdout != "" && (string(got.stdout) != test.stdout && string(got.stderr) != test.stdout) {
			t.Errorf("%s output = %q / %q", test.name, got.stdout, got.stderr)
		}
	}
	json := runNode(t, nil, "dsh-lock-update", before, before, "", "--json")
	if !strings.HasPrefix(string(json.stdout), `{"mode":"equal"`) || !strings.HasSuffix(string(json.stdout), "\n") {
		t.Errorf("--json = %q", json.stdout)
	}
}

func TestDshCordisOverridesArgv(t *testing.T) {
	prefix := t.TempDir()
	for _, test := range []struct {
		name   string
		args   []string
		status int
	}{
		{"none", []string{"dsh-cordis-overrides"}, 2},
		{"unknown-flag", []string{"dsh-cordis-overrides", prefix, "--unknown", "x"}, 2},
		{"odd", []string{"dsh-cordis-overrides", prefix, "--before"}, 2},
		{"both", []string{"dsh-cordis-overrides", prefix, "--propose", "a", "--cordis-overrides", "b"}, 2},
		{"repeated", []string{"dsh-cordis-overrides", prefix, "--before", "a", "--before", "b"}, 2},
		{"empty-value", []string{"dsh-cordis-overrides", prefix, "--before", ""}, 2},
	} {
		got := runNode(t, nil, test.args...)
		if got.status != test.status {
			t.Errorf("%s status = %d, want %d (%q)", test.name, got.status, test.status, got.stderr)
		}
		if test.status == 2 && string(got.stderr) != "usage: recipe-contract dsh-cordis-overrides TENTATIVE_PREFIX [--before COMMITTED_PREFIX] [--propose FILE | --cordis-overrides FILE]\n" {
			t.Errorf("%s usage = %q", test.name, got.stderr)
		}
	}
}

func TestDshUpgradeStageArgv(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		match  string
		status int
	}{
		{"no-action", []string{"dsh-upgrade-stage"}, "missing argument: before", 1},
		{"no-stage", []string{"dsh-upgrade-stage", "tentative", "/tmp/x"}, "lstat", 1},
		{"no-argument", []string{"dsh-upgrade-stage", "tentative", "/tmp/x", "/tmp/y"}, "lstat", 1},
		{"unknown-action", []string{"dsh-upgrade-stage", "wat", nodeFixture(t), "/tmp/y", "/tmp/z"}, "", 1},
	} {
		got := runNode(t, nil, test.args...)
		if got.status != test.status {
			t.Errorf("%s status = %d", test.name, got.status)
		}
		if !strings.HasPrefix(string(got.stderr), "dsh upgrade: ") {
			t.Errorf("%s stderr = %q", test.name, got.stderr)
		}
		if test.match != "" && !strings.Contains(string(got.stderr), test.match) {
			t.Errorf("%s stderr = %q, want %q", test.name, got.stderr, test.match)
		}
		if test.name == "unknown-action" && !strings.Contains(string(got.stderr), "unknown developer staging action") {
			t.Errorf("unknown action = %q", got.stderr)
		}
	}
}

func TestDshValidationOrder(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "absent")
	for _, args := range [][]string{
		{"dsh-lock-update", absent, absent, "latest"},
		{"dsh-upgrade-stage", "tentative", absent},
		{"dsh-upgrade-stage", "tentative", absent, "stage"},
	} {
		result := runNode(t, nil, args...)
		if result.status != 1 || !strings.Contains(string(result.stderr), "lstat") {
			t.Fatalf("%v: %d %s", args, result.status, result.stderr)
		}
	}
	result := runNode(t, nil, "dsh-upgrade-stage", "tentative", nodeFixture(t), filepath.Join(t.TempDir(), "stage"), "")
	if result.status != 1 || string(result.stderr) != "dsh upgrade: expected target exact selection\n" {
		t.Fatalf("%d %s", result.status, result.stderr)
	}
}

func TestNodeStdoutWriteFailure(t *testing.T) {
	prefix := nodeFixture(t)
	status := Run([]string{"pnpm-lock", "check", "dsh", prefix}, Environment{},
		Streams{Out: failingWriter{}, Err: &recorder{}})
	if status != 1 {
		t.Fatalf("stdout failure status = %d", status)
	}
}

func TestNodePanicBecomesStatusOne(t *testing.T) {
	status := (&process{streams: Streams{Out: &recorder{}, Err: &recorder{}}}).nodeCommand("x: ",
		func() (string, error) { panic("boom") })
	if status != 1 {
		t.Fatalf("panic status = %d", status)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

type recorder struct{ payload []byte }

func (r *recorder) Write(source []byte) (int, error) {
	r.payload = append(r.payload, source...)
	return len(source), nil
}

var _ = pnpmlock.KindDsh
