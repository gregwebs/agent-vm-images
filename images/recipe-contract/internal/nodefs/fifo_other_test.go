//go:build !linux

package nodefs

import "testing"

// A FIFO needs mkfifo, which is Linux-only in this module's test matrix; the
// Linux leg covers the refusal.
func TestReadBoundedRejectsFIFO(t *testing.T) { t.Skip("mkfifo coverage is Linux-only") }
