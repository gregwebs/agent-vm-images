package buildinfo

// The witnesses for the build-info rules. Each test names one rule: deleting the
// rule makes that test fail. The strict-decode cases are the #188 G3 fix and are
// declared divergences from the Python oracle (its reader used json.loads); the
// pins suite carries the oracle's verdict on the same bytes.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
)

// h1Of is a syntactically valid h1 for a label: the same shape the module system
// writes, computed so two labels cannot collide by accident.
func h1Of(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "h1:" + base64.StdEncoding.EncodeToString(sum[:])
}

// document is a coherent build-info document for one target.
func document(t *testing.T, arch gotool.Architecture) map[string]any {
	t.Helper()
	settings := []any{}
	expected := ExpectedSettings(arch)
	for _, key := range sortedSettingKeys(expected) {
		settings = append(settings, map[string]any{"Key": key, "Value": expected[key]})
	}
	return map[string]any{
		"GoVersion": "go1.27.3",
		"Path":      "golang.org/x/tools/gopls",
		"Main": map[string]any{"Path": "golang.org/x/tools/gopls", "Version": "v0.0.1",
			"Sum": h1Of("root")},
		"Deps": []any{map[string]any{"Path": "example.test/dep", "Version": "v1.0.0",
			"Sum": h1Of("dep")}},
		"Settings": settings,
	}
}

// encode renders a document.
func encode(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

// selection is the same identity triple the document reports.
func selection() []Module {
	return []Module{
		{Path: "golang.org/x/tools/gopls", Version: "v0.0.1", H1: closure.H1(h1Of("root"))},
		{Path: "example.test/dep", Version: "v1.0.0", H1: closure.H1(h1Of("dep"))},
	}
}

// TestPositiveControlsParseOnBothTargets is the control the refusal cases need: the
// same document shape these rules refuse is accepted when nothing is wrong, on both
// Linux targets.
func TestPositiveControlsParseOnBothTargets(t *testing.T) {
	for _, arch := range gotool.Architectures() {
		info, err := Parse(encode(t, document(t, arch)))
		if err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if err := Verify(selection(), info, arch, "1.27.3"); err != nil {
			t.Fatalf("%s: %v", arch, err)
		}
		if info.GoVersion != "1.27.3" || info.Main.Version != "v0.0.1" ||
			len(info.Deps) != 1 || len(info.Settings) != len(ExpectedSettings(arch)) {
			t.Fatalf("%s: decoded %+v", arch, info)
		}
	}
}

// TestStrictDocumentRefuses is the G3 fix: the same decoder as the pin, not a
// permissive one. A duplicate key or a non-JSON constant is a refusal here where
// the Python oracle accepted it.
func TestStrictDocumentRefuses(t *testing.T) {
	valid := string(encode(t, document(t, gotool.ARM64)))
	for _, testCase := range []struct {
		name     string
		raw      string
		fragment string
	}{
		{"not JSON", "not a document", "build info: not valid JSON"},
		{"a duplicate key", strings.Replace(valid, `"GoVersion":"go1.27.3"`,
			`"GoVersion":"go1.27.3","GoVersion":"go9.9.9"`, 1), "duplicate key"},
		{"a leading BOM", "\xef\xbb\xbf" + valid, "leading UTF-8 BOM"},
		{"trailing data", valid + "{}", "extra data after the document"},
		{"invalid UTF-8", strings.Replace(valid, `"go1.27.3"`, "\"go1.27.3\xff\"", 1),
			"not valid UTF-8"},
		{"a raw control character", strings.Replace(valid, `"go1.27.3"`,
			"\"go1.27.3\t\"", 1), "invalid control character"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Parse([]byte(testCase.raw))
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
}

// TestShapeRulesRefuse: the document's members and types are a closed set, and each
// refusal names the member it decided.
func TestShapeRulesRefuse(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		mutate   func(document map[string]any)
		fragment string
	}{
		{"a missing GoVersion", func(d map[string]any) { delete(d, "GoVersion") },
			"build info: missing 'GoVersion'"},
		{"a missing Main", func(d map[string]any) { delete(d, "Main") },
			"build info: missing 'Main'"},
		{"a missing Deps", func(d map[string]any) { delete(d, "Deps") },
			"build info: missing 'Deps'"},
		{"a missing Settings", func(d map[string]any) { delete(d, "Settings") },
			"build info: missing 'Settings'"},
		{"an unknown key", func(d map[string]any) { d["Extra"] = "value" },
			"build info: unknown key 'Extra'"},
		{"a GoVersion that is not a string", func(d map[string]any) { d["GoVersion"] = 1.27 },
			"build info GoVersion: expected str"},
		{"a GoVersion that is not exact", func(d map[string]any) { d["GoVersion"] = "go1.27" },
			"build info GoVersion: expected an exact X.Y.Z Go version"},
		{"a Main that is not an object", func(d map[string]any) { d["Main"] = "gopls" },
			"build info Main: expected an object"},
		{"a Main without a Sum", func(d map[string]any) {
			delete(d["Main"].(map[string]any), "Sum")
		}, "build info Main: missing 'Sum'"},
		{"a Main Path that is not a module path", func(d map[string]any) {
			d["Main"].(map[string]any)["Path"] = "not a path"
		}, "build info Main.Path: expected a Go module path"},
		{"a Main Sum that is not an h1", func(d map[string]any) {
			d["Main"].(map[string]any)["Sum"] = "sha256:00"
		}, "build info Main.Sum: expected an h1: checksum"},
		{"Deps that are not a list", func(d map[string]any) { d["Deps"] = map[string]any{} },
			"build info Deps: expected a list"},
		{"Settings that are not a list", func(d map[string]any) { d["Settings"] = "x" },
			"build info Settings: expected a list"},
		{"a setting without a Value", func(d map[string]any) {
			d["Settings"] = []any{map[string]any{"Key": "-buildmode"}}
		}, "build info Settings: missing 'Value'"},
		{"a setting key that is not a string", func(d map[string]any) {
			d["Settings"] = []any{map[string]any{"Key": 1, "Value": "exe"}}
		}, "settings.Key: expected str"},
		{"a setting value that is not a string", func(d map[string]any) {
			d["Settings"] = []any{map[string]any{"Key": "-buildmode", "Value": 1}}
		}, "settings.Value: expected str"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			edited := document(t, gotool.ARM64)
			testCase.mutate(edited)
			_, err := Parse(encode(t, edited))
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
}

// TestDuplicateRowsRefuse: a path repeated among the dependencies or a setting
// repeated by name is refused before either side is indexed, because a path-keyed
// map would otherwise collapse the two rows into one.
func TestDuplicateRowsRefuse(t *testing.T) {
	duplicateDep := document(t, gotool.ARM64)
	duplicateDep["Deps"] = append(duplicateDep["Deps"].([]any),
		duplicateDep["Deps"].([]any)[0])
	if _, err := Parse(encode(t, duplicateDep)); err == nil ||
		!strings.Contains(err.Error(), "build info: duplicate dependency") {
		t.Fatalf("a duplicate dependency was accepted: %v", err)
	}
	duplicateSetting := document(t, gotool.ARM64)
	settings := duplicateSetting["Settings"].([]any)
	duplicateSetting["Settings"] = append(settings, settings[0])
	if _, err := Parse(encode(t, duplicateSetting)); err == nil ||
		!strings.Contains(err.Error(), "build info: duplicate setting -buildmode") {
		t.Fatalf("a duplicate setting was accepted: %v", err)
	}
}

// TestModuleReplacementRefuses: a replaced module is a selection this closure never
// reviewed, whatever the replacement names.
func TestModuleReplacementRefuses(t *testing.T) {
	for name, edit := range map[string]func(document map[string]any){
		"main": func(d map[string]any) {
			d["Main"].(map[string]any)["Replace"] = map[string]any{
				"Path": "example.test/evil", "Version": "v9.9.9", "Sum": h1Of("evil")}
		},
		"dependency": func(d map[string]any) {
			d["Deps"].([]any)[0].(map[string]any)["Replace"] = map[string]any{
				"Path": "example.test/evil", "Version": "v9.9.9", "Sum": h1Of("evil")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			edited := document(t, gotool.ARM64)
			edit(edited)
			_, err := Parse(encode(t, edited))
			if err == nil || !strings.Contains(err.Error(),
				"module replacement is not part of the reviewed closure") {
				t.Fatalf("a replacement was accepted: %v", err)
			}
		})
	}
}

// TestVerifyRefusesEachIdentityRule is the verdict-level witness for the comparison:
// compiler, root, extra, missing, version, content hash.
func TestVerifyRefusesEachIdentityRule(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		mutate   func(document map[string]any)
		arch     gotool.Architecture
		compiler closure.GoVersion
		fragment string
	}{
		{"a different compiler",
			func(d map[string]any) { d["GoVersion"] = "go1.26.5" }, gotool.ARM64, "1.27.3",
			"compiler is go1.26.5, want go1.27.3"},
		{"the pin's compiler, a different pin",
			func(d map[string]any) {}, gotool.ARM64, "1.27.4",
			"compiler is go1.27.3, want go1.27.4"},
		{"a different root content hash", func(d map[string]any) {
			d["Main"].(map[string]any)["Sum"] = h1Of("other")
		}, gotool.ARM64, "1.27.3", "not the reviewed root"},
		{"a different root version", func(d map[string]any) {
			d["Main"].(map[string]any)["Version"] = "v0.0.2"
		}, gotool.ARM64, "1.27.3", "not the reviewed root"},
		{"an unreviewed extra dependency", func(d map[string]any) {
			d["Deps"] = append(d["Deps"].([]any), map[string]any{
				"Path": "example.test/extra", "Version": "v1.0.0", "Sum": h1Of("extra")})
		}, gotool.ARM64, "1.27.3", "build used example.test/extra@v1.0.0, not in the " +
			"reviewed closure"},
		{"a missing dependency", func(d map[string]any) { d["Deps"] = []any{} },
			gotool.ARM64, "1.27.3", "build omitted example.test/dep from the reviewed closure"},
		{"a different selected version", func(d map[string]any) {
			d["Deps"].([]any)[0].(map[string]any)["Version"] = "v1.1.0"
		}, gotool.ARM64, "1.27.3", "build used example.test/dep@v1.1.0, not in the " +
			"reviewed closure"},
		{"a different module content hash", func(d map[string]any) {
			d["Deps"].([]any)[0].(map[string]any)["Sum"] = h1Of("different")
		}, gotool.ARM64, "1.27.3", "not the reviewed content"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			edited := document(t, gotool.ARM64)
			testCase.mutate(edited)
			info, err := Parse(encode(t, edited))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			err = Verify(selection(), info, testCase.arch, testCase.compiler)
			if err == nil || !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
}

// TestSettingsAreTheReviewedTarget: the settings map is compared whole, so an
// unknown key, a changed value, another operating system and another feature level
// are each a refusal — the compatibility tripwire, not a difference to accept.
func TestSettingsAreTheReviewedTarget(t *testing.T) {
	edit := func(document map[string]any, key, value string) {
		for _, item := range document["Settings"].([]any) {
			entry := item.(map[string]any)
			if entry["Key"] == key {
				entry["Value"] = value
				return
			}
		}
		document["Settings"] = append(document["Settings"].([]any),
			map[string]any{"Key": key, "Value": value})
	}
	for _, testCase := range []struct {
		name     string
		mutate   func(document map[string]any)
		fragment string
	}{
		{"an unknown setting", func(d map[string]any) {
			edit(d, "GOEXPERIMENT", "none")
		}, "build settings differ"},
		{"a changed DefaultGODEBUG", func(d map[string]any) {
			edit(d, "DefaultGODEBUG", "tracebacklabels=1")
		}, "build settings differ"},
		{"a Darwin build", func(d map[string]any) { edit(d, "GOOS", "darwin") },
			"build settings differ"},
		{"a different feature level", func(d map[string]any) {
			edit(d, "GOARM64", "v9.0")
		}, "build settings differ"},
		{"a missing setting", func(d map[string]any) {
			kept := []any{}
			for _, item := range d["Settings"].([]any) {
				if item.(map[string]any)["Key"] != "-trimpath" {
					kept = append(kept, item)
				}
			}
			d["Settings"] = kept
		}, "build settings differ"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			edited := document(t, gotool.ARM64)
			testCase.mutate(edited)
			info, err := Parse(encode(t, edited))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if err := Verify(selection(), info, gotool.ARM64, "1.27.3"); err == nil ||
				!strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("want a refusal naming %q, got %v", testCase.fragment, err)
			}
		})
	}
	// The two targets differ by exactly their feature level, so a document built for
	// one is refused for the other. Without this pair the settings rule could accept
	// any amd64 build as an arm64 one.
	arm := document(t, gotool.ARM64)
	info, err := Parse(encode(t, arm))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := Verify(selection(), info, gotool.AMD64, "1.27.3"); err == nil ||
		!strings.Contains(err.Error(), "build settings differ") {
		t.Fatalf("an arm64 build was accepted as amd64: %v", err)
	}
}

// TestSelectionMustNameTheRoot: the comparison needs the reviewed root row, and a
// selection without it is refused rather than compared against the zero value.
func TestSelectionMustNameTheRoot(t *testing.T) {
	info, err := Parse(encode(t, document(t, gotool.ARM64)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	err = Verify([]Module{{Path: "example.test/dep", Version: "v1.0.0",
		H1: closure.H1(h1Of("dep"))}}, info, gotool.ARM64, "1.27.3")
	if err == nil || !strings.Contains(err.Error(), "does not name the root module") {
		t.Fatalf("a rootless selection was accepted: %v", err)
	}
}
