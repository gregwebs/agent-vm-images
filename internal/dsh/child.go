package dsh

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// pathExists is `[ -e ]`: it follows a symlink, so a dangling link is absent.
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// regularFile is `[ -f ]`: a regular file, reached through any symlink chain.
func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// executableFile is `[ -x ]` for the one shape every call site means: a regular
// file some uid may execute. It follows symlinks, and it is stricter than the shell
// test in exactly one way -- `[ -x ]` is also true for a directory, which no call
// site here could use.
func executableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// pathCandidate is the first PATH entry `command -v` would accept: the file exists
// once symlinks are followed and someone may execute it. `run-report.sh` resolves
// with lstat semantics of its own because it must report a present-but-unusable
// candidate instead of skipping to a later one; that rule stays in the shell.
func pathCandidate(name string) (string, bool) {
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, name)
		if executableFile(candidate) {
			return candidate, true
		}
	}
	return "", false
}

func hasDotComponent(value string) bool {
	for _, part := range strings.Split(value, string(filepath.Separator)) {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

// signalNumber reads the terminating signal out of a wait status.
func signalNumber(wait syscall.WaitStatus) int { return int(wait.Signal()) }

// firstLine is `sed -n 1p`.
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
}

// refuseRuntimeInjection rejects a Node preload or an injected module path, which
// would make every later check untrustworthy, before any Node process starts. The
// prefix is the caller's own header: the gate indents its lines under `dsh: `.
func refuseRuntimeInjection(prefix string) error {
	for _, key := range []string{"NODE_OPTIONS", "NODE_PATH", "BUN_OPTIONS"} {
		if os.Getenv(key) != "" {
			return refusal(prefix + key + " is not supported")
		}
	}
	return nil
}

// say writes one progress or diagnostic line. A write failure on the caller's own
// stream is reported by the stream layer above -- the CLI turns an unusable stderr
// into the legacy 120 -- so it is not an error this program can act on.
func say(destination io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(destination, format, args...)
}

// writeFile publishes one diagnostic or record file. The bytes are the contract, so
// the mode is fixed rather than inherited from the caller's umask.
func writeFile(path, text string) error {
	return os.WriteFile(path, []byte(text), 0o644)
}
