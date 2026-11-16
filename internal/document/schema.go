package document

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// The YAML 1.2 core-schema tests, in yaml@2.9.0's order. yaml.v3 resolves with
// the YAML 1.1 schema (`017` octal, `0b101` binary, timestamps), which would
// change every value the policies compare, so plain scalars are resolved here
// from the node's style and source text instead.
var coreSchema = []struct {
	test    *regexp.Regexp
	resolve func(string) Value
}{
	{regexp.MustCompile(`^(?:~|[Nn]ull|NULL)?$`), func(string) Value { return NullValue() }},
	{regexp.MustCompile(`^(?:[Tt]rue|TRUE|[Ff]alse|FALSE)$`), func(text string) Value {
		return NewBool(text[0] == 't' || text[0] == 'T')
	}},
	{regexp.MustCompile(`^0o[0-7]+$`), func(text string) Value { return NewNumber(parseRadix(text[2:], 8)) }},
	{regexp.MustCompile(`^[-+]?[0-9]+$`), func(text string) Value { return NewNumber(parseRadix(text, 10)) }},
	{regexp.MustCompile(`^0x[0-9a-fA-F]+$`), func(text string) Value { return NewNumber(parseRadix(text[2:], 16)) }},
	{regexp.MustCompile(`^(?:[-+]?\.(?:inf|Inf|INF)|\.nan|\.NaN|\.NAN)$`), resolveFloatNaN},
	{regexp.MustCompile(`^[-+]?(?:\.[0-9]+|[0-9]+(?:\.[0-9]*)?)[eE][-+]?[0-9]+$`), func(text string) Value {
		return NewNumber(parseNumberText(text))
	}},
	{regexp.MustCompile(`^[-+]?(?:\.[0-9]+|[0-9]+\.[0-9]*)$`), func(text string) Value {
		return NewNumber(parseNumberText(text))
	}},
}

// resolveScalar applies the core schema to a plain scalar's source text; any
// other style is a string. A plain scalar that matches no test stays a string.
func resolveScalar(style yamlStyle, source string) Value {
	if style != stylePlain {
		return NewString(source)
	}
	for _, entry := range coreSchema {
		if entry.test.MatchString(source) {
			return entry.resolve(source)
		}
	}
	return NewString(source)
}

func resolveFloatNaN(text string) Value {
	if strings.HasSuffix(strings.ToLower(text), "nan") {
		return NewNumber(math.NaN())
	}
	if text[0] == '-' {
		return NewNumber(math.Inf(-1))
	}
	return NewNumber(math.Inf(1))
}

// parseRadix reproduces parseInt(str, radix) as a double, including the
// precision loss for integers beyond 2^53.
func parseRadix(text string, radix int) float64 {
	if radix == 10 {
		if value, err := strconv.ParseFloat(text, 64); err == nil || isRangeError(err) {
			return value
		}
	}
	if integer, ok := new(big.Int).SetString(text, radix); ok {
		value, _ := new(big.Float).SetInt(integer).Float64()
		return value
	}
	panic("document: core schema integer")
}

func parseNumberText(text string) float64 {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil && !isRangeError(err) {
		panic("document: core schema number")
	}
	return value
}

func isRangeError(err error) bool {
	numberError, ok := err.(*strconv.NumError)
	return ok && numberError.Err == strconv.ErrRange
}

// jsNumberString is JavaScript `String(number)`, used for property keys.
func jsNumberString(number float64) JSString {
	switch {
	case math.IsNaN(number):
		return MustStringFromUTF8("NaN")
	case math.IsInf(number, 1):
		return MustStringFromUTF8("Infinity")
	case math.IsInf(number, -1):
		return MustStringFromUTF8("-Infinity")
	default:
		return MustStringFromUTF8(string(appendJSNumber(nil, number)))
	}
}
