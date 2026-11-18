package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// preflightRefused is one `preflight` invocation that must refuse.
func preflightRefused(r harness.Reporter, directory string, name platform, path string, needles ...string) string {
	r.Helper()
	return refuses(r, pinInvocation("preflight", directory, "--version", Version,
		"--platform", string(name.token), path), needles)
}

// TestPreflightBothMaintainedELFBlobsAccepted: the closure's own two blobs pass the
// digest, the size and the header check for their own machine.
func TestPreflightBothMaintainedELFBlobsAccepted(t *testing.T) {
	fixture := testFixture(t)
	for _, name := range platforms {
		path := fixture.x64
		if name.token == linuxARM64 {
			path = fixture.arm64
		}
		printed := accepts(t, pinInvocation("preflight", fixture.closure, "--version", Version,
			"--platform", string(name.token), path)).Stdout
		if !strings.Contains(printed, "matches the committed pin") {
			t.Fatalf("preflight %s printed %q", name.token, printed)
		}
	}
}

// TestPreflightDifferentDigestRefused: the downloaded bytes must be the pinned bytes.
func TestPreflightDifferentDigestRefused(t *testing.T) {
	fixture := testFixture(t)
	other := elfBlob(t, filepath.Join(scratch(t, "preflight-hash"), "blob"), amd64Machine,
		elfShape{Size: 8192})
	preflightRefused(t, fixture.closure, platforms[0], other, "sha256")
}

// TestPreflightDifferentSizeRefused: a size that disagrees with the pin is refused
// before the executable is placed, even when the digest chain is intact.
func TestPreflightDifferentSizeRefused(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.resign(t, "preflight-size", func(manifest map[string]any) {
		object(t, manifest, "platforms", string(linuxX64))["size"] = 4097
	})
	rewritePin(t, directory, func(pin map[string]any) {
		object(t, pin, "platforms", "linux/amd64")["size"] = 4097
	})
	preflightRefused(t, directory, platforms[0], fixture.x64, "bytes, but the pin records 4097")
}

// TestPreflightHashConsistentShellScriptRefused: a shell script that hashes correctly is
// still not this release's executable.
func TestPreflightHashConsistentShellScriptRefused(t *testing.T) {
	fixture := testFixture(t)
	path := filepath.Join(scratch(t, "preflight-script"), "claude")
	harness.WriteFile(t, path, []byte("#!/bin/sh\n# not an ELF object, but a valid closure payload\nexit 0\n"))
	// The closure is signed over exactly these bytes, so the SHA-256, the size and the
	// pin all agree; only the header says this is not an executable.
	directory := buildClosure(t, closureRequest{
		Out:     filepath.Join(scratch(t, "preflight-script-closure"), "source"),
		Signer:  fixture.signer,
		Version: Version,
		AMD64:   path,
		ARM64:   path,
	})
	preflightRefused(t, directory, platforms[0], path, "not an ELF object")
}

// TestPreflightELFClass32Refused: a 32-bit object is not a 64-bit executable.
func TestPreflightELFClass32Refused(t *testing.T) {
	fixture := testFixture(t)
	base := scratch(t, "preflight-class")
	path := elfBlob(t, filepath.Join(base, "blob"), amd64Machine, elfShape{Class: 1})
	directory := buildClosure(t, closureRequest{
		Out: filepath.Join(base, "source"), Signer: fixture.signer, Version: Version,
		AMD64: path, ARM64: path,
	})
	preflightRefused(t, directory, platforms[0], path, "not a 64-bit ELF object")
}

// TestPreflightBigEndianELFRefused: nor is a big-endian one.
func TestPreflightBigEndianELFRefused(t *testing.T) {
	fixture := testFixture(t)
	base := scratch(t, "preflight-endian")
	path := elfBlob(t, filepath.Join(base, "blob"), amd64Machine, elfShape{Endian: 2})
	directory := buildClosure(t, closureRequest{
		Out: filepath.Join(base, "source"), Signer: fixture.signer, Version: Version,
		AMD64: path, ARM64: path,
	})
	preflightRefused(t, directory, platforms[0], path, "not a little-endian ELF object")
}

// TestPreflightOtherArchitectureUnderAConsistentPinRefused: a consistent closure whose
// amd64 payload is the arm64 machine, so the digest chain agrees and the executable is
// still the wrong one.
func TestPreflightOtherArchitectureUnderAConsistentPinRefused(t *testing.T) {
	fixture := testFixture(t)
	base := scratch(t, "preflight-machine")
	directory := buildClosure(t, closureRequest{
		Out: filepath.Join(base, "source"), Signer: fixture.signer, Version: Version,
		AMD64: fixture.arm64, ARM64: fixture.arm64,
	})
	preflightRefused(t, directory, platforms[0], fixture.arm64, "targets ELF machine 183, not 62")
}

// TestPreflightSymlinkedFileRefused: the downloaded executable is read as a regular
// file, so a symlink cannot stand in for it.
func TestPreflightSymlinkedFileRefused(t *testing.T) {
	fixture := testFixture(t)
	link := filepath.Join(scratch(t, "preflight-symlink"), "claude")
	if err := os.Symlink(fixture.x64, link); err != nil {
		t.Fatalf("cannot write the symlink: %v", err)
	}
	preflightRefused(t, fixture.closure, platforms[0], link, "not a regular file")
}
