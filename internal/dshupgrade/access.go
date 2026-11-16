package dshupgrade

// W_OK and X_OK are the same permission bits on every platform this helper runs
// on; naming them locally keeps the syscall set explicit.
const (
	accessWrite   = 0x2
	accessExecute = 0x1
)
