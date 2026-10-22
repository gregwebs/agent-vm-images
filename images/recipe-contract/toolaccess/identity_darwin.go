//go:build darwin

package toolaccess

import "syscall"

func deviceNumber(stat *syscall.Stat_t) uint64 { return uint64(uint32(stat.Dev)) }
