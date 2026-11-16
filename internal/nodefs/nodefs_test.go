package nodefs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestReadBoundedRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "file")
	if err := os.WriteFile(path, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := ReadBounded(path)
	if err != nil || string(source) != "payload" {
		t.Fatalf("read = %q, %v", source, err)
	}
}

func TestReadBoundedRejections(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "absent")
	if _, err := ReadBounded(missing); err == nil || !strings.Contains(err.Error(), "ENOENT: no such file or directory, lstat '"+missing+"'") {
		t.Errorf("missing = %v", err)
	}
	if _, err := ReadBounded(directory); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("directory = %v", err)
	}
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBounded(link); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Errorf("symlink = %v", err)
	}
}

func TestReadBoundedSizeBound(t *testing.T) {
	source, err := readCapped(strings.NewReader(strings.Repeat("a", 10)), 5)
	if err == nil || err.Error() != "input grew beyond size bound" {
		t.Fatalf("growth = %q, %v", source, err)
	}
	source, err = readCapped(strings.NewReader(strings.Repeat("a", 4)), 5)
	if err != nil || len(source) != 4 {
		t.Fatalf("short file = %q, %v", source, err)
	}
}

func TestPresent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "file")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if present, err := Present(path); err != nil || !present {
		t.Errorf("present = %v, %v", present, err)
	}
	if present, err := Present(filepath.Join(directory, "absent")); err != nil || present {
		t.Errorf("absent = %v, %v", present, err)
	}
	link := filepath.Join(directory, "dangling")
	if err := os.Symlink(filepath.Join(directory, "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	if present, err := Present(link); err != nil || !present {
		t.Errorf("dangling symlink = %v, %v", present, err)
	}
}

func TestErrorShapes(t *testing.T) {
	pathError := &os.PathError{Op: "lstat", Path: "/x", Err: syscall.ENOENT}
	if got := Wrap("lstat", "/x", pathError).Error(); got != "ENOENT: no such file or directory, lstat '/x'" {
		t.Errorf("path error = %s", got)
	}
	linkError := &os.LinkError{Op: "link", Old: "/a", New: "/b", Err: syscall.EEXIST}
	if got := Wrap("link", "/b", linkError).Error(); got != "EEXIST: file already exists, link '/a' -> '/b'" {
		t.Errorf("link error = %s", got)
	}
}

type failingReader struct{}

func (failingReader) Read(buffer []byte) (int, error) {
	return copy(buffer, "partial"), syscall.EIO
}

func TestReadCappedPreservesReadError(t *testing.T) {
	source, err := readCapped(failingReader{}, 32)
	if !errors.Is(err, syscall.EIO) || source != nil {
		t.Fatalf("read = %q, %v", source, err)
	}
}
