package agywriter

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// labelLine is the image label that reads the ARG default, spelled once.
const labelLine = "LABEL " + Label + `="${` + Argument + `}"`

// renderArg is the reviewed `ARG AGENT_VERSION_AGY` default, on exactly one line,
// with the label that reads it required to be present.
//
// The label is not rewritten: it already reads the argument, so a version bump moves
// the label with it. What is required here is that the image keeps reporting the
// version through *this* argument, which is the only thing that makes a single-line
// rewrite meaningful.
func renderArg(dockerfile string, version artifactpin.Version) (string, error) {
	lines := strings.Split(dockerfile, "\n")
	matches := make([]int, 0, 1)
	for index, line := range lines {
		if strings.HasPrefix(line, "ARG "+Argument+"=") {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		return "", refuse("the Dockerfile must declare exactly one `ARG %s=%s`, found %d",
			Argument, version, len(matches))
	}
	lines[matches[0]] = "ARG " + Argument + "=" + version.String()
	rendered := strings.Join(lines, "\n")
	if err := requireSingleSlot(rendered, version); err != nil {
		return "", err
	}
	return rendered, nil
}

// requireSingleSlot is the committed shape: exactly one agy ARG at this version, and
// the label that reads it.
func requireSingleSlot(dockerfile string, version artifactpin.Version) error {
	lines := strings.Split(dockerfile, "\n")
	args := make([]string, 0, 1)
	for _, line := range lines {
		if strings.HasPrefix(line, "ARG "+Argument+"=") {
			args = append(args, line)
		}
	}
	wanted := "ARG " + Argument + "=" + version.String()
	if len(args) != 1 || args[0] != wanted {
		return refuse("the Dockerfile must declare exactly one `%s`, found %v", wanted, args)
	}
	for _, line := range lines {
		if line == labelLine {
			return nil
		}
	}
	return refuse("the Dockerfile must declare `%s`", labelLine)
}

// authenticationOf is the corroboration the pin records: both records' origins,
// committed paths and digests.
func authenticationOf(version artifactpin.Version, release []byte,
	manifests map[artifactpin.Platform][]byte,
	verifiedOn string) *artifactpin.HashOnly {
	hashOnly := &artifactpin.HashOnly{
		Release: artifactpin.Evidence{
			URL:    artifactpin.AgyReleaseAPI + version.String(),
			Path:   artifactpin.AgyReleaseEvidence,
			Digest: artifactpin.Sum256(release),
		},
		Manifests:  make(map[artifactpin.Platform]artifactpin.Evidence, len(manifests)),
		VerifiedOn: verifiedOn,
		Reason:     Reason,
	}
	for _, platform := range artifactpin.Platforms {
		hashOnly.Manifests[platform] = artifactpin.Evidence{
			URL:    artifactpin.ManifestURL(platform),
			Path:   artifactpin.AgyEvidence[platform],
			Digest: artifactpin.Sum256(manifests[platform]),
		}
	}
	return hashOnly
}

// stagedFiles is the five-file publication, keyed by destination.
func stagedFiles(pin, release []byte, manifests map[artifactpin.Platform][]byte,
	dockerfile []byte) []publication.Staged {
	staged := []publication.Staged{
		{Path: PinFile, Raw: pin},
		{Path: PinDirectory + "/" + artifactpin.AgyReleaseEvidence.String(), Raw: release},
		{Path: Dockerfile, Raw: dockerfile},
	}
	for _, platform := range artifactpin.Platforms {
		staged = append(staged, publication.Staged{
			Path: PinDirectory + "/" + artifactpin.AgyEvidence[platform].String(),
			Raw:  manifests[platform],
		})
	}
	return staged
}

// sameReleaseProjection reports whether two GitHub release records describe the same
// release.
//
// Download counts, ids and timestamps change on every fetch of the same tag; the tag
// and the asset names with their digests are the security projection. A projection
// that moved is a different release record, and the committed bytes are not kept.
func sameReleaseProjection(left, right []byte) (bool, error) {
	leftProjection, err := releaseProjection(left)
	if err != nil {
		return false, err
	}
	rightProjection, err := releaseProjection(right)
	if err != nil {
		return false, err
	}
	return leftProjection == rightProjection, nil
}

type assetProjection struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type releaseView struct {
	Tag    string            `json:"tag_name"`
	Assets []assetProjection `json:"assets"`
}

// releaseProjection is a re-read for comparison, not a validation: both documents
// have already been through the strict decoder that refuses a duplicate key (the
// committed one in CheckCorroboration, the fresh one in ReleaseAssetDigests), so a
// lenient decode here cannot admit a shape either of those refused.
func releaseProjection(raw []byte) (string, error) {
	var view releaseView
	if err := json.Unmarshal(raw, &view); err != nil {
		return "", refuse("the release record is not the reviewed shape: %s", err)
	}
	assets := make([]string, 0, len(view.Assets))
	for _, asset := range view.Assets {
		if asset.Name == "" {
			return "", refuse("the release record names an asset with no name")
		}
		assets = append(assets, asset.Name+"\x00"+asset.Digest)
	}
	sort.Strings(assets)
	return view.Tag + "\x00" + strings.Join(assets, "\x00"), nil
}
