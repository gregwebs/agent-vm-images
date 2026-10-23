package document

import (
	"errors"
	"fmt"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

const maxDepth = 100

// jsonReader is a forward cursor over validated UTF-8. It exists because
// encoding/json would replace unpaired surrogates in keys and text, which the
// ADR 0003 raw-record comparison must distinguish.
type jsonReader struct {
	source []byte
	offset int
	line   int
	column int
	plain  bool
}

func parseJSON(source []byte) (Value, error) {
	quoted, escaped := false, false
	for index, c := range source {
		if quoted {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				quoted = false
			}
		} else if c == '"' {
			quoted = true
		} else if c == '\r' && (index+1 == len(source) || source[index+1] != '\n') {
			return Value{}, errors.New("lone CR outside JSON strings is unsupported")
		}
	}
	return readJSON(source, false)
}

// ParsePlainJSON reads developer proof files with JSON.parse semantics, without
// the YAML policies, TextDecoder BOM removal or project depth bound.
func ParsePlainJSON(source []byte) (Value, error) {
	if !utf8.Valid(source) {
		return Value{}, errors.New("invalid UTF-8 in proof JSON")
	}
	return readJSON(source, true)
}

func readJSON(source []byte, plain bool) (Value, error) {
	reader := &jsonReader{source: source, line: 1, column: 1, plain: plain}
	reader.skipSeparation()
	value, err := reader.parseValue(0)
	if err != nil {
		return Value{}, err
	}
	reader.skipSeparation()
	if reader.offset != len(reader.source) {
		return Value{}, reader.failf("unexpected trailing characters")
	}
	return value, nil
}

func (r *jsonReader) failf(format string, args ...any) error {
	return fmt.Errorf("invalid JSON: %s at byte %d", fmt.Sprintf(format, args...), r.offset+1)
}

func (r *jsonReader) peek() (byte, bool) {
	if r.offset >= len(r.source) {
		return 0, false
	}
	return r.source[r.offset], true
}

func (r *jsonReader) advance() byte {
	c := r.source[r.offset]
	r.offset++
	if c == '\n' {
		r.line++
		r.column = 1
	} else {
		r.column++
	}
	return c
}

func (r *jsonReader) skipSeparation() {
	for r.offset < len(r.source) {
		switch r.source[r.offset] {
		case ' ', '\t', '\n', '\r':
			r.advance()
		default:
			return
		}
	}
}

func (r *jsonReader) parseValue(depth int) (Value, error) {
	if !r.plain && depth > maxDepth {
		return Value{}, fmt.Errorf("input exceeds depth100")
	}
	c, ok := r.peek()
	if !ok {
		return Value{}, r.failf("unexpected end of input")
	}
	switch c {
	case '{':
		return r.parseObject(depth)
	case '[':
		return r.parseArray(depth)
	case '"':
		return r.parseStringValue()
	case 't':
		return r.parseLiteral("true", NewBool(true))
	case 'f':
		return r.parseLiteral("false", NewBool(false))
	case 'n':
		return r.parseLiteral("null", NullValue())
	default:
		if c == '-' || (c >= '0' && c <= '9') {
			return r.parseNumber()
		}
		return Value{}, r.failf("unexpected character %q", c)
	}
}

func (r *jsonReader) parseLiteral(literal string, value Value) (Value, error) {
	for index := 0; index < len(literal); index++ {
		c, ok := r.peek()
		if !ok || c != literal[index] {
			return Value{}, r.failf("invalid literal")
		}
		r.advance()
	}
	return value, nil
}

func (r *jsonReader) parseObject(depth int) (Value, error) {
	r.advance()
	entries := make([]NewMappingEntry, 0, 8)
	var seen map[JSString]struct{}
	if !r.plain {
		seen = make(map[JSString]struct{})
	}
	r.skipSeparation()
	if c, ok := r.peek(); ok && c == '}' {
		r.advance()
		return NewMapping(entries...), nil
	}
	for {
		r.skipSeparation()
		c, ok := r.peek()
		if !ok || c != '"' {
			return Value{}, r.failf("expected object key")
		}
		keyLine, keyColumn := r.line, r.column
		key, err := r.parseString()
		if err != nil {
			return Value{}, err
		}
		if !r.plain && key.Equal(mergeKey) {
			return Value{}, errors.New("merge key forbidden")
		}
		if !r.plain {
			if _, exists := seen[key]; exists {
				// The retired parser emitted one duplicate-key text for JSON and
				// YAML alike, with no key text.
				return Value{}, fmt.Errorf("Map keys must be unique at line %d, column %d", keyLine, keyColumn) //nolint:staticcheck // the retired parser text
			}
			seen[key] = struct{}{}
		}
		r.skipSeparation()
		if c, ok := r.peek(); !ok || c != ':' {
			return Value{}, r.failf("expected ':'")
		}
		r.advance()
		r.skipSeparation()
		value, err := r.parseValue(depth + 1)
		if err != nil {
			return Value{}, err
		}
		entries = append(entries, NewMappingEntry{Key: key, Value: value})
		r.skipSeparation()
		c, ok = r.peek()
		if !ok {
			return Value{}, r.failf("unexpected end of input")
		}
		switch c {
		case ',':
			r.advance()
		case '}':
			r.advance()
			return NewMapping(entries...), nil
		default:
			return Value{}, r.failf("expected ',' or '}'")
		}
	}
}

func (r *jsonReader) parseArray(depth int) (Value, error) {
	r.advance()
	items := make([]Value, 0, 8)
	r.skipSeparation()
	if c, ok := r.peek(); ok && c == ']' {
		r.advance()
		return NewSequence(items), nil
	}
	for {
		r.skipSeparation()
		item, err := r.parseValue(depth + 1)
		if err != nil {
			return Value{}, err
		}
		items = append(items, item)
		r.skipSeparation()
		c, ok := r.peek()
		if !ok {
			return Value{}, r.failf("unexpected end of input")
		}
		switch c {
		case ',':
			r.advance()
		case ']':
			r.advance()
			return NewSequence(items), nil
		default:
			return Value{}, r.failf("expected ',' or ']'")
		}
	}
}

func (r *jsonReader) parseStringValue() (Value, error) {
	text, err := r.parseString()
	if err != nil {
		return Value{}, err
	}
	return NewJSString(text), nil
}

// parseString appends UTF-16 code units. A `\uXXXX` escape contributes exactly
// that unit, so an unpaired surrogate stays distinguishable from U+FFFD and from
// a literal astral character.
func (r *jsonReader) parseString() (JSString, error) {
	r.advance()
	units := make([]uint16, 0, 16)
	for {
		if r.offset >= len(r.source) {
			return JSString{}, r.failf("unterminated string")
		}
		c := r.source[r.offset]
		switch {
		case c == '"':
			r.advance()
			return FromUnits(units), nil
		case c < 0x20:
			return JSString{}, r.failf("control character in string")
		case c == '\\':
			r.advance()
			escape, ok := r.peek()
			if !ok {
				return JSString{}, r.failf("unterminated escape")
			}
			r.advance()
			switch escape {
			case '"', '\\', '/':
				units = append(units, uint16(escape))
			case 'b':
				units = append(units, '\b')
			case 'f':
				units = append(units, '\f')
			case 'n':
				units = append(units, '\n')
			case 'r':
				units = append(units, '\r')
			case 't':
				units = append(units, '\t')
			case 'u':
				unit, err := r.parseHexUnit()
				if err != nil {
					return JSString{}, err
				}
				units = append(units, unit)
			default:
				return JSString{}, r.failf("invalid escape")
			}
		case c < utf8.RuneSelf:
			units = append(units, uint16(c))
			r.advance()
		default:
			rune_, size := utf8.DecodeRune(r.source[r.offset:])
			if rune_ == utf8.RuneError && size <= 1 {
				return JSString{}, r.failf("invalid UTF-8")
			}
			if rune_ > 0xffff {
				high, low := utf16.EncodeRune(rune_)
				units = append(units, uint16(high), uint16(low))
			} else {
				units = append(units, uint16(rune_))
			}
			for index := 0; index < size; index++ {
				r.advance()
			}
		}
	}
}

func (r *jsonReader) parseHexUnit() (uint16, error) {
	var unit uint16
	for index := 0; index < 4; index++ {
		c, ok := r.peek()
		if !ok {
			return 0, r.failf("unterminated \\u escape")
		}
		digit, ok := hexValue(c)
		if !ok {
			return 0, r.failf("invalid \\u escape")
		}
		unit = unit<<4 | digit
		r.advance()
	}
	return unit, nil
}

func hexValue(c byte) (uint16, bool) {
	switch {
	case c >= '0' && c <= '9':
		return uint16(c - '0'), true
	case c >= 'a' && c <= 'f':
		return uint16(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return uint16(c-'A') + 10, true
	default:
		return 0, false
	}
}

func (r *jsonReader) parseNumber() (Value, error) {
	start := r.offset
	if c, ok := r.peek(); ok && c == '-' {
		r.advance()
	}
	if c, ok := r.peek(); !ok || c < '0' || c > '9' {
		return Value{}, r.failf("invalid number")
	}
	first, _ := r.peek()
	if first == '0' {
		r.advance()
	} else {
		for {
			c, ok := r.peek()
			if !ok || c < '0' || c > '9' {
				break
			}
			r.advance()
		}
	}
	if c, ok := r.peek(); ok && c == '.' {
		r.advance()
		count := 0
		for {
			c, ok := r.peek()
			if !ok || c < '0' || c > '9' {
				break
			}
			r.advance()
			count++
		}
		if count == 0 {
			return Value{}, r.failf("invalid number")
		}
	}
	if c, ok := r.peek(); ok && (c == 'e' || c == 'E') {
		r.advance()
		if c, ok := r.peek(); ok && (c == '+' || c == '-') {
			r.advance()
		}
		count := 0
		for {
			c, ok := r.peek()
			if !ok || c < '0' || c > '9' {
				break
			}
			r.advance()
			count++
		}
		if count == 0 {
			return Value{}, r.failf("invalid number")
		}
	}
	text := string(r.source[start:r.offset])
	number, err := strconv.ParseFloat(text, 64)
	if err != nil {
		if err.(*strconv.NumError).Err != strconv.ErrRange {
			return Value{}, r.failf("invalid number")
		}
	}
	return NewNumber(number), nil
}
