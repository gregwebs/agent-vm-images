package document

import (
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

// JSString is a JavaScript string: a sequence of UTF-16 code units. It is
// stored as the big-endian bytes of those units so the value is immutable,
// comparable and usable as a Go map key while unpaired surrogates stay
// distinguishable. Storing a Go string of decoded runes would fold every
// unpaired surrogate and defeat the ADR 0003 raw-record comparison.
type JSString struct{ units string }

// StringFromUTF8 converts valid UTF-8 to its UTF-16 code units. Invalid UTF-8
// is rejected rather than repaired: both the retired TextDecoder(fatal) and
// this reader fail closed.
func StringFromUTF8(s string) (JSString, error) {
	if !utf8.ValidString(s) {
		return JSString{}, errors.New("invalid UTF-8")
	}
	units := make([]byte, 0, len(s)*2)
	for _, r := range s {
		if r > 0xffff {
			high, low := utf16.EncodeRune(r)
			units = appendUnit(units, uint16(high))
			units = appendUnit(units, uint16(low))
			continue
		}
		units = appendUnit(units, uint16(r))
	}
	return JSString{units: string(units)}, nil
}

// MustStringFromUTF8 is for compile-time literals and tests.
func MustStringFromUTF8(s string) JSString {
	value, err := StringFromUTF8(s)
	if err != nil {
		panic(err)
	}
	return value
}

func appendUnit(dst []byte, unit uint16) []byte {
	return append(dst, byte(unit>>8), byte(unit))
}

// FromUnits builds a string from raw code units, keeping unpaired surrogates.
func FromUnits(units []uint16) JSString {
	dst := make([]byte, 0, len(units)*2)
	for _, unit := range units {
		dst = appendUnit(dst, unit)
	}
	return JSString{units: string(dst)}
}

// Units returns a copy of the code units; the backing storage stays immutable.
func (s JSString) Units() []uint16 {
	units := make([]uint16, len(s.units)/2)
	for index := range units {
		units[index] = uint16(s.units[2*index])<<8 | uint16(s.units[2*index+1])
	}
	return units
}

// At returns the code unit at index; Len is the unit count.
func (s JSString) At(index int) uint16 {
	return uint16(s.units[2*index])<<8 | uint16(s.units[2*index+1])
}

// Len is the number of UTF-16 code units, which is what JavaScript reports.
func (s JSString) Len() int { return len(s.units) / 2 }

// UTF8 reports the UTF-8 encoding, and false when the string holds an unpaired
// surrogate that UTF-8 cannot represent. It never substitutes U+FFFD.
func (s JSString) UTF8() (string, bool) {
	if len(s.units)%2 != 0 {
		return "", false
	}
	dst := make([]byte, 0, len(s.units)/2*3)
	for index := 0; index < s.Len(); index++ {
		unit := s.At(index)
		switch {
		case unit >= 0xd800 && unit <= 0xdbff:
			if index+1 < s.Len() {
				low := s.At(index + 1)
				if low >= 0xdc00 && low <= 0xdfff {
					dst = utf8.AppendRune(dst, utf16.DecodeRune(rune(unit), rune(low)))
					index++
					continue
				}
			}
			return "", false
		case unit >= 0xdc00 && unit <= 0xdfff:
			return "", false
		default:
			dst = utf8.AppendRune(dst, rune(unit))
		}
	}
	return string(dst), true
}

// ASCII reports the UTF-8 text when every code unit is ASCII, for the policy
// code that validates package names, versions and setting keys.
func (s JSString) ASCII() (string, bool) {
	if len(s.units)%2 != 0 {
		return "", false
	}
	dst := make([]byte, s.Len())
	for index := 0; index < s.Len(); index++ {
		unit := s.At(index)
		if unit > 0x7f {
			return "", false
		}
		dst[index] = byte(unit)
	}
	return string(dst), true
}

// Equal is JavaScript string equality.
func (s JSString) Equal(other JSString) bool { return s.units == other.units }

// Less is JavaScript `<` on strings: a lexicographic comparison of code units.
func (s JSString) Less(other JSString) bool {
	limit := s.Len()
	if other.Len() < limit {
		limit = other.Len()
	}
	for index := 0; index < limit; index++ {
		left, right := s.At(index), other.At(index)
		if left != right {
			return left < right
		}
	}
	return s.Len() < other.Len()
}

// JSLess is the comparator the retired `sorted()` helper used.
func JSLess(a, b JSString) bool { return a.Less(b) }

// AppendUTF8String is the convenience path for text this package produced
// itself: it validates UTF-8 before recording the units.
func AppendUTF8String(dst []byte, s string) []byte {
	value, err := StringFromUTF8(s)
	if err != nil {
		panic(err)
	}
	return appendJSString(dst, value)
}

// Display renders the string for a diagnostic: UTF-8 when it is representable,
// otherwise JSON.stringify's escaped form so an unpaired surrogate is visible
// instead of being replaced.
func (s JSString) Display() string {
	if text, ok := s.UTF8(); ok {
		return text
	}
	quoted := appendJSString(nil, s)
	return string(quoted[1 : len(quoted)-1])
}
