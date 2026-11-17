//go:build !windows

package evidence

import "syscall"

// noFollowFlag keeps a read from following a final symlink that replaced a file
// between the lstat check and the open.
const noFollowFlag = syscall.O_NOFOLLOW
