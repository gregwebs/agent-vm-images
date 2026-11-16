package artifactpin

import (
	"bytes"
	"encoding/json"
	"strings"
)

// document is the marshalled shape of a pin. The field order and the two-space
// indentation are the pin file's own: a refresh rewrites only the values that
// changed, so a reviewer sees one line instead of a reformatted record. The round
// trip is asserted against both committed pins (document_test.go).
type document struct {
	Schema         int                        `json:"schema"`
	Name           string                     `json:"name"`
	Kind           string                     `json:"kind"`
	Version        string                     `json:"version"`
	Upstream       upstreamDocument           `json:"upstream"`
	Install        installDocument            `json:"install"`
	Platforms      map[string]archiveDocument `json:"platforms"`
	Bundled        map[string]string          `json:"bundled,omitempty"`
	Authentication authenticationDocument     `json:"authentication"`
}

type upstreamDocument struct {
	Project string `json:"project"`
	Release string `json:"release"`
}

type installDocument struct {
	Destination string         `json:"destination"`
	Payload     []any          `json:"payload"`
	Links       []linkDocument `json:"links"`
}

// memberDocument is the second payload spelling: the archive member and the
// published path apart from each other, which is how a hash-only artifact whose
// archive carries a differently named binary is spelled.
type memberDocument struct {
	Member RelPath `json:"member"`
	Path   RelPath `json:"path"`
}

type linkDocument struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

type archiveDocument struct {
	URL          string          `json:"url"`
	Format       string          `json:"archive"`
	MemberPrefix string          `json:"member_prefix"`
	Digest       string          `json:"sha256"`
	SHA512       string          `json:"sha512,omitempty"`
	Bundle       *bundleDocument `json:"bundle,omitempty"`
}

type bundleDocument struct {
	Path   string `json:"path"`
	Digest string `json:"sha256"`
}

// authenticationDocument carries all three methods in one struct so a sigstore
// pin and an openpgp pin keep their own key order: one method's fields are absent,
// never empty.
type authenticationDocument struct {
	Method     string                  `json:"method"`
	Manifest   *evidenceDocument       `json:"manifest,omitempty"`
	Signature  *evidenceDocument       `json:"signature,omitempty"`
	Key        *keyDocument            `json:"key,omitempty"`
	Checksums  *evidenceDocument       `json:"checksums,omitempty"`
	Policy     *policyDocument         `json:"policy,omitempty"`
	Release    *evidencePath           `json:"release,omitempty"`
	Manifests  map[string]evidencePath `json:"manifests,omitempty"`
	VerifiedOn string                  `json:"verified_on"`
	Evidence   []string                `json:"evidence,omitempty"`
	Reason     string                  `json:"reason"`
}

// evidencePath is a committed corroboration file: the URL it came from, where it
// is committed and the digest that binds the two.
type evidencePath struct {
	URL    string  `json:"url"`
	Path   RelPath `json:"path"`
	Digest string  `json:"sha256"`
}

type evidenceDocument struct {
	URL    string `json:"url"`
	Digest string `json:"sha256"`
}

type keyDocument struct {
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	Digest      string `json:"sha256"`
}

type policyDocument struct {
	Repository            string `json:"repository"`
	SignerWorkflow        string `json:"signer_workflow"`
	CertOIDCIssuer        string `json:"cert_oidc_issuer"`
	DenySelfHostedRunners bool   `json:"deny_self_hosted_runners"`
}

// Marshal renders the pin the way the committed files are written: two-space
// indent, no HTML escaping (a URL or a reason may carry `&`), one trailing
// newline.
func (d document) Marshal() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(d); err != nil {
		return nil, refuse("cannot render the pin: %s", err)
	}
	return buffer.Bytes(), nil
}

// documentOf converts a loaded pin back into the committed shape. It is the round
// trip the two committed files are held to, and it is deliberately separate from
// Render: one direction proves the loader lost nothing, the other builds a pin
// from what a refresh resolved.
func documentOf(pin Pin) document {
	platforms := make(map[string]archiveDocument, len(pin.Archives))
	for platform, archive := range pin.Archives {
		entry := archiveDocument{
			URL:          archive.URL,
			Format:       string(archive.Format),
			MemberPrefix: archive.MemberPrefix,
			Digest:       string(archive.Digest),
		}
		if archive.SHA512 != "" {
			entry.SHA512 = string(archive.SHA512)
		}
		if archive.Bundle != nil {
			entry.Bundle = &bundleDocument{
				Path:   archive.Bundle.Path.String(),
				Digest: string(archive.Bundle.Digest),
			}
		}
		platforms[string(platform)] = entry
	}
	payload := make([]any, 0, len(pin.Install.Payload))
	for _, member := range pin.Install.Payload {
		if member.Archive == member.Published {
			payload = append(payload, member.Published.String())
			continue
		}
		payload = append(payload, memberDocument{Member: member.Archive, Path: member.Published})
	}
	links := make([]linkDocument, 0, len(pin.Install.Links))
	for _, link := range pin.Install.Links {
		links = append(links, linkDocument{Path: link.Path.String(), Target: link.Target.String()})
	}
	rendered := document{
		Schema:  Schema,
		Name:    pin.Name.String(),
		Kind:    KindReleaseArchive,
		Version: pin.Version.String(),
		Upstream: upstreamDocument{
			Project: pin.Upstream.Project,
			Release: pin.Upstream.Release,
		},
		Install: installDocument{
			Destination: pin.Install.Destination.String(),
			Payload:     payload,
			Links:       links,
		},
		Platforms: platforms,
		Authentication: authenticationDocument{
			VerifiedOn: pin.Authentication.VerifiedOn(),
			Reason:     pin.Authentication.Reason(),
		},
	}
	if pin.Bundled != nil {
		bundled := make(map[string]string, len(pin.Bundled))
		for name, version := range pin.Bundled {
			bundled[name] = version.String()
		}
		rendered.Bundled = bundled
	}
	switch {
	case pin.Authentication.OpenPGP != nil:
		openpgp := pin.Authentication.OpenPGP
		rendered.Authentication.Method = string(MethodOpenPGP)
		rendered.Authentication.Manifest = &evidenceDocument{
			URL: openpgp.ManifestURL, Digest: string(openpgp.ManifestDigest)}
		rendered.Authentication.Signature = &evidenceDocument{
			URL: openpgp.SignatureURL, Digest: string(openpgp.SignatureDigest)}
		rendered.Authentication.Key = &keyDocument{
			Path:        openpgp.KeyPath.String(),
			Fingerprint: openpgp.KeyFingerprint,
			Digest:      string(openpgp.KeyDigest),
		}
	case pin.Authentication.Sigstore != nil:
		sigstore := pin.Authentication.Sigstore
		rendered.Authentication.Method = string(MethodSigstore)
		rendered.Authentication.Checksums = &evidenceDocument{
			URL: sigstore.ChecksumsURL, Digest: string(sigstore.ChecksumsDigest)}
		rendered.Authentication.Policy = &policyDocument{
			Repository:            sigstore.Policy.Repository,
			SignerWorkflow:        sigstore.Policy.SignerWorkflow,
			CertOIDCIssuer:        sigstore.Policy.CertOIDCIssuer,
			DenySelfHostedRunners: sigstore.Policy.DenySelfHostedRunners,
		}
		rendered.Authentication.Evidence = sigstore.Evidence
	case pin.Authentication.HashOnly != nil:
		hashOnly := pin.Authentication.HashOnly
		rendered.Authentication.Method = string(MethodHashOnly)
		rendered.Authentication.Release = &evidencePath{
			URL:    hashOnly.Release.URL,
			Path:   hashOnly.Release.Path,
			Digest: string(hashOnly.Release.Digest),
		}
		manifests := make(map[string]evidencePath, len(hashOnly.Manifests))
		for platform, manifest := range hashOnly.Manifests {
			manifests[string(platform)] = evidencePath{
				URL:    manifest.URL,
				Path:   manifest.Path,
				Digest: string(manifest.Digest),
			}
		}
		rendered.Authentication.Manifests = manifests
	}
	return rendered
}

// VerifiedOn is the review date the pin records.
func (a Authentication) VerifiedOn() string {
	switch {
	case a.OpenPGP != nil:
		return a.OpenPGP.VerifiedOn
	case a.Sigstore != nil:
		return a.Sigstore.VerifiedOn
	case a.HashOnly != nil:
		return a.HashOnly.VerifiedOn
	}
	return ""
}

// Review is what a refresh resolved and verified for one exact release, before any
// of it is written. Render turns it into the committed shape.
type Review struct {
	// VerifiedOn is the UTC date the evidence was checked.
	VerifiedOn string
	// Archives is one reviewed archive per supported platform.
	Archives map[Platform]Archive
	OpenPGP  *OpenPGP
	Sigstore *Sigstore
	Bundled  map[string]Version
}

// Render builds the pin document a refresh publishes.
func Render(name Name, version Version, review Review) ([]byte, error) {
	if (review.OpenPGP == nil) == (review.Sigstore == nil) {
		return nil, refuse("%s: a release needs exactly one authentication method", name)
	}
	pin := Pin{
		Name:     name,
		Version:  version,
		Upstream: Upstream{Project: upstreamProject(name), Release: ReleaseTag(name, version)},
		Install:  Install{Destination: "/usr/local"},
		Archives: review.Archives,
		Bundled:  review.Bundled,
	}
	switch name {
	case Node:
		pin.Install.Payload = pathMembers(NodePayload)
		pin.Install.Links = NodeLinks
	case GH:
		pin.Install.Payload = pathMembers(GHPayload)
		pin.Install.Links = nil
	case Agy:
		// agy's shape is hash-only and its writer is not ported yet (#182).
		return nil, refuse("%s: no pin writer builds this artifact", name)
	}
	pin.Authentication = Authentication{OpenPGP: review.OpenPGP, Sigstore: review.Sigstore}
	return documentOf(pin).Marshal()
}

// pathMembers publishes a whole reviewed path, which is how Node's and the GitHub
// CLI's payloads are spelled: the archive member and the published path are one.
func pathMembers(paths []RelPath) []Member {
	members := make([]Member, 0, len(paths))
	for _, path := range paths {
		members = append(members, Member{Archive: path, Published: path})
	}
	return members
}

func upstreamProject(name Name) string {
	if name == Node {
		return "https://nodejs.org/dist"
	}
	return "https://github.com/" + GHPolicy.Repository
}

// NodeReason and GHReason are the reviewer's justification the pin records: the
// selection authority for each artifact, in the words a reviewer signs off.
const NodeReason = "The signed manifest is the selection authority: the build verifies the detached " +
	"signature over SHASUMS256.txt against the committed release key, requires the pinned " +
	"issuer fingerprint, and refuses unless the signed line for the archive equals the " +
	"committed digest before the archive is fetched. The npm, npx and corepack commands " +
	"are the ones bundled in this reviewed archive; there is no separate npm pin."

// GHReason is the GitHub CLI's justification.
const GHReason = "cli/cli publishes no detached OpenPGP signature or standalone Sigstore bundle; its " +
	"SLSA provenance is readable only by `gh attestation verify`, and the GitHub CLI is the " +
	"artifact being installed, so the authoritative verification is the pin-time one " +
	"recorded here and the build's check binds the committed bundle to the committed " +
	"archive with the installed verifier. One statement covers every release asset, so the " +
	"two committed bundle files carry the same bytes; each platform entry still hashes its " +
	"own copy. The committed checksums file is the pin-time cross-check that the arch line " +
	"equals the committed digest, and its bytes are committed so an upstream rewrite is " +
	"visible; ordinary builds do not fetch it, because the bundle proves more."

// GHEvidence is where a reviewer can re-read the GitHub CLI's provenance.
func GHEvidence(version Version) []string {
	return []string{
		"https://github.com/" + GHPolicy.Repository + "/releases/tag/" + ReleaseTag(GH, version),
		"https://github.com/" + GHPolicy.Repository + "/attestations",
		"https://github.com/" + GHPolicy.Repository + "/blob/trunk/.github/workflows/deployment.yml",
	}
}

// ChecksumsURL and ManifestURL are the two upstream records the pin names for the
// selection, computed from the version like every other URL the pin records.
func ChecksumsURL(name Name, version Version) string {
	return "https://github.com/" + GHPolicy.Repository + "/releases/download/" +
		ReleaseTag(name, version) + "/" + name.String() + "_" + version.String() + "_checksums.txt"
}

// SHASUMSURL is Node's signed checksums manifest for one release.
func SHASUMSURL(version Version) string {
	return "https://nodejs.org/dist/v" + version.String() + "/SHASUMS256.txt"
}

// LabelPrefix prefixes the Dockerfile label that names a pinned version.
const LabelPrefix = "org.agent-vm.version."

// RenderLabel rewrites the one label that names name's version. A Dockerfile that
// declares it twice, or not at all, is refused: the image would otherwise report a
// version the pin does not select.
func RenderLabel(dockerfile string, name Name, version Version) (string, error) {
	key := LabelPrefix + name.String()
	lines := strings.Split(dockerfile, "\n")
	found := -1
	for index, line := range lines {
		if !strings.HasPrefix(line, "LABEL "+key+"=") {
			continue
		}
		if found >= 0 {
			return "", refuse("images/Dockerfile must declare exactly one `LABEL %s=`, found 2", key)
		}
		found = index
	}
	if found < 0 {
		return "", refuse("images/Dockerfile must declare exactly one `LABEL %s=`, found 0", key)
	}
	lines[found] = "LABEL " + key + `="` + version.String() + `"`
	return strings.Join(lines, "\n"), nil
}
