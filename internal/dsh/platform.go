package dsh

import (
	"os"
	"runtime"
	"syscall"
)

// Usage is a malformed argument: the invocation is wrong, not the request. The
// predecessors exited 2 for it and 1 for a well-formed request they refused.
//
// Bare marks text that is already the whole diagnostic line -- the documented
// `usage: prepare-lock.sh …` interface, which the shell printed without a program
// prefix. Everything else is a sentence the caller's own header introduces.
type Usage struct {
	Text string
	Bare bool
}

func (u *Usage) Error() string { return u.Text }

// hostPlatform is `sys.platform`'s prefix: the ownership proof branches on Linux
// only because procfs is Linux only, and the diagnostic quotes the same spelling.
func hostPlatform() string { return runtime.GOOS }

// ownerOf is `stat().st_uid`. It reports false rather than a sentinel when the
// host cannot name an owner, so a caller cannot compare -1 with the calling uid and
// silently agree.
func ownerOf(info os.FileInfo) (int, bool) {
	status, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(status.Uid), true
}

// callerOwns is the ownership predicate every scratch and temp check uses.
func callerOwns(info os.FileInfo) bool {
	uid, ok := ownerOf(info)
	return ok && uid == os.Getuid()
}
