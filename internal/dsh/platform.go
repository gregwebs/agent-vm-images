package dsh

import (
	"os"
	"runtime"
	"syscall"
)

// Usage is a malformed argument: the invocation is wrong, not the request. The
// predecessors exited 2 for it and 1 for a well-formed request they refused.
type Usage struct{ Text string }

func (u *Usage) Error() string { return u.Text }

// hostPlatform is `sys.platform`'s prefix: the ownership proof branches on Linux
// only because procfs is Linux only, and the diagnostic quotes the same spelling.
func hostPlatform() string { return runtime.GOOS }

// ownerOf is `stat().st_uid` without following a link.
func ownerOf(info os.FileInfo) int {
	if status, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(status.Uid)
	}
	return -1
}
