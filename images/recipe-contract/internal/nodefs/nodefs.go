// Package nodefs reproduces the file probes the retired Node helpers used: a
// bounded read that refuses anything but a regular file, and errno-shaped
// diagnostics the callers and their tests still match on.
package nodefs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// MaxBytes is the lock-file bound the Node helper enforced with Buffer.alloc.
const MaxBytes = 16 << 20

// Error is a Node fs diagnostic: `CODE: description, syscall 'path'`. The
// description is Go's errno text rather than libuv's, which is the documented
// diagnostic difference D11; the code, syscall and path shape are kept because
// those are what callers and fixtures inspect.
type Error struct {
	Errno   syscall.Errno
	Syscall string
	Path    string
	Dest    string
}

func (e *Error) Error() string {
	code, description := errnoText(e.Errno)
	if e.Dest != "" {
		return fmt.Sprintf("%s: %s, %s '%s' -> '%s'", code, description, e.Syscall, e.Path, e.Dest)
	}
	return fmt.Sprintf("%s: %s, %s '%s'", code, description, e.Syscall, e.Path)
}

// Unwrap keeps the errno available to callers that branch on it.
func (e *Error) Unwrap() error { return e.Errno }

func errnoText(errno syscall.Errno) (string, string) {
	switch errno { //nolint:exhaustive // syscall.Errno is an open platform set; the default names the rest.
	case syscall.ENOENT:
		return "ENOENT", "no such file or directory"
	case syscall.EACCES:
		return "EACCES", "permission denied"
	case syscall.EPERM:
		return "EPERM", "operation not permitted"
	case syscall.ENOTDIR:
		return "ENOTDIR", "not a directory"
	case syscall.EISDIR:
		return "EISDIR", "illegal operation on a directory"
	case syscall.ELOOP:
		return "ELOOP", "too many symbolic links encountered"
	case syscall.ENAMETOOLONG:
		return "ENAMETOOLONG", "name too long"
	case syscall.EEXIST:
		return "EEXIST", "file already exists"
	case syscall.EIO:
		return "EIO", "i/o error"
	case syscall.EXDEV:
		return "EXDEV", "cross-device link not permitted"
	default:
		return fmt.Sprintf("E%s", errno.Error()), errno.Error()
	}
}

// Wrap converts a Go path error into the Node shape. Anything else is returned
// unchanged so the caller keeps the original diagnostic.
func Wrap(syscallName, path string, err error) error {
	var pathError *os.PathError
	var linkError *os.LinkError
	switch {
	case errors.As(err, &pathError):
		var errno syscall.Errno
		if !errors.As(pathError.Err, &errno) {
			return err
		}
		return &Error{Errno: errno, Syscall: syscallName, Path: path}
	case errors.As(err, &linkError):
		var errno syscall.Errno
		if !errors.As(linkError.Err, &errno) {
			return err
		}
		return &Error{Errno: errno, Syscall: syscallName, Path: linkError.Old, Dest: linkError.New}
	default:
		return err
	}
}

// ReadBounded returns the bytes of a regular file of at most MaxBytes. Opening
// with O_NOFOLLOW and O_NONBLOCK keeps a symlink or FIFO swap between lstat and
// open from redirecting or blocking the read.
func ReadBounded(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, Wrap("lstat", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: must be a regular file", path)
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, Wrap("open", path, err)
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, Wrap("fstat", path, err)
	}
	if !opened.Mode().IsRegular() || opened.Size() > MaxBytes {
		return nil, fmt.Errorf("%s: must be a regular file <=16 MiB", path)
	}
	return readCapped(file, opened.Size())
}

// readCapped reproduces the Node read loop: allocate size+1 bytes, read until
// the buffer is full or the input ends, and reject input that outgrew the
// size the fstat recorded.
func readCapped(reader io.Reader, size int64) ([]byte, error) {
	buffer := make([]byte, size+1)
	length := 0
	for length < len(buffer) {
		read, err := reader.Read(buffer[length:])
		length += read
		if err != nil {
			break
		}
		if read == 0 {
			break
		}
	}
	if int64(length) > size {
		return nil, errors.New("input grew beyond size bound")
	}
	return buffer[:length], nil
}

// Present reports whether path exists at all, including as a symlink.
func Present(path string) (bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return false, nil
		}
		return false, Wrap("lstat", path, err)
	}
	return true, nil
}
