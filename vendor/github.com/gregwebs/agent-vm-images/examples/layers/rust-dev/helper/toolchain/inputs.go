package toolchain

// Input-directory verification: the audit, the evidence digests, the checksum
// lines and the signature. Authenticity precedes semantics — gpg verifies the
// manifest before any TOML is decoded — so a stale signature over a malformed
// manifest names the signature, not the TOML.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// VerifiedManifest is a pin whose committed manifest is authenticated.
type VerifiedManifest struct {
	Pin Pin
	Raw []byte
}

// VerifyInputs audits the pin directory, checks every committed digest and
// checksum line, verifies the manifest signature and then the manifest closure.
func VerifyInputs(ctx context.Context, directory string, pin Pin,
	verifier SignatureVerifier) (VerifiedManifest, error) {
	var verified VerifiedManifest
	if err := verifyInputDirectory(directory, pin); err != nil {
		return verified, err
	}
	manifestRaw, err := readEvidence(directory, ManifestPath, pin.Manifest.Digest)
	if err != nil {
		return verified, err
	}
	checksumRaw, err := readEvidence(directory, ManifestChecksumPath,
		pin.ManifestChecksum.Digest)
	if err != nil {
		return verified, err
	}
	if _, err := readEvidence(directory, ManifestSignaturePath, pin.ManifestSignature.Digest); err != nil {
		return verified, err
	}
	if _, err := readEvidence(directory, KeyPath, pin.KeySha256); err != nil {
		return verified, err
	}
	if err := verifyManifestChecksumLine(checksumRaw, pin); err != nil {
		return verified, err
	}
	for _, closure := range pin.Closures {
		raw, err := readEvidence(directory, string(closure.BootstrapChecksum.Path),
			closure.BootstrapChecksum.Digest)
		if err != nil {
			return verified, err
		}
		if err := verifyBootstrapChecksumLine(raw, closure.Arch, closure.Bootstrap.Digest); err != nil {
			return verified, err
		}
		for _, archive := range closure.Archives {
			if _, err := readEvidence(directory, string(archive.Signature.Path),
				archive.Signature.Digest); err != nil {
				return verified, err
			}
		}
	}
	if err := verifier.Verify(ctx, filepath.Join(directory, ManifestPath),
		filepath.Join(directory, ManifestSignaturePath), "manifest signature"); err != nil {
		return verified, adopt(err)
	}
	if err := VerifyManifestClosure(manifestRaw, pin); err != nil {
		return verified, err
	}
	verified.Pin = pin
	verified.Raw = manifestRaw
	return verified, nil
}

// walkFiles lists every entry under directory, files and directories alike, as
// sorted slash-separated paths relative to it.
func walkFiles(directory string) ([]string, error) {
	var entries []string
	err := filepath.WalkDir(directory, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if current == directory {
			return nil
		}
		relative, err := filepath.Rel(directory, current)
		if err != nil {
			return err
		}
		entries = append(entries, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)
	return entries, nil
}

// verifyInputDirectory requires a plain directory whose files are exactly the
// pin's required set, whose directories are exactly their parents, and which
// holds no symlink or special file. The first refusal is the sorted first.
func verifyInputDirectory(directory string, pin Pin) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return refuse("input directory is not a plain directory: %s", directory)
	}
	required := make(map[string]bool, len(pin.RequiredFiles()))
	expectedDirs := map[string]bool{}
	for _, file := range pin.RequiredFiles() {
		required[file] = true
		if parent := path.Dir(file); parent != "." {
			expectedDirs[parent] = true
		}
	}
	entries, err := walkFiles(directory)
	if err != nil {
		return refuse("input directory is not a plain directory: %s", directory)
	}
	seenFiles := map[string]bool{}
	seenDirs := map[string]bool{}
	for _, relative := range entries {
		entryInfo, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(relative)))
		if err != nil {
			return refuse("missing input: %s", relative)
		}
		switch {
		case entryInfo.Mode()&os.ModeSymlink != 0:
			return refuse("unreviewed input: symlink %s", relative)
		case entryInfo.IsDir():
			seenDirs[relative] = true
		case entryInfo.Mode().IsRegular():
			seenFiles[relative] = true
		default:
			return refuse("unreviewed input: special file %s", relative)
		}
	}
	var extra []string
	for file := range seenFiles {
		if !required[file] {
			extra = append(extra, file)
		}
	}
	sort.Strings(extra)
	var extraDirs []string
	for dir := range seenDirs {
		if !expectedDirs[dir] {
			extraDirs = append(extraDirs, dir)
		}
	}
	sort.Strings(extraDirs)
	extra = append(extra, extraDirs...)
	if len(extra) != 0 {
		return refuse("unreviewed input: %s", extra[0])
	}
	var missing []string
	for file := range required {
		if !seenFiles[file] {
			missing = append(missing, file)
		}
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		return refuse("missing input: %s", missing[0])
	}
	for _, file := range pin.RequiredFiles() {
		info, err := os.Stat(filepath.Join(directory, filepath.FromSlash(file)))
		if err != nil || !info.Mode().IsRegular() {
			return refuse("missing input: %s", file)
		}
	}
	return nil
}

// readEvidence reads one committed file and requires the pin's digest.
func readEvidence(directory, relative string, digest Sha256) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(relative)))
	if err != nil {
		return nil, refuse("missing input: %s", relative)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != string(digest) {
		return nil, refuse("%s: bytes differ from the committed digest", relative)
	}
	return raw, nil
}

// oneChecksumLine requires exactly one LF-terminated line, with no BOM.
func oneChecksumLine(raw []byte, what string) (string, error) {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return "", refuse("%s: leading BOM", what)
	}
	if !utf8.Valid(raw) {
		return "", refuse("%s: not valid UTF-8", what)
	}
	text := string(raw)
	lines := strings.Split(text, "\n")
	if len(lines) != 2 || lines[1] != "" {
		return "", refuse("%s: expected exactly one trailing-newline line", what)
	}
	return lines[0], nil
}

// verifyManifestChecksumLine requires the committed checksum to name the pinned
// manifest with the pinned digest.
func verifyManifestChecksumLine(raw []byte, pin Pin) error {
	line, err := oneChecksumLine(raw, ManifestChecksumPath)
	if err != nil {
		return err
	}
	digest, name, ok := splitChecksumLine(line)
	if !ok || !isXyzName(name) {
		return refuse("%s: unexpected checksum line format", ManifestChecksumPath)
	}
	if name != pin.Manifest.URL.Filename() {
		return refuse("%s: names %s, not the pinned manifest", ManifestChecksumPath, name)
	}
	if digest != string(pin.Manifest.Digest) {
		return refuse("manifest: committed digest does not match the committed upstream " +
			"checksum")
	}
	return nil
}

// verifyBootstrapChecksumLine requires the committed bootstrap checksum to be
// the reviewed one-line `*./rustup-init` form with the pinned digest.
func verifyBootstrapChecksumLine(raw []byte, arch Architecture, digest Sha256) error {
	what := ExpectedBootstrapChecksumPath(arch)
	line, err := oneChecksumLine(raw, what)
	if err != nil {
		return err
	}
	sum, name, ok := splitChecksumLine(line)
	if !ok || name != "./rustup-init" {
		return refuse("%s: unexpected checksum line format", what)
	}
	if sum != string(digest) {
		return refuse("%s: checksum does not match the pin", what)
	}
	return nil
}

// splitChecksumLine matches `([0-9a-f]{64}) [ *](NAME)` in full: one space, then
// the binary-mode marker (a space or an asterisk), then the name.
func splitChecksumLine(line string) (digest, name string, ok bool) {
	if len(line) < 66 {
		return "", "", false
	}
	digest = line[:64]
	if !isLowerHex(digest, 64) {
		return "", "", false
	}
	// No separate separator rule: the name comparison is what decides, so a line
	// with the wrong marker is refused as naming something else rather than by a
	// second grammar for the same bytes.
	name = line[66:]
	if name == "" {
		return "", "", false
	}
	return digest, name, true
}

// isXyzName matches `channel-rust-X.Y.Z.toml`.
func isXyzName(name string) bool {
	const prefix = "channel-rust-"
	const suffix = ".toml"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return false
	}
	return isXyz(name[len(prefix) : len(name)-len(suffix)])
}
