package pnpmlock

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
)

func TestWritePinsBytes(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	if err := os.Remove(filepath.Join(string(prefix), pinFile)); err != nil {
		t.Fatal(err)
	}
	if err := WritePins(prefix, KindDsh); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join(string(prefix), pinFile))
	if err != nil {
		t.Fatal(err)
	}
	// Captured from the retired Node pin writer over the committed tiny-dsh bytes.
	const expected = "e38332e820531668714bd379a665dd96ae47d0979e8f17285e67099aa626a770  package.json\n" +
		"f5001d8aaae950eabc4d3a1519dd9aff95e1f8b1d67a2dedf522cec8b5c3c93f  pnpm-lock.yaml\n" +
		"de7cbb8ccf9ff2a507c57f45006564333f17dea8aded17aaca2ab2e65501ddc8  pnpm-workspace.yaml\n"
	if string(source) != expected {
		t.Fatalf("pins = %q, want %q", source, expected)
	}
	lines := strings.Split(strings.TrimSuffix(string(source), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("pin file = %q", source)
	}
	for index, file := range PinnedInputs {
		if !strings.HasSuffix(lines[index], "  "+file) {
			t.Errorf("line %d = %q", index, lines[index])
		}
		if len(lines[index]) != 64+2+len(file) {
			t.Errorf("line %d has the wrong shape: %q", index, lines[index])
		}
		if !strings.HasSuffix(string(source), "\n") {
			t.Error("the pin file must end with a newline")
		}
	}
}

func TestWritePinsRejectsNonRegularDestination(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	destination := filepath.Join(string(prefix), pinFile)
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WritePins(prefix, KindDsh); err == nil || !strings.Contains(err.Error(), "pin destination must be a regular file") {
		t.Errorf("directory destination = %v", err)
	}
}

// TestWritePinsUmask runs the writer in a child process because umask is
// process-global: the requested mode must stay subject to it.
func TestWritePinsUmask(t *testing.T) {
	for _, vector := range []struct {
		mask string
		want os.FileMode
	}{{"0022", 0o644}, {"0077", 0o600}} {
		prefix := fixture(t, "tiny-dsh")
		if err := os.Remove(filepath.Join(string(prefix), pinFile)); err != nil {
			t.Fatal(err)
		}
		command := exec.Command(os.Args[0], "-test.run=TestWritePinsUmaskChild")
		command.Env = append(os.Environ(), "PIN_UMASK="+vector.mask, "PIN_PREFIX="+string(prefix))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("umask %s child: %v\n%s", vector.mask, err, output)
		}
		info, err := os.Stat(filepath.Join(string(prefix), pinFile))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != vector.want {
			t.Errorf("umask %s produced mode %o, want %o", vector.mask, info.Mode().Perm(), vector.want)
		}
	}
}

// TestWritePinsUmaskChild is the child half of TestWritePinsUmask.
func TestWritePinsUmaskChild(t *testing.T) {
	mask := os.Getenv("PIN_UMASK")
	if mask == "" {
		t.Skip("child process only")
	}
	syscall.Umask(parseMask(t, mask))
	if err := WritePins(Prefix(os.Getenv("PIN_PREFIX")), KindDsh); err != nil {
		t.Fatal(err)
	}
}

func parseMask(t *testing.T, text string) int {
	t.Helper()
	mask := 0
	for _, digit := range text {
		mask = mask*8 + int(digit-'0')
	}
	return mask
}

func TestWritePinsReplacesHardlinkWithoutWritingThroughIt(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("external bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(string(prefix), pinFile)
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, destination); err != nil {
		t.Fatal(err)
	}
	if err := WritePins(prefix, KindDsh); err != nil {
		t.Fatal(err)
	}
	external, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(external) != "external bytes" {
		t.Errorf("the hardlinked witness changed: %q", external)
	}
}

// fakeOps injects publication failures deterministically.
type fakeOps struct {
	openErr   error
	writeErr  error
	short     bool
	closeErr  error
	renameErr error
	removeErr error
	temporary string
	removed   []string
	order     []string
}

type fakeHandle struct {
	ops  *fakeOps
	path string
}

func (f *fakeOps) OpenExclusive(path string) (pinHandle, error) {
	f.order = append(f.order, "open")
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.temporary = path
	return &fakeHandle{ops: f, path: path}, nil
}

func (f *fakeOps) Rename(from, to string) error {
	f.order = append(f.order, "rename")
	return f.renameErr
}

func (f *fakeOps) Remove(path string) error {
	f.order = append(f.order, "remove")
	f.removed = append(f.removed, path)
	return f.removeErr
}

func (h *fakeHandle) Write(source []byte) (int, error) {
	if h.ops.writeErr != nil {
		return 0, h.ops.writeErr
	}
	if h.ops.short {
		return len(source) / 2, nil
	}
	return len(source), nil
}

func (h *fakeHandle) Close() error { return h.ops.closeErr }

func TestWritePinsFaults(t *testing.T) {
	for _, vector := range []struct {
		name    string
		ops     *fakeOps
		want    string
		removed bool
	}{
		{"write", &fakeOps{writeErr: pathError("write", syscall.ENOSPC)}, "no space left on device", true},
		{"close", &fakeOps{closeErr: pathError("close", syscall.EIO)}, "i/o error", true},
		{"rename", &fakeOps{renameErr: pathError("rename", syscall.EXDEV)}, "cross-device link", true},
		{"cleanup", &fakeOps{writeErr: pathError("write", syscall.EIO), removeErr: pathError("unlink", syscall.EACCES)}, "permission denied", true},
		{"cleanup-absent", &fakeOps{writeErr: pathError("write", syscall.EIO), removeErr: pathError("unlink", syscall.ENOENT)}, "input/output error", true},
		{"collision", &fakeOps{openErr: pathError("open", syscall.EEXIST)}, "file already exists", false},
	} {
		prefix := fixture(t, "tiny-dsh")
		before, err := os.ReadFile(filepath.Join(string(prefix), pinFile))
		if err != nil {
			t.Fatal(err)
		}
		err = writePins(prefix, KindDsh, vector.ops)
		if err == nil || !strings.Contains(err.Error(), vector.want) {
			t.Errorf("%s = %v, want %q", vector.name, err, vector.want)
			continue
		}
		after, readErr := os.ReadFile(filepath.Join(string(prefix), pinFile))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(before) != string(after) {
			t.Errorf("%s changed the committed pins", vector.name)
		}
		if got := len(vector.ops.removed) > 0; got != vector.removed {
			t.Errorf("%s cleanup attempted = %v, want %v", vector.name, got, vector.removed)
		}
		// A cleanup failure names the temporary path it could not remove; it never
		// claims the file is gone.
		if vector.ops.removeErr != nil && vector.name != "cleanup-absent" && !strings.Contains(err.Error(), vector.ops.temporary) {
			t.Errorf("%s cleanup failure does not name %q: %v", vector.name, vector.ops.temporary, err)
		}
		if vector.name == "collision" && len(vector.ops.removed) != 0 {
			t.Errorf("a failed exclusive create must not unlink a path it did not create (D19)")
		}
		if vector.name != "collision" && vector.ops.temporary == "" {
			t.Errorf("%s did not attempt a temporary file", vector.name)
		}
	}
}

func TestWritePinsValidatesBeforeWriting(t *testing.T) {
	prefix := fixture(t, "tiny-dsh")
	path := filepath.Join(string(prefix), "pnpm-workspace.yaml")
	if err := os.WriteFile(path, []byte("nope: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(string(prefix), pinFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePins(prefix, KindDsh); err == nil {
		t.Fatal("an invalid project was pinned")
	}
	after, err := os.ReadFile(filepath.Join(string(prefix), pinFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a failed pin changed the committed pins")
	}
}

func TestReadBoundedErrorShapePassesThrough(t *testing.T) {
	prefix := Prefix(filepath.Join(t.TempDir(), "absent"))
	_, err := ReadProject(prefix, KindDsh)
	var nodeError *nodefs.Error
	if err == nil || !asNodeError(err, &nodeError) {
		t.Fatalf("missing project = %v", err)
	}
	if nodeError.Errno != syscall.ENOENT {
		t.Errorf("errno = %v", nodeError.Errno)
	}
}

func pathError(operation string, errno syscall.Errno) error {
	return &os.PathError{Op: operation, Path: "/tmp/temporary", Err: errno}
}

func asNodeError(err error, target **nodefs.Error) bool {
	nodeError, ok := err.(*nodefs.Error)
	if ok {
		*target = nodeError
	}
	return ok
}
