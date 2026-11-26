package pathjoin

import (
	"testing"
)

// TestJoinPreservesSpelling: the whole point of the package. A `..` after a symlink
// names a different file than a lexical clean, so the join must not touch it.
func TestJoinPreservesSpelling(t *testing.T) {
	for _, testCase := range []struct{ directory, name, want string }{
		{"/opt/closure", "pin.json", "/opt/closure/pin.json"},
		{"/opt/closure/", "pin.json", "/opt/closure/pin.json"},
		{"/opt/a/link/..", "pin.json", "/opt/a/link/../pin.json"},
		{"/", "pin.json", "/pin.json"},
		{"relative/dir", "pin.json", "relative/dir/pin.json"},
	} {
		if got := Join(testCase.directory, testCase.name); got != testCase.want {
			t.Errorf("Join(%q, %q) = %q, want %q", testCase.directory, testCase.name, got,
				testCase.want)
		}
	}
	// The control: filepath.Join is the operation this package exists to avoid, and
	// on the case above it produces a different path.
	if got := Join("/opt/a/link/..", "pin.json"); got == "/opt/a/pin.json" {
		t.Fatalf("Join normalised a path: %q", got)
	}
}
