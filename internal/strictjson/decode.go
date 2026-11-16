// Package strictjson decodes the JSON documents a reviewed pin is made of.
//
// encoding/json keeps the last of two identical keys — and, for structs, the first —
// so a pin could name one version to a reviewer's reader and another to the loader
// that acts on it. This decoder refuses a duplicate key, a trailing document, a
// fraction, an exponent, NaN and Infinity, and returns whole numbers as int64.
//
// It is shared by the artifact pins and the Claude pin because a loader that accepts
// what the other refuses would be a second, quieter policy.
package strictjson

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Refusal is a pin, archive or evidence file this repository will not act on.
// The message is part of the contract: the suites match diagnostic fragments.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

// refuse builds a Refusal.
// Refuse builds a Refusal from a format and its arguments.
func Refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a refusal rather than an I/O or programming
// failure, so callers can map it onto the refusal exit status.
func IsRefusal(err error) bool {
	var refusal *Refusal
	return errors.As(err, &refusal)
}

// Value is a decoded JSON object, array, string, bool, int64 or nil.
type Value = any

// strictJSON decodes one complete JSON document with no duplicate keys and no
// trailing input.
//
// encoding/json keeps the last of two identical keys — and, for structs, the
// first — so a pin could say one version to the reviewer's reader and another to
// this loader. Numbers are refused wholesale because no field a build acts on is
// numeric, and a literal like 1e400 is how a value slips past a comparison.
func Decode(raw []byte, what string) (Value, error) {
	if !utf8.Valid(raw) {
		return nil, Refuse("%s is not UTF-8", what)
	}
	cursor := &cursor{source: raw}
	value, err := cursor.value(what)
	if err != nil {
		return nil, err
	}
	cursor.space()
	if cursor.offset != len(cursor.source) {
		return nil, Refuse("%s has trailing input after the JSON document", what)
	}
	return value, nil
}

type cursor struct {
	source []byte
	offset int
}

func (c *cursor) space() {
	for c.offset < len(c.source) {
		switch c.source[c.offset] {
		case ' ', '\t', '\n', '\r':
			c.offset++
		default:
			return
		}
	}
}

func (c *cursor) value(what string) (Value, error) {
	c.space()
	if c.offset == len(c.source) {
		return nil, Refuse("%s is not JSON: unexpected end of input", what)
	}
	switch b := c.source[c.offset]; {
	case b == '{':
		return c.object(what)
	case b == '[':
		return c.array(what)
	case b == '"':
		return c.requireText(what)
	case b == 't':
		return c.literal("true", true, what)
	case b == 'f':
		return c.literal("false", false, what)
	case b == 'n':
		if strings.HasPrefix(string(c.source[c.offset:]), "null") {
			return c.literal("null", nil, what)
		}
		return nil, Refuse("%s is not a JSON number this build acts on",
			c.constant(what))
	case b == 'N' || b == 'I':
		return nil, Refuse("%s is not a JSON number this build acts on",
			c.constant(what))
	case b == '-' || (b >= '0' && b <= '9'):
		return c.number(what)
	default:
		return nil, Refuse("%s is not JSON: unexpected %q", what, rune(b))
	}
}

// constant reads the word at the cursor, for a refusal that names it: JSON has no
// NaN and no Infinity, and Python's decoder accepts both.
func (c *cursor) constant(what string) string {
	start := c.offset
	for c.offset < len(c.source) {
		b := c.source[c.offset]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') {
			c.offset++
			continue
		}
		break
	}
	if c.offset == start {
		return string(c.source[start])
	}
	return string(c.source[start:c.offset])
}

func (c *cursor) literal(word string, value Value, what string) (Value, error) {
	if !strings.HasPrefix(string(c.source[c.offset:]), word) {
		return nil, Refuse("%s is not JSON: unexpected %q", what, rune(c.source[c.offset]))
	}
	c.offset += len(word)
	return value, nil
}

// number reads one JSON number literal. A fraction or an exponent is refused
// rather than rounded: a float or a `NaN`/`Infinity` constant is not a value any
// reviewed field carries, and 1e400 is how one slips past an integer comparison.
func (c *cursor) number(what string) (Value, error) {
	start := c.offset
	fraction := false
	if c.offset < len(c.source) && c.source[c.offset] == '-' {
		c.offset++
	}
	// JSON has one spelling for a whole number: a single zero, or digits that do not
	// begin with zero. `01` is a literal Python's decoder refuses, and a reader that
	// accepted it would accept a document the review never produced.
	if c.offset < len(c.source) && c.source[c.offset] == '0' {
		c.offset++
		if c.offset < len(c.source) && c.source[c.offset] >= '0' && c.source[c.offset] <= '9' {
			return nil, Refuse("%s is not a JSON number this build acts on",
				c.constant(what))
		}
	}
	for c.offset < len(c.source) {
		b := c.source[c.offset]
		switch {
		case b >= '0' && b <= '9':
			c.offset++
		case b == '.', b == 'e', b == 'E', b == '+', b == '-':
			fraction = true
			c.offset++
		default:
			literal := string(c.source[start:c.offset])
			if fraction {
				return nil, Refuse("%s has a fractional part, which is not a JSON number "+
					"this build acts on", literal)
			}
			integer, err := strconv.ParseInt(literal, 10, 64)
			if err != nil {
				return nil, Refuse("%s is not a JSON number this build acts on", literal)
			}
			return integer, nil
		}
	}
	literal := string(c.source[start:c.offset])
	if fraction {
		return nil, Refuse("%s has a fractional part, which is not a JSON number "+
			"this build acts on", literal)
	}
	integer, err := strconv.ParseInt(literal, 10, 64)
	if err != nil {
		return nil, Refuse("%s is not a JSON number this build acts on", literal)
	}
	return integer, nil
}

func (c *cursor) object(what string) (Value, error) {
	c.offset++ // '{'
	members := make(map[string]Value)
	c.space()
	if c.offset < len(c.source) && c.source[c.offset] == '}' {
		c.offset++
		return members, nil
	}
	for {
		c.space()
		if c.offset == len(c.source) || c.source[c.offset] != '"' {
			return nil, Refuse("%s is not JSON: expected a string key", what)
		}
		key, err := c.requireText(what)
		if err != nil {
			return nil, err
		}
		name := key.(string)
		c.space()
		if c.offset == len(c.source) || c.source[c.offset] != ':' {
			return nil, Refuse("%s is not JSON: expected ':'", what)
		}
		c.offset++
		value, err := c.value(what)
		if err != nil {
			return nil, err
		}
		if _, duplicate := members[name]; duplicate {
			return nil, Refuse("duplicate JSON key %q", name)
		}
		members[name] = value
		c.space()
		if c.offset == len(c.source) {
			return nil, Refuse("%s is not JSON: unexpected end of input", what)
		}
		switch c.source[c.offset] {
		case ',':
			c.offset++
		case '}':
			c.offset++
			return members, nil
		default:
			return nil, Refuse("%s is not JSON: expected ',' or '}'", what)
		}
	}
}

func (c *cursor) array(what string) (Value, error) {
	c.offset++ // '['
	items := []Value{}
	c.space()
	if c.offset < len(c.source) && c.source[c.offset] == ']' {
		c.offset++
		return items, nil
	}
	for {
		item, err := c.value(what)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		c.space()
		if c.offset == len(c.source) {
			return nil, Refuse("%s is not JSON: unexpected end of input", what)
		}
		switch c.source[c.offset] {
		case ',':
			c.offset++
		case ']':
			c.offset++
			return items, nil
		default:
			return nil, Refuse("%s is not JSON: expected ',' or ']'", what)
		}
	}
}

func (c *cursor) requireText(what string) (Value, error) {
	c.offset++ // opening quote
	var out strings.Builder
	for {
		if c.offset == len(c.source) {
			return nil, Refuse("%s is not JSON: unterminated string", what)
		}
		b := c.source[c.offset]
		switch {
		case b == '"':
			c.offset++
			return out.String(), nil
		case b < 0x20:
			return nil, Refuse("%s is not JSON: control character in string", what)
		case b != '\\':
			out.WriteByte(b)
			c.offset++
		default:
			c.offset++
			if err := c.escape(&out, what); err != nil {
				return nil, err
			}
		}
	}
}

func (c *cursor) escape(out *strings.Builder, what string) error {
	if c.offset == len(c.source) {
		return Refuse("%s is not JSON: unterminated escape", what)
	}
	b := c.source[c.offset]
	c.offset++
	switch b {
	case '"', '\\', '/':
		out.WriteByte(b)
	case 'b':
		out.WriteByte('\b')
	case 'f':
		out.WriteByte('\f')
	case 'n':
		out.WriteByte('\n')
	case 'r':
		out.WriteByte('\r')
	case 't':
		out.WriteByte('\t')
	case 'u':
		unit, err := c.hex4(what)
		if err != nil {
			return err
		}
		if utf16.IsSurrogate(rune(unit)) && unit < 0xDC00 && c.offset+1 < len(c.source) &&
			c.source[c.offset] == '\\' && c.source[c.offset+1] == 'u' {
			c.offset += 2
			low, err := c.hex4(what)
			if err != nil {
				return err
			}
			if decoded := utf16.DecodeRune(rune(unit), rune(low)); decoded != utf8.RuneError {
				out.WriteRune(decoded)
				return nil
			}
			// An unpaired pair: keep both halves distinct rather than collapsing
			// them onto one replacement character, which could forge a duplicate
			// key out of two different ones.
			writeWTF8(out, rune(unit))
			writeWTF8(out, rune(low))
			return nil
		}
		if utf16.IsSurrogate(rune(unit)) {
			writeWTF8(out, rune(unit))
			return nil
		}
		out.WriteRune(rune(unit))
	default:
		return Refuse("%s is not JSON: invalid escape %q", what, rune(b))
	}
	return nil
}

func (c *cursor) hex4(what string) (uint16, error) {
	if c.offset+4 > len(c.source) {
		return 0, Refuse("%s is not JSON: truncated \\u escape", what)
	}
	digits := string(c.source[c.offset : c.offset+4])
	value, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return 0, Refuse("%s is not JSON: invalid \\u escape %q", what, digits)
	}
	c.offset += 4
	return uint16(value), nil
}

// writeWTF8 writes one unit as a standalone surrogate, so two different
// surrogates stay two different strings.
func writeWTF8(out *strings.Builder, unit rune) {
	out.WriteByte(byte(0xE0 | unit>>12))
	out.WriteByte(byte(0x80 | (unit>>6)&0x3F))
	out.WriteByte(byte(0x80 | unit&0x3F))
}

// --- strict accessors -------------------------------------------------------

// Fields requires an object with exactly keys, reporting what is missing and
// what is unknown in the order the caller declared them.
func Fields(value Value, keys []string, what string) (map[string]Value, error) {
	members, ok := value.(map[string]Value)
	if !ok {
		return nil, Refuse("%s: expected an object", what)
	}
	var missing, unknown []string
	for _, key := range keys {
		if _, present := members[key]; !present {
			missing = append(missing, key)
		}
	}
	for key := range members {
		if !Contains(keys, key) {
			unknown = append(unknown, key)
		}
	}
	if len(missing) != 0 {
		return nil, Refuse("%s: missing %s", what, strings.Join(missing, ", "))
	}
	if len(unknown) != 0 {
		sort.Strings(unknown)
		return nil, Refuse("%s: unknown %s", what, strings.Join(unknown, ", "))
	}
	return members, nil
}

// FieldsExactly is Fields for a document whose every member is reviewed: the refusal
// names the whole expected set as well as what is missing and what is unexpected, so
// a reader sees the shape the review accepted rather than only the surprise.
func FieldsExactly(value Value, keys []string, what string) (map[string]Value, error) {
	members, ok := value.(map[string]Value)
	if !ok {
		return nil, Refuse("%s must be a JSON object", what)
	}
	var missing, unknown []string
	for _, key := range keys {
		if _, present := members[key]; !present {
			missing = append(missing, key)
		}
	}
	for key := range members {
		if !Contains(keys, key) {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	if len(missing) != 0 || len(unknown) != 0 {
		return nil, Refuse("%s must have exactly [%s]; missing [%s], unexpected [%s]",
			what, strings.Join(keys, " "), strings.Join(missing, " "),
			strings.Join(unknown, " "))
	}
	return members, nil
}

// Contains reports whether values holds value.
func Contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func List(value Value, what string) ([]Value, error) {
	items, ok := value.([]Value)
	if !ok || items == nil {
		return nil, Refuse("%s: expected a list", what)
	}
	return items, nil
}

func Text(value Value, what string) (string, error) {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", Refuse("%s: expected a non-empty string", what)
	}
	return text, nil
}

// integer is the one numeric shape a reviewed field takes.
func Integer(value Value, what string) (int64, error) {
	number, ok := value.(int64)
	if !ok {
		return 0, Refuse("%s: expected an integer, got %v", what, value)
	}
	return number, nil
}

func Bool(value Value, what string) (bool, error) {
	flag, ok := value.(bool)
	if !ok {
		return false, Refuse("%s: expected a boolean", what)
	}
	return flag, nil
}
