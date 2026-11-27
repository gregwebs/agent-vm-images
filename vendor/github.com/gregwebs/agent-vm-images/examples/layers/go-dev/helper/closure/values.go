package closure

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Refusal is a closure this reader will not act on. Its message is the contract:
// `check` prints it after "gopls-closure: ", and the shell suites match on those
// fragments, so the wording is the Python reader's.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

func refuse(format string, arguments ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, arguments...)}
}

// The validated domain types. Each one is produced only by its parser, so a
// value that reaches the model has already been shape-checked.
type (
	// ModulePath is a Go module path in the reviewed repository form.
	ModulePath string
	// ModuleVersion is a semantic module version, including pseudo-versions.
	ModuleVersion string
	// ReleaseVersion is an exact vX.Y.Z release, the only shape gopls ships.
	ReleaseVersion string
	// GoVersion is the exact compiler version the pin was produced with.
	GoVersion string
	// GoLanguage is a `go` directive value, which may omit the patch number.
	GoLanguage string
	// H1 is a Go module content checksum, "h1:" and a base64 SHA-256.
	H1 string
	// SHA256 is a lowercase SHA-256 hex digest of file bytes.
	SHA256 string
	// Date is a calendar date, YYYY-MM-DD.
	Date string
	// NoticePath is a canonical slash-separated path inside a module.
	NoticePath string
)

// SignedObject is what a VCS signature covers.
type SignedObject string

const (
	SignedTag    SignedObject = "tag"
	SignedCommit SignedObject = "commit"
)

// ModuleID is one selected module.
type ModuleID struct {
	Path    ModulePath
	Version ModuleVersion
}

func (m ModuleID) String() string { return string(m.Path) + "@" + string(m.Version) }

// Toolchain is the go.mod `toolchain` spelling of a version.
func (v GoVersion) Toolchain() string { return "go" + string(v) }

var (
	// Every pattern is anchored whole, because MatchString searches where the
	// Python reader used re.fullmatch.
	modulePathPattern    = regexp.MustCompile(`^[A-Za-z0-9._~+-]+(/[A-Za-z0-9._~+-]+)+$`)
	moduleVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+incompatible)?$`)
	releasePattern       = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	goVersionPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	goLanguagePattern    = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))?$`)
	h1Pattern            = regexp.MustCompile(`^h1:[A-Za-z0-9+/]{43}=$`)
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	datePattern          = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	objectIDPattern      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	reviewerPattern      = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	noticeNamePattern    = regexp.MustCompile(`(?i)^(LICEN[CS]E|COPYING|NOTICE|PATENTS)([.-].*)?$`)
)

// parseModulePath validates a module path from decoded JSON.
func parseModulePath(value any, what string) (ModulePath, error) {
	text, ok := value.(string)
	if !ok || utf8.RuneCountInString(text) > 200 || !modulePathPattern.MatchString(text) {
		return "", refuse("%s: expected a Go module path", what)
	}
	for _, part := range strings.Split(text, "/") {
		if part == "." || part == ".." {
			return "", refuse("%s: module path contains a relative element", what)
		}
	}
	return ModulePath(text), nil
}

func parseModuleVersion(value any, what string) (ModuleVersion, error) {
	text, ok := value.(string)
	if !ok || utf8.RuneCountInString(text) > 100 || !moduleVersionPattern.MatchString(text) {
		return "", refuse("%s: expected a semantic module version", what)
	}
	return ModuleVersion(text), nil
}

func parseReleaseVersion(value any, what string) (ReleaseVersion, error) {
	text, ok := value.(string)
	if !ok || !releasePattern.MatchString(text) {
		return "", refuse("%s: expected an exact vX.Y.Z release version", what)
	}
	return ReleaseVersion(text), nil
}

func parseGoVersion(value any, what string) (GoVersion, error) {
	text, ok := value.(string)
	if !ok || !goVersionPattern.MatchString(text) {
		return "", refuse("%s: expected an exact X.Y.Z Go version", what)
	}
	return GoVersion(text), nil
}

func parseGoLanguage(value any, what string) (GoLanguage, error) {
	text, ok := value.(string)
	if !ok || !goLanguagePattern.MatchString(text) {
		return "", refuse("%s: expected a Go language version such as 1.26", what)
	}
	return GoLanguage(text), nil
}

func parseH1(value any, what string) (H1, error) {
	text, ok := value.(string)
	if !ok || !h1Pattern.MatchString(text) {
		return "", refuse("%s: expected an h1: checksum", what)
	}
	return H1(text), nil
}

func parseSHA256(value any, what string) (SHA256, error) {
	text, ok := value.(string)
	if !ok || !sha256Pattern.MatchString(text) {
		return "", refuse("%s: expected a lowercase SHA-256 hex digest", what)
	}
	return SHA256(text), nil
}

// parseDate accepts a calendar date, not the shape alone: 2026-02-30 matches the
// pattern and is not a day.
func parseDate(value any, what string) (Date, error) {
	text, ok := value.(string)
	if !ok || !datePattern.MatchString(text) {
		return "", refuse("%s: expected YYYY-MM-DD", what)
	}
	parts := strings.Split(text, "-")
	year, _ := strconv.Atoi(parts[0])
	month, _ := strconv.Atoi(parts[1])
	day, _ := strconv.Atoi(parts[2])
	if !calendarDate(year, month, day) {
		return "", refuse("%s: not a calendar date: %s", what, text)
	}
	return Date(text), nil
}

// calendarDate is the Gregorian day check Python's datetime.date applies.
func calendarDate(year, month, day int) bool {
	if month < 1 || month > 12 || day < 1 {
		return false
	}
	days := []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	limit := days[month-1]
	if month == 2 && (year%4 == 0 && (year%100 != 0 || year%400 == 0)) {
		limit = 29
	}
	return day <= limit
}

func parseNoticePath(value any, what string) (NoticePath, error) {
	text, ok := value.(string)
	if !ok || text == "" || utf8.RuneCountInString(text) > 240 {
		return "", refuse("%s: expected a relative notice path", what)
	}
	if strings.ContainsAny(text, "\\\x00") || strings.HasPrefix(text, "/") {
		return "", refuse("%s: not a canonical relative path: %s", what, text)
	}
	for _, part := range strings.Split(text, "/") {
		if part == "" || part == "." || part == ".." {
			return "", refuse("%s: not a canonical relative path: %s", what, text)
		}
	}
	for _, r := range text {
		if unicode.IsSpace(r) {
			return "", refuse("%s: notice path contains whitespace: %s", what, text)
		}
	}
	return NoticePath(text), nil
}

// rejectControlCharacters refuses an identity string that carries one. The set is
// Python's: C0, DEL and the C1 block.
func rejectControlCharacters(value, what string) error {
	for _, r := range value {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return refuse("%s: control character in an identity string", what)
		}
	}
	return nil
}

// repr renders a string the way Python's repr does for the ASCII keys and values
// these diagnostics name.
func repr(text string) string {
	var out strings.Builder
	out.WriteByte('\'')
	for _, r := range text {
		switch r {
		case '\'':
			out.WriteString(`\'`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&out, `\x%02x`, r)
				continue
			}
			out.WriteRune(r)
		}
	}
	out.WriteByte('\'')
	return out.String()
}

// ParseReleaseVersion, ParseGoVersion, ParseModulePath, ParseModuleVersion and
// ParseH1 are the exported entry points for callers outside this package: the
// first two turn `--expect-*` spellings into assertions, the last three validate
// a module identity another document reports (the build information a binary
// carries, package buildinfo). The loader itself only ever sees the newtypes.
func ParseReleaseVersion(text, what string) (ReleaseVersion, error) {
	return parseReleaseVersion(text, what)
}

func ParseGoVersion(text, what string) (GoVersion, error) {
	return parseGoVersion(text, what)
}

// ParseDate validates a calendar date from a caller's flag spelling.
func ParseDate(text, what string) (Date, error) {
	return parseDate(text, what)
}

// ParseModulePath validates a module path from a decoded document.
func ParseModulePath(value any, what string) (ModulePath, error) {
	return parseModulePath(value, what)
}

// ParseModuleVersion validates a semantic module version from a decoded document.
func ParseModuleVersion(value any, what string) (ModuleVersion, error) {
	return parseModuleVersion(value, what)
}

// ParseH1 validates a module content checksum from a decoded document.
func ParseH1(value any, what string) (H1, error) {
	return parseH1(value, what)
}
