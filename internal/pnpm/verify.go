package pnpm

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
)

// symlinkHopCeiling bounds one resolution chain. A chain longer than this is a
// loop the kernel would refuse later, and refusing it here names the path instead
// of leaving the build with a bare ELOOP.
const symlinkHopCeiling = 40

// Target is the audited installation: the distribution prefix, the command
// directory the launcher lives in, and the interpreter the launcher execs.
type Target struct {
	Prefix   string
	Commands string
	Node     string
	// FixtureRoot is the invocation-owned root a fixture install lives in. Empty
	// means a real image, where the image-owned-uid rule applies.
	FixtureRoot string
}

// VerifyOptions is one audit.
type VerifyOptions struct {
	Target Target
	// Source is the read-only mount carrying pin.json and launcher.sh.
	Source string
	// PathIdentity requires the first PATH candidate for each published name to be
	// the installed file. The image certification always asks for it; a caller
	// that authenticates a tree it holds (the host manager, a project fixture)
	// asks the same audit without claiming anything about a PATH.
	PathIdentity bool
}

// ExplicitTarget builds the target a caller named on the command line, so the
// same audit can authenticate a tree that is not at the image's fixed paths.
func ExplicitTarget(prefix, commands, node, fixtureRoot string) Target {
	return Target{Prefix: prefix, Commands: commands, Node: node, FixtureRoot: fixtureRoot}
}

// ResolveTarget reads the environment's target: a fixture destination when the
// fixture contract names one, the production paths otherwise.
func ResolveTarget() (Target, error) {
	fixture := os.Getenv(FixtureDestinationVariable)
	if fixture == "" {
		return Target{
			Prefix:   ProductionPrefix,
			Commands: ProductionCommands,
			Node:     ProductionNode,
		}, nil
	}
	prefix, commands, err := FixtureDestination(fixture)
	if err != nil {
		return Target{}, err
	}
	root, err := filepath.EvalSymlinks(fixture)
	if err != nil {
		return Target{}, refuseCause(err)
	}
	return Target{
		Prefix:      prefix,
		Commands:    commands,
		Node:        filepath.Join(root, "usr/bin/node"),
		FixtureRoot: root,
	}, nil
}

// Verify audits the published launcher and every immutable distribution byte,
// ancestry included.
//
// A shebang audit alone sees only /bin/sh, and the bundle hash proves content but
// not that no unprivileged caller can rewrite the installed bytes later. So this
// walks the physical resolution path of each target -- every intermediate link's
// target and that target's ancestor chain -- and checks ownership, writability and
// the readable bits on each component.
func Verify(options VerifyOptions) error {
	target := options.Target
	sourceDir := options.Source
	if sourceDir == "" {
		sourceDir = SourceDir()
	}
	sourcePin, err := LoadPin(filepath.Join(sourceDir, "pin.json"))
	if err != nil {
		return err
	}
	installedPin, err := LoadPin(filepath.Join(target.Prefix, "pin.json"))
	if err != nil {
		return err
	}
	if installedPin != sourcePin {
		return refuseText("installed pin disagrees with reviewed source")
	}
	bundle, err := os.ReadFile(filepath.Join(target.Prefix, "dist/pnpm.mjs"))
	if err != nil {
		return refuseCause(err)
	}
	if Sum256(bundle) != sourcePin.BundleSHA256 {
		return refuseText("pristine distribution changed")
	}
	launcher := filepath.Join(target.Commands, "pnpm")
	info, err := os.Lstat(launcher)
	if err != nil {
		return refuseCause(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return refuseText("pnpm launcher must be a regular file")
	}
	installedLauncher, err := os.ReadFile(launcher)
	if err != nil {
		return refuseCause(err)
	}
	reviewedLauncher, err := os.ReadFile(filepath.Join(sourceDir, "launcher.sh"))
	if err != nil {
		return refuseCause(err)
	}
	if !bytes.Equal(installedLauncher, reviewedLauncher) {
		return refuseText("pnpm launcher changed")
	}
	if options.PathIdentity {
		if err := checkCandidates(target); err != nil {
			return err
		}
	}
	entries, err := distributionEntries(target.Prefix)
	if err != nil {
		return err
	}
	paths := append([]string{target.Prefix, launcher, target.Commands, target.Node}, entries...)
	if err := auditPaths(paths, map[string]bool{launcher: true, target.Node: true}, target.FixtureRoot); err != nil {
		return err
	}
	launcherInfo, err := os.Stat(launcher)
	if err != nil {
		return refuseCause(err)
	}
	// Exactly 0755, including the special bits: a setuid or setgid launcher would
	// execute the interpreter with another identity, and 0755 is the mode the
	// image review signed off on.
	specials := os.ModeSetuid | os.ModeSetgid | os.ModeSticky
	if launcherInfo.Mode().Perm() != 0o755 || launcherInfo.Mode()&specials != 0 {
		return refuseText("launcher mode must be 0755")
	}
	return nil
}

// checkCandidates proves the first PATH candidate for each published name is the
// installed file itself. A same-version copy earlier on PATH is a different
// program, and the image would run that one.
//
// The fixture exemption exists because a fixture root is not the image's /usr
// layout: the fixture's own copy of the command directory is linked into PATH, so
// the first candidate is the same file under another name.
func checkCandidates(target Target) error {
	path := os.Getenv("PATH")
	for _, name := range commandNames {
		installed := filepath.Join(target.Commands, name)
		first, found := firstCandidate(path, name)
		if !found {
			return refuseText("unexpected first PATH candidate for " + name)
		}
		if first != installed {
			if target.FixtureRoot == "" || !sameFile(first, installed) {
				return refuseText("unexpected first PATH candidate for " + name)
			}
		}
		if name == "pnpm" {
			continue
		}
		// A regular file where an alias belongs is refused with the alias rule
		// rather than as a readlink error: both predecessors said the same thing,
		// and "this is not the reviewed link" is the fact a reader needs.
		link, err := os.Readlink(first)
		if err != nil || link != "pnpm" {
			return refuseText("alias must link relatively to pnpm")
		}
	}
	return nil
}

// firstCandidate is the first PATH entry that names the command. Existence is
// lstat existence, so a dangling link is a candidate rather than a skip: the image
// would try to run it.
//
// An empty PATH component means the current directory — `PATH=:/usr/bin` resolves
// `pnpm` to `./pnpm` — so it is searched like any other, and the candidate is
// spelled relative to the cwd. That is the rule the image's identity boundary
// rests on: a same-version `pnpm` in the working directory is a different program,
// and skipping the entry would certify it.
func firstCandidate(pathVariable, name string) (string, bool) {
	for _, directory := range filepath.SplitList(pathVariable) {
		candidate := filepath.Join(directory, name)
		if directory == "" {
			candidate = "." + string(filepath.Separator) + name
		}
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

func sameFile(left, right string) bool {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false
	}
	return os.SameFile(leftInfo, rightInfo)
}

// distributionEntries is every descendant of the installed distribution. The
// authenticated extraction contains no links, so any link here is an
// unauthenticated redirect and refuses the whole audit.
//
// The walk order is the directory order ReadDir returns: sorted by name. The
// behaviour this reproduces was measured with the Python predecessor, whose
// os.scandir handed back filesystem order -- every mutation the suites make
// touches one entry at a time, so the first refusal was stable there, and sorting
// makes a report with two faults reproducible here.
func distributionEntries(prefix string) ([]string, error) {
	var entries []string
	pending := []string{prefix}
	for len(pending) > 0 {
		directory := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		children, err := os.ReadDir(directory)
		if err != nil {
			return nil, refuseCause(err)
		}
		for _, child := range children {
			path := filepath.Join(directory, child.Name())
			if child.Type()&os.ModeSymlink != 0 {
				return nil, refuseText("unexpected symlinked distribution entry: " + path)
			}
			entries = append(entries, path)
			if child.IsDir() {
				pending = append(pending, path)
			}
		}
	}
	return entries, nil
}

// dereferencedPaths is every path the kernel dereferences while resolving path,
// one symlink hop at a time. A collapsing realpath would hide an intermediate
// hop: a directory that is merely a waypoint on the chain -- writable, and
// therefore replaceable by any uid -- would escape a final-target ancestry audit.
//
// Each resolved component is reported as the kernel spells it, so the audit sees
// both the link and the target it lands on. What is reproduced here was measured
// on the Python predecessor: os.readlink was called per hop and a relative target
// was joined to the directory containing the link.
func dereferencedPaths(path string) ([]string, error) {
	var paths []string
	resolved := string(filepath.Separator)
	pending := pathComponents(path)
	hops := 0
	for len(pending) > 0 {
		candidate := joinComponent(resolved, pending[0])
		pending = pending[1:]
		paths = append(paths, candidate)
		info, err := os.Lstat(candidate)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		hops++
		if hops > symlinkHopCeiling {
			return nil, refuseText("symlink chain too deep: " + path)
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return nil, refuseCause(err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(resolved, target)
		}
		resolved = string(filepath.Separator)
		pending = append(pathComponents(target), pending...)
	}
	return paths, nil
}

// pathComponents spells a path the way Path.parts does: the root separator is
// itself the first component of an absolute path.
func pathComponents(path string) []string {
	var parts []string
	for _, element := range strings.Split(filepath.ToSlash(path), "/") {
		if element == "" || element == "." {
			continue
		}
		parts = append(parts, element)
	}
	if filepath.IsAbs(path) {
		return append([]string{string(filepath.Separator)}, parts...)
	}
	return parts
}

func joinComponent(resolved, component string) string {
	if component == string(filepath.Separator) {
		return component
	}
	return filepath.Join(resolved, component)
}

// auditPaths checks every component on every physical resolution path, including
// each intermediate link's target and its ancestry. A writable directory that
// appears only mid-chain must not escape the certification.
func auditPaths(paths []string, executable map[string]bool, fixtureRoot string) error {
	checked := make(map[string]bool, len(paths))
	for _, path := range paths {
		resolved, err := dereferencedPaths(path)
		if err != nil {
			return err
		}
		for _, part := range resolved {
			if err := auditPath(part, executable, fixtureRoot, checked); err != nil {
				return err
			}
		}
	}
	return nil
}

// auditPath applies the three per-path rules: the image owns it, no unprivileged
// caller may write it, and every uid may reach it.
func auditPath(path string, executable map[string]bool, fixtureRoot string, checked map[string]bool) error {
	// A fixture root sits under the caller's own directories, which are not image
	// paths: their mode and owner are the invocation's business. The root itself
	// is still audited -- it is where the published tree starts.
	if fixtureRoot != "" && strictlyContains(path, fixtureRoot) {
		return nil
	}
	if checked[path] {
		return nil
	}
	checked[path] = true
	info, err := os.Stat(path)
	if err != nil {
		return refuseCause(err)
	}
	if fixtureRoot == "" && (ownerUID(info) != 0 || ownerGID(info) != 0) {
		return refuseText("non-image-owned distribution path: " + path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return refuseText("group/other-writable distribution path: " + path)
	}
	required := os.FileMode(0o004)
	if info.IsDir() || executable[path] {
		required = 0o005
	}
	if info.Mode().Perm()&required != required {
		return refuseText("unreadable distribution path: " + path)
	}
	return nil
}
