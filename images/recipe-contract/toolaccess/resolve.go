package toolaccess

import (
	"fmt"
	"os"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/legacy"
)

// pathOverride is an effective PATH selected by a shebang `env` assignment.
// Unset and explicitly empty are different keys even though both resolve the
// same candidates.
type pathOverride struct {
	set   bool
	value string
}

var noPathOverride = pathOverride{}

func explicitPath(value string) pathOverride { return pathOverride{set: true, value: value} }

type visitKey struct {
	final   string
	envPath pathOverride
}

type waypointKind uint8

const (
	waypointEntry waypointKind = iota
	waypointLink
	waypointDirectory
	waypointParent
	waypointAbsolute
)

type waypoint struct {
	path string
	kind waypointKind
}

type rootMode uint8

const (
	finalComponent rootMode = iota
	directoryRoot
)

// resolver walks one path lexically, following symlinks hop by hop.
type resolver struct {
	cwd       func() (string, error)
	dirs      []string
	symlinks  int
	waypoints []waypoint
}

func (r *resolver) addDir(path string) { r.dirs = append(r.dirs, path) }

func (r *resolver) resolve(path string, mode rootMode) (string, os.FileInfo, error) {
	r.symlinks = 0
	r.waypoints = nil
	var components []string
	if strings.HasPrefix(path, "/") {
		components = strings.Split(path, "/")[1:]
	} else {
		// A relative path keeps its lexical components: cleaning first would
		// collapse a `..` before the directory it leaves is ever audited.
		cwd, err := r.cwd()
		if err != nil {
			return "", nil, err
		}
		components = append(strings.Split(cwd, "/")[1:], strings.Split(path, "/")...)
	}
	resolved := []string{""}
	pending := components
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) <= 1 {
				continue
			}
			r.addDir(strings.Join(resolved, "/"))
			r.waypoints = append(r.waypoints, waypoint{path: joinOrSlash(resolved), kind: waypointDirectory})
			resolved = resolved[:len(resolved)-1]
			r.waypoints = append(r.waypoints, waypoint{path: joinOrSlash(resolved), kind: waypointParent})
			continue
		}
		candidate := joinOrSlash(append(append([]string{}, resolved...), component))
		// The legacy helper's only lstat wrapper caught OSError, so a NUL byte
		// escaped as an uncaught ValueError instead of a "does not exist"
		// violation. Parity needs the same class, not a Go error.
		if err := legacy.CheckPath("lstat", candidate); err != nil {
			return "", nil, err
		}
		info, err := os.Lstat(candidate)
		if err != nil {
			return "", nil, violation(candidate, "does not exist")
		}
		kind := waypointEntry
		if info.Mode()&os.ModeSymlink != 0 {
			kind = waypointLink
		}
		r.waypoints = append(r.waypoints, waypoint{path: candidate, kind: kind})
		if info.Mode()&os.ModeSymlink != 0 {
			r.symlinks++
			if r.symlinks > MaxSymlinks {
				return "", nil, violation(candidate, fmt.Sprintf("symlink chain exceeds %d links (cycle?)", MaxSymlinks))
			}
			r.addDir(joinOrSlash(resolved))
			if err := legacy.CheckPath("readlink", candidate); err != nil {
				return "", nil, err
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", nil, err
			}
			parts := strings.Split(target, "/")
			if strings.HasPrefix(target, "/") {
				r.waypoints = append(r.waypoints, waypoint{path: "/", kind: waypointAbsolute})
				resolved = []string{""}
				parts = parts[1:]
			}
			next := make([]string, 0, len(parts)+len(pending))
			for _, part := range parts {
				if part != "" {
					next = append(next, part)
				}
			}
			pending = append(next, pending...)
			continue
		}
		if len(pending) > 0 {
			if !info.IsDir() {
				return "", nil, violation(candidate, "is not a directory")
			}
			r.addDir(candidate)
			resolved = append(resolved, component)
			continue
		}
		return candidate, info, nil
	}
	if mode == directoryRoot {
		final := joinOrSlash(resolved)
		if err := legacy.CheckPath("lstat", final); err != nil {
			return "", nil, err
		}
		info, err := os.Lstat(final)
		if err != nil {
			return "", nil, violation(final, "does not exist")
		}
		return final, info, nil
	}
	return "", nil, violation(path, "resolves to no final component")
}

func joinOrSlash(components []string) string {
	joined := strings.Join(components, "/")
	if joined == "" {
		return "/"
	}
	return joined
}

func checkDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return violation(path, "directory does not exist")
	}
	mode := unixMode(info)
	if !info.IsDir() {
		return violation(path, "is not a directory")
	}
	if mode&allExecute != allExecute {
		return violation(path, fmt.Sprintf("directory is not traversable by all (mode %o)", mode&permissionBits))
	}
	return checkACL(path)
}

func treeDirectory(path string) error {
	if err := checkDir(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return violation(path, "does not exist")
	}
	if unixMode(info)&allReadExecute != allReadExecute {
		return violation(path, "directory is not readable/traversable by all")
	}
	return nil
}

func (a *Auditor) resolveCommand(command string, envPath pathOverride) (string, error) {
	if strings.Contains(command, "/") {
		return command, nil
	}
	pathValue := string(a.env.Path)
	if envPath.set {
		pathValue = envPath.value
	}
	for _, directory := range strings.Split(pathValue, ":") {
		if directory == "" {
			directory = "."
		}
		candidate := pythonJoin(directory, command)
		// A PATH directory or override may carry a NUL byte; the legacy lookup
		// lstat raised ValueError for it instead of trying the next candidate.
		if err := legacy.CheckPath("lstat", candidate); err != nil {
			return "", err
		}
		if _, err := os.Lstat(candidate); err == nil {
			// The first existing candidate is THE command; a later executable is
			// never substituted for a present-but-unusable one.
			return candidate, nil
		}
	}
	return "", violation(command, "not found on PATH")
}

// pythonJoin reproduces posixpath.join so that an empty command or a trailing
// slash keeps the legacy spelling instead of being cleaned.
func pythonJoin(directory, name string) string {
	if strings.HasPrefix(name, "/") {
		return name
	}
	if directory == "" {
		return name
	}
	if strings.HasSuffix(directory, "/") {
		return directory + name
	}
	return directory + "/" + name
}

const (
	allExecute     uint32 = 0o111
	allRead        uint32 = 0o444
	allReadExecute        = allRead | allExecute
	permissionBits uint32 = 0o7777
)

// unixMode projects the Unix mode the legacy helper formatted with %o. Go's
// FileMode carries the special bits separately, so they are reassembled here.
func unixMode(info os.FileInfo) uint32 {
	mode := uint32(info.Mode().Perm())
	if info.Mode()&os.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if info.Mode()&os.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if info.Mode()&os.ModeSticky != 0 {
		mode |= 0o1000
	}
	return mode
}

func openViolation(path string, err error) error {
	return violation(path, "cannot read: "+osErrorText(err, path))
}

func readViolation(path string, err error) error {
	return violation(path, "cannot read: "+osErrorText(err, ""))
}

func osErrorText(err error, filename string) string {
	errno, ok := legacy.ErrnoOf(err)
	if !ok {
		return err.Error()
	}
	return legacy.OSErrorText(errno, filename)
}

func (r *resolver) auditDirs() error {
	for _, directory := range r.dirs {
		if err := checkDir(directory); err != nil {
			return err
		}
	}
	return nil
}
