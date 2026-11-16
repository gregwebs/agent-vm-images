package toolaccess

import (
	"fmt"
	"os"
	"strings"
)

// auditTree certifies a directory subtree beneath cwd. Every subordinate
// symlink hop must stay inside the authorized physical root; the root's own
// establishment is allowed to traverse outside ancestors.
func (a *Auditor) auditTree(target string) (string, error) {
	walk := &resolver{cwd: a.physicalCwd}
	root, info, err := walk.resolve(target, directoryRoot)
	if err != nil {
		return "", err
	}
	walk.addDir("/")
	if err := walk.auditDirs(); err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", violation(root, "tree root is not a directory")
	}
	pending := []string{root}
	visited := make(map[fileIdentity]struct{})
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		entryInfo, err := os.Lstat(current)
		if err != nil {
			return "", violation(current, "does not exist")
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			edge := &resolver{cwd: a.physicalCwd}
			final, resolvedInfo, err := edge.resolve(current, directoryRoot)
			if err != nil {
				return "", err
			}
			if err := edge.auditDirs(); err != nil {
				return "", err
			}
			if err := checkContainment(edge, root); err != nil {
				return "", err
			}
			if !within(root, final) {
				return "", violation(final, "symlink target escapes tree root")
			}
			current, entryInfo = final, resolvedInfo
		}
		if err := checkACL(current); err != nil {
			return "", err
		}
		switch {
		case entryInfo.IsDir():
			if err := treeDirectory(current); err != nil {
				return "", err
			}
			identity, ok := fileIdentityFromInfo(entryInfo)
			if ok {
				if _, seen := visited[identity]; seen {
					continue
				}
				visited[identity] = struct{}{}
			}
			entries, err := a.env.ReadDirectory(current)
			if err != nil {
				return "", violation(current, "cannot enumerate tree: "+osErrorText(err, current))
			}
			for _, name := range entries {
				pending = append(pending, pythonJoin(current, name))
			}
		case entryInfo.Mode().IsRegular():
			if unixMode(entryInfo)&allRead != allRead {
				return "", violation(current, fmt.Sprintf("not readable by all (mode %o)", unixMode(entryInfo)&permissionBits))
			}
		default:
			return "", violation(current, "special file in tree")
		}
	}
	return root, nil
}

// checkContainment rejects any waypoint that leaves the physical root, including
// an outside hop that later returns inside it.
func checkContainment(edge *resolver, root string) error {
	entered := false
	for _, point := range edge.waypoints {
		if point.kind == waypointAbsolute {
			// An absolute in-root path still traverses outside ancestors, but may
			// not resolve an outside link or leave the tree after entering it.
			entered = false
			continue
		}
		contained := within(root, point.path)
		if contained {
			entered = true
			continue
		}
		if entered || point.kind == waypointLink || point.kind == waypointParent ||
			!strings.HasPrefix(root, strings.TrimRight(point.path, "/")+"/") {
			return violation(point.path, "symlink waypoint escapes tree root")
		}
	}
	return nil
}

func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimRight(root, "/")+"/")
}

// readDirNative keeps the filesystem's own iteration order: sorting would change
// which failing sibling is reported first.
func readDirNative(path string) ([]string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	entries, err := handle.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}
