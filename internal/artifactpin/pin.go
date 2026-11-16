// Package artifactpin reads the committed third-party release-artifact pins.
//
// A pin names an exact upstream release, one archive per supported architecture
// with its reviewed SHA-256, the archive's own top-level prefix, what to publish
// and which authentication evidence binds the bytes. There are three artifacts —
// Node, the GitHub CLI and the Google Antigravity CLI (agy) — and the shape is one
// shape on purpose: the installer, the maintenance writer and the offline suite
// read the same record, so a pin the gate accepts is a pin those three can act on.
//
// The loader is strict rather than lenient. Every mapping has an exact key set, so
// an unknown field, a missing platform, an upper-case digest or a `latest` version
// is a refusal instead of a default; the document is decoded once, refusing a
// duplicate key. Nothing here proves a release is safe: it proves which bytes were
// selected and reviewed.
package artifactpin

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Method names how a pin's bytes are authenticated.
type Method string

const (
	// MethodOpenPGP is a detached signature over a checksums manifest.
	MethodOpenPGP Method = "openpgp-signed-manifest"
	// MethodSigstore is a GitHub attestation bundle.
	MethodSigstore Method = "sigstore-attestation"
	// MethodHashOnly is the agy case: no upstream signature exists.
	MethodHashOnly Method = "hash-only"
)

// Kind is the only pin kind the model understands.
const KindReleaseArchive = "release-archive"

// Upstream names the project and the exact release tag.
type Upstream struct {
	Project string
	Release string
}

// Member is one published payload entry: an archive member and where it lands,
// relative to the install destination.
type Member struct {
	Archive   RelPath
	Published RelPath
}

// Link is one published symlink.
type Link struct {
	Path   Path
	Target Path
}

// Install is what a pin publishes.
type Install struct {
	Destination Path
	Payload     []Member
	Links       []Link
}

// Bundle is one gh archive's committed Sigstore bundle.
type Bundle struct {
	Path   RelPath
	Digest Digest
}

// Archive is one architecture's reviewed release archive.
type Archive struct {
	Platform     Platform
	URL          string
	Format       ArchiveFormat
	MemberPrefix string
	Digest       Digest
	Bundle       *Bundle // gh only
	SHA512       SHA512  // agy only
}

// Filename is the asset name the archive's URL names.
func (a Archive) Filename() string {
	if index := strings.LastIndex(a.URL, "/"); index >= 0 {
		return a.URL[index+1:]
	}
	return a.URL
}

// Policy is the reviewed Sigstore identity for the GitHub CLI.
type Policy struct {
	Repository            string
	SignerWorkflow        string
	CertOIDCIssuer        string
	DenySelfHostedRunners bool
}

// Evidence is one committed corroboration file: where it came from, where it is
// committed and the digest that binds the committed bytes.
type Evidence struct {
	URL    string
	Path   RelPath
	Digest Digest
}

// OpenPGP authenticates a release by a detached signature over a checksums
// manifest, verified against a committed key.
type OpenPGP struct {
	ManifestURL     string
	ManifestDigest  Digest
	SignatureURL    string
	SignatureDigest Digest
	KeyPath         RelPath
	KeyFingerprint  string
	KeyDigest       Digest
	VerifiedOn      string
	Reason          string
}

// Sigstore authenticates a release by a GitHub attestation bundle.
type Sigstore struct {
	ChecksumsURL    string
	ChecksumsDigest Digest
	Policy          Policy
	VerifiedOn      string
	Reason          string
	Evidence        []string
}

// HashOnly is the agy case: no upstream signature exists, so the selection rests
// on two records that had to agree at pin time and are re-checked against each
// other at build time.
type HashOnly struct {
	Release    Evidence
	Manifests  map[Platform]Evidence
	VerifiedOn string
	Reason     string
}

// Authentication is the one of the three a pin declares.
type Authentication struct {
	Method   Method
	OpenPGP  *OpenPGP
	Sigstore *Sigstore
	HashOnly *HashOnly
}

// Reason is the reviewer's justification for the selected evidence.
func (a Authentication) Reason() string {
	switch {
	case a.OpenPGP != nil:
		return a.OpenPGP.Reason
	case a.Sigstore != nil:
		return a.Sigstore.Reason
	case a.HashOnly != nil:
		return a.HashOnly.Reason
	}
	return ""
}

// Pin is one committed artifact pin.
type Pin struct {
	// Directory is the pin's own directory: evidence paths are resolved here.
	Directory      string
	Name           Name
	Version        Version
	Upstream       Upstream
	Install        Install
	Archives       map[Platform]Archive
	Authentication Authentication
	// Bundled records what the reviewed archive ships, which is the only thing
	// the image may report: node's npm.
	Bundled map[string]Version
}

// Archive is one platform's reviewed archive.
func (p Pin) Archive(platform Platform) (Archive, error) {
	archive, ok := p.Archives[platform]
	if !ok {
		return Archive{}, refuse("%s: the pin has no %s archive", p.Name, platform)
	}
	return archive, nil
}

// Reason is the reviewer's justification, recorded by the method the pin names.
func (p Pin) Reason() string { return p.Authentication.Reason() }

// Load reads one committed pin, named by its file name: images/artifacts/<name>.json
// for a base artifact, images/tools/<name>/pin.json for an agent's own pin.
func Load(path string) (Pin, error) {
	if filepath.Ext(path) != ".json" {
		return Pin{}, refuse("%s: a pin is a .json file", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Pin{}, err
	}
	directory := filepath.Dir(path)
	name := strings.TrimSuffix(filepath.Base(path), ".json")
	if filepath.Base(path) == "pin.json" {
		name = filepath.Base(directory)
	}
	return Parse(raw, directory, name)
}

// Parse decodes and fully validates one committed pin named by its file name.
func Parse(raw []byte, directory, nameText string) (Pin, error) {
	name, err := parseName(nameText)
	if err != nil {
		return Pin{}, err
	}
	// The pin itself goes through the duplicate-rejecting decoder, not a
	// last-key-wins one: a pin could otherwise say one version to a reviewer's
	// reader and another to the installer.
	document, err := strictJSON(raw, name.String()+": pin")
	if err != nil {
		return Pin{}, err
	}
	keys := []string{"schema", "name", "kind", "version", "upstream", "install", "platforms"}
	if name == Node {
		keys = append(keys, "bundled")
	}
	keys = append(keys, "authentication")
	pin, err := fields(document, keys, name.String()+": pin")
	if err != nil {
		return Pin{}, err
	}
	schema, err := requireInteger(pin["schema"], name.String()+": pin schema")
	if err != nil {
		return Pin{}, err
	}
	if schema != Schema {
		return Pin{}, refuse("%s: unsupported schema %d", name, schema)
	}
	declared, err := requireText(pin["name"], name.String()+": pin name")
	if err != nil {
		return Pin{}, err
	}
	if declared != name.String() {
		return Pin{}, refuse("%s: the pin names artifact %q", name, declared)
	}
	kind, err := requireText(pin["kind"], name.String()+": kind")
	if err != nil {
		return Pin{}, err
	}
	if kind != KindReleaseArchive {
		return Pin{}, refuse("%s: kind must be %s", name, KindReleaseArchive)
	}
	versionText, err := requireText(pin["version"], name.String()+": version")
	if err != nil {
		return Pin{}, err
	}
	version, err := ParseVersion(versionText, name.String()+": version")
	if err != nil {
		return Pin{}, err
	}
	upstream, err := parseUpstream(name, version, pin["upstream"])
	if err != nil {
		return Pin{}, err
	}
	install, err := parseInstall(name, pin["install"])
	if err != nil {
		return Pin{}, err
	}
	archives, err := parsePlatforms(name, version, pin["platforms"])
	if err != nil {
		return Pin{}, err
	}
	bundled, err := parseBundled(name, pin["bundled"])
	if err != nil {
		return Pin{}, err
	}
	authentication, err := parseAuthentication(name, version, pin["authentication"])
	if err != nil {
		return Pin{}, err
	}
	return Pin{
		Directory:      directory,
		Name:           name,
		Version:        version,
		Upstream:       upstream,
		Install:        install,
		Archives:       archives,
		Authentication: authentication,
		Bundled:        bundled,
	}, nil
}

func parseUpstream(name Name, version Version, value object) (Upstream, error) {
	what := name.String() + ": upstream"
	members, err := fields(value, []string{"project", "release"}, what)
	if err != nil {
		return Upstream{}, err
	}
	project, err := requireText(members["project"], name.String()+": project")
	if err != nil {
		return Upstream{}, err
	}
	release, err := requireText(members["release"], name.String()+": release")
	if err != nil {
		return Upstream{}, err
	}
	tag := ReleaseTag(name, version)
	if release != tag {
		return Upstream{}, refuse("%s: upstream release must be %s, got %q", name, tag, release)
	}
	return Upstream{Project: project, Release: release}, nil
}

func parseInstall(name Name, value object) (Install, error) {
	what := name.String() + ": install"
	members, err := fields(value, []string{"destination", "payload", "links"}, what)
	if err != nil {
		return Install{}, err
	}
	destinationText, err := requireText(members["destination"], name.String()+": destination")
	if err != nil {
		return Install{}, err
	}
	destination, err := parsePath(destinationText, name.String()+": destination")
	if err != nil {
		return Install{}, err
	}
	if destination != "/usr/local" {
		return Install{}, refuse("%s: destination must be /usr/local, got %s", name, destination)
	}
	payload, err := parsePayload(name, members["payload"])
	if err != nil {
		return Install{}, err
	}
	links, err := parseLinks(name, members["links"])
	if err != nil {
		return Install{}, err
	}
	return Install{Destination: destination, Payload: payload, Links: links}, nil
}

func parsePayload(name Name, value object) ([]Member, error) {
	what := name.String() + ": payload"
	items, err := requireList(value, what)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, refuse("%s must be a non-empty list", what)
	}
	if name == Agy {
		pairs := make([]Member, 0, len(items))
		for _, item := range items {
			entryWhat := name.String() + ": payload entry"
			entry, err := fields(item, []string{"member", "path"}, entryWhat)
			if err != nil {
				return nil, err
			}
			memberText, err := requireText(entry["member"], name.String()+": payload member")
			if err != nil {
				return nil, err
			}
			member, err := parseRelPath(memberText, name.String()+": payload member")
			if err != nil {
				return nil, err
			}
			pathText, err := requireText(entry["path"], name.String()+": payload path")
			if err != nil {
				return nil, err
			}
			published, err := parseRelPath(pathText, name.String()+": payload path")
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, Member{Archive: member, Published: published})
		}
		if !sameMembers(pairs, AgyPayload) {
			return nil, refuse("%s must publish %s, got %s", what, describe(AgyPayload), describe(pairs))
		}
		return pairs, nil
	}
	expected := NodePayload
	if name == GH {
		expected = GHPayload
	}
	paths := make([]RelPath, 0, len(items))
	for _, item := range items {
		entryWhat := name.String() + ": payload entry"
		entry, err := requireText(item, entryWhat)
		if err != nil {
			return nil, err
		}
		path, err := parseRelPath(entry, entryWhat)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	if !samePaths(paths, expected) {
		return nil, refuse("%s must be %s, got %s", what, describePaths(expected), describePaths(paths))
	}
	pairs := make([]Member, 0, len(paths))
	for _, path := range paths {
		pairs = append(pairs, Member{Archive: path, Published: path})
	}
	return pairs, nil
}

func parseLinks(name Name, value object) ([]Link, error) {
	what := name.String() + ": links"
	items, err := requireList(value, what)
	if err != nil {
		return nil, err
	}
	links := make([]Link, 0, len(items))
	for _, item := range items {
		entryWhat := name.String() + ": link"
		entry, err := fields(item, []string{"path", "target"}, entryWhat)
		if err != nil {
			return nil, err
		}
		pathText, err := requireText(entry["path"], name.String()+": link path")
		if err != nil {
			return nil, err
		}
		path, err := parsePath(pathText, name.String()+": link path")
		if err != nil {
			return nil, err
		}
		targetText, err := requireText(entry["target"], name.String()+": link target")
		if err != nil {
			return nil, err
		}
		target, err := parsePath(targetText, name.String()+": link target")
		if err != nil {
			return nil, err
		}
		links = append(links, Link{Path: path, Target: target})
	}
	expected := []Link{}
	if name == Node {
		expected = NodeLinks
	}
	if !sameLinks(links, expected) {
		return nil, refuse("%s must be %s, got %s", what, describeLinks(expected), describeLinks(links))
	}
	return links, nil
}

func parsePlatforms(name Name, version Version, value object) (map[Platform]Archive, error) {
	keys := make([]string, 0, len(Platforms))
	for _, platform := range Platforms {
		keys = append(keys, string(platform))
	}
	members, err := fields(value, keys, name.String()+": platforms")
	if err != nil {
		return nil, err
	}
	archives := make(map[Platform]Archive, len(Platforms))
	for _, platform := range Platforms {
		archive, err := parseArchive(name, version, platform, members[string(platform)])
		if err != nil {
			return nil, err
		}
		archives[platform] = archive
	}
	return archives, nil
}

func parseArchive(name Name, version Version, platform Platform, value object) (Archive, error) {
	what := name.String() + ": " + string(platform)
	keys := []string{"url", "archive", "member_prefix", "sha256"}
	if name == GH {
		keys = append(keys, "bundle")
	}
	if name == Agy {
		keys = append(keys, "sha512")
	}
	members, err := fields(value, keys, what)
	if err != nil {
		return Archive{}, err
	}
	rawURL, err := requireText(members["url"], what+" url")
	if err != nil {
		return Archive{}, err
	}
	rawURL, err = sourceURL(rawURL, what+" url")
	if err != nil {
		return Archive{}, err
	}
	if err := checkSourceURL(name, rawURL, version, platform); err != nil {
		return Archive{}, err
	}
	filename := ExpectedFilename(name, version, platform)
	format, err := archiveFormat(filename)
	if err != nil {
		return Archive{}, err
	}
	declared, err := requireText(members["archive"], what+" archive")
	if err != nil {
		return Archive{}, err
	}
	if declared != string(format) {
		return Archive{}, refuse("%s: archive must be %s for %s", what, format, filename)
	}
	// agy's archive is prefixless, so an empty string is the reviewed value and
	// the comparison below is the check: it never has to be a non-empty string.
	prefix, _ := members["member_prefix"].(string)
	expected := expectedMemberPrefix(name, filename)
	if prefix != expected {
		return Archive{}, refuse("%s: member_prefix must be %s for %s", what, expected, filename)
	}
	digestText, err := requireText(members["sha256"], what+" sha256")
	if err != nil {
		return Archive{}, err
	}
	digest, err := ParseDigest(digestText, what+" sha256")
	if err != nil {
		return Archive{}, err
	}
	archive := Archive{
		Platform:     platform,
		URL:          rawURL,
		Format:       format,
		MemberPrefix: prefix,
		Digest:       digest,
	}
	if name == GH {
		bundle, err := parseBundle(name, version, platform, members["bundle"])
		if err != nil {
			return Archive{}, err
		}
		archive.Bundle = &bundle
	}
	if name == Agy {
		sha512Text, err := requireText(members["sha512"], what+" sha512")
		if err != nil {
			return Archive{}, err
		}
		sha512, err := parseSHA512(sha512Text, what+" sha512")
		if err != nil {
			return Archive{}, err
		}
		archive.SHA512 = sha512
	}
	return archive, nil
}

func parseBundle(name Name, version Version, platform Platform, value object) (Bundle, error) {
	what := name.String() + ": " + string(platform) + " bundle"
	members, err := fields(value, []string{"path", "sha256"}, what)
	if err != nil {
		return Bundle{}, err
	}
	pathText, err := requireText(members["path"], what+" path")
	if err != nil {
		return Bundle{}, err
	}
	path, err := parseRelPath(pathText, what+" path")
	if err != nil {
		return Bundle{}, err
	}
	expected := BundleName(name, version, platform)
	if path.String() != expected {
		return Bundle{}, refuse("%s: bundle must be %s, got %s", name.String()+": "+string(platform), expected, path)
	}
	digestText, err := requireText(members["sha256"], what+" sha256")
	if err != nil {
		return Bundle{}, err
	}
	digest, err := ParseDigest(digestText, what+" sha256")
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Path: path, Digest: digest}, nil
}

func parseBundled(name Name, value object) (map[string]Version, error) {
	if name != Node {
		return nil, nil
	}
	what := "node: bundled"
	members, err := fields(value, []string{"npm"}, what)
	if err != nil {
		return nil, err
	}
	npmText, err := requireText(members["npm"], "node: bundled npm")
	if err != nil {
		return nil, err
	}
	npm, err := ParseVersion(npmText, "node: bundled npm")
	if err != nil {
		return nil, err
	}
	return map[string]Version{"npm": npm}, nil
}

var fingerprintPattern = regexp.MustCompile(`^([0-9A-F]{40}|[0-9A-F]{64})$`)

func parseAuthentication(name Name, version Version, value object) (Authentication, error) {
	switch name {
	case Agy:
		return parseHashOnly(version, value)
	case Node:
		return parseOpenPGP(value)
	case GH:
		return parseSigstore(value)
	}
	return Authentication{}, refuse("%s: unknown artifact", name)
}

func parseHashOnly(version Version, value object) (Authentication, error) {
	what := "agy: authentication"
	members, err := fields(value,
		[]string{"method", "release", "manifests", "verified_on", "reason"}, what)
	if err != nil {
		return Authentication{}, err
	}
	method, err := requireText(members["method"], what+" method")
	if err != nil {
		return Authentication{}, err
	}
	if method != string(MethodHashOnly) {
		return Authentication{}, refuse("agy: authentication must be hash-only")
	}
	keys := make([]string, 0, len(Platforms))
	for _, platform := range Platforms {
		keys = append(keys, string(platform))
	}
	manifests, err := fields(members["manifests"], keys, "agy: manifests")
	if err != nil {
		return Authentication{}, err
	}

	release, err := committedEvidence(members["release"], "agy: release evidence",
		AgyReleaseAPI+version.String(), AgyReleaseEvidence)
	if err != nil {
		return Authentication{}, err
	}
	authentication := &HashOnly{Release: release, Manifests: map[Platform]Evidence{}}
	for _, platform := range Platforms {
		evidence, err := committedEvidence(manifests[string(platform)],
			"agy: "+string(platform)+" manifest", ManifestURL(platform), AgyEvidence[platform])
		if err != nil {
			return Authentication{}, err
		}
		authentication.Manifests[platform] = evidence
	}
	verifiedOn, err := requireText(members["verified_on"], "agy: verified_on")
	if err != nil {
		return Authentication{}, err
	}
	if err := parseDate(verifiedOn, "agy: verified_on"); err != nil {
		return Authentication{}, err
	}
	reason, err := requireText(members["reason"], "agy: reason")
	if err != nil {
		return Authentication{}, err
	}
	authentication.VerifiedOn, authentication.Reason = verifiedOn, reason
	return Authentication{Method: MethodHashOnly, HashOnly: authentication}, nil
}

func parseOpenPGP(value object) (Authentication, error) {
	what := "node: authentication"
	members, err := fields(value,
		[]string{"method", "manifest", "signature", "key", "verified_on", "reason"}, what)
	if err != nil {
		return Authentication{}, err
	}
	method, err := requireText(members["method"], what+" method")
	if err != nil {
		return Authentication{}, err
	}
	if method != string(MethodOpenPGP) {
		return Authentication{}, refuse("node: authentication must be an openpgp-signed-manifest")
	}
	manifest, err := fields(members["manifest"], []string{"url", "sha256"}, "node: manifest")
	if err != nil {
		return Authentication{}, err
	}
	signature, err := fields(members["signature"], []string{"url", "sha256"}, "node: signature")
	if err != nil {
		return Authentication{}, err
	}
	key, err := fields(members["key"], []string{"path", "fingerprint", "sha256"}, "node: key")
	if err != nil {
		return Authentication{}, err
	}
	manifestURL, err := sourceURLText(manifest["url"], "node: manifest url")
	if err != nil {
		return Authentication{}, err
	}
	manifestDigest, err := digestMember(manifest["sha256"], "node: manifest sha256")
	if err != nil {
		return Authentication{}, err
	}
	signatureURL, err := sourceURLText(signature["url"], "node: signature url")
	if err != nil {
		return Authentication{}, err
	}
	signatureDigest, err := digestMember(signature["sha256"], "node: signature sha256")
	if err != nil {
		return Authentication{}, err
	}
	keyPath, err := relativeMember(key["path"], "node: key path")
	if err != nil {
		return Authentication{}, err
	}
	fingerprintText, err := requireText(key["fingerprint"], "node: key fingerprint")
	if err != nil {
		return Authentication{}, err
	}
	fingerprint := strings.ReplaceAll(fingerprintText, " ", "")
	if !fingerprintPattern.MatchString(fingerprint) {
		return Authentication{}, refuse("node: key fingerprint must be 40 or 64 upper-case hex, got %s", fingerprint)
	}
	if fingerprint != NodeKeyFingerprint {
		return Authentication{}, refuse("node: key fingerprint must be the reviewed %s, got %s",
			NodeKeyFingerprint, fingerprint)
	}
	if keyPath != NodeKey {
		return Authentication{}, refuse("node: key path must be %s, got %s", NodeKey, keyPath)
	}
	keyDigest, err := digestMember(key["sha256"], "node: key sha256")
	if err != nil {
		return Authentication{}, err
	}
	verifiedOn, err := dateMember(members["verified_on"], "node: verified_on")
	if err != nil {
		return Authentication{}, err
	}
	reason, err := requireText(members["reason"], "node: reason")
	if err != nil {
		return Authentication{}, err
	}
	return Authentication{Method: MethodOpenPGP, OpenPGP: &OpenPGP{
		ManifestURL:     manifestURL,
		ManifestDigest:  manifestDigest,
		SignatureURL:    signatureURL,
		SignatureDigest: signatureDigest,
		KeyPath:         keyPath,
		KeyFingerprint:  fingerprint,
		KeyDigest:       keyDigest,
		VerifiedOn:      verifiedOn,
		Reason:          reason,
	}}, nil
}

func parseSigstore(value object) (Authentication, error) {
	what := "gh: authentication"
	members, err := fields(value,
		[]string{"method", "checksums", "policy", "verified_on", "reason", "evidence"}, what)
	if err != nil {
		return Authentication{}, err
	}
	method, err := requireText(members["method"], what+" method")
	if err != nil {
		return Authentication{}, err
	}
	if method != string(MethodSigstore) {
		return Authentication{}, refuse("gh: authentication must be a sigstore-attestation")
	}
	checksums, err := fields(members["checksums"], []string{"url", "sha256"}, "gh: checksums")
	if err != nil {
		return Authentication{}, err
	}
	checksumsURL, err := sourceURLText(checksums["url"], "gh: checksums url")
	if err != nil {
		return Authentication{}, err
	}
	checksumsDigest, err := digestMember(checksums["sha256"], "gh: checksums sha256")
	if err != nil {
		return Authentication{}, err
	}
	policy, err := parsePolicy(members["policy"])
	if err != nil {
		return Authentication{}, err
	}
	evidence, err := requireList(members["evidence"], "gh: evidence")
	if err != nil {
		return Authentication{}, err
	}
	if len(evidence) == 0 {
		return Authentication{}, refuse("gh: evidence must be a non-empty list of URLs")
	}
	urls := make([]string, 0, len(evidence))
	for _, item := range evidence {
		parsed, err := sourceURLText(item, "gh: evidence")
		if err != nil {
			return Authentication{}, err
		}
		urls = append(urls, parsed)
	}
	verifiedOn, err := dateMember(members["verified_on"], "gh: verified_on")
	if err != nil {
		return Authentication{}, err
	}
	reason, err := requireText(members["reason"], "gh: reason")
	if err != nil {
		return Authentication{}, err
	}
	return Authentication{Method: MethodSigstore, Sigstore: &Sigstore{
		ChecksumsURL:    checksumsURL,
		ChecksumsDigest: checksumsDigest,
		Policy:          policy,
		VerifiedOn:      verifiedOn,
		Reason:          reason,
		Evidence:        urls,
	}}, nil
}

func parsePolicy(value object) (Policy, error) {
	members, err := fields(value, []string{"repository", "signer_workflow", "cert_oidc_issuer",
		"deny_self_hosted_runners"}, "gh: policy")
	if err != nil {
		return Policy{}, err
	}
	repository, err := requireText(members["repository"], "gh: policy repository")
	if err != nil {
		return Policy{}, err
	}
	workflow, err := requireText(members["signer_workflow"], "gh: policy signer_workflow")
	if err != nil {
		return Policy{}, err
	}
	issuer, err := requireText(members["cert_oidc_issuer"], "gh: policy cert_oidc_issuer")
	if err != nil {
		return Policy{}, err
	}
	deny, err := requireBool(members["deny_self_hosted_runners"], "gh: policy deny_self_hosted_runners")
	if err != nil {
		return Policy{}, err
	}
	policy := Policy{
		Repository:            repository,
		SignerWorkflow:        workflow,
		CertOIDCIssuer:        issuer,
		DenySelfHostedRunners: deny,
	}
	if policy != GHPolicy {
		return Policy{}, refuse("gh: policy must be the reviewed %s", describePolicy(GHPolicy))
	}
	return policy, nil
}

// committedEvidence reads one corroboration file the pin names. A build never
// requests these, so there is no fixture file: exception: the recorded origin has
// to be the upstream one, and the committed path is fixed.
func committedEvidence(value object, what, expectedURL string, expectedPath RelPath) (Evidence, error) {
	members, err := fields(value, []string{"url", "path", "sha256"}, what)
	if err != nil {
		return Evidence{}, err
	}
	recordedURL, err := requireText(members["url"], what+" url")
	if err != nil {
		return Evidence{}, err
	}
	if recordedURL != expectedURL {
		return Evidence{}, refuse("%s: the url must be exactly %s, got %s", what, expectedURL, recordedURL)
	}
	recordedPath, err := relativeMember(members["path"], what+" path")
	if err != nil {
		return Evidence{}, err
	}
	if recordedPath != expectedPath {
		return Evidence{}, refuse("%s: the path must be %s, got %s", what, expectedPath, recordedPath)
	}
	digest, err := digestMember(members["sha256"], what+" sha256")
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{URL: recordedURL, Path: recordedPath, Digest: digest}, nil
}
