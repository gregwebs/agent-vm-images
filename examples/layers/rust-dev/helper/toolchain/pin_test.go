package toolchain

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// committedPinRaw is the reviewed pin.json.
func committedPinRaw(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(committedPinDir(t), "pin.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// mutatePin decodes the committed pin, applies change to the tree and re-encodes
// it. The evidence digests are unchanged, so a mutation reaches the rule that
// reads the field rather than the directory audit.
func mutatePin(t *testing.T, change func(document map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(committedPinRaw(t), &document); err != nil {
		t.Fatal(err)
	}
	change(document)
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// writePin writes a pin.json variant into a copy of the committed directory.
func writePin(t *testing.T, raw []byte) string {
	t.Helper()
	directory := t.TempDir()
	source := committedPinDir(t)
	if err := copyTree(source, directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, PinFile), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return directory
}

func copyTree(source, destination string) error {
	return filepath.Walk(source, func(current string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(current)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
}

// TestLoadPinAcceptsCommittedPin is the accept control: if this fails, every
// refusal below is meaningless.
func TestLoadPinAcceptsCommittedPin(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatalf("the committed pin was refused: %v", err)
	}
	if pin.Channel != "1.98.1" {
		t.Fatalf("channel is %q", pin.Channel)
	}
	if pin.RustupVersion != "1.28.2" {
		t.Fatalf("rustup version is %q", pin.RustupVersion)
	}
	if len(pin.Closures) != 2 {
		t.Fatalf("closures: %d", len(pin.Closures))
	}
	for _, arch := range Architectures() {
		if got := len(pin.Closure(arch).Archives); got != 6 {
			t.Fatalf("%s: %d archives", arch.Platform(), got)
		}
	}
	// Five top-level files, two bootstrap checksums and twelve archive
	// signatures: the 14 evidence files plus the pin, the manifest and its pair.
	if got := len(pin.RequiredFiles()); got != 19 {
		t.Fatalf("required files: %d (%v)", got, pin.RequiredFiles())
	}
}

// TestReviewedTrustIsTheCommittedRecord holds the compiled policy to the committed
// record: widening the compiled signer set fails here, not only in a review.
func TestReviewedTrustIsTheCommittedRecord(t *testing.T) {
	// The literal, not the constant: a changed constant must not move the
	// expectation with it.
	const reviewed Fingerprint = "108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE"
	if ReviewedTrust.Primary() != reviewed {
		t.Fatalf("primary is %q", ReviewedTrust.Primary())
	}
	if len(ReviewedTrust.Signers()) != 1 || ReviewedTrust.Signers()[0] != reviewed {
		t.Fatalf("signers are %v", ReviewedTrust.Signers())
	}
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	if pin.Primary != ReviewedTrust.Primary() {
		t.Fatalf("pin primary %q != compiled %q", pin.Primary, ReviewedTrust.Primary())
	}
	if len(pin.Signers) != len(ReviewedTrust.Signers()) {
		t.Fatalf("pin signers %v != compiled %v", pin.Signers, ReviewedTrust.Signers())
	}
	for index, signer := range pin.Signers {
		if signer != ReviewedTrust.Signers()[index] {
			t.Fatalf("pin signer %d is %q, compiled %q", index, signer,
				ReviewedTrust.Signers()[index])
		}
	}
}

// TestRequireReviewedTrust is the trust-root and signer-set rule on its own.
func TestRequireReviewedTrust(t *testing.T) {
	const reviewed Fingerprint = "108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE"
	other := Fingerprint("0000000000000000000000000000000000000000")
	cases := []struct {
		label   string
		primary Fingerprint
		signers []Fingerprint
		frag    string
	}{
		{"reviewed", reviewed, []Fingerprint{reviewed}, ""},
		{"other primary", other, []Fingerprint{other}, "is not the reviewed primary key"},
		{"extra signer", reviewed,
			[]Fingerprint{reviewed, other}, "is not in the reviewed signer set"},
		{"no signers", reviewed, nil, ""},
	}
	for _, test := range cases {
		err := requireReviewedTrust(test.primary, test.signers, ReviewedTrust)
		if test.frag == "" && err != nil {
			t.Errorf("%s: refused: %v", test.label, err)
		}
		if test.frag != "" && (err == nil || !strings.Contains(err.Error(), test.frag)) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.frag)
		}
	}
}

// TestLoadPinRefusals is invariant rows 1–2, 7–11, 13–18: one sub-test per rule,
// each naming the fragment the source reader reports.
func TestLoadPinRefusals(t *testing.T) {
	cases := []struct {
		label string
		raw   func(t *testing.T) []byte
		frag  string
	}{
		{"empty file", func(t *testing.T) []byte { return nil }, "not valid JSON"},
		{"top-level array", func(t *testing.T) []byte { return []byte("[]") },
			"expected an object"},
		{"trailing document", func(t *testing.T) []byte {
			return append(committedPinRaw(t), []byte(" {}")...)
		}, "extra data after the document"},
		{"leading BOM", func(t *testing.T) []byte {
			return append([]byte{0xEF, 0xBB, 0xBF}, committedPinRaw(t)...)
		}, "leading UTF-8 BOM is not accepted"},
		{"invalid UTF-8", func(t *testing.T) []byte { return []byte("{\"a\": \"\xff\"}") },
			"not valid UTF-8"},
		{"NaN", func(t *testing.T) []byte { return []byte(`{"schema": NaN}`) },
			"NaN is not valid JSON"},
		{"Infinity", func(t *testing.T) []byte { return []byte(`{"schema": Infinity}`) },
			"Infinity is not valid JSON"},
		{"-Infinity", func(t *testing.T) []byte { return []byte(`{"schema": -Infinity}`) },
			"-Infinity is not valid JSON"},
		{"duplicate key", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"schema": 1,`),
				[]byte(`"schema": 1, "schema": 1,`), 1)
		}, "duplicate key 'schema'"},
		{"duplicate key through an escape", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"schema": 1,`),
				[]byte(`"schema": 1, "\u0073chema": 1,`), 1)
		}, "duplicate key 'schema'"},
		{"nested duplicate", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"path": "rust-release-key.asc",`),
				[]byte(`"path": "rust-release-key.asc", "path": "rust-release-key.asc",`), 1)
		}, "duplicate key 'path'"},
		{"raw control character", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"reviewer": "gregwebs"`),
				[]byte("\"reviewer\": \"greg\x01webs\""), 1)
		}, "invalid control character in a string"},
		{"deep nesting", func(t *testing.T) []byte {
			return []byte(strings.Repeat("[", 100000))
		}, "not valid JSON"},
		{"unknown top-level key", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { d["extra"] = 1 })
		}, "pin.json: unknown keys: extra"},
		{"missing top-level key", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { delete(d, "profile") })
		}, "pin.json: missing profile"},
		{"schema bool", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { d["schema"] = true })
		}, "schema: expected an integer"},
		{"schema float", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"schema": 1`),
				[]byte(`"schema": 1.0`), 1)
		}, "schema: expected an integer"},
		{"schema exponent", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"schema": 1`),
				[]byte(`"schema": 1e0`), 1)
		}, "schema: expected an integer"},
		{"schema 2", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { d["schema"] = 2 })
		}, "pin.json: unsupported schema"},
		// Python's int has no range, so this is an integer that is not 1.
		{"schema 2^64", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"schema": 1`),
				[]byte(`"schema": 18446744073709551616`), 1)
		}, "pin.json: unsupported schema"},
		{"schema -0", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"schema": 1`),
				[]byte(`"schema": -0`), 1)
		}, "pin.json: unsupported schema"},
		{"channel", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { d["channel"] = "1.98" })
		}, "channel: expected an exact X.Y.Z release channel"},
		{"date", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { d["release_date"] = "2026-02-30" })
		}, "release_date: not a calendar date: 2026-02-30"},
		{"profile", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { d["profile"] = "complete" })
		}, "pin.json: profile must be minimal"},
		{"unaudited rustup", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) { d["rustup_version"] = "1.28.1" })
		}, "is not an audited installer"},
		{"key path", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["trust"].(map[string]any)["key"].(map[string]any)["path"] = "other.asc"
			})
		}, "pin.json: key path must be rust-release-key.asc"},
		{"empty signers", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["trust"].(map[string]any)["signing_fingerprints"] = []any{}
			})
		}, "trust.signing_fingerprints must not be empty"},
		{"fixture primary", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["trust"].(map[string]any)["primary_fingerprint"] =
					"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
			})
		}, "is not the reviewed primary key"},
		{"extra signer", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["trust"].(map[string]any)["signing_fingerprints"] = []any{
					"108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE",
					"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
			})
		}, "is not in the reviewed signer set"},
		{"manifest url", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["manifest"].(map[string]any)["url"] =
					"https://static.rust-lang.org/dist/channel-rust-1.98.0.toml"
			})
		}, "manifest URL is not the canonical channel manifest origin"},
		{"manifest checksum url", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["manifest"].(map[string]any)["checksum"].(map[string]any)["url"] =
					"https://static.rust-lang.org/dist/other.toml.sha256"
			})
		}, "manifest checksum URL is not <manifest>.sha256"},
		{"manifest signature url", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["manifest"].(map[string]any)["signature"].(map[string]any)["url"] =
					"https://static.rust-lang.org/dist/other.toml.asc"
			})
		}, "manifest signature URL is not <manifest>.asc"},
		{"lookalike host", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["manifest"].(map[string]any)["url"] =
					"https://static.rust-lang.org.evil.test/dist/channel-rust-1.98.1.toml"
			})
		}, "not a canonical rust-lang.org HTTPS URL"},
		{"query string", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["manifest"].(map[string]any)["url"] =
					"https://static.rust-lang.org/dist/channel-rust-1.98.1.toml?a=1"
			})
		}, "not a canonical rust-lang.org HTTPS URL"},
		{"url traversal", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["manifest"].(map[string]any)["url"] =
					"https://static.rust-lang.org/dist/../channel-rust-1.98.1.toml"
			})
		}, "URL path is not canonical"},
		{"platforms missing", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				delete(d["platforms"].(map[string]any), "linux/arm64")
			})
		}, "platforms must be exactly linux/amd64 and linux/arm64"},
		{"missing platform", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"] = map[string]any{}
			})
		}, "platforms.linux/amd64: missing host, musl, bootstrap, archives"},
		{"foreign host", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["host"] =
					"aarch64-unknown-linux-gnu"
			})
		}, "host/musl triples do not match the architecture"},
		{"bootstrap url", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["bootstrap"].(map[string]any)["url"] =
					"https://static.rust-lang.org/rustup/archive/1.28.1/x86_64-unknown-linux-gnu/rustup-init"
			})
		}, "bootstrap URL is not the versioned rustup-init origin"},
		{"bootstrap checksum path", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["bootstrap"].(map[string]any)["checksum"].(map[string]any)["path"] =
					"evidence/other.sha256"
			})
		}, "bootstrap checksum evidence path must be evidence/rustup-amd64.sha256"},
		{"extra archive", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				archives := d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["archives"].([]any)
				extra := map[string]any{}
				for key, value := range archives[0].(map[string]any) {
					extra[key] = value
				}
				extra["package"] = "rust-src"
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["archives"] =
					append(archives, extra)
			})
		}, "unexpected selected archive 'rust-src'"},
		{"missing musl", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				platform := d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)
				archives := platform["archives"].([]any)
				kept := make([]any, 0, len(archives)-1)
				for _, archive := range archives {
					if archive.(map[string]any)["target"] == "x86_64-unknown-linux-musl" {
						continue
					}
					kept = append(kept, archive)
				}
				platform["archives"] = kept
			})
		}, "selected closure is incomplete: missing rust-std x86_64-unknown-linux-musl"},
		{"reordered archives", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				platform := d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)
				archives := platform["archives"].([]any)
				platform["archives"] = []any{archives[1], archives[0], archives[2], archives[3],
					archives[4], archives[5]}
			})
		}, "selected archives must be in canonical order"},
		{"duplicate archive", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				platform := d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)
				archives := platform["archives"].([]any)
				platform["archives"] = append(archives, archives[0])
			})
		}, "duplicate selected archive"},
		{"archive url", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["archives"].([]any)[0].(map[string]any)["url"] =
					"https://static.rust-lang.org/dist/2026-09-02/cargo-1.98.1-x86_64-unknown-linux-gnu.tar.xz"
			})
		}, "platforms.linux/amd64.cargo: archive URL is not the canonical dated origin"},
		{"archive member prefix", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["archives"].([]any)[0].(map[string]any)["member_prefix"] =
					"cargo-1.98.1-x86_64-unknown-linux-musl/"
			})
		}, "member prefix does not match the archive name"},
		{"evidence path ../x", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["archives"].([]any)[0].(map[string]any)["signature"].(map[string]any)["path"] =
					"evidence/../x"
			})
		}, "platforms.linux/amd64.cargo.signature.path: not a canonical relative path"},
		{"signature evidence path", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["platforms"].(map[string]any)["linux/amd64"].(map[string]any)["archives"].([]any)[0].(map[string]any)["signature"].(map[string]any)["path"] =
					"evidence/amd64-rustc.asc"
			})
		}, "signature evidence path must be evidence/amd64-cargo.asc"},
		{"exception unbound", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["bootstrap_exception"].(map[string]any)["scope"].(map[string]any)["linux/amd64"].(map[string]any)["sha256"] =
					strings.Repeat("0", 64)
			})
		}, "exception is not bound to the committed bootstrap URL and digest"},
		{"exception method", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["bootstrap_exception"].(map[string]any)["method"] = "unsigned"
			})
		}, "bootstrap_signature policy must be the reviewed hash-only exception"},
		{"exception scope", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				delete(d["bootstrap_exception"].(map[string]any)["scope"].(map[string]any), "linux/arm64")
			})
		}, "bootstrap_exception.scope: must cover exactly linux/amd64 and linux/arm64"},
		{"empty reason", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["bootstrap_exception"].(map[string]any)["reason"] = "   "
			})
		}, "reason, reassess_when and evidence must be recorded"},
		{"reviewer handle", func(t *testing.T) []byte {
			return mutatePin(t, func(d map[string]any) {
				d["bootstrap_exception"].(map[string]any)["reviewer"] = "a reviewer"
			})
		}, "expected a simple reviewer handle"},
		{"surrogate in reviewer", func(t *testing.T) []byte {
			return bytes.Replace(committedPinRaw(t), []byte(`"reviewer": "gregwebs"`),
				[]byte(`"reviewer": "greg\ud800webs"`), 1)
		}, "control character in identity string"},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			directory := writePin(t, test.raw(t))
			_, err := LoadPin(directory, ReviewedTrust)
			if err == nil || !strings.Contains(err.Error(), test.frag) {
				t.Fatalf("got %v, want a refusal naming %q", err, test.frag)
			}
		})
	}
}

// TestPinFileMustBeARegularFile is invariant 18: a symlinked pin.json is refused
// before the directory audit, with `missing input`.
func TestPinFileMustBeARegularFile(t *testing.T) {
	directory := t.TempDir()
	if err := copyTree(committedPinDir(t), directory); err != nil {
		t.Fatal(err)
	}
	pin := filepath.Join(directory, PinFile)
	target := filepath.Join(directory, "pin-real.json")
	if err := os.Rename(pin, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, pin); err != nil {
		t.Fatal(err)
	}
	_, err := LoadPin(directory, ReviewedTrust)
	if err == nil || !strings.Contains(err.Error(), "missing input: pin.json") {
		t.Fatalf("got %v, want `missing input: pin.json`", err)
	}
}

// TestSelectionOrder holds the canonical order rule on its own.
func TestSelectionOrder(t *testing.T) {
	arch := AMD64
	var want [][2]string
	for _, pair := range SelectedPairs(arch) {
		want = append(want, [2]string{pair.Package, string(pair.Target)})
	}
	if err := requireSelection(arch, want); err != nil {
		t.Fatalf("canonical order refused: %v", err)
	}
	swapped := [][2]string{want[1], want[0], want[2], want[3], want[4], want[5]}
	if err := requireSelection(arch, swapped); err == nil ||
		!strings.Contains(err.Error(), "canonical order") {
		t.Fatalf("swapped order: %v", err)
	}
	if err := requireSelection(arch, append(want, want[0])); err == nil ||
		!strings.Contains(err.Error(), "duplicate selected archive") {
		t.Fatalf("duplicate: %v", err)
	}
}
