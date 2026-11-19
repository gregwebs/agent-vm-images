package pnpm

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Published locations in a real image. The base owns all three: the prefix holds
// the authenticated upstream distribution, the command directory holds the one
// reviewed launcher, and the interpreter is the pinned Node artifact's entry.
const (
	ProductionPrefix   = "/opt/agent-vm/pnpm"
	ProductionCommands = "/usr/local/bin"
	ProductionNode     = "/usr/bin/node"
	SourceDirDefault   = "/tmp/pnpm-source"
)

// FixtureRootVariable names the invocation-owned directory a fixture install
// happens inside. AGENT_VM_FIXTURE_ROOT is the repository-wide fixture contract
// (every installer already isolates its redirects behind it), not a seam this
// program invented; the download seam is a command-line flag instead.
const FixtureRootVariable = "AGENT_VM_FIXTURE_ROOT"

// FixtureDestinationVariable selects the fixture image root that the bootstrap
// publishes into and the verifier certifies. It is the pre-existing contract
// script/test/fixtures and images/standard/verify-standard.sh already pass.
const FixtureDestinationVariable = "PNPM_FIXTURE_DESTINATION"

// SourceDirVariable names the read-only mount carrying the reviewed pin and
// launcher. The image builds and the dsh hooks both name it, so it is read from
// the environment rather than invented here.
const SourceDirVariable = "PNPM_SOURCE_DIR"

// SourceDir is the reviewed pin directory the caller mounted.
func SourceDir() string {
	if value := os.Getenv(SourceDirVariable); value != "" {
		return value
	}
	return SourceDirDefault
}

// FixtureDestination resolves a fixture image root into the two directories the
// distribution is published into.
//
// A fixture install is not a weaker production install: it is the same bytes in a
// root the invocation owns. So the root must be invocation-owned and must not be
// "/", the destination must be strictly inside it, and no existing descendant may
// redirect publication outside it -- otherwise the caller's own symlink could aim
// a chmod or a copy at a file the build never reviewed.
func FixtureDestination(destination string) (prefix, commands string, err error) {
	root := os.Getenv(FixtureRootVariable)
	if root == "" {
		return "", "", refuse("%s is not set", FixtureRootVariable)
	}
	fixture, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", refuseCause(err)
	}
	info, err := os.Stat(fixture)
	if err != nil {
		return "", "", refuseCause(err)
	}
	if fixture == string(filepath.Separator) || ownerUID(info) != os.Getuid() {
		return "", "", refuseText("fixture root must be invocation-owned")
	}
	resolved, err := filepath.EvalSymlinks(destination)
	if err != nil {
		return "", "", refuseCause(err)
	}
	if !strictlyContains(fixture, resolved) {
		return "", "", refuseText("destination outside fixture root")
	}
	prefix = filepath.Join(resolved, "opt/agent-vm/pnpm")
	commands = filepath.Join(resolved, "usr/local/bin")
	// Alias links are permitted for verification only while they stay confined to
	// the root, so each candidate descendant is checked with the physical
	// resolution of its own path and of every ancestor below the root.
	targets := []string{prefix, commands}
	for _, name := range commandNames {
		targets = append(targets, filepath.Join(commands, name))
	}
	for _, target := range targets {
		if err := checkDescendant(resolved, target); err != nil {
			return "", "", err
		}
	}
	return prefix, commands, nil
}

// commandNames is the published command set: the launcher plus its two aliases.
var commandNames = []string{"pnpm", "pnx", "pnpx"}

// checkDescendant walks one candidate path and each of its ancestors up to the
// fixture root. Both the lexical ancestor and its physical resolution must stay
// inside the root, so a link cannot aim a later write outside it.
func checkDescendant(root, target string) error {
	for path := target; path != root; path = filepath.Dir(path) {
		if path == filepath.Dir(path) {
			// The destination is not under the root at all; the caller's
			// containment check already refused that.
			break
		}
		physical, err := resolveLenient(path)
		if err != nil {
			return refuseCause(err)
		}
		if !strictlyContains(root, physical) {
			// The message is a contract: the escape fixtures quote it.
			return refuseText("fixture descendant escapes destination: " + path)
		}
	}
	return nil
}

// resolveLenient resolves as much of path as exists and appends the rest
// lexically. The Python implementation used Path.resolve(strict=False); a
// fixture writes into directories that do not exist yet, so refusing a
// nonexistent tail would refuse every first install.
func resolveLenient(path string) (string, error) {
	existing := path
	var remainder []string
	for {
		physical, err := filepath.EvalSymlinks(existing)
		if err == nil {
			return filepath.Join(append([]string{physical}, remainder...)...), nil
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return filepath.Join(append([]string{existing}, remainder...)...), nil
		}
		remainder = append([]string{filepath.Base(existing)}, remainder...)
		existing = parent
	}
}

// strictlyContains reports whether path is below root, resolving nothing.
func strictlyContains(root, path string) bool {
	if root == string(filepath.Separator) {
		return path != root
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// ownerUID is the owning uid of a stat result.
func ownerUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

// ownerGID is the owning gid of a stat result.
func ownerGID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Gid)
}
