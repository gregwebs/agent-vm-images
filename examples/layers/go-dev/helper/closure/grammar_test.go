package closure

// The two go.mod grammars and the checksum set.
//
// The wrapper rules are byte-level: the committed go.mod must re-render exactly,
// so every case here re-pins the file digests first and is then refused by
// canonicality rather than by a stale hash.
//
// The root evidence cases are the #188 G3 defect pairs. Each is a named refusal,
// and each has a witness in script/test/gopls-closure-parity.py that records what
// the Python oracle still says about the same bytes, so either side changing turns
// the witness red.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWrapperRulesRefuse covers the canonical wrapper: what it may say and how it
// must be spelled.
func TestWrapperRulesRefuse(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		fragment string
		apply    func(t *testing.T, directory string)
	}{
		{
			name:     "a replace directive",
			fragment: "directive not allowed: replace",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				appendTo(t, directory, FileGoMod,
					"\nreplace github.com/fatih/camelcase => github.com/fatih/camelcase v1.0.0\n")
			},
		},
		{
			name:     "a toolchain directive",
			fragment: "directive not allowed: toolchain",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				appendTo(t, directory, FileGoMod, "\ntoolchain go1.99.0\n")
			},
		},
		{
			name:     "a require block that never closes",
			fragment: "go.mod is not in the reviewed canonical form",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
					text := string(raw)
					index := strings.LastIndex(text, ")")
					if index < 0 {
						t.Fatal("the committed wrapper has no require block to truncate")
					}
					return []byte(text[:index] + text[index+1:])
				})
			},
		},
		{
			name:     "requires out of sorted order",
			fragment: "go.mod is not in the reviewed canonical form",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
					lines := strings.Split(string(raw), "\n")
					swapped := false
					for index := 0; index+1 < len(lines); index++ {
						if strings.HasPrefix(lines[index], "\t") &&
							strings.HasPrefix(lines[index+1], "\t") {
							lines[index], lines[index+1] = lines[index+1], lines[index]
							swapped = true
							break
						}
					}
					if !swapped {
						t.Fatal("the committed wrapper has no two adjacent requires to swap")
					}
					return []byte(strings.Join(lines, "\n"))
				})
			},
		},
		{
			name:     "the module directive is not the wrapper",
			fragment: "go.mod: module directive must be " + Wrapper,
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), "module "+Wrapper,
						"module agent-vm.invalid/other", 1))
				})
			},
		},
		{
			name:     "the tool directive is not gopls",
			fragment: "go.mod: tool directive must be exactly " + Gopls,
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), "tool "+Gopls,
						"tool golang.org/x/tools/goplsd", 1))
				})
			},
		},
		{
			name:     "the tool module is not required",
			fragment: "go.mod: the tool module " + Gopls + " is not required",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
					lines := strings.Split(string(raw), "\n")
					kept := make([]string, 0, len(lines))
					for _, line := range lines {
						if strings.HasPrefix(line, "\t"+Gopls+" ") {
							continue
						}
						kept = append(kept, line)
					}
					return []byte(strings.Join(kept, "\n"))
				})
			},
		},
		{
			name:     "the selected release moved without the pin",
			fragment: "go.mod: " + Gopls + " is v0.24.0, not v0.23.0",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), "\t"+Gopls+" v0.23.0",
						"\t"+Gopls+" v0.24.0", 1))
				})
			},
		},
		{
			name:     "a single-line indirect require",
			fragment: "is not in the reviewed canonical form",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				appendTo(t, directory, FileGoMod,
					"require github.com/fatih/camelcase v1.0.0 // indirect\n")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyCommitted(t)
			testCase.apply(t, directory)
			repin(t, directory)
			requireRefusal(t, directory, testCase.fragment)
		})
	}
}

// TestRootEvidenceGrammarRefuses is the #188 G3 evidence-grammar fix. Before it,
// the Python oracle accepted all three of these: it never read the `module`
// directive, it never noticed a require block that was never closed, and it split
// the directive on one space so a tab-separated line was invisible to it.
func TestRootEvidenceGrammarRefuses(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		fragment string
		apply    func(t *testing.T, directory string)
	}{
		{
			name:     "a module directive that names something else",
			fragment: "the module directive must be " + Gopls,
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileRootGoMod, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), "module "+Gopls,
						"module golang.org/x/tools/goplsx", 1))
				})
			},
		},
		{
			name:     "a require block that never closes",
			fragment: "unterminated require block",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileRootGoMod, func(raw []byte) []byte {
					text := string(raw)
					index := strings.LastIndex(text, ")")
					if index < 0 {
						t.Fatal("the committed evidence has no require block to truncate")
					}
					return []byte(text[:index] + text[index+1:])
				})
			},
		},
		{
			name:     "a require entry with three fields",
			fragment: "a require entry must be a path and a version",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileRootGoMod, func(raw []byte) []byte {
					return []byte(strings.Replace(string(raw), "\tmvdan.cc/gofumpt v0.10.0",
						"\tmvdan.cc/gofumpt v0.10.0 extra", 1))
				})
			},
		},
		{
			name:     "a replace directive",
			fragment: "root module declares replace; review required",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				appendTo(t, directory, FileRootGoMod,
					"\nreplace mvdan.cc/gofumpt => mvdan.cc/gofumpt v0.10.0\n")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyCommitted(t)
			testCase.apply(t, directory)
			repin(t, directory)
			requireRefusal(t, directory, testCase.fragment)
		})
	}
}

// TestRootEvidenceReadsTabSeparatedDirectives is the other side of the same
// defect: the line must be *read*, not skipped. The Python oracle dropped it, so
// its adoption rule then refused a module it had never seen -- an acceptance the
// parity suite records as a divergence with both verdicts named.
func TestRootEvidenceReadsTabSeparatedDirectives(t *testing.T) {
	directory := copyCommitted(t)
	rewrite(t, directory, FileRootGoMod, func(raw []byte) []byte {
		return []byte(strings.Replace(string(raw), "\tgithub.com/fsnotify/fsnotify v1.9.0",
			"\tgithub.com/fsnotify/fsnotify\tv1.9.0", 1))
	})
	repinRootEvidence(t, directory)
	data, err := load(t, directory)
	if err != nil {
		t.Fatalf("a tab-separated require entry is legal go.mod and must be read: %v", err)
	}
	if version, present := data.Root.Requires[ModulePath("github.com/fsnotify/fsnotify")]; !present ||
		string(version) != "v1.9.0" {
		t.Fatalf("the tab-separated entry was not read: %v", data.Root.Requires)
	}
}

// TestRootEvidenceAgreesWithTheGoCommand measures the wider grammar against the
// committed bytes (#188 G3 condition 2). If the Go command and this reader ever
// disagree about the committed file, the recorded verdict changes and the case
// must stop and be reported rather than the reader being widened.
func TestRootEvidenceAgreesWithTheGoCommand(t *testing.T) {
	directory := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(committedDir(t), string(FileRootGoMod)))
	if err != nil {
		t.Fatalf("read the committed evidence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), raw, 0o644); err != nil {
		t.Fatalf("write the scratch module: %v", err)
	}
	command := exec.Command("go", "mod", "edit", "-json")
	command.Dir = directory
	command.Env = append(os.Environ(), "GOTOOLCHAIN=auto", "GOWORK=off", "GOPROXY=off")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("the Go command refused the committed evidence (%v): the wider grammar "+
			"disagrees and the difference must be reported, not absorbed", err)
	}
	var document struct {
		Module struct {
			Path string
		}
		Go      string
		Require []struct {
			Path    string
			Version string
		}
	}
	if err := json.Unmarshal(output, &document); err != nil {
		t.Fatalf("decode go mod edit -json: %v", err)
	}
	parsed, err := parseRootGoMod(raw, string(FileRootGoMod))
	if err != nil {
		t.Fatalf("this reader refused the committed evidence: %v", err)
	}
	if document.Module.Path != string(parsed.Module) {
		t.Fatalf("the Go command reads module %q, this reader %q", document.Module.Path,
			parsed.Module)
	}
	if document.Go != string(parsed.Go) {
		t.Fatalf("the Go command reads go %q, this reader %q", document.Go, parsed.Go)
	}
	if len(document.Require) != len(parsed.Requires) {
		t.Fatalf("the Go command reads %d requires, this reader %d", len(document.Require),
			len(parsed.Requires))
	}
	for _, required := range document.Require {
		if string(parsed.Requires[ModulePath(required.Path)]) != required.Version {
			t.Fatalf("the Go command reads %s %s, this reader %q", required.Path,
				required.Version, parsed.Requires[ModulePath(required.Path)])
		}
	}
}

// TestGoSumRulesRefuse covers both checksum kinds per selected module and Go's own
// line order.
func TestGoSumRulesRefuse(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		fragment string
		apply    func(t *testing.T, directory string)
	}{
		{
			name:     "the root's zip h1 removed",
			fragment: "go.sum lacks h1 for " + Gopls + "@v0.23.0",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				dropLine(t, directory, FileGoSum, Gopls+" v0.23.0 h1:")
			},
		},
		{
			name:     "the root's go.mod h1 removed",
			fragment: "go.sum lacks go.mod hash for " + Gopls + "@v0.23.0",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				dropLine(t, directory, FileGoSum, Gopls+" v0.23.0/go.mod h1:")
			},
		},
		{
			name:     "a module's h1 disagrees with the pin",
			fragment: "h1 differs from go.sum",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				editPin(t, directory, func(document map[string]any) {
					modules := document["modules"].([]any)
					for _, raw := range modules {
						module := raw.(map[string]any)
						if module["path"] == "github.com/fatih/camelcase" {
							root := ""
							for _, other := range modules {
								candidate := other.(map[string]any)
								if candidate["path"] == "github.com/BurntSushi/toml" {
									root = candidate["h1"].(string)
								}
							}
							module["h1"] = root
						}
					}
				})
			},
		},
		{
			name:     "a module's go.mod h1 disagrees with the pin",
			fragment: "go.mod h1 differs from go.sum",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoSum, func(raw []byte) []byte {
					return []byte(replaceDigestCharacter(string(raw),
						"github.com/fatih/camelcase v1.0.0/go.mod h1:"))
				})
			},
		},
		{
			name:     "a conflicting duplicate line",
			fragment: "conflicting h1 for ",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				appendTo(t, directory, FileGoSum,
					"github.com/fatih/camelcase v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n")
			},
		},
		{
			name:     "two entries out of semver order",
			fragment: "unsorted or duplicated entries",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				rewrite(t, directory, FileGoSum, func(raw []byte) []byte {
					lines := strings.Split(string(raw), "\n")
					for index := 0; index+1 < len(lines); index++ {
						first, second := lines[index], lines[index+1]
						if strings.HasPrefix(first, "github.com/fatih/camelcase ") &&
							strings.HasPrefix(second, "github.com/fatih/camelcase ") {
							lines[index], lines[index+1] = second, first
							return []byte(strings.Join(lines, "\n"))
						}
					}
					t.Fatal("the committed go.sum has no adjacent pair to swap")
					return nil
				})
			},
		},
		{
			name:     "a malformed line",
			fragment: "go.sum: malformed line:",
			apply: func(t *testing.T, directory string) {
				t.Helper()
				appendTo(t, directory, FileGoSum, "mvdan.cc/gofumpt v0.10.0\n")
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyCommitted(t)
			testCase.apply(t, directory)
			// The checksum rules are reached only once the file digests agree with
			// the mutated bytes: otherwise every case here would be the digest rule.
			repin(t, directory)
			requireRefusal(t, directory, testCase.fragment)
		})
	}
}

// TestRootEvidenceH1RefusesMismatch keeps the independent root go.mod check: the
// evidence bytes must hash to the go.sum go.mod h1 even when the file digests were
// re-pinned around them.
func TestRootEvidenceH1RefusesMismatch(t *testing.T) {
	directory := copyCommitted(t)
	appendTo(t, directory, FileRootGoMod, "\n// re-pinned evidence only\n")
	repin(t, directory)
	requireRefusal(t, directory, "root go.mod evidence does not match go.sum")
}

func appendTo(t *testing.T, directory string, name Filename, text string) {
	t.Helper()
	rewrite(t, directory, name, func(raw []byte) []byte {
		return append(raw, []byte(text)...)
	})
}

// replaceDigestCharacter changes one base64 character of the h1 on the first line
// named by prefix, keeping the checksum's shape valid: a malformed hash would test
// the parser instead of the disagreement.
func replaceDigestCharacter(text, prefix string) string {
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		digest := strings.TrimPrefix(line, prefix)
		replacement := "A"
		if strings.HasPrefix(digest, "A") {
			replacement = "B"
		}
		return strings.Replace(text, line, prefix+replacement+digest[1:], 1)
	}
	return text
}

// repinRootEvidence re-binds the committed root evidence bytes to the go.sum go.mod
// h1, for the cases that must reach a rule after the h1 check.
func repinRootEvidence(t *testing.T, directory string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(directory, string(FileRootGoMod)))
	if err != nil {
		t.Fatalf("read %s: %v", FileRootGoMod, err)
	}
	digest := string(h1GoMod(raw))
	rewrite(t, directory, FileGoSum, func(current []byte) []byte {
		lines := strings.Split(string(current), "\n")
		replaced := false
		for index, line := range lines {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[0] != Gopls || !strings.HasSuffix(fields[1], "/go.mod") {
				continue
			}
			lines[index] = fields[0] + " " + fields[1] + " " + digest
			replaced = true
		}
		if !replaced {
			t.Fatalf("%s: no root go.mod line to re-bind", FileGoSum)
		}
		return []byte(strings.Join(lines, "\n"))
	})
	editPin(t, directory, func(document map[string]any) {
		for _, raw := range document["modules"].([]any) {
			module := raw.(map[string]any)
			if module["path"] == Gopls {
				module["go_mod_h1"] = digest
			}
		}
	})
	repin(t, directory)
}

// dropLine removes the first line containing the fragment.
func dropLine(t *testing.T, directory string, name Filename, fragment string) {
	t.Helper()
	rewrite(t, directory, name, func(raw []byte) []byte {
		lines := strings.Split(string(raw), "\n")
		kept := make([]string, 0, len(lines))
		dropped := false
		for _, line := range lines {
			if !dropped && strings.Contains(line, fragment) {
				dropped = true
				continue
			}
			kept = append(kept, line)
		}
		if !dropped {
			t.Fatalf("%s: no line contains %q", name, fragment)
		}
		return []byte(strings.Join(kept, "\n"))
	})
}
