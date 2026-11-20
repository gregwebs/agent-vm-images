package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/opencode"
)

func runOpencode(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	status := Run(args, ProcessEnvironment(), Streams{Out: &stdout, Err: &stderr})
	return status, stdout.String(), stderr.String()
}

// TestOpencodeUsageIsStatusTwo pins the ported status contract: a missing
// positional slot is usage (2) and the preparation driver's documented interface
// line is the shell's, unchanged.
//
// A *missing required flag* is the flag parser's own refusal, and its Fail calls
// os.Exit(2) directly, so nothing here can observe it. That path is documented and
// uncovered: no test in this repository, in-process or in shell, drives a missing
// flag through any of these three verbs.
func TestOpencodeUsageIsStatusTwo(t *testing.T) {
	status, stdout, stderr := runOpencode(t,
		"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--pin", "/p")
	if status != 2 {
		t.Fatalf("status = %d, want 2 (%s)", status, stderr)
	}
	if stdout != "" {
		t.Fatalf("a usage refusal wrote %q to stdout", stdout)
	}
	if want := "usage: prepare-lock.sh PREFIX PLUGIN_VERSION [--refresh-lock]\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// TestOpencodeUnknownOptionIsSilentUsage keeps the one usage refusal the
// predecessor printed nothing for. The words after `--` are positional, so this is
// the driver's grammar refusing a third positional word.
func TestOpencodeUnknownOptionIsSilentUsage(t *testing.T) {
	status, stdout, stderr := runOpencode(t,
		"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--pin", "/p", "--", "/prefix", "1.4.1", "--repin")
	if status != 2 {
		t.Fatalf("status = %d, want 2", status)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("a silent usage refusal wrote %q / %q", stdout, stderr)
	}
}

// TestOpencodeCallerWordsCannotBecomeFlags is the seam `--` closes: a caller that
// writes `PREFIX VERSION --check other.mjs` must not replace the reviewed checker.
// It is a usage refusal, and the run never starts.
func TestOpencodeCallerWordsCannotBecomeFlags(t *testing.T) {
	status, stdout, stderr := runOpencode(t,
		"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--pin", "/p", "--",
		"/prefix", "1.4.1", "--check", "/evil.mjs")
	if status != 2 {
		t.Fatalf("status = %d, want the driver's usage refusal (%q %q)", status, stdout, stderr)
	}
	if want := "usage: prepare-lock.sh PREFIX PLUGIN_VERSION [--refresh-lock]\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// TestOpencodePrepareLockPassesTheDriversOwnOptions is the regression test for a
// flag parser swallowing `--refresh-lock`: the entry point ends its flags with
// `--`, so the option reaches the driver's grammar and the refusal is the driver's
// (1), never a usage refusal (2).
func TestOpencodePrepareLockPassesTheDriversOwnOptions(t *testing.T) {
	for _, extra := range [][]string{{"--refresh-lock"}, {""}} {
		status, _, stderr := runOpencode(t, append([]string{
			"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--pin", "/p", "--",
			"/nonexistent-prefix", "1.4.1"}, extra...)...)
		if status != 1 {
			t.Fatalf("%q: status = %d, want 1 (%s)", extra[0], status, stderr)
		}
	}
}

// TestOpencodeEmptyFixtureRootIsAValue is the boundary the image build cannot do
// without: the entry point passes `--fixture-root "$AGENT_VM_FIXTURE_ROOT"` whether
// or not that variable is set, so an empty value must reach the driver as "no
// fixture root" rather than being refused as a missing flag. A mutant that refuses
// it breaks every product build and no other local gate notices.
func TestOpencodeEmptyFixtureRootIsAValue(t *testing.T) {
	status, stdout, stderr := runOpencode(t, "opencode-prepare-lock",
		"--contract", "/c", "--check", "/k", "--pin", "/p", "--fixture-root", "", "--",
		"/nonexistent-prefix", "1.4.1")
	if status != 1 {
		t.Fatalf("status = %d, want the prefix refusal (1), not a usage refusal (%q %q)", status, stdout, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want the silent prefix refusal", stderr)
	}
}

// TestOpencodeFailureStatuses pins the mapping: usage is 2, a refusal is 1 on
// stderr, a child's status is kept, and a signal keeps 128+N.
func TestOpencodeFailureStatuses(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"usage", &opencode.Usage{Text: "usage: prepare-lock.sh P V"}, 2, "usage: prepare-lock.sh P V\n"},
		{"silent usage", &opencode.Usage{}, 2, ""},
		{"refusal", &opencode.Refusal{Text: "  opencode: --version report contains non-printable bytes -- hard failure"}, 1,
			"  opencode: --version report contains non-printable bytes -- hard failure\n"},
		{"child", &opencode.Silent{Status: 7}, 7, ""},
		{"interrupt", &opencode.Interrupted{Status: 143}, 143, ""},
		{"untyped", errors.New("boom"), 1, "boom\n"},
	} {
		var stdout, stderr strings.Builder
		process := &process{out: newStdout(&stdout), err: newStderr(&stderr),
			streams: Streams{Out: &stdout, Err: &stderr}}
		status := process.opencodeFailure(testCase.err)
		if status != testCase.status {
			t.Errorf("%s: status = %d, want %d", testCase.name, status, testCase.status)
		}
		if err := process.err.flush(); err != nil {
			t.Fatal(err)
		}
		if stdout.String() != "" {
			t.Errorf("%s: a failure wrote %q to stdout", testCase.name, stdout.String())
		}
		if got := stderr.String(); got != testCase.text {
			t.Errorf("%s: stderr = %q, want %q", testCase.name, got, testCase.text)
		}
	}
}

// TestOpencodeVerifyRefusesAnEmptySlot is the seam's status: the entry point's
// `${:?}` is a missing required argument, which the predecessor's dash reported at
// status 2, and a direct caller gets the same.
func TestOpencodeVerifyRefusesAnEmptySlot(t *testing.T) {
	status, stdout, stderr := runOpencode(t,
		"opencode-verify", "--prefix", "/p", "--status-dir", "/s")
	if status != 2 {
		t.Fatalf("status = %d, want 2 (%s)", status, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "verify-opencode.sh needs AGENT_VM_VERSION_OPENCODE\n" {
		t.Fatalf("stderr = %q", stderr)
	}
}
