// Package strictjson decodes the JSON pin documents this layer acts on.
//
// It is a copy of examples/layers/rust-dev/helper/internal/strictjson, which is
// itself a copy of the root module's `internal/strictjson` (ADR 0017: an example
// layer is its own module and cannot import `./internal`, and it must not import
// another layer's). The copy is deliberate and the decision is recorded here:
//
//   - the decode *rules* are not a new authority. Until #254 the Python reader
//     (examples/layers/go-dev/gopls_closure.py, decode_json) decides what a pin
//     document may contain, and script/test/gopls-closure-parity.py drives both
//     readers over the same mutants, so a drift between the copies shows up as a
//     disagreement on this layer's data;
//   - extracting a shared package was rejected: two nested modules cannot import
//     one another without a root-level link, and a link would make this layer's
//     build depend on the other layer and put a second copy of the dependency in
//     every vendor tree (ADR 0017, example layers stay self-contained).
//
// What it adds over encoding/json, which keeps the last of two identical keys and
// would let a pin name one version to a reviewer's reader and another to the
// loader that acts on it: a duplicate key at any depth is a refusal, as are a
// trailing document, NaN, Infinity and a raw control character in a string. A
// leading UTF-8 BOM is refused, as Python's decode_json refuses it. Numbers keep
// their literal and whether it is a whole number, because the source reader
// applies `type(x) is int` to `schema` — `1.0`, `1e3` and `true` are all "expected
// an integer", and `-0` is the integer zero. Nesting has a depth bound, so a deep
// document is a refusal rather than a stack overflow (the source reader raises
// RecursionError there: a declared difference, status 1 either way). Two different
// lone surrogates stay two different strings (WTF-8), so neither can be forged
// into the other or into a duplicate key.
//
// The diagnostic fragments are the source reader's ("not valid JSON", "not valid
// UTF-8", "duplicate key", "leading UTF-8 BOM"), because the parity suite matches
// them.
package strictjson

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Refusal is a document this layer will not act on. The message is the contract:
// after "rust_toolchain: " it is byte-for-byte the source reader's diagnostic.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

// refuse builds a Refusal.
func refuse(format string, arguments ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, arguments...)}
}

// Value is a decoded JSON object, array, string, Number, bool or nil.
type Value = any

// Number is one JSON number literal, kept as written.
//
// The pin's `schema` is the only numeric field, and the source reader's rule is
// `type(value) is int`: a fraction, an exponent, a `true` or a string is not an
// integer whatever it evaluates to, so the literal has to survive the decode.
type Number struct {
	Literal string
	Integer bool
}

// maxDepth bounds nesting. No reviewed pin comes near it; a document that does
// is refused rather than recursed into.
const maxDepth = 512

// Decode reads one complete JSON document. what names the document in every
// diagnostic (the source reader's `pin.json`).
func Decode(raw []byte, what string) (Value, error) {
	if len(raw) >= 3 && raw[0] == 0xEF && raw[1] == 0xBB && raw[2] == 0xBF {
		return nil, refuse("%s: leading UTF-8 BOM is not accepted", what)
	}
	if !utf8.Valid(raw) {
		return nil, refuse("%s: not valid UTF-8: invalid byte at offset %d", what,
			firstInvalidUTF8(raw))
	}
	cursor := &cursor{source: raw, what: what}
	value, err := cursor.value(1)
	if err != nil {
		return nil, err
	}
	cursor.space()
	if cursor.offset != len(cursor.source) {
		return nil, cursor.errorf("extra data after the document")
	}
	return value, nil
}

// firstInvalidUTF8 is the offset the diagnostic names.
func firstInvalidUTF8(raw []byte) int {
	for offset := 0; offset < len(raw); {
		r, size := utf8.DecodeRune(raw[offset:])
		if r == utf8.RuneError && size <= 1 {
			return offset
		}
		offset += size
	}
	return len(raw)
}

type cursor struct {
	source []byte
	offset int
	what   string
}

// errorf reports a syntax problem the way the source reader's decoder does:
// "not valid JSON" and the position.
func (c *cursor) errorf(format string, arguments ...any) error {
	line, column := 1, 1
	for _, b := range c.source[:c.offset] {
		if b == '\n' {
			line++
			column = 1
			continue
		}
		column++
	}
	return refuse("%s: not valid JSON: %s: line %d column %d (char %d)", c.what,
		fmt.Sprintf(format, arguments...), line, column, c.offset)
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

func (c *cursor) value(depth int) (Value, error) {
	if depth > maxDepth {
		return nil, c.errorf("the document nests deeper than %d levels", maxDepth)
	}
	c.space()
	if c.offset == len(c.source) {
		return nil, c.errorf("expecting a value")
	}
	switch b := c.source[c.offset]; {
	case b == '{':
		return c.object(depth)
	case b == '[':
		return c.array(depth)
	case b == '"':
		return c.requireText()
	case b == 't':
		return c.literal("true", true)
	case b == 'f':
		return c.literal("false", false)
	case b == 'n':
		return c.literal("null", nil)
	case b == 'N' || b == 'I':
		return nil, refuse("%s: %s is not valid JSON", c.what, c.constant())
	case b == '-':
		if c.offset+1 < len(c.source) && c.source[c.offset+1] >= '0' &&
			c.source[c.offset+1] <= '9' {
			return c.number()
		}
		// The source reader's parse_constant sees the whole word, minus sign
		// included, so the refusal names it that way.
		if strings.HasPrefix(string(c.source[c.offset:]), "-Infinity") {
			c.offset += len("-Infinity")
			return nil, refuse("%s: %s is not valid JSON", c.what, "-Infinity")
		}
		return nil, c.errorf("expecting a value")
	case b >= '0' && b <= '9':
		return c.number()
	default:
		return nil, c.errorf("expecting a value, found %q", rune(b))
	}
}

// constant reads the word at the cursor, for a refusal that names it: JSON has
// no NaN and no Infinity, and Python's decoder accepts both as constants.
func (c *cursor) constant() string {
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

func (c *cursor) literal(word string, value Value) (Value, error) {
	if !strings.HasPrefix(string(c.source[c.offset:]), word) {
		return nil, c.errorf("expecting a value, found %q", rune(c.source[c.offset]))
	}
	c.offset += len(word)
	return value, nil
}

// number reads one JSON number literal and whether it is a whole number. The
// grammar is JSON's, not Python's: a leading zero is refused, and a fraction or
// exponent keeps its literal so the caller can apply `type(x) is int`.
func (c *cursor) number() (Value, error) {
	start := c.offset
	if c.source[c.offset] == '-' {
		c.offset++
	}
	if c.offset < len(c.source) && c.source[c.offset] == '0' {
		c.offset++
		if c.offset < len(c.source) && c.source[c.offset] >= '0' && c.source[c.offset] <= '9' {
			return nil, c.errorf("numbers may not have a leading zero")
		}
	} else {
		digits := 0
		for c.offset < len(c.source) && c.source[c.offset] >= '0' && c.source[c.offset] <= '9' {
			c.offset++
			digits++
		}
		if digits == 0 {
			return nil, c.errorf("expecting a value")
		}
	}
	integer := true
	if c.offset < len(c.source) && c.source[c.offset] == '.' {
		integer = false
		c.offset++
		digits := 0
		for c.offset < len(c.source) && c.source[c.offset] >= '0' && c.source[c.offset] <= '9' {
			c.offset++
			digits++
		}
		if digits == 0 {
			return nil, c.errorf("expecting a digit after the decimal point")
		}
	}
	if c.offset < len(c.source) && (c.source[c.offset] == 'e' || c.source[c.offset] == 'E') {
		integer = false
		c.offset++
		if c.offset < len(c.source) && (c.source[c.offset] == '+' || c.source[c.offset] == '-') {
			c.offset++
		}
		digits := 0
		for c.offset < len(c.source) && c.source[c.offset] >= '0' && c.source[c.offset] <= '9' {
			c.offset++
			digits++
		}
		if digits == 0 {
			return nil, c.errorf("expecting a digit in the exponent")
		}
	}
	return Number{Literal: string(c.source[start:c.offset]), Integer: integer}, nil
}

func (c *cursor) object(depth int) (Value, error) {
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
			return nil, c.errorf("expecting a string key")
		}
		key, err := c.requireText()
		if err != nil {
			return nil, err
		}
		name := key.(string)
		c.space()
		if c.offset == len(c.source) || c.source[c.offset] != ':' {
			return nil, c.errorf("expecting ':'")
		}
		c.offset++
		// The depth counts containers, so a member's value is one level down.
		value, err := c.value(depth + 1)
		if err != nil {
			return nil, err
		}
		if _, duplicate := members[name]; duplicate {
			return nil, refuse("%s: duplicate key %s", c.what, repr(name))
		}
		members[name] = value
		c.space()
		if c.offset == len(c.source) {
			return nil, c.errorf("expecting ',' or '}'")
		}
		switch c.source[c.offset] {
		case ',':
			c.offset++
		case '}':
			c.offset++
			return members, nil
		default:
			return nil, c.errorf("expecting ',' or '}'")
		}
	}
}

func (c *cursor) array(depth int) (Value, error) {
	c.offset++ // '['
	items := []Value{}
	c.space()
	if c.offset < len(c.source) && c.source[c.offset] == ']' {
		c.offset++
		return items, nil
	}
	for {
		item, err := c.value(depth + 1)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		c.space()
		if c.offset == len(c.source) {
			return nil, c.errorf("expecting ',' or ']'")
		}
		switch c.source[c.offset] {
		case ',':
			c.offset++
		case ']':
			c.offset++
			return items, nil
		default:
			return nil, c.errorf("expecting ',' or ']'")
		}
	}
}

func (c *cursor) requireText() (Value, error) {
	c.offset++ // opening quote
	var out strings.Builder
	for {
		if c.offset == len(c.source) {
			return nil, c.errorf("unterminated string")
		}
		b := c.source[c.offset]
		switch {
		case b == '"':
			c.offset++
			return out.String(), nil
		case b < 0x20:
			return nil, c.errorf("invalid control character in a string")
		case b != '\\':
			out.WriteByte(b)
			c.offset++
		default:
			c.offset++
			if err := c.escape(&out); err != nil {
				return nil, err
			}
		}
	}
}

func (c *cursor) escape(out *strings.Builder) error {
	if c.offset == len(c.source) {
		return c.errorf("unterminated escape")
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
		unit, err := c.hex4()
		if err != nil {
			return err
		}
		if utf16.IsSurrogate(rune(unit)) && unit < 0xDC00 && c.offset+1 < len(c.source) &&
			c.source[c.offset] == '\\' && c.source[c.offset+1] == 'u' {
			c.offset += 2
			low, err := c.hex4()
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
		return c.errorf("invalid escape %q", rune(b))
	}
	return nil
}

func (c *cursor) hex4() (uint16, error) {
	if c.offset+4 > len(c.source) {
		return 0, c.errorf("truncated \\u escape")
	}
	digits := string(c.source[c.offset : c.offset+4])
	value, err := strconv.ParseUint(digits, 16, 32)
	if err != nil {
		return 0, c.errorf("invalid \\u escape %q", digits)
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

// repr renders a string the way Python's repr does for the simple ASCII keys and
// values these diagnostics name (single quotes, backslash escapes).
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
			if r >= 0xD800 && r <= 0xDFFF {
				fmt.Fprintf(&out, `\u%04x`, r)
				continue
			}
			out.WriteRune(r)
		}
	}
	out.WriteByte('\'')
	return out.String()
}

// Repr renders a string the way Python's repr does, for a diagnostic that names
// a value (an installed setting, a channel).
func Repr(text string) string { return repr(text) }
