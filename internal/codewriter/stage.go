package codewriter

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
)

// proposalFile is one destination's proposed bytes, relative to the repository root,
// in the published order.
type proposalFile struct {
	destination string
	raw         []byte
}

// proposal is the publication: the six files, the scratch archives the preflight
// reads, and the release they belong to.
type proposal struct {
	files    []proposalFile
	archives map[string]string
	release  string
}

// stageNames is the fixed stage layout: the pin directory's shape plus one
// Dockerfile, in the order the files are written and published.
func stageNames() []string {
	names := []string{pinName, codexpin.MetadataPath, codexpin.ChecksumsPath}
	for _, platform := range codexpin.Platforms {
		names = append(names, codexpin.BundlePath(codexpin.Targets[platform]))
	}
	return append(names, dockerfileName)
}

// stage resolves, authenticates, preflights and renders one release. It writes only in
// work, and its result is the publication proposal.
func (c *Client) stage(ctx context.Context, release, work string, verify BlobVerifier) (proposal, error) {
	version := codexpin.ReleaseVersion(release)
	downloads := filepath.Join(work, "downloads")
	if err := os.MkdirAll(downloads, 0o700); err != nil {
		return proposal{}, refusal("cannot create %s: %s", downloads, err)
	}
	committed, haveCommitted, err := c.committedPin()
	if err != nil {
		return proposal{}, err
	}
	repeat := haveCommitted && committed.Release == release

	// 1. Every upstream byte, bounded and in private scratch. Nothing below this
	//    point re-fetches: a failed step is a failed proposal, not a retry.
	received, err := c.fetchUpstream(ctx, release, downloads)
	if err != nil {
		return proposal{}, err
	}
	metadata, err := received.metadata.read(codexpin.MaxMetadataBytes,
		"the downloaded release metadata")
	if err != nil {
		return proposal{}, err
	}
	checksums, err := received.checksums.read(codexpin.MaxChecksumsBytes,
		"the downloaded checksums")
	if err != nil {
		return proposal{}, err
	}
	expected := received.expectations()
	if err := requireMetadataCovers(metadata, expected); err != nil {
		return proposal{}, err
	}
	if err := requireChecksumsCover(checksums, received.payloads()); err != nil {
		return proposal{}, err
	}

	// 2. The exact signer policy, on both payloads, before anything is rendered.
	for _, platform := range codexpin.Platforms {
		payloads := received.platforms[platform]
		if err := verify(payloads.archive.path, payloads.bundle.path,
			codexpin.Identity(release), codexpin.Issuer,
			filepath.Join(work, "verify", platformDirectory(platform))); err != nil {
			return proposal{}, err
		}
	}

	// 3. A same-release repeat keeps the committed raw metadata and the original
	//    verification date when every identity agrees, so the proposal is
	//    byte-identical; any changed identity is refused above, never re-pinned.
	verifiedOn := c.today()
	if repeat {
		committedMetadata, err := boundedFile(pinDirectory(c.Env.Root),
			codexpin.MetadataPath, codexpin.MaxMetadataBytes,
			"the committed release metadata")
		if err != nil {
			return proposal{}, err
		}
		proposed := sameRelease{platforms: received.identities(),
			checksums: received.checksums.digest, metadata: metadata}
		if err := requireSameRelease(committed, committedMetadata, proposed); err != nil {
			return proposal{}, err
		}
		metadata = committedMetadata
		if checksums, err = boundedFile(pinDirectory(c.Env.Root), codexpin.ChecksumsPath,
			codexpin.MaxChecksumsBytes, "the committed checksums"); err != nil {
			return proposal{}, err
		}
		verifiedOn = committed.Authentication.VerifiedOn
	}

	// 4. The proposal bytes, with the ARG rewritten and the label required.
	dockerfilePath := filepath.Join(c.Env.Root, Dockerfile)
	dockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return proposal{}, refusal("%s: %s", dockerfilePath, err)
	}
	renderedDockerfile, err := RenderArgument(string(dockerfile), release)
	if err != nil {
		return proposal{}, err
	}
	pin, err := Render(release, version, metadata, checksums, received.identities(), verifiedOn)
	if err != nil {
		return proposal{}, err
	}
	files := []proposalFile{
		{destination: PinFile, raw: pin},
		{destination: PinDirectory + "/" + codexpin.MetadataPath, raw: metadata},
		{destination: PinDirectory + "/" + codexpin.ChecksumsPath, raw: checksums},
	}
	archives := make(map[string]string, len(codexpin.Platforms))
	for _, platform := range codexpin.Platforms {
		target := codexpin.Targets[platform]
		payloads := received.platforms[platform]
		bundle, err := payloads.bundle.read(codexpin.MaxBundleBytes,
			"the downloaded "+codexpin.BundleName(target))
		if err != nil {
			return proposal{}, err
		}
		files = append(files, proposalFile{
			destination: PinDirectory + "/" + codexpin.BundlePath(target), raw: bundle})
		archives[platform] = payloads.archive.path
	}
	files = append(files, proposalFile{destination: Dockerfile, raw: []byte(renderedDockerfile)})
	proposed := proposal{files: files, archives: archives, release: release}

	// 5. Validate the proposal through the production loader, then preflight both
	//    real archives against it. Publishing waits for both.
	if err := c.validateProposal(proposed, work); err != nil {
		return proposal{}, err
	}
	return proposed, nil
}

// validateProposal runs the production reader over a private copy of exactly the bytes
// that are about to be published, then the production archive checks on both payloads.
func (c *Client) validateProposal(proposed proposal, work string) error {
	directory := filepath.Join(work, "proposal")
	if err := writeStage(directory, proposed.files); err != nil {
		return err
	}
	pin, err := codexpin.Load(directory)
	if err != nil {
		return err
	}
	for _, platform := range codexpin.Platforms {
		selected, err := pin.Selection(proposed.release, codexpin.Targets[platform])
		if err != nil {
			return err
		}
		if err := codexpin.Preflight(pin, selected, proposed.archives[platform]); err != nil {
			return err
		}
	}
	return nil
}

// loadProposal validates staged bytes through the production loader over a private
// copy, so the bytes the transaction publishes are the bytes the loader accepted.
func (c *Client) loadProposal(files []stagedFile) (codexpin.Pin, error) {
	directory, err := os.MkdirTemp(c.Env.TempRoot, "pin-codex.verify.")
	if err != nil {
		return codexpin.Pin{}, refusal("cannot create a verification directory (%s)", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()
	for _, file := range files {
		path := filepath.Join(directory, filepath.FromSlash(file.name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return codexpin.Pin{}, refusal("cannot create %s: %s", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, file.raw, 0o600); err != nil {
			return codexpin.Pin{}, refusal("cannot write %s: %s", path, err)
		}
	}
	return codexpin.Load(directory)
}

// pinDirectory is the committed closure's directory inside the repository.
func pinDirectory(root string) string { return filepath.Join(root, PinDirectory) }

// platformDirectory is the private scratch directory one platform's verification runs
// in. The platform name is the maintainer's own spelling, flattened into one path
// component.
func platformDirectory(platform string) string {
	return strings.ReplaceAll(platform, "/", "_")
}
