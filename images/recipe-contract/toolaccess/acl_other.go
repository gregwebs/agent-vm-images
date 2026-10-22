//go:build !linux

package toolaccess

// checkACL is a no-op where the legacy helper's xattr call could not succeed:
// macOS carries no Linux POSIX access ACL entry under that name.
func checkACL(path string) error { return nil }
