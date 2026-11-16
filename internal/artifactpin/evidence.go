package artifactpin

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
)

// Sum256 is the lower-case SHA-256 of raw.
func Sum256(raw []byte) Digest {
	sum := sha256.Sum256(raw)
	return Digest(hex.EncodeToString(sum[:]))
}

// EvidencePath is a committed evidence file, refused if it is not a plain file
// inside the pin's directory: a symlink could redirect the hash to other bytes.
func EvidencePath(pin Pin, relative RelPath) (string, error) {
	path := filepath.Join(pin.Directory, relative.String())
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", refuse("%s: missing or symlinked evidence file %s", pin.Name, relative)
	}
	return path, nil
}

// CheckEvidence reads one committed evidence file and requires the digest the
// pin records.
func CheckEvidence(pin Pin, relative RelPath, digest Digest) (string, error) {
	path, err := EvidencePath(pin, relative)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if actual := Sum256(raw); actual != digest {
		return "", refuse("%s: %s is %s, the pin says %s", pin.Name, relative, actual, digest)
	}
	return path, nil
}

var sha256SumLine = regexp.MustCompile(`^([0-9a-f]{64}) [ *]([^/\n]+)$`)

// SignedDigest is the digest a signed checksums manifest lists for one file.
//
// Both the maintenance tool and the installer refuse on a missing line: an
// archive the reviewed manifest does not name has no reviewed identity.
func SignedDigest(manifest []byte, filename string) (Digest, error) {
	for _, line := range splitLines(string(manifest)) {
		match := sha256SumLine.FindStringSubmatch(line)
		if match != nil && match[2] == filename {
			return Digest(match[1]), nil
		}
	}
	return "", refuse("the signed manifest carries no line for %s", filename)
}

func splitLines(text string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(text); index++ {
		if text[index] == '\n' {
			line := text[start:index]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, trimSpace(line))
			start = index + 1
		}
	}
	if start < len(text) {
		lines = append(lines, trimSpace(text[start:]))
	}
	return lines
}

// trimSpace trims the ASCII whitespace a checksums line may carry. It avoids the
// Unicode set: a manifest line ends at its file name.
func trimSpace(text string) string {
	for len(text) > 0 && isSpace(text[0]) {
		text = text[1:]
	}
	for len(text) > 0 && isSpace(text[len(text)-1]) {
		text = text[:len(text)-1]
	}
	return text
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\f' || b == '\v' }

// Manifest is one Google updater manifest: exactly the three fields the
// corroboration acts on. The url is only required to be https because it is never
// a download source.
type Manifest struct {
	Version string
	URL     string
	SHA512  SHA512
}

// ParseManifest reads one Google updater manifest.
func ParseManifest(raw []byte, what string) (Manifest, error) {
	document, err := strictJSON(raw, what)
	if err != nil {
		return Manifest{}, err
	}
	fields, err := fields(document, []string{"version", "url", "sha512"}, what)
	if err != nil {
		return Manifest{}, err
	}
	version, err := requireText(fields["version"], what+" version")
	if err != nil {
		return Manifest{}, err
	}
	sha512Text, err := requireText(fields["sha512"], what+" sha512")
	if err != nil {
		return Manifest{}, err
	}
	sha512, err := parseSHA512(sha512Text, what+" sha512")
	if err != nil {
		return Manifest{}, err
	}
	url, err := requireText(fields["url"], what+" url")
	if err != nil {
		return Manifest{}, err
	}
	if url, err = httpsURL(url, what+" url"); err != nil {
		return Manifest{}, err
	}
	return Manifest{Version: version, URL: url, SHA512: sha512}, nil
}

// ReleaseAssetDigests reads a GitHub release record as an asset name -> digest
// map, refusing an ambiguous release. The build-time corroboration check and the
// pin-time writer share it so the two read the same record the same way.
func ReleaseAssetDigests(releaseRaw []byte, name Name, version Version) (map[string]string, error) {
	what := name.String() + ": " + AgyReleaseEvidence.String()
	document, err := strictJSON(releaseRaw, what)
	if err != nil {
		return nil, err
	}
	release, ok := document.(map[string]object)
	if !ok {
		return nil, refuse("%s: expected the GitHub release object", what)
	}
	if tag, _ := release["tag_name"].(string); tag != version.String() {
		return nil, refuse("%s: the release is %q, the pin says %q", what, release["tag_name"], version.String())
	}
	if draft, ok := release["draft"].(bool); !ok || draft {
		return nil, refuse("%s: the release is not a published release", what)
	}
	assets, ok := release["assets"].([]object)
	if !ok {
		return nil, refuse("%s: expected an assets list", what)
	}
	digests := make(map[string]string, len(assets))
	for _, item := range assets {
		asset, ok := item.(map[string]object)
		if !ok {
			return nil, refuse("%s: expected an asset object", what)
		}
		assetName, nameOK := asset["name"].(string)
		digest, digestOK := asset["digest"].(string)
		if !nameOK || !digestOK {
			return nil, refuse("%s: an asset carries no name or digest", what)
		}
		if _, duplicate := digests[assetName]; duplicate {
			return nil, refuse("%s: the release names %q twice", what, assetName)
		}
		digests[assetName] = digest
	}
	return digests, nil
}

// CheckCorroboration requires the committed corroboration files to still agree
// with the pin.
//
// agy publishes no signature, so two records from its one publisher — the GitHub
// release asset's digest and Google's manifest sha512 — were compared at pin time
// and committed. They corroborate the bytes across two *channels*; neither is an
// independent identity, and neither is an authentication. Offline, this re-checks
// that each committed file hashes to the pin *and* that the files still say what
// the pin says: a hand-edited evidence file, or a pin asking for a different
// release than its evidence describes, is a refusal before any fetch.
func CheckCorroboration(pin Pin) error {
	authentication := pin.Authentication.HashOnly
	if authentication == nil {
		return refuse("%s: only a hash-only pin carries corroboration evidence", pin.Name)
	}
	releasePath, err := CheckEvidence(pin, authentication.Release.Path, authentication.Release.Digest)
	if err != nil {
		return err
	}
	releaseRaw, err := os.ReadFile(releasePath)
	if err != nil {
		return err
	}
	digests, err := ReleaseAssetDigests(releaseRaw, pin.Name, pin.Version)
	if err != nil {
		return err
	}
	for _, platform := range Platforms {
		archive, err := pin.Archive(platform)
		if err != nil {
			return err
		}
		filename := ExpectedFilename(pin.Name, pin.Version, platform)
		found, ok := digests[filename]
		if !ok {
			return refuse("%s: GitHub's committed release carries no %s for %s",
				pin.Name, filename, platform)
		}
		if found != "sha256:"+string(archive.Digest) {
			return refuse("%s: GitHub records %s for %s, the pin says sha256:%s",
				pin.Name, found, filename, archive.Digest)
		}
	}
	for _, platform := range Platforms {
		evidence := authentication.Manifests[platform]
		path, err := CheckEvidence(pin, evidence.Path, evidence.Digest)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		what := pin.Name.String() + ": " + evidence.Path.String()
		manifest, err := ParseManifest(raw, what)
		if err != nil {
			return err
		}
		if manifest.Version != pin.Version.String() {
			return refuse("%s: the manifest names %s, the pin says %s",
				what, manifest.Version, pin.Version)
		}
		archive, err := pin.Archive(platform)
		if err != nil {
			return err
		}
		if manifest.SHA512 != archive.SHA512 {
			return refuse("%s: the manifest sha512 is %s, the pin says %s",
				what, manifest.SHA512, archive.SHA512)
		}
	}
	return nil
}
