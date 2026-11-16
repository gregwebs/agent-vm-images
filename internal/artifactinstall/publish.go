package artifactinstall

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
)

// rootPath is the install root as a path the filesystem walk can use.
func rootPath(root string) string {
	if root == "" {
		return string(filepath.Separator)
	}
	return root
}

// extractArchive runs the three archive stages in order: preflight every member,
// check the reviewed shape, then extract.
func extractArchive(options Options, archiveFile string, archive artifactpin.Archive,
	staging string) error {
	open := archiveOpener(archiveFile, archive.Format, options.Compress)
	members, err := membersOf(open)
	if err != nil {
		return err
	}
	byName, err := preflight(members, archive.MemberPrefix)
	if err != nil {
		return err
	}
	if err := checkMembers(options.Pin, byName); err != nil {
		return err
	}
	if err := extract(open, byName, staging, archive.MemberPrefix); err != nil {
		return err
	}
	if options.Events != nil {
		options.Events.extract(len(members))
	}
	return nil
}

// checkDestination requires every destination ancestor to be a real directory: a
// symlinked `root/usr/local` used to redirect the whole publication outside the
// root the caller named.
func checkDestination(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || strings.HasPrefix(relative, "..") {
		return refuse("%s is outside the install root %s", target, root)
	}
	current := root
	parts := strings.Split(relative, string(filepath.Separator))
	for _, part := range parts[:max(len(parts)-1, 0)] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return refuse("refusing to publish through the symlinked directory %s", current)
		}
		if !info.IsDir() {
			return refuse("refusing to publish through the file %s", current)
		}
	}
	return nil
}

// step is one planned publication: a directory or a leaf file.
type step struct {
	isDir  bool
	staged string
	target string
}

// planPublication is the complete list of directories and files publication
// would create. Collecting it first is what makes the precheck possible: a
// conflict in the *second* published file used to leave the first one installed.
func planPublication(staging, root string, destination artifactpin.Path,
	payload []artifactpin.Member) ([]step, error) {
	into := filepath.Join(root, strings.TrimPrefix(destination.String(), "/"))
	var plan []step
	var walk func(staged, target string) error
	walk = func(staged, target string) error {
		info, err := os.Lstat(staged)
		if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			plan = append(plan, step{isDir: true, staged: staged, target: target})
			children, err := os.ReadDir(staged)
			if err != nil {
				return err
			}
			sort.Slice(children, func(left, right int) bool {
				return children[left].Name() < children[right].Name()
			})
			for _, child := range children {
				if err := walk(filepath.Join(staged, child.Name()),
					filepath.Join(target, child.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		plan = append(plan, step{isDir: false, staged: staged, target: target})
		return nil
	}
	for _, entry := range payload {
		staged := filepath.Join(staging, entry.Archive.String())
		if _, err := os.Lstat(staged); err != nil {
			return nil, refuse("the reviewed archive carries no %s to publish", entry.Archive)
		}
		if err := walk(staged, filepath.Join(into, entry.Published.String())); err != nil {
			return nil, err
		}
	}
	for _, planned := range plan {
		if err := checkDestination(root, planned.target); err != nil {
			return nil, err
		}
		info, err := os.Lstat(planned.target)
		exists := err == nil
		if planned.isDir {
			if exists && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
				return nil, refuse("refusing to replace the existing %s with a directory", planned.target)
			}
			continue
		}
		if exists {
			return nil, refuse("refusing to replace an installed path: %s", planned.target)
		}
	}
	return plan, nil
}

// publish writes the prechecked plan and returns what was written, so a later
// failure can take it back out again.
func publish(root string, plan []step) ([]string, []string, error) {
	var files, directories []string
	fail := func(err error) ([]string, []string, error) {
		rollback(files, directories)
		return nil, nil, err
	}
	for _, planned := range plan {
		if err := checkDestination(root, planned.target); err != nil {
			return fail(err)
		}
		if planned.isDir {
			if _, err := os.Stat(planned.target); err != nil {
				if err := os.MkdirAll(planned.target, 0o755); err != nil {
					return fail(err)
				}
				directories = append(directories, planned.target)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(planned.target), 0o755); err != nil {
			return fail(err)
		}
		if err := os.Rename(planned.staged, planned.target); err != nil {
			// A cross-device target cannot be renamed into place; copy instead.
			if err := copyFile(planned.staged, planned.target); err != nil {
				return fail(err)
			}
		}
		files = append(files, planned.target)
	}
	return files, directories, nil
}

func copyFile(source, target string) error {
	raw, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, raw, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Remove(source)
}

// rollback undoes a partial publication. Best effort by design: the installer is
// failing anyway, and an unremovable path must not hide the original refusal.
func rollback(files, directories []string) {
	for index := len(files) - 1; index >= 0; index-- {
		_ = os.Remove(files[index])
	}
	for index := len(directories) - 1; index >= 0; index-- {
		_ = os.Remove(directories[index])
	}
}

// link publishes one reviewed symlink.
func link(root string, linkPath, target artifactpin.Path) (string, error) {
	destination := filepath.Join(root, strings.TrimPrefix(linkPath.String(), "/"))
	resolved := filepath.Join(root, strings.TrimPrefix(target.String(), "/"))
	if err := checkDestination(root, destination); err != nil {
		return "", err
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", refuse("refusing to replace an installed path: %s", destination)
	}
	if _, err := os.Lstat(resolved); err != nil {
		return "", refuse("link target is missing: %s", resolved)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink(target.String(), destination); err != nil {
		return "", err
	}
	return destination, nil
}
