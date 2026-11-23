package toolchain

import (
	"os"
	"path/filepath"
	"testing"
)

// Invariant 30: a malformed document is a refusal, never a panic. Both targets
// are seeded from the committed bytes, so `go test` alone (not only `go test
// -fuzz`) runs the seeds on every gate.

// FuzzLoadPin fuzzes the pin decoder and every rule that reads pin.json.
func FuzzLoadPin(f *testing.F) {
	committed, err := os.ReadFile("../../data/rust-toolchain/pin.json")
	if err != nil {
		f.Fatal(err)
	}
	f.Add(committed)
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	f.Add([]byte(`{"schema": 1}`))
	f.Add([]byte(`{"schema": 1, "trust": []}`))
	f.Add([]byte(`{"platforms": {"linux/amd64": []}}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, PinFile), raw, 0o644); err != nil {
			t.Fatal(err)
		}
		pin, err := LoadPin(directory, ReviewedTrust)
		if err != nil {
			if _, isRefusal := err.(*Refusal); !isRefusal {
				t.Fatalf("not a refusal: %T %v", err, err)
			}
			return
		}
		// An accepted pin must have both closures and a full required set.
		if len(pin.Closures) != 2 || len(pin.RequiredFiles()) < 19 {
			t.Fatalf("accepted pin is incomplete: %v", pin.RequiredFiles())
		}
	})
}

// FuzzManifestClosure fuzzes the manifest semantics against the committed pin.
func FuzzManifestClosure(f *testing.F) {
	committed, err := os.ReadFile("../../data/rust-toolchain/channel.toml")
	if err != nil {
		f.Fatal(err)
	}
	pin, err := LoadPin("../../data/rust-toolchain", ReviewedTrust)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(committed)
	f.Add([]byte(``))
	f.Add([]byte("manifest-version = \"2\"\n"))
	f.Add([]byte("[pkg]\nrust = 1\n"))
	f.Add([]byte("[[pkg.rust.target.x86_64-unknown-linux-gnu.components]]\npkg = 1\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if err := VerifyManifestClosure(raw, pin); err != nil {
			if _, isRefusal := err.(*Refusal); !isRefusal {
				t.Fatalf("not a refusal: %T %v", err, err)
			}
		}
	})
}
