package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// stateRefused is one `native-state` invocation that must refuse.
func stateRefused(r harness.Reporter, prefix string, needles ...string) string {
	r.Helper()
	return refuses(r, []string{"native-state", "--prefix", prefix}, needles)
}

func stateAccepted(r harness.Reporter, prefix string) harness.Result {
	r.Helper()
	return accepts(r, []string{"native-state", "--prefix", prefix})
}

// statePath is the native state file's path in one prefix.
func statePath(prefix string) string { return filepath.Join(prefix, ".claude.json") }

// TestNativeStateOfflineInstallStateAccepted: the state the offline native step leaves
// behind is exactly what the image needs.
func TestNativeStateOfflineInstallStateAccepted(t *testing.T) {
	prefix := installLayout(t, scratch(t, "state-good"),
		installShape{Payload: harness.ReadFile(t, testFixture(t).x64)})
	printed := stateAccepted(t, prefix).Stdout
	if !strings.Contains(printed, "matches the offline install") {
		t.Fatalf("the accepted state printed %q", printed)
	}
}

// TestNativeStateAutoUpdatesRefused: a JSON boolean false, not `"false"`, not true and
// not a missing key.
func TestNativeStateAutoUpdatesRefused(t *testing.T) {
	prefix := installLayout(t, scratch(t, "state-auto"), installShape{})
	for _, value := range []any{true, "false"} {
		writeNativeState(t, prefix, nativeState{InstallMethod: "native", AutoUpdates: value})
		stateRefused(t, prefix, "autoUpdates")
	}
	harness.WriteFile(t, statePath(prefix), encodeDocument(t, map[string]any{"installMethod": "native"}))
	stateRefused(t, prefix, "autoUpdates")
}

// TestNativeStateInstallMethodRefused: the method is "native", and a missing one is not.
func TestNativeStateInstallMethodRefused(t *testing.T) {
	prefix := installLayout(t, scratch(t, "state-method"), installShape{})
	for _, value := range []any{"npm", nil} {
		harness.WriteFile(t, statePath(prefix), encodeDocument(t, map[string]any{
			"installMethod": value,
			"autoUpdates":   false,
		}))
		stateRefused(t, prefix, "installMethod")
	}
}

// TestNativeStateDuplicateJSONKeyRefused: a repeated key is refused rather than resolved
// to the last value.
func TestNativeStateDuplicateJSONKeyRefused(t *testing.T) {
	prefix := installLayout(t, scratch(t, "state-duplicate"), installShape{})
	harness.WriteFile(t, statePath(prefix), []byte(
		`{"installMethod": "native", "autoUpdates": false, "autoUpdates": true}`+"\n"))
	stateRefused(t, prefix, "duplicate JSON key")
}

// TestNativeStateSymlinkedStateRefused: the state file is read as a regular file.
func TestNativeStateSymlinkedStateRefused(t *testing.T) {
	prefix := installLayout(t, scratch(t, "state-symlink"), installShape{})
	real := filepath.Join(prefix, "real.json")
	if err := os.Rename(statePath(prefix), real); err != nil {
		t.Fatalf("cannot move the state file: %v", err)
	}
	if err := os.Symlink(real, statePath(prefix)); err != nil {
		t.Fatalf("cannot write the state symlink: %v", err)
	}
	stateRefused(t, prefix, "not a regular file")
}

// TestNativeStateNonObjectRefused: an array is not a state document.
func TestNativeStateNonObjectRefused(t *testing.T) {
	prefix := installLayout(t, scratch(t, "state-array"), installShape{})
	harness.WriteFile(t, statePath(prefix), []byte("[]\n"))
	stateRefused(t, prefix, "not a JSON object")
}

// TestNativeStateDownloadedOrStagedFileRefused: a download or a staging file left in the
// prefix means the offline step was not the one that produced this install.
func TestNativeStateDownloadedOrStagedFileRefused(t *testing.T) {
	for _, relative := range []string{".claude/downloads", ".cache/claude/staging"} {
		t.Run(relative, func(t *testing.T) {
			prefix := installLayout(t, scratch(t, "state-leftover"), installShape{})
			harness.WriteFile(t, filepath.Join(prefix, relative, "claude-"+Version), []byte("x"))
			stateRefused(t, prefix, relative, "not empty")
			empty := installLayout(t, scratch(t, "state-empty"), installShape{})
			if err := os.MkdirAll(filepath.Join(empty, relative), 0o755); err != nil {
				t.Fatalf("cannot create %s: %v", relative, err)
			}
			stateAccepted(t, empty)
		})
	}
}
