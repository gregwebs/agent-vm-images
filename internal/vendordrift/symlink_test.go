package vendordrift

// TestCheckThroughASymlinkedRoot is the gate's path-identity witness. A checkout
// reached through a symlinked prefix is the same checkout -- `/tmp` on macOS is a
// symlink to `/private/tmp`, `t.TempDir()` lives under the same shape, and a home
// directory can be one too -- so `Check` must reach the same answer from either
// spelling of the same tree.
//
// It failed before the gate canonicalized its own paths: `go list` reports
// resolved directories (`/private/tmp/.../vendor/...`) while the root the caller
// typed was `/tmp/...`, so the residency assertion compared two spellings of one
// path, saw the closure outside `vendor/`, and refused a correct tree.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckThroughASymlinkedRoot(t *testing.T) {
	root := repositoryRoot(t)
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(root, link); err != nil {
		t.Fatalf("symlink the checkout: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(link); err != nil || resolved == link {
		t.Fatalf("the witness needs a root that really is reached through a symlink: %q -> %q (%v)",
			link, resolved, err)
	}
	report, err := Check(Options{Root: link})
	if err != nil {
		t.Fatalf("a checkout reached through a symlink is the same checkout: %v", err)
	}
	// The derived closure must be non-empty in this form too, or a green result
	// here would be a comparison over nothing.
	if len(report.Imported) < MinPackages || len(report.Vendored) < MinPackages {
		t.Fatalf("derivation is implausibly small through the symlink: imported %v, vendored %v",
			report.Imported, report.Vendored)
	}
	if !sameStrings(report.Imported, report.Vendored) {
		t.Fatalf("imported %v, vendored %v", report.Imported, report.Vendored)
	}
	if report.Files < MinFiles {
		t.Fatalf("compared %d files through the symlink, want at least %d", report.Files,
			MinFiles)
	}
	if !report.VendorOnly {
		t.Fatal("the closure is not entirely under vendor/ when the root is a symlink")
	}
}
