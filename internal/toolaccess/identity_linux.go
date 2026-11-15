//go:build linux

package toolaccess

import "syscall"

func deviceNumber(stat *syscall.Stat_t) uint64 { return uint64(stat.Dev) }
