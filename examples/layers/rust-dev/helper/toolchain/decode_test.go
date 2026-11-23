package toolchain

import (
	"strings"
	"testing"
)

// TestTOMLGrammar is the TOML 1.0 invariant (row 23).
//
// The verdicts in `want` are the TOML 1.0 column measured from `tomllib` on
// Python 3.12–3.14 — the grammars the image (trixie, 3.13) and CI (ubuntu-24.04,
// 3.12) ship, and the one rustup itself reads. They are asserted unconditionally,
// so a host whose own `tomllib` is TOML 1.1 cannot weaken this test; the pins
// suite is what compares the two implementations, and it says how many rows it
// compared and against which measured grammar.
//
// One row is a declared divergence: no go-toml/v2 release refuses `"\e"` (see
// docs/adr/0017-go-programs.md for the measurement), so this test pins the
// acceptance *and the value it produces* instead of describing it.
func TestTOMLGrammar(t *testing.T) {
	cases := []struct {
		label string
		doc   string
		// want is the TOML 1.0 verdict: "A" accepts, "R" refuses.
		want string
		// value, when set, is the decoded value of key "a" on acceptance.
		value any
	}{
		{"inline table extended by a dotted key", "a = {b = 1}\na.c = 2\n", "R", nil},
		{"inline table extended by a header", "a = {b = 1}\n[a.c]\nd = 1\n", "R", nil},
		{"table redefined after dotted keys", "[a]\nb.c = 1\n[a.b]\nd = 2\n", "R", nil},
		{"trailing comma in an inline table", "a = {b = 1,}\n", "R", nil},
		{"newline in an inline table", "a = {b = 1,\nc = 2}\n", "R", nil},
		{"escape e", "a = \"\\e\"\n", "R", "\x1b"},
		{"escape x", "a = \"\\x41\"\n", "R", nil},
		{"local time", "a = 07:32\n", "R", nil},
		{"crlf", "a = 1\r\n", "A", int64(1)},
		{"duplicate key", "a = 1\na = 2\n", "R", nil},
		{"redefined table", "[a]\nb = 1\n[a]\nc = 2\n", "R", nil},
		{"date is not a string", "a = 2026-01-01\n", "A", nil},
		{"boolean", "a = true\n", "A", true},
		{"integer", "a = 7\n", "A", int64(7)},
		{"float", "a = 7.5\n", "A", 7.5},
		{"comment only", "# nothing\n", "A", nil},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			document, err := decodeTOML([]byte(test.doc), "settings.toml")
			if test.want == "R" {
				if test.value == nil {
					if err == nil {
						t.Fatalf("accepted %q; TOML 1.0 refuses it", test.doc)
					}
					if !strings.Contains(err.Error(), "not valid TOML") {
						t.Fatalf("refusal does not name the decoder: %v", err)
					}
					return
				}
				// The declared divergence: the document is accepted, and this is
				// the value it produces.
				if err != nil {
					t.Fatalf("the declared divergence changed: %v", err)
				}
				if document["a"] != test.value {
					t.Fatalf("a is %#v, want %#v", document["a"], test.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused %q; TOML 1.0 accepts it: %v", test.doc, err)
			}
			if test.value != nil && document["a"] != test.value {
				t.Fatalf("a is %#v, want %#v", document["a"], test.value)
			}
			if test.label == "date is not a string" {
				if _, isString := document["a"].(string); isString {
					t.Fatal("a TOML date decoded as a string")
				}
			}
		})
	}
}

// TestTOMLRefusesBeforeDecoding is the first half of row 23: a BOM and invalid
// UTF-8 are refused before the grammar, as the source reader refuses them.
func TestTOMLRefusesBeforeDecoding(t *testing.T) {
	if _, err := decodeTOML([]byte("\xef\xbb\xbfa = 1\n"), "settings.toml"); err == nil ||
		!strings.Contains(err.Error(), "leading UTF-8 BOM is not accepted") {
		t.Fatalf("BOM: %v", err)
	}
	if _, err := decodeTOML([]byte("a = \"\xff\"\n"), "settings.toml"); err == nil ||
		!strings.Contains(err.Error(), "not valid UTF-8") {
		t.Fatalf("invalid UTF-8: %v", err)
	}
}

// TestTOMLIntegerRange pins the decoder's measured int64 limit. Python's int has
// no range, so this is the one TOML row where the vendored decoder refuses a
// document the oracle accepts: a library differential the plan's stop rule
// reserves for the owner's ruling (reported on #230, not reconciled in code).
func TestTOMLIntegerRange(t *testing.T) {
	if _, err := decodeTOML([]byte("x = 9223372036854775807\n"), "settings.toml"); err != nil {
		t.Fatalf("the largest int64 was refused: %v", err)
	}
	if _, err := decodeTOML([]byte("x = 9223372036854775808\n"), "settings.toml"); err == nil {
		t.Fatal("an integer beyond int64 was accepted; the declared stop changed")
	}
}
