package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/dsh"
)

// The dsh hooks' status contract is what their callers trap on, so it is pinned here
// separately from any payload: usage is 2, a refusal is 1 on stderr, a child keeps its
// own status and its own bytes, and a signal keeps 130/143/129.

func runDsh(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	status := Run(args, ProcessEnvironment(), Streams{Out: &stdout, Err: &stderr})
	return status, stdout.String(), stderr.String()
}

func TestDshUsageIsStatusTwo(t *testing.T) {
	for _, args := range [][]string{
		{"dsh-probe"},
		{"dsh-probe", "--prefix", "/opt/agent-vm/dsh"},
		{"dsh-probe", "--source", "/s", "--prefix", "/p", "--command", "/c", "--scratch-root", "/r", "--readiness-seconds", "0"},
		{"dsh-probe", "--source", "/s", "--prefix", "/p", "--command", "/c", "--scratch-root", "/r", "--readiness-seconds", "61"},
		{"dsh-probe", "--source", "/s", "--prefix", "/p", "--command", "/c", "--scratch-root", "/r", "--readiness-seconds", "soon"},
		{"dsh-probe", "--self-test", "--prefix", "/opt/agent-vm/dsh"},
		{"dsh-prepare-lock"},
	} {
		status, stdout, stderr := runDsh(t, args)
		if status != 2 {
			t.Errorf("%v: status = %d, want 2 (%s)", args, status, stderr)
		}
		if stdout != "" {
			t.Errorf("%v: a usage refusal wrote %q to stdout", args, stdout)
		}
		if stderr == "" {
			t.Errorf("%v: a usage refusal wrote nothing to stderr", args)
		}
	}
}

// TestDshFailureStatuses pins the mapping itself, including the case that used to be
// invisible: a refusal never reaches stdout.
func TestDshFailureStatuses(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		err    error
		status int
		stream string
		text   string
	}{
		{"usage", &dsh.Usage{Text: "usage: x"}, 2, "err", "dsh-probe: usage: x\n"},
		{"refusal", &dsh.Refusal{Text: "private exec TMPDIR survived cleanup"}, 1, "err",
			"dsh-runtime: web: private exec TMPDIR survived cleanup\n"},
		{"child", &dsh.ChildFailure{Status: 7, Stderr: "pnpm-project: symlink waypoint: /tmp\n"}, 7, "err",
			"pnpm-project: symlink waypoint: /tmp\n"},
		{"silent", &dsh.Silent{Status: 3}, 3, "err", ""},
		{"interrupt", &dsh.Interrupted{Status: 143}, 143, "err", "dsh-runtime: web: interrupted\n"},
	} {
		var stdout, stderr strings.Builder
		process := &process{out: newStdout(&stdout), err: newStderr(&stderr),
			streams: Streams{Out: &stdout, Err: &stderr}}
		status := process.dshFailure(dshProbeDiagnostics, testCase.err)
		if status != testCase.status {
			t.Errorf("%s: status = %d, want %d", testCase.name, status, testCase.status)
		}
		if err := process.err.flush(); err != nil {
			t.Fatal(err)
		}
		got := stderr.String()
		if stdout.String() != "" {
			t.Errorf("%s: a refusal wrote %q to stdout", testCase.name, stdout.String())
		}
		if got != testCase.text {
			t.Errorf("%s: stderr = %q, want %q", testCase.name, got, testCase.text)
		}
	}
}

// TestProbeBound refusals keeps the production deadline a ceiling rather than a
// default: a caller cannot buy a slower gate.
func TestProbeBound(t *testing.T) {
	if bound, err := probeBound(""); err != nil || bound != nil {
		t.Fatalf("empty = %v, %v", bound, err)
	}
	bound, err := probeBound("0.5")
	if err != nil || bound == nil || *bound != 500*time.Millisecond {
		t.Fatalf("0.5 = %v, %v", bound, err)
	}
	for _, rejected := range []string{"0", "-1", "60.5", "80", "abc", "NaN", "Inf", "-Inf", "Infinity"} {
		var usage *dsh.Usage
		err := func() error { _, err := probeBound(rejected); return err }()
		if err == nil || !errors.As(err, &usage) {
			t.Errorf("probeBound(%q) = %v, want a usage refusal", rejected, err)
		}
	}
	if _, err := probeBound("60"); err != nil {
		t.Errorf("probeBound(60) = %v, want the production deadline to be allowed", err)
	}
}
