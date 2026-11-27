// Package installed is the one owner of the installed-state check for this layer:
// which path the verifier inspects, what its mode and its notice file must be, and
// the order in which those rules and the build-info check fire.
//
// The rule table, in the order the rules fire, with the Python reader
// (examples/layers/go-dev/gopls_closure.py at 335fd30) as the oracle until #254;
// the line ranges are that file's. Every rule names the test that fails when it is
// deleted (run for every row in the #250 PR), so a deletion mutation that leaves
// the suite green is a rule without a witness.
//
//	rule                                                Python                    test
//	--go-root absolute with bin/go                      _target 1730-1734         TestTargetRulesRefuse
//	--binary absolute and named gopls                   _target 1735-1740         TestTargetRulesRefuse
//	the caller's spelling is used, as the kernel reads it  _target 1722-1740      TestPathSemanticsAreTheKernels, main: TestVerifyInstalledUsesTheRequestedSpelling
//	the requested binary is present, not a symlink      verify_installed 1659     TestBinaryRulesRefuse
//	exactly the requested path, no sibling fallback     verify_installed 1657     TestSiblingIsNotSubstituted
//	binary mode is exactly 0755, special bits included  verify_installed 1660-62  TestBinaryRulesRefuse, TestModeSpecialBitsDiverge
//	the notice file is present, not a symlink           verify_installed 1665     TestNoticeRulesRefuse
//	notice mode is exactly 0644 (G3)                    absent in Python          TestNoticeRulesRefuse, TestNoticeModeDiverges
//	notice bytes equal the pinned digest                verify_installed 1666-68  TestNoticeRulesRefuse
//	the closed Go environment and argv                  GoEnvironment 1296-1325   TestChildArguments
//	go env GOVERSION is the pinned compiler             require_pinned_compiler   TestCompilerRulesRefuse
//	go env GOROOT is --go-root                          require_pinned_compiler   TestGoRootRulesRefuse
//	strict build-info JSON (G3)                         parse_build_info 1342     package buildinfo: TestStrictDocumentRefuses
//	build-info identity, target and settings            verify_module_build_info 1386-1410  package buildinfo
//	the candidate is never executed, no request made    verify_installed 1654     TestChildArguments, TestCandidateIsNeverExecuted, TestScratchIsRemoved, pins: "a verifiable candidate that would write a marker is never started", "the verifier made no proxy request"
//	status 0 with the release on stdout, 1 on refusal   main 1738-1765            main: TestStatusMatrix
//
// Not ported, with the reason:
//
//   - the notice *section* re-parse (Python 1669-1671) cannot change a verdict: it
//     runs after the pinned-digest comparison, and equal bytes parse to the reviewed
//     sections. It is a duplicate call, not defence in depth (#188 R-c); the digest
//     rule is the witness for the family.
//   - the copy of go.mod/go.sum into the scratch module (Python 1672-1677). Neither
//     closed command's *answer* depends on a module's contents — `go env` reports
//     configuration and `go version -m -json` reads the named binary — so copying
//     them would add a second read of the data directory after the loader's digest
//     check without deciding either verdict. It is not a claim about which files the
//     Go command opens.
//
// The two G3 fixes this verb owns are declared divergences while the Python oracle
// lives: a binary mode is compared with its special bits included (Python masked
// them, so 01755 read as 0755), and the installed notice file's mode is checked at
// all (Python did not). script/test/gopls-closure-pins.py carries both sides of each.
package installed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/buildinfo"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/pathjoin"
)

// Prefix is the frozen diagnostic prefix this program's consumer parses. It is
// written once, here, and main.go prints every refusal through it.
const Prefix = "gopls-closure"

// NewTarget validates the paths the verb was given and turns them into the one
// target the rest of the check uses.
//
// Every path is kept in the spelling the caller wrote. `filepath.Clean` would turn
// `a/link/../gopls` into `a/gopls`, which is a different file when `link` is a
// symlink: the kernel resolves `..` after the link, so the caller's spelling and the
// cleaned spelling name different files. This verb must inspect the file the caller
// named, so it neither cleans nor resolves here — and every path the rules below
// append goes through internal/pathjoin, the module's single no-normalise join,
// rather than filepath.Join.
func NewTarget(goRoot, binary, shareDir string, arch gotool.Architecture) (gotool.Target, error) {
	if err := CheckGoRoot(goRoot); err != nil {
		return gotool.Target{}, err
	}
	if !filepath.IsAbs(binary) {
		return gotool.Target{}, fmt.Errorf("--binary must be absolute: %s", binary)
	}
	if filepath.Base(binary) != "gopls" {
		return gotool.Target{}, fmt.Errorf("--binary must name the installed tool gopls, not %s",
			quote(filepath.Base(binary)))
	}
	return gotool.Target{GoRoot: goRoot, ShareDir: shareDir, Arch: arch, Binary: binary}, nil
}

// CheckGoRoot is the `--go-root` rule, and the only copy of it.
//
// `install` writes into a directory rather than naming a binary, so it cannot call
// NewTarget; it calls this instead, and the rule the two verbs apply is then one
// rule. A Go root that is not absolute, or that does not hold `bin/go`, would make
// every child below it a different compiler than the pin names.
func CheckGoRoot(goRoot string) error {
	if !filepath.IsAbs(goRoot) ||
		!isRegularFile(pathjoin.Join(pathjoin.Join(goRoot, "bin"), "go")) {
		return fmt.Errorf("--go-root must be an absolute Go root with bin/go: %s", goRoot)
	}
	return nil
}

// Verify proves the installed state against the loaded closure and returns the
// release the consumer parses.
//
// The candidate is never executed: it is an argument to `go version -m -json`, and
// the pinned compiler is a metadata reader. Nothing here is a network request —
// GOPROXY is off, there is no fallback origin, and both commands are local
// configuration and file reads.
func Verify(data *closure.Data, target gotool.Target, runner gotool.Runner) (closure.ReleaseVersion, error) {
	if err := checkBinary(target.Binary); err != nil {
		return "", err
	}
	pinnedNotice, present := data.Pin.Files[closure.FileLicenses]
	if !present {
		return "", fmt.Errorf("the loaded closure records no digest for %s",
			closure.FileLicenses)
	}
	if err := checkNotice(target.ShareDir, pinnedNotice); err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp("", "gopls-verify-")
	if err != nil {
		return "", fmt.Errorf("cannot create the invocation scratch tree: %v", err)
	}
	// A retained scratch tree is a resource leak, not a verification failure: the
	// status is already decided, so it is reported with the exact path instead of
	// being folded into a second error that would hide the first.
	defer func() {
		if err := gotool.RemoveTree(scratch); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", Prefix, err)
		}
	}()
	paths := gotool.Paths{Scratch: scratch}
	if err := paths.Prepare(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(paths.Work(), 0o755); err != nil {
		return "", fmt.Errorf("cannot create %s: %v", paths.Work(), err)
	}
	environment, err := gotool.Offline().ChildEnv(target, paths, nil)
	if err != nil {
		return "", err
	}
	if err := RequirePinnedCompiler(environment, paths.Work(), data.Pin.Go, target, runner); err != nil {
		return "", err
	}
	raw, err := ReadBuildInfo(environment, paths.Work(), target.Binary, runner)
	if err != nil {
		return "", err
	}
	info, err := buildinfo.Parse([]byte(raw))
	if err != nil {
		return "", err
	}
	if err := buildinfo.Verify(Selection(data), info, target.Arch, data.Pin.Go); err != nil {
		return "", err
	}
	return data.Pin.Gopls, nil
}

// Selection projects the validated pin onto the identity triple the build-info
// check compares. The installed-state check and the installer both prove a binary
// against it, so the projection has one owner.
func Selection(data *closure.Data) []buildinfo.Module {
	modules := make([]buildinfo.Module, 0, len(data.Pin.Modules))
	for _, module := range data.Pin.Modules {
		modules = append(modules, buildinfo.Module{Path: module.ID.Path,
			Version: module.ID.Version, H1: module.H1})
	}
	return modules
}

// checkBinary applies the presence and mode rules to exactly the requested path.
func checkBinary(binary string) error {
	info, err := os.Lstat(binary)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("installed: missing %s", binary)
	}
	if mode := unixMode(info); mode != 0o755 {
		return fmt.Errorf("installed: %s has mode %o, want 755", binary, mode)
	}
	return nil
}

// checkNotice applies the notice rules: present, exactly 0644, and the pinned bytes.
//
// The digest is the strong rule. Equal bytes to the committed file are the reviewed
// evidence; the section re-parse the Python reader also ran could not disagree with
// it, so it is not ported (see the package comment).
func checkNotice(shareDir string, pinned closure.SHA256) error {
	path := pathjoin.Join(shareDir, string(closure.FileLicenses))
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("installed: missing %s", path)
	}
	if mode := unixMode(info); mode != 0o644 {
		return fmt.Errorf("installed: %s has mode %o, want 644", path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("installed: missing %s", path)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != string(pinned) {
		return fmt.Errorf("installed: %s does not match the pinned license digest", path)
	}
	return nil
}

// RequirePinnedCompiler refuses before the first request if the compiler that will
// answer is not the one the pin names, or if it lives somewhere other than the Go
// root the caller named. The installer calls it before its build, so a wrong
// compiler costs no network request; the verifier calls it before its metadata read.
func RequirePinnedCompiler(environment []string, dir string, compiler closure.GoVersion,
	target gotool.Target, runner gotool.Runner) error {
	version, err := output(environment, gotool.EnvGoVersion, nil, dir, "go env GOVERSION",
		runner)
	if err != nil {
		return err
	}
	if version != compiler.Toolchain() {
		return fmt.Errorf("compiler is %s, want %s", version, compiler.Toolchain())
	}
	goroot, err := output(environment, gotool.EnvGoRoot, nil, dir, "go env GOROOT", runner)
	if err != nil {
		return err
	}
	if realPath(goroot) != realPath(target.GoRoot) {
		return fmt.Errorf("GOROOT is %s, want the pinned %s", goroot, target.GoRoot)
	}
	return nil
}

// ReadBuildInfo runs the one command that reads a binary's metadata. It is the
// reader both verbs use: the verifier on the installed path, the installer on the
// bytes it just built and has not published yet.
func ReadBuildInfo(environment []string, dir, binary string, runner gotool.Runner) (string, error) {
	return output(environment, gotool.BuildInfo, []string{binary}, dir, "go version -m -json",
		runner)
}

// output runs one closed command and returns its trimmed stdout, or the refusal the
// reader this ported from printed: the command's own name, its status and whatever
// the child said.
func output(environment []string, command gotool.Command, extra []string, dir, what string,
	runner gotool.Runner) (string, error) {
	child, err := gotool.NewChild(environment, command, extra, dir)
	if err != nil {
		return "", err
	}
	result, err := runner.Run(child)
	if err != nil {
		return "", err
	}
	if result.Status != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return "", fmt.Errorf("%s failed (status %d): %s", what, result.Status, detail)
	}
	return strings.TrimSpace(result.Stdout), nil
}

// isRegularFile reports whether path is a regular file, following no symlink of its
// own name (the Go root may legitimately be reached through one).
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// unixMode is the file's permission bits including setuid, setgid and sticky. The
// Python reader masked those away with `& 0o777`, which made a 01755 or 02755
// binary look like the reviewed one.
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

// realPath is Python's os.path.realpath for a path that exists: the resolved
// location, so a Go root reached through a symlinked prefix compares equal to
// itself and a different tree does not.
func realPath(path string) string {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return resolved
}

// quote renders a name the way the Python reader's repr did.
func quote(text string) string { return "'" + text + "'" }
