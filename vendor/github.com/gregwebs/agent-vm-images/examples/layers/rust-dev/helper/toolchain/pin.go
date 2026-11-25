package toolchain

// The pin model and its loader. Every refusal is checked in the source reader's
// order, because both readers must report the same first refusal: the trust
// root, then the directory audit, then digests, then the signature, then the
// manifest.

import (
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/strictjson"
)

// Content is one committed upstream object: where it comes from and the digest
// of the bytes the pin names.
type Content struct {
	URL    CanonicalURL
	Digest Sha256
}

// Evidence is a pin-directory file and its digest.
type Evidence struct {
	Path   EvidencePath
	Digest Sha256
}

// ComponentArchive is one selected component archive.
type ComponentArchive struct {
	Package      string
	Target       TargetTriple
	Content      Content
	MemberPrefix MemberPrefix
	Signature    Evidence
}

// HostClosure is one architecture's selected closure.
type HostClosure struct {
	Arch              Architecture
	Host              TargetTriple
	Musl              TargetTriple
	Bootstrap         Content
	BootstrapChecksum Evidence
	Archives          []ComponentArchive
}

// BootstrapException is the reviewed hash-only exception for rustup-init: the
// bootstrap binary has no published signature, so its URL and digest are
// reviewed here instead.
type BootstrapException struct {
	ReviewedOn   IsoDate
	Reviewer     string
	Reason       string
	Evidence     []string
	ReassessWhen string
	Scope        map[string]Content
}

// Pin is the decoded, validated pin.
type Pin struct {
	Channel            Channel
	ReleaseDate        IsoDate
	Profile            string
	RustupVersion      Semver
	Primary            Fingerprint
	Signers            []Fingerprint
	KeySha256          Sha256
	Manifest           Content
	ManifestChecksum   Content
	ManifestSignature  Content
	Closures           [2]HostClosure
	BootstrapException BootstrapException
}

// Closure is one architecture's closure. Both platforms are required to be
// present, so this cannot fail.
func (p Pin) Closure(arch Architecture) HostClosure { return p.Closures[int(arch)] }

// RequiredFiles is the pin directory's exact file set, sorted.
func (p Pin) RequiredFiles() []string {
	paths := []string{PinFile, ManifestPath, ManifestChecksumPath, ManifestSignaturePath, KeyPath}
	for _, closure := range p.Closures {
		paths = append(paths, string(closure.BootstrapChecksum.Path))
		for _, archive := range closure.Archives {
			paths = append(paths, string(archive.Signature.Path))
		}
	}
	sort.Strings(paths)
	return paths
}

// --- canonical origins --------------------------------------------------------

// SelectedPair is one selected (package, target) pair.
type SelectedPair struct {
	Package string
	Target  TargetTriple
}

// SelectedPairs is the reviewed selection for one architecture, in canonical
// order: the minimal profile's host packages, the explicit clippy and rustfmt,
// and the musl standard library.
func SelectedPairs(arch Architecture) []SelectedPair {
	return []SelectedPair{
		{"cargo", arch.Host()},
		{"clippy-preview", arch.Host()},
		{"rust-std", arch.Host()},
		{"rust-std", arch.Musl()},
		{"rustc", arch.Host()},
		{"rustfmt-preview", arch.Host()},
	}
}

// ArchiveFilename is the distribution archive's file name.
func ArchiveFilename(channel Channel, pkg string, target TargetTriple) string {
	return fmt.Sprintf("%s-%s-%s.tar.xz", FilenameStems[pkg], channel, target)
}

// ExpectedArchiveURL is the canonical dated origin of one selected archive.
func ExpectedArchiveURL(channel Channel, release IsoDate, pkg string,
	target TargetTriple) string {
	return fmt.Sprintf("%s/dist/%s/%s", DistServer, release, ArchiveFilename(channel, pkg, target))
}

// ExpectedSignaturePath is the evidence path of one archive signature.
func ExpectedSignaturePath(arch Architecture, pkg string, target TargetTriple) string {
	suffix := ""
	if strings.HasSuffix(string(target), "-musl") {
		suffix = "-musl"
	}
	return fmt.Sprintf("%s/%s-%s%s.asc", EvidenceDir, arch.Value(), pkg, suffix)
}

// ExpectedBootstrapURL is the versioned rustup-init origin.
func ExpectedBootstrapURL(version Semver, arch Architecture) string {
	return fmt.Sprintf("%s/rustup/archive/%s/%s/rustup-init", DistServer, version, arch.Host())
}

// ExpectedBootstrapChecksumPath is the evidence path of the bootstrap checksum.
func ExpectedBootstrapChecksumPath(arch Architecture) string {
	return fmt.Sprintf("%s/rustup-%s.sha256", EvidenceDir, arch.Value())
}

// PinContent reads one {url, sha256} pair.
func pinContent(node any, what string) (Content, error) {
	url, err := fieldOf(node, "url", what)
	if err != nil {
		return Content{}, err
	}
	digest, err := fieldOf(node, "sha256", what)
	if err != nil {
		return Content{}, err
	}
	parsedURL, err := ParseURL(url, what+".url", nil)
	if err != nil {
		return Content{}, err
	}
	parsedDigest, err := ParseSha256(digest, what+".sha256")
	if err != nil {
		return Content{}, err
	}
	return Content{URL: parsedURL, Digest: parsedDigest}, nil
}

// pinEvidence reads one {path, sha256} pair.
func pinEvidence(node any, what string) (Evidence, error) {
	path, err := fieldOf(node, "path", what)
	if err != nil {
		return Evidence{}, err
	}
	digest, err := fieldOf(node, "sha256", what)
	if err != nil {
		return Evidence{}, err
	}
	parsedPath, err := ParseEvidencePath(path, what+".path")
	if err != nil {
		return Evidence{}, err
	}
	parsedDigest, err := ParseSha256(digest, what+".sha256")
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{Path: parsedPath, Digest: parsedDigest}, nil
}

// LoadPin decodes and validates directory/pin.json against policy.
func LoadPin(directory string, policy TrustPolicy) (Pin, error) {
	var pin Pin
	pinFile := filepath.Join(directory, PinFile)
	info, err := os.Lstat(pinFile)
	if err != nil || !info.Mode().IsRegular() {
		return pin, refuse("missing input: %s", PinFile)
	}
	raw, err := os.ReadFile(pinFile)
	if err != nil {
		return pin, refuse("missing input: %s", PinFile)
	}
	document, err := decodeJSON(raw, PinFile)
	if err != nil {
		return pin, err
	}
	root, err := requireKeys(document, []string{"schema", "channel", "release_date", "profile",
		"rustup_version", "trust", "manifest", "platforms", "bootstrap_exception"}, nil, PinFile)
	if err != nil {
		return pin, err
	}
	schema, err := fieldOf(root, "schema", PinFile)
	if err != nil {
		return pin, err
	}
	number, err := exactInt(schema, "schema")
	if err != nil {
		return pin, err
	}
	if number.Cmp(big.NewInt(1)) != 0 {
		return pin, refuse("%s: unsupported schema", PinFile)
	}
	channelNode, err := fieldOf(root, "channel", PinFile)
	if err != nil {
		return pin, err
	}
	if pin.Channel, err = ParseChannel(channelNode, "channel"); err != nil {
		return pin, err
	}
	dateNode, err := fieldOf(root, "release_date", PinFile)
	if err != nil {
		return pin, err
	}
	if pin.ReleaseDate, err = ParseDate(dateNode, "release_date"); err != nil {
		return pin, err
	}
	profileNode, err := fieldOf(root, "profile", PinFile)
	if err != nil {
		return pin, err
	}
	profile, err := exactTextField(profileNode, "profile")
	if err != nil {
		return pin, err
	}
	if profile != Profile {
		return pin, refuse("%s: profile must be %s", PinFile, Profile)
	}
	pin.Profile = profile
	versionNode, err := fieldOf(root, "rustup_version", PinFile)
	if err != nil {
		return pin, err
	}
	if pin.RustupVersion, err = ParseSemver(versionNode, "rustup_version"); err != nil {
		return pin, err
	}
	if !contains(SupportedRustupVersions, string(pin.RustupVersion)) {
		return pin, refuse("%s: rustup %s is not an audited installer; only %s has a "+
			"reviewed format/trust policy, so a new version needs code and policy review "+
			"rather than the existing record", PinFile, pin.RustupVersion,
			strings.Join(SupportedRustupVersions, ", "))
	}

	trustNode, err := fieldOf(root, "trust", PinFile)
	if err != nil {
		return pin, err
	}
	trust, err := requireKeys(trustNode, []string{"key", "primary_fingerprint",
		"signing_fingerprints"}, nil, "trust")
	if err != nil {
		return pin, err
	}
	keyNode, err := fieldOf(trust, "key", "trust")
	if err != nil {
		return pin, err
	}
	key, err := requireKeys(keyNode, []string{"path", "sha256"}, nil, "trust.key")
	if err != nil {
		return pin, err
	}
	keyEvidence, err := pinEvidence(key, "trust.key")
	if err != nil {
		return pin, err
	}
	pin.KeySha256 = keyEvidence.Digest
	if string(keyEvidence.Path) != KeyPath {
		return pin, refuse("%s: key path must be %s", PinFile, KeyPath)
	}
	primaryNode, err := fieldOf(trust, "primary_fingerprint", "trust")
	if err != nil {
		return pin, err
	}
	if pin.Primary, err = ParseFingerprint(primaryNode, "trust.primary_fingerprint"); err != nil {
		return pin, err
	}
	signersNode, err := fieldOf(trust, "signing_fingerprints", "trust")
	if err != nil {
		return pin, err
	}
	signers, err := stringList(signersNode, "trust.signing_fingerprints")
	if err != nil {
		return pin, err
	}
	pin.Signers = make([]Fingerprint, 0, len(signers))
	for _, signer := range signers {
		parsed, err := ParseFingerprint(signer, "trust.signing_fingerprints")
		if err != nil {
			return pin, err
		}
		pin.Signers = append(pin.Signers, parsed)
	}
	if len(pin.Signers) == 0 {
		return pin, refuse("%s: trust.signing_fingerprints must not be empty", PinFile)
	}
	if err := requireReviewedTrust(pin.Primary, pin.Signers, policy); err != nil {
		return pin, err
	}

	manifestNode, err := fieldOf(root, "manifest", PinFile)
	if err != nil {
		return pin, err
	}
	manifest, err := requireKeys(manifestNode, []string{"url", "path", "sha256", "checksum",
		"signature"}, nil, "manifest")
	if err != nil {
		return pin, err
	}
	if pin.Manifest, err = pinContent(manifest, "manifest"); err != nil {
		return pin, err
	}
	if string(pin.Manifest.URL) != fmt.Sprintf("%s/dist/channel-rust-%s.toml", DistServer,
		pin.Channel) {
		return pin, refuse("%s: manifest URL is not the canonical channel manifest origin", PinFile)
	}
	checksumNode, err := fieldOf(manifest, "checksum", "manifest")
	if err != nil {
		return pin, err
	}
	signatureNode, err := fieldOf(manifest, "signature", "manifest")
	if err != nil {
		return pin, err
	}
	checksum, err := requireKeys(checksumNode, []string{"url", "path", "sha256"}, nil,
		"manifest.checksum")
	if err != nil {
		return pin, err
	}
	signature, err := requireKeys(signatureNode, []string{"url", "path", "sha256"}, nil,
		"manifest.signature")
	if err != nil {
		return pin, err
	}
	if pin.ManifestChecksum, err = pinContent(checksum, "manifest.checksum"); err != nil {
		return pin, err
	}
	if pin.ManifestSignature, err = pinContent(signature, "manifest.signature"); err != nil {
		return pin, err
	}
	if string(pin.ManifestChecksum.URL) != string(pin.Manifest.URL)+".sha256" {
		return pin, refuse("%s: manifest checksum URL is not <manifest>.sha256", PinFile)
	}
	if string(pin.ManifestSignature.URL) != string(pin.Manifest.URL)+".asc" {
		return pin, refuse("%s: manifest signature URL is not <manifest>.asc", PinFile)
	}
	manifestPath, err := pathField(manifest, "path", "manifest")
	if err != nil {
		return pin, err
	}
	if manifestPath != ManifestPath {
		return pin, refuse("%s: manifest path must be %s", PinFile, ManifestPath)
	}
	checksumPath, err := pathField(checksum, "path", "manifest.checksum")
	if err != nil {
		return pin, err
	}
	if checksumPath != ManifestChecksumPath {
		return pin, refuse("%s: manifest checksum path must be %s", PinFile, ManifestChecksumPath)
	}
	signaturePath, err := pathField(signature, "path", "manifest.signature")
	if err != nil {
		return pin, err
	}
	if signaturePath != ManifestSignaturePath {
		return pin, refuse("%s: manifest signature path must be %s", PinFile, ManifestSignaturePath)
	}

	platformsNode, err := fieldOf(root, "platforms", PinFile)
	if err != nil {
		return pin, err
	}
	platforms, ok := platformsNode.(map[string]any)
	if !ok || !sameSet(keysOf(platforms), platformNames()) {
		return pin, refuse("%s: platforms must be exactly linux/amd64 and linux/arm64", PinFile)
	}
	for _, arch := range Architectures() {
		what := "platforms." + arch.Platform()
		node, err := requireKeys(platforms[arch.Platform()], []string{"host", "musl",
			"bootstrap", "archives"}, nil, what)
		if err != nil {
			return pin, err
		}
		closure := HostClosure{Arch: arch}
		hostNode, err := fieldOf(node, "host", what)
		if err != nil {
			return pin, err
		}
		if closure.Host, err = ParseTriple(hostNode, what+".host"); err != nil {
			return pin, err
		}
		muslNode, err := fieldOf(node, "musl", what)
		if err != nil {
			return pin, err
		}
		if closure.Musl, err = ParseTriple(muslNode, what+".musl"); err != nil {
			return pin, err
		}
		if closure.Host != arch.Host() || closure.Musl != arch.Musl() {
			return pin, refuse("%s: host/musl triples do not match the architecture", what)
		}
		bootstrapNode, err := fieldOf(node, "bootstrap", what)
		if err != nil {
			return pin, err
		}
		bootstrap, err := requireKeys(bootstrapNode, []string{"url", "sha256", "checksum"}, nil,
			what+".bootstrap")
		if err != nil {
			return pin, err
		}
		if closure.Bootstrap, err = pinContent(bootstrap, what+".bootstrap"); err != nil {
			return pin, err
		}
		if string(closure.Bootstrap.URL) != ExpectedBootstrapURL(pin.RustupVersion, arch) {
			return pin, refuse("%s: bootstrap URL is not the versioned rustup-init origin %s",
				what, ExpectedBootstrapURL(pin.RustupVersion, arch))
		}
		bootstrapChecksumNode, err := fieldOf(bootstrap, "checksum", what+".bootstrap")
		if err != nil {
			return pin, err
		}
		bootstrapChecksum, err := requireKeys(bootstrapChecksumNode, []string{"path", "sha256"},
			nil, what+".bootstrap.checksum")
		if err != nil {
			return pin, err
		}
		if closure.BootstrapChecksum, err = pinEvidence(bootstrapChecksum,
			what+".bootstrap.checksum"); err != nil {
			return pin, err
		}
		if string(closure.BootstrapChecksum.Path) != ExpectedBootstrapChecksumPath(arch) {
			return pin, refuse("%s: bootstrap checksum evidence path must be %s", what,
				ExpectedBootstrapChecksumPath(arch))
		}
		archivesNode, err := fieldOf(node, "archives", what)
		if err != nil {
			return pin, err
		}
		archives, ok := archivesNode.([]any)
		if !ok {
			return pin, refuse("%s.archives: expected a list", what)
		}
		pairs := make([][2]string, 0, len(archives))
		for _, rawArchive := range archives {
			archiveNode, err := requireKeys(rawArchive, []string{"package", "target", "url",
				"sha256", "member_prefix", "signature"}, nil, what+".archives")
			if err != nil {
				return pin, err
			}
			packageNode, err := fieldOf(archiveNode, "package", what+".archives")
			if err != nil {
				return pin, err
			}
			pkg, err := exactTextField(packageNode, what+".archives.package")
			if err != nil {
				return pin, err
			}
			if !contains(Packages, pkg) {
				return pin, refuse("%s: unexpected selected archive %s", what,
					strictjson.Repr(pkg))
			}
			targetNode, err := fieldOf(archiveNode, "target", what+".archives")
			if err != nil {
				return pin, err
			}
			target, err := ParseTriple(targetNode, what+"."+pkg+".target")
			if err != nil {
				return pin, err
			}
			content, err := pinContent(archiveNode, what+"."+pkg)
			if err != nil {
				return pin, err
			}
			if string(content.URL) != ExpectedArchiveURL(pin.Channel, pin.ReleaseDate, pkg, target) {
				return pin, refuse("%s.%s: archive URL is not the canonical dated origin", what, pkg)
			}
			prefixNode, err := fieldOf(archiveNode, "member_prefix", what+".archives")
			if err != nil {
				return pin, err
			}
			prefix, err := ParseMemberPrefix(prefixNode, what+"."+pkg+".member_prefix")
			if err != nil {
				return pin, err
			}
			stem := ArchiveFilename(pin.Channel, pkg, target)
			if string(prefix) != stem[:len(stem)-len(".tar.xz")]+"/" {
				return pin, refuse("%s.%s: member prefix does not match the archive name", what, pkg)
			}
			signatureNode, err := fieldOf(archiveNode, "signature", what+".archives")
			if err != nil {
				return pin, err
			}
			signature, err := requireKeys(signatureNode, []string{"path", "sha256"}, nil,
				what+"."+pkg+".signature")
			if err != nil {
				return pin, err
			}
			evidence, err := pinEvidence(signature, what+"."+pkg+".signature")
			if err != nil {
				return pin, err
			}
			if string(evidence.Path) != ExpectedSignaturePath(arch, pkg, target) {
				return pin, refuse("%s.%s: signature evidence path must be %s", what, pkg,
					ExpectedSignaturePath(arch, pkg, target))
			}
			closure.Archives = append(closure.Archives, ComponentArchive{Package: pkg,
				Target: target, Content: content, MemberPrefix: prefix, Signature: evidence})
			pairs = append(pairs, [2]string{pkg, string(target)})
		}
		if err := requireSelection(arch, pairs); err != nil {
			return pin, err
		}
		pin.Closures[int(arch)] = closure
	}

	exceptionNode, err := fieldOf(root, "bootstrap_exception", PinFile)
	if err != nil {
		return pin, err
	}
	exception, err := requireKeys(exceptionNode, []string{"method", "reviewed_on", "reviewer",
		"scope", "reason", "evidence", "reassess_when"}, nil, "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	methodNode, err := fieldOf(exception, "method", "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	method, err := exactTextField(methodNode, "bootstrap_exception.method")
	if err != nil {
		return pin, err
	}
	if method != "reviewed-hash-only" {
		return pin, refuse("%s: bootstrap_signature policy must be the reviewed hash-only "+
			"exception", PinFile)
	}
	scopeNode, err := fieldOf(exception, "scope", "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	scopeTable, ok := scopeNode.(map[string]any)
	if !ok || !sameSet(keysOf(scopeTable), platformNames()) {
		return pin, refuse("bootstrap_exception.scope: must cover exactly linux/amd64 and " +
			"linux/arm64")
	}
	scope := make(map[string]Content, len(platformNames()))
	for _, arch := range Architectures() {
		what := "bootstrap_exception.scope." + arch.Platform()
		entry, err := requireKeys(scopeTable[arch.Platform()], []string{"url", "sha256"}, nil,
			what)
		if err != nil {
			return pin, err
		}
		content, err := pinContent(entry, what)
		if err != nil {
			return pin, err
		}
		if content != pin.Closure(arch).Bootstrap {
			return pin, refuse("%s: exception is not bound to the committed bootstrap URL "+
				"and digest", what)
		}
		scope[arch.Platform()] = content
	}
	reviewerNode, err := fieldOf(exception, "reviewer", "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	reviewer, err := exactTextField(reviewerNode, "bootstrap_exception.reviewer")
	if err != nil {
		return pin, err
	}
	reasonNode, err := fieldOf(exception, "reason", "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	reason, err := exactTextField(reasonNode, "bootstrap_exception.reason")
	if err != nil {
		return pin, err
	}
	reassessNode, err := fieldOf(exception, "reassess_when", "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	reassess, err := exactTextField(reassessNode, "bootstrap_exception.reassess_when")
	if err != nil {
		return pin, err
	}
	evidenceNode, err := fieldOf(exception, "evidence", "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	evidence, err := stringList(evidenceNode, "bootstrap_exception.evidence")
	if err != nil {
		return pin, err
	}
	for _, value := range []string{reviewer, reason, reassess} {
		if err := RejectControlCharacters(value, "bootstrap_exception"); err != nil {
			return pin, err
		}
	}
	if !isReviewerHandle(reviewer) {
		return pin, refuse("bootstrap_exception.reviewer: expected a simple reviewer handle")
	}
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(reassess) == "" ||
		len(evidence) == 0 {
		return pin, refuse("bootstrap_exception: reason, reassess_when and evidence must be " +
			"recorded")
	}
	reviewedOnNode, err := fieldOf(exception, "reviewed_on", "bootstrap_exception")
	if err != nil {
		return pin, err
	}
	reviewedOn, err := ParseDate(reviewedOnNode, "bootstrap_exception.reviewed_on")
	if err != nil {
		return pin, err
	}
	pin.BootstrapException = BootstrapException{ReviewedOn: reviewedOn, Reviewer: reviewer,
		Reason: reason, Evidence: evidence, ReassessWhen: reassess, Scope: scope}
	return pin, nil
}

// isReviewerHandle matches `[A-Za-z0-9_.-]{1,64}` in full.
func isReviewerHandle(text string) bool {
	if len(text) < 1 || len(text) > MaxReviewerHandleRunes {
		return false
	}
	for _, b := range []byte(text) {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
			b == '_' || b == '.' || b == '-' {
			continue
		}
		return false
	}
	return true
}

// requireSelection applies the duplicate, unknown, missing and order rules in
// the source reader's order.
func requireSelection(arch Architecture, pairs [][2]string) error {
	seen := make(map[[2]string]bool, len(pairs))
	for _, pair := range pairs {
		if seen[pair] {
			return refuse("%s: duplicate selected archive", arch.Platform())
		}
		seen[pair] = true
	}
	want := make([][2]string, 0, 6)
	for _, pair := range SelectedPairs(arch) {
		want = append(want, [2]string{pair.Package, string(pair.Target)})
	}
	var unknown [][2]string
	for _, pair := range pairs {
		if !containsPair(want, pair) {
			unknown = append(unknown, pair)
		}
	}
	sortPairs(unknown)
	if len(unknown) != 0 {
		return refuse("%s: unexpected selected archive %s %s", arch.Platform(), unknown[0][0],
			unknown[0][1])
	}
	for _, pair := range want {
		if !containsPair(pairs, pair) {
			return refuse("%s: selected closure is incomplete: missing %s %s",
				arch.Platform(), pair[0], pair[1])
		}
	}
	for index, pair := range want {
		if pairs[index] != pair {
			return refuse("%s: selected archives must be in canonical order", arch.Platform())
		}
	}
	return nil
}

func containsPair(pairs [][2]string, pair [2]string) bool {
	for _, candidate := range pairs {
		if candidate == pair {
			return true
		}
	}
	return false
}

func sortPairs(pairs [][2]string) {
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})
}

func keysOf(table map[string]any) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func platformNames() []string {
	names := make([]string, 0, 2)
	for _, arch := range Architectures() {
		names = append(names, arch.Platform())
	}
	sort.Strings(names)
	return names
}

func sameSet(left, right []string) bool {
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

// pathField reads one string member that must equal a compiled path.
func pathField(node any, key, what string) (string, error) {
	value, err := fieldOf(node, key, what)
	if err != nil {
		return "", err
	}
	return exactTextField(value, what)
}
