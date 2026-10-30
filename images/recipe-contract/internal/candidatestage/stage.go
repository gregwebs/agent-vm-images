// Package candidatestage captures the three unpinned inputs of a preparation
// candidate through an inherited descriptor.
//
// A preparation driver mints a private 0700 `pnpm-project-*` scratch directory
// under the fixed system temporary directory, opens it, and passes the
// descriptor to recipe-contract as `--scratch-fd`. This package independently
// establishes that the descriptor and the canonical path name the same
// same-UID directory, opens `candidate` beneath it without following links, and
// reads exactly three bounded regular files. Authority never comes from the
// spelling alone: the descriptor, path, device/inode agreement, ownership, mode
// and entry inventory must all agree before a byte is parsed.
//
// Descriptor-relative access uses os.Root, whose Unix implementation walks every
// component with openat(..., O_NOFOLLOW) and refuses root escapes; Go's syscall
// package no longer exposes openat on Darwin, so a hand-rolled trampoline would
// be the only alternative and would be less reviewable. The inherited descriptor
// still decides identity: the path-derived root must report the same device and
// inode before any byte is read.
//
// It is deliberately not an integrity sandbox against a same-UID concurrent
// writer. Its product is three captured buffers whose digests the caller
// rechecks before it writes pins or publishes anything.
//
//go:build linux || darwin

package candidatestage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
)

// InputNames are the only files a candidate may carry, in pin-file order.
var InputNames = []string{"package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"}

const (
	// scratchPrefix is the minted directory name the driver uses. The prefix is
	// not authority; it is one more fact that must agree with the descriptor.
	scratchPrefix = "pnpm-project-"
	// candidateDir is the fixed 0700 child the driver creates for import output.
	candidateDir = "candidate"
	// scratchMode and candidateMode are the only accepted directory modes.
	directoryMode = 0o700
)

// File is one captured candidate input.
type File struct {
	Name  string
	Bytes []byte
}

// Stage is a captured candidate: three files and the scratch path they came from.
type Stage struct {
	scratch string
	fd      int
	files   []File
}

// Scratch is the canonical absolute scratch directory the stage was captured
// from.
func (s Stage) Scratch() string { return s.scratch }

// File returns the captured bytes for one input name.
func (s Stage) File(name string) ([]byte, bool) {
	for _, file := range s.files {
		if file.Name == name {
			return file.Bytes, true
		}
	}
	return nil, false
}

// Files returns the captured inputs in InputNames order.
func (s Stage) Files() []File { return s.files }

// Recheck re-captures the stage through the same descriptor and fails unless the
// bytes are identical. The driver runs it after its comparisons and before it
// writes pins, so a candidate edited mid-preparation cannot be pinned under the
// digests that were compared.
func (s Stage) Recheck() error {
	fresh, err := Open(s.scratch, s.fd)
	if err != nil {
		return err
	}
	for index, file := range s.files {
		if !bytes.Equal(file.Bytes, fresh.files[index].Bytes) {
			return errorf("%s: changed during candidate validation", file.Name)
		}
	}
	return nil
}

// Open validates the inherited descriptor and path and captures the three files.
func Open(scratch string, fd int) (Stage, error) {
	if fd <= 2 {
		return Stage{}, errorf("scratch fd must be an inherited descriptor above 2")
	}
	var descriptor syscall.Stat_t
	if err := syscall.Fstat(fd, &descriptor); err != nil {
		return Stage{}, nodefs.Wrap("fstat", strconv.Itoa(fd), err)
	}
	if descriptor.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return Stage{}, errorf("scratch fd is not a directory")
	}
	if err := checkDirectoryFacts("scratch fd", &descriptor); err != nil {
		return Stage{}, err
	}
	clean, err := canonicalScratch(scratch)
	if err != nil {
		return Stage{}, err
	}
	if err := checkNoSymlinkAncestry(clean); err != nil {
		return Stage{}, err
	}
	pathInfo, err := os.Lstat(clean)
	if err != nil {
		return Stage{}, nodefs.Wrap("lstat", clean, err)
	}
	if !pathInfo.IsDir() {
		return Stage{}, errorf("scratch path is not a directory")
	}
	if err := checkPathFacts("scratch path", clean, pathInfo); err != nil {
		return Stage{}, err
	}
	if err := checkAgreement(clean, pathInfo, &descriptor); err != nil {
		return Stage{}, err
	}
	root, err := os.OpenRoot(clean)
	if err != nil {
		return Stage{}, fmt.Errorf("candidate stage: cannot open scratch root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := checkRootAgreement(clean, root, &descriptor); err != nil {
		return Stage{}, err
	}
	candidateInfo, err := root.Lstat(candidateDir)
	if err != nil {
		return Stage{}, nodefs.Wrap("lstat", filepath.Join(clean, candidateDir), err)
	}
	if !candidateInfo.IsDir() {
		return Stage{}, errorf("candidate is not a directory")
	}
	if err := checkPathFacts("candidate", candidateDir, candidateInfo); err != nil {
		return Stage{}, err
	}
	candidate, err := root.OpenRoot(candidateDir)
	if err != nil {
		return Stage{}, fmt.Errorf("candidate stage: cannot open candidate: %w", err)
	}
	defer func() { _ = candidate.Close() }()
	candidateStat, err := syscallStat(candidateInfo)
	if err != nil {
		return Stage{}, err
	}
	if err := checkRootAgreement(filepath.Join(clean, candidateDir), candidate, candidateStat); err != nil {
		return Stage{}, err
	}
	if err := checkEntries(candidate); err != nil {
		return Stage{}, err
	}
	files := make([]File, 0, len(InputNames))
	for _, name := range InputNames {
		source, err := readCandidateFile(candidate, name)
		if err != nil {
			return Stage{}, err
		}
		files = append(files, File{Name: name, Bytes: source})
	}
	return Stage{scratch: clean, fd: fd, files: files}, nil
}

// SystemTempDir is the one fixed scratch parent. Darwin's /tmp is a symlink to
// /private/tmp, so the only canonical spelling there is /private/tmp.
func SystemTempDir() string {
	if runtime.GOOS == "darwin" {
		return "/private/tmp"
	}
	return "/tmp"
}

func canonicalScratch(scratch string) (string, error) {
	if scratch == "" || !filepath.IsAbs(scratch) {
		return "", errorf("scratch must be an absolute path")
	}
	clean := filepath.Clean(scratch)
	if clean != scratch {
		return "", errorf("scratch must be a canonical absolute path")
	}
	if filepath.Dir(clean) != SystemTempDir() {
		return "", errorf("scratch must be a direct child of %s", SystemTempDir())
	}
	if !strings.HasPrefix(filepath.Base(clean), scratchPrefix) {
		return "", errorf("scratch must be named %s*", scratchPrefix)
	}
	return clean, nil
}

func checkNoSymlinkAncestry(path string) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return nodefs.Wrap("lstat", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errorf("%s: symlinks are not permitted in the scratch path", current)
		}
		if current != path && !info.IsDir() {
			return errorf("%s: scratch ancestry is not a directory", current)
		}
	}
	return nil
}

func checkPathFacts(label, path string, info os.FileInfo) error {
	stat, err := syscallStat(info)
	if err != nil {
		return err
	}
	return checkDirectoryFacts(label+" "+path, stat)
}

func checkDirectoryFacts(label string, stat *syscall.Stat_t) error {
	if int(stat.Uid) != os.Geteuid() {
		return errorf("%s: not owned by the invoking uid", label)
	}
	if stat.Mode&0o777 != directoryMode {
		return errorf("%s: mode must be 0700", label)
	}
	return nil
}

func checkAgreement(path string, info os.FileInfo, descriptor *syscall.Stat_t) error {
	pathStat, err := syscallStat(info)
	if err != nil {
		return err
	}
	if pathStat.Dev != descriptor.Dev || pathStat.Ino != descriptor.Ino {
		return errorf("%s: path and inherited descriptor disagree", path)
	}
	return nil
}

func checkRootAgreement(path string, root *os.Root, want *syscall.Stat_t) error {
	info, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("candidate stage: cannot stat %s: %w", path, err)
	}
	stat, err := syscallStat(info)
	if err != nil {
		return err
	}
	if stat.Dev != want.Dev || stat.Ino != want.Ino {
		return errorf("%s: path and retained descriptor disagree", path)
	}
	return nil
}

func syscallStat(info os.FileInfo) (*syscall.Stat_t, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errorf("cannot read the filesystem identity")
	}
	return stat, nil
}

func checkEntries(candidate *os.Root) error {
	directory, err := candidate.Open(".")
	if err != nil {
		return fmt.Errorf("candidate stage: cannot open candidate directory: %w", err)
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("candidate stage: cannot list candidate directory: %w", err)
	}
	allowed := map[string]bool{}
	for _, name := range InputNames {
		allowed[name] = true
	}
	seen := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !allowed[entry.Name()] {
			// Pins, node_modules, package-lock.json, npm-shrinkwrap.json, .npmrc
			// and every pnpm hook land here: an extra entry is a policy stop, never
			// a file this stage can authenticate.
			return errorf("unexpected candidate entry: %s", entry.Name())
		}
		seen = append(seen, entry.Name())
	}
	if len(seen) != len(InputNames) {
		missing := make([]string, 0, len(InputNames))
		for _, name := range InputNames {
			found := false
			for _, seenName := range seen {
				if seenName == name {
					found = true
				}
			}
			if !found {
				missing = append(missing, name)
			}
		}
		return errorf("missing candidate input: %s", strings.Join(missing, ", "))
	}
	return nil
}

func readCandidateFile(candidate *os.Root, name string) ([]byte, error) {
	path := filepath.Join(candidate.Name(), name)
	linkInfo, err := candidate.Lstat(name)
	if err != nil {
		return nil, nodefs.Wrap("lstat", path, err)
	}
	linkStat, err := syscallStat(linkInfo)
	if err != nil {
		return nil, err
	}
	if !linkInfo.Mode().IsRegular() || linkStat.Nlink != 1 {
		return nil, errorf("%s: candidate input must be a single-link regular file", name)
	}
	if int(linkStat.Uid) != os.Geteuid() {
		return nil, errorf("%s: candidate input is not owned by the invoking uid", name)
	}
	if linkStat.Mode&0o022 != 0 {
		return nil, errorf("%s: candidate input must not be group or other writable", name)
	}
	file, err := candidate.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, nodefs.Wrap("open", path, err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, nodefs.Wrap("fstat", path, err)
	}
	openedStat, err := syscallStat(opened)
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || openedStat.Nlink != 1 || openedStat.Dev != linkStat.Dev || openedStat.Ino != linkStat.Ino {
		return nil, errorf("%s: candidate input changed between name and open", name)
	}
	if opened.Size() > nodefs.MaxBytes {
		return nil, errorf("%s: candidate input exceeds the 16 MiB bound", name)
	}
	source, err := io.ReadAll(io.LimitReader(file, nodefs.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(source)) > nodefs.MaxBytes {
		return nil, errors.New("input grew beyond size bound")
	}
	if int64(len(source)) != opened.Size() {
		return nil, errorf("%s: candidate input changed while it was read", name)
	}
	return source, nil
}

func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }
