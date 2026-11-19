package codex

import (
	"bytes"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// closureFiles is the committed closure's five files, relative and sorted.
func closureFiles() []string {
	names := []string{"pin.json", "evidence/release.json", "evidence/" + ChecksumsAsset}
	for _, name := range platforms {
		names = append(names, "evidence/"+bundleName(targetOf(name)))
	}
	sort.Strings(names)
	return names
}

// TestWriteFixtureWritesTheClosureTheCodexInstallerMounts holds the fixture seam to
// its contract, because `script/test/codex-installer.sh` mounts what it wrote and that
// script is not this package's gate.
//
// It runs through `harness.Command`, which is exactly what `avm-test write-fixture`
// calls, so the adapter is exercised too.
func TestWriteFixtureWritesTheClosureTheCodexInstallerMounts(t *testing.T) {
	dir := filepath.Join(harness.TempDir(t, "codex-fixture."), "source")
	printed := ""
	if err := harness.Command(func(r harness.Reporter) {
		printed = WriteFixture(r, Fixture{Dir: dir, Release: Release})
	}); err != nil {
		t.Fatalf("the fixture builder refused: %v", err)
	}
	if strings.TrimSpace(printed) != Release {
		t.Fatalf("the builder reports the release %q, want %s", printed, Release)
	}
	requireSameFiles(t, filesBelow(t, dir), closureFiles(), "the fixture closure")
	// The closure in the directory is one the real reader accepts from that directory:
	// that is the whole contract the installer suite relies on.
	accepts(t, pinInvocation("check", dir, "--release", Release))
}

// TestWriteInstallerFixtureWritesTheLayoutTheInstallerSuitesConsume: `source/` is the
// closure, `assets/` the six upstream documents by their canonical names,
// `archive.tgz` the selected target's package and `upstream/` the tree
// `--fixture-upstream` serves.
//
// It also holds the archive to the reviewed layout, mode by mode, and the entrypoint
// to the ELF header followed by the caller's script: those are what the production
// preflight reads and what a suite that runs the payload executes.
func TestWriteInstallerFixtureWritesTheLayoutTheInstallerSuitesConsume(t *testing.T) {
	work := harness.TempDir(t, "codex-installer-fixture.")
	body := []byte("#!/bin/sh\nprintf 'codex-cli " + Version + "\\n'\n")
	native := filepath.Join(work, "native.sh")
	harness.WriteFile(t, native, body)
	dir := filepath.Join(work, "fixture")
	printed := ""
	if err := harness.Command(func(r harness.Reporter) {
		printed = WriteInstallerFixture(r, InstallerFixture{
			Dir:          dir,
			Release:      Release,
			Target:       string(targetAMD64),
			NativeScript: native,
		})
	}); err != nil {
		t.Fatalf("the installer fixture builder refused: %v", err)
	}
	if strings.TrimSpace(printed) != Release {
		t.Fatalf("the builder reports the release %q, want %s", printed, Release)
	}

	requireSameFiles(t, filesBelow(t, filepath.Join(dir, "source")), closureFiles(),
		"the fixture closure")
	accepts(t, pinInvocation("check", filepath.Join(dir, "source"), "--release", Release))

	assets := []string{ChecksumsAsset, "release.json"}
	for _, name := range platforms {
		assets = append(assets, assetName(targetOf(name)), bundleName(targetOf(name)))
	}
	sort.Strings(assets)
	requireSameFiles(t, filesBelow(t, filepath.Join(dir, "assets")), assets, "the assets")
	requireSameFiles(t, filesBelow(t, filepath.Join(dir, "upstream")), upstreamFiles(),
		"the served tree")
	// The served tree is not just the right six names: each document is the byte the
	// closure or the assets directory holds, because the bumper suite answers the
	// writer's requests from here and a stale or substituted document would be served
	// silently.
	//
	// The six URLs are spelled out rather than computed from this package's own
	// helpers: a URL the builder and the assertion both derive from `assetURL` would
	// agree with itself even if that helper were wrong, and the writer asks for exactly
	// these strings.
	const download = "https://github.com/openai/codex/releases/download/" + Release + "/"
	served := map[string]string{
		"https://api.github.com/repos/openai/codex/releases/tags/" + Release:  "release.json",
		download + "codex-package_SHA256SUMS":                                 ChecksumsAsset,
		download + "codex-package-x86_64-unknown-linux-musl.tar.gz":           "codex-package-x86_64-unknown-linux-musl.tar.gz",
		download + "codex-package-aarch64-unknown-linux-musl.tar.gz":          "codex-package-aarch64-unknown-linux-musl.tar.gz",
		download + "codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore":  "codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore",
		download + "codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore": "codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore",
	}
	for url, asset := range served {
		_, rest, found := strings.Cut(url, "://")
		if !found {
			t.Fatalf("the fixture writes a URL with no origin: %q", url)
		}
		want := harness.ReadFile(t, filepath.Join(dir, "assets", asset))
		got := harness.ReadFile(t, filepath.Join(dir, "upstream", filepath.FromSlash(rest)))
		if !bytes.Equal(got, want) {
			t.Fatalf("the served %s is not assets/%s", url, asset)
		}
	}
	// And those bytes describe themselves: every asset the release document declares
	// is the file of that name, which is what makes the served tree consistent rather
	// than merely copied.
	released := document(t, harness.ReadFile(t, filepath.Join(dir, "assets", "release.json")))
	for _, entry := range released["assets"].([]any) {
		record := entry.(map[string]any)
		name := record["name"].(string)
		raw := harness.ReadFile(t, filepath.Join(dir, "assets", name))
		if want := "sha256:" + harness.SHA256Hex(raw); record["digest"] != want {
			t.Fatalf("the release document declares %v for %s, but the file is %s",
				record["digest"], name, want)
		}
	}
	// The checksum manifest's shape is the deleted Python fixture's, which is what the
	// suites that consume this tree were written against: the two package rows, plus one
	// extra row the review keeps and the installer never trusts. It is not a copy of the
	// retained upstream file, which lists other platforms and package variants; what it
	// must never gain is a bundle row, because the reader grants authority only to the
	// payload rows and the bundle's authority comes from the release document.
	manifest := string(harness.ReadFile(t, filepath.Join(dir, "assets", ChecksumsAsset)))
	rows := nonEmptyLines(manifest)
	if len(rows) != len(platforms)+1 {
		t.Fatalf("the checksum manifest has %d rows, want %d: %q", len(rows),
			len(platforms)+1, rows)
	}
	for _, name := range platforms {
		if !strings.Contains(manifest, assetName(targetOf(name))) {
			t.Fatalf("the checksum manifest has no row for %s", assetName(targetOf(name)))
		}
		if strings.Contains(manifest, bundleName(targetOf(name))) {
			t.Fatalf("the checksum manifest corroborates a bundle: %s", bundleName(targetOf(name)))
		}
	}
	if !strings.Contains(manifest, "  decoy\n") {
		t.Fatalf("the checksum manifest has no decoy row: %q", manifest)
	}

	// `archive.tgz` is the selected target's package, byte for byte, so the vendored
	// suite installs exactly the archive this fixture's pin commits.
	selected := filepath.Join(dir, "assets", assetName(targetAMD64))
	if !bytes.Equal(harness.ReadFile(t, filepath.Join(dir, "archive.tgz")),
		harness.ReadFile(t, selected)) {
		t.Fatal("archive.tgz is not the selected target's archive")
	}
	// The archive the installer suite routes is one the production preflight accepts,
	// which is what makes it a fixture rather than a lookalike.
	accepts(t, pinInvocation("preflight", filepath.Join(dir, "source"), "--release", Release,
		"--target", string(targetAMD64), "--archive", filepath.Join(dir, "archive.tgz")))

	archive := ReadArchive(t, filepath.Join(dir, "archive.tgz"))
	wantNames := []string{"bin", "bin/codex", "bin/codex-code-mode-host", "codex-package.json",
		"codex-path", "codex-path/rg", "codex-resources", "codex-resources/bwrap"}
	if strings.Join(archive.Names, "\n") != strings.Join(wantNames, "\n") {
		t.Fatalf("the archive members are %v, want %v", archive.Names, wantNames)
	}
	wantModes := map[string]int64{
		"bin":                      0o755,
		"bin/codex":                0o755,
		"bin/codex-code-mode-host": 0o755,
		"codex-package.json":       0o644,
		"codex-path":               0o755,
		"codex-path/rg":            0o755,
		"codex-resources":          0o755,
		"codex-resources/bwrap":    0o755,
	}
	for name, want := range wantModes {
		if got := archive.Modes[name]; got != want {
			t.Fatalf("%s has mode %#o, want %#o", name, got, want)
		}
	}
	payload := archive.Bodies["bin/codex"]
	wantPayload := make([]byte, 0, elfHeaderBytes+1+len(body))
	wantPayload = append(wantPayload, elfHeader(62)[:elfHeaderBytes]...)
	wantPayload = append(wantPayload, '\n')
	wantPayload = append(wantPayload, body...)
	if !bytes.Equal(payload, wantPayload) {
		t.Fatalf("the entrypoint is not the ELF header followed by the native script:\n got: %q\nwant: %q",
			payload, wantPayload)
	}
	metadata := document(t, archive.Bodies["codex-package.json"])
	if metadata["version"] != Version {
		t.Fatalf("the package metadata names version %v, want %s", metadata["version"], Version)
	}
	if metadata["target"] != string(targetAMD64) {
		t.Fatalf("the package metadata names target %v, want %s", metadata["target"], targetAMD64)
	}
}

// TestWriteInstallerFixtureCarriesThePackageMetadataOverride: --package-version and
// --package-target rewrite the archive's own record, which is how the installer suite
// builds a payload the maintenance preflight must refuse.
func TestWriteInstallerFixtureCarriesThePackageMetadataOverride(t *testing.T) {
	work := harness.TempDir(t, "codex-installer-override.")
	native := filepath.Join(work, "native.sh")
	harness.WriteFile(t, native, []byte("#!/bin/sh\nexit 0\n"))
	dir := filepath.Join(work, "fixture")
	if err := harness.Command(func(r harness.Reporter) {
		WriteInstallerFixture(r, InstallerFixture{
			Dir:            dir,
			Release:        Release,
			Target:         string(targetARM64),
			NativeScript:   native,
			PackageVersion: "0.159.4",
			PackageTarget:  string(targetAMD64),
		})
	}); err != nil {
		t.Fatalf("the installer fixture builder refused: %v", err)
	}
	for _, name := range platforms {
		archive := ReadArchive(t, filepath.Join(dir, "assets", assetName(targetOf(name))))
		metadata := document(t, archive.Bodies["codex-package.json"])
		if metadata["version"] != "0.159.4" {
			t.Fatalf("%s names version %v, want the override 0.159.4", name, metadata["version"])
		}
		if metadata["target"] != string(targetAMD64) {
			t.Fatalf("%s names target %v, want the override %s", name, metadata["target"],
				targetAMD64)
		}
	}
	// The ELF machine still follows the platform, not the metadata override: the
	// override exists to make the preflight refuse, not to change what the payload is.
	arm64 := ReadArchive(t, filepath.Join(dir, "assets", assetName(targetARM64)))
	if !bytes.HasPrefix(arm64.Bodies["bin/codex"], elfHeader(machineOf(targetARM64))[:elfHeaderBytes]) {
		t.Fatal("the arm64 entrypoint is not an arm64 ELF header")
	}
}
