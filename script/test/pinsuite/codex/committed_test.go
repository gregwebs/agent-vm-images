package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// committedDirectory is the real committed closure, read by the real programs.
func committedDirectory(t *testing.T) string {
	t.Helper()
	return filepath.Join(harness.Repository(t), PinDirectory)
}

// committedPin is the real committed pin document, parsed with its numbers intact.
//
// A missing or unreadable committed pin is a failure, not a reason to skip: the
// closure is adopted (images/tools/codex/pin.json is committed), so every way it can
// be absent is a defect in this checkout -- and skipping three cases on that defect is
// how the offline check, both selection rows and the writer's own check stop running
// while the suite still reports green.
func committedPin(t *testing.T) map[string]any {
	t.Helper()
	directory := committedDirectory(t)
	if _, err := os.Stat(filepath.Join(directory, "pin.json")); err != nil {
		t.Fatalf("the committed Codex pin is not readable: %v", err)
	}
	return document(t, harness.ReadFile(t, filepath.Join(directory, "pin.json")))
}

// TestCommittedReaderVerifiesTheClosureOffline: `check` over the real committed
// closure re-hashes the committed evidence and says, in one frozen transcript, what
// it did and did not verify.
func TestCommittedReaderVerifiesTheClosureOffline(t *testing.T) {
	pin := committedPin(t)
	release, ok := pin["release"].(string)
	if !ok {
		t.Fatalf("the committed pin names no release: %v", pin["release"])
	}
	printed := accepts(t, pinInvocation("check", committedDirectory(t), "--release", release)).Stdout
	lines := nonEmptyLines(printed)
	if len(lines) != len(platforms)+2 {
		t.Fatalf("check printed %d lines: %q", len(lines), lines)
	}
	want := fmt.Sprintf("codex %s: offline committed-input check "+
		"(committed bytes and policy only; not a signature check)", release)
	if lines[0] != want {
		t.Fatalf("check did not report the offline verification:\n got: %s\nwant: %s", lines[0], want)
	}
	for _, line := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(line, "  linux/") {
			t.Fatalf("unexpected platform line: %s", line)
		}
	}
	if last, want := lines[len(lines)-1], "  evidence: 2 files plus 2 structurally consistent bundles"; last != want {
		t.Fatalf("check did not report the evidence set:\n got: %s\nwant: %s", last, want)
	}
}

// TestCommittedSelectPrintsFourFieldsPerTarget: `select` is parsed positionally by the
// vendored installer, so it prints exactly four newline-delimited fields per target.
func TestCommittedSelectPrintsFourFieldsPerTarget(t *testing.T) {
	pin := committedPin(t)
	release, ok := pin["release"].(string)
	if !ok {
		t.Fatalf("the committed pin names no release: %v", pin["release"])
	}
	for _, name := range platforms {
		pinned := object(t, pin, "platforms", string(name))
		printed := accepts(t, pinInvocation("select", committedDirectory(t), "--release", release,
			"--target", string(targetOf(name)))).Stdout
		want := fmt.Sprintf("%v\n%v\n%v\n%v\n", pin["version"], pinned["asset"], pinned["url"],
			pinned["sha256"])
		if printed != want {
			t.Fatalf("select %s printed %q, want %q", targetOf(name), printed, want)
		}
	}
}

// TestCommittedWriterCheckAcceptsTheTree: the maintainer wrapper re-verifies the real
// committed inputs offline. This is the one case that drives
// `script/dev/pin-codex.sh` itself -- the compile, the `cd` to the checkout and the
// `exec avm-pin codex` CONTRIBUTING tells a maintainer to run.
func TestCommittedWriterCheckAcceptsTheTree(t *testing.T) {
	pin := committedPin(t)
	release, ok := pin["release"].(string)
	if !ok {
		t.Fatalf("the committed pin names no release: %v", pin["release"])
	}
	repo := &repository{work: scratch(t, "committed-writer"), root: harness.Repository(t)}
	result := runWriterWrapper(t, repo, arguments("--check"))
	requireStatus(t, result, 0)
	result.RequireStdout(t, "offline committed-input check")
	result.RequireStdout(t, dockerfileArg+release)
}
