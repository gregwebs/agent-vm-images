package toolchain

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syntheticPin is a pin-shaped value whose bytes are the test's own. Prepare's
// caller has already authenticated the committed inputs, so the unit tests for
// the download sequence take the authenticated pin directly and do not need a
// signed fixture manifest.
func syntheticPin(t *testing.T, archs []Architecture,
	bootstrap []byte, archives []ComponentArchive) Pin {
	t.Helper()
	pin := Pin{Channel: "1.98.1", ReleaseDate: "2026-01-01", Profile: Profile,
		RustupVersion: RequiredRustupVersion,
		Manifest:      Content{URL: CanonicalURL(DistServer + "/dist/channel-rust-1.98.1.toml")}}
	bootstrapDigest := digestOf(bootstrap)
	for _, arch := range archs {
		closure := HostClosure{Arch: arch, Host: arch.Host(), Musl: arch.Musl(),
			Bootstrap: Content{URL: CanonicalURL(ExpectedBootstrapURL(RequiredRustupVersion,
				arch)), Digest: bootstrapDigest}}
		closure.Archives = append(closure.Archives, archives...)
		pin.Closures[int(arch)] = closure
	}
	return pin
}

func digestOf(raw []byte) Sha256 {
	digest := sha256.Sum256(raw)
	return Sha256(hex.EncodeToString(digest[:]))
}

func elfbootstrap(arch Architecture) []byte {
	return append(elfHeader(arch.ELFMachine(), 2, 1), make([]byte, 64)...)
}

// rustStdArchive builds one valid rust-std component and returns its metadata plus
// its bytes.
func rustStdArchive(t *testing.T, target TargetTriple) (ComponentArchive, []byte) {
	t.Helper()
	f := newArchiveFixture("rust-std", target)
	componentRoot := f.componentRoot()
	rlib := fileHeader(componentRoot+"lib/rustlib/"+string(target)+"/lib/libstd.rlib", 0, 0o644)
	path := f.write(t, "file:lib/rustlib/"+string(target)+"/lib/libstd.rlib\n",
		[]tar.Header{rlib}, nil)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := f.archive
	archive.Content.Digest = digestOf(raw)
	return archive, raw
}

type recordingTransport struct {
	files  map[string][]byte
	calls  []string
	limits []int64
}

func (r *recordingTransport) Fetch(_ context.Context, url, destination string, maxBytes int64) error {
	r.calls = append(r.calls, url)
	r.limits = append(r.limits, maxBytes)
	raw, present := r.files[url]
	if !present {
		return errors.New("no fixture for " + url)
	}
	return os.WriteFile(destination, raw, 0o600)
}

type stubVerifier struct {
	failAt int
	calls  []string
}

func (s *stubVerifier) Verify(_ context.Context, data, _ string, what string) error {
	s.calls = append(s.calls, what)
	if s.failAt > 0 && len(s.calls) == s.failAt {
		return errors.New(what + " failed")
	}
	return nil
}

func prepareFixture(t *testing.T, archiveCount int) (Pin, string, []ComponentArchive,
	map[string][]byte, []byte) {
	t.Helper()
	arch := AMD64
	archives := make([]ComponentArchive, 0, archiveCount)
	files := map[string][]byte{}
	for index := 0; index < archiveCount; index++ {
		target := arch.Host()
		if index%2 == 1 {
			target = arch.Musl()
		}
		archive, raw := rustStdArchive(t, target)
		archives = append(archives, archive)
		files[string(archive.Content.URL)] = raw
	}
	bootstrap := elfbootstrap(arch)
	pin := syntheticPin(t, []Architecture{arch}, bootstrap, archives)
	files[string(pin.Closure(arch).Bootstrap.URL)] = bootstrap
	return pin, makeManifestDirectory(t), archives, files, bootstrap
}

func makeManifestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{ManifestPath, ManifestChecksumPath, ManifestSignaturePath} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("manifest"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

func TestPrepareOrdering(t *testing.T) {
	pin, directory, archives, files, _ := prepareFixture(t, 2)
	// The second archive is not a valid xz stream, so preflight refuses it.
	invalid := []byte("not an xz stream")
	archives[1].Content.Digest = digestOf(invalid)
	pin.Closures[int(AMD64)].Archives[1] = archives[1]
	files[string(archives[1].Content.URL)] = invalid
	transport := &recordingTransport{files: files}
	verifier := &stubVerifier{}
	work := t.TempDir()
	eventsPath := filepath.Join(t.TempDir(), "events")
	events := NewEvents(eventsPath)
	if _, err := prepareVerified(context.Background(), directory, pin, []byte("manifest"),
		AMD64, work, transport, verifier, events); err == nil {
		t.Fatal("prepare accepted an invalid archive")
	}
	raw, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("the events log is missing: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	expected := []string{
		"fetch " + string(pin.Closure(AMD64).Bootstrap.URL),
		"hash-ok " + pin.Closure(AMD64).Bootstrap.URL.Filename(),
		"fetch " + string(archives[0].Content.URL),
		"hash-ok " + archives[0].Content.URL.Filename(),
		"signature-ok " + archives[0].Content.URL.Filename(),
		"preflight-ok " + archives[0].Content.URL.Filename(),
		"fetch " + string(archives[1].Content.URL),
		"hash-ok " + archives[1].Content.URL.Filename(),
		"signature-ok " + archives[1].Content.URL.Filename(),
	}
	if strings.Join(lines, "\n") != strings.Join(expected, "\n") {
		t.Fatalf("events are\n%s\nwant\n%s", strings.Join(lines, "\n"),
			strings.Join(expected, "\n"))
	}
	if strings.Contains(string(raw), "layout-ready") {
		t.Fatal("layout-ready was recorded before every archive passed")
	}
	failed := filepath.Join(work, "dist-root", "dist", string(pin.ReleaseDate),
		archives[1].Content.URL.Filename())
	if _, err := os.Lstat(failed); err == nil {
		t.Fatal("the failed archive reached dist-root")
	}
	if len(verifier.calls) != 2 {
		t.Fatalf("the verifier saw %d archives, want 2", len(verifier.calls))
	}
}

func TestPrepareNoRetryOnHashMismatch(t *testing.T) {
	pin, directory, archives, files, _ := prepareFixture(t, 2)
	good := files[string(archives[0].Content.URL)]
	files[string(archives[0].Content.URL)] = append([]byte{}, good[:len(good)-1]...)
	transport := &recordingTransport{files: files}
	_, err := prepareVerified(context.Background(), directory, pin, []byte("manifest"), AMD64,
		t.TempDir(), transport, &stubVerifier{}, NewEvents(""))
	if err == nil || !strings.Contains(err.Error(), "do not match the signed manifest hash") {
		t.Fatalf("refusal is %v, want a hash mismatch", err)
	}
	if len(transport.calls) != 2 {
		t.Fatalf("transport made %d calls, want bootstrap + one archive: %v", len(transport.calls),
			transport.calls)
	}
	for index := 1; index < len(transport.calls); index++ {
		if transport.calls[index] == transport.calls[index-1] {
			t.Fatalf("a URL was fetched twice: %v", transport.calls)
		}
	}
}

func TestPrepareSizeCaps(t *testing.T) {
	pin, directory, _, files, _ := prepareFixture(t, 2)
	transport := &recordingTransport{files: files}
	if _, err := prepareVerified(context.Background(), directory, pin, []byte("manifest"), AMD64,
		t.TempDir(), transport, &stubVerifier{}, NewEvents("")); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	want := []int64{MaxBootstrapBytes, MaxArchiveBytes, MaxArchiveBytes}
	if len(transport.limits) != len(want) {
		t.Fatalf("limits: %v", transport.limits)
	}
	for index, limit := range want {
		if transport.limits[index] != limit {
			t.Errorf("call %d cap is %d, want %d", index, transport.limits[index], limit)
		}
	}
}

func TestPrepareBootstrapWrongELF(t *testing.T) {
	pin, directory, _, files, bootstrap := prepareFixture(t, 1)
	notELF := []byte("this is not an elf bootstrap")
	files[string(pin.Closure(AMD64).Bootstrap.URL)] = notELF
	pin.Closures[int(AMD64)].Bootstrap.Digest = digestOf(notELF)
	work := t.TempDir()
	_, err := prepareVerified(context.Background(), directory, pin, []byte("manifest"), AMD64,
		work, &recordingTransport{files: files}, &stubVerifier{}, NewEvents(""))
	if err == nil || !strings.Contains(err.Error(), "not an ELF binary") {
		t.Fatalf("refusal is %v, want an ELF refusal", err)
	}
	info, statErr := os.Stat(filepath.Join(work, "bootstrap", "rustup-init"))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the rejected bootstrap is %o, want 0600 (never published as 0755)",
			info.Mode().Perm())
	}
	_ = bootstrap
}

func TestPrepareModePublication(t *testing.T) {
	pin, directory, _, files, _ := prepareFixture(t, 1)
	work := t.TempDir()
	if _, err := prepareVerified(context.Background(), directory, pin, []byte("manifest"), AMD64,
		work, &recordingTransport{files: files}, &stubVerifier{}, NewEvents("")); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	info, err := os.Stat(filepath.Join(work, "bootstrap", "rustup-init"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("the verified bootstrap is %o, want 0755", info.Mode().Perm())
	}
	for _, directory := range []string{filepath.Join(work, "downloads"),
		filepath.Join(work, "dist-root"), filepath.Join(work, "bootstrap")} {
		info, err := os.Stat(directory)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s is %o, want 0700", directory, info.Mode().Perm())
		}
	}
}

func TestPrepareVerbatimManifest(t *testing.T) {
	pin, directory, _, files, _ := prepareFixture(t, 1)
	// The pinned manifest digest is not checked here (VerifyInputs is the caller's
	// step), so the copied bytes are compared against the authenticated raw.
	if err := os.WriteFile(filepath.Join(directory, ManifestPath), []byte("forged"),
		0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Prepare(context.Background(), directory, pin, AMD64, t.TempDir(),
		&recordingTransport{files: files}, &stubVerifier{}, NewEvents(""))
	// VerifyInputs refuses the forged manifest first; that is the authentication
	// ordering. The verbatim rule itself is exercised directly below.
	if err == nil {
		t.Fatal("a forged manifest was accepted")
	}
	work := t.TempDir()
	verifiedRaw := []byte("authenticated")
	if _, err := prepareVerified(context.Background(), directory, pin, verifiedRaw, AMD64, work,
		&recordingTransport{files: files}, &stubVerifier{}, NewEvents("")); err == nil ||
		!strings.Contains(err.Error(), "not copied verbatim") {
		t.Fatalf("refusal is %v, want the verbatim rule", err)
	}
}
