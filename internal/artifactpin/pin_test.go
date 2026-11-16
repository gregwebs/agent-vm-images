package artifactpin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repositoryRoot is the module root, three levels above this package.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func committedPin(t *testing.T, relative string) Pin {
	t.Helper()
	pin, err := Load(filepath.Join(repositoryRoot(t), relative))
	if err != nil {
		t.Fatalf("loading %s: %v", relative, err)
	}
	return pin
}

// The three committed pins load, which is what the installer, the writer and the
// gate all rely on.
func TestCommittedPinsLoad(t *testing.T) {
	for _, relative := range []string{
		"images/artifacts/node.json",
		"images/artifacts/gh.json",
		"images/tools/agy/pin.json",
	} {
		pin := committedPin(t, relative)
		if string(pin.Name) == "" || pin.Version.String() == "" {
			t.Errorf("%s: loaded an empty pin", relative)
		}
		if pin.Reason() == "" {
			t.Errorf("%s: records no reason", relative)
		}
		for _, platform := range Platforms {
			archive, err := pin.Archive(platform)
			if err != nil {
				t.Fatalf("%s: %v", relative, err)
			}
			if archive.URL == "" || archive.Digest == "" {
				t.Errorf("%s: %s archive is incomplete", relative, platform)
			}
		}
	}
}

func TestCommittedPinsNameTheirArtifact(t *testing.T) {
	node := committedPin(t, "images/artifacts/node.json")
	if node.Name != Node {
		t.Fatalf("node pin names %s", node.Name)
	}
	if got := node.Bundled["npm"].String(); got == "" {
		t.Fatal("the node pin records no bundled npm")
	}
	if node.Authentication.Method != MethodOpenPGP || node.Authentication.OpenPGP == nil {
		t.Fatalf("the node pin authenticates with %s", node.Authentication.Method)
	}
	if node.Authentication.OpenPGP.KeyFingerprint != NodeKeyFingerprint {
		t.Errorf("key fingerprint is %s", node.Authentication.OpenPGP.KeyFingerprint)
	}
	gh := committedPin(t, "images/artifacts/gh.json")
	if gh.Authentication.Method != MethodSigstore || gh.Authentication.Sigstore == nil {
		t.Fatalf("the gh pin authenticates with %s", gh.Authentication.Method)
	}
	if gh.Authentication.Sigstore.Policy != GHPolicy {
		t.Error("the gh pin records a policy other than the reviewed one")
	}
	for _, platform := range Platforms {
		archive, err := gh.Archive(platform)
		if err != nil {
			t.Fatal(err)
		}
		if archive.Bundle == nil {
			t.Fatalf("%s: the gh archive carries no bundle", platform)
		}
	}
	agy := committedPin(t, "images/tools/agy/pin.json")
	if agy.Authentication.Method != MethodHashOnly || agy.Authentication.HashOnly == nil {
		t.Fatalf("the agy pin authenticates with %s", agy.Authentication.Method)
	}
	if err := CheckCorroboration(agy); err != nil {
		t.Errorf("the committed agy corroboration does not check: %v", err)
	}
}

// A pin is refused for each shape the suites name, with the fragment they match.
func TestRefusals(t *testing.T) {
	raw := readCommitted(t, "images/artifacts/node.json")
	cases := []struct {
		label  string
		tamper func(pin map[string]any)
		needle string
	}{
		{
			label:  "unknown field",
			tamper: func(pin map[string]any) { pin["latest"] = true },
			needle: "unknown latest",
		},
		{
			label: "a release that is not the version's tag",
			tamper: func(pin map[string]any) {
				pin["upstream"].(map[string]any)["release"] = "v1.0.0"
			},
			needle: "upstream release",
		},
		{
			label:  "deleted authentication",
			tamper: func(pin map[string]any) { delete(pin, "authentication") },
			needle: "missing authentication",
		},
		{
			label: "downgraded authentication",
			tamper: func(pin map[string]any) {
				pin["authentication"].(map[string]any)["method"] = "hash-only"
			},
			needle: "openpgp-signed-manifest",
		},
		{
			label: "upper-case digest",
			tamper: func(pin map[string]any) {
				platform := pin["platforms"].(map[string]any)["linux/amd64"].(map[string]any)
				platform["sha256"] = strings.ToUpper(platform["sha256"].(string))
			},
			needle: "lower-case SHA-256",
		},
		{
			label:  "a latest version",
			tamper: func(pin map[string]any) { pin["version"] = "latest" },
			needle: "exact X.Y.Z",
		},
		{
			label: "a URL that does not name the pinned asset",
			tamper: func(pin map[string]any) {
				platform := pin["platforms"].(map[string]any)["linux/amd64"].(map[string]any)
				platform["url"] = "file:///tmp/fixture/node-v9.9.8-linux-x64.tar.xz"
			},
			needle: "must name",
		},
		{
			label: "an upstream URL that is not the computed one",
			tamper: func(pin map[string]any) {
				platform := pin["platforms"].(map[string]any)["linux/amd64"].(map[string]any)
				platform["url"] = "https://nodejs.org/dist/v9.9.8/node-v22.23.4-linux-x64.tar.xz"
			},
			needle: "the url must be exactly",
		},
		{
			label: "a payload escaping the destination",
			tamper: func(pin map[string]any) {
				pin["install"].(map[string]any)["payload"] = []any{"../escape"}
			},
			needle: "payload",
		},
		{
			label: "a missing platform",
			tamper: func(pin map[string]any) {
				delete(pin["platforms"].(map[string]any), "linux/arm64")
			},
			needle: "linux/arm64",
		},
		{
			label:  "a duplicate key",
			tamper: func(map[string]any) {},
			needle: "duplicate JSON key",
		},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			document := decode(t, raw)
			test.tamper(document)
			encoded := encode(t, document)
			if test.label == "a duplicate key" {
				encoded = strings.Replace(encoded, `"kind":"release-archive",`,
					`"kind":"release-archive","kind":"release-archive",`, 1)
			}
			_, err := Parse([]byte(encoded), t.TempDir(), "node")
			if err == nil {
				t.Fatal("accepted the tampered pin")
			}
			if !IsRefusal(err) {
				t.Fatalf("reported %T instead of a refusal: %v", err, err)
			}
			if !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("refusal does not mention %q: %v", test.needle, err)
			}
		})
	}
}

// A float is refused rather than rounded, and the whole document is read: a
// number is never a value a reviewed field carries.
func TestFloatIsRefused(t *testing.T) {
	raw := strings.Replace(readCommitted(t, "images/artifacts/node.json"),
		`"schema": 1,`, `"schema": 1.0,`, 1)
	_, err := Parse([]byte(raw), t.TempDir(), "node")
	if err == nil || !strings.Contains(err.Error(), "is not a JSON number this build acts on") {
		t.Fatalf("a float schema reported %v", err)
	}
}

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	return document
}

func encode(t *testing.T, document map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func readCommitted(t *testing.T, relative string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), relative))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A pin's location names the artifact: an unknown directory is a refusal rather
// than a fall-through to the GitHub CLI defaults.
func TestUnknownArtifact(t *testing.T) {
	_, err := Parse([]byte(`{}`), t.TempDir(), "minimal")
	if err == nil || !strings.Contains(err.Error(), "unknown artifact") {
		t.Fatalf("an unknown artifact name reported %v", err)
	}
	if _, err := Load(filepath.Join(repositoryRoot(t), "images/artifacts/node.txt")); err == nil ||
		!strings.Contains(err.Error(), "a pin is a .json file") {
		t.Fatalf("a non-json pin reported %v", err)
	}
}

// A committed evidence file that no longer hashes to the pin is a refusal before
// any fetch, and so is a record that describes another release.
func TestCorroborationRefusals(t *testing.T) {
	directory := t.TempDir()
	committed := filepath.Join(repositoryRoot(t), "images/tools/agy")
	for _, relative := range []string{"pin.json", "evidence/release.json",
		"evidence/manifest-linux-amd64.json", "evidence/manifest-linux-arm64.json"} {
		raw, err := os.ReadFile(filepath.Join(committed, relative))
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(directory, relative), string(raw))
	}
	pin := committedPin(t, "images/tools/agy/pin.json")
	pin.Directory = directory
	if err := CheckCorroboration(pin); err != nil {
		t.Fatalf("the copied corroboration does not check: %v", err)
	}
	write(t, filepath.Join(directory, "evidence/manifest-linux-amd64.json"),
		`{"version": "1.4.11", "url": "https://example.invalid/a.tar.gz", "sha512": "`+
			strings.Repeat("0", 128)+`"}`)
	err := CheckCorroboration(pin)
	if err == nil || !strings.Contains(err.Error(), "evidence/manifest-linux-amd64.json") {
		t.Fatalf("a hand-edited manifest reported %v", err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
