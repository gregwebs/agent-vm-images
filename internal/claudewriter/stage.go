package claudewriter

import (
	"context"
	"os"
	"path/filepath"

	"github.com/gregwebs/agent-vm-images/internal/claudepin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// stage resolves, verifies, preflights and renders one release. It writes only in
// `work`, and its result is the publication proposal.
func (c *Client) stage(ctx context.Context, version, work string) (stagedClaude, error) {
	// 1. The committed key is the trust anchor and is never written by this tool.
	key, err := c.committedKey()
	if err != nil {
		return stagedClaude{}, err
	}

	// 2. The evidence, from the computed URLs only.
	upstream, err := c.fetchUpstream(ctx, version, work)
	if err != nil {
		return stagedClaude{}, err
	}
	if !bytesEqual(upstream.publishedKey, key) {
		return stagedClaude{}, refusal("the published release key changed; review it and "+
			"replace %s by hand", claudepin.KeyPath)
	}

	// 3. The signature over the manifest, under the reviewed signer, and the signed
	//    manifest's own selection.
	// The signature is checked here as well as when the proposal is validated: the
	// bytes must be proven before either executable is downloaded, not after.
	if _, err := claudepin.VerifySignature(key, c.signer(),
		upstream.manifest, upstream.signature, c.Warn); err != nil {
		return stagedClaude{}, err
	}
	manifest, err := claudepin.ParseManifest(upstream.manifest)
	if err != nil {
		return stagedClaude{}, err
	}
	if manifest.Version != version {
		return stagedClaude{}, refusal("the signed manifest names %s, not the requested %s",
			manifest.Version, version)
	}

	// 4. The committed state, then the payload.
	committed, haveCommitted, err := c.committedPin()
	if err != nil {
		return stagedClaude{}, err
	}
	executables, err := c.downloadExecutables(ctx, version, work, manifest)
	if err != nil {
		return stagedClaude{}, err
	}

	// 5. A same-release upstream rewrite is a separate reviewed adoption, never a
	//    silent re-pin. An identical repeat keeps the committed `verified_on` so the
	//    run prints `unchanged` and touches nothing.
	verifiedOn := c.today()
	sameRelease := haveCommitted && committed.Pin.Version == version
	if sameRelease {
		if err := sameReleaseUnchanged(committed, upstream, executables); err != nil {
			return stagedClaude{}, err
		}
		verifiedOn = committed.Pin.VerifiedOn.Format("2006-01-02")
	}

	// 6. The proposal bytes, with the ARG rewritten and the label required.
	dockerfilePath := filepath.Join(c.Env.Root, Dockerfile)
	dockerfileRaw, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return stagedClaude{}, refusal("%s: %s", dockerfilePath, err)
	}
	dockerfile, err := RenderArgument(string(dockerfileRaw), version)
	if err != nil {
		return stagedClaude{}, err
	}
	pinRaw, err := Render(version, upstream, key, executables, verifiedOn, c.signer())
	if err != nil {
		return stagedClaude{}, err
	}
	files := []publicationFile{
		{path: PinFile, raw: pinRaw},
		{path: PinDirectory + "/" + claudepin.ManifestPath, raw: upstream.manifest},
		{path: PinDirectory + "/" + claudepin.SignaturePath, raw: upstream.signature},
		{path: Dockerfile, raw: []byte(dockerfile)},
	}

	// 7. Validate exactly the bytes that are then published: the production loader
	//    over a private copy, the signer policy, and the identities this run
	//    downloaded.
	proposalDirectory, err := writeProposal(work, files, key)
	if err != nil {
		return stagedClaude{}, refusal("cannot write the proposal: %s", err)
	}
	loaded, err := claudepin.Load(proposalDirectory)
	if err != nil {
		return stagedClaude{}, err
	}
	verified, err := loaded.Verify(version)
	if err != nil {
		return stagedClaude{}, err
	}
	if loaded.Key.Fingerprint != c.signer() {
		return stagedClaude{}, refusal("the proposal pin names the signer %s, not %s",
			loaded.Key.Fingerprint, c.signer())
	}
	for _, platform := range claudepin.PlatformList() {
		token := claudepin.Platforms[platform]
		pinned := verified.Pin.Executables[token]
		received := executables[token]
		if pinned.SHA256 != received.sha256 || pinned.Size != received.size {
			return stagedClaude{}, refusal("the proposal pin records a different identity "+
				"for %s than the bytes this run downloaded", platform)
		}
	}
	staged := stagedClaude{
		delta: payloadDelta(pointerOrNil(committed, haveCommitted), verified),
		files: publicationFiles(files),
	}
	staged.unchanged = c.matchesCommitted(files)
	return staged, nil
}

// publicationFiles converts the proposal to the transaction's own type.
func publicationFiles(files []publicationFile) []publication.Staged {
	staged := make([]publication.Staged, 0, len(files))
	for _, file := range files {
		staged = append(staged, publication.Staged{Path: file.path, Raw: file.raw})
	}
	return staged
}

// matchesCommitted is the `unchanged` case: every publication byte already equals
// the committed byte.
func (c *Client) matchesCommitted(files []publicationFile) bool {
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Join(c.Env.Root, file.path))
		if err != nil || !bytesEqual(raw, file.raw) {
			return false
		}
	}
	return true
}

// sameReleaseUnchanged refuses a same-release upstream rewrite, whatever changed.
func sameReleaseUnchanged(committed claudepin.VerifiedPin, upstream fetched,
	executables map[string]downloaded) error {
	version := committed.Pin.Version
	if raw, err := committed.Pin.EvidenceBytes(committed.Pin.Manifest.Path,
		committed.Pin.Manifest.SHA256); err != nil {
		return err
	} else if !bytesEqual(raw, upstream.manifest) {
		return refusal("the signed manifest changed for the already-pinned %s; a "+
			"same-release upstream rewrite needs a separate reviewed adoption", version)
	}
	if raw, err := committed.Pin.EvidenceBytes(committed.Pin.Signature.Path,
		committed.Pin.Signature.SHA256); err != nil {
		return err
	} else if !bytesEqual(raw, upstream.signature) {
		return refusal("the detached signature changed for the already-pinned %s; a "+
			"same-release upstream rewrite needs a separate reviewed adoption", version)
	}
	for _, platform := range claudepin.PlatformList() {
		token := claudepin.Platforms[platform]
		pinned := committed.Pin.Executables[token]
		received := executables[token]
		if pinned.SHA256 != received.sha256 || pinned.Size != received.size {
			return refusal("the %s executable changed for the already-pinned %s; a "+
				"same-release upstream rewrite needs a separate reviewed adoption",
				platform, version)
		}
	}
	return nil
}

// today is the `verified_on` stamp: the UTC date, from the reader's own definition.
func (c *Client) today() string {
	now := c.Now
	if now == nil {
		return claudepin.UTCToday().Format("2006-01-02")
	}
	return now().UTC().Format("2006-01-02")
}

func pointerOrNil(committed claudepin.VerifiedPin, present bool) *claudepin.VerifiedPin {
	if !present {
		return nil
	}
	return &committed
}

func bytesEqual(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
