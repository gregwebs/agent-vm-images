package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// committedDirectory is the real committed closure, read by the real reader.
func committedDirectory(t *testing.T) string {
	t.Helper()
	return filepath.Join(harness.Repository(t), pinDirectory)
}

// TestCommittedVersion: `version` prints what the committed pin names.
func TestCommittedVersion(t *testing.T) {
	directory := committedDirectory(t)
	if _, err := os.Stat(filepath.Join(directory, "pin.json")); err != nil {
		t.Skipf("no committed Claude pin adopted yet: %v", err)
	}
	pin := readPin(t, directory)
	printed := strings.TrimSpace(accepts(t, pinInvocation("version", directory)).Stdout)
	if printed != pin["version"] {
		t.Fatalf("version printed %q, the pin names %v", printed, pin["version"])
	}
}

// TestCommittedDigestBothTokens: `digest` prints each platform's pinned SHA-256.
func TestCommittedDigestBothTokens(t *testing.T) {
	directory := committedDirectory(t)
	if _, err := os.Stat(filepath.Join(directory, "pin.json")); err != nil {
		t.Skipf("no committed Claude pin adopted yet: %v", err)
	}
	pin := readPin(t, directory)
	for _, name := range platforms {
		printed := strings.TrimSpace(accepts(t,
			pinInvocation("digest", directory, "--platform", string(name.token))).Stdout)
		expected := object(t, pin, "platforms", name.key)["sha256"]
		if printed != expected {
			t.Fatalf("digest %s printed %q, the pin records %v", name.token, printed, expected)
		}
	}
}

// TestCommittedCheckVerifiesTheSignatureOffline: `check` verifies the committed
// signature and says so, on one line.
func TestCommittedCheckVerifiesTheSignatureOffline(t *testing.T) {
	directory := committedDirectory(t)
	if _, err := os.Stat(filepath.Join(directory, "pin.json")); err != nil {
		t.Skipf("no committed Claude pin adopted yet: %v", err)
	}
	pin := readPin(t, directory)
	version, ok := pin["version"].(string)
	if !ok {
		t.Fatalf("the committed pin names no version: %v", pin["version"])
	}
	printed := checkAccepted(t, directory, version).Stdout
	lines := []string{}
	for _, line := range strings.Split(printed, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("check printed %d lines: %q", len(lines), lines)
	}
	if !strings.Contains(lines[0], "committed closure verified offline") {
		t.Fatalf("check did not report the offline verification: %s", lines[0])
	}
	if !strings.Contains(lines[0], version) {
		t.Fatalf("check did not name the pinned version: %s", lines[0])
	}
}
