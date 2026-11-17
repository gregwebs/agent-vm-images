package agywriter

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/artifactinstall"
	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// manifest is one parsed, validated updater manifest.
type manifest struct {
	version artifactpin.Version
	url     string
	sha512  artifactpin.SHA512
}

// corroborated is one platform after both channels agreed with the bytes.
type corroborated struct {
	platform artifactpin.Platform
	archive  string
	sha256   artifactpin.Digest
	sha512   artifactpin.SHA512
}

// resolve turns one exact version into the bytes to publish. It writes only in work.
func (c *Client) resolve(ctx context.Context, version artifactpin.Version,
	work string) ([]publication.Staged, error) {
	// 1. Both manifests, compared on their version fields only. This is the
	//    latest-only gate: the manifest names one release, so that is the release a
	//    run may pin.
	manifests, raw, err := c.fetchManifests(ctx, version, work)
	if err != nil {
		return nil, err
	}

	// 2. The committed state, before a payload is downloaded: a manifest that names
	//    a release older than the pin is a refusal, not a downgrade. A newer one is
	//    the normal forward bump and continues.
	committed, err := c.committed()
	if err != nil {
		return nil, err
	}
	if committed != nil && newerVersion(committed.Version, version) {
		return nil, refuse("the updater manifest names %s, which is older than the "+
			"pinned %s", version, committed.Version)
	}

	// 3. The exact release, and its per-asset digests.
	release, err := c.fetchRelease(ctx, version, work)
	if err != nil {
		return nil, err
	}
	digests, err := artifactpin.ReleaseAssetDigests(release, artifactpin.Agy, version)
	if err != nil {
		return nil, err
	}

	// 4. The payload, accepted only when both channels name these bytes.
	archives := make(map[artifactpin.Platform]artifactpin.Archive, len(manifests))
	downloaded := make(map[artifactpin.Platform]corroborated, len(manifests))
	for _, platform := range artifactpin.Platforms {
		found, err := c.corroborate(ctx, version, platform, work, manifests, digests)
		if err != nil {
			return nil, err
		}
		archives[platform] = artifactpin.Archive{
			Platform:     platform,
			URL:          artifactpin.CanonicalURL(artifactpin.Agy, version, platform),
			Format:       artifactpin.TarGz,
			MemberPrefix: artifactpin.MemberPrefix(artifactpin.Agy, version, platform),
			Digest:       found.sha256,
			SHA512:       found.sha512,
		}
		downloaded[platform] = found
	}

	verifiedOn := c.Now().UTC().Format("2006-01-02")
	if committed != nil && committed.Version == version {
		// A same-release run may only repeat itself. Download counts change daily, so
		// keeping the committed evidence bytes when the security projection is
		// unchanged is what makes a repeat print `unchanged` instead of a churn-only
		// diff -- and the review date does not move.
		if err := requireSameRelease(committed, downloaded); err != nil {
			return nil, err
		}
		release, raw, err = c.keepCommittedEvidence(committed, release, raw)
		if err != nil {
			return nil, err
		}
		verifiedOn = committed.Authentication.HashOnly.VerifiedOn
	}

	// 5. The proposal bytes, with the ARG rewritten and the label required.
	document, err := artifactpin.Render(artifactpin.Agy, version, artifactpin.Review{
		VerifiedOn: verifiedOn,
		Archives:   archives,
		HashOnly:   authenticationOf(version, release, raw, verifiedOn),
	})
	if err != nil {
		return nil, err
	}
	dockerfilePath := filepath.Join(c.Env.Root, Dockerfile)
	committedDockerfile, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return nil, refuse("cannot read %s", dockerfilePath)
	}
	dockerfile, err := renderArg(string(committedDockerfile), version)
	if err != nil {
		return nil, err
	}
	staged := stagedFiles(document, release, raw, []byte(dockerfile))

	// 6. Validate exactly the bytes that are then published: the production loader
	//    over a private copy, its corroboration check, the digests from step 4, and
	//    the archive's own shape read from the downloaded file.
	if err := c.validateProposal(staged, version, downloaded, work); err != nil {
		return nil, err
	}
	return staged, nil
}

// fetchManifests reads both updater manifests and requires them to name the same
// release, which must be the release the run was asked for.
func (c *Client) fetchManifests(ctx context.Context, version artifactpin.Version,
	work string) (map[artifactpin.Platform]manifest, map[artifactpin.Platform][]byte, error) {
	manifests := make(map[artifactpin.Platform]manifest, len(artifactpin.Platforms))
	raw := make(map[artifactpin.Platform][]byte, len(artifactpin.Platforms))
	names := make([]string, 0, len(artifactpin.Platforms))
	for _, platform := range artifactpin.Platforms {
		document, err := c.get(ctx, artifactpin.ManifestURL(platform), MaxJSONBytes)
		if err != nil {
			return nil, nil, err
		}
		what := "agy " + string(platform) + " manifest"
		parsed, err := artifactpin.ParseManifest(document, what)
		if err != nil {
			return nil, nil, err
		}
		found, err := artifactpin.ParseVersion(parsed.Version, what)
		if err != nil {
			return nil, nil, err
		}
		manifests[platform] = manifest{version: found, url: parsed.URL, sha512: parsed.SHA512}
		raw[platform] = document
		names = append(names, string(platform)+" "+found.String())
	}
	for _, platform := range artifactpin.Platforms {
		if found := manifests[platform].version; found != version {
			return nil, nil, refuse("the updater manifests name %s; only the release "+
				"they currently name can be pinned (asked for %s)",
				strings.Join(names, ", "), version)
		}
	}
	return manifests, raw, nil
}

// fetchRelease reads the GitHub release record for exactly this version.
func (c *Client) fetchRelease(ctx context.Context, version artifactpin.Version,
	work string) ([]byte, error) {
	return c.get(ctx, artifactpin.AgyReleaseAPI+version.String(), MaxJSONBytes)
}

// corroborate downloads one archive and requires both channels to name its bytes.
func (c *Client) corroborate(ctx context.Context, version artifactpin.Version,
	platform artifactpin.Platform, work string,
	manifests map[artifactpin.Platform]manifest,
	digests map[string]string) (corroborated, error) {
	filename := artifactpin.ExpectedFilename(artifactpin.Agy, version, platform)
	expected, ok := digests[filename]
	if !ok {
		return corroborated{}, refuse("GitHub does not list %s for %s", filename, version)
	}
	payload, err := c.get(ctx, artifactpin.CanonicalURL(artifactpin.Agy, version, platform),
		artifactpin.MaxArchiveBytes(artifactpin.Agy))
	if err != nil {
		return corroborated{}, err
	}
	if err := os.WriteFile(filepath.Join(work, filename), payload, 0o600); err != nil {
		return corroborated{}, refuse("cannot write %s in the work directory: %s", filename, err)
	}
	sha256 := artifactpin.Sum256(payload)
	sha512 := sum512(payload)
	if expected != "sha256:"+string(sha256) {
		return corroborated{}, refuse("GitHub records %s for %s, but the downloaded "+
			"bytes are sha256:%s", expected, filename, sha256)
	}
	if manifests[platform].sha512 != sha512 {
		return corroborated{}, refuse("Google's %s manifest records sha512 %s for %s, "+
			"but the downloaded bytes are %s", platform, manifests[platform].sha512,
			filename, sha512)
	}
	return corroborated{platform: platform, archive: filepath.Join(work, filename),
		sha256: sha256, sha512: sha512}, nil
}

// committed is the validated committed pin, or nil on a clean first adoption.
//
// A half-populated directory is refused rather than repaired: this writer publishes
// a reviewed selection, it does not make a broken one look better.
func (c *Client) committed() (*artifactpin.Pin, error) {
	directory := filepath.Join(c.Env.Root, PinDirectory)
	pinPath := filepath.Join(directory, "pin.json")
	if !plainFile(pinPath) {
		entries, err := os.ReadDir(directory)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, nil
			}
			return nil, refuse("cannot read %s: %s", directory, err)
		}
		if len(entries) > 0 {
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return nil, refuse("%s holds %v with no pin.json; refusing a partial old "+
				"state", directory, names)
		}
		return nil, nil
	}
	pin, err := artifactpin.Load(pinPath)
	if err != nil {
		return nil, err
	}
	if pin.Name != artifactpin.Agy {
		return nil, refuse("%s names artifact %s", pinPath, pin.Name)
	}
	if err := artifactpin.CheckCorroboration(pin); err != nil {
		return nil, err
	}
	return &pin, nil
}

// keepCommittedEvidence replaces the freshly fetched bytes with the committed ones
// wherever the two documents say the same thing.
func (c *Client) keepCommittedEvidence(committed *artifactpin.Pin, release []byte,
	raw map[artifactpin.Platform][]byte) ([]byte, map[artifactpin.Platform][]byte, error) {
	releasePath := filepath.Join(c.Env.Root, PinDirectory,
		committed.Authentication.HashOnly.Release.Path.String())
	committedRelease, err := os.ReadFile(releasePath)
	if err != nil {
		return nil, nil, refuse("cannot read %s: %s", releasePath, err)
	}
	same, err := sameReleaseProjection(release, committedRelease)
	if err != nil {
		return nil, nil, err
	}
	if same {
		release = committedRelease
	}
	for _, platform := range artifactpin.Platforms {
		path := filepath.Join(c.Env.Root, PinDirectory,
			committed.Authentication.HashOnly.Manifests[platform].Path.String())
		committedManifest, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, refuse("cannot read %s: %s", path, err)
		}
		fetched, err := artifactpin.ParseManifest(raw[platform],
			"agy "+string(platform)+" manifest")
		if err != nil {
			return nil, nil, err
		}
		kept, err := artifactpin.ParseManifest(committedManifest,
			"agy "+string(platform)+" manifest")
		if err != nil {
			return nil, nil, err
		}
		if fetched == kept {
			raw[platform] = committedManifest
		}
	}
	return release, raw, nil
}

// requireSameRelease refuses an upstream rewrite of an already-pinned release: the
// digest chain cannot tell a re-upload from a re-download.
func requireSameRelease(committed *artifactpin.Pin,
	downloaded map[artifactpin.Platform]corroborated) error {
	for _, platform := range artifactpin.Platforms {
		archive, err := committed.Archive(platform)
		if err != nil {
			return err
		}
		found := downloaded[platform]
		if archive.Digest != found.sha256 || archive.SHA512 != found.sha512 {
			return refuse("%s changed for the already-pinned %s; a same-release "+
				"upstream rewrite needs a separate reviewed adoption",
				filepath.Base(found.archive), committed.Version)
		}
	}
	return nil
}

// validateProposal loads exactly the bytes that are published and runs the build's
// own archive checks over the downloads they describe.
func (c *Client) validateProposal(staged []publication.Staged, version artifactpin.Version,
	downloaded map[artifactpin.Platform]corroborated, work string) error {
	proposal := filepath.Join(work, "proposal")
	for _, entry := range staged {
		path := filepath.Join(proposal, entry.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return refuse("cannot create %s: %s", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, entry.Raw, 0o600); err != nil {
			return refuse("cannot write %s: %s", path, err)
		}
	}
	pin, err := artifactpin.Load(filepath.Join(proposal, PinFile))
	if err != nil {
		return err
	}
	if err := artifactpin.CheckCorroboration(pin); err != nil {
		return err
	}
	if pin.Version != version {
		return refuse("the proposal pin selects %s, not %s", pin.Version, version)
	}
	for _, platform := range artifactpin.Platforms {
		archive, err := pin.Archive(platform)
		if err != nil {
			return err
		}
		found := downloaded[platform]
		if archive.Digest != found.sha256 || archive.SHA512 != found.sha512 {
			return refuse("the proposal pin records a different identity for %s than "+
				"the bytes this run corroborated", platform)
		}
		// The same gate the build runs, from the same code: a pin is a promise that
		// the bytes were read the way the installer reads them.
		if err := artifactinstall.PreflightArchive(pin, platform, found.archive); err != nil {
			return err
		}
	}
	return nil
}

// get is one bounded read of one upstream document.
func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	raw, err := c.Fetch.Get(ctx, url, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, refuse("%s is larger than the %d byte ceiling", url, limit)
	}
	return raw, nil
}

func sum512(raw []byte) artifactpin.SHA512 {
	sum := sha512.Sum512(raw)
	return artifactpin.SHA512(hex.EncodeToString(sum[:]))
}

// newerVersion reports whether left is a later release than right.
func newerVersion(left, right artifactpin.Version) bool {
	parse := func(version artifactpin.Version) []int {
		parts := strings.Split(version.String(), ".")
		values := make([]int, 0, len(parts))
		for _, part := range parts {
			value := 0
			for _, digit := range part {
				value = value*10 + int(digit-'0')
			}
			values = append(values, value)
		}
		return values
	}
	leftParts, rightParts := parse(left), parse(right)
	for index := 0; index < len(leftParts) && index < len(rightParts); index++ {
		if leftParts[index] != rightParts[index] {
			return leftParts[index] > rightParts[index]
		}
	}
	return len(leftParts) > len(rightParts)
}

// refuse is a case that failed before anything was published: a status-1 refusal
// whose message is the diagnostic.
func refuse(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
