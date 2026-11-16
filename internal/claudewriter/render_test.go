package claudewriter

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/claudepin"
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

// committedData is the committed closure read from disk, so the renderer is held to
// the record a review accepted rather than to its own output.
func committedData(t *testing.T) (pin claudepin.Pin, upstream fetched, key []byte) {
	t.Helper()
	root := filepath.Join(repositoryRoot(t), PinDirectory)
	pin, err := claudepin.Load(root)
	if err != nil {
		t.Fatalf("the committed pin does not load: %s", err)
	}
	evidence := func(relative string) []byte {
		raw, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatalf("%s: %s", relative, err)
		}
		return raw
	}
	upstream = fetched{
		manifest:     evidence(claudepin.ManifestPath),
		signature:    evidence(claudepin.SignaturePath),
		publishedKey: evidence(claudepin.KeyPath),
	}
	return pin, upstream, upstream.publishedKey
}

func downloadedOf(pin claudepin.Pin) map[string]downloaded {
	executables := map[string]downloaded{}
	for token, executable := range pin.Executables {
		executables[token] = downloaded{token: token, sha256: executable.SHA256,
			size: executable.Size}
	}
	return executables
}

// TestRenderReproducesTheCommittedPin is the whole point of the renderer: a refresh
// that selects the same bytes must produce the same file, byte for byte, or the
// review diff stops being readable.
func TestRenderReproducesTheCommittedPin(t *testing.T) {
	pin, upstream, key := committedData(t)
	rendered, err := Render(pin.Version, upstream, key, downloadedOf(pin),
		pin.VerifiedOn.Format("2006-01-02"), claudepin.ReviewedSignerFingerprint)
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	committed, err := os.ReadFile(filepath.Join(repositoryRoot(t), PinFile))
	if err != nil {
		t.Fatalf("read the committed pin: %s", err)
	}
	if !bytes.Equal(rendered, committed) {
		t.Errorf("a rendered refresh differs from the committed record\n--- committed\n%s\n--- rendered\n%s",
			committed, rendered)
	}
	loaded, err := claudepin.Parse(rendered, PinDirectory)
	if err != nil {
		t.Fatalf("the rendered pin does not load: %s", err)
	}
	if err := loaded.CheckPolicy(); err != nil {
		t.Errorf("the rendered pin fails the signer policy: %s", err)
	}
}

// TestRenderRecordsTheVerifiedIdentity holds each field a build acts on to the bytes
// this run was given, not to the committed record.
func TestRenderRecordsTheVerifiedIdentity(t *testing.T) {
	pin, upstream, key := committedData(t)
	executables := downloadedOf(pin)
	executables["linux-arm64"] = downloaded{token: "linux-arm64",
		sha256: strings.Repeat("ab", 32), size: 4242}
	rendered, err := Render(pin.Version, upstream, key, executables, "2026-01-02",
		claudepin.ReviewedSignerFingerprint)
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	loaded, err := claudepin.Parse(rendered, PinDirectory)
	if err != nil {
		t.Fatalf("the rendered pin does not load: %s", err)
	}
	arm := loaded.Executables["linux-arm64"]
	if arm.SHA256 != strings.Repeat("ab", 32) || arm.Size != 4242 {
		t.Errorf("the rendered arm64 identity is %s / %d", arm.SHA256, arm.Size)
	}
	if loaded.VerifiedOn.Format("2006-01-02") != "2026-01-02" {
		t.Errorf("the rendered verified_on is %s", loaded.VerifiedOn)
	}
	if loaded.Reason != Reason {
		t.Errorf("the rendered reason is not the review record: %s", loaded.Reason)
	}
}

func TestPayloadDeltaNamesEveryTransition(t *testing.T) {
	pin, _, _ := committedData(t)
	committed := claudepin.VerifiedPin{Pin: pin, Manifest: claudepin.Manifest{Commit: "abc"},
		Signature: claudepin.Signature{PrimaryFingerprint: pin.Key.Fingerprint,
			Created: "2026-11-05"}}
	proposed := committed
	proposed.Pin.Version = "2.1.320"
	proposed.Manifest.Commit = "def"
	proposed.Signature.Created = "2026-11-20"
	before := pin.Executables["linux-x64"]
	after := before
	after.SHA256 = strings.Repeat("cd", 32)
	after.Size = 99
	proposed.Pin.Executables = map[string]claudepin.Executable{"linux-x64": after,
		"linux-arm64": pin.Executables["linux-arm64"]}
	delta := payloadDelta(&committed, proposed)
	for _, needle := range []string{
		"claude payload delta: " + pin.Version + " -> 2.1.320",
		"manifest     sha256 " + pin.Manifest.SHA256,
		"abc -> def",
		"2026-11-05 -> 2026-11-20",
		before.SHA256 + " 340226576 B -> " + after.SHA256 + " 99 B",
	} {
		if !strings.Contains(delta, needle) {
			t.Errorf("the delta does not mention %q:\n%s", needle, delta)
		}
	}
	first := payloadDelta(nil, proposed)
	if !strings.Contains(first, "claude payload delta: (none) -> 2.1.320") {
		t.Errorf("a first adoption must say (none):\n%s", first)
	}
}

func TestRenderArgumentRewritesExactlyOneLine(t *testing.T) {
	committed, err := os.ReadFile(filepath.Join(repositoryRoot(t), Dockerfile))
	if err != nil {
		t.Fatalf("read the Dockerfile: %s", err)
	}
	rendered, err := RenderArgument(string(committed), "2.1.320")
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	if err := RequireSingleSlot(rendered, "2.1.320"); err != nil {
		t.Errorf("the rendered Dockerfile does not declare the slot: %s", err)
	}
	if strings.Contains(rendered, "ARG "+Argument+"=2.1.319") {
		t.Errorf("the previous default is still there")
	}

	for label, dockerfile := range map[string]string{
		"no arg":      "LABEL " + Label + `="${` + Argument + `}"`,
		"two args":    "ARG " + Argument + "=1\nARG " + Argument + "=1\n",
		"no label":    "ARG " + Argument + "=1\n",
		"other label": "ARG " + Argument + "=1\nLABEL " + Label + `="x"`,
	} {
		if _, err := RenderArgument(dockerfile, "1"); err == nil {
			t.Errorf("%s: a Dockerfile without exactly one ARG and its label is refused", label)
		}
	}
}
