package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// TestDshUsageIsStatusTwo pins the ported status contract: usage is 2, on stderr, and
// the flag parser's own refusal is one of those usages. `dsh-prepare-lock` with no
// flag is go-arg's Fail, which the parser hands to this process's stderr and a
// recorded exit rather than os.Exit, so it is observable here like the rest.
func TestDshUsageIsStatusTwo(t *testing.T) {
	for _, args := range [][]string{
		{"dsh-probe"},
		{"dsh-probe", "--prefix", "/opt/agent-vm/dsh"},
		{"dsh-probe", "--source", "/s", "--prefix", "/p", "--command", "/c", "--scratch-root", "/r", "--readiness-seconds", "soon"},
		{"dsh-probe", "--self-test", "--prefix", "/opt/agent-vm/dsh"},
		{"dsh-prepare-lock"},
		// The words after `--` are the driver's own grammar, so a short slot is its
		// bare usage line rather than this flag parser's.
		{"dsh-prepare-lock", "--source", "/s", "--contract", "/c", "--", "/prefix", "0.1.5-rc.2"},
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

// TestDshPrepareMissingFlagIsTheParsersOwnRefusal is the witness for `required`: a
// flag the caller never named is refused by go-arg, with its `PLACEHOLDER is
// required` line, before the empty check and the driver. Deleting `required` from
// either path leaves an empty value behind, so this asserts the parser's text and not
// only the status.
func TestDshPrepareMissingFlagIsTheParsersOwnRefusal(t *testing.T) {
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"source", []string{"dsh-prepare-lock", "--contract", "/c", "--", "/prefix", "", ""}},
		{"contract", []string{"dsh-prepare-lock", "--source", "/s", "--", "/prefix", "", ""}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := runDsh(t, testCase.args)
			if status != 2 {
				t.Fatalf("status = %d, want 2 (%q)", status, stderr)
			}
			if stdout != "" {
				t.Fatalf("the refusal wrote %q to stdout", stdout)
			}
			if !strings.Contains(stderr, "error: DIR is required") {
				t.Fatalf("stderr = %q, want the flag parser's missing-flag line", stderr)
			}
		})
	}
}

// TestDshPrepareRefusesAnEmptyPath is the seam `required` does not close: it records
// that the flag appeared, not that it carries a value. An empty `--source` makes the
// driver run `project.py` out of the caller's working directory, so the refusal has to
// happen here, at status 2, with the adapter's own line -- and nothing may run.
func TestDshPrepareRefusesAnEmptyPath(t *testing.T) {
	working := t.TempDir()
	marker := filepath.Join(working, "ran")
	// The unreviewed helper: if an empty source reaches the driver, this runs.
	script := "import pathlib, sys\npathlib.Path(" + strconv.Quote(marker) + ").write_text('ran')\nsys.exit(7)\n"
	if err := os.WriteFile(filepath.Join(working, "project.py"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(working)
	for _, testCase := range []struct {
		name string
		args []string
	}{
		{"source", []string{"dsh-prepare-lock", "--source", "", "--contract", "/c", "--", "/prefix", "", ""}},
		{"contract", []string{"dsh-prepare-lock", "--source", "/s", "--contract", "", "--", "/prefix", "", ""}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, stdout, stderr := runDsh(t, testCase.args)
			if status != 2 {
				t.Fatalf("status = %d, want 2 (%q)", status, stderr)
			}
			if stdout != "" || stderr != dshPrepareUsageText {
				t.Fatalf("wrote %q / %q, want the adapter's usage line on stderr", stdout, stderr)
			}
		})
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the working directory's project.py ran (stat err %v)", err)
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
	// Only the parse is this layer's: a value that is not a number at all. The ceiling
	// is the package's rule (TestProbeBounds), so it holds for every caller of Probe.
	for _, rejected := range []string{"abc", "NaN", "Inf", "-Inf", "Infinity"} {
		var usage *dsh.Usage
		err := func() error { _, err := probeBound(rejected); return err }()
		if err == nil || !errors.As(err, &usage) {
			t.Errorf("probeBound(%q) = %v, want a usage refusal", rejected, err)
		}
	}
	for _, outOfRange := range []string{"0", "-1", "60.5", "80"} {
		if _, err := probeBound(outOfRange); err != nil {
			t.Errorf("probeBound(%q) = %v; the range is the package's decision", outOfRange, err)
		}
	}
	if _, err := probeBound("60"); err != nil {
		t.Errorf("probeBound(60) = %v, want the production deadline to be allowed", err)
	}
}
