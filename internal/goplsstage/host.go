package goplsstage

// Resolving the host compiler. The writer must not use `os/exec` for this: it starts no
// program here, and the guarded-import register reads a program-start capability rather
// than a path search. What the writer needs is the `go` it will then name to the child
// environment, so the search is written down once, in the same "first PATH entry" shape
// package gotool resolves the child's program from.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveGo is the maintainer host's `go`: the first executable `go` on this process's
// PATH, made absolute. An empty PATH entry is skipped rather than read as the current
// directory, because a writer that ran `go` out of whatever directory it was started in
// would be a different program than the one a maintainer selected.
func ResolveGo() (string, error) {
	for _, entry := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if entry == "" {
			continue
		}
		candidate := filepath.Join(entry, "go")
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		if filepath.IsAbs(candidate) {
			return candidate, nil
		}
		working, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot resolve %s: %v", candidate, err)
		}
		return filepath.Join(working, candidate), nil
	}
	return "", fmt.Errorf("missing prerequisite: go")
}
