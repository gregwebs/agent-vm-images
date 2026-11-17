package codewriter

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

// stagedFile is one proposal file as it was read back from a staging directory.
type stagedFile struct {
	name string
	raw  []byte
}

// stageRoot is an opened staging directory: the inode every write and read of a
// proposal is anchored to.
type stageRoot struct {
	path string
	root *os.Root
}

func (s stageRoot) close() { _ = s.root.Close() }

// openStageDirectory opens the staging directory itself, following no symlink to it.
//
// Validation and writing must be the *same* operation on the *same* inode, so the
// directory is opened through os.Root from its parent and every later create happens
// relative to that handle. A path that is swapped for a symlink (or for another
// directory) after the emptiness check can therefore no longer receive a write, and
// requireSameDirectory reports the swap afterwards so the caller never publishes
// something it did not inspect.
//
// The directory must be a real directory this invocation owns and no other user can
// reach (0700): a shared `$TMPDIR` entry is somebody else's staging area.
func openStageDirectory(path string, create bool) (stageRoot, []string, error) {
	directory, err := absolute(path)
	if err != nil {
		return stageRoot{}, nil, err
	}
	if filepath.Dir(directory) == directory {
		return stageRoot{}, nil, refusal("%s is not a staging directory", directory)
	}
	if create {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return stageRoot{}, nil, refusal("cannot create %s: %s", directory, err)
		}
	}
	parent, err := os.OpenRoot(filepath.Dir(directory))
	if err != nil {
		return stageRoot{}, nil, refusal("cannot open %s: %s", filepath.Dir(directory), err)
	}
	defer func() { _ = parent.Close() }()
	// os.Root follows a symlink that stays inside its root, so opening the staging
	// directory is not by itself a symlink refusal: a caller's "directory" that is a
	// link to a sibling would be opened, and the four evidence writes would land in
	// the link's target before anything noticed. The link is refused here, and the
	// inode is proven to be the one the pathname names *before* the first write, so
	// a swap in the window between the two is still refused with nothing written.
	if info, err := parent.Lstat(filepath.Base(directory)); err == nil &&
		info.Mode()&os.ModeSymlink != 0 {
		return stageRoot{}, nil, refusal("%s is not a real directory (a symlink is "+
			"refused as a staging directory)", directory)
	}
	opened, err := parent.OpenRoot(filepath.Base(directory))
	if err != nil {
		return stageRoot{}, nil, openRefusal(directory, parent, err)
	}
	stage := stageRoot{path: directory, root: opened}
	if err := requireSameDirectory(directory, stage.root); err != nil {
		stage.close()
		return stageRoot{}, nil, err
	}
	entries, err := checkStagingDirectory(stage)
	if err != nil {
		stage.close()
		return stageRoot{}, nil, err
	}
	return stage, entries, nil
}

// openRefusal names *why* the open failed, without trusting the reason to decide
// whether the operation is allowed: an open that failed is refused whatever it was.
func openRefusal(directory string, parent *os.Root, cause error) error {
	info, err := parent.Lstat(filepath.Base(directory))
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return refusal("%s is not a real directory (a symlink or a non-directory is "+
			"refused as a staging directory): %s", directory, cause)
	}
	return refusal("cannot open %s: %s", directory, cause)
}

// checkStagingDirectory is ownership, mode and the entry inventory of the opened
// inode, never of the path.
func checkStagingDirectory(stage stageRoot) ([]string, error) {
	info, err := stage.root.Stat(".")
	if err != nil {
		return nil, refusal("cannot stat %s: %s", stage.path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, refusal("cannot read the filesystem identity of %s", stage.path)
	}
	if int(stat.Uid) != os.Geteuid() {
		return nil, refusal("%s is owned by uid %d, not by this invocation (uid %d)",
			stage.path, stat.Uid, os.Geteuid())
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return nil, refusal("%s is mode %04o; a staging directory must be private to "+
			"this user (0700)", stage.path, mode)
	}
	return directoryEntries(stage.root, stage.path)
}

func directoryEntries(root *os.Root, label string) ([]string, error) {
	handle, err := root.Open(".")
	if err != nil {
		return nil, refusal("cannot list %s: %s", label, err)
	}
	defer func() { _ = handle.Close() }()
	entries, err := handle.ReadDir(-1)
	if err != nil {
		return nil, refusal("cannot list %s: %s", label, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

// requireSameDirectory proves the pathname still names the inode every write or read
// went to.
func requireSameDirectory(directory string, root *os.Root) error {
	opened, err := root.Stat(".")
	if err != nil {
		return refusal("%s disappeared while it was used: %s", directory, err)
	}
	current, err := os.Lstat(directory)
	if err != nil {
		return refusal("%s disappeared while it was used: %s", directory, err)
	}
	if !os.SameFile(opened, current) {
		return refusal("%s was replaced while it was in use; the operation stayed "+
			"anchored to the directory that was checked, so the new path was neither "+
			"read nor written", directory)
	}
	return nil
}

// requireOwnedDirectory is an absent or empty, private, non-symlink directory this
// invocation may fill.
//
// Only a directory this invocation created, or one the caller deliberately handed over
// empty, is written to. Recursively erasing a caller's directory would delete
// unrelated files with no backup and no record of what was lost.
func requireOwnedDirectory(path string) (string, error) {
	directory, err := absolute(path)
	if err != nil {
		return "", err
	}
	stage, entries, err := openStageDirectory(directory, true)
	if err != nil {
		return "", err
	}
	stage.close()
	if len(entries) > 0 {
		return "", refusal("%s is not empty (%v); stage into an absent or empty directory",
			directory, entries)
	}
	return directory, nil
}

// writeStage writes the fixed proposal into a staging directory the caller owns.
func writeStage(path string, files []proposalFile) error {
	directory, err := requireOwnedDirectory(path)
	if err != nil {
		return err
	}
	stage, _, err := openStageDirectory(directory, false)
	if err != nil {
		return err
	}
	defer stage.close()
	// The child proposal directory is created through the verified handle and never
	// reused: a pre-planted `evidence` (a symlink, say) would otherwise redirect the
	// four evidence writes.
	if err := stage.root.Mkdir(evidenceName, 0o700); err != nil {
		return refusal("cannot create %s as a new directory: %s",
			filepath.Join(directory, evidenceName), err)
	}
	evidenceRoot, err := openChild(stage.root, evidenceName,
		filepath.Join(directory, evidenceName))
	if err != nil {
		return err
	}
	defer func() { _ = evidenceRoot.Close() }()
	for _, file := range files {
		name, err := stageNameOf(file.destination)
		if err != nil {
			return err
		}
		head, rest, nested := strings.Cut(name, "/")
		if nested && head == evidenceName && rest != "" {
			if err := writeStageFile(evidenceRoot, rest, file.raw); err != nil {
				return err
			}
			continue
		}
		if err := writeStageFile(stage.root, name, file.raw); err != nil {
			return err
		}
	}
	return requireSameDirectory(directory, stage.root)
}

// writeStageFile creates exactly one proposal file, exclusively.
func writeStageFile(root *os.Root, name string, raw []byte) error {
	handle, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return refusal("cannot create %s in the staging directory: %s", name, err)
	}
	defer func() { _ = handle.Close() }()
	if _, err := handle.Write(raw); err != nil {
		return refusal("cannot write %s in the staging directory: %s", name, err)
	}
	return nil
}

// readStage reads the fixed proposal through verified handles, never via the path.
//
// The handle is what makes the read safe: a directory swapped for a symlink or for
// another directory between the check and the read cannot supply different bytes, and
// requireSameDirectory still reports the swap.
func readStage(path string, names []string) ([]stagedFile, error) {
	directory, err := absolute(path)
	if err != nil {
		return nil, err
	}
	stage, _, err := openStageDirectory(directory, false)
	if err != nil {
		return nil, err
	}
	defer stage.close()
	var evidenceRoot *os.Root
	if opened, err := openChild(stage.root, evidenceName,
		filepath.Join(directory, evidenceName)); err == nil {
		evidenceRoot = opened
		defer func() { _ = evidenceRoot.Close() }()
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	read := make([]stagedFile, 0, len(names))
	missing := make([]string, 0, 1)
	for _, name := range names {
		head, rest, nested := strings.Cut(name, "/")
		root := stage.root
		entry := head
		if nested && head == evidenceName {
			root, entry = evidenceRoot, rest
		}
		label := filepath.Join(directory, filepath.FromSlash(name))
		if root == nil {
			missing = append(missing, name)
			continue
		}
		raw, err := readStageFile(root, entry, label)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				missing = append(missing, name)
				continue
			}
			return nil, err
		}
		read = append(read, stagedFile{name: name, raw: raw})
	}
	if len(missing) > 0 {
		return nil, refusal("the staged proposal is incomplete: %v", missing)
	}
	if evidenceRoot != nil {
		unexpected, err := unexpectedEvidence(evidenceRoot, names)
		if err != nil {
			return nil, err
		}
		if len(unexpected) > 0 {
			return nil, refusal("the staged evidence set has unexpected entries: %v", unexpected)
		}
	}
	if err := requireSameDirectory(directory, stage.root); err != nil {
		return nil, err
	}
	return read, nil
}

// openChild opens one directory child of an opened staging directory, refusing a
// symlink: a pre-planted `evidence` link would otherwise redirect the four evidence
// writes, and os.Root follows a link that stays inside itself.
func openChild(root *os.Root, name, label string) (*os.Root, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, refusal("%s is not a real directory (a symlink is refused)", label)
	}
	opened, err := root.OpenRoot(name)
	if err != nil {
		return nil, refusal("cannot open %s: %s", label, err)
	}
	inside, err := opened.Stat(".")
	if err != nil {
		_ = opened.Close()
		return nil, refusal("cannot read %s: %s", label, err)
	}
	if !os.SameFile(info, inside) {
		_ = opened.Close()
		return nil, refusal("%s was replaced while it was in use", label)
	}
	return opened, nil
}

func readStageFile(root *os.Root, name, label string) ([]byte, error) {
	// A staged file that is a symlink is not the file the transaction read: the
	// Python publisher opened every proposal file with O_NOFOLLOW, so this refusal is
	// part of the contract rather than a detail of the traversal.
	if info, err := root.Lstat(name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, refusal("%s is not a regular file (a symlink is refused)", label)
	}
	handle, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	info, err := handle.Stat()
	if err != nil {
		return nil, refusal("%s: %s", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, refusal("%s is not a regular file", label)
	}
	raw, err := io.ReadAll(handle)
	if err != nil {
		return nil, refusal("cannot read %s: %s", label, err)
	}
	return raw, nil
}

// unexpectedEvidence is every staged evidence entry the fixed layout does not name.
func unexpectedEvidence(evidenceRoot *os.Root, names []string) ([]string, error) {
	entries, err := directoryEntries(evidenceRoot, evidenceName)
	if err != nil {
		return nil, err
	}
	expected := make(map[string]bool, len(names))
	for _, name := range names {
		expected[name] = true
	}
	unexpected := make([]string, 0, 1)
	for _, entry := range entries {
		if !expected[evidenceName+"/"+entry] {
			unexpected = append(unexpected, entry)
		}
	}
	return unexpected, nil
}

// decodeStaged is the staged text, or a refusal: the proposal is refused, not a
// traceback, when a byte sequence is not the text the loader will read.
func decodeStaged(files []stagedFile, name string) (string, error) {
	for _, file := range files {
		if file.name != name {
			continue
		}
		if !utf8.Valid(file.raw) {
			return "", refusal("the staged %s is not valid UTF-8", name)
		}
		return string(file.raw), nil
	}
	return "", refusal("the staged proposal is incomplete: [%s]", name)
}

// absolute is an absolute path for every message and every recorded location.
func absolute(path string) (string, error) {
	resolved, err := filepath.Abs(path)
	if err != nil {
		return "", refusal("%s is not a usable path: %s", path, err)
	}
	return resolved, nil
}

// equalStrings is the mask comparison's equality: two views are the same file exactly
// when every line agrees.
func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
