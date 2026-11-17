package strictjson

import (
	"strings"
	"testing"
)

// TestIntegersFollowJSONGrammar: one spelling for a whole number, so a document the
// review never produced is not a document this reader accepts.
func TestIntegersFollowJSONGrammar(t *testing.T) {
	cases := []struct {
		label  string
		raw    string
		want   int64
		needle string
	}{
		{label: "zero", raw: `0`, want: 0},
		{label: "negative", raw: `-7`, want: -7},
		{label: "plain", raw: `340226576`, want: 340226576},
		{label: "leading zero", raw: `01`, needle: "is not a JSON number"},
		{label: "leading zero after minus", raw: `-01`, needle: "is not a JSON number"},
		{label: "fraction", raw: `1.5`, needle: "fractional part"},
		{label: "exponent", raw: `1e3`, needle: "fractional part"},
		{label: "NaN", raw: `NaN`, needle: "is not a JSON number"},
		{label: "Infinity", raw: `Infinity`, needle: "is not a JSON number"},
	}
	for _, test := range cases {
		value, err := Decode([]byte(test.raw), "document")
		if test.needle != "" {
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %s", test.label, err)
			continue
		}
		if number, ok := value.(int64); !ok || number != test.want {
			t.Errorf("%s: got %v, want %d", test.label, value, test.want)
		}
	}
}

// TestDuplicateKeysAreRefused: encoding/json keeps the last of two identical keys, so
// a document could read one way to a reviewer and another to a build.
func TestDuplicateKeysAreRefused(t *testing.T) {
	if _, err := Decode([]byte(`{"a": 1, "a": 2}`), "document"); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("a duplicate key reported %v", err)
	}
}
