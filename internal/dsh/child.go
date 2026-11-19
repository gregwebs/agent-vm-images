package dsh

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// fileExists reports presence only. A dangling symlink is present, which is what
// `[ -f ]`/`[ -e ]` in the retired shells distinguished per site.
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// executableFile is the `[ -x ]` test: a regular file the caller may execute.
func executableFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// pathCandidate is the first PATH entry that holds name, with the lstat semantics
// `command -v` and `run-report.sh` use: a present-but-unusable candidate is the
// one found, never skipped in favour of a later executable.
func pathCandidate(name string) (string, bool) {
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			directory = "."
		}
		candidate := filepath.Join(directory, name)
		if fileExists(candidate) {
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
func signalNumber(status any) int {
	if wait, ok := status.(syscall.WaitStatus); ok {
		return int(wait.Signal())
	}
	return 0
}

// firstLine is `sed -n 1p`.
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return text[:index]
	}
	return text
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
