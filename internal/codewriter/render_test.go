package codewriter

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
)

// repositoryRoot is the checkout this test runs in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot read the working directory: %s", err)
	}
	return filepath.Join(directory, "..", "..")
}

// committedPin is the committed closure the review accepted, read from disk.
func committedPin(t *testing.T) (codexpin.Pin, string) {
	t.Helper()
	directory := filepath.Join(repositoryRoot(t), PinDirectory)
	pin, err := codexpin.Load(directory)
	if err != nil {
		t.Fatalf("the committed pin does not load: %s", err)
	}
	return pin, directory
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %s", path, err)
	}
	return raw
}

func committedIdentities(pin codexpin.Pin) map[string]identity {
	identities := map[string]identity{}
	for _, platform := range codexpin.Platforms {
		pinned := pin.Platforms[platform]
		identities[platform] = identity{archive: pinned.SHA256, bundle: pinned.Bundle.SHA256}
	}
	return identities
}

// TestRenderReproducesTheCommittedPin is the whole point of the renderer: a refresh
// that selects the same bytes must produce the same file, byte for byte, or the
// review diff stops being readable.
func TestRenderReproducesTheCommittedPin(t *testing.T) {
	pin, directory := committedPin(t)
	rendered, err := Render(pin.Release, pin.Version,
		readFile(t, filepath.Join(directory, codexpin.MetadataPath)),
		readFile(t, filepath.Join(directory, codexpin.ChecksumsPath)),
		committedIdentities(pin), pin.Authentication.VerifiedOn)
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	committed := readFile(t, filepath.Join(repositoryRoot(t), PinFile))
	if !bytes.Equal(rendered, committed) {
		t.Errorf("a rendered refresh differs from the committed record\n--- committed\n%s\n--- rendered\n%s",
			committed, rendered)
	}
	loaded, err := codexpin.Parse(rendered, PinDirectory)
	if err != nil {
		t.Fatalf("the rendered pin does not load: %s", err)
	}
	if loaded.Release != pin.Release {
		t.Errorf("the rendered pin names %s, not %s", loaded.Release, pin.Release)
	}
	if loaded.Authentication.Identity != codexpin.Identity(pin.Release) {
		t.Errorf("the rendered identity is %s", loaded.Authentication.Identity)
	}
}

// TestRenderRecordsTheVerifiedDigests holds each platform to the digests this run
// downloaded, not to the committed record.
func TestRenderRecordsTheVerifiedDigests(t *testing.T) {
	pin, directory := committedPin(t)
	identities := committedIdentities(pin)
	identities["linux/arm64"] = identity{archive: strings.Repeat("ab", 32), bundle: strings.Repeat("cd", 32)}
	rendered, err := Render(pin.Release, pin.Version,
		readFile(t, filepath.Join(directory, codexpin.MetadataPath)),
		readFile(t, filepath.Join(directory, codexpin.ChecksumsPath)),
		identities, pin.Authentication.VerifiedOn)
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	loaded, err := codexpin.Parse(rendered, PinDirectory)
	if err != nil {
		t.Fatalf("the rendered pin does not load: %s", err)
	}
	arm := loaded.Platforms["linux/arm64"]
	if arm.SHA256 != strings.Repeat("ab", 32) || arm.Bundle.SHA256 != strings.Repeat("cd", 32) {
		t.Errorf("the rendered arm64 identity is %s / %s", arm.SHA256, arm.Bundle.SHA256)
	}
	if _, err := Render(pin.Release, pin.Version, nil, nil,
		map[string]identity{"linux/amd64": identities["linux/amd64"]},
		pin.Authentication.VerifiedOn); err == nil {
		t.Errorf("a pin without both platforms was rendered")
	}
}

func TestRenderArgumentRewritesExactlyOneLine(t *testing.T) {
	pin, _ := committedPin(t)
	path := filepath.Join(repositoryRoot(t), Dockerfile)
	committed := readFile(t, path)
	rendered, err := RenderArgument(string(committed), pin.Release)
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	if rendered != string(committed) {
		t.Errorf("rewriting the single slot at the committed release changed the file")
	}
	if err := RequireSingleSlot(rendered, pin.Release); err != nil {
		t.Errorf("the committed Dockerfile does not declare the slot: %s", err)
	}
	next, err := RenderArgument(string(committed), "rust-v0.0.1")
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	if err := RequireSingleSlot(next, "rust-v0.0.1"); err != nil {
		t.Errorf("the rewritten Dockerfile does not declare the slot: %s", err)
	}
	for label, dockerfile := range map[string]string{
		"no arg":   "LABEL " + Label + `="${` + Argument + `}"`,
		"two args": "ARG " + Argument + "=1\nARG " + Argument + "=1\n",
	} {
		if _, err := RenderArgument(dockerfile, "1"); err == nil {
			t.Errorf("%s: a Dockerfile without exactly one ARG is refused", label)
		}
	}
	for label, dockerfile := range map[string]string{
		"missing label":      "ARG " + Argument + "=1\n",
		"label without read": "ARG " + Argument + "=1\nLABEL " + Label + `="1"` + "\n",
		"another release":    "ARG " + Argument + "=2\nLABEL " + Label + `="${` + Argument + `}"` + "\n",
	} {
		if err := RequireSingleSlot(dockerfile, "1"); err == nil {
			t.Errorf("%s: a Dockerfile without exactly one ARG at the release is refused", label)
		}
	}
}

// TestUnownedViewMasksOnlyTheSingleSlotDefaults: the mask is what lets the bumper add
// its own ARG edits to the staged Dockerfile without a Codex refusal, and what still
// refuses an unrelated edit.
func TestUnownedViewMasksOnlyTheSingleSlotDefaults(t *testing.T) {
	committed := string(readFile(t, filepath.Join(repositoryRoot(t), Dockerfile)))
	before := unownedView(committed)
	masked := []string{}
	for _, name := range append([]string{Argument}, UnownedArgs...) {
		masked = append(masked, "ARG "+name+"=")
	}
	if !containsLine(before, "ARG "+Argument+"=<masked>") {
		t.Fatalf("the Codex ARG default is not masked:\n%s", strings.Join(before, "\n"))
	}

	// A Dockerfile that differs only in the three single-slot defaults is the same
	// file for this transaction.
	onlySlots := rewriteSlots(t, committed, "rust-v9.9.9", "1.2.3", "4.5.6")
	if !equalStrings(unownedView(onlySlots), before) {
		t.Errorf("a Dockerfile differing only in the single-slot defaults was not masked")
	}
	// Any other edit is a different file, and the Codex transaction refuses it.
	unrelated := strings.Replace(committed, "FROM ", "RUN true\nFROM ", 1)
	if equalStrings(unownedView(unrelated), before) {
		t.Errorf("an unrelated Dockerfile edit was masked away")
	}
	if len(masked) != 3 {
		t.Errorf("the single-slot set is %v", masked)
	}
}

func rewriteSlots(t *testing.T, dockerfile, codex, opencode, copilot string) string {
	t.Helper()
	rendered, err := RenderArgument(dockerfile, codex)
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	lines := strings.Split(rendered, "\n")
	for index, line := range lines {
		switch {
		case strings.HasPrefix(line, "ARG AGENT_VERSION_OPENCODE="):
			lines[index] = "ARG AGENT_VERSION_OPENCODE=" + opencode
		case strings.HasPrefix(line, "ARG AGENT_VERSION_COPILOT="):
			lines[index] = "ARG AGENT_VERSION_COPILOT=" + copilot
		}
	}
	return strings.Join(lines, "\n")
}

func containsLine(lines []string, wanted string) bool {
	for _, line := range lines {
		if line == wanted {
			return true
		}
	}
	return false
}
