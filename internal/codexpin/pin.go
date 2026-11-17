// Package codexpin is the committed content identity of one exact Codex release.
//
// `images/tools/codex/pin.json` is the reviewed selection: the exact release, the
// two maintained Linux packages with their exact URLs and SHA-256 digests, the
// original release metadata, the original `codex-package_SHA256SUMS` and both
// original `.sigstore` bundles, plus the signer policy that was enforced when the pin
// was made. This package is the only reader. It is offline: nothing here resolves a
// release, downloads a byte or writes a pin.
//
// What ordinary builds prove here: the committed release, the committed evidence
// bytes (re-hashed now), the committed archive digest (re-hashed now), the package
// metadata inside the archive, and the ELF machine of `bin/codex`. They do not
// repeat cryptographic verification -- that happened once, on the maintainer host,
// through `avm-pin codex`. A well-formed `.sigstore` bundle is therefore never
// described as cryptographically verified here; it is only checked for the shape
// that makes the committed evidence a meaningful record.
package codexpin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/evidence"
	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// The reviewed schema, in the pin's own words.
const (
	Schema          = 1
	Name            = "codex"
	Kind            = "codex-standalone-package"
	Repository      = "openai/codex"
	Workflow        = "https://github.com/openai/codex/.github/workflows/rust-release.yml"
	Issuer          = "https://token.actions.githubusercontent.com"
	VerifierName    = "cosign"
	VerifierVersion = "v2.4.1"

	AuthenticationMethod = "sigstore-blob-pin-time"
	TrustRoot            = "sigstore-production-tuf"

	PackageLayoutVersion = 1
	PackageVariant       = "codex"
	PackageEntrypoint    = "bin/codex"

	MetadataPath   = "evidence/release.json"
	ChecksumsAsset = "codex-package_SHA256SUMS"
	ChecksumsPath  = "evidence/codex-package_SHA256SUMS"

	ReleasePrefix = "rust-v"
)

// Every ceiling is a reviewed policy value; a larger release is a policy change,
// never an automatic widening.
const (
	MaxCompressedBytes      = 256 << 20
	MaxDecodedBytes         = 1 << 30
	MaxExpandedRegularBytes = 1 << 30
	MaxLogicalMembers       = 4096
	MaxPhysicalHeaders      = 4096
	MaxPackageMetadataBytes = 64 << 10
	MaxExtendedHeaderBytes  = 64 << 10
	MaxExtendedTotalBytes   = 1 << 20
	MaxMetadataBytes        = 8 << 20
	MaxChecksumsBytes       = 1 << 20
	MaxBundleBytes          = 4 << 20
	MaxPinBytes             = 64 << 10
	elfHeaderBytes          = 20
	releaseVersionPattern   = `(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-alpha(\.(0|[1-9][0-9]*)){0,2}|-beta(\.(0|[1-9][0-9]*))?)?`
	packageMetadataName     = "codex-package.json"
)

// The maintained platforms, and the only upstream targets each one may name.
var (
	// Platforms is the maintained platform order.
	Platforms = []string{"linux/amd64", "linux/arm64"}
	// Targets is the upstream target each platform may name.
	Targets = map[string]Target{
		"linux/amd64": TargetAMD64,
		"linux/arm64": TargetARM64,
	}
	// ELFMachines is the ELF machine each maintained target must name.
	ELFMachines = map[Target]uint16{
		TargetAMD64: 62,
		TargetARM64: 183,
	}
)

// Target is one upstream build target. It is a newtype so a platform string can
// never stand in for a target: they differ for every maintained pair.
type Target string

const (
	TargetAMD64 Target = "x86_64-unknown-linux-musl"
	TargetARM64 Target = "aarch64-unknown-linux-musl"
)

var (
	releaseVersionRE = regexp.MustCompile(`^` + releaseVersionPattern + `$`)
	sha256RE         = regexp.MustCompile(`^[0-9a-f]{64}$`)
	metadataDigestRE = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	checksumLineRE   = regexp.MustCompile(`^([0-9a-fA-F]{64})[ \t]+\*?(\S.*)$`)
	dateRE           = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// The exact member sets, in the order the refusal prints them.
var (
	rootKeys            = []string{"schema", "name", "kind", "release", "version", "package", "metadata", "checksums", "platforms", "authentication"}
	packageKeys         = []string{"layout_version", "variant", "entrypoint"}
	evidenceKeys        = []string{"url", "path", "sha256"}
	platformKeys        = []string{"target", "asset", "url", "sha256", "bundle"}
	authenticationKeys  = []string{"method", "verifier", "trust_root", "certificate_identity", "certificate_oidc_issuer", "verified_on"}
	verifierKeys        = []string{"name", "version"}
	bundleKeys          = []string{"base64Signature", "cert", "rekorBundle"}
	rekorBundleKeys     = []string{"Payload", "SignedEntryTimestamp"}
	rekorPayloadKeys    = []string{"body", "integratedTime", "logID", "logIndex"}
	packageMetadataKeys = []string{"layoutVersion", "version", "target", "variant", "entrypoint", "resourcesDir", "pathDir"}
	// EvidenceFiles is the committed evidence set: the metadata and the checksums.
	EvidenceFiles = []string{MetadataPath, ChecksumsPath}
)

// RequiredExecutables is the reviewed executable set of a package archive.
var RequiredExecutables = []string{"bin/codex", "bin/codex-code-mode-host", "codex-path/rg", "codex-resources/bwrap"}

// TopLevelRoots is the reviewed top-level member set. A new root is a review
// event, not a silent pass.
var TopLevelRoots = map[string]bool{"bin": true, "codex-path": true, "codex-resources": true, packageMetadataName: true}

// Refusal is a committed input that is not what the reviewed pin says it is.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

// Refuse builds a Refusal.
func Refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a refusal rather than an I/O or programming
// failure, so a caller can map it onto the refusal status.
func IsRefusal(err error) bool {
	var refusal *Refusal
	return errorsAs(err, &refusal)
}

// Evidence is one committed evidence file and the exact URL it was taken from.
type Evidence struct {
	URL    string
	Path   string
	SHA256 string
}

// PlatformPin is one platform's reviewed payload, bundle included.
type PlatformPin struct {
	Platform string
	Target   Target
	Asset    string
	URL      string
	SHA256   string
	Bundle   Evidence
}

// Authentication is the signer policy that was enforced when the pin was made.
type Authentication struct {
	VerifierVersion string
	TrustRoot       string
	Identity        string
	Issuer          string
	VerifiedOn      time.Time
}

// Pin is the reviewed closure.
type Pin struct {
	Directory      string
	Release        string
	Version        string
	Metadata       Evidence
	Checksums      Evidence
	Platforms      map[string]PlatformPin
	Authentication Authentication
}

// ChecksumLine is one decoded `sha256sum`-style manifest row.
type ChecksumLine struct {
	Digest string
	Name   string
}

// --- canonical derivations --------------------------------------------------
//
// Every URL, filename and evidence path is reconstructed from the validated
// release here. Callers compare for equality, never for a prefix: a lookalike
// origin, an extra path segment or a different repository can then not satisfy the
// check by sharing a leading substring.

// APIURL is the exact-tag release document's URL.
func APIURL(release string) string {
	return "https://api.github.com/repos/" + Repository + "/releases/tags/" + release
}

// AssetURL is one release asset's URL.
func AssetURL(release, asset string) string {
	return "https://github.com/" + Repository + "/releases/download/" + release + "/" + asset
}

// AssetName is the package archive's upstream name for one target.
func AssetName(target Target) string { return "codex-package-" + string(target) + ".tar.gz" }

// BundleName is the legacy sigstore bundle's upstream name for one target.
func BundleName(target Target) string { return AssetName(target) + ".sigstore" }

// BundlePath is where a bundle is committed.
func BundlePath(target Target) string { return "evidence/" + BundleName(target) }

// Identity is the exact signer identity the pin may name.
func Identity(release string) string { return Workflow + "@refs/tags/" + release }

// RequireRelease is the only exact `rust-v` spelling the hook and this package
// both accept.
func RequireRelease(release string, what string) (string, error) {
	if release == "" {
		return "", Refuse("%s must be a non-empty string", what)
	}
	if !strings.HasPrefix(release, ReleasePrefix) {
		return "", Refuse("%s must be rust-v<version>, found %q", what, release)
	}
	body := release[len(ReleasePrefix):]
	if !releaseVersionRE.MatchString(body) {
		return "", Refuse("%s is not an exact Codex version: %q", what, release)
	}
	return release, nil
}

// ReleaseVersion is the numeric version a release tag names.
func ReleaseVersion(release string) string { return release[len(ReleasePrefix):] }

// RequireTarget is the upstream target a caller asked for.
func RequireTarget(raw string, what string) (Target, error) {
	for _, candidate := range []Target{TargetAMD64, TargetARM64} {
		if string(candidate) == raw {
			return candidate, nil
		}
	}
	return "", Refuse("%s must be one of %s, found %q", what, describeTargets(), raw)
}

func describeTargets() string {
	quoted := make([]string, 0, 2)
	for _, target := range []Target{TargetAMD64, TargetARM64} {
		quoted = append(quoted, "'"+string(target)+"'")
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// UTCToday is the date `verified_on` is stamped and checked against, in UTC.
//
// A pin is written on one maintainer host and re-checked on builders in every
// zone, so the stamp has to be one instant rather than the writer's local
// calendar: a local stamp east of UTC records a date UTC-or-west checkers still
// read as tomorrow and refuse as future, which leaves a reviewed pin unbuildable
// for up to ~14 hours (#136). `avm-pin codex` stamps with this same function, so
// there is one definition of "today".
func UTCToday() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// --- schema -----------------------------------------------------------------

// Parse validates the pin's schema. Evidence is a separate, later step.
func Parse(raw []byte, directory string) (Pin, error) {
	document, err := strictjson.Decode(raw, "pin.json")
	if err != nil {
		return Pin{}, err
	}
	members, err := strictjson.FieldsExactly(document, rootKeys, "pin")
	if err != nil {
		return Pin{}, err
	}
	if schema, err := strictjson.Integer(members["schema"], "pin schema"); err != nil {
		return Pin{}, err
	} else if schema != Schema {
		return Pin{}, Refuse("pin schema must be %d", Schema)
	}
	if name, err := strictjson.Text(members["name"], "pin name"); err != nil {
		return Pin{}, err
	} else if name != Name {
		return Pin{}, Refuse("pin name must be %s", Name)
	}
	if kind, err := strictjson.Text(members["kind"], "pin kind"); err != nil {
		return Pin{}, err
	} else if kind != Kind {
		return Pin{}, Refuse("pin kind must be %s", Kind)
	}
	releaseValue, err := strictjson.Text(members["release"], "pin release")
	if err != nil {
		return Pin{}, err
	}
	release, err := RequireRelease(releaseValue, "pin release")
	if err != nil {
		return Pin{}, err
	}
	version, err := strictjson.Text(members["version"], "pin version")
	if err != nil {
		return Pin{}, err
	}
	if !releaseVersionRE.MatchString(version) {
		return Pin{}, Refuse("pin version is not an exact Codex version: %q", version)
	}
	if release != ReleasePrefix+version {
		return Pin{}, Refuse("pin release %s does not spell pin version %s", release, version)
	}
	pkg, err := strictjson.FieldsExactly(members["package"], packageKeys, "pin package")
	if err != nil {
		return Pin{}, err
	}
	if layout, err := strictjson.Integer(pkg["layout_version"], "pin package layout_version"); err != nil {
		return Pin{}, err
	} else if layout != PackageLayoutVersion {
		return Pin{}, Refuse("pin package layout_version must be %d", PackageLayoutVersion)
	}
	if variant, err := strictjson.Text(pkg["variant"], "pin package variant"); err != nil {
		return Pin{}, err
	} else if variant != PackageVariant {
		return Pin{}, Refuse("pin package variant must be %s", PackageVariant)
	}
	if entrypoint, err := strictjson.Text(pkg["entrypoint"], "pin package entrypoint"); err != nil {
		return Pin{}, err
	} else if entrypoint != PackageEntrypoint {
		return Pin{}, Refuse("pin package entrypoint must be %s", PackageEntrypoint)
	}
	metadata, err := evidenceOf(members["metadata"], APIURL(release), MetadataPath, "metadata evidence")
	if err != nil {
		return Pin{}, err
	}
	checksums, err := evidenceOf(members["checksums"], AssetURL(release, ChecksumsAsset), ChecksumsPath, "checksums evidence")
	if err != nil {
		return Pin{}, err
	}
	platformMembers, err := strictjson.FieldsExactly(members["platforms"], Platforms, "pin platforms")
	if err != nil {
		return Pin{}, err
	}
	platforms := make(map[string]PlatformPin, len(Platforms))
	for _, platform := range Platforms {
		pinned, err := platformOf(platformMembers[platform], platform, release)
		if err != nil {
			return Pin{}, err
		}
		platforms[platform] = pinned
	}
	authentication, err := authenticationOf(members["authentication"], release)
	if err != nil {
		return Pin{}, err
	}
	return Pin{
		Directory:      directory,
		Release:        release,
		Version:        version,
		Metadata:       metadata,
		Checksums:      checksums,
		Platforms:      platforms,
		Authentication: authentication,
	}, nil
}

func evidenceOf(value any, url, path, what string) (Evidence, error) {
	members, err := strictjson.FieldsExactly(value, evidenceKeys, what)
	if err != nil {
		return Evidence{}, err
	}
	foundURL, err := strictjson.Text(members["url"], what+" url")
	if err != nil {
		return Evidence{}, err
	}
	if foundURL != url {
		return Evidence{}, Refuse("%s url must be %s, found %q", what, url, foundURL)
	}
	foundPath, err := strictjson.Text(members["path"], what+" path")
	if err != nil {
		return Evidence{}, err
	}
	if foundPath != path {
		return Evidence{}, Refuse("%s path must be %s, found %q", what, path, foundPath)
	}
	digest, err := digestOf(members["sha256"], what+" sha256")
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{URL: url, Path: path, SHA256: digest}, nil
}

func platformOf(value any, platform, release string) (PlatformPin, error) {
	what := platform + " platform"
	members, err := strictjson.FieldsExactly(value, platformKeys, what)
	if err != nil {
		return PlatformPin{}, err
	}
	targetValue, err := strictjson.Text(members["target"], what+" target")
	if err != nil {
		return PlatformPin{}, err
	}
	target, err := RequireTarget(targetValue, what+" target")
	if err != nil {
		return PlatformPin{}, err
	}
	expected := Targets[platform]
	if target != expected {
		return PlatformPin{}, Refuse("%s target must be %s, found %s", what, expected, target)
	}
	asset, err := strictjson.Text(members["asset"], what+" asset")
	if err != nil {
		return PlatformPin{}, err
	}
	if asset != AssetName(target) {
		return PlatformPin{}, Refuse("%s asset must be %s, found %q", what, AssetName(target), asset)
	}
	url, err := strictjson.Text(members["url"], what+" url")
	if err != nil {
		return PlatformPin{}, err
	}
	expectedURL := AssetURL(release, asset)
	if url != expectedURL {
		return PlatformPin{}, Refuse("%s url must be %s, found %q", what, expectedURL, url)
	}
	bundle, err := evidenceOf(members["bundle"], AssetURL(release, BundleName(target)),
		BundlePath(target), what+" bundle")
	if err != nil {
		return PlatformPin{}, err
	}
	digest, err := digestOf(members["sha256"], what+" sha256")
	if err != nil {
		return PlatformPin{}, err
	}
	return PlatformPin{Platform: platform, Target: target, Asset: asset, URL: url,
		SHA256: digest, Bundle: bundle}, nil
}

func authenticationOf(value any, release string) (Authentication, error) {
	const what = "authentication"
	members, err := strictjson.FieldsExactly(value, authenticationKeys, what)
	if err != nil {
		return Authentication{}, err
	}
	method, err := strictjson.Text(members["method"], what+" method")
	if err != nil {
		return Authentication{}, err
	}
	if method != AuthenticationMethod {
		return Authentication{}, Refuse("%s method must be %s, found %q", what, AuthenticationMethod, method)
	}
	verifier, err := strictjson.FieldsExactly(members["verifier"], verifierKeys, what+" verifier")
	if err != nil {
		return Authentication{}, err
	}
	name, err := strictjson.Text(verifier["name"], what+" verifier name")
	if err != nil {
		return Authentication{}, err
	}
	if name != VerifierName {
		return Authentication{}, Refuse("%s verifier must be %s, found %q", what, VerifierName, name)
	}
	version, err := strictjson.Text(verifier["version"], what+" verifier version")
	if err != nil {
		return Authentication{}, err
	}
	if version != VerifierVersion {
		return Authentication{}, Refuse("%s verifier version must be %s, found %q",
			what, VerifierVersion, version)
	}
	trustRoot, err := strictjson.Text(members["trust_root"], what+" trust_root")
	if err != nil {
		return Authentication{}, err
	}
	if trustRoot != TrustRoot {
		return Authentication{}, Refuse("%s trust_root must be %s, found %q", what, TrustRoot, trustRoot)
	}
	identity, err := strictjson.Text(members["certificate_identity"], what+" identity")
	if err != nil {
		return Authentication{}, err
	}
	if identity != Identity(release) {
		return Authentication{}, Refuse("%s certificate identity must be %s, found %q",
			what, Identity(release), identity)
	}
	issuer, err := strictjson.Text(members["certificate_oidc_issuer"], what+" issuer")
	if err != nil {
		return Authentication{}, err
	}
	if issuer != Issuer {
		return Authentication{}, Refuse("%s OIDC issuer must be %s, found %q", what, Issuer, issuer)
	}
	day, err := strictjson.Text(members["verified_on"], what+" verified_on")
	if err != nil {
		return Authentication{}, err
	}
	if !dateRE.MatchString(day) {
		return Authentication{}, Refuse("%s verified_on must be YYYY-MM-DD, found %q", what, day)
	}
	verifiedOn, err := time.Parse("2006-01-02", day)
	if err != nil {
		return Authentication{}, Refuse("%s verified_on is not a calendar date: %q", what, day)
	}
	if verifiedOn.Year() < 1 {
		return Authentication{}, Refuse("%s verified_on is not a calendar date: %q", what, day)
	}
	if verifiedOn.After(UTCToday()) {
		return Authentication{}, Refuse("%s verified_on %s is in the future", what, day)
	}
	return Authentication{VerifierVersion: version, TrustRoot: trustRoot, Identity: identity,
		Issuer: issuer, VerifiedOn: verifiedOn}, nil
}

func digestOf(value any, what string) (string, error) {
	raw, err := strictjson.Text(value, what)
	if err != nil {
		return "", err
	}
	if !sha256RE.MatchString(raw) {
		return "", Refuse("%s must be 64 lower-case hex characters", what)
	}
	return raw, nil
}

// --- committed evidence ------------------------------------------------------

// Load is the reviewed closure from `directory`; every evidence byte is checked.
func Load(directory string) (Pin, error) {
	path, err := evidence.RegularFile(directory, "pin.json", "pin.json")
	if err != nil {
		return Pin{}, err
	}
	raw, err := evidence.ReadBounded(path, MaxPinBytes, "pin.json")
	if err != nil {
		return Pin{}, err
	}
	pin, err := Parse(raw, directory)
	if err != nil {
		return Pin{}, err
	}
	metadata, err := readEvidence(pin.Directory, pin.Metadata, MaxMetadataBytes,
		"metadata evidence", "committed release metadata")
	if err != nil {
		return Pin{}, err
	}
	if err := checkMetadata(metadata, pin); err != nil {
		return Pin{}, err
	}
	checksums, err := readEvidence(pin.Directory, pin.Checksums, MaxChecksumsBytes,
		"checksums evidence", "committed checksums")
	if err != nil {
		return Pin{}, err
	}
	if err := checkChecksums(checksums, pin); err != nil {
		return Pin{}, err
	}
	// Both architectures, including the one this build will not install: a pin that
	// carries stale evidence for the other platform is not a closure.
	for _, platform := range Platforms {
		pinned := pin.Platforms[platform]
		bundle, err := readEvidence(pin.Directory, pinned.Bundle, MaxBundleBytes,
			platform+" bundle evidence", platform+" bundle")
		if err != nil {
			return Pin{}, err
		}
		if err := checkBundle(bundle, pinned); err != nil {
			return Pin{}, err
		}
	}
	return pin, nil
}

func readEvidence(directory string, wanted Evidence, limit int64, what, label string) ([]byte, error) {
	path, err := evidence.RegularFile(directory, wanted.Path, what)
	if err != nil {
		return nil, err
	}
	raw, err := evidence.ReadBounded(path, limit, label)
	if err != nil {
		return nil, err
	}
	if evidence.Sha256Of(raw) != wanted.SHA256 {
		return nil, Refuse("%s does not match the pin digest", label)
	}
	return raw, nil
}

// checkMetadata cross-checks the original exact-tag API document against the pin.
//
// Only the security projection is inspected: the tag and the per-asset names and
// digests. Download counts, body text and timestamps are mutable and are
// deliberately ignored here; the maintenance writer applies the same rule when it
// decides whether a repeat run changed anything (#97 plan step 4.5).
func checkMetadata(raw []byte, pin Pin) (err error) {
	document, err := strictjson.Decode(raw, "committed release metadata")
	if err != nil {
		return err
	}
	members, ok := document.(map[string]any)
	if !ok {
		return Refuse("committed release metadata must be a JSON object")
	}
	tag, ok := members["tag_name"].(string)
	if !ok {
		return Refuse("committed release metadata has no string tag_name")
	}
	if tag != pin.Release {
		return Refuse("committed release metadata names %q, pin names %q", tag, pin.Release)
	}
	assets, ok := members["assets"].([]any)
	if !ok {
		return Refuse("committed release metadata has no assets array")
	}
	digests := make(map[string]string, len(assets))
	declared := make(map[string]bool, len(assets))
	for _, entry := range assets {
		asset, ok := entry.(map[string]any)
		if !ok {
			return Refuse("committed release metadata has a non-object asset")
		}
		name, ok := asset["name"].(string)
		if !ok || name == "" {
			return Refuse("committed release metadata has an asset with no name")
		}
		if declared[name] {
			return Refuse("committed release metadata repeats asset %q", name)
		}
		declared[name] = true
		value, present := asset["digest"]
		if !present || value == nil {
			continue
		}
		digest, ok := value.(string)
		if !ok {
			return Refuse("committed release metadata asset %s has a non-string digest", name)
		}
		match := metadataDigestRE.FindStringSubmatch(digest)
		if match == nil {
			return Refuse("committed release metadata asset %s has a non-sha256 digest %q", name, digest)
		}
		digests[name] = match[1]
	}
	type expectedAsset struct {
		name           string
		digest         string
		digestRequired bool
	}
	expected := []expectedAsset{{name: ChecksumsAsset, digest: pin.Checksums.SHA256, digestRequired: true}}
	for _, platform := range Platforms {
		pinned := pin.Platforms[platform]
		expected = append(expected,
			expectedAsset{name: pinned.Asset, digest: pinned.SHA256, digestRequired: true},
			expectedAsset{name: BundleName(pinned.Target), digest: pinned.Bundle.SHA256})
	}
	for _, want := range expected {
		if !declared[want.name] {
			return Refuse("committed release metadata does not list the required asset %q", want.name)
		}
		found, has := digests[want.name]
		if !has {
			if want.digestRequired {
				return Refuse("committed release metadata has no digest for %q", want.name)
			}
			continue
		}
		if found != want.digest {
			return Refuse("committed release metadata digest for %s is %s, but the pin commits %s",
				want.name, found, want.digest)
		}
	}
	return nil
}

// checkChecksums corroborates exactly the committed digests from the original
// `SHA256SUMS`. Other upstream rows stay in the committed file as reviewed
// evidence but give no install authority, so only the maintained payload lines are
// compared.
func checkChecksums(raw []byte, pin Pin) error {
	lines, err := ParseChecksums(raw, "committed checksums")
	if err != nil {
		return err
	}
	declared := make(map[string]string, len(lines))
	for _, line := range lines {
		declared[line.Name] = line.Digest
	}
	for _, platform := range Platforms {
		pinned := pin.Platforms[platform]
		found, ok := declared[pinned.Asset]
		if !ok {
			return Refuse("committed checksums have no line for %q", pinned.Asset)
		}
		if found != pinned.SHA256 {
			return Refuse("committed checksums disagree with the pin for %s: %s vs %s",
				pinned.Asset, found, pinned.SHA256)
		}
	}
	return nil
}

// ParseChecksums decodes a `sha256sum`-style manifest into typed records, exactly
// once.
//
// The offline loader (committed evidence) and the maintenance writer (fresh
// upstream bytes) both read this grammar, so a change to it cannot be made in one
// security parser and forgotten in the other. Only the grammar and its duplicate
// rejection live here: what each caller does with the records stays with the
// caller.
func ParseChecksums(raw []byte, what string) ([]ChecksumLine, error) {
	if !isUTF8(raw) {
		return nil, Refuse("%s are not UTF-8", what)
	}
	lines := make([]ChecksumLine, 0, 8)
	seen := make(map[string]bool, 8)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		match := checksumLineRE.FindStringSubmatch(line)
		if match == nil {
			return nil, Refuse("%s have an unreadable line: %q", what, line)
		}
		digest, name := strings.ToLower(match[1]), match[2]
		if seen[name] {
			return nil, Refuse("%s repeat the entry %q", what, name)
		}
		seen[name] = true
		lines = append(lines, ChecksumLine{Digest: digest, Name: name})
	}
	return lines, nil
}

// checkBundle is the shape and cross-field consistency of a committed legacy cosign
// bundle.
//
// This is not signature verification and this function never claims any: it proves
// the committed bytes are a legacy bundle for *this* payload digest, so a swapped,
// truncated or re-architected bundle is refused here instead of being carried into
// the pin as meaningless evidence. Cryptographic verification ran once on the
// maintainer host (`avm-pin codex`).
func checkBundle(raw []byte, pinned PlatformPin) error {
	what := pinned.Bundle.Path
	document, err := strictjson.Decode(raw, what)
	if err != nil {
		return err
	}
	members, err := strictjson.FieldsExactly(document, bundleKeys, what)
	if err != nil {
		return err
	}
	if _, err := decodeBase64(members["base64Signature"], what+" base64Signature"); err != nil {
		return err
	}
	certificate, err := decodeBase64(members["cert"], what+" cert")
	if err != nil {
		return err
	}
	if !strings.Contains(string(certificate), "-----BEGIN CERTIFICATE-----") {
		return Refuse("%s cert is not a PEM certificate", what)
	}
	rekor, err := strictjson.FieldsExactly(members["rekorBundle"], rekorBundleKeys, what+" rekorBundle")
	if err != nil {
		return err
	}
	if _, err := decodeBase64(rekor["SignedEntryTimestamp"], what+" SignedEntryTimestamp"); err != nil {
		return err
	}
	payload, err := strictjson.FieldsExactly(rekor["Payload"], rekorPayloadKeys, what+" Payload")
	if err != nil {
		return err
	}
	bodyRaw, err := decodeBase64(payload["body"], what+" Payload.body")
	if err != nil {
		return err
	}
	bodyDocument, err := strictjson.Decode(bodyRaw, what+" Payload.body")
	if err != nil {
		return err
	}
	body, ok := bodyDocument.(map[string]any)
	if !ok {
		return Refuse("%s does not carry a hashedrekord entry", what)
	}
	if kind, _ := body["kind"].(string); kind != "hashedrekord" {
		return Refuse("%s does not carry a hashedrekord entry", what)
	}
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		return Refuse("%s entry has no spec", what)
	}
	data, _ := spec["data"].(map[string]any)
	hashField, ok := data["hash"].(map[string]any)
	if !ok {
		return Refuse("%s entry names no payload hash", what)
	}
	if algorithm, _ := hashField["algorithm"].(string); algorithm != "sha256" {
		return Refuse("%s entry is not a sha256 hashedrekord", what)
	}
	value, _ := hashField["value"].(string)
	if value != pinned.SHA256 {
		return Refuse("%s entry is for %q, but %s is %s", what, value, pinned.Asset, pinned.SHA256)
	}
	return nil
}

// --- selection ---------------------------------------------------------------

// RequireRequested is the exact-but-unreviewed refusal every verb applies first.
func (p Pin) RequireRequested(release string) error {
	exact, err := RequireRelease(release, "requested release")
	if err != nil {
		return err
	}
	if exact != p.Release {
		return Refuse("%s is not the pinned Codex release %s; an exact but unreviewed "+
			"version needs a reviewed pin", exact, p.Release)
	}
	return nil
}

// Selection is the reviewed payload for one exact release and one upstream target.
func (p Pin) Selection(release string, target Target) (PlatformPin, error) {
	if err := p.RequireRequested(release); err != nil {
		return PlatformPin{}, err
	}
	for _, platform := range Platforms {
		pinned := p.Platforms[platform]
		if pinned.Target == target {
			return pinned, nil
		}
	}
	return PlatformPin{}, Refuse("%s is not a maintained Codex platform", target)
}

// CheckReport is the offline `check` transcript: the closure and the policy, never
// a signature claim.
func (p Pin) CheckReport() string {
	var report strings.Builder
	report.WriteString("codex " + p.Release + ": offline committed-input check " +
		"(committed bytes and policy only; not a signature check)\n")
	for _, platform := range Platforms {
		pinned := p.Platforms[platform]
		report.WriteString("  " + platform + " " + string(pinned.Target) + " " +
			pinned.SHA256 + "\n")
	}
	report.WriteString("  evidence: " + itoa(int64(len(EvidenceFiles))) + " files plus " +
		itoa(int64(len(p.Platforms))) + " structurally consistent bundles\n")
	return report.String()
}

// Names is every committed file of the closure, in the fixed published order.
func (p Pin) Names() []string {
	names := []string{"pin.json", MetadataPath, ChecksumsPath}
	for _, platform := range Platforms {
		names = append(names, BundlePath(p.Platforms[platform].Target))
	}
	return names
}

func itoa(value int64) string { return strconv.FormatInt(value, 10) }
