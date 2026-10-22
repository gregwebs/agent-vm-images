//go:build linux

package toolaccess

import "syscall"

func makeFIFO(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
