// Package evidence is how a pin reader reaches a committed or downloaded file.
//
// Every read is proven regular, reached without passing through a symlink and
// bounded, so a pin directory or a scratch directory that can redirect a read
// cannot substitute the bytes a reviewed certificate was issued for. Both pin
// families (Claude and Codex) read this way, so there is one implementation of the
// rule rather than one per owner.
package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Refusal is a committed input that is not what the reviewed pin says it is.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

// Refuse builds a Refusal.
func Refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a refusal rather than an I/O or programming
// failure, so a caller can map it onto the refusal status.
func IsRefusal(err error) bool {
	var refusal *Refusal
	return errors.As(err, &refusal)
}

// RegularFile is a regular, non-symlink file reached without passing through a
// symlink. Every committed input is reached this way: a pin directory that can
// redirect a read is not the reviewed closure.
func RegularFile(directory, relative, what string) (string, error) {
	parts := strings.Split(relative, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", Refuse("%s path %q is not a plain relative path", what, relative)
		}
	}
	current := directory
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", Refuse("%s is missing at %s", what, current)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", Refuse("%s is reached through the symlink %s", what, current)
		}
		if !info.IsDir() {
			return "", Refuse("%s ancestor %s is not a directory", what, current)
		}
	}
	path := filepath.Join(current, parts[len(parts)-1])
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", Refuse("%s is missing at %s", what, path)
		}
		return "", Refuse("%s cannot be read at %s: %s", what, path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", Refuse("%s at %s is not a regular file", what, path)
	}
	return path, nil
}

// ReadBounded reads a file already proven regular, refusing a swap to another
// filetype between the check and the read.
func ReadBounded(path string, limit int64, what string) ([]byte, error) {
	file, err := OpenNoFollow(path)
	if err != nil {
		return nil, Refuse("%s cannot be opened at %s: %s", what, path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, Refuse("%s cannot be read at %s: %s", what, path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, Refuse("%s at %s is not a regular file", what, path)
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, Refuse("%s cannot be read at %s: %s", what, path, err)
	}
	if int64(len(raw)) > limit {
		return nil, Refuse("%s is larger than the %d byte ceiling", what, limit)
	}
	return raw, nil
}

// OpenNoFollow opens a path without following a final symlink, where the platform
// has that flag.
func OpenNoFollow(path string) (*os.File, error) {
	flags := os.O_RDONLY | noFollowFlag
	return os.OpenFile(path, flags, 0)
}

// Sha256Of is the digest the pins record, in the case they record it in.
func Sha256Of(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// FileDigest is the SHA-256 and byte count of a regular file, read in chunks so a
// 340 MB payload never lands in memory twice.
func FileDigest(path string, limit int64, what string) (string, int64, error) {
	file, err := OpenNoFollow(path)
	if err != nil {
		return "", 0, Refuse("%s cannot be opened at %s: %s", what, path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", 0, Refuse("%s cannot be read at %s: %s", what, path, err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, Refuse("%s at %s is not a regular file", what, path)
	}
	hasher := sha256.New()
	buffer := make([]byte, ChunkBytes)
	var total int64
	for {
		read, err := file.Read(buffer)
		if read > 0 {
			total += int64(read)
			if total > limit {
				return "", 0, Refuse("%s is larger than the %d byte ceiling", what, limit)
			}
			hasher.Write(buffer[:read])
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", 0, Refuse("%s cannot be read at %s: %s", what, path, err)
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), total, nil
}

// ChunkBytes is the read size every streaming digest uses: one page-sized read, so
// a caller can never ask a decoder for a caller-sized allocation.
const ChunkBytes = 1 << 16
