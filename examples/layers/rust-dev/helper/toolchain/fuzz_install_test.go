package toolchain

import (
	"os"
	"path/filepath"
	"testing"
)

// FuzzPreflightArchive holds the archive walk to "refuse, never panic". The
// pre-pass parses attacker-controlled block offsets and PAX framing by hand, so a
// panic here would be a crash in a build, not a refusal.
func FuzzPreflightArchive(f *testing.F) {
	archive := ComponentArchive{Package: "cargo", Target: AMD64.Host(),
		MemberPrefix: MemberPrefix("cargo-1.0.0-x86_64-unknown-linux-gnu/")}
	f.Add([]byte("not an xz stream"))
	f.Add([]byte{0xfd, '7', 'z', 'X', 'Z', 0})
	path := filepath.Join(f.TempDir(), "seed.tar.xz")
	f.Fuzz(func(t *testing.T, raw []byte) {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = PreflightArchive(path, archive, NewEvents(""))
	})
}
