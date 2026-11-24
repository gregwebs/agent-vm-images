package toolchain

// Unit tests for the staging side. The end-to-end differential against the Python
// writer is `script/test/rust-toolchain-maintenance.py`; these hold the pieces
// that can be checked without a distribution, and one that needs the committed
// pin: the document the writer emits must be the committed bytes.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// committedDataDir is the layer's committed pin directory, read by this module's
// own tests only. It is the reviewed input set the image installs.
func committedDataDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.Abs(filepath.Join("..", "..", "data", "rust-toolchain"))
	if err != nil {
		t.Fatalf("resolve the committed data directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, PinFile)); err != nil {
		t.Fatalf("committed pin: %v", err)
	}
	return directory
}

// TestPinDocumentReproducesCommittedBytes is the writer's formatting contract: the
// document it builds from the committed model is byte-identical to the committed
// pin.json (key order, two-space indent, trailing newline). A change to either
// shape fails here instead of appearing as a diff nobody intended.
func TestPinDocumentReproducesCommittedBytes(t *testing.T) {
	directory := committedDataDir(t)
	pin, err := LoadPin(directory, ReviewedTrust)
	if err != nil {
		t.Fatalf("load the committed pin: %v", err)
	}
	encoded, err := jsonBytes(encodeDocument(pin))
	if err != nil {
		t.Fatalf("encode the pin document: %v", err)
	}
	committed, err := os.ReadFile(filepath.Join(directory, PinFile))
	if err != nil {
		t.Fatalf("read the committed pin: %v", err)
	}
	if !bytes.Equal(encoded, committed) {
		first := 0
		for first < len(encoded) && first < len(committed) && encoded[first] == committed[first] {
			first++
		}
		t.Fatalf("the emitted document is not the committed bytes (first difference at byte %d)\n"+
			"emitted:   %q\ncommitted: %q", first, snippet(encoded, first),
			snippet(committed, first))
	}
}

func snippet(raw []byte, at int) string {
	start := at - 40
	if start < 0 {
		start = 0
	}
	end := at + 40
	if end > len(raw) {
		end = len(raw)
	}
	return string(raw[start:end])
}

// TestRewriteToolchainArgument pins the one Dockerfile edit and its refusal.
func TestRewriteToolchainArgument(t *testing.T) {
	directory := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	rewritten, err := rewriteToolchainArgument(write("ok", "FROM base\nARG RUST_TOOLCHAIN=1.0.0\nRUN x\n"), "1.2.3")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if string(rewritten) != "FROM base\nARG RUST_TOOLCHAIN=1.2.3\nRUN x\n" {
		t.Fatalf("rewritten bytes: %q", rewritten)
	}
	for name, body := range map[string]string{
		"absent":   "FROM base\n",
		"twice":    "ARG RUST_TOOLCHAIN=1.0.0\nARG RUST_TOOLCHAIN=1.0.0\n",
		"prefixed": "ARG RUST_TOOLCHAIN_EXTRA=1.0.0\n",
	} {
		if _, err := rewriteToolchainArgument(write(name, body), "1.2.3"); err == nil ||
			!strings.Contains(err.Error(), "expected exactly one ARG RUST_TOOLCHAIN line") {
			t.Fatalf("%s: want the exact-one-line refusal, got %v", name, err)
		}
	}
}

// TestBootstrapReviewRenewal proves the two halves of the exception rule: an
// unchanged bootstrap keeps the committed review, and a changed one is refused
// until a human renews it.
func TestBootstrapReviewRenewal(t *testing.T) {
	pin, err := LoadPin(committedDataDir(t), ReviewedTrust)
	if err != nil {
		t.Fatalf("load the committed pin: %v", err)
	}
	unchanged := map[Architecture]Sha256{}
	for _, arch := range Architectures() {
		unchanged[arch] = pin.Closure(arch).Bootstrap.Digest
	}
	if !exceptionUnchanged(pin.BootstrapException, pin.RustupVersion, unchanged) {
		t.Fatal("the committed review does not describe the committed bootstrap")
	}
	changed := map[Architecture]Sha256{}
	for arch, digest := range unchanged {
		changed[arch] = digest
	}
	changed[ARM64] = Sha256("0")
	if exceptionUnchanged(pin.BootstrapException, pin.RustupVersion, changed) {
		t.Fatal("a changed bootstrap digest kept the committed review")
	}
	request := StageRequest{Channel: pin.Channel, Rustup: pin.RustupVersion}
	previous := pin
	if _, err := stageException(request, &previous, changed); err == nil ||
		!strings.Contains(err.Error(), "hash-only bootstrap exception requires renewed review") ||
		!strings.Contains(err.Error(), "-> new") {
		t.Fatalf("want the renewal refusal with the old and new identity, got %v", err)
	}
	reviewedOn := IsoDate("2026-02-03")
	request.ReviewedOn, request.Reviewer = &reviewedOn, "second-reviewer"
	renewed, err := stageException(request, &previous, changed)
	if err != nil {
		t.Fatalf("a renewed review was refused: %v", err)
	}
	if renewed.ReviewedOn != reviewedOn || renewed.Reviewer != "second-reviewer" {
		t.Fatalf("renewed review: %+v", renewed)
	}
	if renewed.Scope[ARM64.Platform()].Digest != changed[ARM64] {
		t.Fatal("the renewed review does not bind the new digest")
	}
}

// TestStageRefusesBeforeAnyFetch proves the preconditions hold before the writer
// touches the network: an existing output path, a non-directory --current, and a
// missing key for a first adoption are all refusals, with no transport call.
func TestStageRefusesBeforeAnyFetch(t *testing.T) {
	transport := &countingTransport{}
	dependencies := StageDependencies{Transport: transport, Trust: ReviewedTrust,
		Verifier: VerifierFor("", ReviewedTrust)}
	existing := t.TempDir()
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	for label, request := range map[string]StageRequest{
		"existing out": {Channel: "1.2.3", Rustup: RequiredRustupVersion, Current: t.TempDir(),
			Out: existing, KeyFile: file},
		"current is a file": {Channel: "1.2.3", Rustup: RequiredRustupVersion, Current: file,
			Out: filepath.Join(t.TempDir(), "new"), KeyFile: file},
		"first adoption without a key": {Channel: "1.2.3", Rustup: RequiredRustupVersion,
			Current: filepath.Join(t.TempDir(), "absent"),
			Out:     filepath.Join(t.TempDir(), "new")},
	} {
		if _, err := Stage(context.Background(), request, dependencies); err == nil {
			t.Fatalf("%s: expected a refusal", label)
		}
	}
	if transport.calls != 0 {
		t.Fatalf("a refused run fetched %d URLs", transport.calls)
	}
}

// acceptingVerifier accepts every signature: these cases are about the rules that
// run *before* an archive is fetched, and a real keyring would only add gpg.
type acceptingVerifier struct{}

func (acceptingVerifier) Verify(context.Context, string, string, string) error { return nil }

// mapTransport serves exactly the URLs a case names, so a stager refusal can be
// driven without a distribution on disk.
type mapTransport map[string][]byte

func (m mapTransport) Fetch(_ context.Context, url, destination string, maxBytes int64) error {
	raw, ok := m[url]
	if !ok {
		return errors.New("fetch " + url + ": refused endpoint")
	}
	if int64(len(raw)) > maxBytes {
		return errors.New("fetch " + url + ": response exceeds its bound")
	}
	return os.WriteFile(destination, raw, 0o600)
}

// TestStageRefusesAManifestItCannotAdopt holds the three manifest rules the
// stager applies before it downloads a single component: the upstream version must
// be the channel asked for, the manifest must be the reviewed format, and the
// published .sha256 line must name the bytes that arrived.
func TestStageRefusesAManifestItCannotAdopt(t *testing.T) {
	const channel = "9.9.9"
	const manifestURL = "https://static.rust-lang.org/dist/channel-rust-" + channel + ".toml"
	good := "manifest-version = \"2\"\ndate = \"2026-01-02\"\n" +
		"[pkg.rust]\nversion = \"" + channel + " (000000000 2026-01-01)\"\n"
	digest := func(text string) string {
		return string(shaOfBytes([]byte(text)))
	}
	for _, testCase := range []struct {
		name     string
		manifest string
		checksum string
		want     string
	}{
		{name: "another channel", want: "upstream manifest describes",
			manifest: strings.Replace(good, `version = "9.9.9`, `version = "9.9.8`, 1)},
		{name: "unreviewed manifest format", want: "is not manifest-version 2",
			manifest: strings.Replace(good, `manifest-version = "2"`, `manifest-version = "3"`, 1)},
		{name: "checksum line for other bytes", want: "does not match its published .sha256 line",
			manifest: good, checksum: strings.Repeat("0", 64) + "  channel-rust-9.9.9.toml\n"},
	} {
		checksum := testCase.checksum
		if checksum == "" {
			checksum = digest(testCase.manifest) + "  channel-rust-9.9.9.toml\n"
		}
		transport := mapTransport{
			manifestURL:             []byte(testCase.manifest),
			manifestURL + ".sha256": []byte(checksum),
			manifestURL + ".asc":    []byte("a signature"),
		}
		key := filepath.Join(t.TempDir(), "key.asc")
		if err := os.WriteFile(key, []byte("a key"), 0o644); err != nil {
			t.Fatalf("write the key: %v", err)
		}
		dockerfile := filepath.Join(t.TempDir(), "Dockerfile")
		if err := os.WriteFile(dockerfile, []byte("ARG RUST_TOOLCHAIN=1.0.0\n"), 0o644); err != nil {
			t.Fatalf("write the Dockerfile: %v", err)
		}
		_, err := Stage(context.Background(), StageRequest{
			Channel: channel, Rustup: RequiredRustupVersion, KeyFile: key,
			Current: filepath.Join(t.TempDir(), "absent"),
			Out:     filepath.Join(t.TempDir(), "candidate"), Dockerfile: dockerfile,
		}, StageDependencies{Transport: transport, Trust: ReviewedTrust,
			Verifier: func(string, string) SignatureVerifier { return acceptingVerifier{} }})
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: want %q, got %v", testCase.name, testCase.want, err)
		}
	}
}

// TestBootstrapELFRefusals pins the three lines the one ELF rule emits, under both
// callers' labels: the installer's `bootstrap` and the writer's `rustup-init`.
func TestBootstrapELFRefusals(t *testing.T) {
	machine := func(value uint16) []byte {
		header := []byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		header[18] = byte(value)
		header[19] = byte(value >> 8)
		return header
	}
	directory := t.TempDir()
	write := func(name string, raw []byte) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, raw, 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	notELF := write("rustup-init.txt", []byte("not an elf at all"))
	foreign := write("foreign", machine(183))
	installer := elfLabels{Prefix: "bootstrap"}
	writer := elfLabels{Prefix: "rustup-init-amd64", Machine: "bootstrap amd64"}
	for _, each := range []struct {
		path   string
		labels elfLabels
		want   string
	}{
		{notELF, installer, "bootstrap: not an ELF binary"},
		{notELF, writer, "rustup-init-amd64: not an ELF binary"},
		{foreign, installer, "bootstrap: ELF machine is not x86_64-unknown-linux-gnu"},
		{foreign, writer, "bootstrap amd64: ELF machine is not x86_64-unknown-linux-gnu"},
	} {
		if err := checkELFFile(each.path, AMD64, each.labels); err == nil ||
			err.Error() != each.want {
			t.Fatalf("%s with %v: want %q, got %v", filepath.Base(each.path), each.labels,
				each.want, err)
		}
	}
	if err := checkELFFile(write("native", machine(62)), AMD64, installer); err != nil {
		t.Fatalf("the amd64 machine was refused: %v", err)
	}
}

type countingTransport struct{ calls int }

func (c *countingTransport) Fetch(context.Context, string, string, int64) error {
	c.calls++
	return nil
}

// TestStageRefusesGitHubActions keeps the maintainer-host rule where the writer
// is, not only in the shell wrapper that normally calls it.
func TestStageRefusesGitHubActions(t *testing.T) {
	_, err := Stage(context.Background(), StageRequest{Channel: "1.2.3",
		Rustup: RequiredRustupVersion}, StageDependencies{GitHubActions: true})
	if err == nil || !strings.Contains(err.Error(), "refused in GitHub Actions") {
		t.Fatalf("want the GitHub Actions refusal, got %v", err)
	}
	if _, err := Stage(context.Background(), StageRequest{Channel: "1.2.3",
		Rustup: "9.9.9"}, StageDependencies{}); err == nil ||
		!strings.Contains(err.Error(), "is not an audited installer") {
		t.Fatalf("want the audited-installer refusal, got %v", err)
	}
}
