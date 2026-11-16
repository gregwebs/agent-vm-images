//go:build linux || darwin

package candidatestage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mint creates the layout the preparation driver mints: a 0700 pnpm-project-*
// scratch directly under the fixed system temporary directory with a 0700
// `candidate` child holding the three inputs. The test owns cleanup.
func mint(t *testing.T) (string, int, string) {
	t.Helper()
	scratch, err := os.MkdirTemp(SystemTempDir(), "pnpm-project-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })
	fd, err := os.Open(scratch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fd.Close() })
	candidate := filepath.Join(scratch, "candidate")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	for index, name := range InputNames {
		if err := os.WriteFile(filepath.Join(candidate, name), []byte(name+" "+string(rune('a'+index))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return scratch, int(fd.Fd()), candidate
}

func TestOpenCapturesThreeInputs(t *testing.T) {
	scratch, fd, _ := mint(t)
	stage, err := Open(scratch, fd)
	if err != nil {
		t.Fatal(err)
	}
	if stage.Scratch() != scratch {
		t.Errorf("scratch = %q", stage.Scratch())
	}
	if len(stage.Files()) != 3 {
		t.Fatalf("files = %d", len(stage.Files()))
	}
	for index, name := range InputNames {
		source, ok := stage.File(name)
		if !ok || string(source) != name+" "+string(rune('a'+index)) {
			t.Errorf("%s = %q, %v", name, source, ok)
		}
	}
	if err := stage.Recheck(); err != nil {
		t.Fatalf("recheck = %v", err)
	}
	// The inherited descriptor is borrowed, never closed by Open.
	if _, err := Open(scratch, fd); err != nil {
		t.Fatalf("second open through the same fd = %v", err)
	}
}

func TestOpenRejectsDescriptor(t *testing.T) {
	scratch, fd, _ := mint(t)
	for _, stdio := range []int{0, 1, 2} {
		if _, err := Open(scratch, stdio); err == nil || !strings.Contains(err.Error(), "above 2") {
			t.Errorf("fd %d = %v", stdio, err)
		}
	}
	plain, err := os.CreateTemp(t.TempDir(), "plain")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	if _, err := Open(scratch, int(plain.Fd())); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("regular file fd = %v", err)
	}
	closed, err := os.Open(scratch)
	if err != nil {
		t.Fatal(err)
	}
	closedFD := int(closed.Fd())
	_ = closed.Close()
	if _, err := Open(scratch, closedFD); err == nil {
		t.Errorf("closed fd accepted")
	}
	if _, err := Open(scratch, fd+1000); err == nil {
		t.Errorf("unused fd accepted")
	}
}

func TestOpenRejectsScratchSpelling(t *testing.T) {
	scratch, fd, _ := mint(t)
	for _, vector := range []struct {
		name    string
		scratch string
		match   string
	}{
		{"relative", strings.TrimPrefix(scratch, SystemTempDir()+"/"), "absolute"},
		{"empty", "", "absolute"},
		{"dotdot", scratch + "/../" + filepath.Base(scratch), "canonical"},
		{"trailing", scratch + "/", "canonical"},
		{"wrong-parent", t.TempDir() + "/pnpm-project-x", "direct child of " + SystemTempDir()},
		{"wrong-prefix", filepath.Join(SystemTempDir(), "scratch-"+filepath.Base(scratch)), "named pnpm-project-"},
	} {
		t.Run(vector.name, func(t *testing.T) {
			if _, err := Open(vector.scratch, fd); err == nil || !strings.Contains(err.Error(), vector.match) {
				t.Fatalf("%s = %v, want %q", vector.name, err, vector.match)
			}
		})
	}
}

func TestOpenRejectsModesAndDisagreement(t *testing.T) {
	scratch, fd, _ := mint(t)
	if err := os.Chmod(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "mode must be 0700") {
		t.Errorf("scratch mode = %v", err)
	}
	if err := os.Chmod(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(scratch, "candidate"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "mode must be 0700") {
		t.Errorf("candidate mode = %v", err)
	}
	if err := os.Chmod(filepath.Join(scratch, "candidate"), 0o700); err != nil {
		t.Fatal(err)
	}
	other, otherFD, _ := mint(t)
	if _, err := Open(other, fd); err == nil || !strings.Contains(err.Error(), "path and inherited descriptor disagree") {
		t.Errorf("swapped path = %v", err)
	}
	if _, err := Open(scratch, otherFD); err == nil || !strings.Contains(err.Error(), "path and inherited descriptor disagree") {
		t.Errorf("swapped fd = %v", err)
	}
	real, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(SystemTempDir(), "pnpm-project-link-"+filepath.Base(scratch))
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if _, err := Open(link, fd); err == nil || !strings.Contains(err.Error(), "symlinks are not permitted") {
		t.Errorf("symlinked scratch = %v", err)
	}
}

func TestOpenRejectsCandidateShape(t *testing.T) {
	scratch, fd, candidate := mint(t)
	if err := os.Remove(filepath.Join(candidate, "pnpm-lock.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "missing candidate input: pnpm-lock.yaml") {
		t.Errorf("missing input = %v", err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "pnpm-lock.yaml"), []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"pnpm-inputs.sha256", "package-lock.json", "npm-shrinkwrap.json", ".npmrc", ".pnpmfile.mjs", "pnpmfile.cjs"} {
		path := filepath.Join(candidate, entry)
		if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "unexpected candidate entry: "+entry) {
			t.Errorf("%s = %v", entry, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	nodeModules := filepath.Join(candidate, "node_modules")
	if err := os.Mkdir(nodeModules, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "unexpected candidate entry: node_modules") {
		t.Errorf("node_modules = %v", err)
	}
	if err := os.Remove(nodeModules); err != nil {
		t.Fatal(err)
	}
	// A missing candidate directory is a failure, never an empty capture.
	if err := os.RemoveAll(candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "lstat") {
		t.Errorf("missing candidate = %v", err)
	}
}

func TestOpenRejectsUnsafeInputFiles(t *testing.T) {
	scratch, fd, candidate := mint(t)
	backup := filepath.Join(scratch, "package.json.bak")
	if err := os.Rename(filepath.Join(candidate, "package.json"), backup); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(scratch, "outside.json")
	if err := os.WriteFile(outside, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink input is substituted bytes, not a captured candidate file.
	if err := os.Symlink(outside, filepath.Join(candidate, "package.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
		t.Errorf("symlink input = %v", err)
	}
	if err := os.Remove(filepath.Join(candidate, "package.json")); err != nil {
		t.Fatal(err)
	}
	// A hardlink has a second name a same-UID writer can mutate through.
	if err := os.Link(outside, filepath.Join(candidate, "package.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
		t.Errorf("hardlink input = %v", err)
	}
	if err := os.Remove(filepath.Join(candidate, "package.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "package.json"), []byte("{}"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(candidate, "package.json"), 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "group or other writable") {
		t.Errorf("writable input = %v", err)
	}
	if err := os.Remove(filepath.Join(candidate, "package.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(candidate, "package.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "single-link regular file") {
		t.Errorf("directory input = %v", err)
	}
}

func TestOpenRejectsOversizedInput(t *testing.T) {
	scratch, fd, candidate := mint(t)
	large := make([]byte, 16<<20+1)
	if err := os.WriteFile(filepath.Join(candidate, "pnpm-lock.yaml"), large, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(scratch, fd); err == nil || !strings.Contains(err.Error(), "16 MiB") {
		t.Errorf("oversized = %v", err)
	}
}

func TestRecheckDetectsChange(t *testing.T) {
	scratch, fd, candidate := mint(t)
	stage, err := Open(scratch, fd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "package.json"), []byte(`{"changed":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stage.Recheck(); err == nil || !strings.Contains(err.Error(), "package.json: changed during candidate validation") {
		t.Errorf("recheck = %v", err)
	}
}
