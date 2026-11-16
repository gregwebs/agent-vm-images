// Package claudepin reads the committed Claude native payload closure (#98).
//
// `pin.json` is the reviewed selection: the exact release, the upstream signed
// manifest and its detached signature, the release signing key, and per Linux
// architecture the exact executable URL, SHA-256 and byte size that the native
// installer is allowed to place. This package is its only reader. It needs no
// network, resolves no release and writes nothing.
//
// Verification order, because each step is only meaningful over the previous one:
// the pin's structure (exact key sets, canonical URLs recomputed from the version,
// typed digests and sizes), then the committed evidence bytes re-hashed now, then
// the key file's single primary fingerprint, then the detached OpenPGP signature
// over the committed manifest, then the signed manifest's own version, binary name,
// SHA-256 and size for each maintained platform. A build then compares the fetched
// executable with the pin and checks that it is an ELF object for the target
// machine. What is proven is *which bytes* were selected and reviewed; nothing here
// says a release is safe.
//
// `gpg` (with its `gpgconf`) is the only external program.
package claudepin

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// Schema is the only pin schema this repository writes or reads.
const Schema = 1

// Name and Kind identify the closure: a pin that names another artifact is not
// this one, however well-formed it is.
const (
	Name = "claude"
	Kind = "claude-native-release"
)

// AuthenticationMethod is the evidence shape: an OpenPGP-signed manifest.
const AuthenticationMethod = "openpgp-signed-manifest"

// The three URLs and two evidence paths are derived, never read from config: a
// pin that names another location is not this closure.
const (
	DownloadBase  = "https://downloads.claude.ai/claude-code-releases"
	KeyURL        = "https://downloads.claude.ai/keys/claude-code.asc"
	KeyPath       = "claude-code-release-key.asc"
	ManifestPath  = "evidence/manifest.json"
	SignaturePath = "evidence/manifest.json.sig"
)

// The reviewed signer (https://code.claude.com/docs/en/setup). The build binds the
// pin's fingerprint to the committed key file and to the signature but never reads
// this constant; the writer and the source gate hold the pin to it.
const (
	ReviewedSignerFingerprint = "31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE"
	ReviewedSignerUID         = "Anthropic Claude Code Release Signing <security@anthropic.com>"
)

// Byte ceilings. A release executable is ~241 MB today; 512 MiB leaves room for a
// normal bump while making an accidental order-of-magnitude read a refusal.
const (
	MaxExecutableBytes = 512 << 20
	MaxEvidenceBytes   = 1 << 20
	MaxPinBytes        = 64 << 10
	MaxStateBytes      = 1 << 20
	ReadChunkBytes     = 1 << 20
	ELFHeaderBytes     = 20
)

// Platforms maps a pin platform key to the token the signed manifest and the CDN
// path use.
var Platforms = map[string]string{
	"linux/amd64": "linux-x64",
	"linux/arm64": "linux-arm64",
}

// Tokens is the reverse of Platforms, in the manifest's own naming.
var Tokens = map[string]string{
	"linux-x64":   "linux/amd64",
	"linux-arm64": "linux/arm64",
}

// ELFMachines is the machine field an executable for each token must carry.
var ELFMachines = map[string]int{
	"linux-x64":   62,
	"linux-arm64": 183,
}

// versionPattern is the owning hook's exact-slot grammar, byte-for-byte the one in
// `images/tools/claude/install-claude.sh`: a value the hook accepts must not be a
// value this loader rejects, or the reverse.
var versionPattern = regexp.MustCompile(
	`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)` +
		`(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

var (
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9A-F]{40}$`)
	datePattern        = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

// Refusal is a committed input that is not what the reviewed pin says it is. The
// message is the contract: the suites match diagnostic fragments.
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
	return errors.As(err, &refusal)
}

// Evidence is one committed file the pin names: an upstream URL, its path in the
// pin directory, and the digest the review accepted.
type Evidence struct {
	URL    string
	Path   string
	SHA256 string
}

// Key is the committed release signing key.
type Key struct {
	URL         string
	Path        string
	Fingerprint string
	SHA256      string
}

// Executable is one platform's pinned payload.
type Executable struct {
	Platform string
	Token    string
	URL      string
	SHA256   string
	Size     int64
}

// Pin is the committed closure, validated but not yet proven: `Verify` is what
// reads evidence and gpg.
type Pin struct {
	Directory   string
	Version     string
	Manifest    Evidence
	Signature   Evidence
	Key         Key
	Executables map[string]Executable
	VerifiedOn  time.Time
	Reason      string
}

// Signature is the outcome of verifying the manifest's detached signature.
type Signature struct {
	PrimaryFingerprint string
	Created            string
}

// Manifest is the security projection of the signed manifest. Other content is
// ignored: a release may add fields, none of them select anything here.
type Manifest struct {
	Version     string
	Commit      string
	BuildDate   string
	Executables map[string]SignatureID
}

// SignatureID is what the signed manifest says about one platform's payload.
type SignatureID struct {
	SHA256 string
	Size   int64
}

// VerifiedPin is a pin proven against its own committed evidence.
type VerifiedPin struct {
	Pin       Pin
	Signature Signature
	Manifest  Manifest
}

// ExecutableURL is the one executable location a version and token can name.
func ExecutableURL(version, token string) string {
	return DownloadBase + "/" + version + "/" + token + "/claude"
}

// ManifestURL is the signed manifest's own location.
func ManifestURL(version string) string { return DownloadBase + "/" + version + "/manifest.json" }

// SignatureURL is the detached signature's location.
func SignatureURL(version string) string { return DownloadBase + "/" + version + "/manifest.json.sig" }

// RequireVersion accepts one exact Claude Code version.
func RequireVersion(value, what string) (string, error) {
	if value == "" {
		return "", Refuse("%s must be a non-empty string", what)
	}
	if !versionPattern.MatchString(value) {
		return "", Refuse("%s is not an exact Claude Code version: %q", what, value)
	}
	return value, nil
}

// UTCToday is the date `verified_on` is stamped and checked against.
//
// Both sides of this comparison are UTC, never the host's local date: the
// maintainer running the writer is routinely east of the builders, so a pin stamped
// with a local date can carry a UTC date that is still in the future for every
// UTC-or-west builder. That fails closed and heals itself within a day, but it
// leaves a merged pin unbuildable for up to ~14 hours, and the rule is that a pinned
// image input stays buildable and boring. The writer stamps with this same function,
// so there is one definition of "today".
func UTCToday() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Parse validates `pin.json`'s structure; no evidence byte and no gpg call is read.
func Parse(raw []byte, directory string) (Pin, error) {
	document, err := strictjson.Decode(raw, "pin.json")
	if err != nil {
		return Pin{}, err
	}
	members, err := strictjson.FieldsExactly(document, rootKeys, "pin")
	if err != nil {
		return Pin{}, err
	}
	schema, err := strictjson.Integer(members["schema"], "pin schema")
	if err != nil {
		return Pin{}, err
	}
	if schema != Schema {
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
	declared, err := strictjson.Text(members["version"], "pin version")
	if err != nil {
		return Pin{}, err
	}
	version, err := RequireVersion(declared, "pin version")
	if err != nil {
		return Pin{}, err
	}
	manifest, err := evidence(members["manifest"], ManifestURL(version), ManifestPath,
		"manifest evidence")
	if err != nil {
		return Pin{}, err
	}
	signature, err := evidence(members["signature"], SignatureURL(version), SignaturePath,
		"signature evidence")
	if err != nil {
		return Pin{}, err
	}
	key, err := key(members["key"])
	if err != nil {
		return Pin{}, err
	}
	platformMembers, err := strictjson.FieldsExactly(members["platforms"],
		platformNames(), "pin platforms")
	if err != nil {
		return Pin{}, err
	}
	executables := map[string]Executable{}
	for _, platform := range platformNames() {
		pinned, err := platformEntry(platformMembers[platform], platform, version)
		if err != nil {
			return Pin{}, err
		}
		executables[pinned.Token] = pinned
	}
	verifiedOn, reason, err := authentication(members["authentication"])
	if err != nil {
		return Pin{}, err
	}
	return Pin{
		Directory:   directory,
		Version:     version,
		Manifest:    manifest,
		Signature:   signature,
		Key:         key,
		Executables: executables,
		VerifiedOn:  verifiedOn,
		Reason:      reason,
	}, nil
}

var rootKeys = []string{"schema", "name", "kind", "version", "manifest", "signature",
	"key", "platforms", "authentication"}

var evidenceKeys = []string{"url", "path", "sha256"}
var keyKeys = []string{"url", "path", "fingerprint", "sha256"}
var platformKeys = []string{"manifest_platform", "url", "sha256", "size"}
var authenticationKeys = []string{"method", "verified_on", "reason"}

// platformNames is the pin's platform keys in a fixed order, so a refusal and a
// rendered document name them the same way on every host.
func platformNames() []string {
	names := make([]string, 0, len(Platforms))
	for name := range Platforms {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// tokens is the maintained tokens in the same fixed order.
func tokens() []string {
	names := make([]string, 0, len(Tokens))
	for token := range Tokens {
		names = append(names, token)
	}
	sort.Strings(names)
	return names
}

func evidence(value any, url, path, what string) (Evidence, error) {
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
	digest, err := sha256Field(members["sha256"], what+" sha256")
	if err != nil {
		return Evidence{}, err
	}
	return Evidence{URL: url, Path: path, SHA256: digest}, nil
}

func key(value any) (Key, error) {
	const what = "pin key"
	members, err := strictjson.FieldsExactly(value, keyKeys, what)
	if err != nil {
		return Key{}, err
	}
	url, err := strictjson.Text(members["url"], what+" url")
	if err != nil {
		return Key{}, err
	}
	if url != KeyURL {
		return Key{}, Refuse("%s url must be %s, found %q", what, KeyURL, url)
	}
	path, err := strictjson.Text(members["path"], what+" path")
	if err != nil {
		return Key{}, err
	}
	if path != KeyPath {
		return Key{}, Refuse("%s path must be %s, found %q", what, KeyPath, path)
	}
	fingerprint, err := strictjson.Text(members["fingerprint"], what+" fingerprint")
	if err != nil {
		return Key{}, err
	}
	if !fingerprintPattern.MatchString(fingerprint) {
		return Key{}, Refuse("%s fingerprint must be 40 upper-case hex characters", what)
	}
	digest, err := sha256Field(members["sha256"], what+" sha256")
	if err != nil {
		return Key{}, err
	}
	return Key{URL: url, Path: path, Fingerprint: fingerprint, SHA256: digest}, nil
}

func platformEntry(value any, platform, version string) (Executable, error) {
	what := platform + " platform"
	token := Platforms[platform]
	members, err := strictjson.FieldsExactly(value, platformKeys, what)
	if err != nil {
		return Executable{}, err
	}
	foundToken, err := strictjson.Text(members["manifest_platform"], what+" manifest_platform")
	if err != nil {
		return Executable{}, err
	}
	if foundToken != token {
		return Executable{}, Refuse("%s manifest_platform must be %s, found %q", what, token, foundToken)
	}
	url, err := strictjson.Text(members["url"], what+" url")
	if err != nil {
		return Executable{}, err
	}
	expected := ExecutableURL(version, token)
	if url != expected {
		return Executable{}, Refuse("%s url must be %s, found %q", what, expected, url)
	}
	size, err := strictjson.Integer(members["size"], what+" size")
	if err != nil {
		return Executable{}, err
	}
	if size < 1 || size > MaxExecutableBytes {
		return Executable{}, Refuse("%s size must be between 1 and %d, found %d",
			what, MaxExecutableBytes, size)
	}
	digest, err := sha256Field(members["sha256"], what+" sha256")
	if err != nil {
		return Executable{}, err
	}
	return Executable{Platform: platform, Token: token, URL: url, SHA256: digest, Size: size}, nil
}

// authentication reads the review record: when it was verified and why the method
// is what it is.
func authentication(value any) (time.Time, string, error) {
	const what = "pin authentication"
	members, err := strictjson.FieldsExactly(value, authenticationKeys, what)
	if err != nil {
		return time.Time{}, "", err
	}
	method, err := strictjson.Text(members["method"], what+" method")
	if err != nil {
		return time.Time{}, "", err
	}
	if method != AuthenticationMethod {
		return time.Time{}, "", Refuse("%s method must be %s, found %q", what, AuthenticationMethod, method)
	}
	day, err := strictjson.Text(members["verified_on"], what+" verified_on")
	if err != nil {
		return time.Time{}, "", err
	}
	if !datePattern.MatchString(day) {
		return time.Time{}, "", Refuse("%s verified_on must be YYYY-MM-DD, found %q", what, day)
	}
	parsed, err := time.Parse("2006-01-02", day)
	if err != nil {
		return time.Time{}, "", Refuse("%s verified_on is not a calendar date: %q", what, day)
	}
	if parsed.Year() < 1 {
		return time.Time{}, "", Refuse("%s verified_on is not a calendar date: %q", what, day)
	}
	if parsed.After(UTCToday()) {
		return time.Time{}, "", Refuse("%s verified_on %s is in the future", what, day)
	}
	reason, err := strictjson.Text(members["reason"], what+" reason")
	if err != nil {
		return time.Time{}, "", err
	}
	return parsed, reason, nil
}

func sha256Field(value any, what string) (string, error) {
	text, err := strictjson.Text(value, what)
	if err != nil {
		return "", err
	}
	if !sha256Pattern.MatchString(text) {
		return "", Refuse("%s must be 64 lower-case hex characters", what)
	}
	return text, nil
}

// Executable returns one platform's pinned payload.
func (p Pin) Executable(token string) (Executable, error) {
	pinned, ok := p.Executables[token]
	if !ok {
		return Executable{}, Refuse("%s is not a maintained Claude Code platform", token)
	}
	return pinned, nil
}

// CheckPolicy holds the pin to the reviewed signer. The build binds the fingerprint
// to the key file and the signature; this is the writer's and the source gate's
// check that the reviewed signer is the one the review accepted.
func (p Pin) CheckPolicy() error {
	if p.Key.Fingerprint != ReviewedSignerFingerprint {
		return Refuse("the pin names the signer %s, not the reviewed %s",
			p.Key.Fingerprint, ReviewedSignerFingerprint)
	}
	return nil
}

// PlatformList is the maintained platforms in order, for a diagnostic.
func PlatformList() []string { return platformNames() }

// TokenList is the maintained manifest tokens in order.
func TokenList() []string { return tokens() }

// DescribePlatforms is the `platform token` list the writer's --check prints.
func DescribePlatforms() string {
	parts := make([]string, 0, len(Platforms))
	for _, platform := range platformNames() {
		parts = append(parts, platform+" "+Platforms[platform])
	}
	return strings.Join(parts, ", ")
}
