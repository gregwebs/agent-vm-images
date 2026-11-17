package claudepin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParsePrimaries holds the key-file grammar: one primary, never a subkey and
// never a key file that also carries secret material.
func TestParsePrimaries(t *testing.T) {
	primary := "31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE"
	subkey := "422E8241E320573700499D74E010800BADA0909D"
	cases := []struct {
		label  string
		colons string
		want   []string
		needle string
	}{
		{label: "one primary, one subkey",
			colons: "pub:u:255:22:AABB:1700000000::\n" +
				"fpr:::::::::" + primary + ":\n" +
				"uid:u::::::hash::Anthropic\n" +
				"sub:u:255:22:CCDD:1700000000::::\n" +
				"fpr:::::::::" + subkey + ":\n",
			want: []string{primary}},
		{label: "no records", colons: "", want: []string{}},
		{label: "two primaries",
			colons: "pub:u:255:22:AABB::::\nfpr:::::::::" + primary + ":\n" +
				"pub:u:255:22:CCDD::::\nfpr:::::::::" + subkey + ":\n",
			want: []string{primary, subkey}},
		{label: "secret key material",
			colons: "sec:u:255:22:AABB::::\nfpr:::::::::" + primary + ":\n",
			needle: "secret key material"},
		{label: "pub without fpr",
			colons: "pub:u:255:22:AABB::::\nuid:u::::::hash::x\n",
			needle: "pub record without an fpr"},
		{label: "short fpr record",
			colons: "pub:u:255:22:AABB::::\nfpr::::abc\n",
			needle: "unreadable fpr record"},
		{label: "unusable fingerprint",
			colons: "pub:u:255:22:AABB::::\nfpr:::::::::not-a-fingerprint:\n",
			needle: "unusable fingerprint"},
	}
	for _, test := range cases {
		primaries, err := ParsePrimaries(test.colons)
		if test.needle != "" {
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %s", test.label, err)
			continue
		}
		if strings.Join(primaries, " ") != strings.Join(test.want, " ") {
			t.Errorf("%s: got %v, want %v", test.label, primaries, test.want)
		}
	}
}

// writeClosure copies the committed closure into a scratch directory so a test can
// mutate one file without touching the review record.
func writeClosure(t *testing.T) string {
	t.Helper()
	source := filepath.Join("..", "..", "images", "tools", "claude")
	destination := filepath.Join(t.TempDir(), "claude")
	if err := os.MkdirAll(filepath.Join(destination, "evidence"), 0o700); err != nil {
		t.Fatalf("scratch: %s", err)
	}
	for _, relative := range []string{"pin.json", KeyPath, ManifestPath, SignaturePath} {
		raw, err := os.ReadFile(filepath.Join(source, relative))
		if err != nil {
			t.Fatalf("%s: %s", relative, err)
		}
		if err := os.WriteFile(filepath.Join(destination, relative), raw, 0o600); err != nil {
			t.Fatalf("%s: %s", relative, err)
		}
	}
	return destination
}

func mutatePin(t *testing.T, directory string, edit func(string) string) {
	t.Helper()
	path := filepath.Join(directory, "pin.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the pin: %s", err)
	}
	if err := os.WriteFile(path, []byte(edit(string(raw))), 0o600); err != nil {
		t.Fatalf("write the pin: %s", err)
	}
}

// TestLoadRefusesWhatTheReviewDidNotAccept: the loader is the only place a pin's
// shape is decided, so each of these is a refusal rather than a default.
func TestLoadRefusesWhatTheReviewDidNotAccept(t *testing.T) {
	cases := []struct {
		label  string
		edit   func(string) string
		needle string
	}{
		{label: "duplicate key", needle: "duplicate JSON key",
			edit: func(pin string) string {
				needle := `"version": "2.1.319"`
				return strings.Replace(pin, needle, needle+", "+needle, 1)
			}},
		{label: "fractional size", needle: "fractional part",
			edit: func(pin string) string {
				return strings.Replace(pin, `"size": 340226576`, `"size": 1.5`, 1)
			}},
		{label: "NaN size", needle: "is not a JSON number",
			edit: func(pin string) string {
				return strings.Replace(pin, `"size": 340226576`, `"size": NaN`, 1)
			}},
		{label: "unexpected member", needle: "must have exactly",
			edit: func(pin string) string {
				return strings.Replace(pin, `"schema": 1,`, `"schema": 1, "note": 1,`, 1)
			}},
		{label: "misspelled origin", needle: "url must be",
			edit: func(pin string) string {
				return strings.Replace(pin,
					"https://downloads.claude.ai/claude-code-releases/2.1.319/linux-x64/claude",
					"https://downloads.claude.ai.example/claude-code-releases/2.1.319/linux-x64/claude", 1)
			}},
		{label: "another signer", needle: "must hold exactly the primary",
			edit: func(pin string) string {
				return strings.Replace(pin, ReviewedSignerFingerprint,
					"422E8241E320573700499D74E010800BADA0909D", 1)
			}},
		{label: "year zero", needle: "not a calendar date",
			edit: func(pin string) string {
				return strings.Replace(pin, `"verified_on": "2026-11-13"`,
					`"verified_on": "0000-01-01"`, 1)
			}},
		{label: "leading-zero schema", needle: "is not a JSON number",
			edit: func(pin string) string {
				return strings.Replace(pin, `"schema": 1,`, `"schema": 01,`, 1)
			}},
		{label: "future verified_on", needle: "is in the future",
			edit: func(pin string) string {
				return strings.Replace(pin, `"verified_on": "2026-11-13"`,
					`"verified_on": "2999-01-01"`, 1)
			}},
	}
	for _, test := range cases {
		directory := writeClosure(t)
		mutatePin(t, directory, test.edit)
		pin, err := Load(directory)
		if err != nil {
			if !strings.Contains(err.Error(), test.needle) {
				t.Errorf("%s: refused with %q, want %q", test.label, err, test.needle)
			}
			continue
		}
		if _, err := pin.Verify(pin.Version); err == nil ||
			!strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: verify did not refuse with %q: %v", test.label, test.needle, err)
		}
	}
}

// TestVerifyRefusesAnUnreviewedVersion: an exact version the pin does not select is
// a refusal, not a resolution.
func TestVerifyRefusesAnUnreviewedVersion(t *testing.T) {
	pin, err := Load(writeClosure(t))
	if err != nil {
		t.Fatalf("load: %s", err)
	}
	if _, err := pin.Verify("9.9.9"); err == nil ||
		!strings.Contains(err.Error(), "is not the pinned Claude Code version") {
		t.Errorf("an unpinned version must be refused, got %v", err)
	}
}

// TestLoadFollowsNoSymlink: a closure read through a symlink is a redirection, not a
// pinned input.
func TestLoadFollowsNoSymlink(t *testing.T) {
	directory := writeClosure(t)
	elsewhere := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(elsewhere, []byte("{}"), 0o600); err != nil {
		t.Fatalf("scratch: %s", err)
	}
	pinPath := filepath.Join(directory, "pin.json")
	if err := os.Remove(pinPath); err != nil {
		t.Fatalf("remove: %s", err)
	}
	if err := os.Symlink(elsewhere, pinPath); err != nil {
		t.Fatalf("symlink: %s", err)
	}
	if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a symlinked pin.json must be refused, got %v", err)
	}
}

// TestEvidenceDigestIsRehashedNow: the pin's digest is the authority, so a rewritten
// evidence file is refused before the signature is even asked about.
func TestEvidenceDigestIsRehashedNow(t *testing.T) {
	directory := writeClosure(t)
	pin, err := Load(directory)
	if err != nil {
		t.Fatalf("load: %s", err)
	}
	if err := os.WriteFile(filepath.Join(directory, ManifestPath), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("rewrite: %s", err)
	}
	if _, err := pin.Verify(pin.Version); err == nil ||
		!strings.Contains(err.Error(), "does not match the pin digest") {
		t.Errorf("a rewritten manifest must be refused by its digest, got %v", err)
	}
}

// TestCheckELFRefusesTheWrongObject: the digest proves which bytes these are, not
// that they can run here.
func TestCheckELFRefusesTheWrongObject(t *testing.T) {
	header := func(class, data byte, machine uint16) []byte {
		raw := make([]byte, ELFHeaderBytes)
		copy(raw, "\x7fELF")
		raw[4] = class
		raw[5] = data
		raw[18] = byte(machine)
		raw[19] = byte(machine >> 8)
		return raw
	}
	scratch := t.TempDir()
	cases := []struct {
		label  string
		raw    []byte
		token  string
		needle string
	}{
		{label: "x64", raw: header(2, 1, 62), token: "linux-x64"},
		{label: "arm64", raw: header(2, 1, 183), token: "linux-arm64"},
		{label: "script", raw: []byte("#!/bin/sh\nexit 0\n# padding padding\n"),
			token: "linux-x64", needle: "not an ELF object"},
		{label: "truncated", raw: []byte("\x7fELF"), token: "linux-x64",
			needle: "shorter than an ELF header"},
		{label: "32-bit", raw: header(1, 1, 62), token: "linux-x64",
			needle: "not a 64-bit ELF object"},
		{label: "big-endian", raw: header(2, 2, 62), token: "linux-x64",
			needle: "not a little-endian ELF object"},
		{label: "wrong machine", raw: header(2, 1, 183), token: "linux-x64",
			needle: "targets ELF machine 183"},
		{label: "unmaintained token", raw: header(2, 1, 62), token: "linux-x64-musl",
			needle: "not a maintained Claude Code platform"},
	}
	for _, test := range cases {
		path := filepath.Join(scratch, strings.ReplaceAll(test.label, " ", "-"))
		if err := os.WriteFile(path, test.raw, 0o600); err != nil {
			t.Fatalf("%s: %s", test.label, err)
		}
		err := CheckELF(path, test.token)
		if test.needle == "" {
			if err != nil {
				t.Errorf("%s: %s", test.label, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
		}
	}
}

// TestNativeStateRefusesUpdates: the image's contract is an offline native install
// with updates off, so every other state is a refusal.
func TestNativeStateRefusesUpdates(t *testing.T) {
	cases := []struct {
		label  string
		body   string
		files  []string
		needle string
	}{
		{label: "good",
			body: `{"installMethod": "native", "autoUpdates": false}`},
		{label: "updates on",
			body:   `{"installMethod": "native", "autoUpdates": true}`,
			needle: "autoUpdates"},
		{label: "updates as a string",
			body:   `{"installMethod": "native", "autoUpdates": "false"}`,
			needle: "autoUpdates"},
		{label: "another install method",
			body:   `{"installMethod": "npm", "autoUpdates": false}`,
			needle: "installMethod"},
		{label: "duplicate key",
			body:   `{"installMethod": "native", "installMethod": "npm", "autoUpdates": false}`,
			needle: "duplicate JSON key"},
		{label: "not an object",
			body:   `[]`,
			needle: "not a JSON object"},
		{label: "leftover downloads",
			body:   `{"installMethod": "native", "autoUpdates": false}`,
			files:  []string{".claude/downloads/partial"},
			needle: "is not empty"},
	}
	for _, test := range cases {
		prefix := t.TempDir()
		if err := os.WriteFile(filepath.Join(prefix, ".claude.json"), []byte(test.body+"\n"),
			0o600); err != nil {
			t.Fatalf("%s: %s", test.label, err)
		}
		for _, relative := range test.files {
			path := filepath.Join(prefix, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("%s: %s", test.label, err)
			}
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatalf("%s: %s", test.label, err)
			}
		}
		err := CheckNativeState(prefix)
		if test.needle == "" {
			if err != nil {
				t.Errorf("%s: %s", test.label, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
		}
	}
}

// TestGPGHomeIsPrivateAndBounded: a keyring must be a fresh 0700 directory under the
// fixed short root -- never TMPDIR, whose depth can exceed gpg-agent's socket path --
// and an unusable root is a refusal rather than a fallback.
func TestGPGHomeIsPrivateAndBounded(t *testing.T) {
	home, err := NewGPGHome(nil)
	if err != nil {
		t.Fatalf("a private home: %s", err)
	}
	if !strings.HasPrefix(home.Path, GPGHomeRoot+"/"+GPGHomePrefix) {
		t.Errorf("the home is %s, not under %s/%s", home.Path, GPGHomeRoot, GPGHomePrefix)
	}
	if len(home.Path) > MaxGPGHomeChars {
		t.Errorf("the home %s is longer than %d characters", home.Path, MaxGPGHomeChars)
	}
	info, err := os.Lstat(home.Path)
	if err != nil {
		t.Fatalf("%s: %s", home.Path, err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("the home has mode %04o, not 0700", mode)
	}
	for _, entry := range home.Environment {
		if strings.HasPrefix(entry, "GNUPGHOME="+home.Path) {
			return
		}
	}
	t.Errorf("the environment does not point gpg at the private home: %v", home.Environment)
}

func TestGPGHomeRefusesAnUnusableRoot(t *testing.T) {
	original := GPGHomeRoot
	t.Cleanup(func() { GPGHomeRoot = original })
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("scratch: %s", err)
	}
	for _, root := range []string{file, filepath.Join(file, "child")} {
		GPGHomeRoot = root
		if _, err := NewGPGHome(nil); err == nil ||
			!strings.Contains(err.Error(), "cannot create a private gpg home") {
			t.Errorf("an unusable root %s gave %v", root, err)
		}
	}
}

// TestInstalledPayloadModeIsExact: a setuid or setgid payload runs with privileges
// the review never granted, so the whole mode word is compared, not the nine rwx bits.
func TestInstalledPayloadModeIsExact(t *testing.T) {
	pin, err := Load(writeClosure(t))
	if err != nil {
		t.Fatalf("load: %s", err)
	}
	verified, err := pin.Verify(pin.Version)
	if err != nil {
		t.Fatalf("verify: %s", err)
	}
	payload, err := os.ReadFile(filepath.Join("..", "..", "images", "tools", "claude", "pin.json"))
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	if len(payload) == 0 {
		t.Fatal("the committed pin is empty")
	}
	for _, test := range []struct {
		label  string
		mode   os.FileMode
		needle string
	}{
		{label: "0755", mode: 0o755},
		// A setuid bit is the case `Perm()` would hide. (A setgid bit is not: BSD
		// clears it on a regular file owned by the caller's group, so a case for it
		// would pass for the wrong reason on macOS.)
		{label: "04755", mode: 0o4755 | os.ModeSetuid, needle: "not 0755"},
		{label: "0644", mode: 0o644, needle: "not 0755"},
		{label: "0775", mode: 0o775, needle: "not 0755"},
	} {
		prefix := t.TempDir()
		versions := filepath.Join(prefix, ".local", "share", "claude", "versions")
		installed := filepath.Join(versions, pin.Version)
		for _, directory := range []string{filepath.Join(prefix, ".local", "bin"), versions} {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatalf("%s: %s", test.label, err)
			}
		}
		// The payload's digest never matches the committed pin here; the mode is what
		// this case is about, so only the mode refusal is asserted.
		if err := os.WriteFile(installed, []byte("payload"), 0o600); err != nil {
			t.Fatalf("%s: %s", test.label, err)
		}
		if err := os.Chmod(installed, test.mode); err != nil {
			t.Fatalf("%s: %s", test.label, err)
		}
		if err := os.Symlink(installed, filepath.Join(prefix, ".local", "bin", "claude")); err != nil {
			t.Fatalf("%s: %s", test.label, err)
		}
		owner, _ := ownerOf(mustStat(t, installed))
		err := verified.CheckInstalled("linux-x64", prefix, owner[0], owner[1])
		if test.needle == "" {
			if err != nil && strings.Contains(err.Error(), "not 0755") {
				t.Errorf("%s: the exact mode was refused: %s", test.label, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
		}
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %s", path, err)
	}
	return info
}
