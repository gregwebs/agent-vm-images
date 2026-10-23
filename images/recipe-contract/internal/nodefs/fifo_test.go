//go:build linux || darwin

package nodefs

import (
	"strings"
	"syscall"
	"testing"
)

// A FIFO must be rejected by lstat before the blocking open: the read is
// O_NONBLOCK, but the mode check is what makes the refusal deterministic.
// mkfifo exists on both legs this module targets (linux images, darwin hosts).
func TestReadBoundedRejectsFIFO(t *testing.T) {
	path := t.TempDir() + "/fifo"
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadBounded(path); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("fifo = %v", err)
	}
}
