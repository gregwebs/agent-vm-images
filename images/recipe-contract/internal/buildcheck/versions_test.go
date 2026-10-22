package buildcheck

import (
	"go/version"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLanguageMinimum(t *testing.T) {
	directives := readGoMod(t)
	language, toolchain := "go"+directives["go"], directives["toolchain"]
	if !version.IsValid(language) || !version.IsValid(toolchain) {
		t.Fatalf("invalid go.mod versions: %v", directives)
	}
	if version.Compare(language, toolchain) > 0 {
		t.Fatalf("language minimum %s is newer than toolchain %s", language, toolchain)
	}
}

func readGoMod(t *testing.T) map[string]string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		candidate := filepath.Join(directory, "go.mod")
		if data, err := os.ReadFile(candidate); err == nil {
			return parseDirectives(t, string(data))
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("no go.mod above %s", directory)
		}
		directory = parent
	}
}

func parseDirectives(t *testing.T, contents string) map[string]string {
	t.Helper()
	directives := make(map[string]string)
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "go", "toolchain":
			directives[fields[0]] = fields[1]
		}
	}
	if directives["go"] == "" || directives["toolchain"] == "" {
		t.Fatalf("go.mod must declare both go and toolchain: %v", directives)
	}
	return directives
}

func TestVersionOrdering(t *testing.T) {
	for _, pair := range [][2]string{{"go1.27.9", "go1.27.10"}, {"go1.28rc1", "go1.28.0"}} {
		if !version.IsValid(pair[0]) || !version.IsValid(pair[1]) || version.Compare(pair[0], pair[1]) >= 0 {
			t.Fatalf("invalid version ordering: %v", pair)
		}
	}
}
