package toolchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/gpg"
)

// committedManifest is the reviewed channel manifest.
func committedManifest(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(committedPinDir(t), ManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// manifestWith is the committed manifest with one replacement applied, exactly
// once.
func manifestWith(t *testing.T, old, replacement string) []byte {
	t.Helper()
	raw := string(committedManifest(t))
	if strings.Count(raw, old) != 1 {
		t.Fatalf("the manifest holds %d occurrences of %q", strings.Count(raw, old), old)
	}
	return []byte(strings.Replace(raw, old, replacement, 1))
}

// TestRealManifestSelection is invariant 24: the committed manifest selects
// exactly the committed closure, and every documented mutation is refused with
// the source reader's diagnostic.
func TestRealManifestSelection(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyManifestClosure(committedManifest(t), pin); err != nil {
		t.Fatalf("the committed manifest was refused: %v", err)
	}
	const host = "x86_64-unknown-linux-gnu"
	cases := []struct {
		label string
		raw   []byte
		frag  string
	}{
		{"manifest-version", manifestWith(t, "manifest-version = \"2\"",
			"manifest-version = \"3\""), "manifest: unsupported manifest-version"},
		{"date", manifestWith(t, "date = \"2026-09-03\"", "date = \"2026-09-02\""),
			"manifest: release date does not match the pin"},
		{"unquoted date", manifestWith(t, "date = \"2026-09-03\"", "date = 2026-09-03"),
			"manifest.date: expected YYYY-MM-DD"},
		{"rust version", manifestWith(t, "[pkg.rust]\nversion = \"1.98.1 (48a229cea 2026-09-01)\"",
			"[pkg.rust]\nversion = \"1.98.0 (48a229cea 2026-09-01)\""),
			"manifest: release version does not match the pinned channel"},
		{"rename target", manifestWith(t, "[renames.clippy]\nto = \"clippy-preview\"",
			"[renames.clippy]\nto = \"clippy\""),
			"manifest: unexpected rename for clippy; schema review required"},
		{"missing rename", manifestWith(t, "[renames.rustfmt]\nto = \"rustfmt-preview\"\n",
			""), "manifest: missing rename for rustfmt; schema review required"},
		{"minimal profile", manifestWith(t,
			"minimal = [\"rustc\", \"cargo\", \"rust-std\", \"rust-mingw\"]",
			"minimal = [\"rustc\", \"cargo\", \"rust-std\"]"),
			"manifest: the minimal profile changed; schema/policy review required"},
		{"top-level unknown key", manifestWith(t, "manifest-version = \"2\"",
			"manifest-version = \"2\"\nextra = 1"), "manifest: unknown keys: extra"},
		{"wildcard extension", manifestWith(t,
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"rust-src\"\ntarget = \"*\"",
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"rustfmt-preview\"\ntarget = \"*\""),
			"manifest: rustup would install the target-independent component(s)"},
		{"extension flag", manifestWith(t,
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"rust-src\"",
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"rust-src\"\nextension = true"),
			"`extension` is not a rustup component field"},
		{"clippy extension for another target", manifestWith(t,
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"clippy-preview\"\ntarget = \""+host+"\"",
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"clippy-preview\"\ntarget = \"aarch64-unknown-linux-gnu\""),
			"is not a single " + host + " extension"},
		{"musl extension for another target", manifestWith(t,
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"rust-std\"\ntarget = \"x86_64-unknown-linux-musl\"",
			"[[pkg.rust.target."+host+".extensions]]\npkg = \"rust-std\"\ntarget = \"i686-unknown-linux-musl\""),
			"rust-std for x86_64-unknown-linux-musl is not a single " + host + " extension"},
		{"unavailable archive", manifestWith(t,
			"[pkg.clippy-preview.target."+host+"]\navailable = true",
			"[pkg.clippy-preview.target."+host+"]\navailable = false"),
			"manifest: clippy-preview " + host + " is not available"},
		{"xz url", manifestWith(t,
			"xz_url = \"https://static.rust-lang.org/dist/2026-09-03/clippy-1.98.1-"+host+".tar.xz\"",
			"xz_url = \"https://static.rust-lang.org/dist/2026-09-02/clippy-1.98.1-"+host+".tar.xz\""),
			"manifest: xz_url for clippy-preview " + host + " differs from the pin"},
		{"missing xz pair", manifestWith(t,
			"xz_url = \"https://static.rust-lang.org/dist/2026-09-03/clippy-1.98.1-"+host+".tar.xz\"\n",
			""), "manifest: clippy-preview " + host + " lacks a full xz pair"},
		{"zstd", manifestWith(t,
			"[pkg.clippy-preview.target."+host+"]\navailable = true",
			"[pkg.clippy-preview.target."+host+"]\navailable = true\nzst_url = \"https://static.rust-lang.org/dist/2026-09-03/clippy.tar.zst\""),
			"advertises zstd; rustup 1.28.2 prefers zstd"},
		{"half gzip pair", manifestWith(t,
			"url = \"https://static.rust-lang.org/dist/2026-09-03/clippy-1.98.1-"+host+".tar.gz\"\n",
			""), "carries a half gzip format pair"},
		{"gzip origin", manifestWith(t,
			"url = \"https://static.rust-lang.org/dist/2026-09-03/clippy-1.98.1-"+host+".tar.gz\"",
			"url = \"https://static.rust-lang.org/dist/2026-09-02/clippy-1.98.1-"+host+".tar.gz\""),
			"gzip url is not the same dated origin as its xz pair"},
		{"rust host entry unavailable", manifestWith(t,
			"[pkg.rust.target."+host+"]\navailable = true",
			"[pkg.rust.target."+host+"]\navailable = false"),
			"manifest: no available rust package for " + host},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			err := VerifyManifestClosure(test.raw, pin)
			if err == nil || !strings.Contains(err.Error(), test.frag) {
				t.Fatalf("got %v, want a refusal naming %q", err, test.frag)
			}
		})
	}
}

// TestManifestStructure is the P1 half of row 24: a field of the wrong kind is a
// refusal, never a panic.
func TestManifestStructure(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		label string
		doc   string
	}{
		{"pkg not a table", "manifest-version = \"2\"\ndate = \"2026-09-03\"\npkg = 1\n" +
			"[renames.clippy]\nto = \"clippy-preview\"\n[renames.rustfmt]\nto = \"rustfmt-preview\"\n" +
			"[profiles]\nminimal = [\"rustc\", \"cargo\", \"rust-std\", \"rust-mingw\"]\n"},
		{"renames a string", "manifest-version = \"2\"\ndate = \"2026-09-03\"\n" +
			"[pkg.rust]\nversion = \"1.98.1 (x)\"\nrenames = \"x\"\n"},
		{"profiles not a table", "manifest-version = \"2\"\ndate = \"2026-09-03\"\n" +
			"[pkg.rust]\nversion = \"1.98.1 (x)\"\nprofiles = 1\n"},
		{"rust not a table", "manifest-version = \"2\"\ndate = \"2026-09-03\"\n" +
			"[pkg]\nrust = 1\n"},
		{"components not a list", "manifest-version = \"2\"\ndate = \"2026-09-03\"\n" +
			"[pkg.rust]\nversion = \"1.98.1 (x)\"\n[pkg.rust.target." +
			"x86_64-unknown-linux-gnu]\navailable = true\ncomponents = 1\n"},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			err := VerifyManifestClosure([]byte(test.doc), pin)
			if err == nil {
				t.Fatal("accepted a malformed manifest")
			}
			if _, isRefusal := err.(*Refusal); !isRefusal {
				t.Fatalf("not a refusal: %T %v", err, err)
			}
		})
	}
}

// TestInputDirectoryAudit is invariant 19: the input is a plain directory whose
// entries are exactly the pin's required set.
func TestInputDirectoryAudit(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		label  string
		change func(t *testing.T, directory string)
		frag   string
	}{
		{"clean", func(t *testing.T, directory string) {}, ""},
		{"extra file", func(t *testing.T, directory string) {
			write(t, filepath.Join(directory, "extra.txt"), "x")
		}, "unreviewed input: extra.txt"},
		{"extra directory", func(t *testing.T, directory string) {
			if err := os.Mkdir(filepath.Join(directory, "spare"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "unreviewed input: spare"},
		{"missing file", func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, "evidence", "amd64-cargo.asc")); err != nil {
				t.Fatal(err)
			}
		}, "missing input: evidence/amd64-cargo.asc"},
		{"symlink", func(t *testing.T, directory string) {
			write(t, filepath.Join(directory, "extra.txt"), "x")
			if err := os.Symlink(filepath.Join(directory, "extra.txt"),
				filepath.Join(directory, "link.txt")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(directory, "extra.txt")); err != nil {
				t.Fatal(err)
			}
		}, "unreviewed input: symlink link.txt"},
		{"special file", func(t *testing.T, directory string) {
			if err := syscall.Mkfifo(filepath.Join(directory, "pipe"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "unreviewed input: special file pipe"},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			directory := t.TempDir()
			if err := copyTree(committedPinDir(t), directory); err != nil {
				t.Fatal(err)
			}
			test.change(t, directory)
			err := verifyInputDirectory(directory, pin)
			if test.frag == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.frag) {
				t.Fatalf("got %v, want %q", err, test.frag)
			}
		})
	}
	t.Run("symlinked root", func(t *testing.T) {
		directory := t.TempDir()
		if err := copyTree(committedPinDir(t), directory); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(directory, link); err != nil {
			t.Fatal(err)
		}
		err := verifyInputDirectory(link, pin)
		if err == nil || !strings.Contains(err.Error(),
			"input directory is not a plain directory") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("missing root", func(t *testing.T) {
		err := verifyInputDirectory(filepath.Join(t.TempDir(), "absent"), pin)
		if err == nil || !strings.Contains(err.Error(),
			"input directory is not a plain directory") {
			t.Fatalf("got %v", err)
		}
	})
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestChecksumLines is invariant 21: one LF-terminated line, no BOM, the exact
// grammar, the pinned name and the pinned digest.
func TestChecksumLines(t *testing.T) {
	pin, err := LoadPin(committedPinDir(t), ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	digest := string(pin.Manifest.Digest)
	name := pin.Manifest.URL.Filename()
	good := fmt.Sprintf("%s  %s\n", digest, name)
	cases := []struct {
		label string
		raw   string
		frag  string
	}{
		{"good", good, ""},
		{"binary marker", fmt.Sprintf("%s *%s\n", digest, name), ""},
		{"leading BOM", "\xef\xbb\xbf" + good, "leading BOM"},
		{"invalid UTF-8", "\xff\n", "not valid UTF-8"},
		{"no trailing newline", strings.TrimSuffix(good, "\n"),
			"expected exactly one trailing-newline line"},
		{"two lines", good + good, "expected exactly one trailing-newline line"},
		{"one space", fmt.Sprintf("%s %s\n", digest, name),
			"unexpected checksum line format"},
		{"upper-case digest", fmt.Sprintf("%s  %s\n", strings.ToUpper(digest), name),
			"unexpected checksum line format"},
		{"other name", fmt.Sprintf("%s  channel-rust-1.0.0.toml\n", digest),
			"names channel-rust-1.0.0.toml, not the pinned manifest"},
		{"other digest", fmt.Sprintf("%s  %s\n", strings.Repeat("0", 64), name),
			"committed digest does not match the committed upstream checksum"},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			err := verifyManifestChecksumLine([]byte(test.raw), pin)
			if test.frag == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.frag) {
				t.Fatalf("got %v, want %q", err, test.frag)
			}
		})
	}
	t.Run("bootstrap", func(t *testing.T) {
		closure := pin.Closure(AMD64)
		good := fmt.Sprintf("%s *./rustup-init\n", closure.Bootstrap.Digest)
		if err := verifyBootstrapChecksumLine([]byte(good), AMD64,
			closure.Bootstrap.Digest); err != nil {
			t.Fatalf("refused the committed line: %v", err)
		}
		bad := fmt.Sprintf("%s *./rustup-init\n", strings.Repeat("0", 64))
		if err := verifyBootstrapChecksumLine([]byte(bad), AMD64,
			closure.Bootstrap.Digest); err == nil ||
			!strings.Contains(err.Error(), "checksum does not match the pin") {
			t.Fatalf("got %v", err)
		}
		wrongName := fmt.Sprintf("%s *rustup-init\n", closure.Bootstrap.Digest)
		if err := verifyBootstrapChecksumLine([]byte(wrongName), AMD64,
			closure.Bootstrap.Digest); err == nil ||
			!strings.Contains(err.Error(), "unexpected checksum line format") {
			t.Fatalf("got %v", err)
		}
	})
}

// TestEvidenceDigests is invariant 20: every committed file must match its pin
// digest, and the refusal names the file.
func TestEvidenceDigests(t *testing.T) {
	directory := t.TempDir()
	if err := copyTree(committedPinDir(t), directory); err != nil {
		t.Fatal(err)
	}
	pin, err := LoadPin(directory, ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(directory, "channel.toml"), "substituted")
	_, err = readEvidence(directory, ManifestPath, pin.Manifest.Digest)
	if err == nil || !strings.Contains(err.Error(),
		"channel.toml: bytes differ from the committed digest") {
		t.Fatalf("got %v", err)
	}
}

// TestVerifyInputsSignaturePrecedesSemantics is invariant 22: the manifest is
// authenticated before it is decoded, so a stale signature over malformed TOML
// names the signature and not `not valid TOML`.
func TestVerifyInputsSignaturePrecedesSemantics(t *testing.T) {
	directory := t.TempDir()
	if err := copyTree(committedPinDir(t), directory); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPin(directory, ReviewedTrust); err != nil {
		t.Fatal(err)
	}
	malformed := []byte("[renames.clippy\nto = \"clippy-preview\"\n")
	write(t, filepath.Join(directory, ManifestPath), string(malformed))
	write(t, filepath.Join(directory, ManifestChecksumPath),
		shaOf(malformed)+"  channel-rust-1.98.1.toml\n")
	document := map[string]any{}
	raw, err := os.ReadFile(filepath.Join(directory, PinFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	manifest := document["manifest"].(map[string]any)
	manifest["sha256"] = shaOf(malformed)
	manifest["checksum"].(map[string]any)["sha256"] = shaOf([]byte(
		shaOf(malformed) + "  channel-rust-1.98.1.toml\n"))
	edited, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(directory, PinFile), string(edited))
	mutant, err := LoadPin(directory, ReviewedTrust)
	if err != nil {
		t.Fatal(err)
	}
	// A verifier whose every call fails: the signature must be what refuses.
	work := t.TempDir()
	verifier := gpg.NewVerifier(gpg.Program("/bin/false"), filepath.Join(directory, KeyPath),
		work, PolicyFor(ReviewedTrust)).WithExec(
		func(ctx context.Context, argv, env []string) (gpg.Result, error) {
			joined := strings.Join(argv, " ")
			if strings.Contains(joined, "--list-keys") {
				return gpg.Result{Stdout: "pub:u:1:1:" + string(ReviewedPrimaryFingerprint) +
					":0:0:::::::\nfpr:::::::::" + string(ReviewedPrimaryFingerprint) + ":\n"}, nil
			}
			if strings.Contains(joined, "--verify") {
				return gpg.Result{Status: 1, Stderr: "stale signature\n"}, nil
			}
			return gpg.Result{}, nil
		})
	_, err = VerifyInputs(context.Background(), directory, mutant, verifier)
	if err == nil || !strings.Contains(err.Error(), "manifest signature check failed") {
		t.Fatalf("got %v, want the signature named before the grammar", err)
	}
	if strings.Contains(err.Error(), "not valid TOML") {
		t.Fatalf("the grammar was read before the signature: %v", err)
	}
}

// shaOf is the lowercase hex SHA-256 of raw.
func shaOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
