package artifactpin

import (
	"net/url"
	"regexp"
	"strings"
)

// Schema is the only pin document version this loader reads.
const Schema = 1

// Name is one artifact the shared model acts on. A pin's location names it,
// never its content, so an unrecognised directory is a refusal.
type Name string

const (
	Node Name = "node"
	GH   Name = "gh"
	Agy  Name = "agy"
)

// Names are the reviewed artifacts.
var Names = []Name{Node, GH, Agy}

func parseName(value string) (Name, error) {
	name := Name(value)
	for _, candidate := range Names {
		if name == candidate {
			return name, nil
		}
	}
	return "", refuse("%s: unknown artifact; the reviewed pins are %s", value, nameList(Names))
}

func nameList(names []Name) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, "'"+string(name)+"'")
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// Platform is one of the two architectures the base image builds for, spelled
// the way the pins and the gate spell it.
type Platform string

const (
	LinuxAMD64 Platform = "linux/amd64"
	LinuxARM64 Platform = "linux/arm64"
)

// Platforms is the reviewed set, in the order the pin documents name it.
var Platforms = []Platform{LinuxAMD64, LinuxARM64}

// DockerArch is the TARGETARCH token for the platform.
func (p Platform) DockerArch() string {
	if p == LinuxAMD64 {
		return "amd64"
	}
	return "arm64"
}

// Hyphenated is the platform with the slash replaced, as evidence file names
// spell it.
func (p Platform) Hyphenated() string { return strings.ReplaceAll(string(p), "/", "-") }

// ArchiveFormat is the compression a reviewed archive carries.
type ArchiveFormat string

const (
	TarGz ArchiveFormat = "tar.gz"
	TarXz ArchiveFormat = "tar.xz"
)

// Version is an exact X.Y.Z release. A pre-release, a build suffix or a range is
// a different selection with a different review, so it is refused.
type Version struct{ text string }

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,5})[.](0|[1-9][0-9]{0,5})[.](0|[1-9][0-9]{0,5})$`)

// ParseVersion validates an exact release version.
func ParseVersion(value, what string) (Version, error) {
	if !versionPattern.MatchString(value) {
		return Version{}, refuse("%s: expected an exact X.Y.Z version, got %q", what, value)
	}
	return Version{text: value}, nil
}

func (v Version) String() string { return v.text }

// Digest is a lower-case SHA-256 of one archive, archive member or evidence file.
type Digest string

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseDigest validates a lower-case SHA-256.
func ParseDigest(value, what string) (Digest, error) {
	if !digestPattern.MatchString(value) {
		return "", refuse("%s: expected a lower-case SHA-256, got %q", what, value)
	}
	return Digest(value), nil
}

// SHA512 is the digest Google's updater manifest carries for one archive.
type SHA512 string

var sha512Pattern = regexp.MustCompile(`^[0-9a-f]{128}$`)

func parseSHA512(value, what string) (SHA512, error) {
	if !sha512Pattern.MatchString(value) {
		return "", refuse("%s: expected a lower-case SHA-512, got %q", what, value)
	}
	return SHA512(value), nil
}

// RelPath is an archive-relative or pin-directory-relative path. An absolute
// path, a `..` component or a backslash is a refusal.
type RelPath string

func parseRelPath(value, what string) (RelPath, error) {
	if strings.HasPrefix(value, "/") || strings.Contains(value, `\`) {
		return "", refuse("%s: expected an artifacts-relative path, got %q", what, value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", refuse("%s: expected an artifacts-relative path, got %q", what, value)
		}
	}
	return RelPath(value), nil
}

func (p RelPath) String() string { return string(p) }

// Path is an absolute published path with no `..` component.
type Path string

func parsePath(value, what string) (Path, error) {
	if !strings.HasPrefix(value, "/") {
		return "", refuse("%s: expected an absolute path, got %q", what, value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", refuse("%s: expected an absolute path, got %q", what, value)
		}
	}
	return Path(value), nil
}

func (p Path) String() string { return string(p) }

// SourceURL is a URL the loader may hand to a fetch. `file:` exists for the
// offline suite, which drives this loader; which origins a *committed* pin may
// name is the gate's decision.
func sourceURL(value, what string) (string, error) {
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "file://") {
		return "", refuse("%s: expected an https (or fixture file) URL, got %q", what, value)
	}
	return value, nil
}

// httpsURL is a URL that is only ever compared, never fetched.
func httpsURL(value, what string) (string, error) {
	if !strings.HasPrefix(value, "https://") {
		return "", refuse("%s: expected an https URL, got %q", what, value)
	}
	return value, nil
}

// --- the reviewed shape -----------------------------------------------------

// Payload lists what each artifact publishes. Node installs the whole reviewed
// archive because the bundled npm/npx and lib/node_modules/npm are one tree; gh
// publishes the binary and its man pages, and deliberately not the archive's
// LICENSE, which would land on /usr/local/LICENSE next to Node's.
var NodePayload = []RelPath{"bin", "include", "lib", "share", "CHANGELOG.md", "LICENSE", "README.md"}
var GHPayload = []RelPath{"bin/gh", "share/man"}

// AgyPayload is (archive member, published path) rather than a bare path: the
// image publishes /usr/local/bin/agy and never an `antigravity` alias.
var AgyPayload = []Member{{Archive: "antigravity", Published: "bin/agy"}}

// NodeLinks makes /usr/bin/node the pinned interpreter path of the whole
// repository: the pnpm launcher, its verifiers, the dsh helper and several
// suites hardcode it.
var NodeLinks = []Link{{Path: "/usr/bin/node", Target: "/usr/local/bin/node"}}

// NodeKey is the committed Node release key and the fingerprint it must carry.
const (
	NodeKey            = RelPath("node-release-key.asc")
	NodeKeyFingerprint = "5BE8A3F6C8A5C01D106C0AD820B1A390B168D356"
)

// NPMPackage is the npm command Node's archive ships, relative to the install
// destination. The pin records the version reviewed in that exact file, so a
// refresh that changes the bundled npm is visible.
const NPMPackage = "lib/node_modules/npm/package.json"

// GHPolicy is the reviewed Sigstore identity: cli/cli signs its release assets
// from its deployment workflow, which is also why the pin-time check is
// authoritative and the build check binds committed bundle to committed hash.
var GHPolicy = Policy{
	Repository:            "cli/cli",
	SignerWorkflow:        "cli/cli/.github/workflows/deployment.yml",
	CertOIDCIssuer:        "https://token.actions.githubusercontent.com",
	DenySelfHostedRunners: true,
}

// AgyRepository is agy's upstream, which also names its release API.
const AgyRepository = "google-antigravity/antigravity-cli"

// AgyManifestOrigin is Google's updater manifest, the second corroboration
// channel. Only its version and per-architecture sha512 are acted on; the GCS URL
// it carries is never a download source.
const AgyManifestOrigin = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/"

// AgyReleaseAPI is the committed GitHub release record's origin.
const AgyReleaseAPI = "https://api.github.com/repos/" + AgyRepository + "/releases/tags/"

// AgyEvidence is where each corroboration file is committed.
var AgyEvidence = map[Platform]RelPath{
	LinuxAMD64: "evidence/manifest-linux-amd64.json",
	LinuxARM64: "evidence/manifest-linux-arm64.json",
}

// AgyReleaseEvidence is the committed GitHub release record.
const AgyReleaseEvidence = RelPath("evidence/release.json")

// MemberCeilings bound a hash-only artifact's archive shape as well as its
// digest: 64 MiB is absurdly generous for Node (amd64 is 31,029,996 B). agy
// 1.4.11 amd64 is 67,990,723 B compressed and decodes to 233,943,440 B, so it
// needs its own numbers; 128 MiB/512 MiB leave about 2x headroom.
func maxArchiveBytes(name Name) int64 {
	if name == Agy {
		return 128 << 20
	}
	return 64 << 20
}

// MaxArchiveBytes is the ceiling for one archive of name.
func MaxArchiveBytes(name Name) int64 { return maxArchiveBytes(name) }

// MaxMemberBytes is the ceiling for one extracted member.
func MaxMemberBytes(name Name) int64 {
	if name == Agy {
		return 512 << 20
	}
	return 0
}

// ExactMembers is a hash-only artifact's reviewed archive shape: an archive that
// carries a second member, or an ELF for another machine under a consistent
// hash, is not the reviewed binary.
var ExactMembers = map[Name][]string{Node: nil, GH: nil, Agy: {"antigravity"}}

// ExactMemberModes is the reviewed mode for an EXACT_MEMBERS artifact. The digest
// covers the archive's member mode too, so this is not a hole in the hash: both
// channels can agree on an archive whose member is mode 0777, which would publish
// a world-writable /usr/local/bin/agy that every guest UID runs.
var ExactMemberModes = map[Name]int64{Node: 0, GH: 0, Agy: 0o755}

// ELFMachines is the e_machine each platform's agy binary must declare.
var ELFMachines = map[Name]map[Platform]uint16{
	Node: nil,
	GH:   nil,
	Agy:  {LinuxAMD64: 62, LinuxARM64: 183},
}

// PlatformToken is the architecture token each project spells its own way: Node
// says linux-x64, the GitHub CLI says linux_amd64, agy says x64.
func platformToken(name Name, platform Platform) string {
	arch := platform.DockerArch()
	switch name {
	case Node:
		if arch == "amd64" {
			return "linux-x64"
		}
		return "linux-arm64"
	case Agy:
		if arch == "amd64" {
			return "x64"
		}
		return "arm64"
	case GH:
		return "linux_" + arch
	}
	return "linux_" + arch
}

// ExpectedFilename is the one asset name a pin for this release and platform may
// name.
func ExpectedFilename(name Name, version Version, platform Platform) string {
	token := platformToken(name, platform)
	switch name {
	case Node:
		return "node-v" + version.String() + "-" + token + ".tar.xz"
	case Agy:
		return "agy_cli_linux_" + token + ".tar.gz"
	case GH:
		return name.String() + "_" + version.String() + "_" + token + ".tar.gz"
	}
	return name.String() + "_" + version.String() + "_" + token + ".tar.gz"
}

func (n Name) String() string { return string(n) }

// ReleaseTag is the upstream tag a release is published under: Node and the
// GitHub CLI prefix it with v, agy's tag is the bare version.
func ReleaseTag(name Name, version Version) string {
	if name == Agy {
		return version.String()
	}
	return "v" + version.String()
}

// CanonicalURL is the upstream URL, *computed* rather than matched by prefix. A
// pin has to equal this exactly, so `..` segments (which GitHub resolves into
// another repository), a lookalike host, an extra path segment and a query string
// all stop being able to name something other than the reviewed release asset.
func CanonicalURL(name Name, version Version, platform Platform) string {
	filename := ExpectedFilename(name, version, platform)
	if name == Node {
		return "https://nodejs.org/dist/v" + version.String() + "/" + filename
	}
	tag := ReleaseTag(name, version)
	if name == Agy {
		return "https://github.com/" + AgyRepository + "/releases/download/" + tag + "/" + filename
	}
	return "https://github.com/" + GHPolicy.Repository + "/releases/download/" + tag + "/" + filename
}

// BundleName is where one gh archive's Sigstore bundle is committed.
func BundleName(name Name, version Version, platform Platform) string {
	return name.String() + "-" + version.String() + "-" + platform.Hyphenated() + ".bundle.jsonl"
}

// ManifestURL is Google's updater manifest for one architecture. Its GCS `url`
// field is never a download source; only the version and sha512 are acted on.
func ManifestURL(platform Platform) string {
	return AgyManifestOrigin + "linux_" + platform.DockerArch() + ".json"
}

// ArchiveSuffix is the compression format a supported release archive carries.
func archiveFormat(filename string) (ArchiveFormat, error) {
	switch {
	case strings.HasSuffix(filename, ".tar.gz"):
		return TarGz, nil
	case strings.HasSuffix(filename, ".tar.xz"):
		return TarXz, nil
	}
	return "", refuse("%s: not a supported release archive ([tar.gz tar.xz])", filename)
}

// memberPrefix is the archive's own top-level directory: Node and gh carry one
// named after the file, agy's archive carries its binary at the root.
func memberPrefix(filename string) string {
	format, err := archiveFormat(filename)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(filename, "."+string(format)) + "/"
}

func expectedMemberPrefix(name Name, filename string) string {
	if name == Agy {
		return ""
	}
	return memberPrefix(filename)
}

// checkSourceURL requires the pin's URL to be the computed one, not merely to
// start like it.
func checkSourceURL(name Name, rawURL string, version Version, platform Platform) error {
	what := name.String() + ": " + string(platform)
	expected := CanonicalURL(name, version, platform)
	if rawURL == expected {
		return nil
	}
	filename := ExpectedFilename(name, version, platform)
	if !strings.HasPrefix(rawURL, "file:") {
		return refuse("%s: the url must be exactly %s, got %s", what, expected, rawURL)
	}
	// The offline suite's transport. It is held to the same asset name and refused
	// any traversal, query or fragment, so a fixture cannot express a URL a
	// committed pin could not.
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return refuse("%s: %s is not a plain file url", what, rawURL)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Host != "" && parsed.Host != "localhost") {
		return refuse("%s: %s is not a plain file url", what, rawURL)
	}
	var parts []string
	for _, part := range strings.Split(parsed.Path, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			return refuse("%s: %s must name %s in a plain directory", what, rawURL, filename)
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 || parts[len(parts)-1] != filename {
		return refuse("%s: %s must name %s in a plain directory", what, rawURL, filename)
	}
	return nil
}
