package cli

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/toolaccess"
)

// fixtureLauncher is the reviewed launcher the fixture publishes. It answers the
// calls the certification makes -- the report and the five defaults -- so a case
// can move one answer without a Node process.
const fixtureLauncher = `#!/bin/sh
if [ "${1-}" = --version ]; then printf '%s\n' "${FIXTURE_REPORT}"; exit 0; fi
if [ -n "${FIXTURE_DEFAULT_KEY-}" ] && [ "${3-}" = "$FIXTURE_DEFAULT_KEY" ]; then
    printf '%s\n' "${FIXTURE_DEFAULT}"; exit 0
fi
case "${3-}" in
    minimum-release-age) printf '1440\n' ;;
    minimum-release-age-strict) printf 'true\n' ;;
    minimum-release-age-ignore-missing-time) printf 'false\n' ;;
    trust-policy) printf 'no-downgrade\n' ;;
    ignore-scripts) printf 'true\n' ;;
    *) printf 'unreviewed\n' ;;
esac
`

// pnpmFixture is a staged fixture image: the reviewed source mount next to an
// installed distribution the certification can walk.
type pnpmFixture struct {
	root        string
	destination string
	source      string
	prefix      string
	commands    string
}

// stagePnpmFixture builds the fixture under a world-traversable root, because the
// access audit is what the certification runs.
func stagePnpmFixture(t *testing.T) *pnpmFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatalf("cannot resolve the temporary root: %v", err)
	}
	root, err := os.MkdirTemp(base, "pnpm-cli-")
	if err != nil {
		t.Fatalf("cannot create the fixture root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("cannot open the fixture root: %v", err)
	}
	staged := &pnpmFixture{
		root:        root,
		destination: filepath.Join(root, "image"),
		source:      filepath.Join(root, "source"),
	}
	staged.prefix = filepath.Join(staged.destination, "opt/agent-vm/pnpm")
	staged.commands = filepath.Join(staged.destination, "usr/local/bin")
	for _, directory := range []string{
		staged.destination, staged.source, staged.prefix,
		filepath.Join(staged.prefix, "bin"), filepath.Join(staged.prefix, "dist"),
		staged.commands, filepath.Join(staged.destination, "usr/bin"),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("cannot create %s: %v", directory, err)
		}
	}
	bundle := []byte("pristine fixture bundle")
	pin := staged.pin(t, bundle)
	staged.write(t, filepath.Join(staged.source, "pin.json"), pin, 0o644)
	staged.write(t, filepath.Join(staged.source, "launcher.sh"), []byte(fixtureLauncher), 0o644)
	staged.write(t, filepath.Join(staged.prefix, "pin.json"), pin, 0o644)
	staged.write(t, filepath.Join(staged.prefix, "dist/pnpm.mjs"), bundle, 0o644)
	staged.write(t, filepath.Join(staged.prefix, "bin/pnpm.mjs"), []byte("entry"), 0o755)
	staged.write(t, filepath.Join(staged.prefix, "bin/pnpx.mjs"), []byte("alias entry"), 0o755)
	staged.write(t, filepath.Join(staged.commands, "pnpm"), []byte(fixtureLauncher), 0o755)
	staged.write(t, filepath.Join(staged.destination, "usr/bin/node"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	for _, name := range []string{"pnx", "pnpx"} {
		if err := os.Symlink("pnpm", filepath.Join(staged.commands, name)); err != nil {
			t.Fatalf("cannot link %s: %v", name, err)
		}
	}
	return staged
}

// pin is a self-consistent pin document for the fixture bundle.
func (f *pnpmFixture) pin(t *testing.T, bundle []byte) []byte {
	t.Helper()
	sum := sha512.Sum512([]byte("fixture archive"))
	digest := hex.EncodeToString(sum[:])
	document := map[string]any{
		"version":        "11.11.0",
		"url":            "https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz",
		"integrity":      "sha512-" + base64.StdEncoding.EncodeToString(sum[:]),
		"sha512":         digest,
		"bundleSha256":   hex.EncodeToString(sum256(bundle)),
		"upstreamTag":    "v11.11.0",
		"upstreamCommit": strings.Repeat("b", 40),
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("cannot marshal the pin: %v", err)
	}
	return raw
}

func sum256(payload []byte) []byte {
	sum := sha256.Sum256(payload)
	return sum[:]
}

func (f *pnpmFixture) write(t *testing.T, path string, payload []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, payload, mode); err != nil {
		t.Fatalf("cannot write %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("cannot set the mode of %s: %v", path, err)
	}
}

// environment is the fixture's process view: the fixture contract, the reviewed
// source mount and a PATH whose first candidate is the published launcher.
func (f *pnpmFixture) environment(t *testing.T) Environment {
	t.Helper()
	t.Setenv("AGENT_VM_FIXTURE_ROOT", f.root)
	t.Setenv("PNPM_FIXTURE_DESTINATION", f.destination)
	t.Setenv("PNPM_SOURCE_DIR", f.source)
	t.Setenv("PATH", f.commands+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FIXTURE_REPORT", "11.11.0")
	t.Setenv("FIXTURE_DEFAULT_KEY", "")
	t.Setenv("FIXTURE_DEFAULT", "")
	return testEnv(toolaccess.PhysicalDirectoryOf(f.root), os.Getenv("PATH"))
}

// The default case: every certification key answers with its reviewed value.
func TestPnpmVerifyCertifyStatuses(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *pnpmFixture)
		status int
		stderr string
		stdout string
	}{
		{name: "reviewed installation", status: 0, stdout: "tool-access: ok "},
		{
			name: "the exact report",
			change: func(t *testing.T, _ *pnpmFixture) {
				t.Setenv("FIXTURE_REPORT", "11.10.0")
			},
			status: 1,
			stderr: "pnpm: exact report mismatch\n",
		},
		{
			name: "the five defaults",
			change: func(t *testing.T, _ *pnpmFixture) {
				t.Setenv("FIXTURE_DEFAULT_KEY", "minimum-release-age")
				t.Setenv("FIXTURE_DEFAULT", "60")
			},
			status: 1,
			stderr: "pnpm: default minimum-release-age mismatch: 60\n",
		},
		{
			name: "the distribution audit",
			change: func(t *testing.T, fixture *pnpmFixture) {
				fixture.write(t, filepath.Join(fixture.prefix, "dist/pnpm.mjs"), []byte("raw shadow"), 0o644)
			},
			status: 1,
			stderr: "pnpm verification: pristine distribution changed\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := stagePnpmFixture(t)
			env := fixture.environment(t)
			if testCase.change != nil {
				testCase.change(t, fixture)
			}
			result := runInProcess(t, env, "pnpm-verify", "--certify")
			if result.status != testCase.status {
				t.Fatalf("status is %d, want %d (stderr %q)", result.status, testCase.status, result.stderr)
			}
			if testCase.stderr != "" && string(result.stderr) != testCase.stderr {
				t.Fatalf("stderr is %q, want %q", result.stderr, testCase.stderr)
			}
			if testCase.stdout != "" && !strings.HasPrefix(string(result.stdout), testCase.stdout) {
				t.Fatalf("stdout is %q, want it to start with %q", result.stdout, testCase.stdout)
			}
		})
	}
}

func TestPnpmBootstrapStatuses(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		status int
		stderr string
		child  bool
	}{
		{
			name:   "a positional argument is a refusal",
			args:   []string{"pnpm-bootstrap", "extra"},
			status: 1,
			stderr: "pnpm bootstrap: no production positional arguments supported\n",
		},
		{
			// go-arg terminates the process for a malformed invocation, so the
			// usage status is only observable through a child.
			name:   "an unknown flag is usage",
			args:   []string{"pnpm-bootstrap", "--unknown"},
			status: 2,
			child:  true,
		},
		{
			name:   "a missing pin is a refusal",
			args:   []string{"pnpm-bootstrap", "--fixture-archive", "/nonexistent/pnpm.tgz"},
			status: 1,
			stderr: "pnpm bootstrap: ",
		},
		{
			name:   "the validate mode needs an owned fixture root",
			args:   []string{"pnpm-bootstrap", "--validate-fixture", "/"},
			status: 1,
			stderr: "pnpm bootstrap: ",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := stagePnpmFixture(t)
			env := fixture.environment(t)
			if testCase.name == "the validate mode needs an owned fixture root" {
				t.Setenv("AGENT_VM_FIXTURE_ROOT", "")
			}
			result := runChildProcess(t, childSpec{args: testCase.args, env: os.Environ()})
			if !testCase.child {
				result = runInProcess(t, env, testCase.args...)
			}
			if result.status != testCase.status {
				t.Fatalf("status is %d, want %d (stderr %q)", result.status, testCase.status, result.stderr)
			}
			if testCase.stderr != "" && !strings.HasPrefix(string(result.stderr), testCase.stderr) {
				t.Fatalf("stderr is %q, want it to start with %q", result.stderr, testCase.stderr)
			}
		})
	}
}

func TestPnpmValidateFixtureStatuses(t *testing.T) {
	fixture := stagePnpmFixture(t)
	env := fixture.environment(t)
	if result := runInProcess(t, env, "pnpm-bootstrap", "--validate-fixture", fixture.destination); result.status != 0 {
		t.Fatalf("a fixture destination was refused: %q (status %d)", result.stderr, result.status)
	}
	outside := filepath.Join(fixture.root, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("cannot create the outside directory: %v", err)
	}
	escape := filepath.Join(fixture.root, "escape")
	if err := os.Mkdir(escape, 0o755); err != nil {
		t.Fatalf("cannot create the escape destination: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(escape, "opt")); err != nil {
		t.Fatalf("cannot redirect the escape prefix: %v", err)
	}
	result := runInProcess(t, env, "pnpm-bootstrap", "--validate-fixture", escape)
	if result.status != 1 {
		t.Fatalf("an escaping destination was accepted (status %d)", result.status)
	}
}

func TestPnpmPinVersionStatuses(t *testing.T) {
	fixture := stagePnpmFixture(t)
	env := fixture.environment(t)
	pin := filepath.Join(fixture.source, "pin.json")
	t.Run("the reviewed version", func(t *testing.T) {
		result := runInProcess(t, env, "pnpm-pin-version", pin)
		if result.status != 0 || string(result.stdout) != "11.11.0\n" {
			t.Fatalf("status %d, stdout %q", result.status, result.stdout)
		}
	})
	t.Run("a missing pin is usage", func(t *testing.T) {
		// A missing required argument terminates the process, so the status is only
		// observable through a child.
		result := runChildProcess(t, childSpec{args: []string{"pnpm-pin-version"}, env: os.Environ()})
		if result.status != 2 {
			t.Fatalf("status is %d, want 2", result.status)
		}
	})
	t.Run("a pin without a version is a refusal", func(t *testing.T) {
		invalid := filepath.Join(t.TempDir(), "pin.json")
		if err := os.WriteFile(invalid, []byte(`{"url":"https://example.com/pnpm.tgz"}`), 0o644); err != nil {
			t.Fatalf("cannot write the invalid pin: %v", err)
		}
		result := runInProcess(t, env, "pnpm-pin-version", invalid)
		if result.status != 1 || string(result.stderr) != "pnpm-pin-version: invalid pin fields\n" {
			t.Fatalf("status %d, stderr %q", result.status, result.stderr)
		}
	})
}
