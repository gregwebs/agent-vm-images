package pnpmlock

import (
	"strings"
	"testing"
)

// JS maps every term before combining alternatives; unprovable terms stop it.
func TestFamilyRangeChecksEveryTerm(t *testing.T) {
	selected, err := ParseExactVersion("1.1.4")
	if err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{"^1.1.4 || 1.x", ">=1.0.0 || ^2.0.0-rc.1", "1.1.4 || 1.0.0 - 2.0.0", "<1.0.0 1.x"} {
		t.Run(wanted, func(t *testing.T) {
			_, err := satisfiesFamilyRange(selected, wanted)
			if err == nil || !strings.Contains(err.Error(), "unprovable family range") {
				t.Fatalf("got %v", err)
			}
		})
	}
}
