package pnpm

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reviewedPin is a valid pin document the cases mutate one field at a time. The
// digests are real: the SRI is derived from the hex digest exactly as the
// committed pin derives it.
func reviewedPin() map[string]any {
	sum := sha512.Sum512([]byte("reviewed archive bytes"))
	digest := hex.EncodeToString(sum[:])
	return map[string]any{
		"version":        "11.11.0",
		"url":            "https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz",
		"integrity":      "sha512-" + base64.StdEncoding.EncodeToString(sum[:]),
		"sha512":         digest,
		"bundleSha256":   strings.Repeat("a", 64),
		"upstreamTag":    "v11.11.0",
		"upstreamCommit": strings.Repeat("b", 40),
	}
}

// writePin writes one document into the test's temporary directory.
func writePin(t *testing.T, document any) string {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("cannot marshal the fixture pin: %v", err)
	}
	path := filepath.Join(t.TempDir(), "pin.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("cannot write the fixture pin: %v", err)
	}
	return path
}

func TestLoadPin(t *testing.T) {
	cases := []struct {
		name   string
		change func(map[string]any)
		raw    string
		want   string
	}{
		{name: "valid"},
		{name: "field set is closed", change: func(pin map[string]any) { pin["distTag"] = "latest" }, want: "invalid pin fields"},
		{name: "field set is complete", change: func(pin map[string]any) { delete(pin, "upstreamCommit") }, want: "invalid pin fields"},
		{
			name:   "field types are strings",
			change: func(pin map[string]any) { pin["version"] = 11.11 },
			want:   "pin fields must be strings",
		},
		{name: "not an object", raw: `[]`, want: "invalid pin fields"},
		{
			name:   "version is exact",
			change: func(pin map[string]any) { pin["version"] = "^11.11.0" },
			want:   "pin must be exact stable version",
		},
		{
			name:   "version has no leading zero",
			change: func(pin map[string]any) { pin["version"] = "011.11.0" },
			want:   "pin must be exact stable version",
		},
		{
			name:   "version is not a dist tag",
			change: func(pin map[string]any) { pin["version"] = "latest" },
			want:   "pin must be exact stable version",
		},
		{
			name:   "url is the registry archive",
			change: func(pin map[string]any) { pin["url"] = "https://example.com/pnpm.tgz" },
			want:   "unexpected archive URL",
		},
		{
			name:   "sha512 width",
			change: func(pin map[string]any) { pin["sha512"] = strings.Repeat("a", 127) },
			want:   "invalid digest/commit: sha512",
		},
		{
			name:   "sha512 is lowercase hex",
			change: func(pin map[string]any) { pin["sha512"] = strings.Repeat("A", 128) },
			want:   "invalid digest/commit: sha512",
		},
		{
			name:   "bundle digest width",
			change: func(pin map[string]any) { pin["bundleSha256"] = strings.Repeat("a", 63) },
			want:   "invalid digest/commit: bundleSha256",
		},
		{
			name:   "commit width",
			change: func(pin map[string]any) { pin["upstreamCommit"] = strings.Repeat("b", 39) },
			want:   "invalid digest/commit: upstreamCommit",
		},
		{
			name:   "sri agrees with the hex digest",
			change: func(pin map[string]any) { pin["integrity"] = "sha512-bad" },
			want:   "SRI and hex digest disagree",
		},
		{
			name:   "tag names the version",
			change: func(pin map[string]any) { pin["upstreamTag"] = "v11.10.0" },
			want:   "upstream tag disagrees",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var path string
			if testCase.raw != "" {
				path = filepath.Join(t.TempDir(), "pin.json")
				if err := os.WriteFile(path, []byte(testCase.raw), 0o644); err != nil {
					t.Fatalf("cannot write the fixture pin: %v", err)
				}
			} else {
				document := reviewedPin()
				if testCase.change != nil {
					testCase.change(document)
				}
				path = writePin(t, document)
			}
			pin, err := LoadPin(path)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("LoadPin refused a reviewed pin: %v", err)
				}
				if pin.Version != "11.11.0" || pin.UpstreamTag != "v11.11.0" {
					t.Fatalf("LoadPin returned %+v", pin)
				}
				return
			}
			if err == nil {
				t.Fatalf("LoadPin accepted a pin it must refuse: want %q", testCase.want)
			}
			if err.Error() != testCase.want {
				t.Fatalf("refusal is %q, want %q", err.Error(), testCase.want)
			}
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("refusal %v is not a *Refusal", err)
			}
		})
	}
}

func TestLoadPinMissingFile(t *testing.T) {
	_, err := LoadPin(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("LoadPin accepted a missing pin file")
	}
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a missing pin file is %T, want *Refusal", err)
	}
}

// base64Of is the standard base64 the SRI prefix carries.
func base64Of(hexDigest string) string {
	raw, err := hex.DecodeString(hexDigest)
	if err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestPinVersion(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		version string
		want    string
	}{
		{name: "a reviewed pin", raw: `{"version":"11.11.0"}`, version: "11.11.0"},
		{name: "a confined base-pin record", raw: `{"version":"11.7.0"}`, version: "11.7.0"},
		{name: "a dist tag is still a value", raw: `{"version":"latest"}`, version: "latest"},
		{name: "no version member", raw: `{"url":"x"}`, want: "invalid pin fields"},
		{name: "a non-string version", raw: `{"version":11}`, want: "invalid pin fields"},
		{name: "not an object", raw: `[]`, want: "cannot read "},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pin.json")
			if err := os.WriteFile(path, []byte(testCase.raw), 0o644); err != nil {
				t.Fatalf("cannot write the fixture pin: %v", err)
			}
			version, err := PinVersion(path)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("PinVersion refused a readable pin: %v", err)
				}
				if version != testCase.version {
					t.Fatalf("version is %q, want %q", version, testCase.version)
				}
				return
			}
			if err == nil {
				t.Fatalf("PinVersion accepted a pin it must refuse: want %q", testCase.want)
			}
			if !strings.HasPrefix(err.Error(), testCase.want) {
				t.Fatalf("refusal is %q, want it to start with %q", err.Error(), testCase.want)
			}
		})
	}
}
