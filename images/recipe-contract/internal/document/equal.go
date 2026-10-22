package document

import "math"

// StrictEqual is JavaScript `===`. The retired `assert.equal` uses `==`, but
// every operand is a scalar whose type the caller already fixed, so `===` is the
// same predicate here and the container cases cannot arise.
func StrictEqual(a, b Value) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case Undefined, Null:
		return true
	case Bool:
		return a.boolean == b.boolean
	case Number:
		return a.number == b.number
	case String:
		return a.text.Equal(b.text)
	case Sequence, Mapping:
		return false
	default:
		return false
	}
}

// DeepStrictEqual is assert.deepStrictEqual: mapping key order is irrelevant,
// numbers use Object.is (-0 differs from 0, NaN equals NaN), and Undefined only
// equals Undefined.
func DeepStrictEqual(a, b Value) bool {
	if a.kind != b.kind {
		return false
	}
	switch a.kind {
	case Undefined, Null:
		return true
	case Bool:
		return a.boolean == b.boolean
	case Number:
		return sameNumber(a.number, b.number)
	case String:
		return a.text.Equal(b.text)
	case Sequence:
		if len(a.items) != len(b.items) {
			return false
		}
		for index := range a.items {
			if !DeepStrictEqual(a.items[index], b.items[index]) {
				return false
			}
		}
		return true
	case Mapping:
		if len(a.mapping.order) != len(b.mapping.order) {
			return false
		}
		for key, value := range a.mapping.values {
			other, ok := b.mapping.values[key]
			if !ok || !DeepStrictEqual(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func sameNumber(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	if a == 0 && b == 0 {
		return math.Signbit(a) == math.Signbit(b)
	}
	return a == b
}
