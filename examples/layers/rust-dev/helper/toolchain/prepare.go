package toolchain

// The download-and-verify phase: authenticate committed evidence, fetch exactly
// the seven pinned URLs, and assemble a private `file://` dist layout. Nothing
// executes here, and no home is touched: a failure in this phase leaves both
// homes absent and removes every scratch directory.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Transport fetches one canonical URL to one scratch destination.
type Transport interface {
	Fetch(ctx context.Context, url string, destination string, maxBytes int64) error
}

// SignatureVerifier verifies one archive against its committed detached
// signature. *gpg.Verifier is the production implementation; unit tests pass a
// stub so the download sequence is tested without a keyring.
type SignatureVerifier interface {
	Verify(ctx context.Context, data, signature, what string) error
}

// TransportFactory builds the transport once the pin (and therefore the exact
// URL closure it names) is known. The fixture build uses it to install a loopback
// rewriter that accepts only those exact URLs (Q1 of #231); the shipped build
// ignores its argument and returns the one HTTPS client.
type TransportFactory func(pinned []CanonicalURL) (Transport, error)

// PinnedURLs is the exact seven-URL closure one architecture names: the
// bootstrap and the six component archives. A fixture transport may rewrite these
// and nothing else.
func PinnedURLs(pin Pin, arch Architecture) []CanonicalURL {
	closure := pin.Closure(arch)
	urls := []CanonicalURL{closure.Bootstrap.URL}
	for _, archive := range closure.Archives {
		urls = append(urls, archive.Content.URL)
	}
	return urls
}

// VerifiedLayout is the private dist layout and the bootstrap ready to execute.
type VerifiedLayout struct {
	Root      string
	Bootstrap string
	Closure   HostClosure
	Channel   Channel
}

// Prepare verifies the committed inputs and all seven objects, preflights every
// archive, and writes the dist layout. work must already be a 0700 scratch
// directory; Prepare creates its subdirectories.
func Prepare(ctx context.Context, directory string, pin Pin, arch Architecture, work string,
	transport Transport, verifier SignatureVerifier, events *Events) (*VerifiedLayout, error) {
	verified, err := VerifyInputs(ctx, directory, pin, verifier)
	if err != nil {
		return nil, err
	}
	return prepareVerified(ctx, directory, pin, verified.Raw, arch, work, transport, verifier, events)
}

// prepareVerified is the download-and-verify sequence over an already
// authenticated pin. It is separate so a unit test can hold the ordering without
// a signed fixture manifest; production reaches it only through VerifyInputs.
func prepareVerified(ctx context.Context, directory string, pin Pin, manifestRaw []byte,
	arch Architecture, work string, transport Transport, verifier SignatureVerifier,
	events *Events) (*VerifiedLayout, error) {
	closure := pin.Closure(arch)
	downloads := filepath.Join(work, "downloads")
	dated := filepath.Join(work, "dist-root", "dist", string(pin.ReleaseDate))
	bootstrapDir := filepath.Join(work, "bootstrap")
	for _, each := range []string{downloads, dated, bootstrapDir} {
		if err := makePrivate(each); err != nil {
			return nil, err
		}
	}

	bootstrap := filepath.Join(bootstrapDir, "rustup-init")
	if err := events.Record("fetch " + string(closure.Bootstrap.URL)); err != nil {
		return nil, err
	}
	if err := transport.Fetch(ctx, string(closure.Bootstrap.URL), bootstrap, MaxBootstrapBytes); err != nil {
		return nil, adopt(err)
	}
	digest, err := hashFile(bootstrap)
	if err != nil {
		return nil, err
	}
	if digest != closure.Bootstrap.Digest {
		// The URL is the only actionable part of a withdrawn-pin failure.
		return nil, refuse("bootstrap: downloaded bytes at %s do not match the committed "+
			"digest", closure.Bootstrap.URL)
	}
	if err := events.Record("hash-ok " + closure.Bootstrap.URL.Filename()); err != nil {
		return nil, err
	}
	if err := checkELFFile(bootstrap, arch); err != nil {
		return nil, err
	}
	// Mode publication: 0755 only after the hash and the ELF identity passed.
	if err := os.Chmod(bootstrap, 0o755); err != nil {
		return nil, adopt(err)
	}

	for _, archive := range closure.Archives {
		name := archive.Content.URL.Filename()
		target := filepath.Join(downloads, name)
		if err := events.Record("fetch " + string(archive.Content.URL)); err != nil {
			return nil, err
		}
		if err := transport.Fetch(ctx, string(archive.Content.URL), target, MaxArchiveBytes); err != nil {
			return nil, adopt(err)
		}
		digest, err := hashFile(target)
		if err != nil {
			return nil, err
		}
		if digest != archive.Content.Digest {
			return nil, refuse("%s: downloaded bytes at %s do not match the signed "+
				"manifest hash", name, archive.Content.URL)
		}
		if err := events.Record("hash-ok " + name); err != nil {
			return nil, err
		}
		if err := verifier.Verify(ctx, target,
			filepath.Join(directory, string(archive.Signature.Path)),
			name+" signature"); err != nil {
			return nil, adopt(err)
		}
		if err := events.Record("signature-ok " + name); err != nil {
			return nil, err
		}
		if err := PreflightArchive(target, archive, events); err != nil {
			return nil, err
		}
		if err := copyFile(target, filepath.Join(dated, name), 0o600); err != nil {
			return nil, err
		}
	}

	// The layout uses the upstream file names: rustup requests
	// <dist-server>/dist/channel-rust-<channel>.toml and its .sha256/.asc.
	upstream := pin.Manifest.URL.Filename()
	pairs := [][2]string{
		{ManifestPath, filepath.Join(work, "dist-root", "dist", upstream)},
		{ManifestChecksumPath, filepath.Join(work, "dist-root", "dist", upstream+".sha256")},
		{ManifestSignaturePath, filepath.Join(work, "dist-root", "dist", upstream+".asc")},
	}
	for _, pair := range pairs {
		if err := copyFile(filepath.Join(directory, pair[0]), pair[1], 0o600); err != nil {
			return nil, err
		}
	}
	copied, err := os.ReadFile(filepath.Join(work, "dist-root", "dist", upstream))
	if err != nil {
		return nil, adopt(err)
	}
	if !bytes.Equal(copied, manifestRaw) {
		return nil, refuse("layout: manifest bytes were not copied verbatim")
	}
	if err := makePrivate(filepath.Join(work, NoUpdatesDirectory)); err != nil {
		return nil, err
	}
	if err := events.Record("layout-ready " + arch.Value()); err != nil {
		return nil, err
	}
	return &VerifiedLayout{Root: filepath.Join(work, "dist-root"), Bootstrap: bootstrap,
		Closure: closure, Channel: pin.Channel}, nil
}

// makePrivate creates one 0700 directory and re-applies the mode, because Mkdir
// honours the process umask.
func makePrivate(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return adopt(err)
	}
	return adopt(os.Chmod(directory, 0o700))
}

func checkELFFile(path string, arch Architecture) error {
	handle, err := os.Open(path)
	if err != nil {
		return adopt(err)
	}
	defer func() { _ = handle.Close() }()
	header := make([]byte, 20)
	count, err := io.ReadFull(handle, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return adopt(err)
	}
	machine, err := CheckELF(header[:count])
	if err != nil {
		if errors.Is(err, errNotELF) {
			return refuse("bootstrap: not an ELF binary")
		}
		if errors.Is(err, errNotELF64LE) {
			return refuse("bootstrap: expected a 64-bit little-endian ELF binary")
		}
		return adopt(err)
	}
	if machine != arch.ELFMachine() {
		return refuse("bootstrap: ELF machine is not %s", arch.BootstrapTarget())
	}
	return nil
}

func hashFile(path string) (Sha256, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", adopt(err)
	}
	defer func() { _ = handle.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, handle); err != nil {
		return "", adopt(err)
	}
	return Sha256(hex.EncodeToString(digest.Sum(nil))), nil
}

// copyFile writes atomically with the requested mode and never leaves a partial
// destination behind.
func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return adopt(err)
	}
	defer func() { _ = input.Close() }()
	partial := destination + ".partial"
	_ = os.Remove(partial)
	output, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return adopt(err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		_ = os.Remove(partial)
		return adopt(err)
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(partial)
		return adopt(err)
	}
	if err := os.Rename(partial, destination); err != nil {
		_ = os.Remove(partial)
		return adopt(err)
	}
	return nil
}
