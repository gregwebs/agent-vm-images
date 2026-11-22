package toolchain

import (
	"strings"
	"testing"
)

// committedPinDir is the reviewed pin directory, as an absolute path: a test is
// run from this package's directory, so the path is stable however the caller
// started it.
func committedPinDir(t *testing.T) string {
	t.Helper()
	return "../../data/rust-toolchain"
}

func TestParseTypedValues(t *testing.T) {
	good := []struct {
		label string
		parse func(any, string) error
		value any
	}{
		{"sha256", func(v any, w string) error { _, err := ParseSha256(v, w); return err },
			strings.Repeat("a", 64)},
		{"fingerprint", func(v any, w string) error { _, err := ParseFingerprint(v, w); return err },
			"108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE"},
		{"channel", func(v any, w string) error { _, err := ParseChannel(v, w); return err }, "1.98.1"},
		{"date", func(v any, w string) error { _, err := ParseDate(v, w); return err }, "2026-09-03"},
		{"triple", func(v any, w string) error { _, err := ParseTriple(v, w); return err },
			"x86_64-unknown-linux-gnu"},
		{"triple with a version dot", func(v any, w string) error { _, err := ParseTriple(v, w); return err },
			"thumbv8m.main-none-eabi"},
		{"evidence path", func(v any, w string) error { _, err := ParseEvidencePath(v, w); return err },
			"evidence/rustup-amd64.sha256"},
		{"member prefix", func(v any, w string) error { _, err := ParseMemberPrefix(v, w); return err },
			"cargo-1.98.1-x86_64-unknown-linux-gnu/"},
	}
	for _, test := range good {
		if err := test.parse(test.value, "field"); err != nil {
			t.Errorf("%s: refused %v: %v", test.label, test.value, err)
		}
	}
	bad := []struct {
		label string
		parse func(any, string) error
		value any
		frag  string
	}{
		{"sha256 upper", func(v any, w string) error { _, err := ParseSha256(v, w); return err },
			strings.Repeat("A", 64), "expected a lowercase SHA-256 hex digest"},
		{"sha256 short", func(v any, w string) error { _, err := ParseSha256(v, w); return err },
			"abc", "expected a lowercase SHA-256 hex digest"},
		{"sha256 not a string", func(v any, w string) error { _, err := ParseSha256(v, w); return err },
			7, "expected a lowercase SHA-256 hex digest"},
		{"fingerprint lower", func(v any, w string) error { _, err := ParseFingerprint(v, w); return err },
			"108f66205eaeb0aaa8dd5e1c85ab96e6fa1be5fe", "expected an uppercase 40-hex OpenPGP fingerprint"},
		{"channel leading zero", func(v any, w string) error { _, err := ParseChannel(v, w); return err },
			"01.98.1", "expected an exact X.Y.Z release channel"},
		{"channel two parts", func(v any, w string) error { _, err := ParseChannel(v, w); return err },
			"1.98", "expected an exact X.Y.Z release channel"},
		{"date shape", func(v any, w string) error { _, err := ParseDate(v, w); return err },
			"2026-9-03", "expected YYYY-MM-DD"},
		{"date february 30", func(v any, w string) error { _, err := ParseDate(v, w); return err },
			"2026-02-30", "not a calendar date: 2026-02-30"},
		{"date year zero", func(v any, w string) error { _, err := ParseDate(v, w); return err },
			"0000-01-01", "not a calendar date: 0000-01-01"},
		{"date a real leap day", func(v any, w string) error { _, err := ParseDate(v, w); return err },
			"2024-02-29", ""},
		{"triple too long", func(v any, w string) error { _, err := ParseTriple(v, w); return err },
			strings.Repeat("a-", 40) + "b", "expected a target triple"},
		{"triple no dash", func(v any, w string) error { _, err := ParseTriple(v, w); return err },
			"x86_64", "expected a target triple"},
		{"evidence path parent", func(v any, w string) error { _, err := ParseEvidencePath(v, w); return err },
			"evidence/../x", "not a canonical relative path: evidence/../x"},
		{"evidence path absolute", func(v any, w string) error { _, err := ParseEvidencePath(v, w); return err },
			"/etc/passwd", "not a canonical relative path: /etc/passwd"},
		{"evidence path empty", func(v any, w string) error { _, err := ParseEvidencePath(v, w); return err },
			"", "expected a relative evidence path"},
		{"member prefix two segments", func(v any, w string) error { _, err := ParseMemberPrefix(v, w); return err },
			"a/b/", "expected a single archive member prefix like name/"},
	}
	for _, test := range bad {
		err := test.parse(test.value, "field")
		if test.frag == "" {
			if err != nil {
				t.Errorf("%s: refused %v: %v", test.label, test.value, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.frag) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.frag)
		}
	}
}

// TestParseURL is the url grammar invariant. Every vector carries the verdict the
// source reader's `urlsplit` produces, including the case folding it applies
// before the canonical-origin comparison.
func TestParseURL(t *testing.T) {
	cases := []struct {
		label string
		url   string
		frag  string
	}{
		{"canonical", "https://static.rust-lang.org/dist/x.tar.xz", ""},
		// The case-folded parse passes the canonical-origin rule, then differs from
		// the committed origin: the same first refusal the source reader reports.
		{"upper-case scheme and host", "HTTPS://STATIC.RUST-LANG.ORG/dist/x.tar.xz",
			"!= committed origin"},
		{"explicit port", "https://static.rust-lang.org:443/dist/x.tar.xz",
			"not a canonical rust-lang.org HTTPS URL"},
		{"userinfo", "https://user@static.rust-lang.org/dist/x.tar.xz",
			"not a canonical rust-lang.org HTTPS URL"},
		{"query", "https://static.rust-lang.org/dist/x.tar.xz?a=1",
			"not a canonical rust-lang.org HTTPS URL"},
		{"fragment", "https://static.rust-lang.org/dist/x.tar.xz#frag",
			"not a canonical rust-lang.org HTTPS URL"},
		{"other host", "https://example.com/dist/x.tar.xz",
			"not a canonical rust-lang.org HTTPS URL"},
		{"lookalike host", "https://static.rust-lang.org.evil.test/dist/x.tar.xz",
			"not a canonical rust-lang.org HTTPS URL"},
		{"http", "http://static.rust-lang.org/dist/x.tar.xz",
			"not a canonical rust-lang.org HTTPS URL"},
		{"no scheme", "static.rust-lang.org/dist/x.tar.xz",
			"not a canonical rust-lang.org HTTPS URL"},
		{"empty", "", "not a canonical rust-lang.org HTTPS URL"},
		{"backslash", "https://static.rust-lang.org/dist\\x.tar.xz",
			"URL path is not canonical"},
		{"double slash in path", "https://static.rust-lang.org//dist/x.tar.xz",
			"URL path is not canonical"},
		{"parent segment", "https://static.rust-lang.org/dist/../x.tar.xz",
			"URL path is not canonical"},
		{"trailing parent", "https://static.rust-lang.org/dist/x.tar.xz/..",
			"URL path is not canonical"},
		{"tab is stripped then compared", "https://static.rust-lang.org/dist/x.tar.xz\t",
			"!= committed origin"},
		{"newline is stripped then compared", "https://static.rust-lang.org/dist/x.tar.xz\n",
			"!= committed origin"},
		{"long", "https://static.rust-lang.org/" + strings.Repeat("a", 600),
			"expected a URL string"},
		{"not a string", "", "expected a URL string"},
	}
	for _, test := range cases {
		expected := "https://static.rust-lang.org/dist/x.tar.xz"
		value := any(test.url)
		if test.label == "not a string" {
			value = 7
		}
		_, err := ParseURL(value, "field", &expected)
		switch {
		case test.frag == "" && err != nil:
			t.Errorf("%s: refused %q: %v", test.label, test.url, err)
		case test.frag != "" && (err == nil || !strings.Contains(err.Error(), test.frag)):
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.frag)
		}
	}
	// Without an expected origin a case-folded URL is accepted, which is what the
	// pin loader then compares against the canonical origin itself.
	if _, err := ParseURL("HTTPS://STATIC.RUST-LANG.ORG/dist/x", "field", nil); err != nil {
		t.Fatalf("a case-folded canonical URL was refused without an expectation: %v", err)
	}
}

// TestRejectControlCharacters covers C0, DEL–C1 and the surrogate form, which
// reaches Go as WTF-8 from a JSON `\ud800` escape.
func TestRejectControlCharacters(t *testing.T) {
	cases := []struct {
		label string
		value string
		bad   bool
	}{
		{"plain", "gregwebs", false},
		{"C0", "greg\x01webs", true},
		{"DEL", "greg\x7fwebs", true},
		{"C1", "greg\u0085webs", true},
		{"WTF-8 surrogate", "greg\xed\xa0\x80webs", true},
		{"astral", "greg\U0001F600webs", false},
		{"empty", "", false},
	}
	for _, test := range cases {
		err := RejectControlCharacters(test.value, "field")
		if test.bad && (err == nil || !strings.Contains(err.Error(),
			"control character in identity string")) {
			t.Errorf("%s: got %v, want a refusal", test.label, err)
		}
		if !test.bad && err != nil {
			t.Errorf("%s: refused a clean string: %v", test.label, err)
		}
	}
}

func TestArchitectureText(t *testing.T) {
	for _, arch := range Architectures() {
		var parsed Architecture
		if err := parsed.UnmarshalText([]byte(arch.Value())); err != nil {
			t.Fatalf("%s: %v", arch.Value(), err)
		}
		if parsed != arch {
			t.Fatalf("%s decoded to %v", arch.Value(), parsed)
		}
	}
	var parsed Architecture
	if err := parsed.UnmarshalText([]byte("riscv64")); err == nil {
		t.Fatal("riscv64 decoded")
	}
}
