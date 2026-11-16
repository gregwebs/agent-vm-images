package claudewriter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gregwebs/agent-vm-images/internal/claudepin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// fetched is the three committed evidence candidates, as upstream served them.
type fetched struct {
	manifest     []byte
	signature    []byte
	publishedKey []byte
}

// downloaded is one maintained executable, as this run received it.
type downloaded struct {
	token  string
	sha256 string
	size   int64
}

// stagedClaude is the publication: the bytes, the delta a reviewer reads, and
// whether every byte already matches the committed one.
type stagedClaude struct {
	files     []publication.Staged
	delta     string
	unchanged bool
}

// publicationFile is one destination of the transaction, relative to the root.
type publicationFile struct {
	path string
	raw  []byte
}

// fetchUpstream reads the three evidence files from their computed URLs.
func (c *Client) fetchUpstream(ctx context.Context, version, work string) (fetched, error) {
	targets := []struct {
		url  string
		name string
	}{
		{claudepin.ManifestURL(version), "manifest.json"},
		{claudepin.SignatureURL(version), "manifest.json.sig"},
		{claudepin.KeyURL, "claude-code.asc"},
	}
	raw := map[string][]byte{}
	for _, target := range targets {
		bytes, err := c.Fetch.Get(ctx, target.url, claudepin.MaxEvidenceBytes)
		if err != nil {
			return fetched{}, withPath(target.url, err)
		}
		if int64(len(bytes)) > claudepin.MaxEvidenceBytes {
			return fetched{}, refusal("%s is larger than the %d byte ceiling",
				target.url, claudepin.MaxEvidenceBytes)
		}
		raw[target.name] = bytes
	}
	return fetched{
		manifest:     raw["manifest.json"],
		signature:    raw["manifest.json.sig"],
		publishedKey: raw["claude-code.asc"],
	}, nil
}

// downloadExecutables receives both maintained executables, each accepted only as
// the manifest's bytes.
func (c *Client) downloadExecutables(ctx context.Context, version, work string,
	manifest claudepin.Manifest) (map[string]downloaded, error) {
	received := map[string]downloaded{}
	for _, token := range claudepin.TokenList() {
		url := claudepin.ExecutableURL(version, token)
		path := filepath.Join(work, "claude-"+token)
		bytes, err := c.Fetch.Get(ctx, url, claudepin.MaxExecutableBytes)
		if err != nil {
			return nil, withPath(url, err)
		}
		if int64(len(bytes)) > claudepin.MaxExecutableBytes {
			return nil, refusal("%s is larger than the %d byte ceiling",
				url, claudepin.MaxExecutableBytes)
		}
		// Written once, then hashed and preflighted from the file: the bytes that are
		// checked are the bytes on disk, and a 340 MB slice is not kept beside them.
		if err := os.WriteFile(path, bytes, 0o600); err != nil {
			return nil, refusal("cannot write %s: %s", path, err)
		}
		digest, size, err := claudepin.FileDigest(path, claudepin.MaxExecutableBytes,
			"the downloaded executable")
		if err != nil {
			return nil, err
		}
		signed, ok := manifest.Executables[token]
		if !ok {
			return nil, refusal("the signed manifest has no %s platform", token)
		}
		if digest != signed.SHA256 || size != signed.Size {
			return nil, refusal("the downloaded %s executable is %s / %d bytes, but the "+
				"signed manifest records %s / %d bytes", token, digest, size,
				signed.SHA256, signed.Size)
		}
		if err := claudepin.CheckELF(path, token); err != nil {
			return nil, err
		}
		received[token] = downloaded{token: token, sha256: digest, size: size}
	}
	return received, nil
}

// committedPin is the validated committed closure, or an empty result on a clean
// first adoption.
//
// A half-populated closure is refused rather than repaired: this writer publishes a
// reviewed selection, it does not make a broken one look better. Only the closure's
// own files count -- the directory also holds the hooks and the vendor tree.
func (c *Client) committedPin() (claudepin.VerifiedPin, bool, error) {
	directory := c.pinDirectory()
	if _, err := os.Stat(filepath.Join(directory, "pin.json")); err != nil {
		if !isNotExist(err) {
			return claudepin.VerifiedPin{}, false, err
		}
		evidence := filepath.Join(directory, "evidence")
		if entries, err := os.ReadDir(evidence); err == nil {
			leftovers := []string{}
			for _, entry := range entries {
				leftovers = append(leftovers, entry.Name())
			}
			return claudepin.VerifiedPin{}, false,
				refusal("%s holds %v with no pin.json; refusing a partial old state",
					evidence, leftovers)
		}
		return claudepin.VerifiedPin{}, false, nil
	}
	pin, err := claudepin.Load(directory)
	if err != nil {
		return claudepin.VerifiedPin{}, false, err
	}
	verified, err := pin.Verify(pin.Version)
	if err != nil {
		return claudepin.VerifiedPin{}, false, err
	}
	return verified, true, nil
}

// committedKey is the trust anchor: committed by hand and never written here.
func (c *Client) committedKey() ([]byte, error) {
	path := filepath.Join(c.pinDirectory(), claudepin.KeyPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, refusal("%s is missing: the release key is committed by hand "+
			"before the first adoption", path)
	}
	home, err := claudepin.NewGPGHome(c.Warn)
	if err != nil {
		return nil, err
	}
	primaries, keyErr := home.KeyPrimaries(path)
	if closeErr := home.Close(keyErr); closeErr != nil && keyErr == nil {
		return nil, closeErr
	}
	if keyErr != nil {
		return nil, keyErr
	}
	if len(primaries) != 1 || primaries[0] != c.signer() {
		return nil, refusal("the committed release key must hold exactly the reviewed "+
			"primary %s, found %v", c.signer(), primaries)
	}
	return raw, nil
}

// transition is the delta's `old -> new`, or the one value when nothing changed.
func transition(old, new string) string {
	if old == new {
		return new
	}
	return old + " -> " + new
}

// payloadDelta is what the review is being asked to accept, field by field, before
// it lands.
func payloadDelta(committed *claudepin.VerifiedPin, proposed claudepin.VerifiedPin) string {
	new := proposed.Pin
	old := claudepin.Pin{}
	have := committed != nil
	if have {
		old = committed.Pin
	}
	none := "(none)"
	version := none
	if have {
		version = old.Version
	}
	lines := []string{fmt.Sprintf("claude payload delta: %s -> %s", version, new.Version)}
	lines = append(lines, fmt.Sprintf("  manifest     sha256 %s",
		transition(shaOr(none, have, old.Manifest.SHA256), new.Manifest.SHA256)))
	lines = append(lines, fmt.Sprintf("               commit %s   buildDate %s",
		transition(pick(none, have, committed, func(v claudepin.VerifiedPin) string {
			return v.Manifest.Commit
		}), proposed.Manifest.Commit),
		transition(pick(none, have, committed, func(v claudepin.VerifiedPin) string {
			return v.Manifest.BuildDate
		}), proposed.Manifest.BuildDate)))
	lines = append(lines, fmt.Sprintf("  signature    primary %s   created %s",
		transition(pick(none, have, committed, func(v claudepin.VerifiedPin) string {
			return v.Signature.PrimaryFingerprint
		}), proposed.Signature.PrimaryFingerprint),
		transition(pick(none, have, committed, func(v claudepin.VerifiedPin) string {
			return v.Signature.Created
		}), proposed.Signature.Created)))
	for _, platform := range claudepin.PlatformList() {
		token := claudepin.Platforms[platform]
		left := none
		if have {
			before := old.Executables[token]
			left = fmt.Sprintf("%s %d B", before.SHA256, before.Size)
		}
		after := new.Executables[token]
		lines = append(lines, fmt.Sprintf("  %-12s %s -> %s %d B", platform, left,
			after.SHA256, after.Size))
	}
	return stringsJoinLines(lines)
}

func shaOr(fallback string, have bool, value string) string {
	if !have {
		return fallback
	}
	return value
}

func pick(fallback string, have bool, committed *claudepin.VerifiedPin,
	field func(claudepin.VerifiedPin) string) string {
	if !have {
		return fallback
	}
	return field(*committed)
}

func stringsJoinLines(lines []string) string {
	out := ""
	for _, line := range lines {
		out += line + "\n"
	}
	return out
}
