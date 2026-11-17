package codexpin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const committedDirectory = "../../images/tools/codex"

// committedPin is the reviewed closure, loaded once for the cases that mutate it.
func committedPin(t *testing.T) Pin {
	t.Helper()
	pin, err := Load(committedDirectory)
	if err != nil {
		t.Fatalf("load the committed pin: %s", err)
	}
	return pin
}

// copyClosure writes the committed closure into a fresh directory.
func copyClosure(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	files := []string{"pin.json", MetadataPath, ChecksumsPath,
		BundlePath(TargetAMD64), BundlePath(TargetARM64)}
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(committedDirectory, name))
		if err != nil {
			t.Fatalf("%s: %s", name, err)
		}
		target := filepath.Join(directory, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("%s: %s", name, err)
		}
		if err := os.WriteFile(target, raw, 0o644); err != nil {
			t.Fatalf("%s: %s", name, err)
		}
	}
	return directory
}

// TestLoadAcceptsTheCommittedClosure.
func TestLoadAcceptsTheCommittedClosure(t *testing.T) {
	pin := committedPin(t)
	if pin.Release != "rust-v"+pin.Version {
		t.Fatalf("release %s does not spell version %s", pin.Release, pin.Version)
	}
	if len(pin.Platforms) != 2 {
		t.Fatalf("got %d platforms", len(pin.Platforms))
	}
	for _, platform := range Platforms {
		pinned := pin.Platforms[platform]
		if pinned.Target != Targets[platform] {
			t.Errorf("%s names target %s", platform, pinned.Target)
		}
		if pinned.URL != AssetURL(pin.Release, pinned.Asset) {
			t.Errorf("%s url %s is not canonical", platform, pinned.URL)
		}
	}
	if pin.Authentication.VerifiedOn.IsZero() {
		t.Error("verified_on is zero")
	}
}

// TestParseRefusesWhatTheReviewDidNotAccept: every field of the pin is load-bearing,
// so a document the review never produced is a refusal rather than a silent pass.
func TestParseRefusesWhatTheReviewDidNotAccept(t *testing.T) {
	directory := copyClosure(t)
	original, err := os.ReadFile(filepath.Join(directory, "pin.json"))
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	cases := []struct {
		label   string
		needle  string
		replace [][2]string
	}{
		{"schema", "pin schema must be 1", [][2]string{{`"schema": 1`, `"schema": 2`}}},
		{"leading-zero schema", "is not a JSON number", [][2]string{{`"schema": 1`, `"schema": 01`}}},
		{"name", "pin name must be codex", [][2]string{{`"name": "codex"`, `"name": "codexx"`}}},
		{"kind", "pin kind must be", [][2]string{{`"codex-standalone-package"`, `"codex-package"`}}},
		{"bare release", "must be rust-v<version>", [][2]string{{`"release": "rust-v0.163.0"`, `"release": "0.163.0"`}}},
		{"release does not spell version", "does not spell pin version",
			[][2]string{{`"release": "rust-v0.163.0"`, `"release": "rust-v0.163.1"`}}},
		{"layout version", "layout_version must be 1", [][2]string{{`"layout_version": 1`, `"layout_version": 2`}}},
		{"variant", "variant must be codex", [][2]string{{`"variant": "codex"`, `"variant": "daemon"`}}},
		{"entrypoint", "entrypoint must be bin/codex", [][2]string{{`"entrypoint": "bin/codex"`, `"entrypoint": "bin/codex-cli"`}}},
		{"metadata url", "metadata evidence url must be", [][2]string{{
			`"url": "https://api.github.com/repos/openai/codex/releases/tags/rust-v0.163.0"`,
			`"url": "https://api.github.com/repos/openai/codex/releases/tags/rust-v0.163.1"`}}},
		{"checksums path", "checksums evidence path must be", [][2]string{{
			`"path": "evidence/codex-package_SHA256SUMS"`, `"path": "evidence/SHA256SUMS"`}}},
		{"digest case", "64 lower-case hex characters", [][2]string{{
			`"sha256": "582646c6bf0561eb0de540f7fc85b9121b0091a826bc6e96830b7f1722122182"`,
			`"sha256": "582646C6BF0561EB0DE540F7FC85B9121B0091A826BC6E96830B7F1722122182"`}}},
		{"extra key", "unexpected", [][2]string{{`"schema": 1,`, `"schema": 1, "extra": 1,`}}},
		{"verifier version", "verifier version must be v2.4.1", [][2]string{{`"version": "v2.4.1"`, `"version": "v2.5.0"`}}},
		{"identity", "certificate identity must be", [][2]string{{
			`"certificate_identity": "https://github.com/openai/codex/.github/workflows/rust-release.yml@refs/tags/rust-v0.163.0"`,
			`"certificate_identity": "https://github.com/openai/codex/.github/workflows/rust-release.yml@refs/heads/main"`}}},
		{"issuer", "OIDC issuer must be", [][2]string{{`"https://token.actions.githubusercontent.com"`, `"https://example.test"`}}},
		{"verified_on shape", "must be YYYY-MM-DD", [][2]string{{`"verified_on": "2026-11-13"`, `"verified_on": "2026-11-3"`}}},
		{"verified_on calendar", "not a calendar date", [][2]string{{`"verified_on": "2026-11-13"`, `"verified_on": "2026-02-30"`}}},
		{"verified_on year zero", "not a calendar date", [][2]string{{`"verified_on": "2026-11-13"`, `"verified_on": "0000-01-01"`}}},
		{"verified_on future", "is in the future", [][2]string{{`"verified_on": "2026-11-13"`, `"verified_on": "2999-01-01"`}}},
	}
	for _, test := range cases {
		text := string(original)
		for _, pair := range test.replace {
			if !strings.Contains(text, pair[0]) {
				t.Fatalf("%s: the committed pin does not contain %q", test.label, pair[0])
			}
			text = strings.Replace(text, pair[0], pair[1], 1)
		}
		_, err := Parse([]byte(text), directory)
		if err == nil || !strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
		}
	}
}

// TestEvidenceIsRehashedNow: the pin's digest is a claim about bytes on this disk,
// so a rewritten evidence file is refused even though the pin still parses.
func TestEvidenceIsRehashedNow(t *testing.T) {
	for _, name := range []string{MetadataPath, ChecksumsPath, BundlePath(TargetARM64)} {
		directory := copyClosure(t)
		path := filepath.Join(directory, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %s", name, err)
		}
		if err := os.WriteFile(path, append(raw, ' '), 0o644); err != nil {
			t.Fatalf("%s: %s", name, err)
		}
		if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), "does not match the pin digest") {
			t.Errorf("%s: got %v, want a digest refusal", name, err)
		}
	}
}

// TestLoadFollowsNoSymlink: a pin directory that can redirect a read is not the
// reviewed closure.
func TestLoadFollowsNoSymlink(t *testing.T) {
	directory := copyClosure(t)
	raw, err := os.ReadFile(filepath.Join(directory, ChecksumsPath))
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "checksums")
	if err := os.WriteFile(elsewhere, raw, 0o644); err != nil {
		t.Fatalf("write: %s", err)
	}
	if err := os.Remove(filepath.Join(directory, ChecksumsPath)); err != nil {
		t.Fatalf("remove: %s", err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(directory, ChecksumsPath)); err != nil {
		t.Fatalf("symlink: %s", err)
	}
	if _, err := Load(directory); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("got %v, want a symlink refusal", err)
	}
}

// TestChecksumsGrammar: the manifest grammar is shared by the loader and the writer,
// so it is tested as grammar rather than as one caller's convenience.
func TestChecksumsGrammar(t *testing.T) {
	cases := []struct {
		label  string
		raw    string
		needle string
		lines  int
	}{
		{label: "plain", raw: "abc123" + strings.Repeat("0", 58) + "  name.tar.gz\n", lines: 1},
		{label: "binary marker", raw: strings.Repeat("a", 64) + " *name.tar.gz\n", lines: 1},
		{label: "blank lines", raw: "\n" + strings.Repeat("a", 64) + "  a\n\n", lines: 1},
		{label: "short digest", raw: "abc  a\n", needle: "unreadable line"},
		{label: "no name", raw: strings.Repeat("a", 64) + "\n", needle: "unreadable line"},
		{label: "duplicate", raw: strings.Repeat("a", 64) + "  a\n" + strings.Repeat("b", 64) + "  a\n",
			needle: "repeat the entry"},
		{label: "not UTF-8", raw: "\xff\xfe  a\n", needle: "not UTF-8"},
	}
	for _, test := range cases {
		lines, err := ParseChecksums([]byte(test.raw), "committed checksums")
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
		if len(lines) != test.lines {
			t.Errorf("%s: got %d lines, want %d", test.label, len(lines), test.lines)
		}
	}
}

// TestBundleIsRefusedForAnotherPayload: a bundle is committed because it names this
// archive's digest, so a bundle for another digest is not evidence for this one.
func TestBundleIsRefusedForAnotherPayload(t *testing.T) {
	pin := committedPin(t)
	pinned := pin.Platforms["linux/amd64"]
	raw, err := os.ReadFile(filepath.Join(committedDirectory, pinned.Bundle.Path))
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	if err := checkBundle(raw, pinned); err != nil {
		t.Fatalf("the committed bundle is refused: %s", err)
	}
	other := pinned
	other.SHA256 = strings.Repeat("0", 64)
	if err := checkBundle(raw, other); err == nil || !strings.Contains(err.Error(), "entry is for") {
		t.Fatalf("got %v, want a hashedrekord digest refusal", err)
	}
}

// TestELFRefusesTheWrongMachine: "the checksums agree" can never stand in for "this
// is a binary for this machine".
func TestELFRefusesTheWrongMachine(t *testing.T) {
	cases := []struct {
		label  string
		header []byte
		needle string
	}{
		{"short", []byte{0x7f, 'E'}, "shorter than an ELF header"},
		{"not ELF", []byte("#!/bin/sh\necho codex\n"), "not an ELF object"},
		{"32-bit", elfHeaderFor(62, 1, 1, 1), "not a 64-bit ELF object"},
		{"big endian", elfHeaderFor(62, 2, 2, 1), "not a little-endian ELF object"},
		{"old version", elfHeaderFor(62, 2, 1, 0), "not a current-version ELF object"},
		{"wrong machine", elfHeaderFor(183, 2, 1, 1), "targets ELF machine 183, not 62"},
	}
	for _, test := range cases {
		err := requireELF(test.header, TargetAMD64)
		if err == nil || !strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
		}
	}
	if err := requireELF(elfHeaderFor(62, 2, 1, 1), TargetAMD64); err != nil {
		t.Errorf("the maintained machine was refused: %s", err)
	}
}

// TestSelectionRefusesAnUnreviewedRelease.
func TestSelectionRefusesAnUnreviewedRelease(t *testing.T) {
	pin := committedPin(t)
	if _, err := pin.Selection(pin.Release, TargetARM64); err != nil {
		t.Fatalf("the pinned release was refused: %s", err)
	}
	if _, err := pin.Selection("rust-v0.164.0", TargetARM64); err == nil ||
		!strings.Contains(err.Error(), "not the pinned Codex release") {
		t.Fatalf("got %v, want an unreviewed-release refusal", err)
	}
	if _, err := pin.Selection(pin.Release, Target("wasm32-unknown-unknown")); err == nil ||
		!strings.Contains(err.Error(), "not a maintained Codex platform") {
		t.Fatalf("got %v, want an unmaintained-target refusal", err)
	}
}

func elfHeaderFor(machine uint16, class, data, version byte) []byte {
	header := make([]byte, elfHeaderBytes)
	copy(header, []byte{0x7f, 'E', 'L', 'F'})
	header[4], header[5], header[6] = class, data, version
	header[18] = byte(machine)
	header[19] = byte(machine >> 8)
	return header
}

// --- archive fixtures --------------------------------------------------------

// member is one fixture archive entry.
type member struct {
	name     string
	data     []byte
	typeflag byte
	mode     int64
}

func packageDocument(version, target string) []byte {
	raw, err := json.Marshal(map[string]any{
		"layoutVersion": 1,
		"version":       version,
		"target":        target,
		"variant":       "codex",
		"entrypoint":    "bin/codex",
		"resourcesDir":  "codex-resources",
		"pathDir":       "codex-path",
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func defaultMembers() []member {
	return []member{
		{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "bin/codex", data: elfHeaderFor(62, 2, 1, 1), typeflag: tar.TypeReg, mode: 0o755},
		{name: "bin/codex-code-mode-host", data: []byte("host\n"), typeflag: tar.TypeReg, mode: 0o755},
		{name: "codex-path/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "codex-path/rg", data: []byte("rg\n"), typeflag: tar.TypeReg, mode: 0o755},
		{name: "codex-resources/", typeflag: tar.TypeDir, mode: 0o755},
		{name: "codex-resources/bwrap", data: []byte("bwrap\n"), typeflag: tar.TypeReg, mode: 0o755},
		{name: "codex-package.json", data: packageDocument("0.163.0", string(TargetAMD64)),
			typeflag: tar.TypeReg, mode: 0o644},
	}
}

func archiveOf(t *testing.T, members []member) []byte {
	t.Helper()
	var decoded bytes.Buffer
	writer := tar.NewWriter(&decoded)
	for _, entry := range members {
		header := &tar.Header{Name: entry.name, Typeflag: entry.typeflag, Mode: entry.mode,
			Size: int64(len(entry.data))}
		if entry.typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("%s: %s", entry.name, err)
		}
		if header.Size > 0 {
			if _, err := writer.Write(entry.data); err != nil {
				t.Fatalf("%s: %s", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %s", err)
	}
	var compressed bytes.Buffer
	gzipper := gzip.NewWriter(&compressed)
	if _, err := gzipper.Write(decoded.Bytes()); err != nil {
		t.Fatalf("gzip: %s", err)
	}
	if err := gzipper.Close(); err != nil {
		t.Fatalf("gzip: %s", err)
	}
	return compressed.Bytes()
}

// syntheticPin is a pin whose committed digest names exactly this archive, so the
// archive policy is the only thing a case can be refused by.
func syntheticPin(t *testing.T, archive []byte, platform string) (Pin, PlatformPin) {
	t.Helper()
	target := Targets[platform]
	pinned := PlatformPin{Platform: platform, Target: target,
		Asset: AssetName(target), URL: AssetURL("rust-v0.163.0", AssetName(target)),
		SHA256: digestOfBytes(archive)}
	pin := Pin{Release: "rust-v0.163.0", Version: "0.163.0",
		Platforms: map[string]PlatformPin{platform: pinned}}
	return pin, pinned
}

func digestOfBytes(raw []byte) string {
	return sha256Hex(raw)
}

func writeArchive(t *testing.T, raw []byte, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write: %s", err)
	}
	return path
}

// TestPreflightAcceptsTheReviewedLayout: the passing case is what makes the refusals
// below mean something.
func TestPreflightAcceptsTheReviewedLayout(t *testing.T) {
	archive := archiveOf(t, defaultMembers())
	pin, pinned := syntheticPin(t, archive, "linux/amd64")
	path := writeArchive(t, archive, "archive.tar.gz")
	if err := Preflight(pin, pinned, path); err != nil {
		t.Fatalf("the reviewed layout was refused: %s", err)
	}
}

// TestPreflightRefusesTheArchivePolicy walks the member and package policies.
func TestPreflightRefusesTheArchivePolicy(t *testing.T) {
	mutate := func(edit func([]member) []member) []member { return edit(defaultMembers()) }
	replace := func(name string, entry member) func([]member) []member {
		return func(members []member) []member {
			for index, existing := range members {
				if existing.name == name {
					members[index] = entry
				}
			}
			return members
		}
	}
	cases := []struct {
		label   string
		members []member
		needle  string
	}{
		{"absolute", mutate(func(m []member) []member {
			return append(m, member{name: "/etc/passwd", data: []byte("x"), typeflag: tar.TypeReg, mode: 0o644})
		}), "absolute member name"},
		{"traversal", mutate(func(m []member) []member {
			return append(m, member{name: "../evil", data: []byte("x"), typeflag: tar.TypeReg, mode: 0o644})
		}), "non-canonical member name"},
		{"dot spelling", mutate(func(m []member) []member {
			return append(m, member{name: "./extra", data: []byte("x"), typeflag: tar.TypeReg, mode: 0o644})
		}), "non-canonical member name"},
		{"duplicate", mutate(func(m []member) []member {
			return append(m, member{name: "bin/codex", data: []byte("y"), typeflag: tar.TypeReg, mode: 0o755})
		}), "duplicate archive member"},
		{"setuid", mutate(func(m []member) []member {
			return append(m, member{name: "bin/suid", data: []byte("x"), typeflag: tar.TypeReg, mode: 0o4755})
		}), "setuid/setgid member"},
		{"link", mutate(func(m []member) []member {
			return append(m, member{name: "bin/link", typeflag: tar.TypeSymlink, mode: 0o777})
		}), "link member"},
		{"extra top-level root", mutate(func(m []member) []member {
			return append(m, member{name: "lib/", typeflag: tar.TypeDir, mode: 0o755},
				member{name: "lib/codex.so", data: []byte("x"), typeflag: tar.TypeReg, mode: 0o644})
		}), "unexpected top-level entry"},
		{"missing required", mutate(func(m []member) []member {
			kept := m[:0]
			for _, entry := range m {
				if entry.name != "bin/codex-code-mode-host" {
					kept = append(kept, entry)
				}
			}
			return kept
		}), "missing required file"},
		{"not executable", mutate(replace("bin/codex",
			member{name: "bin/codex", data: elfHeaderFor(62, 2, 1, 1), typeflag: tar.TypeReg, mode: 0o644})),
			"is not executable"},
		{"missing metadata", mutate(func(m []member) []member {
			kept := m[:0]
			for _, entry := range m {
				if entry.name != "codex-package.json" {
					kept = append(kept, entry)
				}
			}
			return kept
		}), "missing codex-package.json"},
		{"wrong package version", mutate(replace("codex-package.json",
			member{name: "codex-package.json", data: packageDocument("0.164.0", string(TargetAMD64)),
				typeflag: tar.TypeReg, mode: 0o644})), "version is not the pinned"},
		{"wrong package target", mutate(replace("codex-package.json",
			member{name: "codex-package.json", data: packageDocument("0.163.0", string(TargetARM64)),
				typeflag: tar.TypeReg, mode: 0o644})), "target is not"},
		{"wrong ELF machine", mutate(replace("bin/codex",
			member{name: "bin/codex", data: elfHeaderFor(183, 2, 1, 1), typeflag: tar.TypeReg, mode: 0o755})),
			"ELF machine"},
		{"not an ELF", mutate(replace("bin/codex",
			member{name: "bin/codex", data: []byte("#!/bin/sh\necho codex\n"), typeflag: tar.TypeReg, mode: 0o755})),
			"not an ELF object"},
	}
	for _, test := range cases {
		archive := archiveOf(t, test.members)
		pin, pinned := syntheticPin(t, archive, "linux/amd64")
		path := writeArchive(t, archive, "archive.tar.gz")
		err := Preflight(pin, pinned, path)
		if err == nil || !strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
		}
	}
}

// TestPreflightRefusesAnotherArchive: a consistently rewritten server still has to
// match the committed digest.
func TestPreflightRefusesAnotherArchive(t *testing.T) {
	archive := archiveOf(t, defaultMembers())
	pin, pinned := syntheticPin(t, archive, "linux/amd64")
	pinned.SHA256 = strings.Repeat("0", 64)
	path := writeArchive(t, archive, "archive.tar.gz")
	if err := Preflight(pin, pinned, path); err == nil ||
		!strings.Contains(err.Error(), "does not match the committed") {
		t.Fatalf("got %v, want a digest refusal", err)
	}
}

// TestPreflightRefusesToFollowASymlink.
func TestPreflightRefusesToFollowASymlink(t *testing.T) {
	archive := archiveOf(t, defaultMembers())
	pin, pinned := syntheticPin(t, archive, "linux/amd64")
	real := writeArchive(t, archive, "archive.tar.gz")
	link := filepath.Join(t.TempDir(), "link.tar.gz")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %s", err)
	}
	if err := Preflight(pin, pinned, link); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("got %v, want a symlink refusal", err)
	}
}

// --- raw block fixtures ------------------------------------------------------
//
// The bounds that matter are on the compressed stream, not on the members a parser
// yields, so these fixtures are written as 512-byte blocks: no library will produce
// a bomb.

func tarBlock(name string, size int64, typeflag byte, mode int64) []byte {
	block := make([]byte, tarBlockBytes)
	copy(block[0:tarNameLimit], name)
	copy(block[100:108], []byte("0000755\x00"))
	copy(block[108:116], []byte("0000000\x00"))
	copy(block[116:124], []byte("0000000\x00"))
	copy(block[124:136], []byte(fmt.Sprintf("%011o\x00", size)))
	copy(block[136:148], []byte("00000000000\x00"))
	block[156] = typeflag
	copy(block[257:265], []byte("ustar\x00\x00"))
	copy(block[265:269], []byte("00\x00"))
	copy(block[148:156], []byte("        "))
	var sum int64
	for _, value := range block {
		sum += int64(value)
	}
	copy(block[148:156], []byte(fmt.Sprintf("%06o\x00 ", sum)))
	_ = mode
	return block
}

func padToBlock(raw []byte) []byte {
	if remainder := len(raw) % tarBlockBytes; remainder != 0 {
		return append(raw, make([]byte, tarBlockBytes-remainder)...)
	}
	return raw
}

func gzipOf(t *testing.T, raw []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatalf("gzip: %s", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip: %s", err)
	}
	return compressed.Bytes()
}

func paxRecord(key, value string) []byte {
	body := " " + key + "=" + value + "\n"
	length := len(body) + 1
	for {
		record := fmt.Sprintf("%d%s", length, body)
		if len(record) == length {
			return []byte(record)
		}
		length = len(record)
	}
}

// TestPreflightBoundsTheRawWalk: a small compressed archive that declares enormous
// metadata is refused before the parser allocates it.
func TestPreflightBoundsTheRawWalk(t *testing.T) {
	cases := []struct {
		label  string
		raw    []byte
		needle string
	}{
		{"pax header bomb", tarBlock("PaxHeaders/x", 1<<20, typePAX, 0o644), "extended header of"},
		{"gnu long header bomb", tarBlock("././@LongLink", 1<<20, typeGNULongName, 0o644), "extended header of"},
		{"gnu sparse", tarBlock("sparse", 0, typeGNUSparse, 0o644), "GNU sparse members"},
		{"v7 directory with data", append(tarBlock("dir/", 512, 0x00, 0o755), make([]byte, 512)...),
			"V7 directory header declares"},
		{"pax sparse", padToBlock(append(
			append(tarBlock("PaxHeaders/x", int64(len(paxRecord("GNU.sparse.size", "512"))), typePAX, 0o644),
				padToBlock(paxRecord("GNU.sparse.size", "512"))...),
			tarBlock("spare", 0, typeRegular, 0o644)...)), "sparse PAX metadata"},
		{"physical headers", func() []byte {
			// One header past the budget: cheap to build, and the raw walk refuses
			// before the inventory ever sees a member.
			blocks := make([]byte, 0, (MaxPhysicalHeaders+1)*tarBlockBytes)
			for index := 0; index <= MaxPhysicalHeaders; index++ {
				blocks = append(blocks, tarBlock(fmt.Sprintf("bin/spare%d", index), 0, typeRegular, 0o644)...)
			}
			return blocks
		}(), "archive headers"},
		{"extended header aggregate", func() []byte {
			record := paxRecord("comment", strings.Repeat("x", 40))
			header := tarBlock("PaxHeaders/x", int64(len(record)), typePAX, 0o644)
			header = append(header, padToBlock(record)...)
			per := MaxExtendedHeaderBytes - MaxExtendedHeaderBytes%tarBlockBytes
			body := padToBlock(paxRecord("comment", strings.Repeat("y", per-40)))
			raw := make([]byte, 0, len(header)+len(body)*20)
			raw = append(raw, header...)
			for index := 0; index < 20; index++ {
				raw = append(raw, tarBlock("PaxHeaders/x", int64(len(body)), typePAX, 0o644)...)
				raw = append(raw, body...)
			}
			return raw
		}(), "extended headers total"},
	}
	for _, test := range cases {
		archive := gzipOf(t, append(padToBlock(test.raw), make([]byte, 1024)...))
		pin, pinned := syntheticPin(t, archive, "linux/amd64")
		path := writeArchive(t, archive, "archive.tar.gz")
		err := Preflight(pin, pinned, path)
		if err == nil || !strings.Contains(err.Error(), test.needle) {
			t.Errorf("%s: got %v, want a refusal naming %q", test.label, err, test.needle)
		}
	}
}

// TestPreflightRefusesATruncatedStream: gzip integrity is part of the check, not an
// inconvenience.
func TestPreflightRefusesATruncatedStream(t *testing.T) {
	archive := archiveOf(t, defaultMembers())
	truncated := archive[:len(archive)/2]
	pin, pinned := syntheticPin(t, truncated, "linux/amd64")
	path := writeArchive(t, truncated, "archive.tar.gz")
	err := Preflight(pin, pinned, path)
	if err == nil || !strings.Contains(err.Error(), "gzip") {
		t.Fatalf("got %v, want a gzip refusal", err)
	}
}

// TestPreflightCountsBytesAfterTheEndMarker: tar stops at its end marker, but the
// decoded stream keeps going, and the budget counts every byte of it.
func TestPreflightCountsBytesAfterTheEndMarker(t *testing.T) {
	var raw bytes.Buffer
	for _, entry := range defaultMembers() {
		block := tarBlock(entry.name, int64(len(entry.data)), entry.typeflag, entry.mode)
		raw.Write(block)
		raw.Write(padToBlock(entry.data))
	}
	raw.Write(make([]byte, 1024))
	decoded := raw.Len()
	raw.Write(make([]byte, 200000))
	archive := gzipOf(t, raw.Bytes())
	pin, pinned := syntheticPin(t, archive, "linux/amd64")
	path := writeArchive(t, archive, "archive.tar.gz")
	if err := Preflight(pin, pinned, path); err != nil {
		t.Fatalf("the archive is within the reviewed budget: %s", err)
	}
	stream, err := openDecoded(path, "archive", int64(decoded+1000))
	if err != nil {
		t.Fatalf("open: %s", err)
	}
	defer func() { _ = stream.close() }()
	if err := stream.drain(); err == nil || !strings.Contains(err.Error(), "decoded stream exceeds") {
		t.Fatalf("got %v, want a decoded-stream refusal", err)
	}
}

// TestPaxSizeOverrideMovesBothWalksTogether: the archive is legal and must pass, so
// the bomb cases above are about the bound rather than about a rejection of PAX.
func TestPaxSizeOverrideMovesBothWalksTogether(t *testing.T) {
	var raw bytes.Buffer
	// The PAX `size` record makes this member 1024 bytes; the JSON document fills it
	// and the rest is the space padding a tar writer emits.
	document := bytes.Repeat([]byte(" "), 1024)
	copy(document, packageDocument("0.163.0", string(TargetAMD64)))
	records := paxRecord("size", "1024")
	raw.Write(tarBlock("PaxHeaders/codex-package.json", int64(len(records)), typePAX, 0o644))
	raw.Write(padToBlock(records))
	raw.Write(tarBlock("codex-package.json", 0, typeRegular, 0o644))
	raw.Write(document)
	for _, entry := range defaultMembers() {
		if entry.name == "codex-package.json" {
			continue
		}
		raw.Write(tarBlock(entry.name, int64(len(entry.data)), entry.typeflag, entry.mode))
		raw.Write(padToBlock(entry.data))
	}
	raw.Write(make([]byte, 1024))
	archive := gzipOf(t, raw.Bytes())
	pin, pinned := syntheticPin(t, archive, "linux/amd64")
	path := writeArchive(t, archive, "archive.tar.gz")
	if err := Preflight(pin, pinned, path); err != nil {
		t.Fatalf("a legal PAX size override was refused: %s", err)
	}
}

// TestPaxPrecedenceMatchesTheParser: consecutive PAX headers for one member are
// merged by the parser, and a later set wins a key an earlier one also names. The raw
// walk has to agree, or the region it skips is not the region the parser reads.
func TestPaxPrecedenceMatchesTheParser(t *testing.T) {
	// `size=1024` then `size=0` for the member that follows: the parser reads no
	// member data, so the next block is a header -- here an oversized GNU long name
	// that the extended-header ceiling must refuse.
	var raw []byte
	for index, size := range []string{"1024", "0"} {
		record := paxRecord("size", size)
		raw = append(raw, tarBlock(fmt.Sprintf("PaxHeaders/%d", index), int64(len(record)),
			typePAX, 0o644)...)
		raw = append(raw, padToBlock(record)...)
	}
	raw = append(raw, tarBlock("member", 0, typeRegular, 0o644)...)
	raw = append(raw, tarBlock("././@LongLink", 1<<20, typeGNULongName, 0o644)...)
	archive := gzipOf(t, raw)
	pin, pinned := syntheticPin(t, archive, "linux/amd64")
	path := writeArchive(t, archive, "archive.tar.gz")
	err := Preflight(pin, pinned, path)
	if err == nil || !strings.Contains(err.Error(), "extended header of") {
		t.Fatalf("got %v, want the hidden extended header refused", err)
	}
}

// TestPaxRecordLengthIsNeverASlicePanic: a record whose declared length ends inside
// its own length field is a refusal, not a crash.
func TestPaxRecordLengthIsNeverASlicePanic(t *testing.T) {
	for _, payload := range [][]byte{
		[]byte("0000000000005 a=b\n"),
		[]byte("5 a=b\n"),
		[]byte("0000000000010 a=b\n"),
	} {
		if _, err := paxRecords(payload, "archive"); err == nil {
			t.Errorf("%q was accepted", payload)
		}
	}
	// A well-formed record still decodes.
	records, err := paxRecords(paxRecord("size", "1024"), "archive")
	if err != nil || string(records["size"]) != "1024" {
		t.Fatalf("a well-formed record reported %v (%v)", err, records)
	}
}
