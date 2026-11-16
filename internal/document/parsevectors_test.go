package document

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// parseVector is one row of the corpus captured from npm yaml@2.9.0 through the
// retired parse(). Accepted rows pin the emitted JSON bytes; rejected rows pin a
// message pattern. Deviation rows are inputs the retired parser accepted and this
// parser rejects; each names the owner decision that approved the rejection.
// JS outcomes are captured independently, never inferred from this parser.
type parseVector struct {
	Source    string `json:"source"`
	Format    string `json:"format"`
	Outcome   string `json:"outcome"`
	JSON      string `json:"json"`
	Match     string `json:"match"`
	Deviation string `json:"deviation"`
	Approval  string `json:"approval"`
	JS        string `json:"js"`
}

func loadParseVectors(t *testing.T) []parseVector {
	t.Helper()
	raw, err := os.ReadFile("testdata/parse-vectors.json")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var rows []parseVector
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(rows) < 100 {
		t.Fatalf("corpus shrank to %d rows", len(rows))
	}
	return rows
}

func TestParseVectors(t *testing.T) {
	for _, row := range loadParseVectors(t) {
		name := row.Format + "/" + truncate(row.Source)
		t.Run(name, func(t *testing.T) {
			format := YAML
			if row.Format == "json" {
				format = JSON
			}
			value, err := Parse([]byte(row.Source), format)
			switch row.Outcome {
			case "accept":
				if row.Deviation != "" {
					t.Fatalf("an accepted row cannot carry a deviation")
				}
				if err != nil {
					t.Fatalf("expected accept, got error %q", err)
				}
				if got := string(value.AppendJS(nil)); got != row.JSON {
					t.Fatalf("value differs\n got: %s\nwant: %s", got, row.JSON)
				}
			case "reject":
				if err == nil {
					t.Fatalf("expected rejection, got %s", value.Describe())
				}
				matched, matchErr := regexp.MatchString(row.Match, err.Error())
				if matchErr != nil {
					t.Fatalf("bad pattern %q: %v", row.Match, matchErr)
				}
				if !matched {
					t.Fatalf("message %q does not match %q", err.Error(), row.Match)
				}
				switch row.Deviation {
				case "":
					if row.Approval != "" || row.JS != "reject" {
						t.Fatalf("deviation metadata without a deviation")
					}
				case "D2", "D5", "D6", "D7":
					if row.Approval != "approved-by-owner-decision-4" || row.JS != "accept" {
						t.Fatalf("deviation %s must record its approval", row.Deviation)
					}
					if row.Match == ".+" || row.Match == "" {
						t.Fatalf("approved deviation %s needs a construct-specific pattern", row.Deviation)
					}
				case "F5", "F6", "F4-CR", "YAML-CR", "R2-2":
					if row.Approval != "approved-by-owner-decision-6" || row.JS != "accept" || row.JSON == "" {
						t.Fatal("owner decision 6 deviation must retain the oracle outcome and its approval")
					}
					if row.Match == ".+" || row.Match == "" {
						t.Fatalf("approved deviation %s needs a diagnostic pattern", row.Deviation)
					}
				default:
					t.Fatalf("unapproved deviation %q", row.Deviation)
				}
			default:
				t.Fatalf("unknown outcome %q", row.Outcome)
			}
		})
	}
}

// TestTagSpellingsCovered keeps the source-scanner contract visible: every tag
// spelling in the design has a rejection row, and every quoted/block/plain/
// comment control has an acceptance row.
func TestTagSpellingsCovered(t *testing.T) {
	rejects := []string{"!", "! x\n", "x: ! 1\n", "- ! x\n", "? ! x\n: y\n", "{\"a\":! x}\n",
		"[a, !t b]\n", "!!str x\n", "!<tag> x\n", "--- ! x\n"}
	accepts := []string{"x: \"!x\"\n", "x: '!x'\n", "x: |\n  !\n", "x: a!b\n", "x: a ! b\n",
		"x: a\n  !b\n", "x: a # !\n"}
	for _, want := range rejects {
		if !hasVector(t, want, "reject") {
			t.Errorf("missing rejection vector %q", want)
		}
	}
	for _, want := range accepts {
		if !hasVector(t, want, "accept") {
			t.Errorf("missing acceptance vector %q", want)
		}
	}
}

func hasVector(t *testing.T, source, outcome string) bool {
	t.Helper()
	for _, row := range loadParseVectors(t) {
		if row.Source == source && row.Outcome == outcome {
			return true
		}
	}
	return false
}

func truncate(source string) string {
	text := strings.ReplaceAll(source, "\n", "\\n")
	if len(text) > 24 {
		return text[:24]
	}
	return text
}

// Owner decision 6 approved these classes as fail-closed exceptions. The port may
// reject them; it may never accept them, so every row keeps outcome "reject" and
// the oracle's accepted value. Only the owner-approved enumerations carry an approval.
func TestDecision6RestrictionClassifications(t *testing.T) {
	rows := loadParseVectors(t)
	foundTabComment, foundNoncharacter := false, false
	for _, row := range rows {
		if row.Format != "yaml" {
			continue
		}
		if row.Source == "\t#c\nx: 1\n" {
			foundTabComment = true
			if row.Deviation != "F6" || row.Approval != "approved-by-owner-decision-6" || row.JSON != `{"x":1}` {
				t.Fatal("tab-indented comment must retain its approval, rejection and JS value")
			}
		}
		if strings.ContainsRune(row.Source, '\uffff') {
			foundNoncharacter = true
			if row.Deviation != "F6" || row.Approval != "approved-by-owner-decision-6" || row.JS != "accept" || row.JSON == "" {
				t.Fatal("U+FFFF is approved by owner decision 6, not by D6")
			}
		}
	}
	if !foundTabComment || !foundNoncharacter {
		t.Fatal("decision-6 restriction controls missing")
	}
}

// TestDecision6SyntaxFamilies pins the four syntax families approved by owner
// decision 6: the port rejects each one, and the JS accepted it with this value.
func TestDecision6SyntaxFamilies(t *testing.T) {
	controls := map[string]string{
		"? foo\n : bar\n":          `{"foo":"bar"}`,
		"{a\nx: 1}\n":              `{"a x":1}`,
		" \tx\n":                   `"x"`,
		"- \tx\n":                  `["x"]`,
		"- a:\n  #c\n   |-\n  v\n": `[{"a":"v"}]`,
	}
	for _, row := range loadParseVectors(t) {
		want, ok := controls[row.Source]
		if !ok || row.Format != "yaml" {
			continue
		}
		if row.Approval != "approved-by-owner-decision-6" || row.JS != "accept" || row.JSON != want || row.Deviation != "R2-2" || row.Outcome != "reject" {
			t.Fatalf("syntax family lost its approval or JS value: %q", row.Source)
		}
		delete(controls, row.Source)
	}
	if len(controls) != 0 {
		t.Fatalf("missing decision-6 syntax controls: %v", controls)
	}
}
