package pnpmlock

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// jsSpace is JavaScript's `\s`. Go's `\s` is ASCII-only and
// strings.TrimSpace disagrees in both directions (it strips U+0085 and keeps
// nothing else), so the class is written out once and reused.
const jsSpace = `[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

const (
	partial = `(?:0|[1-9][0-9]*|[xX*])(?:\.(?:0|[1-9][0-9]*|[xX*])){0,2}` +
		`(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
		`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?`
	comparatorTerm = `(?:\^|~|>=|<=|>|<|=)?` + jsSpace + `*` + partial
)

var (
	exactVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?` +
		`(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	registryNamePattern = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
	comparatorSet       = regexp.MustCompile(`^` + comparatorTerm + `(?:` + jsSpace + `+` + comparatorTerm + `)*$`)
	hyphenRange         = regexp.MustCompile(`^` + partial + jsSpace + `+` + `-` + jsSpace + `+` + partial + `$`)
	alternativeSplit    = regexp.MustCompile(jsSpace + `*\|\|` + jsSpace + `*`)
	leadingComparator   = regexp.MustCompile(`^=` + jsSpace + `*`)
	spaceRun            = regexp.MustCompile(jsSpace + `+`)
	familyTerm          = regexp.MustCompile(`^(\^|~|>=|<=|>|<|=)?((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))$`)
	spaceSet            = func() map[rune]bool {
		set := map[rune]bool{}
		for _, r := range "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff" {
			set[r] = true
		}
		return set
	}()
)

// ExactVersion is a canonical exact semver string.
type ExactVersion struct{ value string }

func (v ExactVersion) String() string { return v.value }

// ParseExactVersion validates the version syntax the policies accept.
func ParseExactVersion(text string) (ExactVersion, error) {
	if !exactVersionPattern.MatchString(text) || strings.Contains(text, "\n") {
		return ExactVersion{}, errorf("not canonical exact semver: %s", text)
	}
	return ExactVersion{value: text}, nil
}

func exactVersionOf(value document.Value) (ExactVersion, error) {
	text, err := textOf(value)
	if err != nil {
		return ExactVersion{}, err
	}
	raw := displayOf(text)
	if !exactVersionPattern.MatchString(raw) || strings.Contains(raw, "\n") {
		return ExactVersion{}, errorf("not canonical exact semver: %s", raw)
	}
	return ExactVersion{value: raw}, nil
}

// PackageName is a registry package name.
type PackageName struct{ value string }

func (n PackageName) String() string { return n.value }

// ParsePackageName validates a registry package name.
func ParsePackageName(text string) (PackageName, error) {
	if !registryNamePattern.MatchString(text) || strings.Contains(text, "\n") || len(text) > 214 {
		return PackageName{}, errorf("not registry package name: %s", text)
	}
	return PackageName{value: text}, nil
}

func packageNameOf(value document.Value) (PackageName, error) {
	text, err := textOf(value)
	if err != nil {
		return PackageName{}, err
	}
	raw := displayOf(text)
	if !registryNamePattern.MatchString(raw) || strings.Contains(raw, "\n") || text.Len() > 214 {
		return PackageName{}, errorf("not registry package name: %s", raw)
	}
	return PackageName{value: raw}, nil
}

// trimJS is String.prototype.trim.
func trimJS(text string) string {
	return strings.TrimFunc(text, func(r rune) bool { return spaceSet[r] })
}

// exactDeclaration reports the exact version a peer specification pins, or false
// when it is not an exact declaration at all. `=X`, `= X`, `vX` and surrounding
// whitespace all pin one version; the alternative spelling is stripped before the
// exact test so a spaced comparator cannot fall through to the range grammar.
func exactDeclaration(wanted string) (string, bool) {
	pin := trimJS(leadingComparator.ReplaceAllString(trimJS(wanted), ""))
	pin = strings.TrimPrefix(pin, "v")
	if exactVersionPattern.MatchString(pin) && !strings.Contains(pin, "\n") {
		return pin, true
	}
	return "", false
}

func rangeDeclaration(wanted string) bool {
	spec := trimJS(wanted)
	if spec == "" {
		return false
	}
	for _, alternative := range alternativeSplit.Split(spec, -1) {
		trimmed := trimJS(alternative)
		if trimmed == "" || (!hyphenRange.MatchString(trimmed) && !comparatorSet.MatchString(trimmed)) {
			return false
		}
	}
	return true
}

// peerSpecClass classifies a specification, stopping on syntax outside the
// reviewed grammar instead of silently skipping an unrecognised pin.
func peerSpecClass(wanted string) (specClass, error) {
	if _, ok := exactDeclaration(wanted); ok {
		return specExact, nil
	}
	if !rangeDeclaration(wanted) {
		return specExact, errorf("unknown peer specification: %s", wanted)
	}
	return specRange, nil
}

type specClass uint8

const (
	specExact specClass = iota
	specRange
)

// satisfiesFamilyRange is deliberately conservative: unprovable syntax is a
// review stop, never a permissive fallback or a reason to pick another version.
func satisfiesFamilyRange(selected ExactVersion, wanted string) (bool, error) {
	if pin, ok := exactDeclaration(wanted); ok {
		return selected.value == pin, nil
	}
	if strings.ContainsAny(selected.value, "-+") {
		return false, errorf("unprovable family range: %s", wanted)
	}
	parts, err := familyParts(selected.value, wanted)
	if err != nil {
		return false, err
	}
	any := false
	for _, alternative := range alternativeSplit.Split(wanted, -1) {
		terms := splitJSSpace(trimJS(alternative))
		all := true
		for _, term := range terms {
			matched, err := familyTermMatches(parts, term, wanted)
			if err != nil {
				return false, err
			}
			if !matched {
				all = false
			}
		}
		any = any || all
	}
	return any, nil
}

func familyParts(selected, wanted string) ([3]uint64, error) {
	var parts [3]uint64
	for index, component := range strings.Split(selected, ".") {
		if index > 2 {
			return parts, errorf("unprovable family range: %s", wanted)
		}
		value, ok := safeInteger(component)
		if !ok {
			return parts, errorf("unprovable family range: %s", wanted)
		}
		parts[index] = value
	}
	return parts, nil
}

func familyTermMatches(parts [3]uint64, term, wanted string) (bool, error) {
	match := familyTerm.FindStringSubmatch(term)
	if match == nil {
		return false, errorf("unprovable family range: %s", wanted)
	}
	var lower [3]uint64
	for index, component := range strings.Split(match[2], ".") {
		value, ok := safeInteger(component)
		if !ok {
			return false, errorf("unprovable family range: %s", wanted)
		}
		lower[index] = value
	}
	comparison := compareTriples(parts, lower)
	operator := match[1]
	if operator == "" {
		operator = "="
	}
	switch operator {
	case "=":
		return comparison == 0, nil
	case ">=":
		return comparison >= 0, nil
	case "<=":
		return comparison <= 0, nil
	case ">":
		return comparison > 0, nil
	case "<":
		return comparison < 0, nil
	case "~":
		if !safeIntegerValue(lower[1] + 1) {
			return false, errorf("unprovable family range: %s", wanted)
		}
		return comparison >= 0 && compareTriples(parts, [3]uint64{lower[0], lower[1] + 1, 0}) < 0, nil
	case "^":
		upper := [3]uint64{lower[0] + 1, 0, 0}
		if lower[0] == 0 {
			if lower[1] == 0 {
				upper = [3]uint64{0, 0, lower[2] + 1}
			} else {
				upper = [3]uint64{0, lower[1] + 1, 0}
			}
		}
		if !safeIntegerValue(upper[0]) || !safeIntegerValue(upper[1]) || !safeIntegerValue(upper[2]) {
			return false, errorf("unprovable family range: %s", wanted)
		}
		return comparison >= 0 && compareTriples(parts, upper) < 0, nil
	default:
		return false, errorf("unprovable family range: %s", wanted)
	}
}

func compareTriples(a, b [3]uint64) int {
	for index := 0; index < 3; index++ {
		if a[index] != b[index] {
			if a[index] < b[index] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// maxSafeInteger is Number.MAX_SAFE_INTEGER: a larger component cannot be
// compared as a double without loss, so the range is unprovable.
const maxSafeInteger = 1<<53 - 1

func safeInteger(text string) (uint64, bool) {
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value > maxSafeInteger {
		return 0, false
	}
	return value, true
}

func safeIntegerValue(value uint64) bool { return value <= maxSafeInteger }

// splitJSSpace reproduces `s.split(/\s+/)`: an empty string contributes one
// empty field, which strings.Fields would drop.
func splitJSSpace(text string) []string {
	if text == "" {
		return []string{""}
	}
	return spaceRun.Split(text, -1)
}
