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

// TestOpencodeUsageIsStatusTwo pins the ported status contract: a missing flag or
// positional slot is usage (2), and the preparation driver's documented interface
// line is the shell's, unchanged.
func TestOpencodeUsageIsStatusTwo(t *testing.T) {
	// The preparation driver parses its own flags, so its refusals are observable
	// here. The two gates use the flag parser, whose Fail calls os.Exit(2) directly:
	// an in-process test cannot see that, and the shell harnesses run the real
	// binary for it.
	status, stdout, stderr := runOpencode(t, "opencode-prepare-lock")
	if status != 2 || stdout != "" || stderr != opencodePrepareFlagsUsage+"\n" {
		t.Fatalf("missing flags = %d, %q, %q, want 2 and the driver's own usage line", status, stdout, stderr)
	}
	// The preparation driver owns its positional grammar, so the documented
	// interface line is the retired script's and reaches the caller's stderr.
	status, stdout, stderr = runOpencode(t,
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
// predecessor printed nothing for.
func TestOpencodeUnknownOptionIsSilentUsage(t *testing.T) {
	status, stdout, stderr := runOpencode(t,
		"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--pin", "/p", "/prefix", "1.4.1", "--repin")
	if status != 2 {
		t.Fatalf("status = %d, want 2", status)
	}
	if stdout != "" || stderr != "" {
		t.Fatalf("a silent usage refusal wrote %q / %q", stdout, stderr)
	}
}

// TestOpencodePrepareLockPassesTheDriversOwnOptions is the regression test for a
// flag parser swallowing `--refresh-lock`: the driver's grammar owns the words
// after its flags, so the option reaches it and the refusal is the driver's (1),
// never a usage refusal (2).
func TestOpencodePrepareLockPassesTheDriversOwnOptions(t *testing.T) {
	for _, extra := range [][]string{{"--refresh-lock"}, {""}, {"--repin"}} {
		status, _, stderr := runOpencode(t, append([]string{
			"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--pin", "/p",
			"/nonexistent-prefix", "1.4.1"}, extra...)...)
		want := 1
		if extra[0] == "--repin" {
			want = 2
		}
		if status != want {
			t.Fatalf("%q: status = %d, want %d (%s)", extra[0], status, want, stderr)
		}
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
// `${:?}` refused an unset slot at status 1, and a direct caller gets the same
// status rather than a usage refusal.
func TestOpencodeVerifyRefusesAnEmptySlot(t *testing.T) {
	status, stdout, stderr := runOpencode(t,
		"opencode-verify", "--contract", "/c", "--prefix", "/p", "--status-dir", "/s")
	if status != 1 {
		t.Fatalf("status = %d, want 1 (%s)", status, stderr)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr != "  opencode: AGENT_VM_VERSION_OPENCODE is required\n" {
		t.Fatalf("stderr = %q", stderr)
	}
}
