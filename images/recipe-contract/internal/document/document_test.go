package document

import (
	"bytes"
	"math"
	"os"
	"strings"
	"testing"
)

func mustParse(t *testing.T, source []byte, format Format) Value {
	t.Helper()
	value, err := Parse(source, format)
	if err != nil {
		t.Fatalf("parse %q: %v", source, err)
	}
	return value
}

func TestCoreSchemaResolution(t *testing.T) {
	for _, test := range []struct {
		source string
		want   string
	}{
		{"x: ~\n", `{"x":null}`},
		{"x: null\n", `{"x":null}`},
		{"x: NULL\n", `{"x":null}`},
		{"x: nULL\n", `{"x":"nULL"}`},
		{"x: true\n", `{"x":true}`},
		{"x: FALSE\n", `{"x":false}`},
		{"x: Yes\n", `{"x":"Yes"}`},
		{"x: 0o17\n", `{"x":15}`},
		{"x: 017\n", `{"x":17}`},
		{"x: 00\n", `{"x":0}`},
		{"x: +1\n", `{"x":1}`},
		{"x: 0x1f\n", `{"x":31}`},
		{"x: -0x1\n", `{"x":"-0x1"}`},
		{"x: 0b101\n", `{"x":"0b101"}`},
		{"x: 1_000\n", `{"x":"1_000"}`},
		{"x: 1e3\n", `{"x":1000}`},
		{"x: .5\n", `{"x":0.5}`},
		{"x: 1.\n", `{"x":1}`},
		{"x: .inf\n", `{"x":null}`},
		{"x: -.Inf\n", `{"x":null}`},
		{"x: .nan\n", `{"x":null}`},
		{"x: 12345678901234567890\n", `{"x":12345678901234567000}`},
		{"x: 2001-12-14\n", `{"x":"2001-12-14"}`},
		{"x: \"1\"\n", `{"x":"1"}`},
		{"x: '1'\n", `{"x":"1"}`},
		{"x: |\n  1\n", `{"x":"1\n"}`},
	} {
		value := mustParse(t, []byte(test.source), YAML)
		if got := string(value.AppendJS(nil)); got != test.want {
			t.Errorf("%q = %s, want %s", test.source, got, test.want)
		}
	}
	value := mustParse(t, []byte("x: .inf\ny: -.inf\nz: .nan\n"), YAML)
	for _, key := range []string{"x", "y", "z"} {
		number, ok := value.Get(key).Number()
		if !ok || !math.IsInf(number, map[string]int{"x": 1, "y": -1}[key]) && key != "z" {
			t.Errorf("%s = %v", key, number)
		}
		if key == "z" && !math.IsNaN(number) {
			t.Errorf("z = %v", number)
		}
	}
}

func TestKeyOrderAndKeys(t *testing.T) {
	value := mustParse(t, []byte("b: 1\n'10': 2\na: 3\n'2': 4\n'01': 5\n"), YAML)
	keys, err := value.StringKeys()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2", "10", "b", "a", "01"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v", keys)
	}
	for index := range want {
		if keys[index] != want[index] {
			t.Fatalf("keys = %v, want %v", keys, want)
		}
	}
}

func TestDeepStrictEqualSemantics(t *testing.T) {
	zero := NewNumber(0)
	negativeZero := NewNumber(math.Copysign(0, -1))
	if DeepStrictEqual(zero, negativeZero) {
		t.Error("-0 must not equal 0 under Object.is")
	}
	if !DeepStrictEqual(NewNumber(math.NaN()), NewNumber(math.NaN())) {
		t.Error("NaN must equal NaN")
	}
	a := mustParse(t, []byte("x: 1\ny: 2\n"), YAML)
	b := mustParse(t, []byte("y: 2\nx: 1\n"), YAML)
	if !DeepStrictEqual(a, b) {
		t.Error("mapping order must not matter")
	}
	if DeepStrictEqual(UndefinedValue(), NullValue()) {
		t.Error("undefined must differ from null")
	}
	if StrictEqual(a, a) {
		t.Error("=== on containers must not report structural equality")
	}
	if !StrictEqual(NewString("a"), NewString("a")) {
		t.Error("string equality")
	}
}

func TestNumberFormatting(t *testing.T) {
	for _, test := range []struct {
		number float64
		want   string
	}{
		{1e21, "1e+21"},
		{1.5e-7, "1.5e-7"},
		{1.2345678901234568e20, "123456789012345680000"},
		{0.30000000000000004, "0.30000000000000004"},
		{math.Copysign(0, -1), "0"},
		{math.Inf(1), "null"},
		{math.NaN(), "null"},
		{1e-7, "1e-7"},
		{0.000001, "0.000001"},
		{-42, "-42"},
	} {
		if got := string(NewNumber(test.number).AppendJS(nil)); got != test.want {
			t.Errorf("%v = %s, want %s", test.number, got, test.want)
		}
	}
}

func TestStringEscaping(t *testing.T) {
	value := NewString("<>&\u007f\u2028\u001f")
	if got, want := string(value.AppendJS(nil)), "\"<>&\u007f\u2028\\u001f\""; got != want {
		t.Errorf("escaping = %q, want %q", got, want)
	}
	lone := FromUnits([]uint16{'a', 0xd800, 'b', 0xdc00})
	if got, want := string(appendJSString(nil, lone)), `"a\ud800b\udc00"`; got != want {
		t.Errorf("lone surrogates = %s, want %s", got, want)
	}
	pair := FromUnits([]uint16{0xd83d, 0xde00})
	if got, want := string(appendJSString(nil, pair)), `"😀"`; got != want {
		t.Errorf("surrogate pair = %s, want %s", got, want)
	}
	if _, ok := lone.UTF8(); ok {
		t.Error("unpaired surrogate must not claim a UTF-8 form")
	}
}

func TestJSStringEqualityAndOrder(t *testing.T) {
	if FromUnits([]uint16{0xd800}) == FromUnits([]uint16{0xfffd}) {
		t.Error("a lone surrogate must not collide with U+FFFD")
	}
	if got := MustStringFromUTF8("😀"); got.Len() != 2 {
		t.Errorf("astral length = %d, want 2 UTF-16 units", got.Len())
	}
	if !MustStringFromUTF8("😀").Less(MustStringFromUTF8("\uffff")) {
		t.Error("JSLess compares code units, so a high surrogate sorts before U+FFFF")
	}
}

func TestJSONRoundTrips(t *testing.T) {
	for _, source := range []string{
		`{"a":"\ud800"}`, `{"a":"\ud801"}`, `{"a":"\udc00"}`, `{"a":"\ud800\ud800"}`,
		`{"a":"\udc00\udc00"}`, `{"a":"\ud800x"}`, `{"a":"😀"}`, `{"a":"\ud83d\ude00"}`,
		`{"\ud800":1,"\ud801":2}`, `{"\ufffd":1}`, `{"a":"\/"}`, `{"a":"<>&"}`,
	} {
		value, err := Parse([]byte(source), JSON)
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		printed := value.AppendJS(nil)
		again, err := Parse(printed, JSON)
		if err != nil {
			t.Fatalf("%s reparse: %v", source, err)
		}
		if !DeepStrictEqual(value, again) {
			t.Errorf("%s did not survive a JSON round trip: %s", source, printed)
		}
		indented := value.AppendJSIndent(nil, "  ")
		third, err := Parse(indented, JSON)
		if err != nil {
			t.Fatalf("%s indented reparse: %v", source, err)
		}
		if !DeepStrictEqual(value, third) {
			t.Errorf("%s did not survive an indented round trip", source)
		}
	}
	literal := mustParse(t, []byte(`{"a":"😀"}`), JSON)
	escaped := mustParse(t, []byte(`{"a":"\ud83d\ude00"}`), JSON)
	if !DeepStrictEqual(literal, escaped) {
		t.Error("a literal astral character must equal its surrogate-pair escape")
	}
	replacement := mustParse(t, []byte(`{"a":"\ufffd"}`), JSON)
	lonesurrogate := mustParse(t, []byte(`{"a":"\ud800"}`), JSON)
	if DeepStrictEqual(replacement, lonesurrogate) {
		t.Error("U+FFFD must differ from a lone surrogate")
	}
}

func TestJSONPolicies(t *testing.T) {
	for _, test := range []struct {
		source string
		match  string
	}{
		{`{"a":1,"a":2}`, "unique"},
		{`{"a":1}`, ""},
		{`[]`, ""},
		{`01`, "invalid JSON"},
		{`{"a":1,}`, "invalid JSON"},
		{`{"a":}`, "invalid JSON"},
		{`{"a":1}x`, "trailing"},
		{"{\"a\":\"\x01\"}", "control"},
		{`{"a":"\q"}`, "escape"},
	} {
		value, err := Parse([]byte(test.source), JSON)
		if test.match == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", test.source, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: expected %q, got %s", test.source, test.match, value.Describe())
			continue
		}
		if !contains(err.Error(), test.match) {
			t.Errorf("%s: error %q does not mention %q", test.source, err, test.match)
		}
	}
	deep := `{"a":` + repeat("[", 101) + "0" + repeat("]", 101) + "}"
	if _, err := Parse([]byte(deep), JSON); err == nil || !contains(err.Error(), "depth100") {
		t.Errorf("deep JSON = %v", err)
	}
}

func TestDepthBoundIsShared(t *testing.T) {
	source := "allowBuilds: " + repeat("[", 101) + "0" + repeat("]", 101) + "\n"
	if _, err := Parse([]byte(source), YAML); err == nil || !contains(err.Error(), "depth100") {
		t.Errorf("deep YAML = %v", err)
	}
}

func TestInvalidUTF8IsFatal(t *testing.T) {
	if _, err := Parse([]byte{0xff, 0xfe}, YAML); err == nil || !contains(err.Error(), "valid for encoding utf-8") {
		t.Errorf("invalid UTF-8 = %v", err)
	}
	if _, err := Parse([]byte{}, YAML); err != nil {
		t.Errorf("empty document = %v", err)
	}
	if value := mustParse(t, []byte{}, YAML); value.Kind() != Null {
		t.Errorf("empty document = %s", value.Describe())
	}
	if value := mustParse(t, []byte("\xef\xbb\xbfx: 1\n"), YAML); value.Get("x").Kind() != Number {
		t.Errorf("BOM was not stripped: %s", value.Describe())
	}
}

func TestScannerStrictIndentation(t *testing.T) {
	for _, source := range []string{
		"x: \"a\nb\"\n", "x: 'a\nb'\n",
	} {
		if _, err := Parse([]byte(source), YAML); err == nil || !contains(err.Error(), "indented scalar continuation") {
			t.Errorf("%q = %v", source, err)
		}
	}
	for _, source := range []string{"x: \"a\n b\"\n", "x: \"a\n\n b\"\n", "x: [a,\n b]\n"} {
		if _, err := Parse([]byte(source), YAML); err != nil {
			t.Errorf("%q = %v", source, err)
		}
	}
}

func TestMultipleDocuments(t *testing.T) {
	for _, source := range []string{"x: 1\n---\ny: 2\n", "---\nx: 1\n---\n"} {
		if _, err := Parse([]byte(source), YAML); err == nil || !contains(err.Error(), "multiple documents") {
			t.Errorf("%q = %v", source, err)
		}
	}
}

func TestWorkspaceEdit(t *testing.T) {
	committed, err := osReadFile("../../../tools/dsh/pnpm-workspace.yaml")
	if err != nil {
		t.Fatalf("read committed workspace: %v", err)
	}
	identity, err := DeleteTopLevel(committed, "overrides")
	if err != nil {
		t.Fatalf("delete overrides: %v", err)
	}
	if _, err := Parse(identity, YAML); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	value := mustParse(t, committed, YAML)
	overrides := value.Get("overrides")
	replaced, err := ReplaceTopLevel(committed, "overrides", overrides)
	if err != nil {
		t.Fatalf("replace overrides: %v", err)
	}
	if !bytesEqual(replaced, committed) {
		t.Errorf("replacing the committed map changed the bytes:\n%s", replaced)
	}
	changed, err := ReplaceTopLevel(committed, "overrides", NewMapping())
	if err != nil {
		t.Fatalf("replace with an empty map: %v", err)
	}
	reparsed, err := Parse(changed, YAML)
	if err != nil {
		t.Fatalf("reparse changed: %v", err)
	}
	if !DeepStrictEqual(reparsed, value.With("overrides", NewMapping())) {
		t.Errorf("value changed unexpectedly:\n%s", changed)
	}
}

func contains(text, substring string) bool { return strings.Contains(text, substring) }

func repeat(text string, count int) string { return strings.Repeat(text, count) }

func bytesEqual(a, b []byte) bool { return bytes.Equal(a, b) }

func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
