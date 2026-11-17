package agypin

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestExactVersionIsTheHookGrammar: the install hook's version grammar is the
// reader's, so a value the pin loader would reject never reaches a pin comparison.
func TestExactVersionIsTheHookGrammar(t *testing.T) {
	accepted := []string{"1.4.11", "0.0.0", "10.20.30"}
	for _, value := range accepted {
		if !ExactVersion(value) {
			t.Errorf("%q was refused", value)
		}
	}
	refused := []string{"", "v1.4.11", "1.4", "1.4.11.2", "latest", "1.4.11 ",
		" 1.4.11", "1.4.11\n2.0.0", "01.4.11", "1.04.11", "1.4.11-rc1", "1.4.11\t"}
	for _, value := range refused {
		if ExactVersion(value) {
			t.Errorf("%q was accepted", value)
		}
	}
}

// TestMarkerSearchCrossesAChunkBoundary: the command is read in chunks, so the
// marker has to be found when it straddles one.
func TestMarkerSearchCrossesAChunkBoundary(t *testing.T) {
	marker := []byte(Marker)
	body := []byte("junk before the string #" + Marker + " and junk after it")
	for _, chunk := range []int{1, 2, 3, 7, len(marker), len(marker) + 1, len(body)} {
		found, err := containsMarker(bytes.NewReader(body), marker, chunk)
		if err != nil {
			t.Fatalf("chunk %d: %s", chunk, err)
		}
		if !found {
			t.Errorf("chunk %d did not find the marker", chunk)
		}
	}
	absent := bytes.Repeat([]byte("x"), 100)
	for _, chunk := range []int{1, 3, 100} {
		found, err := containsMarker(bytes.NewReader(absent), marker, chunk)
		if err != nil {
			t.Fatalf("chunk %d: %s", chunk, err)
		}
		if found {
			t.Errorf("chunk %d found a marker that is not there", chunk)
		}
	}
}

// TestInstalledCarriesTheReviewedShape is the gate's whole rule: a regular file at
// exactly 0755 with the expected owner and a release that still reads the switch.
func TestInstalledCarriesTheReviewedShape(t *testing.T) {
	directory := t.TempDir()
	bin := filepath.Join(directory, "agy")
	body := "#!/bin/sh\n# " + Marker + "\nprintf '1.4.11\\n'\n"
	if err := os.WriteFile(bin, []byte(body), 0o755); err != nil {
		t.Fatalf("%s: %s", bin, err)
	}
	if err := os.Chmod(bin, ReviewedMode); err != nil {
		t.Fatalf("%s: %s", bin, err)
	}
	uid, gid := identity(t, bin)
	if err := CheckInstalled(bin, uid, gid); err != nil {
		t.Fatalf("the reviewed shape was refused: %s", err)
	}

	// A mode a group or every guest can write is the failure the check exists for.
	if err := os.Chmod(bin, 0o775); err != nil {
		t.Fatalf("%s: %s", bin, err)
	}
	err := CheckInstalled(bin, uid, gid)
	if err == nil || !strings.Contains(err.Error(), "is '775 ") ||
		!strings.Contains(err.Error(), "want '755 ") {
		t.Errorf("a group-writable command reported %v", err)
	}
	if err := os.Chmod(bin, ReviewedMode); err != nil {
		t.Fatalf("%s: %s", bin, err)
	}

	// A command that is a link is refused before the stat would follow it.
	link := filepath.Join(directory, "link-agy")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatalf("%s: %s", link, err)
	}
	if err := CheckInstalled(link, uid, gid); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("a symlinked command reported %v", err)
	}

	// A missing command is a refusal, not a panic.
	if err := CheckInstalled(filepath.Join(directory, "absent"), uid, gid); err == nil ||
		!strings.Contains(err.Error(), "cannot read the mode and owner") {
		t.Errorf("a missing command reported %v", err)
	}

	// The switch is the reason this is a gate: a release that renamed it would ship
	// a binary that updates itself in a guest.
	renamed := filepath.Join(directory, "renamed")
	if err := os.WriteFile(renamed, []byte("#!/bin/sh\nprintf '1.4.11\\n'\n"), 0o755); err != nil {
		t.Fatalf("%s: %s", renamed, err)
	}
	if err := os.Chmod(renamed, ReviewedMode); err != nil {
		t.Fatalf("%s: %s", renamed, err)
	}
	if err := CheckInstalled(renamed, uid, gid); err == nil ||
		!strings.Contains(err.Error(), "no longer names "+Marker) {
		t.Errorf("a release that dropped the switch reported %v", err)
	}
}

// TestSelectionIsTheCommittedPin: the install hook refuses an exact-but-unpinned
// version, an unsupported architecture and a missing pin, each before a fetch.
func TestSelectionIsTheCommittedPin(t *testing.T) {
	// The pin's directory names its artifact, so the fixture keeps production's
	// `agy/` spelling.
	directory := filepath.Join(t.TempDir(), "agy")
	committed, err := os.ReadFile(filepath.Join("..", "..", "images", "tools", "agy",
		"pin.json"))
	if err != nil {
		t.Fatalf("cannot read the committed pin: %s", err)
	}
	evidenceSource := filepath.Join("..", "..", "images", "tools", "agy", "evidence")
	if err := os.MkdirAll(filepath.Join(directory, "evidence"), 0o700); err != nil {
		t.Fatalf("%s: %s", directory, err)
	}
	if err := os.WriteFile(filepath.Join(directory, "pin.json"), committed, 0o600); err != nil {
		t.Fatalf("%s: %s", directory, err)
	}
	entries, err := os.ReadDir(evidenceSource)
	if err != nil {
		t.Fatalf("%s: %s", evidenceSource, err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(evidenceSource, entry.Name()))
		if err != nil {
			t.Fatalf("%s: %s", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(directory, "evidence", entry.Name()), raw, 0o600); err != nil {
			t.Fatalf("%s: %s", entry.Name(), err)
		}
	}

	version, err := CheckSelection(directory, "1.4.11", "amd64")
	if err != nil {
		t.Fatalf("the committed selection was refused: %s", err)
	}
	if version.String() != "1.4.11" {
		t.Errorf("the committed pin selects %s", version)
	}

	for _, arch := range []string{"amd64", "arm64"} {
		if _, err := CheckSelection(directory, "1.4.11", arch); err != nil {
			t.Errorf("%s was refused: %s", arch, err)
		}
	}
	for _, arch := range []string{"x86_64", "aarch64", ""} {
		if _, err := CheckSelection(directory, "1.4.11", arch); err == nil ||
			!strings.Contains(err.Error(), "unsupported architecture") {
			t.Errorf("architecture %q reported %v", arch, err)
		}
	}
	if _, err := CheckSelection(directory, "1.4.10", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "selects 1.4.11, not 1.4.10") {
		t.Errorf("an unpinned version reported %v", err)
	}
	if _, err := CheckSelection(directory, "v1.4.11", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "not an exact agy version") {
		t.Errorf("a malformed version reported %v", err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := CheckSelection(missing, "1.4.11", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "not readable") {
		t.Errorf("a missing pin reported %v", err)
	}
}

// identity is the test file's own owner, which is the only owner a suite can create.
func identity(t *testing.T, path string) (int, int) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %s", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("cannot read the filesystem identity of %s", path)
	}
	return int(stat.Uid), int(stat.Gid)
}
