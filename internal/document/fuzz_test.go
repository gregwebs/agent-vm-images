package document

import "testing"

// FuzzParse keeps the scanner and the bounded JSON reader total: no input may
// panic, and a rejected input must produce a message instead.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"x: 1\n", "x: ! 1\n", "x: \"a\n b\"\n", "- a\n- b\n", "{\"a\":1}",
		"{\"a\":\"\\ud800\"}", "%YAML 1.2\n---\nx: 1\n", "x: |\n  !\n",
		"x: [a, {b: c}]\n", "---\n", "", "\xef\xbb\xbfx: 1\n", "x: a\u2028b\n",
	} {
		f.Add([]byte(seed), false)
		f.Add([]byte(seed), true)
	}
	f.Fuzz(func(t *testing.T, source []byte, jsonInput bool) {
		format := YAML
		if jsonInput {
			format = JSON
		}
		value, err := Parse(source, format)
		if err != nil {
			if err.Error() == "" {
				t.Fatal("empty diagnostic")
			}
			return
		}
		if _, ok := value.Text(); ok {
			return
		}
		_ = value.Describe()
	})
}
