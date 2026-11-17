package codewriter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
	"github.com/gregwebs/agent-vm-images/internal/evidence"
)

// document is the pin's marshalled shape: the field order and the two-space indent
// the committed file has, so a refresh is a one-line review diff.
type document struct {
	Schema         int                    `json:"schema"`
	Name           string                 `json:"name"`
	Kind           string                 `json:"kind"`
	Release        string                 `json:"release"`
	Version        string                 `json:"version"`
	Package        packageDocument        `json:"package"`
	Metadata       evidenceDocument       `json:"metadata"`
	Checksums      evidenceDocument       `json:"checksums"`
	Platforms      platformsDocument      `json:"platforms"`
	Authentication authenticationDocument `json:"authentication"`
}

type packageDocument struct {
	LayoutVersion int    `json:"layout_version"`
	Variant       string `json:"variant"`
	Entrypoint    string `json:"entrypoint"`
}

type evidenceDocument struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type platformDocument struct {
	Target string           `json:"target"`
	Asset  string           `json:"asset"`
	URL    string           `json:"url"`
	SHA256 string           `json:"sha256"`
	Bundle evidenceDocument `json:"bundle"`
}

type platformsDocument struct {
	AMD64 platformDocument `json:"linux/amd64"`
	ARM64 platformDocument `json:"linux/arm64"`
}

type verifierDocument struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type authenticationDocument struct {
	Method                string           `json:"method"`
	Verifier              verifierDocument `json:"verifier"`
	TrustRoot             string           `json:"trust_root"`
	CertificateIdentity   string           `json:"certificate_identity"`
	CertificateOIDCIssuer string           `json:"certificate_oidc_issuer"`
	VerifiedOn            string           `json:"verified_on"`
}

// Render builds the pin this run resolved. Nothing is read from the committed pin
// here: everything a build acts on comes from this run's downloaded digests.
func Render(release, version string, metadata, checksums []byte,
	platforms map[string]identity, verifiedOn time.Time) ([]byte, error) {
	platform := func(name string) (platformDocument, error) {
		pinned, ok := platforms[name]
		if !ok || pinned.archive == "" || pinned.bundle == "" {
			return platformDocument{}, refusal("the pin is missing the %s digests", name)
		}
		target := codexpin.Targets[name]
		return platformDocument{
			Target: string(target),
			Asset:  codexpin.AssetName(target),
			URL:    codexpin.AssetURL(release, codexpin.AssetName(target)),
			SHA256: pinned.archive,
			Bundle: evidenceDocument{
				URL:    codexpin.AssetURL(release, codexpin.BundleName(target)),
				Path:   codexpin.BundlePath(target),
				SHA256: pinned.bundle,
			},
		}, nil
	}
	amd64, err := platform("linux/amd64")
	if err != nil {
		return nil, err
	}
	arm64, err := platform("linux/arm64")
	if err != nil {
		return nil, err
	}
	rendered := document{
		Schema:  codexpin.Schema,
		Name:    codexpin.Name,
		Kind:    codexpin.Kind,
		Release: release,
		Version: version,
		Package: packageDocument{
			LayoutVersion: codexpin.PackageLayoutVersion,
			Variant:       codexpin.PackageVariant,
			Entrypoint:    codexpin.PackageEntrypoint,
		},
		Metadata: evidenceDocument{
			URL:    codexpin.APIURL(release),
			Path:   codexpin.MetadataPath,
			SHA256: evidence.Sha256Of(metadata),
		},
		Checksums: evidenceDocument{
			URL:    codexpin.AssetURL(release, codexpin.ChecksumsAsset),
			Path:   codexpin.ChecksumsPath,
			SHA256: evidence.Sha256Of(checksums),
		},
		Platforms: platformsDocument{AMD64: amd64, ARM64: arm64},
		Authentication: authenticationDocument{
			Method:                codexpin.AuthenticationMethod,
			Verifier:              verifierDocument{Name: codexpin.VerifierName, Version: codexpin.VerifierVersion},
			TrustRoot:             codexpin.TrustRoot,
			CertificateIdentity:   codexpin.Identity(release),
			CertificateOIDCIssuer: codexpin.Issuer,
			VerifiedOn:            verifiedOn.Format("2006-01-02"),
		},
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(rendered); err != nil {
		return nil, refusal("cannot render the pin: %s", err)
	}
	return buffer.Bytes(), nil
}

// RenderArgument is the reviewed `ARG AGENT_VERSION_CODEX` default, on exactly one
// line. Every other byte of the Dockerfile is the caller's.
func RenderArgument(dockerfile, release string) (string, error) {
	lines := strings.Split(dockerfile, "\n")
	matches := make([]int, 0, 1)
	for index, line := range lines {
		if strings.HasPrefix(line, "ARG "+Argument+"=") {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		return "", refusal("the Dockerfile must declare exactly one `ARG %s=`, found %d",
			Argument, len(matches))
	}
	lines[matches[0]] = "ARG " + Argument + "=" + release
	return strings.Join(lines, "\n"), nil
}

// RequireSingleSlot demands exactly one Codex ARG at this release and a label that
// reads it. The label is not decoration: it is how a released image says which
// version its binary is.
func RequireSingleSlot(dockerfile, release string) error {
	lines := strings.Split(dockerfile, "\n")
	args := make([]string, 0, 1)
	for _, line := range lines {
		if strings.HasPrefix(line, "ARG "+Argument+"=") {
			args = append(args, line)
		}
	}
	want := "ARG " + Argument + "=" + release
	if len(args) != 1 || args[0] != want {
		return refusal("the Dockerfile must declare exactly one `ARG %s=%s`", Argument, release)
	}
	label := "LABEL " + Label + `="${` + Argument + `}"`
	if !slices.Contains(lines, label) {
		return refusal("the Dockerfile label `%s` must reference ${%s}", Label, Argument)
	}
	return nil
}

// unownedView is the Dockerfile with every single-slot ARG default masked.
//
// Two recipes that differ only in those defaults are the same file for the purpose
// of this transaction, so a Codex proposal cannot smuggle an unrelated Dockerfile
// edit through the Codex publication.
func unownedView(dockerfile string) []string {
	names := append([]string{Argument}, UnownedArgs...)
	view := []string{}
	for _, line := range strings.Split(dockerfile, "\n") {
		masked := line
		for _, name := range names {
			if strings.HasPrefix(line, "ARG "+name+"=") {
				masked = "ARG " + name + "=<masked>"
				break
			}
		}
		view = append(view, masked)
	}
	return view
}

// unchanged is the no-op case: every publication byte already equals the committed
// byte.
func (c *Client) unchanged(proposed proposal) bool {
	for _, file := range proposed.files {
		raw, err := os.ReadFile(filepath.Join(c.Env.Root, filepath.FromSlash(file.destination)))
		if err != nil || !bytes.Equal(raw, file.raw) {
			return false
		}
	}
	return true
}
