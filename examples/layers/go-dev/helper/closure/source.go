package closure

import (
	"os"
	"path/filepath"
)

// Source is the committed data directory as the loader sees it: the input set and
// the bytes. It is the loader's only I/O, so a test can hand the same rules a
// tree it built in memory, and the shipped reader can only reach these five
// files.
type Source interface {
	// List returns every entry name in the directory.
	List() ([]string, error)
	// Read returns one input's bytes, refusing a symlink or a non-regular file.
	Read(name Filename) ([]byte, error)
}

// Dir is the real directory source.
type Dir struct{ directory string }

// DirSource returns the source for one absolute-or-relative directory. The
// directory is made absolute here, because a relative spelling would make two
// readers of one tree name it differently.
func DirSource(directory string) (Source, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	return &Dir{directory: absolute}, nil
}

// List refuses a path that is not a plain directory: a symlink to one is a
// different input set than the one the pin describes.
func (d *Dir) List() ([]string, error) {
	info, err := os.Lstat(d.directory)
	if err != nil || !info.IsDir() {
		return nil, refuse("not a directory: %s", d.directory)
	}
	entries, err := os.ReadDir(d.directory)
	if err != nil {
		return nil, refuse("cannot read %s: %v", d.directory, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

// Read refuses a symlink before reading it: `lstat` is the mode the pin binds, and
// a symlinked input would let the bytes come from anywhere.
func (d *Dir) Read(name Filename) ([]byte, error) {
	path := filepath.Join(d.directory, string(name))
	info, err := os.Lstat(path)
	if err != nil {
		return nil, refuse("missing input: %s", name)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, refuse("symlink %s", name)
	}
	if !info.Mode().IsRegular() {
		return nil, refuse("not a regular file: %s", name)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, refuse("cannot read %s: %v", name, err)
	}
	return content, nil
}
