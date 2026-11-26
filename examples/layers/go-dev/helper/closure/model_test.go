package closure

// The model rules: adoption, the compiler/language relation, module uniqueness,
// the dated scoped review and the caller's assertions.

import (
	"strings"
	"testing"
)

// TestAdoptionRuleRefuses is the reviewed current-pin adoption restriction: every
// module the wrapper selects must be named by the root's own go.mod at the same
// version.
func TestAdoptionRuleRefuses(t *testing.T) {
	t.Run("a selected version the root does not require", func(t *testing.T) {
		directory := copyCommitted(t)
		rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), "\tgithub.com/fsnotify/fsnotify v1.9.0",
				"\tgithub.com/fsnotify/fsnotify v1.10.0", 1))
		})
		repin(t, directory)
		requireRefusal(t, directory, "differs from the root module's requirement")
	})
	t.Run("a selected module the root does not name", func(t *testing.T) {
		directory := copyCommitted(t)
		rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), "\tgithub.com/fatih/structtag v1.2.0",
				"\tgithub.com/fatih/structtagx v1.2.0", 1))
		})
		repin(t, directory)
		requireRefusal(t, directory, "is not required by the root module; review required")
	})
	// The verdict-level witness: a requirement removed from the root's own evidence,
	// with its h1, the pin's digests and the scope all re-bound. Nothing else can
	// refuse these bytes, so deleting the adoption rule accepts them -- which is what
	// the Go test and the parity case both assert.
	t.Run("a root requirement removed from the evidence", func(t *testing.T) {
		directory := copyCommitted(t)
		rewrite(t, directory, FileRootGoMod, func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw),
				"\tgithub.com/fatih/structtag v1.2.0 // indirect\n", "", 1))
		})
		repinRootEvidence(t, directory)
		requireRefusal(t, directory, "is not required by the root module; review required")
	})
	// The remediation text is part of the contract: a maintainer acts on it.
	t.Run("the refusal carries the remediation", func(t *testing.T) {
		directory := copyCommitted(t)
		rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), "\tgithub.com/fsnotify/fsnotify v1.9.0",
				"\tgithub.com/fsnotify/fsnotify v1.10.0", 1))
		})
		repin(t, directory)
		_, err := load(t, directory)
		if err == nil || !strings.Contains(err.Error(), "current-pin adoption restriction "+
			"changed; inspect upstream go.mod and both Linux build lists; update the rule "+
			"and ADR in a reviewed proposal") {
			t.Fatalf("the adoption refusal must carry the remediation, got %v", err)
		}
	})
}

// TestGoLanguageRelationRefuses holds both halves of the go directive rule: it
// must equal the root module's, and it must not be newer than the pinned compiler.
func TestGoLanguageRelationRefuses(t *testing.T) {
	t.Run("the wrapper and the root disagree", func(t *testing.T) {
		directory := copyCommitted(t)
		rewrite(t, directory, FileGoMod, func(raw []byte) []byte {
			return []byte(strings.Replace(string(raw), "\ngo 1.26.0\n", "\ngo 1.26.1\n", 1))
		})
		repin(t, directory)
		requireRefusal(t, directory, "go directive 1.26.1 does not equal the root module's 1.26.0")
	})
	t.Run("the language is newer than the compiler", func(t *testing.T) {
		directory := copyCommitted(t)
		for _, name := range []Filename{FileGoMod, FileRootGoMod} {
			rewrite(t, directory, name, func(raw []byte) []byte {
				return []byte(strings.Replace(string(raw), "\ngo 1.26.0\n", "\ngo 1.99.0\n", 1))
			})
		}
		repinRootEvidence(t, directory)
		requireRefusal(t, directory, "go directive 1.99.0 is newer than the pinned compiler go1.27.3")
	})
}

// TestModuleUniquenessRefuses is the #188 G3 collapse: two versions of one module
// path used to reach a path-keyed map, so the second version vanished before the
// version comparisons ran. The duplicate-row case is the same map with the same
// identity twice.
func TestModuleUniquenessRefuses(t *testing.T) {
	t.Run("two versions of one module path", func(t *testing.T) {
		directory := copyCommitted(t)
		addSecondVersion(t, directory, "github.com/fatih/camelcase", "v1.0.0", "v0.0.1")
		repin(t, directory)
		requireRefusal(t, directory, "is selected at both")
	})
	// The duplicate-row rule names the defect earlier than the duplicate-notice rule
	// would; both refuse, so deleting it changes the message, not the verdict. The
	// verdict-level witness for the uniqueness block is the two-versions case above
	// (and the parity row for it). Stated because a rule that only reorders a
	// refusal must not be presented as an acceptance boundary.
	t.Run("the same module row twice", func(t *testing.T) {
		directory := copyCommitted(t)
		editPin(t, directory, func(document map[string]any) {
			modules := document["modules"].([]any)
			document["modules"] = append(modules, modules[0])
		})
		repin(t, directory)
		requireRefusal(t, directory, "pin.json: duplicate module")
	})
	t.Run("modules out of path order", func(t *testing.T) {
		directory := copyCommitted(t)
		editPin(t, directory, func(document map[string]any) {
			modules := document["modules"].([]any)
			modules[0], modules[1] = modules[1], modules[0]
		})
		repin(t, directory)
		requireRefusal(t, directory, "pin.json: modules must be sorted by path")
	})
}

// TestReviewScopeRefuses covers the dated, scoped determination: the policy
// strings, the scope digests, the date, the reviewer and the evidence.
func TestReviewScopeRefuses(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		fragment string
		// noRepin skips the re-pin for a case whose mutation *is* a scope digest:
		// re-pinning would restore the value under test.
		noRepin bool
		edit    func(document map[string]any)
	}{
		{
			name:     "the scope names another root",
			fragment: "scope root",
			edit: func(document map[string]any) {
				authentication(document)["scope"].(map[string]any)["root"] =
					"golang.org/x/tools/gopls@v0.22.0"
			},
		},
		{
			name:     "the scope digests do not cover the committed files",
			fragment: "the reviewed go.mod/go.sum digests differ from the committed files",
			noRepin:  true,
			edit: func(document map[string]any) {
				authentication(document)["scope"].(map[string]any)["go.sum"] =
					strings.Repeat("0", 64)
			},
		},
		{
			name:     "a date that is not a calendar day",
			fragment: "not a calendar date: 2026-02-30",
			edit: func(document map[string]any) {
				authentication(document)["reviewed_on"] = "2026-02-30"
			},
		},
		{
			name:     "an unsupported determination",
			fragment: "unsupported determination",
			edit: func(document map[string]any) {
				authentication(document)["determination"] = "module-channel-trust-me-v1"
			},
		},
		{
			name:     "a sumdb that is not the reviewed key",
			fragment: "sumdb is not the reviewed verifier key",
			edit: func(document map[string]any) {
				authentication(document)["sumdb"] = "sum.golang.org"
			},
		},
		{
			name:     "a proxy that is not the reviewed origin",
			fragment: "proxy is not the reviewed origin",
			edit: func(document map[string]any) {
				authentication(document)["proxy"] = "https://proxy.golang.org,direct"
			},
		},
		{
			name:     "a follow-up that is not the filed issue",
			fragment: "follow-up must be the filed #94 sub-issue",
			edit: func(document map[string]any) {
				authentication(document)["follow_up"] = "https://example.invalid/issue"
			},
		},
		{
			name:     "no recorded evidence",
			fragment: "evidence must be recorded",
			edit: func(document map[string]any) {
				authentication(document)["evidence"] = []any{}
			},
		},
		{
			name:     "evidence that is not an HTTPS URL",
			fragment: "expected an HTTPS URL",
			edit: func(document map[string]any) {
				authentication(document)["evidence"] = []any{"http://example.invalid/x"}
			},
		},
		{
			name:     "a reviewer handle that is not one",
			fragment: "expected a reviewer handle",
			edit: func(document map[string]any) {
				authentication(document)["reviewer"] = "two words"
			},
		},
		{
			name:     "an empty reason",
			fragment: "reason and reassess_when must be recorded",
			edit: func(document map[string]any) {
				authentication(document)["reason"] = "   "
			},
		},
		{
			name:     "a signature claimed as verified",
			fragment: "does not verify VCS signatures",
			edit: func(document map[string]any) {
				signatures(document)[0].(map[string]any)["verified"] = true
			},
		},
		{
			name:     "a signature inventory row that outlived its version",
			fragment: "but the closure selects",
			edit: func(document map[string]any) {
				signatures(document)[0].(map[string]any)["version"] = "v1.7.0"
			},
		},
		{
			name:     "a signature for a module the closure does not select",
			fragment: "which the closure does not select",
			edit: func(document map[string]any) {
				signatures(document)[0].(map[string]any)["path"] = "github.com/example/absent"
			},
		},
		{
			name:     "an authenticated provenance claim",
			fragment: "unauthenticated metadata",
			edit: func(document map[string]any) {
				document["provenance"].(map[string]any)["authenticated"] = true
			},
		},
		{
			name:     "an origin that is not this release's proxy document",
			fragment: "not the canonical proxy .info origin for this release",
			edit: func(document map[string]any) {
				document["provenance"].(map[string]any)["source"] =
					"https://proxy.golang.org/golang.org/x/tools/gopls/@v/v0.22.0.info"
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := copyCommitted(t)
			editPin(t, directory, testCase.edit)
			if !testCase.noRepin {
				repin(t, directory)
			}
			requireRefusal(t, directory, testCase.fragment)
		})
	}
}

// TestExpectationAssertionsRefuse holds the two optional assertions the recipe's
// ARGs become. They fire last, so a stale closure reports its own rule first.
func TestExpectationAssertionsRefuse(t *testing.T) {
	directory := copyCommitted(t)
	source, err := DirSource(directory)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	wrongRelease := ReleaseVersion("v0.24.0")
	if _, err := Load(t.Context(), Request{ExpectGopls: &wrongRelease},
		source); err == nil || !strings.Contains(err.Error(),
		"asserted gopls version v0.24.0 != pinned v0.23.0") {
		t.Fatalf("the gopls assertion did not fire: %v", err)
	}
	wrongCompiler := GoVersion("1.27.4")
	if _, err := Load(t.Context(), Request{ExpectGo: &wrongCompiler},
		source); err == nil || !strings.Contains(err.Error(),
		"pin generated with go1.27.3; the layer installs go1.27.4") {
		t.Fatalf("the Go assertion did not fire: %v", err)
	}
}

func authentication(document map[string]any) map[string]any {
	return document["authentication"].(map[string]any)
}

func signatures(document map[string]any) []any {
	return authentication(document)["known_vcs_signatures"].([]any)
}

// addSecondVersion inserts a second version of one module path into the selection
// with its own frames and its own go.sum lines, so nothing but the uniqueness rule
// can refuse it. The insertion is *before* the real row, because a path-keyed map
// kept the last row: that is the collapse #188 G3 describes.
func addSecondVersion(t *testing.T, directory, path, version, second string) {
	t.Helper()
	var body []byte
	readings := map[string]string{}
	editPin(t, directory, func(document map[string]any) {
		var original map[string]any
		for _, raw := range document["modules"].([]any) {
			module := raw.(map[string]any)
			if module["path"] == path && module["version"] == version {
				original = module
			}
		}
		if original == nil {
			t.Fatalf("the committed pin does not select %s@%s", path, version)
		}
		readings["h1"] = original["h1"].(string)
		readings["go_mod_h1"] = original["go_mod_h1"].(string)
		duplicate := map[string]any{}
		for key, value := range original {
			duplicate[key] = value
		}
		duplicate["version"] = second
		// Immediately before the real row: the path order the reader also checks stays
		// untouched, so the one-version-per-path rule is the only one in question.
		modules := document["modules"].([]any)
		for index, raw := range modules {
			module := raw.(map[string]any)
			if module["path"] == path && module["version"] == version {
				document["modules"] = append(append(append([]any{}, modules[:index]...),
					duplicate), modules[index:]...)
				return
			}
		}
		t.Fatalf("the committed pin does not select %s@%s", path, version)
	})
	// The frames move with the version, or the notice set refuses first.
	rewrite(t, directory, FileLicenses, func(raw []byte) []byte {
		header, frames := noticesOf(t, directory)
		added := [][]byte{}
		for _, candidate := range frames {
			if strings.HasPrefix(string(candidate), "=== "+path+" "+version+" ") {
				body = append([]byte{}, candidate...)
			}
		}
		if body == nil {
			t.Fatalf("no frame for %s@%s", path, version)
		}
		text := strings.Replace(string(body), "=== "+path+" "+version+" ",
			"=== "+path+" "+second+" ", 1)
		added = append(added, []byte(text))
		return append(append([]byte{}, header...), joinFrames(append(frames, added...))...)
	})
	rewrite(t, directory, FileGoSum, func(raw []byte) []byte {
		lines := strings.Split(string(raw), "\n")
		kept := make([]string, 0, len(lines)+2)
		inserted := false
		for _, line := range lines {
			if !inserted && strings.HasPrefix(line, path+" "+version+" ") {
				kept = append(kept,
					path+" "+second+" "+readings["h1"],
					path+" "+second+"/go.mod "+readings["go_mod_h1"])
				inserted = true
			}
			kept = append(kept, line)
		}
		if !inserted {
			t.Fatalf("%s: no lines for %s %s", FileGoSum, path, version)
		}
		return []byte(strings.Join(kept, "\n"))
	})
}
