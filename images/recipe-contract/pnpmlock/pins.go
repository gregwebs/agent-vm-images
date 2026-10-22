package pnpmlock

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/nodefs"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/tempname"
)

// pinOps is the publication seam: tests inject open, short-write, close, rename
// and cleanup failures instead of trying to provoke them on a real filesystem.
type pinOps interface {
	OpenExclusive(path string) (pinHandle, error)
	Rename(from, to string) error
	Remove(path string) error
}

type pinHandle interface {
	Write(source []byte) (int, error)
	Close() error
}

type realPinOps struct{}

func (realPinOps) OpenExclusive(path string) (pinHandle, error) {
	// O_EXCL is the ownership proof; the mode is requested, not forced, so the
	// process umask still applies exactly as it did to the Node writer.
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

func (realPinOps) Rename(from, to string) error { return os.Rename(from, to) }

func (realPinOps) Remove(path string) error { return os.Remove(path) }

// WritePins validates the project with pins skipped, then publishes the reviewed
// input digests. Nothing is written until the whole project has passed.
func WritePins(prefix Prefix, kind Kind) error {
	return writePins(prefix, kind, realPinOps{})
}

func writePins(prefix Prefix, kind Kind, ops pinOps) error {
	if _, err := readProjectInputs(prefix, kind, skipPins, readLocked); err != nil {
		return err
	}
	body := &strings.Builder{}
	for _, file := range PinnedInputs {
		digest, err := inputDigest(prefix, file)
		if err != nil {
			return err
		}
		fmt.Fprintf(body, "%s  %s\n", digest, file)
	}
	destination := filepath.Join(string(prefix), pinFile)
	previous, err := os.Lstat(destination)
	if err != nil {
		if !os.IsNotExist(err) {
			return nodefs.Wrap("lstat", destination, err)
		}
	} else if !previous.Mode().IsRegular() {
		return errorf("pin destination must be a regular file")
	}
	identity, err := tempname.UUID4()
	if err != nil {
		return err
	}
	temporary := filepath.Join(string(prefix), ".pnpm-pins-"+identity)
	handle, err := ops.OpenExclusive(temporary)
	if err != nil {
		// A failed exclusive create is not evidence that this invocation owns the
		// name, so the pre-existing path is left alone (D19).
		return nodefs.Wrap("open", temporary, err)
	}
	// From here the temporary file is invocation-owned: every failure path tries
	// to remove it, and a cleanup failure is the diagnostic that wins, matching
	// the retired `finally`.
	if err := writeAll(handle, []byte(body.String())); err != nil {
		_ = handle.Close()
		return cleanupFailure(ops, temporary, err)
	}
	if err := handle.Close(); err != nil {
		return cleanupFailure(ops, temporary, nodefs.Wrap("close", temporary, err))
	}
	if err := ops.Rename(temporary, destination); err != nil {
		return cleanupFailure(ops, temporary, nodefs.Wrap("rename", destination, err))
	}
	return nil
}

func writeAll(handle pinHandle, source []byte) error {
	written := 0
	for written < len(source) {
		count, err := handle.Write(source[written:])
		written += count
		if err != nil {
			return err
		}
		if count == 0 {
			return errorf("short write")
		}
	}
	return nil
}

func cleanupFailure(ops pinOps, temporary string, cause error) error {
	if err := ops.Remove(temporary); err != nil {
		return nodefs.Wrap("unlink", temporary, err)
	}
	return cause
}
