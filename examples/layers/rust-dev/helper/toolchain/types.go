package toolchain

// Validated domain types. Every parser here is a port of the source reader's
// regex, length and canonical-origin rules, and each refusal names the field
// path the review reads. Values arrive as decoded JSON or TOML nodes, so a
// parser takes `any` and applies Python's exact-type rule rather than converting.

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Sha256 is a lowercase SHA-256 hex digest.
type Sha256 string

// Fingerprint is an uppercase 40-hex OpenPGP fingerprint.
type Fingerprint string

// Channel is an exact X.Y.Z release channel.
type Channel string

// Semver is an exact X.Y.Z version.
type Semver string

// IsoDate is a YYYY-MM-DD calendar date.
type IsoDate string

// TargetTriple is a Rust target triple.
type TargetTriple string

// CanonicalURL is a canonical rust-lang.org HTTPS origin.
type CanonicalURL string

// Filename is the last path segment of a canonical URL.
func (u CanonicalURL) Filename() string {
	parts := strings.Split(string(u), "/")
	return parts[len(parts)-1]
}

// EvidencePath is a canonical relative path under the pin directory.
type EvidencePath string

// MemberPrefix is a single archive member prefix like `name/`.
type MemberPrefix string

// Architecture is a supported Linux architecture. It is the `--arch` value, so
// it carries the go-arg text decoder: a bad value is a parse error.
type Architecture int

const (
	// AMD64 is linux/amd64.
	AMD64 Architecture = iota
	// ARM64 is linux/arm64.
	ARM64
)

// Architectures are the supported architectures in the source reader's order.
func Architectures() []Architecture { return []Architecture{AMD64, ARM64} }

// Value is the architecture's own name.
func (a Architecture) Value() string {
	if a == ARM64 {
		return "arm64"
	}
	return "amd64"
}

// Platform is the pin's platform key.
func (a Architecture) Platform() string { return "linux/" + a.Value() }

// Host is the GNU host triple.
func (a Architecture) Host() TargetTriple {
	if a == ARM64 {
		return "aarch64-unknown-linux-gnu"
	}
	return "x86_64-unknown-linux-gnu"
}

// Musl is the musl target triple.
func (a Architecture) Musl() TargetTriple {
	if a == ARM64 {
		return "aarch64-unknown-linux-musl"
	}
	return "x86_64-unknown-linux-musl"
}

// UnmarshalText decodes `--arch amd64|arm64`.
func (a *Architecture) UnmarshalText(text []byte) error {
	switch string(text) {
	case "amd64":
		*a = AMD64
	case "arm64":
		*a = ARM64
	default:
		return fmt.Errorf("invalid architecture %q (choose amd64 or arm64)", string(text))
	}
	return nil
}

// String makes the type printable in a diagnostic.
func (a Architecture) String() string { return a.Value() }

// --- parsers ------------------------------------------------------------------

func exactText(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

// ParseSha256 requires a lowercase 64-hex digest.
func ParseSha256(value any, what string) (Sha256, error) {
	text, ok := exactText(value)
	if !ok || !isLowerHex(text, 64) {
		return "", refuse("%s: expected a lowercase SHA-256 hex digest", what)
	}
	return Sha256(text), nil
}

// ParseFingerprint requires an uppercase 40-hex fingerprint.
func ParseFingerprint(value any, what string) (Fingerprint, error) {
	text, ok := exactText(value)
	if !ok || !isUpperHex(text, 40) {
		return "", refuse("%s: expected an uppercase 40-hex OpenPGP fingerprint", what)
	}
	return Fingerprint(text), nil
}

func isLowerHex(text string, width int) bool {
	if len(text) != width {
		return false
	}
	for _, b := range []byte(text) {
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func isUpperHex(text string, width int) bool {
	if len(text) != width {
		return false
	}
	for _, b := range []byte(text) {
		if (b < '0' || b > '9') && (b < 'A' || b > 'F') {
			return false
		}
	}
	return true
}

// isXyz matches `(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` in full.
func isXyz(text string) bool {
	parts := strings.Split(text, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if len(part) > 1 && part[0] == '0' {
			return false
		}
		for _, b := range []byte(part) {
			if b < '0' || b > '9' {
				return false
			}
		}
	}
	return true
}

// ParseChannel requires an exact X.Y.Z release channel.
func ParseChannel(value any, what string) (Channel, error) {
	text, ok := exactText(value)
	if !ok || !isXyz(text) {
		return "", refuse("%s: expected an exact X.Y.Z release channel", what)
	}
	return Channel(text), nil
}

// ParseSemver requires an exact X.Y.Z version.
func ParseSemver(value any, what string) (Semver, error) {
	text, ok := exactText(value)
	if !ok || !isXyz(text) {
		return "", refuse("%s: expected an exact X.Y.Z version", what)
	}
	return Semver(text), nil
}

// ParseDate requires a real calendar date, so 2026-02-30 cannot become an
// identity.
func ParseDate(value any, what string) (IsoDate, error) {
	text, ok := exactText(value)
	if !ok || len(text) != 10 || text[4] != '-' || text[7] != '-' ||
		!allDigits(text[0:4]) || !allDigits(text[5:7]) || !allDigits(text[8:10]) {
		return "", refuse("%s: expected YYYY-MM-DD", what)
	}
	year := atoi(text[0:4])
	month := atoi(text[5:7])
	day := atoi(text[8:10])
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return "", refuse("%s: not a calendar date: %s", what, text)
	}
	if year < 1 {
		return "", refuse("%s: not a calendar date: %s", what, text)
	}
	parsed := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if parsed.Year() != year || int(parsed.Month()) != month || parsed.Day() != day {
		return "", refuse("%s: not a calendar date: %s", what, text)
	}
	return IsoDate(text), nil
}

func allDigits(text string) bool {
	for _, b := range []byte(text) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func atoi(text string) int {
	value := 0
	for _, b := range []byte(text) {
		value = value*10 + int(b-'0')
	}
	return value
}

// isTriple matches `[a-z0-9][a-z0-9_.]*(-[a-z0-9_.]+)+` in full.
func isTriple(text string) bool {
	dash := strings.IndexByte(text, '-')
	if dash <= 0 {
		return false
	}
	for _, b := range []byte(text[:dash]) {
		if !isTripleBody(b) {
			return false
		}
	}
	segments := strings.Split(text[dash+1:], "-")
	for _, segment := range segments {
		if segment == "" {
			return false
		}
		for _, b := range []byte(segment) {
			if !isTripleBody(b) {
				return false
			}
		}
	}
	return true
}

func isTripleBody(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '.'
}

// ParseTriple requires a target triple of at most 64 code points.
func ParseTriple(value any, what string) (TargetTriple, error) {
	text, ok := exactText(value)
	if !ok || utf8.RuneCountInString(text) > MaxTripleRunes || !isTriple(text) {
		return "", refuse("%s: expected a target triple", what)
	}
	return TargetTriple(text), nil
}

// ParseEvidencePath requires a canonical relative path: no empty, `.` or `..`
// segment, no leading slash, no backslash and no NUL.
func ParseEvidencePath(value any, what string) (EvidencePath, error) {
	text, ok := exactText(value)
	if !ok || text == "" || utf8.RuneCountInString(text) > MaxEvidencePathRunes {
		return "", refuse("%s: expected a relative evidence path", what)
	}
	if strings.ContainsAny(text, "\\\x00") || strings.HasPrefix(text, "/") {
		return "", refuse("%s: not a canonical relative path: %s", what, text)
	}
	for _, segment := range strings.Split(text, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", refuse("%s: not a canonical relative path: %s", what, text)
		}
	}
	return EvidencePath(text), nil
}

// isMemberPrefix matches `([A-Za-z0-9_.+-]+)/` in full.
func isMemberPrefix(text string) bool {
	if !strings.HasSuffix(text, "/") {
		return false
	}
	body := text[:len(text)-1]
	if body == "" {
		return false
	}
	for _, b := range []byte(body) {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
			b == '_' || b == '.' || b == '+' || b == '-' {
			continue
		}
		return false
	}
	return true
}

// ParseMemberPrefix requires a single archive member prefix like `name/`.
func ParseMemberPrefix(value any, what string) (MemberPrefix, error) {
	text, ok := exactText(value)
	if !ok || !isMemberPrefix(text) {
		return "", refuse("%s: expected a single archive member prefix like name/", what)
	}
	return MemberPrefix(text), nil
}

// RejectControlCharacters refuses C0, DEL–C1 and surrogate code points in an
// identity string. A lone surrogate arrives as WTF-8 (ED A0..BF xx), which is
// why this decodes by hand: Go's own decoder would fold each byte onto U+FFFD.
func RejectControlCharacters(value, what string) error {
	for offset := 0; offset < len(value); {
		r, size := decodeWTF8(value[offset:])
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0xd800 && r <= 0xdfff) {
			return refuse("%s: control character in identity string", what)
		}
		offset += size
	}
	return nil
}

// decodeWTF8 decodes one code point, accepting the three-byte surrogate form.
func decodeWTF8(text string) (rune, int) {
	b := text[0]
	switch {
	case b < 0x80:
		return rune(b), 1
	case b&0xE0 == 0xC0 && len(text) >= 2:
		return rune(b&0x1F)<<6 | rune(text[1]&0x3F), 2
	case b&0xF0 == 0xE0 && len(text) >= 3:
		return rune(b&0x0F)<<12 | rune(text[1]&0x3F)<<6 | rune(text[2]&0x3F), 3
	case b&0xF8 == 0xF0 && len(text) >= 4:
		return rune(b&0x07)<<18 | rune(text[1]&0x3F)<<12 | rune(text[2]&0x3F)<<6 |
			rune(text[3]&0x3F), 4
	default:
		return utf8.RuneError, 1
	}
}

// ParseURL requires a canonical rust-lang.org HTTPS origin, and equals expected
// when one is given.
//
// It reimplements `urlsplit` rather than using net/url, because the verdict — not
// merely the spelling — has to be the source reader's: scheme and host case are
// folded for the checks while the value keeps its original text, a port or
// userinfo is refused, and the path may not carry a backslash, `//`, `/../` or a
// trailing `/..`.
func ParseURL(value any, what string, expected *string) (CanonicalURL, error) {
	text, ok := exactText(value)
	if !ok || utf8.RuneCountInString(text) > MaxURLRunes {
		return "", refuse("%s: expected a URL string", what)
	}
	split := splitURL(text)
	if split.scheme != "https" || split.host != "static.rust-lang.org" || split.hasPort ||
		split.userinfo || split.query != "" || split.fragment != "" ||
		!strings.HasPrefix(split.path, "/") {
		return "", refuse("%s: not a canonical rust-lang.org HTTPS URL: %s", what, text)
	}
	if strings.Contains(text, "\\") || strings.Contains(split.path, "//") ||
		strings.Contains(split.path, "/../") || strings.HasSuffix(split.path, "/..") {
		return "", refuse("%s: URL path is not canonical: %s", what, text)
	}
	if expected != nil && text != *expected {
		return "", refuse("%s: %s != committed origin %s", what, text, *expected)
	}
	return CanonicalURL(text), nil
}

// urlSplit is the part of `urllib.parse.SplitResult` the canonical-origin rule
// reads. A non-numeric port is a ValueError in the source reader (a crash, P1);
// here hasPort is set and the caller refuses, which keeps the status.
type urlSplit struct {
	scheme   string
	userinfo bool
	host     string
	hasPort  bool
	path     string
	query    string
	fragment string
}

// splitURL mirrors `urlsplit` on the reviewed subset: ASCII tab, CR and LF are
// removed first, the scheme is detected by its own grammar and lower-cased, the
// host is lower-cased, and the userinfo, port, path, query and fragment are
// separated at the same delimiters.
func splitURL(text string) urlSplit {
	text = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(text)
	var split urlSplit
	rest := text
	if colon := strings.IndexByte(rest, ':'); colon > 0 && isAlpha(rest[0]) &&
		isScheme(rest[:colon]) {
		split.scheme = strings.ToLower(rest[:colon])
		rest = rest[colon+1:]
	}
	if strings.HasPrefix(rest, "//") {
		authority := rest[2:]
		if end := strings.IndexAny(authority, "/?#"); end >= 0 {
			split.host, rest = authority[:end], authority[end:]
		} else {
			split.host, rest = authority, ""
		}
		split.readAuthority()
	}
	pathAndMore := rest
	if hash := strings.IndexByte(pathAndMore, '#'); hash >= 0 {
		split.fragment = pathAndMore[hash+1:]
		pathAndMore = pathAndMore[:hash]
	}
	if question := strings.IndexByte(pathAndMore, '?'); question >= 0 {
		split.query = pathAndMore[question+1:]
		pathAndMore = pathAndMore[:question]
	}
	split.path = pathAndMore
	return split
}

func (s *urlSplit) readAuthority() {
	authority := s.host
	hostinfo := authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		s.userinfo = true
		hostinfo = authority[at+1:]
	}
	host := hostinfo
	if strings.HasPrefix(hostinfo, "[") {
		if end := strings.IndexByte(hostinfo, ']'); end >= 0 {
			host = hostinfo[1:end]
			rest := hostinfo[end+1:]
			if strings.HasPrefix(rest, ":") {
				s.hasPort = true
			}
		}
	} else if colon := strings.LastIndexByte(hostinfo, ':'); colon >= 0 {
		host = hostinfo[:colon]
		s.hasPort = true
	}
	// `split.port` is not None for any numeric port, so the canonical rule
	// refuses one whether or not it is 443; only a non-numeric port (a crash in
	// the source reader) needs the distinction HasPort already carries.
	s.host = strings.ToLower(host)
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func isScheme(text string) bool {
	for index, b := range []byte(text) {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		case index > 0 && (b >= '0' && b <= '9' || b == '+' || b == '-' || b == '.'):
		default:
			return false
		}
	}
	return true
}
