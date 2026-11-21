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
// A *missing required flag* is the flag parser's own refusal. For
// `opencode-prepare-lock` the parser is this process's own (`argParser`), so the path
// is observable and `TestOpencodePrepareMissingFlagIsTheParsersOwnRefusal` drives it.
// `opencode-verify` and `opencode-verify-plugins` still build go-arg's default parser,
// whose Fail calls os.Exit(2) directly: that path is documented and uncovered, and no
// test here drives a missing flag through either of those two verbs.
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

// TestOpencodePrepareMissingFlagIsTheParsersOwnRefusal is the witness for `required`
// on the three executed paths: a flag the caller never named is refused by go-arg
// before the empty check and the driver. Deleting `required` leaves an empty value
// behind, so this asserts the parser's text and not only the status.
func TestOpencodePrepareMissingFlagIsTheParsersOwnRefusal(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"contract", []string{"opencode-prepare-lock", "--check", "/k", "--pin", "/p", "--", "/prefix", "1.4.1"}},
		{"check", []string{"opencode-prepare-lock", "--contract", "/c", "--pin", "/p", "--", "/prefix", "1.4.1"}},
		{"pin", []string{"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--", "/prefix", "1.4.1"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := runOpencode(t, testCase.args...)
			if status != 2 {
				t.Fatalf("status = %d, want 2 (%q)", status, stderr)
			}
			if stdout != "" {
				t.Fatalf("the refusal wrote %q to stdout", stdout)
			}
			if !strings.Contains(stderr, "error: ") || !strings.Contains(stderr, " is required") {
				t.Fatalf("stderr = %q, want the flag parser's missing-flag line", stderr)
			}
		})
	}
}

// TestOpencodePrepareRefusesAnEmptyPath is the same hole #221 closed in dsh, shipped
// in #220: `required` records that the flag appeared, not that it carries a value. An
// empty `--contract` composes `run-pnpm.sh` relative to the caller's working
// directory, so the refusal has to happen before the driver and it names the flag.
func TestOpencodePrepareRefusesAnEmptyPath(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"--contract", []string{"opencode-prepare-lock", "--contract", "", "--check", "/k", "--pin", "/p", "--", "/prefix", "1.4.1"}},
		{"--check", []string{"opencode-prepare-lock", "--contract", "/c", "--check", "", "--pin", "/p", "--", "/prefix", "1.4.1"}},
		{"--pin", []string{"opencode-prepare-lock", "--contract", "/c", "--check", "/k", "--pin", "", "--", "/prefix", "1.4.1"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := runOpencode(t, testCase.args...)
			if status != 2 {
				t.Fatalf("status = %d, want 2 (%q)", status, stderr)
			}
			if stdout != "" {
				t.Fatalf("the refusal wrote %q to stdout", stdout)
			}
			if want := opencode.EmptyPathUsage(testCase.name) + "\n"; stderr != want {
				t.Fatalf("stderr = %q, want %q", stderr, want)
			}
		})
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

// TestOpencodeInstallRefusesAnEmptyPath is the witness for `required` plus the empty
// check on the two install verbs: every path they create or execute is named, and an
// empty one would address the caller's working directory instead of the container's.
func TestOpencodeInstallRefusesAnEmptyPath(t *testing.T) {
	for _, testCase := range []struct {
		name string
		flag string
		args []string
	}{
		{"--vendor", "--vendor", []string{"opencode-install", "--vendor", "", "--prefix", "/p", "--link", "/l", "--opencode-version", "v2.0.29", "--status-dir", "/s"}},
		{"--prefix", "--prefix", []string{"opencode-install", "--vendor", "/v", "--prefix", "", "--link", "/l", "--opencode-version", "v2.0.29", "--status-dir", "/s"}},
		{"--link", "--link", []string{"opencode-install", "--vendor", "/v", "--prefix", "/p", "--link", "", "--opencode-version", "v2.0.29", "--status-dir", "/s"}},
		{"--status-dir", "--status-dir", []string{"opencode-install", "--vendor", "/v", "--prefix", "/p", "--link", "/l", "--opencode-version", "v2.0.29", "--status-dir", ""}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := runOpencode(t, testCase.args...)
			if status != 2 {
				t.Fatalf("status = %d, want 2 (%q)", status, stderr)
			}
			if stdout != "" {
				t.Fatalf("the refusal wrote %q to stdout", stdout)
			}
			if want := opencode.EmptyFlagUsage("opencode-install", testCase.flag) + "\n"; stderr != want {
				t.Fatalf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

func TestOpencodeInstallPluginsRefusesAnEmptyPath(t *testing.T) {
	for _, testCase := range []struct {
		name string
		flag string
		args []string
	}{
		{"--contract", "--contract", []string{"opencode-install-plugins", "--contract", "", "--source", "/s", "--prefix", "/p"}},
		{"--source", "--source", []string{"opencode-install-plugins", "--contract", "/c", "--source", "", "--prefix", "/p"}},
		{"--prefix", "--prefix", []string{"opencode-install-plugins", "--contract", "/c", "--source", "/s", "--prefix", ""}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := runOpencode(t, testCase.args...)
			if status != 2 {
				t.Fatalf("status = %d, want 2 (%q)", status, stderr)
			}
			if stdout != "" {
				t.Fatalf("the refusal wrote %q to stdout", stdout)
			}
			if want := opencode.EmptyFlagUsage("opencode-install-plugins", testCase.flag) + "\n"; stderr != want {
				t.Fatalf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

// TestOpencodeInstallNeedsTheSlot: the entry point's `${:?}` is the shell's status 2,
// and a direct caller of the verb gets the same status and the same text.
func TestOpencodeInstallNeedsTheSlot(t *testing.T) {
	status, stdout, stderr := runOpencode(t,
		"opencode-install", "--vendor", "/v", "--prefix", "/p", "--link", "/l",
		"--opencode-version", "", "--status-dir", "/s")
	if status != 2 {
		t.Fatalf("status = %d, want 2 (%q)", status, stderr)
	}
	if stdout != "" {
		t.Fatalf("the refusal wrote %q to stdout", stdout)
	}
	if want := "install-opencode.sh needs AGENT_VM_VERSION_OPENCODE\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}
