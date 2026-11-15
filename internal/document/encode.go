package document

import (
	"math"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// AppendJS emits JSON.stringify(v). The encoder is hand-written because
// encoding/json would HTML-escape `<`, `>` and `&` and would replace unpaired
// surrogates, and both changes are visible in stdout bytes and in raw-record
// comparison.
func (v Value) AppendJS(dst []byte) []byte {
	switch v.kind {
	case Undefined:
		// JSON.stringify drops undefined object members; the callers never emit
		// one at the top level, and `null` is the sequence-position rendering.
		return append(dst, "null"...)
	case Null:
		return append(dst, "null"...)
	case Bool:
		if v.boolean {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case Number:
		return appendJSNumber(dst, v.number)
	case String:
		return appendJSString(dst, v.text)
	case Sequence:
		dst = append(dst, '[')
		for index, item := range v.items {
			if index > 0 {
				dst = append(dst, ',')
			}
			dst = item.AppendJS(dst)
		}
		return append(dst, ']')
	case Mapping:
		dst = append(dst, '{')
		for index, key := range v.Keys() {
			if index > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSString(dst, key)
			dst = append(dst, ':')
			dst = v.mapping.values[key].AppendJS(dst)
		}
		return append(dst, '}')
	default:
		panic("document: unhandled value kind")
	}
}

// AppendJSIndent emits JSON.stringify(v, null, indent).
func (v Value) AppendJSIndent(dst []byte, indent string) []byte {
	return v.appendJSLevel(dst, indent, "")
}

func (v Value) appendJSLevel(dst []byte, indent, prefix string) []byte {
	empty := (v.kind == Sequence && len(v.items) == 0) || (v.kind == Mapping && len(v.Keys()) == 0)
	if indent == "" || empty {
		return v.AppendJS(dst)
	}
	inner := prefix + indent
	switch v.kind {
	case Sequence:
		dst = append(dst, "[\n"...)
		for index, item := range v.items {
			if index > 0 {
				dst = append(dst, ",\n"...)
			}
			dst = append(dst, inner...)
			dst = item.appendJSLevel(dst, indent, inner)
		}
		dst = append(dst, '\n')
		return append(append(dst, prefix...), ']')
	case Mapping:
		dst = append(dst, "{\n"...)
		for index, key := range v.Keys() {
			if index > 0 {
				dst = append(dst, ",\n"...)
			}
			dst = append(dst, inner...)
			dst = appendJSString(dst, key)
			dst = append(dst, ": "...)
			dst = v.mapping.values[key].appendJSLevel(dst, indent, inner)
		}
		dst = append(dst, '\n')
		return append(append(dst, prefix...), '}')
	case Undefined, Null, Bool, Number, String:
		return v.AppendJS(dst)
	default:
		panic("document: unhandled value kind")
	}
}

// appendJSString emits one string or key. Valid surrogate pairs become their
// code point; unpaired surrogates are escaped, which is what well-formed
// JSON.stringify does for lone units.
func appendJSString(dst []byte, s JSString) []byte {
	dst = append(dst, '"')
	for index := 0; index < s.Len(); index++ {
		unit := s.At(index)
		switch unit {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = appendJSUnit(dst, s, &index, unit)
		}
	}
	return append(dst, '"')
}

func appendJSUnit(dst []byte, s JSString, index *int, unit uint16) []byte {
	switch {
	case unit < 0x20:
		return appendEscapedUnit(dst, unit)
	case unit >= 0xd800 && unit <= 0xdbff:
		if *index+1 < s.Len() {
			low := s.At(*index + 1)
			if low >= 0xdc00 && low <= 0xdfff {
				*index++
				return utf8.AppendRune(dst, utf16.DecodeRune(rune(unit), rune(low)))
			}
		}
		return appendEscapedUnit(dst, unit)
	case unit >= 0xdc00 && unit <= 0xdfff:
		return appendEscapedUnit(dst, unit)
	default:
		return utf8.AppendRune(dst, rune(unit))
	}
}

func appendEscapedUnit(dst []byte, unit uint16) []byte {
	return append(dst, '\\', 'u',
		hexDigits[unit>>12&0xf], hexDigits[unit>>8&0xf], hexDigits[unit>>4&0xf], hexDigits[unit&0xf])
}

// appendJSNumber reproduces ECMAScript Number::toString.
func appendJSNumber(dst []byte, number float64) []byte {
	switch {
	case math.IsNaN(number), math.IsInf(number, 0):
		return append(dst, "null"...)
	case number == 0:
		return append(dst, '0')
	}
	if number < 0 {
		dst = append(dst, '-')
		number = -number
	}
	// Go and ECMAScript both choose the shortest round-trip digits, so the
	// exponent form only needs re-laying out to ECMAScript's threshold rules.
	formatted := strconv.FormatFloat(number, 'e', -1, 64)
	mantissa, exponent := splitExponent(formatted)
	digits := make([]byte, 0, len(mantissa))
	for index := 0; index < len(mantissa); index++ {
		if mantissa[index] != '.' {
			digits = append(digits, mantissa[index])
		}
	}
	n := exponent + 1
	k := len(digits)
	switch {
	case k <= n && n <= 21:
		dst = append(dst, digits...)
		for index := 0; index < n-k; index++ {
			dst = append(dst, '0')
		}
	case 0 < n && n <= 21:
		dst = append(dst, digits[:n]...)
		dst = append(dst, '.')
		dst = append(dst, digits[n:]...)
	case -6 < n && n <= 0:
		dst = append(dst, '0', '.')
		for index := 0; index < -n; index++ {
			dst = append(dst, '0')
		}
		dst = append(dst, digits...)
	default:
		dst = append(dst, digits[0])
		if k > 1 {
			dst = append(dst, '.')
			dst = append(dst, digits[1:]...)
		}
		dst = append(dst, 'e')
		if n-1 >= 0 {
			dst = append(dst, '+')
		}
		dst = strconv.AppendInt(dst, int64(n-1), 10)
	}
	return dst
}

func splitExponent(formatted string) (string, int) {
	for index := 0; index < len(formatted); index++ {
		if formatted[index] != 'e' {
			continue
		}
		exponent, err := strconv.Atoi(formatted[index+1:])
		if err != nil {
			panic("document: exponent form")
		}
		return formatted[:index], exponent
	}
	panic("document: expected exponent form")
}
