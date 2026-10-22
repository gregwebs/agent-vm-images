// Package installstatus owns the recipe-contract install-record grammar and the
// absence predicate used to authorise a missing command.
//
// The rules are the whole point of this package: an absence record only
// authorises behaviour when the path is genuinely absent, and a record only
// authorises a transport failure when it is one exact, known line.
package installstatus

import (
	"errors"
	"os"
	"syscall"
)

// PathState is the closed answer to "may this path's absence be trusted?".
type PathState uint8

const (
	// Indeterminate is also the zero value: an uninitialised answer authorises
	// nothing. The walk refuses empty paths, exactly "/", and reached ".."
	// components, or cannot decide after a syscall failure.
	Indeterminate PathState = iota
	// Absent means the first missing directory entry was reached before any
	// indeterminate component.
	Absent
	// Present includes dangling leaf links and non-directory ancestors.
	Present
)

// ClassifyPath walks the path component by component. It deliberately does not
// use a whole-path stat: that collapses a broken ancestor and a missing leaf
// into the same ENOENT, which would let a dangling ancestor read as absence.
func ClassifyPath(path string) PathState {
	if path == "" || path == "/" {
		return Indeterminate
	}
	absolute := path[0] == '/'
	components := splitComponents(path)
	current := ""
	for index, component := range components {
		if component == "." {
			continue
		}
		if component == ".." {
			// Auditing the directory `..` leaves needs the prefix we discarded.
			// Refuse closed instead of guessing.
			return Indeterminate
		}
		if current == "" {
			if absolute {
				current = "/" + component
			} else {
				current = component
			}
		} else {
			current += "/" + component
		}
		info, err := os.Lstat(current)
		if err != nil {
			switch {
			case errors.Is(err, syscall.ENOENT):
				return Absent
			case errors.Is(err, syscall.ENOTDIR):
				return Present
			default:
				return Indeterminate
			}
		}
		last := index == len(components)-1
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if last {
				continue // the link itself is a directory entry: present
			}
			target, err := os.Stat(current)
			if err != nil {
				if errors.Is(err, syscall.ENOENT) {
					return Present // dangling ancestor symlink
				}
				return Indeterminate // ancestor symlink loop or unreadable target
			}
			if !target.IsDir() {
				return Present
			}
		case !last && !info.IsDir():
			return Present
		}
	}
	return Present
}

func splitComponents(path string) []string {
	components := make([]string, 0, 8)
	start := 0
	for index := 0; index <= len(path); index++ {
		if index == len(path) || path[index] == '/' {
			if index > start {
				components = append(components, path[start:index])
			}
			start = index + 1
		}
	}
	return components
}
