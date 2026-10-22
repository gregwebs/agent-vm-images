//go:build linux || darwin

package toolaccess

import (
	"os"
	"syscall"
)

type fileIdentity struct {
	dev uint64
	ino uint64
}

func fileIdentityFromInfo(info os.FileInfo) (fileIdentity, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{dev: deviceNumber(stat), ino: stat.Ino}, true
}
