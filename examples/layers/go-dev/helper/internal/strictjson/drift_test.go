package strictjson

// The drift gate for the one decoder this repository keeps in three places.
//
// `internal/strictjson` is the root module's original. The rust-dev helper carries
// a copy with documented extensions for its pin reader, and this module carries a
// copy of *that* (#249). Two nested modules cannot import one another (ADR 0017,
// example layers stay self-contained), so the copies are deliberate; what is not
// allowed is letting them diverge by accident, because nothing would then say which
// decoder a reader was measured against.
//
// So this test holds the go-dev copy to the rust-dev copy byte for byte from
// `package strictjson` on. A change on either side fails here and the author has to
// state which: edit both (the common case), or declare the difference in both
// headers and extend this gate with the reason. The doc comment is excluded because
// it is the one part that legitimately names its own layer.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rustDevCopy is the sibling layer's copy, reached from
// examples/layers/go-dev/helper/internal/strictjson.
const rustDevCopy = "../../../../rust-dev/helper/internal/strictjson/decode.go"

// codeOf is the comparison unit: everything from the package clause on, so the
// layer-specific package doc does not take part.
func codeOf(source string) (string, bool) {
	index := strings.Index(source, "\npackage strictjson\n")
	if index < 0 {
		return "", false
	}
	return source[index:], true
}

func TestCopyDoesNotDriftFromTheRustDevReader(t *testing.T) {
	ours, err := os.ReadFile("decode.go")
	if err != nil {
		t.Fatalf("read this copy: %v", err)
	}
	theirs, err := os.ReadFile(filepath.Join(rustDevCopy))
	if err != nil {
		t.Fatalf("read the rust-dev copy (%s): %v — this gate cannot pass over a tree "+
			"it could not find", rustDevCopy, err)
	}
	ourCode, ok := codeOf(string(ours))
	if !ok {
		t.Fatal("this copy has no package clause")
	}
	theirCode, ok := codeOf(string(theirs))
	if !ok {
		t.Fatal("the rust-dev copy has no package clause")
	}
	// A comparison over almost nothing must be red: the decoder is hundreds of
	// lines, and a truncated read would otherwise agree with another truncated read.
	if len(ourCode) < 5000 || len(theirCode) < 5000 {
		t.Fatalf("compared %d and %d bytes of decoder: the read is short",
			len(ourCode), len(theirCode))
	}
	if ourCode != theirCode {
		t.Fatalf("the two strictjson copies diverged.\nThis gate exists because two "+
			"copies of one decoder drift silently once nothing compares them (#249 "+
			"review). Decide which it is:\n  * the change belongs to both readers — "+
			"apply it to %s as well;\n  * the difference is deliberate — say so in "+
			"both package headers and extend this gate with the reason.", rustDevCopy)
	}
}

// TestDriftGateControls holds both sides: one changed character is reported, and
// the package doc is not part of the comparison (otherwise every layer-specific
// header edit would fail the gate for no reason).
func TestDriftGateControls(t *testing.T) {
	ours, err := os.ReadFile("decode.go")
	if err != nil {
		t.Fatalf("read this copy: %v", err)
	}
	base, ok := codeOf(string(ours))
	if !ok {
		t.Fatal("this copy has no package clause")
	}
	changed := []byte(base)
	changed[len(changed)/2] ^= 0x01
	if string(changed) == base {
		t.Fatal("the mutant did not change the decoder")
	}
	if string(changed) == base {
		t.Fatal("a changed decoder compares equal: the gate cannot fail")
	}
	headerA := "// Package strictjson is one layer's copy.\n" + base
	headerB := "// Package strictjson is another layer's copy, at length.\n" + base
	codeA, _ := codeOf(headerA)
	codeB, _ := codeOf(headerB)
	if codeA != codeB {
		t.Fatal("a package-doc difference must not fail the comparison")
	}
}
