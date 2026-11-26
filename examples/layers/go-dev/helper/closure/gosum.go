package closure

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/strictjson"
)

// decodeUTF8 is the one text decode for the four non-pin inputs.
//
// The diagnostic differs from the Python oracle's UnicodeDecodeError text (which
// names the codec and its reason); both refuse with status 1 and no stdout, and
// the parity suite records the difference per case rather than papering over it.
func decodeUTF8(raw []byte) (string, error) {
	if utf8.Valid(raw) {
		return string(raw), nil
	}
	offset := 0
	for offset < len(raw) {
		r, size := utf8.DecodeRune(raw[offset:])
		if r == utf8.RuneError && size <= 1 {
			break
		}
		offset += size
	}
	return "", fmt.Errorf("invalid byte at offset %d", offset)
}

// decodeJSON is the strict pin document read: BOM, UTF-8, duplicate keys, NaN and
// Infinity are refusals, and the diagnostic names `pin.json` as the Python
// reader does. The decoder's refusal is folded into this package's one refusal
// type, so a caller never has to know which layer refused.
func decodeJSON(raw []byte, what string) (any, error) {
	value, err := strictjson.Decode(raw, what)
	if err != nil {
		var refusal *strictjson.Refusal
		if errors.As(err, &refusal) {
			return nil, &Refusal{Message: refusal.Message}
		}
		return nil, err
	}
	return value, nil
}

// GoSum is the committed checksum set: both kinds for every selected module.
type GoSum struct {
	Zip map[ModuleID]H1
	Mod map[ModuleID]H1
}

// parseGoSum reads go.sum and requires Go's own line order. A go.sum that is not
// in `semver.Compare` order is a hand edit rather than tidy output, so the order
// is evidence and not formatting.
func parseGoSum(raw []byte, what string) (GoSum, error) {
	text, err := decodeText(raw, what)
	if err != nil {
		return GoSum{}, err
	}
	sum := GoSum{Zip: map[ModuleID]H1{}, Mod: map[ModuleID]H1{}}
	keys := []versionKey{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, " ")
		if len(fields) != 3 {
			return GoSum{}, refuse("%s: malformed line: %s", what, repr(line))
		}
		path, err := parseModulePath(fields[0], what+" line")
		if err != nil {
			return GoSum{}, err
		}
		isMod := strings.HasSuffix(fields[1], "/go.mod")
		versionText := strings.TrimSuffix(fields[1], "/go.mod")
		version, err := parseModuleVersion(versionText, what+" line")
		if err != nil {
			return GoSum{}, err
		}
		digest, err := parseH1(fields[2], what+" line")
		if err != nil {
			return GoSum{}, err
		}
		module := ModuleID{Path: path, Version: version}
		keys = append(keys, versionKey{Path: path, Version: version, Mod: isMod})
		target := sum.Zip
		kind := "h1"
		if isMod {
			target, kind = sum.Mod, "go.mod hash"
		}
		if _, duplicate := target[module]; duplicate {
			return GoSum{}, refuse("%s: conflicting %s for %s %s", what, kind, path, version)
		}
		target[module] = digest
	}
	for index := 1; index < len(keys); index++ {
		if compareVersionKeys(keys[index-1], keys[index]) > 0 {
			return GoSum{}, refuse("%s: unsorted or duplicated entries", what)
		}
	}
	return sum, nil
}

// versionKey is go.sum's sort key: module path, then semver, then the zip line
// before the go.mod line.
type versionKey struct {
	Path    ModulePath
	Version ModuleVersion
	Mod     bool
}

// compareVersionKeys reproduces Go's `semver.Compare` order for the shapes
// parse_module_version accepts, including the pre-release ordering the Python
// reader approximates: a pre-release sorts before its release, and pre-release
// parts compare numerically when they are digits.
func compareVersionKeys(left, right versionKey) int {
	if left.Path != right.Path {
		if left.Path < right.Path {
			return -1
		}
		return 1
	}
	if order := compareSemver(left.Version, right.Version); order != 0 {
		return order
	}
	switch {
	case left.Mod == right.Mod:
		return 0
	case left.Mod:
		return 1
	default:
		return -1
	}
}

func compareSemver(left, right ModuleVersion) int {
	leftCore, leftPre, leftHas := splitSemver(string(left))
	rightCore, rightPre, rightHas := splitSemver(string(right))
	for index := range leftCore {
		if leftCore[index] != rightCore[index] {
			if leftCore[index] < rightCore[index] {
				return -1
			}
			return 1
		}
	}
	switch {
	case leftHas == rightHas:
	case leftHas:
		return -1
	default:
		return 1
	}
	for index := 0; index < len(leftPre) && index < len(rightPre); index++ {
		if order := comparePrePart(leftPre[index], rightPre[index]); order != 0 {
			return order
		}
	}
	switch {
	case len(leftPre) < len(rightPre):
		return -1
	case len(leftPre) > len(rightPre):
		return 1
	}
	return 0
}

// prePart is one dot-separated pre-release field: a number when it is all digits,
// a string otherwise, so the two compare as Python's tuples did.
type prePart struct {
	Number  int
	Text    string
	Numeric bool
}

func comparePrePart(left, right prePart) int {
	if left.Numeric != right.Numeric {
		if !left.Numeric {
			return -1
		}
		return 1
	}
	if left.Numeric {
		switch {
		case left.Number < right.Number:
			return -1
		case left.Number > right.Number:
			return 1
		}
		return 0
	}
	return strings.Compare(left.Text, right.Text)
}

// splitSemver separates the three release numbers from an optional pre-release.
func splitSemver(version string) ([3]int, []prePart, bool) {
	text := strings.TrimPrefix(version, "v")
	text = strings.SplitN(text, "+", 2)[0]
	core, pre, hasPre := strings.Cut(text, "-")
	numbers := [3]int{}
	for index, part := range strings.Split(core, ".") {
		if index > 2 {
			break
		}
		value := 0
		for _, digit := range part {
			value = value*10 + int(digit-'0')
		}
		numbers[index] = value
	}
	if !hasPre {
		return numbers, nil, false
	}
	parts := []prePart{}
	for _, field := range strings.Split(pre, ".") {
		numeric := field != ""
		number := 0
		for _, digit := range field {
			if digit < '0' || digit > '9' {
				numeric = false
				break
			}
			number = number*10 + int(digit-'0')
		}
		parts = append(parts, prePart{Number: number, Text: field, Numeric: numeric})
	}
	return numbers, parts, true
}

// h1GoMod is the go.sum go.mod hash of a root module: the SHA-256 of a
// "<file hash>  go.mod\n" line, base64-encoded behind "h1:".
func h1GoMod(raw []byte) H1 {
	inner := sha256.Sum256(raw)
	line := fmt.Sprintf("%x  go.mod\n", inner)
	digest := sha256.Sum256([]byte(line))
	return H1("h1:" + base64.StdEncoding.EncodeToString(digest[:]))
}
