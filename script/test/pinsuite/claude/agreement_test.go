package claude

import (
	"strings"
	"testing"
)

// TestAgreementManifestVersionDiffersFromThePin: the signed manifest's own version is
// the pin's, and a re-signed manifest that says otherwise is refused.
func TestAgreementManifestVersionDiffersFromThePin(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.resign(t, "manifest-version", func(manifest map[string]any) {
		manifest["version"] = "9.9.8"
	})
	checkRefused(t, directory, "signed manifest names 9.9.8")
}

// TestAgreementManifestChecksumDiffersFromThePin: the signed checksum is the pin's.
func TestAgreementManifestChecksumDiffersFromThePin(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.resign(t, "manifest-checksum", func(manifest map[string]any) {
		object(t, manifest, "platforms", string(linuxX64))["checksum"] = strings.Repeat("a", 64)
	})
	checkRefused(t, directory, "does not carry the pinned linux-x64")
}

// TestAgreementManifestSizeDiffersFromThePin: so is the signed size.
func TestAgreementManifestSizeDiffersFromThePin(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.resign(t, "manifest-size", func(manifest map[string]any) {
		object(t, manifest, "platforms", string(linuxX64))["size"] = 4097
	})
	checkRefused(t, directory, "does not carry the pinned linux-x64")
}

// TestAgreementManifestBinaryIsNotClaude: the manifest names the binary it ships.
func TestAgreementManifestBinaryIsNotClaude(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.resign(t, "manifest-binary", func(manifest map[string]any) {
		object(t, manifest, "platforms", string(linuxX64))["binary"] = "claude.zst"
	})
	checkRefused(t, directory, "binary is")
}

// TestAgreementManifestMissingAMaintainedPlatform: both maintained architectures must
// be in the signed manifest, not only the one this build wants.
func TestAgreementManifestMissingAMaintainedPlatform(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.resign(t, "manifest-missing", func(manifest map[string]any) {
		delete(object(t, manifest, "platforms"), string(linuxX64))
	})
	checkRefused(t, directory, "has no linux-x64 platform")
}

// TestAgreementUnpinnedVersionRefused: an exact but unreviewed version is refused, and
// the refusal says why.
func TestAgreementUnpinnedVersionRefused(t *testing.T) {
	fixture := testFixture(t)
	checkRefusedAt(t, fixture.closure, "9.9.8", nil, "not the pinned Claude Code version")
}

// TestAgreementMuslTokenRefused: the manifest carries a musl platform; the pin does
// not, so the token cannot be selected.
func TestAgreementMuslTokenRefused(t *testing.T) {
	fixture := testFixture(t)
	refuses(t, pinInvocation("select", fixture.closure, "--version", Version, "--platform",
		string(linuxX64Musl)), []string{"not a maintained Claude Code platform"})
}
