package claude

import (
	"path/filepath"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// flipped refuses one evidence file whose first byte was changed, with a gpg on PATH
// that fails loudly: the digest refusal must come first, so an empty log is the
// evidence that nothing was asked of the keyring.
func flipped(t *testing.T, fixture *fixture, relative, label string) {
	t.Helper()
	directory := fixture.copy(t, label)
	path := filepath.Join(directory, filepath.FromSlash(relative))
	raw := harness.ReadFile(t, path)
	raw[0] ^= 0xFF
	harness.WriteFile(t, path, raw)
	probe := newGPGStub(t, scratch(t, label+"-bin"), stubRequest{Mode: stubBomb})
	text := checkRefusedAt(t, directory, Version, probe.Env, "does not match the pin digest")
	if logged := probe.homes(t); len(logged) != 0 {
		t.Fatalf("%s: the refusal named %q and still ran gpg for %v", label, text, logged)
	}
}

// TestEvidenceFlippedManifestByteRefusedWithoutGPG: the committed evidence is re-hashed
// before anything is verified.
func TestEvidenceFlippedManifestByteRefusedWithoutGPG(t *testing.T) {
	flipped(t, testFixture(t), "evidence/manifest.json", "flip-manifest")
}

// TestEvidenceFlippedSignatureByteRefusedWithoutGPG: the detached signature is re-hashed
// the same way.
func TestEvidenceFlippedSignatureByteRefusedWithoutGPG(t *testing.T) {
	flipped(t, testFixture(t), "evidence/manifest.json.sig", "flip-signature")
}

// TestEvidenceFlippedKeyByteRefusedWithoutGPG: so is the committed release key.
func TestEvidenceFlippedKeyByteRefusedWithoutGPG(t *testing.T) {
	flipped(t, testFixture(t), keyName, "flip-key")
}
