package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// TestLoaderSelectPrintsTheCanonicalURLDigestAndSize: `select` prints the pinned URL,
// digest and size, in that order, on one line.
func TestLoaderSelectPrintsTheCanonicalURLDigestAndSize(t *testing.T) {
	fixture := testFixture(t)
	pin := readPin(t, fixture.closure)
	for _, name := range platforms {
		result := accepts(t, pinInvocation("select", fixture.closure, "--version", Version,
			"--platform", string(name.token)))
		pinned := object(t, pin, "platforms", name.key)
		want := fmt.Sprintf("%v %v %v\n", pinned["url"], pinned["sha256"], pinned["size"])
		if result.Stdout != want {
			t.Fatalf("select %s printed %q, want %q", name.token, result.Stdout, want)
		}
	}
}

// TestLoaderUnknownOrMissingKeyAtEveryLevel: every document in the closure has an exact
// key set, so an unknown or missing key at any level is refused by name.
func TestLoaderUnknownOrMissingKeyAtEveryLevel(t *testing.T) {
	fixture := testFixture(t)
	rows := []struct {
		label   string
		mutate  func(map[string]any)
		needles []string
	}{
		{
			label:   "root extra key",
			mutate:  func(pin map[string]any) { pin["extra"] = 1 },
			needles: []string{"pin must have exactly", "unexpected [extra]"},
		},
		{
			label:   "root missing key",
			mutate:  func(pin map[string]any) { delete(pin, "authentication") },
			needles: []string{"pin must have exactly", "missing [authentication]"},
		},
		{
			label: "manifest extra key",
			mutate: func(pin map[string]any) {
				object(t, pin, "manifest")["note"] = "x"
			},
			needles: []string{"manifest evidence must have exactly", "unexpected [note]"},
		},
		{
			label: "signature missing key",
			mutate: func(pin map[string]any) {
				delete(object(t, pin, "signature"), "sha256")
			},
			needles: []string{"signature evidence must have exactly", "missing [sha256]"},
		},
		{
			label:   "key extra key",
			mutate:  func(pin map[string]any) { object(t, pin, "key")["uid"] = "x" },
			needles: []string{"pin key must have exactly", "unexpected [uid]"},
		},
		{
			label: "platform extra key",
			mutate: func(pin map[string]any) {
				object(t, pin, "platforms", "linux/amd64")["note"] = 1
			},
			needles: []string{"linux/amd64 platform must have exactly", "unexpected [note]"},
		},
		{
			label: "authentication extra key",
			mutate: func(pin map[string]any) {
				object(t, pin, "authentication")["note"] = 1
			},
			needles: []string{"pin authentication must have exactly", "unexpected [note]"},
		},
	}
	for _, row := range rows {
		t.Run(row.label, func(t *testing.T) {
			loadRefusal(t, fixture.mutate(t, row.label, row.mutate), row.needles...)
		})
	}
}

// TestLoaderDuplicateJSONKey: a repeated key is refused rather than resolved to one of
// the two values.
func TestLoaderDuplicateJSONKey(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.copy(t, "duplicate-key")
	path := filepath.Join(directory, "pin.json")
	needle := `"version": "` + Version + `"`
	text := string(harness.ReadFile(t, path))
	if count := strings.Count(text, needle); count != 1 {
		t.Fatalf("the fixture pin repeats %s %d times", needle, count)
	}
	harness.WriteFile(t, path, []byte(strings.Replace(text, needle, needle+", "+needle, 1)))
	loadRefusal(t, directory, "duplicate JSON key")
}

// TestLoaderFloatAndNaNNumber: a size that is a fraction, and one that is the NaN
// constant Python's decoder accepts, are both refused as not being JSON numbers.
func TestLoaderFloatAndNaNNumber(t *testing.T) {
	fixture := testFixture(t)
	t.Run("fractional size", func(t *testing.T) {
		loadRefusal(t, fixture.mutate(t, "float-size", func(pin map[string]any) {
			object(t, pin, "platforms", "linux/arm64")["size"] = 1.5
		}), "has a fractional part")
	})
	t.Run("NaN size", func(t *testing.T) {
		directory := fixture.copy(t, "nan-size")
		path := filepath.Join(directory, "pin.json")
		// encoding/json cannot render a non-finite number, so the constant is written
		// where a string stands and the quotes are then removed: the file the reader
		// sees holds `NaN`, which is what the deleted suite wrote with json's
		// allow_nan.
		rewritePin(t, directory, func(pin map[string]any) {
			object(t, pin, "platforms", "linux/arm64")["size"] = "NAN-CONSTANT"
		})
		text := strings.Replace(string(harness.ReadFile(t, path)), `"NAN-CONSTANT"`, "NaN", 1)
		if !strings.Contains(text, "NaN") {
			t.Fatal("the fixture pin was not given the NaN constant")
		}
		harness.WriteFile(t, path, []byte(text))
		loadRefusal(t, directory, "is not a JSON number")
	})
}

// TestLoaderSchemaNameKindAuthentication: the pin's identity and its review record are
// held to one value each.
func TestLoaderSchemaNameKindAuthentication(t *testing.T) {
	fixture := testFixture(t)
	rows := []struct {
		label  string
		mutate func(map[string]any)
		needle string
	}{
		{"schema", func(pin map[string]any) { pin["schema"] = 2 }, "pin schema must be 1"},
		{"name", func(pin map[string]any) { pin["name"] = "codex" }, "pin name must be claude"},
		{"kind", func(pin map[string]any) { pin["kind"] = "release-archive" },
			"pin kind must be claude-native-release"},
		{"method", func(pin map[string]any) {
			object(t, pin, "authentication")["method"] = "hash-only"
		}, "method must be openpgp-signed-manifest"},
		{"verified_on", func(pin map[string]any) {
			object(t, pin, "authentication")["verified_on"] = "2026-13-01"
		}, "is not a calendar date"},
	}
	for _, row := range rows {
		t.Run(row.label, func(t *testing.T) {
			loadRefusal(t, fixture.mutate(t, row.label+"-wrong", row.mutate), row.needle)
		})
	}
}

// TestLoaderVersionGrammar: only an exact version can be pinned.
func TestLoaderVersionGrammar(t *testing.T) {
	fixture := testFixture(t)
	for _, bad := range []string{"latest", "1.2", "01.2.3", "1.2.3+b", "1.2.3 ", ""} {
		t.Run(fmt.Sprintf("%q", bad), func(t *testing.T) {
			loadRefusal(t, fixture.mutate(t, "version-grammar", func(pin map[string]any) {
				pin["version"] = bad
			}), "version")
		})
	}
}

// TestLoaderNonCanonicalEvidenceAndExecutableURLs: every location in the pin is
// recomputed from the version, so a pin that names another host, scheme, version or
// path shape is refused.
func TestLoaderNonCanonicalEvidenceAndExecutableURLs(t *testing.T) {
	fixture := testFixture(t)
	t.Run("manifest url", func(t *testing.T) {
		for label, url := range map[string]string{
			"other host": "https://downloads.claude.ai.evil.example/claude-code-releases/" +
				Version + "/manifest.json",
			"http scheme": "http://downloads.claude.ai/claude-code-releases/" + Version +
				"/manifest.json",
			"other version": "https://downloads.claude.ai/claude-code-releases/9.9.8/manifest.json",
			"query": "https://downloads.claude.ai/claude-code-releases/" + Version +
				"/manifest.json?x=1",
			"trailing slash": "https://downloads.claude.ai/claude-code-releases/" + Version +
				"/manifest.json/",
		} {
			t.Run(label, func(t *testing.T) {
				loadRefusal(t, fixture.mutate(t, "manifest-url", func(pin map[string]any) {
					object(t, pin, "manifest")["url"] = url
				}), "manifest evidence url must be")
			})
		}
	})
	t.Run("signature path", func(t *testing.T) {
		loadRefusal(t, fixture.mutate(t, "signature-path", func(pin map[string]any) {
			object(t, pin, "signature")["path"] = "evidence/other.json"
		}), "signature evidence path must be")
	})
	t.Run("key url", func(t *testing.T) {
		loadRefusal(t, fixture.mutate(t, "key-url", func(pin map[string]any) {
			object(t, pin, "key")["url"] = "https://downloads.claude.ai/keys/other.asc"
		}), "pin key url must be")
	})
	t.Run("platform url", func(t *testing.T) {
		for label, url := range map[string]string{
			"other token": "https://downloads.claude.ai/claude-code-releases/" + Version +
				"/linux-arm64/claude",
			"other version": "https://downloads.claude.ai/claude-code-releases/9.9.8/linux-x64/claude",
			"lookalike": "https://downloads.claude.ai.evil.example/claude-code-releases/" +
				Version + "/linux-x64/claude",
			"query": "https://downloads.claude.ai/claude-code-releases/" + Version +
				"/linux-x64/claude?x=1",
		} {
			t.Run(label, func(t *testing.T) {
				loadRefusal(t, fixture.mutate(t, "platform-url", func(pin map[string]any) {
					object(t, pin, "platforms", "linux/amd64")["url"] = url
				}), "platform url must be")
			})
		}
	})
}

// TestLoaderPlatformTokensMissingExtraAndFingerprintCase: the two maintained
// architectures are exactly the set, each under its own manifest token, and the pin's
// identity is upper-case hex.
func TestLoaderPlatformTokensMissingExtraAndFingerprintCase(t *testing.T) {
	fixture := testFixture(t)
	t.Run("swapped tokens", func(t *testing.T) {
		loadRefusal(t, fixture.mutate(t, "swapped-tokens", func(pin map[string]any) {
			object(t, pin, "platforms", "linux/amd64")["manifest_platform"] = "linux-arm64"
		}), "manifest_platform must be")
	})
	t.Run("missing platform", func(t *testing.T) {
		loadRefusal(t, fixture.mutate(t, "platform-missing", func(pin map[string]any) {
			delete(object(t, pin, "platforms"), "linux/arm64")
		}), "pin platforms must have exactly")
	})
	t.Run("extra platform", func(t *testing.T) {
		loadRefusal(t, fixture.mutate(t, "platform-extra", func(pin map[string]any) {
			names := object(t, pin, "platforms")
			names["linux/riscv64"] = names["linux/amd64"]
		}), "pin platforms must have exactly")
	})
	t.Run("lower-case fingerprint", func(t *testing.T) {
		loadRefusal(t, fixture.mutate(t, "fingerprint-case", func(pin map[string]any) {
			key := object(t, pin, "key")
			key["fingerprint"] = strings.ToLower(key["fingerprint"].(string))
		}), "upper-case hex")
	})
}

// TestLoaderDigestSpellingAndSizeBounds: a digest is 64 lower-case hex characters and
// a size is a positive integer under the ceiling.
func TestLoaderDigestSpellingAndSizeBounds(t *testing.T) {
	fixture := testFixture(t)
	platform := func(pin map[string]any) map[string]any {
		return object(t, pin, "platforms", "linux/amd64")
	}
	rows := []struct {
		label  string
		mutate func(map[string]any)
		needle string
	}{
		{"upper-case sha", func(pin map[string]any) {
			platform(pin)["sha256"] = strings.ToUpper(platform(pin)["sha256"].(string))
		}, "lower-case hex"},
		{"short sha", func(pin map[string]any) { platform(pin)["sha256"] = "abc" },
			"lower-case hex"},
		{"size zero", func(pin map[string]any) { platform(pin)["size"] = 0 },
			"size must be between"},
		{"size negative", func(pin map[string]any) { platform(pin)["size"] = -1 },
			"size must be between"},
		{"size bool", func(pin map[string]any) { platform(pin)["size"] = true },
			"expected an integer"},
		{"size too large", func(pin map[string]any) { platform(pin)["size"] = 512*1024*1024 + 1 },
			"size must be between"},
	}
	for _, row := range rows {
		t.Run(row.label, func(t *testing.T) {
			loadRefusal(t, fixture.mutate(t, row.label, row.mutate), row.needle)
		})
	}
}

// TestLoaderEvidencePathTraversalAndAbsolutePaths: an evidence path is a plain relative
// path, so a traversal or an absolute path cannot redirect the read.
func TestLoaderEvidencePathTraversalAndAbsolutePaths(t *testing.T) {
	fixture := testFixture(t)
	for _, bad := range []string{"../manifest.json", "/etc/passwd", "evidence/../manifest.json"} {
		t.Run(bad, func(t *testing.T) {
			loadRefusal(t, fixture.mutate(t, "evidence-path", func(pin map[string]any) {
				object(t, pin, "manifest")["path"] = bad
			}), "manifest evidence path must be")
		})
	}
}

// TestLoaderSymlinkedEvidence: the evidence directory itself may not be a symlink, so
// no committed read passes through one.
func TestLoaderSymlinkedEvidence(t *testing.T) {
	fixture := testFixture(t)
	directory := fixture.copy(t, "symlinked-evidence")
	evidence := filepath.Join(directory, "evidence")
	moved := filepath.Join(directory, "evidence-real")
	if err := os.Rename(evidence, moved); err != nil {
		t.Fatalf("cannot move the evidence directory: %v", err)
	}
	if err := os.Symlink("evidence-real", evidence); err != nil {
		t.Fatalf("cannot write the evidence symlink: %v", err)
	}
	checkRefused(t, directory, "symlink")
}
