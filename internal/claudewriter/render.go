package claudewriter

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/claudepin"
)

// document is the pin's marshalled shape: the field order and the two-space indent
// the committed file has, so a refresh is a one-line review diff.
type document struct {
	Schema         int                    `json:"schema"`
	Name           string                 `json:"name"`
	Kind           string                 `json:"kind"`
	Version        string                 `json:"version"`
	Manifest       evidenceDocument       `json:"manifest"`
	Signature      evidenceDocument       `json:"signature"`
	Key            keyDocument            `json:"key"`
	Platforms      platformsDocument      `json:"platforms"`
	Authentication authenticationDocument `json:"authentication"`
}

type evidenceDocument struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type keyDocument struct {
	URL         string `json:"url"`
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	SHA256      string `json:"sha256"`
}

type platformsDocument struct {
	AMD64 executableDocument `json:"linux/amd64"`
	ARM64 executableDocument `json:"linux/arm64"`
}

type executableDocument struct {
	ManifestPlatform string `json:"manifest_platform"`
	URL              string `json:"url"`
	SHA256           string `json:"sha256"`
	Size             int64  `json:"size"`
}

type authenticationDocument struct {
	Method     string `json:"method"`
	VerifiedOn string `json:"verified_on"`
	Reason     string `json:"reason"`
}

// Render builds the pin a refresh resolved. Nothing is read from the committed pin
// here: everything a build acts on comes from this run's verification.
func Render(version string, upstream fetched, key []byte,
	executables map[string]downloaded, verifiedOn, signer string) ([]byte, error) {
	platform := func(name string) executableDocument {
		token := claudepin.Platforms[name]
		received := executables[token]
		return executableDocument{
			ManifestPlatform: token,
			URL:              claudepin.ExecutableURL(version, token),
			SHA256:           received.sha256,
			Size:             received.size,
		}
	}
	rendered := document{
		Schema:  claudepin.Schema,
		Name:    claudepin.Name,
		Kind:    claudepin.Kind,
		Version: version,
		Manifest: evidenceDocument{
			URL:    claudepin.ManifestURL(version),
			Path:   claudepin.ManifestPath,
			SHA256: claudepin.Sha256Of(upstream.manifest),
		},
		Signature: evidenceDocument{
			URL:    claudepin.SignatureURL(version),
			Path:   claudepin.SignaturePath,
			SHA256: claudepin.Sha256Of(upstream.signature),
		},
		Key: keyDocument{
			URL:         claudepin.KeyURL,
			Path:        claudepin.KeyPath,
			Fingerprint: signer,
			SHA256:      claudepin.Sha256Of(key),
		},
		Platforms: platformsDocument{AMD64: platform("linux/amd64"), ARM64: platform("linux/arm64")},
		Authentication: authenticationDocument{
			Method:     claudepin.AuthenticationMethod,
			VerifiedOn: verifiedOn,
			Reason:     Reason,
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

// RequireSingleSlot demands exactly one Claude ARG at this version and a label that
// reads it. The label is not decoration: it is how a released image says which
// version its binary is.
func RequireSingleSlot(dockerfile, version string) error {
	lines := strings.Split(dockerfile, "\n")
	want := "ARG " + Argument + "=" + version
	matches := []string{}
	for _, line := range lines {
		if strings.HasPrefix(line, "ARG "+Argument+"=") {
			matches = append(matches, line)
		}
	}
	if len(matches) != 1 || matches[0] != want {
		return refusal("the Dockerfile must declare exactly one `ARG %s=%s`, found %v",
			Argument, version, matches)
	}
	label := "LABEL " + Label + `="${` + Argument + `}"`
	for _, line := range lines {
		if line == label {
			return nil
		}
	}
	return refusal("the Dockerfile must declare `%s`", label)
}

// RenderArgument is the reviewed `ARG AGENT_VERSION_CLAUDE` default, on exactly one
// line.
func RenderArgument(dockerfile, version string) (string, error) {
	lines := strings.Split(dockerfile, "\n")
	matches := []int{}
	for index, line := range lines {
		if strings.HasPrefix(line, "ARG "+Argument+"=") {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		return "", refusal("the Dockerfile must declare exactly one `ARG %s=%s`, found %d",
			Argument, version, len(matches))
	}
	lines[matches[0]] = "ARG " + Argument + "=" + version
	rendered := strings.Join(lines, "\n")
	if err := RequireSingleSlot(rendered, version); err != nil {
		return "", err
	}
	return rendered, nil
}

// writeProposal writes the publication bytes into a private copy of the pin
// directory, so the production loader validates exactly the bytes that are then
// published.
func writeProposal(work string, files []publicationFile, key []byte) (string, error) {
	directory := filepath.Join(work, "proposal")
	for _, file := range files {
		path := filepath.Join(directory, file.path)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, file.raw, 0o644); err != nil {
			return "", err
		}
	}
	keyPath := filepath.Join(directory, PinDirectory, claudepin.KeyPath)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(keyPath, key, 0o644); err != nil {
		return "", err
	}
	return filepath.Join(directory, PinDirectory), nil
}
