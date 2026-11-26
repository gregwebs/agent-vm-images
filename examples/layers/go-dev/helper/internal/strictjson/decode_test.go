package strictjson

import (
	"strings"
	"testing"
)

// TestDecodeRefusals is invariant 6 at the decoder level: every document the
// source reader refuses is refused here with the same fragment, and no document
// it accepts is refused.
func TestDecodeRefusals(t *testing.T) {
	cases := []struct {
		label string
		raw   string
		frag  string
	}{
		{"object", `{"a": 1}`, ""},
		{"empty object", `{}`, ""},
		{"array", `[1, 2]`, ""},
		{"null", `null`, ""},
		{"leading BOM", "\xef\xbb\xbf{}", "leading UTF-8 BOM is not accepted"},
		{"invalid UTF-8", "{\"a\": \"\xff\"}", "not valid UTF-8"},
		{"empty", "", "not valid JSON"},
		{"trailing document", `{} {}`, "extra data after the document"},
		{"duplicate key", `{"a": 1, "a": 2}`, "duplicate key 'a'"},
		{"nested duplicate", `{"a": {"b": 1, "b": 2}}`, "duplicate key 'b'"},
		{"duplicate through an escape", `{"s": 1, "\u0073": 2}`, "duplicate key 's'"},
		{"NaN", `NaN`, "NaN is not valid JSON"},
		{"Infinity", `Infinity`, "Infinity is not valid JSON"},
		{"-Infinity", `-Infinity`, "-Infinity is not valid JSON"},
		{"raw control character", "\"a\nb\"", "invalid control character"},
		{"leading zero", `01`, "leading zero"},
		{"single quote", `{'a': 1}`, "not valid JSON"},
		{"unterminated string", `"abc`, "unterminated string"},
		{"deep nesting", strings.Repeat("[", 600) + strings.Repeat("]", 600),
			"not valid JSON"},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			_, err := Decode([]byte(test.raw), "pin.json")
			if test.frag == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.frag) {
				t.Fatalf("got %v, want a refusal naming %q", err, test.frag)
			}
		})
	}
}

// TestNumbersKeepTheirLiteral is invariant 7's decoder half: `type(x) is int` is
// decided from the literal, so 1.0, 1e0 and 1 are three different answers.
func TestNumbersKeepTheirLiteral(t *testing.T) {
	cases := []struct {
		raw     string
		integer bool
	}{
		{"1", true},
		{"-0", true},
		{"18446744073709551616", true},
		{"1.0", false},
		{"1e0", false},
		{"-1.5e-3", false},
	}
	for _, test := range cases {
		value, err := Decode([]byte(test.raw), "pin.json")
		if err != nil {
			t.Fatalf("%s: %v", test.raw, err)
		}
		number, ok := value.(Number)
		if !ok {
			t.Fatalf("%s decoded to %T", test.raw, value)
		}
		if number.Integer != test.integer {
			t.Errorf("%s: integer is %v, want %v", test.raw, number.Integer, test.integer)
		}
		if number.Literal != test.raw {
			t.Errorf("%s: literal is %q", test.raw, number.Literal)
		}
	}
}

// TestSurrogatesStayDistinct is row 10's decoder half: two lone surrogates are
// two different strings, so neither can be forged into the other or into a
// duplicate key.
func TestSurrogatesStayDistinct(t *testing.T) {
	first, err := Decode([]byte(`"\ud800"`), "pin.json")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Decode([]byte(`"\ud801"`), "pin.json")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two different lone surrogates decoded to one string")
	}
	if first.(string) == "\uFFFD" {
		t.Fatal("a lone surrogate collapsed onto a replacement character")
	}
	// A valid pair is one code point.
	pair, err := Decode([]byte(`"\ud83d\ude00"`), "pin.json")
	if err != nil {
		t.Fatal(err)
	}
	if pair != "\U0001F600" {
		t.Fatalf("a surrogate pair decoded to %q", pair)
	}
}

func TestRepr(t *testing.T) {
	if got := Repr("a'b"); got != `'a\'b'` {
		t.Fatalf("repr is %s", got)
	}
	if got := Repr("schema"); got != "'schema'" {
		t.Fatalf("repr is %s", got)
	}
}
