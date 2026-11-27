// Package pathjoin appends one path element without normalising either side.
//
// It exists because `filepath.Join` cleans, and cleaning is not the same operation
// as resolving: `a/link/../gopls` is `b/gopls` to the kernel when `a/link` is a
// symlink to `b/sub`, while a lexical clean reads `a/gopls`. A program that verifies
// or loads something must use the path the caller named, or it acts on a file the
// caller never asked about. The Python readers this layer ported joined paths with no
// normalisation, so this is also the behaviour the suites compare against.
//
// Every caller that builds a path from a user-supplied string uses this and nothing
// else; paths this code invents for its own scratch tree may use filepath.Join,
// because no caller's meaning depends on their spelling.
package pathjoin

import (
	"os"
	"strings"
)

// Join appends name to directory, preserving both spellings.
func Join(directory, name string) string {
	if strings.HasSuffix(directory, string(os.PathSeparator)) {
		return directory + name
	}
	return directory + string(os.PathSeparator) + name
}
