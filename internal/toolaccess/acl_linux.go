//go:build linux

package toolaccess

import "syscall"

const posixACLAccess = "system.posix_acl_access"

// checkACL rejects a non-empty POSIX access ACL: mode bits alone cannot prove an
// ACL does not grant an arbitrary uid more. Every lookup error, including the
// ENODATA of an absent ACL and an unsupported filesystem, is ignored exactly as
// the legacy helper ignored every OSError and AttributeError. ADR 0004 records
// the accepted fail-open risk, including unexpected syscall failures.
func checkACL(path string) error {
	size, err := syscall.Getxattr(path, posixACLAccess, nil)
	if err != nil || size <= 0 {
		return nil
	}
	value := make([]byte, size)
	read, err := syscall.Getxattr(path, posixACLAccess, value)
	if err != nil || read <= 0 {
		return nil
	}
	return violation(path, "carries a POSIX access ACL")
}
