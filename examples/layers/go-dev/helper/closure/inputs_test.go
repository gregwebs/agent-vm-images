package closure

// The input-shape, pin-shape and digest rules. Deleting any one of these refusals
// turns one of the cases below green, which is what #188 R-c asks a rule to name.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedClosureLoads is the positive control every refusal below is
// measured against: the committed bytes are accepted, the selection is the
// reviewed size, and every framed notice is there. If this case ever reports an
// implausibly small count, the mutation tests are comparing nothing.
func TestCommittedClosureLoads(t *testing.T) {
	directory := copyCommitted(t)
	data, err := load(t, directory)
	if err != nil {
		t.Fatalf("the committed closure is refused: %v", err)
	}
	if string(data.Pin.Gopls) != "v0.23.0" {
		t.Fatalf("selected release %q", data.Pin.Gopls)
	}
	if string(data.Pin.Go) != "1.27.3" {
		t.Fatalf("pinned compiler %q", data.Pin.Go)
	}
	if len(data.Pin.Modules) < 20 {
		t.Fatalf("selected %d modules, want at least 20: the selection was read as nothing",
			len(data.Pin.Modules))
	}
	if len(data.Notices) < 30 {
		t.Fatalf("framed %d notices, want at least 30: the evidence was read as nothing",
			len(data.Notices))
	}
	if len(data.Wrapper.Requires) != len(data.Pin.Modules) {
		t.Fatalf("wrapper selects %d modules, pin records %d",
			len(data.Wrapper.Requires), len(data.Pin.Modules))
	}
	root := ModuleID{Path: ModulePath(Gopls), Version: ModuleVersion(data.Pin.Gopls)}
	found := false
	for _, module := range data.Pin.Modules {
		if module.ID == root {
			found = true
		}
	}
	if !found {
		t.Fatalf("the selection does not name %s", root)
	}
	if data.Pin.Review.ReviewedOn != "2026-10-26" {
		t.Fatalf("reviewed_on %q", data.Pin.Review.ReviewedOn)
	}
}

// TestSymlinkedDataDirectoryRefuses is the shape rule that has nowhere else to
// live: the directory itself, not an entry in it.
func TestSymlinkedDataDirectoryRefuses(t *testing.T) {
	directory := copyCommitted(t)
	link := filepath.Join(t.TempDir(), "closure-link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	source, err := DirSource(link)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	if _, err := Load(context.Background(), Request{}, source); err == nil {
		t.Fatal("a symlinked data directory is not the directory the pin describes")
	}
}

// TestInputShapeRulesRefuse covers the five-regular-files rule: a missing input, an
// extra input, a symlink and a directory in the input's place.
func TestInputShapeRulesRefuse(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		fragment string
		apply    func(t *testing.T, directory string)
	}{
		{
			name:     "a missing input",
			fragment: "missing input: go.sum",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				remove(t, directory, FileGoSum)
			},
		},
		{
			name:     "an unreviewed extra input",
			fragment: "unreviewed input: .extra",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				writeFile(t, directory, ".extra", "unexpected")
			},
		},
		{
			name:     "a symlinked input",
			fragment: "symlink go.sum",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				remove(t, directory, FileGoSum)
				if err := os.Symlink(string(FileGoMod), filepath.Join(directory,
					string(FileGoSum))); err != nil {
					t.Fatalf("symlink: %v", err)
				}
			},
		},
		{
			// The presence pass is not the same rule as the read: Python reports the
			// missing input before it looks at the extra one, and this case is the
			// only one that distinguishes the two orders. Without it, deleting the
			// presence loop leaves the suite green because the read refuses the same
			// file with the same message.
			name:     "a missing input outranks an unreviewed one",
			fragment: "missing input: go.sum",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				remove(t, directory, FileGoSum)
				writeFile(t, directory, ".extra", "unexpected")
			},
		},
		{
			name:     "a directory where an input belongs",
			fragment: "not a regular file: pin.json",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				remove(t, directory, FilePin)
				if err := os.Mkdir(filepath.Join(directory, string(FilePin)), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyCommitted(t)
			testCase.apply(t, directory)
			requireRefusal(t, directory, testCase.fragment)
		})
	}
}

// TestPinShapeRulesRefuse covers the record's own grammar: the closed key set, the
// integer schema, strict JSON and the wrapper identity.
func TestPinShapeRulesRefuse(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		fragment string
		apply    func(t *testing.T, directory string)
	}{
		{
			name:     "an unknown top-level key",
			fragment: "pin.json: unknown key 'extra'",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					document["extra"] = true
				})
			},
		},
		{
			name:     "a missing top-level key",
			fragment: "pin.json: missing 'gopls'",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					delete(document, "gopls")
				})
			},
		},
		{
			name:     "schema written as a float",
			fragment: "schema: expected an integer",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FilePin, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), `"schema": 1`,
						`"schema": 1.0`, 1))
				})
			},
		},
		{
			name:     "schema written as a boolean",
			fragment: "schema: expected an integer",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FilePin, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), `"schema": 1`,
						`"schema": true`, 1))
				})
			},
		},
		{
			name:     "an unsupported schema",
			fragment: "pin.json: unsupported schema",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					document["schema"] = 2
				})
			},
		},
		{
			name:     "a duplicate key",
			fragment: "duplicate key",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FilePin, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), `"gopls": "v0.23.0"`,
						`"gopls": "v0.23.0", "gopls": "v0.23.0"`, 1))
				})
			},
		},
		{
			name:     "a leading byte-order mark",
			fragment: "leading UTF-8 BOM is not accepted",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FilePin, func(raw []byte) []byte {
					return append([]byte{0xEF, 0xBB, 0xBF}, raw...)
				})
			},
		},
		{
			name:     "the wrapper module is not the reviewed one",
			fragment: "pin.json: module must be " + Wrapper,
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					document["module"] = "agent-vm.invalid/other"
				})
			},
		},
		{
			name:     "gopls is not an exact release",
			fragment: "gopls: expected an exact vX.Y.Z release version",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					document["gopls"] = "v0.23"
				})
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyCommitted(t)
			testCase.apply(t, directory)
			requireRefusal(t, directory, testCase.fragment)
		})
	}
}

// TestContentDigestRulesRefuse covers the four digests the pin carries. The pin
// cannot hash itself, so a pin.json edit is the one that has to be caught by the
// rules it feeds rather than by a digest.
func TestContentDigestRulesRefuse(t *testing.T) {
	for _, name := range FileKeys {
		name := name
		t.Run(string(name), func(t *testing.T) {
			directory := copyCommitted(t)
			rewrite(t, directory, name, func(raw []byte) []byte {
				return append(raw, '\n')
			})
			requireRefusal(t, directory, string(name)+": bytes differ from the committed digest")
		})
	}
	t.Run("a pin that disagrees with its own file digest", func(t *testing.T) {
		directory := copyCommitted(t)
		editPin(t, directory, func(document map[string]any) {
			files := document["files"].(map[string]any)
			files[string(FileLicenses)] = strings.Repeat("0", 64)
		})
		requireRefusal(t, directory,
			string(FileLicenses)+": bytes differ from the committed digest")
	})
}

func remove(t *testing.T, directory string, name Filename) {
	t.Helper()
	if err := os.Remove(filepath.Join(directory, string(name))); err != nil {
		t.Fatalf("remove %s: %v", name, err)
	}
}

func writeFile(t *testing.T, directory, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
