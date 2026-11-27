package closure

import (
	"os"
	"path/filepath"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/pathjoin"
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

// Locator is a Source whose bytes come from a directory the caller may name again.
// A source that cannot (an in-memory fixture) still loads; the caller then has no
// directory to re-read, and a verb that must re-read refuses instead of guessing.
type Locator interface {
	// Directory is the spelling the caller gave, unaltered.
	Directory() string
}

// Dir is the real directory source.
type Dir struct{ directory string }

// Directory is the caller's spelling of the data directory.
func (d *Dir) Directory() string { return d.directory }

// DirSource returns the source for one absolute-or-relative directory, in the
// spelling the caller wrote.
//
// A relative path is anchored to the working directory and nothing is normalised:
// filepath.Abs cleans, and cleaning is not resolving. With `a/link -> b/sub`, the
// caller's `a/link/../closure` is `b/closure` to the kernel and `a/closure` to a
// lexical clean, so a reader that cleaned could load a closure the caller never
// named. The Python reader this ported from used the path as written.
func DirSource(directory string) (Source, error) {
	if filepath.IsAbs(directory) {
		return &Dir{directory: directory}, nil
	}
	working, err := os.Getwd()
	if err != nil {
		return nil, refuse("cannot resolve %s: %v", directory, err)
	}
	return &Dir{directory: pathjoin.Join(working, directory)}, nil
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
	path := pathjoin.Join(d.directory, string(name))
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
