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
// parser rejects; each names its approval so a new, unapproved difference cannot
// be added without failing here.
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
					if row.Approval != "" || row.JS != "" {
						t.Fatalf("deviation metadata without a deviation")
					}
				case "D2", "D5", "D6", "D7":
					if row.Approval != "approved-by-owner-decision-4" || row.JS != "accept" {
						t.Fatalf("deviation %s must record its approval", row.Deviation)
					}
					if err.Error() == "" {
						t.Fatalf("deviation %s must name the offending construct", row.Deviation)
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
