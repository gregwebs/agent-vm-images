package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/toolaccess"
)

// fakeEnvelopeReport is the composed run-report.sh for a gate case: the same
// argument shape, the same capture files and the same status the child produced.
const fakeEnvelopeReport = `#!/bin/sh
printf 'run-report|%s\n' "$*" >>"$CALLS"
seconds=$1
out=$2
err=$3
shift 3
: >"$out"
: >"$err"
scratch=$(mktemp -d)
HOME=$scratch
export HOME
status=0
"$@" >"$out" 2>"$err" || status=$?
rm -rf "$scratch"
exit "$status"
`

// accessEnvironment is the process view the CLI hands every audit.
func accessEnvironment() toolaccess.Environment {
	return toolaccess.Environment{
		WorkingDirectory: func() (toolaccess.PhysicalDirectory, error) {
			directory, err := syscall.Getwd()
			return toolaccess.PhysicalDirectoryOf(directory), err
		},
		Path: toolaccess.PathValue(os.Getenv("PATH")),
	}
}

// verifyFixture is one installed-command gate case.
type verifyFixture struct {
	*fixture
	contract  string
	statusDir string
	command   string
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	tree := newFixture(t)
	contract := filepath.Join(tree.root, "contract")
	if err := os.MkdirAll(contract, 0o755); err != nil {
		t.Fatalf("mkdir contract: %v", err)
	}
	if err := os.WriteFile(filepath.Join(contract, "run-report.sh"), []byte(fakeEnvelopeReport), 0o755); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
	command := filepath.Join(tree.prefix, ".opencode", "bin", "opencode")
	if err := os.MkdirAll(filepath.Dir(command), 0o755); err != nil {
		t.Fatalf("mkdir command: %v", err)
	}
	statusDir := filepath.Join(tree.root, "status")
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		t.Fatalf("mkdir status: %v", err)
	}
	// The image links the bare name onto PATH; the audit resolves it there.
	t.Setenv("PATH", filepath.Dir(command)+string(os.PathListSeparator)+tree.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &verifyFixture{fixture: tree, contract: contract, statusDir: statusDir, command: command}
}

func (v *verifyFixture) options(slot string) VerifyOptions {
	return VerifyOptions{
		Contract:  v.contract,
		Prefix:    v.prefix,
		Slot:      slot,
		StatusDir: v.statusDir,
		Access:    accessEnvironment(),
	}
}

func (v *verifyFixture) run(t *testing.T, slot string) (string, string, error) {
	t.Helper()
	options := v.options(slot)
	out, errOut := &strings.Builder{}, &strings.Builder{}
	options.Out, options.Err = out, errOut
	err := Verify(context.Background(), options)
	return out.String(), errOut.String(), err
}

// installCommand writes the fixture command's report.
func (v *verifyFixture) installCommand(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(v.command, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write command: %v", err)
	}
}

func (v *verifyFixture) record(t *testing.T) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(v.statusDir, recordName))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	return string(data), true
}

// TestVerifyAbsenceRecordIsAccepted is the softened-transport state: the record is
// the only thing that may waive a missing command, and the gate then records
// nothing of its own.
func TestVerifyAbsenceRecordIsAccepted(t *testing.T) {
	tree := newVerifyFixture(t)
	if err := os.WriteFile(filepath.Join(tree.statusDir, recordName), []byte("absent-transport 28\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := tree.run(t, "v2.0.29")
	if err != nil {
		t.Fatalf("gate = %v (%s)", err, stderr)
	}
	if want := "  opencode: ABSENT (absent-transport 28); raw developer build only\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	tree.expectNoChild(t)
	if got, found := tree.record(t); !found || got != "absent-transport 28\n" {
		t.Fatalf("record = %q, %v; an absence report must not rewrite it", got, found)
	}
}

// TestVerifyAbsenceNeedsOneExactRecord pins the record grammar: a missing, stale,
// unknown-code or multi-line record authorises nothing.
func TestVerifyAbsenceNeedsOneExactRecord(t *testing.T) {
	for _, record := range []string{"", "pending\n", "installed\n", "absent-transport 9\n", "absent-transport 28\njunk\n", "absent-pi\n"} {
		tree := newVerifyFixture(t)
		if record != "" {
			if err := os.WriteFile(filepath.Join(tree.statusDir, recordName), []byte(record), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, _, err := tree.run(t, "v2.0.29")
		mustRefusal(t, err, "  opencode: command missing with no valid absence record -- hard failure")
		tree.expectNoChild(t)
	}
}

// TestVerifyDanglingLinkIsPresent keeps lstat semantics: a dangling link is a
// contract violation even with a valid absence record.
func TestVerifyDanglingLinkIsPresent(t *testing.T) {
	tree := newVerifyFixture(t)
	if err := os.Symlink(filepath.Join(tree.root, "nowhere"), tree.command); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree.statusDir, recordName), []byte("absent-transport 28\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := tree.run(t, "v2.0.29")
	if err == nil {
		t.Fatal("a dangling command passed the gate")
	}
	if strings.Contains(stdout, "ABSENT") {
		t.Fatalf("a dangling link was reported absent: %q", stdout)
	}
}

// TestVerifyIndeterminatePathIsAHardFailure keeps a permission error on an
// ancestor from reading as absence.
func TestVerifyIndeterminatePathIsAHardFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a privileged uid can stat through an unreadable parent")
	}
	tree := newVerifyFixture(t)
	directory := filepath.Dir(tree.command)
	if err := os.Chmod(directory, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o755) })
	if err := os.WriteFile(filepath.Join(tree.statusDir, recordName), []byte("absent-transport 28\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := tree.run(t, "v2.0.29")
	mustRefusal(t, err, "  opencode: cannot stat "+tree.command+" (permission/loop) -- hard failure")
}

// TestVerifyReport pins the exact-report rule: the report is one exact line, and
// every deviation is refused with its own documented diagnostic.
func TestVerifyReport(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		report string
		want   string
	}{
		{"bare version", "2.0.29\n", "  opencode: report does not match v2.0.29 exactly"},
		{"old banner", "opencode v1.18.34\n", "  opencode: report does not match v2.0.29 exactly"},
		{"extra newline", "opencode v2.0.29\n\n", "  opencode: report does not match v2.0.29 exactly"},
		{"tab", "opencode v2.0.29\t\n", "  opencode: report does not match v2.0.29 exactly"},
		{"no newline", "opencode v2.0.29", "  opencode: report does not match v2.0.29 exactly"},
		{"nul", "opencode v2.0.\x0034\n", "  opencode: --version report contains non-printable bytes -- hard failure"},
		{"longer", "opencode v2.0.290\n", "  opencode: report does not match v2.0.29 exactly"},
		{"leading warning", "warning: expected 2.0.29\nopencode v2.0.29\n", "  opencode: report does not match v2.0.29 exactly"},
		{"unterminated trailing report", "opencode v2.0.29\nUNRECOGNIZED TRAILING REPORT", "  opencode: report does not match v2.0.29 exactly"},
	} {
		tree := newVerifyFixture(t)
		// The bytes travel in a file: a NUL cannot live in generated shell syntax.
		t.Setenv("REPORT_FILE", tree.write(t, "report", testCase.report, 0o644))
		tree.installCommand(t, "cat \"$REPORT_FILE\"\n")
		_, _, err := tree.run(t, "v2.0.29")
		mustRefusal(t, err, testCase.want)
		if _, found := tree.record(t); found {
			t.Fatalf("%s: a refused report wrote the record", testCase.name)
		}
	}
}

// TestVerifyExactReport covers the accepted form and proves the record is the last
// thing written.
func TestVerifyExactReport(t *testing.T) {
	tree := newVerifyFixture(t)
	tree.installCommand(t, "printf 'opencode v2.0.29\\n'\n")
	stdout, stderr, err := tree.run(t, "v2.0.29")
	if err != nil {
		t.Fatalf("gate = %v (%s)", err, stderr)
	}
	if !strings.HasPrefix(stdout, "  opencode: v2.0.29 (pinned)\n") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "tool-access: ok ") {
		t.Fatalf("stdout = %q, want the access audit's own line", stdout)
	}
	if got, found := tree.record(t); !found || got != "installed\n" {
		t.Fatalf("record = %q, %v, want installed", got, found)
	}
	// The report envelope owns the bound: 60 seconds is the reviewed one, and the
	// gate must not choose its own.
	if invocations := tree.invocations(t); len(invocations) != 1 || !strings.HasPrefix(invocations[0], "run-report|60 ") {
		t.Fatalf("invocations = %q, want one envelope bounded at 60 seconds", invocations)
	}
}

// TestVerifyStderrContradiction is the B1 rule: a version report on stderr
// contradicts the exact stdout report, in either banner form.
func TestVerifyStderrContradiction(t *testing.T) {
	for _, diagnostics := range []string{"9.9.9\n", "opencode v2.0.28\n", "v2.0.29\n"} {
		tree := newVerifyFixture(t)
		t.Setenv("DIAGNOSTICS_FILE", tree.write(t, "diagnostics", diagnostics, 0o644))
		tree.installCommand(t, "printf 'opencode v2.0.29\\n'\ncat \"$DIAGNOSTICS_FILE\" >&2\n")
		_, _, err := tree.run(t, "v2.0.29")
		mustRefusal(t, err, "  opencode: version report on stderr -- hard failure")
	}
	// A diagnostic that is not a report is ordinary stderr and must not be refused.
	tree := newVerifyFixture(t)
	tree.installCommand(t, "printf 'opencode v2.0.29\\n'\nprintf 'warning: 9 plugins\\n' >&2\n")
	if _, stderr, err := tree.run(t, "v2.0.29"); err != nil {
		t.Fatalf("gate = %v (%s)", err, stderr)
	}
}

// TestVerifyNonzeroReport keeps a nonzero status from being certified by the right
// bytes, and reprints the first ten lines of the child's stderr.
func TestVerifyNonzeroReport(t *testing.T) {
	tree := newVerifyFixture(t)
	lines := strings.Builder{}
	for index := 0; index < 12; index++ {
		lines.WriteString("line " + string(rune('a'+index)) + "\n")
	}
	tree.installCommand(t, "printf 'opencode v2.0.29\\n'\nprintf '"+lines.String()+"' >&2\nexit 1\n")
	_, stderr, err := tree.run(t, "v2.0.29")
	mustSilent(t, err, 1)
	if !strings.Contains(stderr, "  opencode: --version exited 1; refusing to accept the report\n") {
		t.Fatalf("stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "line a\n") || !strings.Contains(stderr, "line j\n") {
		t.Fatalf("stderr = %q, want the first ten lines", stderr)
	}
	if strings.Contains(stderr, "line k\n") {
		t.Fatalf("stderr = %q, want at most ten lines", stderr)
	}
	if _, found := tree.record(t); found {
		t.Fatal("a failed report wrote the record")
	}
}

// TestVerifyAccessAudit keeps the access audit hard: a command the image links but
// an arbitrary uid cannot run is a broken image.
func TestVerifyAccessAudit(t *testing.T) {
	tree := newVerifyFixture(t)
	tree.installCommand(t, "printf 'opencode v2.0.29\\n'\n")
	// The same version at a path the audit cannot reach: the bare name still
	// resolves, so the audit of the resolved ancestors has to fail.
	if err := os.Chmod(filepath.Dir(tree.command), 0o700); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := tree.run(t, "v2.0.29")
	mustSilent(t, err, 1)
	if !strings.HasPrefix(stderr, "tool-access: ") {
		t.Fatalf("stderr = %q, want the audit's own diagnostic", stderr)
	}
	if _, found := tree.record(t); found {
		t.Fatal("a refused audit wrote the record")
	}
}

// TestVerifySlotIsRequired is the empty-slot refusal: a direct caller gets the
// same status the entry point's `${:?}` gave.
func TestVerifySlotIsRequired(t *testing.T) {
	tree := newVerifyFixture(t)
	_, _, err := tree.run(t, "")
	mustRefusal(t, err, "  opencode: AGENT_VM_VERSION_OPENCODE is required")
}
