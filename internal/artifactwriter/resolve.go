package artifactwriter

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
	"github.com/ulikunitz/xz"
)

// resolve fetches, verifies and renders one release. Nothing is written outside
// the work directory: the returned files are what the transaction may publish.
func (c *Client) resolve(ctx context.Context, options Options, key, work string) ([]publication.Staged, error) {
	review := artifactpin.Review{VerifiedOn: c.Now().UTC().Format("2006-01-02")}
	review.Archives = map[artifactpin.Platform]artifactpin.Archive{}
	evidence := []publication.Staged{}
	switch options.Name {
	case artifactpin.Node:
		openpgp, manifest, bundled, err := c.resolveNode(ctx, options.Version, key, work, review.Archives)
		if err != nil {
			return nil, err
		}
		review.OpenPGP = openpgp
		review.Bundled = map[string]artifactpin.Version{"npm": bundled}
		evidence = []publication.Staged{{Path: filepath.Join("images", "artifacts", string(artifactpin.NodeKey)), Raw: manifest}}
	case artifactpin.GH:
		sigstore, bundles, err := c.resolveGH(ctx, options.Version, work, review.Archives)
		if err != nil {
			return nil, err
		}
		review.Sigstore = sigstore
		evidence = bundles
	case artifactpin.Agy:
		// agy's pin is hash-only and its writer is not ported yet (#182).
		return nil, refuse("%s: no pin writer builds this artifact", options.Name)
	}
	raw, err := artifactpin.Render(options.Name, options.Version, review)
	if err != nil {
		return nil, err
	}
	// The writer refuses to publish bytes its own loader would reject.
	if _, err := artifactpin.Parse(raw, filepath.Join(c.Env.Root, "images", "artifacts"),
		options.Name.String()); err != nil {
		return nil, err
	}
	dockerfile, err := os.ReadFile(c.Env.Dockerfile)
	if err != nil {
		return nil, refuse("%s", err)
	}
	labelled, err := artifactpin.RenderLabel(string(dockerfile), options.Name, options.Version)
	if err != nil {
		return nil, err
	}
	staged := []publication.Staged{{
		Path: filepath.Join("images", "artifacts", options.Name.String()+".json"),
		Raw:  raw,
	}, {Path: filepath.Join("images", "Dockerfile"), Raw: []byte(labelled)}}
	return append(staged, evidence...), nil
}

// resolveNode verifies Node's detached signature over the checksums manifest and
// hashes both archives against it.
func (c *Client) resolveNode(ctx context.Context, version artifactpin.Version, key, work string,
	archives map[artifactpin.Platform]artifactpin.Archive) (*artifactpin.OpenPGP, []byte, artifactpin.Version, error) {
	manifestURL := artifactpin.SHASUMSURL(version)
	manifest, err := c.Fetch.Get(ctx, manifestURL, artifactpin.MaxArchiveBytes(artifactpin.Node))
	if err != nil {
		return nil, nil, artifactpin.Version{}, err
	}
	signature, err := c.Fetch.Get(ctx, manifestURL+".sig", artifactpin.MaxArchiveBytes(artifactpin.Node))
	if err != nil {
		return nil, nil, artifactpin.Version{}, err
	}
	manifestPath := filepath.Join(work, "SHASUMS256.txt")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		return nil, nil, artifactpin.Version{}, refuse("%s", err)
	}
	signaturePath := manifestPath + ".sig"
	if err := os.WriteFile(signaturePath, signature, 0o600); err != nil {
		return nil, nil, artifactpin.Version{}, refuse("%s", err)
	}
	fingerprint, err := c.verifySignedManifest(ctx, key, work, manifestPath, signaturePath, manifestURL+".sig")
	if err != nil {
		return nil, nil, artifactpin.Version{}, err
	}
	keyBytes, err := os.ReadFile(key)
	if err != nil {
		return nil, nil, artifactpin.Version{}, refuse("%s", err)
	}
	var bundled artifactpin.Version
	for _, platform := range artifactpin.Platforms {
		rawURL := artifactpin.CanonicalURL(artifactpin.Node, version, platform)
		archive, err := c.Fetch.Get(ctx, rawURL, artifactpin.MaxArchiveBytes(artifactpin.Node))
		if err != nil {
			return nil, nil, artifactpin.Version{}, err
		}
		filename := rawURL[strings.LastIndex(rawURL, "/")+1:]
		digest := artifactpin.Sum256(archive)
		signed, err := artifactpin.SignedDigest(manifest, filename)
		if err != nil {
			return nil, nil, artifactpin.Version{}, err
		}
		if signed != digest {
			return nil, nil, artifactpin.Version{}, refuse("the signed manifest disagrees with %s", filename)
		}
		found, err := bundledNPM(archive, filename)
		if err != nil {
			return nil, nil, artifactpin.Version{}, err
		}
		if bundled.String() != "" && bundled != found {
			return nil, nil, artifactpin.Version{}, refuse(
				"the %s archive ships npm %s, another supported architecture ships npm %s",
				platform, found, bundled)
		}
		bundled = found
		archives[platform] = artifactpin.Archive{
			Platform:     platform,
			URL:          rawURL,
			Format:       artifactpin.TarXz,
			MemberPrefix: strings.TrimSuffix(filename, ".tar.xz") + "/",
			Digest:       digest,
		}
	}
	openpgp := &artifactpin.OpenPGP{
		ManifestURL:     manifestURL,
		ManifestDigest:  artifactpin.Sum256(manifest),
		SignatureURL:    manifestURL + ".sig",
		SignatureDigest: artifactpin.Sum256(signature),
		KeyPath:         artifactpin.NodeKey,
		KeyFingerprint:  fingerprint,
		KeyDigest:       artifactpin.Sum256(keyBytes),
		VerifiedOn:      c.Now().UTC().Format("2006-01-02"),
		Reason:          artifactpin.NodeReason,
	}
	return openpgp, keyBytes, bundled, nil
}

// verifySignedManifest imports the reviewed key into a private keyring, requires
// exactly one primary key, verifies the detached signature and returns the
// fingerprint that actually signed.
func (c *Client) verifySignedManifest(ctx context.Context, key, work, manifestPath, signaturePath,
	signatureURL string) (string, error) {
	home := filepath.Join(work, "gnupg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", refuse("%s", err)
	}
	environment := gpgEnvironment(home, c.Env.Path)
	importArgv := []string{"gpg", "--batch", "--quiet", "--import", key}
	imported, err := c.run(ctx, importArgv, work, environment)
	if err != nil {
		return "", refuse("%s", err)
	}
	if err := imported.failure(importArgv); err != nil {
		return "", err
	}
	listArgv := []string{"gpg", "--batch", "--with-colons", "--import-options", "show-only",
		"--import", key}
	listed, err := c.run(ctx, listArgv, work, environment)
	if err != nil {
		return "", refuse("%s", err)
	}
	primaries := keyPrimaries(listed.Stdout)
	if len(primaries) != 1 || primaries[0] == "" {
		return "", refuse("%s must carry exactly one primary key, found %s", key, primaries)
	}
	verifyArgv := []string{"gpg", "--batch", "--status-fd=1", "--verify", signaturePath, manifestPath}
	verified, err := c.run(ctx, verifyArgv, work, environment)
	if err != nil {
		return "", refuse("%s", err)
	}
	if verified.Status != 0 {
		return "", refuse("%s does not verify: %s", signatureURL, strings.TrimSpace(verified.Stdout))
	}
	signers := validSigners(verified.Stdout)
	if len(signers) != 1 || signers[0] != primaries[0] {
		return "", refuse("the manifest is signed by %s, not by %s", signers, primaries[0])
	}
	return primaries[0], nil
}

// validSigners is the set of fingerprints gpg reports as having signed, which is
// gpg's own answer rather than the key file's claim.
func validSigners(status string) []string {
	signers := []string{}
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "VALIDSIG" {
			continue
		}
		seen := false
		for _, candidate := range []string{fields[2], fields[len(fields)-1]} {
			if len(candidate) != 40 && len(candidate) != 64 {
				continue
			}
			for _, existing := range signers {
				if existing == strings.ToUpper(candidate) {
					seen = true
				}
			}
			if !seen {
				signers = append(signers, strings.ToUpper(candidate))
				seen = true
			}
		}
	}
	return signers
}

// run is the runner call with the start failure surfaced separately from a
// non-zero status.
func (c *Client) run(ctx context.Context, argv []string, dir string,
	environment []string) (CommandResult, error) {
	return c.Runner.Run(ctx, argv, dir, environment)
}

// keyPrimaries reads the primary-key fingerprints out of gpg's colon listing, in
// order, so an empty or doubled key is refused instead of trusted.
func keyPrimaries(listed string) []string {
	primaries := []string{}
	for _, line := range strings.Split(listed, "\n") {
		switch {
		case strings.HasPrefix(line, "pub:"):
			primaries = append(primaries, "")
		case strings.HasPrefix(line, "fpr:") && len(primaries) > 0 && primaries[len(primaries)-1] == "":
			fields := strings.Split(line, ":")
			if len(fields) > 9 {
				primaries[len(primaries)-1] = strings.ToUpper(fields[9])
			}
		}
	}
	return primaries
}

// bundledNPM is the npm version the reviewed archive ships, read from the reviewed
// bytes: a release can change the bundled npm without changing the major.
func bundledNPM(archive []byte, filename string) (artifactpin.Version, error) {
	reader, err := xz.NewReader(bytes.NewReader(archive))
	if err != nil {
		return artifactpin.Version{}, refuse("%s: %s", filename, err)
	}
	member := strings.TrimSuffix(filename, ".tar.xz") + "/" + artifactpin.NPMPackage
	tarball := tar.NewReader(reader)
	for {
		header, err := tarball.Next()
		if err == io.EOF {
			return artifactpin.Version{}, refuse("%s carries no %s", filename, artifactpin.NPMPackage)
		}
		if err != nil {
			return artifactpin.Version{}, refuse("%s: %s", filename, err)
		}
		if header.Name != member {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tarball, 1<<20))
		if err != nil {
			return artifactpin.Version{}, refuse("%s: %s", filename, err)
		}
		var manifest struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(raw, &manifest); err != nil {
			return artifactpin.Version{}, refuse("%s: unreadable npm version: %s", filename, err)
		}
		return artifactpin.ParseVersion(manifest.Version, filename+": bundled npm")
	}
}

// resolveGH cross-checks the release checksums and verifies the Sigstore bundle
// with the policy the pin records.
func (c *Client) resolveGH(ctx context.Context, version artifactpin.Version, work string,
	archives map[artifactpin.Platform]artifactpin.Archive) (*artifactpin.Sigstore, []publication.Staged, error) {
	checksumsURL := artifactpin.ChecksumsURL(artifactpin.GH, version)
	var checksums []byte
	bundles := []publication.Staged{}
	for _, platform := range artifactpin.Platforms {
		rawURL := artifactpin.CanonicalURL(artifactpin.GH, version, platform)
		archive, err := c.Fetch.Get(ctx, rawURL, artifactpin.MaxArchiveBytes(artifactpin.GH))
		if err != nil {
			return nil, nil, err
		}
		filename := rawURL[strings.LastIndex(rawURL, "/")+1:]
		digest := artifactpin.Sum256(archive)
		if checksums == nil {
			checksums, err = c.Fetch.Get(ctx, checksumsURL, artifactpin.MaxArchiveBytes(artifactpin.GH))
			if err != nil {
				return nil, nil, err
			}
		}
		signed, err := artifactpin.SignedDigest(checksums, filename)
		if err != nil {
			return nil, nil, err
		}
		if signed != digest {
			return nil, nil, refuse("the upstream checksums disagree with %s", filename)
		}
		archivePath := filepath.Join(work, filename)
		if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
			return nil, nil, refuse("%s", err)
		}
		bundle, err := c.ghBundle(ctx, filename, archivePath, digest, work)
		if err != nil {
			return nil, nil, err
		}
		bundleName := artifactpin.BundleName(artifactpin.GH, version, platform)
		bundles = append(bundles, publication.Staged{
			Path: filepath.Join("images", "artifacts", bundleName),
			Raw:  bundle,
		})
		archives[platform] = artifactpin.Archive{
			Platform:     platform,
			URL:          rawURL,
			Format:       artifactpin.TarGz,
			MemberPrefix: strings.TrimSuffix(filename, ".tar.gz") + "/",
			Digest:       digest,
			Bundle: &artifactpin.Bundle{
				Path:   artifactpin.RelPath(bundleName),
				Digest: artifactpin.Sum256(bundle),
			},
		}
	}
	sigstore := &artifactpin.Sigstore{
		ChecksumsURL:    checksumsURL,
		ChecksumsDigest: artifactpin.Sum256(checksums),
		Policy:          artifactpin.GHPolicy,
		VerifiedOn:      c.Now().UTC().Format("2006-01-02"),
		Evidence:        artifactpin.GHEvidence(version),
		Reason:          artifactpin.GHReason,
	}
	return sigstore, bundles, nil
}

// ghBundle downloads the Sigstore bundle for one archive and verifies it with the
// policy the pin will record. `--bundle` needs no token; the download is the only
// step that uses the maintainer's `gh` credentials.
func (c *Client) ghBundle(ctx context.Context, filename, archivePath string,
	digest artifactpin.Digest, work string) ([]byte, error) {
	downloadArgv := []string{"gh", "attestation", "download", archivePath,
		"--repo", artifactpin.GHPolicy.Repository}
	downloaded, err := c.run(ctx, downloadArgv, work, nil)
	if err != nil {
		return nil, refuse("%s", err)
	}
	if err := downloaded.failure(downloadArgv); err != nil {
		return nil, err
	}
	bundlePath := filepath.Join(work, "sha256:"+string(digest)+".jsonl")
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, refuse("gh attestation download produced no bundle for %s", filename)
	}
	policy := artifactpin.GHPolicy
	verifyArgv := []string{"gh", "attestation", "verify", archivePath, "--bundle", bundlePath,
		"--repo", policy.Repository,
		"--signer-workflow", policy.SignerWorkflow,
		"--cert-oidc-issuer", policy.CertOIDCIssuer,
		"--deny-self-hosted-runners"}
	verified, err := c.run(ctx, verifyArgv, work, nil)
	if err != nil {
		return nil, refuse("%s", err)
	}
	if verified.Status != 0 {
		return nil, refuse("the %s attestation does not verify: %s", filename,
			commandOutput(verified.Stdout, verified.Stderr))
	}
	return bundle, nil
}
