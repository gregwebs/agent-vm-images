// Package install is the one owner of writing this layer's gopls into an image:
// the single readonly build, the transcript of the real `go` child, publication of
// exactly the verified bytes and modes, and withdrawal of exactly the paths this
// invocation created.
//
// It restates no rule an owner already has. The selection and its evidence come from
// package closure (the loader and its notice framing); the closed command set, the
// constructed child environment and the private scratch tree come from package
// gotool; the pinned-compiler check, the build-info read and the identity projection
// come from package installed; the build-info parse and comparison come from package
// buildinfo. What is left here is what no owner has:
//
//   - one `go build` with the reviewed flags, and the record of the children that
//     ran it (package gotool had no build member before this slice);
//   - the module-cache proof: the notice bytes the build actually used must equal the
//     committed, framed evidence (the loader proves the evidence, not the cache);
//   - publication of the built bytes at 0755 and the committed notice bytes at 0644;
//   - ownership: a destination is claimed before the first fallible operation on it,
//     and withdrawal removes those claims *and* the directories this invocation
//     created, so a same-target retry is possible.
//
// # Rule table
//
// In the order the rules fire, with the Python reader
// (examples/layers/go-dev/gopls_closure.py at 335fd30) as the live oracle until #254;
// the line ranges are that file's. Every rule names the test that fails when it is
// deleted, and the deletion mutation is run in the #251 PR.
//
//	rule                                                     Python                    test
//	the closure is loaded and proved before a child runs     main 1738-1757            pins: install rows
//	--go-root absolute with bin/go                           _target 1720-1724         TestTargetRulesRefuse
//	the arch enum owns GOARCH, not `dpkg` output             Architecture 1296-1325    TestChildArguments
//	a destination is never overwritten                       1585-1588                 TestExistingDestinationRefused
//	the scratch tree belongs to this invocation              1589-1592                 TestScratchIsRemoved, TestScratchMessageIsNotAVeto
//	closed 21-key child environment, reviewed transport only 1296-1325                 TestChildArguments
//	pinned compiler before the first request                 1435-1443                 TestWrongCompilerMakesNoRequest
//	committed go.mod/go.sum bytes are copied, digest-checked 1519-1530                 TestCommittedBytesAreCopied
//	exactly one readonly build, reviewed flags               1449-1457                 TestChildArguments, pins: install rows
//	build-info identity, target and settings                 1458-1464                 TestWrongSelectionIsRefused
//	cache notice bytes equal the committed evidence          1512-1533                 TestCacheNoticeMutation
//	license bytes re-read and re-checked before publication  1500-1510                 TestNoticeDriftRefused
//	binary published at 0755 through an owned temporary      1600-1605                 pins: install rows
//	notices published at 0644 from the re-checked bytes      1610-1613                 pins: install rows
//	withdrawal removes exactly the claimed paths             1617-1637                 TestPublicationFailureWithdraws
//	withdrawal removes the owned empty directories (G3)      absent in Python          TestOwnedDirectoryIsRemoved, TestOwnedDirectoryDiverges
//	a same-target retry after a late failure succeeds (G3)   absent in Python          TestRetryAfterLateFailure, TestRetryDiverges
//	status 0 with the release on stdout, 1 on refusal         main 1738-1765            main: TestStatusMatrix
//
// The two G3 fixes this verb owns are declared divergences while the Python oracle
// lives: the oracle leaves the directories it created behind (so a retry over the
// same target is refused by its own preflight), and its temporary file name is fixed,
// not invocation-owned. script/test/gopls-closure-pins.py carries both sides of each.
//
// # What this package is not
//
// It is not an authenticator. The build-info check below proves the binary reports
// the reviewed modules, sums, compiler and Linux target settings; it is metadata the
// binary carries about itself, so a candidate that lies coherently with the pin is
// not detected. Module content authentication is the committed go.sum plus the
// reviewed sum-database determination, and this program never speaks HTTP: the pinned
// `go` child is the module-protocol client (G8a). A fixture that points that child at
// a loopback recorder with the sum database off proves the request set and the
// verdicts, never live sum-database authentication.
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/buildinfo"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/installed"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/pathjoin"
)

// Target is what this invocation writes and which Linux target it builds for.
//
// Every path keeps the spelling the caller wrote: see internal/pathjoin. BinDir and
// ShareDir are positions of their own because installation creates them; the
// verifier's Target names a binary instead.
type Target struct {
	GoRoot   string
	BinDir   string
	ShareDir string
	Arch     gotool.Architecture
}

// NewTarget validates the `--go-root` rule through its owner and nothing else: a
// destination that does not exist yet is not an error, because installing into a
// fresh tree is the ordinary case.
func NewTarget(goRoot, binDir, shareDir string, arch gotool.Architecture) (Target, error) {
	if err := installed.CheckGoRoot(goRoot); err != nil {
		return Target{}, err
	}
	return Target{GoRoot: goRoot, BinDir: binDir, ShareDir: shareDir, Arch: arch}, nil
}

// Binary is the one path this verb publishes the tool at.
func (t Target) Binary() string { return pathjoin.Join(t.BinDir, "gopls") }

// Notice is the one path this verb publishes the notice evidence at.
func (t Target) Notice() string { return pathjoin.Join(t.ShareDir, string(closure.FileLicenses)) }

// goal is the go command's own view of the target: the compiler root and the
// architecture. It is derived, never supplied, so no caller can name a GOROOT or a
// GOARCH the loaded closure did not select.
func (t Target) goal() gotool.Target {
	return gotool.Target{GoRoot: t.GoRoot, ShareDir: t.ShareDir, Arch: t.Arch}
}

// Step is one finished child: the whole request the program made, and what came
// back. This is the transcript of the real `go` child — program, argv, environment,
// directory and answer — and it is the artefact a suite counts before it asserts a
// request set (a count over nothing is not evidence).
type Step struct {
	Program string
	Argv    []string
	Env     []string
	Dir     string
	Result  gotool.Result
}

// Transcript collects every step this invocation ran, in order. The caller allocates
// it, so a refused installation still has the record of what it did.
type Transcript struct {
	steps []Step
}

// Steps is the finished transcript. It is a copy: a caller cannot rewrite the record.
func (t *Transcript) Steps() []Step { return append([]Step{}, t.steps...) }

// record wraps a runner and appends each child to the transcript. The child is
// recorded before it runs, so a child that cannot be started is still visible as an
// attempt with a zero result.
func (t *Transcript) record(runner gotool.Runner) gotool.Runner {
	return recordingRunner{transcript: t, next: runner}
}

type recordingRunner struct {
	transcript *Transcript
	next       gotool.Runner
}

func (r recordingRunner) Run(child gotool.Child) (gotool.Result, error) {
	result, err := r.next.Run(child)
	r.transcript.steps = append(r.transcript.steps, Step{
		Program: child.Program,
		Argv:    append([]string{}, child.Argv...),
		Env:     append([]string{}, child.Env...),
		Dir:     child.Dir,
		Result:  result,
	})
	return result, err
}

// Phase is a point in publication a caller may fail at. The shipped program never
// fails there on purpose; the `-tags example_fixture` build is the only caller that
// can pass a non-nil Fault, which is how a real late write failure is provoked
// without weakening a rule or mutating the production source.
type Phase int

const (
	// PhaseBinaryPublished is after the built bytes were renamed into place and
	// before the notice file exists.
	PhaseBinaryPublished Phase = iota
	// PhaseNoticeWritten is after the notice bytes were written and before the mode
	// is set: the file exists, the metadata does not.
	PhaseNoticeWritten
)

// Options is one invocation's inputs that are not the closure or the target.
type Options struct {
	// Environment is the constructed child environment. gotool.Install() in the
	// shipped program; a fixture build substitutes its loopback recorder.
	Environment gotool.Environment
	// Runner runs (or records) the children. nil means the real one.
	Runner gotool.Runner
	// Paths is the private scratch tree. An empty Scratch creates one this
	// invocation owns and removes.
	Paths gotool.Paths
	// Transport is the reviewed transport passthrough (proxy URL, CA), never a
	// content decision.
	Transport map[string]string
	// Transcript is allocated by the caller and is written even on refusal.
	Transcript *Transcript
	// Fault is nil in every shipped build. It is consulted at the named publication
	// phase and its error becomes the primary failure, so withdrawal runs for real.
	Fault func(Phase) error
}

// Installed is what a successful installation produced.
type Installed struct {
	// Release is the installed release, the token the consumer parses.
	Release closure.ReleaseVersion
	// Binary is the published tool path.
	Binary string
	// Notice is the published notice path.
	Notice string
}

// Install proves the closure's build, then publishes exactly the bytes it verified.
//
// Nothing is overwritten: a destination that exists — file or directory — is refused
// before the first child runs. On any failure the paths this invocation created are
// withdrawn, including the directories it made, so the same target can be retried.
func Install(data *closure.Data, target Target, options Options) (*Installed, error) {
	if options.Transcript == nil {
		return nil, errors.New("the caller must provide a transcript")
	}
	if data.Directory == "" {
		return nil, errors.New("the loaded closure names no data directory to re-read")
	}
	transcript := options.Transcript
	runner := options.Runner
	if runner == nil {
		runner = gotool.ExecRunner{}
	}
	runner = transcript.record(runner)
	environment := options.Environment
	if environment == (gotool.Environment{}) {
		environment = gotool.Install()
	}
	paths := options.Paths
	if paths.Scratch == "" {
		scratch, err := os.MkdirTemp("", "gopls-install-")
		if err != nil {
			return nil, fmt.Errorf("cannot create the invocation scratch tree: %v", err)
		}
		paths.Scratch = scratch
		defer func() {
			// A retained scratch tree is a resource leak, not an installation
			// failure: the status is already decided, so it is reported with the
			// exact paths instead of hiding the failure being handled.
			if err := gotool.RemoveTree(scratch); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", installed.Prefix, err)
			}
		}()
	}

	for _, existing := range []string{target.Binary(), target.ShareDir} {
		if _, err := os.Lstat(existing); err == nil {
			return nil, fmt.Errorf("refusing to install over an existing path: %s", existing)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("cannot inspect %s: %v", existing, err)
		}
	}

	if err := paths.Prepare(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(paths.Work(), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create %s: %v", paths.Work(), err)
	}
	work := paths.Work()
	for _, name := range []closure.Filename{closure.FileGoMod, closure.FileGoSum} {
		raw, err := verifiedFile(data, name)
		if err != nil {
			return nil, err
		}
		// The verified bytes are copied, never re-read from a mutable checkout: the
		// build below compiles from this copy.
		if err := os.WriteFile(pathjoin.Join(work, string(name)), raw, 0o644); err != nil {
			return nil, fmt.Errorf("cannot write %s: %v", name, err)
		}
	}

	env, err := environment.ChildEnv(target.goal(), paths, options.Transport)
	if err != nil {
		return nil, err
	}
	if err := installed.RequirePinnedCompiler(env, work, data.Pin.Go, target.goal(),
		runner); err != nil {
		return nil, err
	}
	out := pathjoin.Join(pathjoin.Join(paths.Scratch, "out"), "gopls")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return nil, fmt.Errorf("cannot create %s: %v", filepath.Dir(out), err)
	}
	if err := build(env, work, out, runner); err != nil {
		return nil, err
	}
	raw, err := installed.ReadBuildInfo(env, work, out, runner)
	if err != nil {
		return nil, err
	}
	info, err := buildinfo.Parse([]byte(raw))
	if err != nil {
		return nil, err
	}
	if err := buildinfo.Verify(installed.Selection(data), info, target.Arch,
		data.Pin.Go); err != nil {
		return nil, err
	}
	if err := verifyModuleNotices(data, paths.ModuleCache()); err != nil {
		return nil, err
	}
	noticeBytes, err := verifiedFile(data, closure.FileLicenses)
	if err != nil {
		return nil, err
	}

	published, err := publish(target, out, noticeBytes, options.Fault)
	if err != nil {
		return nil, err
	}
	return &Installed{Release: data.Pin.Gopls, Binary: published.binary,
		Notice: published.notice}, nil
}

// build is the one readonly build this program makes: the reviewed flags, the root
// module the closure selects, and the loaded wrapper's committed requirements.
func build(env []string, dir, out string, runner gotool.Runner) error {
	argv := []string{"-mod=readonly", "-trimpath", "-buildvcs=false", "-o", out,
		closure.Gopls}
	child, err := gotool.NewChild(env, gotool.Build, argv, dir)
	if err != nil {
		return err
	}
	result, err := runner.Run(child)
	if err != nil {
		return err
	}
	if result.Status != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return fmt.Errorf("go build failed (status %d): %s", result.Status, detail)
	}
	return nil
}

// published is what publication left behind.
type published struct {
	binary string
	notice string
}

// publish writes the built bytes and the verified notice bytes, claiming each
// destination before the first fallible operation on it.
//
// Claiming first is what makes a partial write withdrawable: a metadata error leaves
// the bytes on disk, so a list built after the write would not name the file it has
// to remove. The temporary name is this invocation's own (Python used a fixed
// `.gopls.new`, which two invocations would have fought over).
func publish(target Target, built string, notices []byte, fault func(Phase) error) (
	published, error) {
	var claimed []string
	var created []string
	claim := func(path string) { claimed = append(claimed, path) }
	withdraw := func() []string {
		failures := []string{}
		for i := len(claimed) - 1; i >= 0; i-- {
			path := claimed[i]
			if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err := os.Remove(path); err != nil {
				failures = append(failures, path+": "+err.Error())
			}
		}
		// Directories last, deepest first: one that still holds a file we could not
		// remove is reported, never silently left as the only trace of a failure.
		for i := len(created) - 1; i >= 0; i-- {
			if err := os.Remove(created[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
				failures = append(failures, created[i]+": "+err.Error())
			}
		}
		return failures
	}

	binary := target.Binary()
	notice := target.Notice()
	partial := ""
	if err := func() error {
		directories, err := createOwnedDirs(target.BinDir)
		created = append(created, directories...)
		if err != nil {
			return err
		}
		directories, err = createOwnedDirs(target.ShareDir)
		created = append(created, directories...)
		if err != nil {
			return err
		}
		file, err := os.CreateTemp(target.BinDir, ".gopls-")
		if err != nil {
			return fmt.Errorf("cannot create a temporary file in %s: %v", target.BinDir, err)
		}
		partial = file.Name()
		claim(partial)
		if err := copyBytes(built, file); err != nil {
			// The copy failed; the close's own error cannot add to a failure that is
			// already being reported, and the temporary file is withdrawn either way.
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("cannot write %s: %v", partial, err)
		}
		if err := os.Chmod(partial, 0o755); err != nil {
			return fmt.Errorf("cannot set the mode of %s: %v", partial, err)
		}
		claim(binary)
		if err := os.Rename(partial, binary); err != nil {
			return fmt.Errorf("cannot publish %s: %v", binary, err)
		}
		if fault != nil {
			if err := fault(PhaseBinaryPublished); err != nil {
				return err
			}
		}
		claim(notice)
		if err := os.WriteFile(notice, notices, 0o644); err != nil {
			return fmt.Errorf("cannot write %s: %v", notice, err)
		}
		if fault != nil {
			if err := fault(PhaseNoticeWritten); err != nil {
				return err
			}
		}
		if err := os.Chmod(notice, 0o644); err != nil {
			return fmt.Errorf("cannot set the mode of %s: %v", notice, err)
		}
		return nil
	}(); err != nil {
		// The primary failure is what the caller must see; a withdrawal problem is
		// reported with the retained paths so a half-published tool cannot be
		// mistaken for a clean refusal.
		if failures := withdraw(); len(failures) > 0 {
			fmt.Fprintf(os.Stderr, "%s: could not withdraw every path this installation "+
				"created; retained: %s\n", installed.Prefix, strings.Join(failures, "; "))
		}
		return published{}, err
	}
	return published{binary: binary, notice: notice}, nil
}

// createOwnedDirs creates path and every missing ancestor, returning exactly the
// directories this invocation created, outermost first.
//
// The list, not a guess about which directories a target "usually" has, is what
// withdrawal removes: Python removed neither, so its `share/gopls` survived a failed
// publication and its own preflight then refused the retry.
func createOwnedDirs(path string) ([]string, error) {
	var missing []string
	current := path
	for {
		if _, err := os.Lstat(current); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("cannot inspect %s: %v", current, err)
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	created := make([]string, 0, len(missing))
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], 0o755); err != nil {
			return created, fmt.Errorf("cannot create %s: %v", missing[i], err)
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// copyBytes copies the built binary into the already-created temporary file.
func copyBytes(built string, destination *os.File) error {
	source, err := os.Open(built)
	if err != nil {
		return fmt.Errorf("cannot read the built binary: %v", err)
	}
	defer func() { _ = source.Close() }()
	if _, err := io.Copy(destination, source); err != nil {
		return fmt.Errorf("cannot write %s: %v", destination.Name(), err)
	}
	return nil
}

// verifiedFile is one committed input, re-read from disk and proved against the
// digest the loaded pin records.
//
// The loader already compared the file it read; this is a second read of a mutable
// path, not the same check twice: the bytes copied into the build and the bytes
// published are this read's, so a file rewritten between loading and publication
// cannot ship unchecked content.
func verifiedFile(data *closure.Data, name closure.Filename) ([]byte, error) {
	raw, err := os.ReadFile(pathjoin.Join(data.Directory, string(name)))
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %v", name, err)
	}
	pinned, present := data.Pin.Files[name]
	if !present {
		return nil, fmt.Errorf("the loaded closure records no digest for %s", name)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != string(pinned) {
		return nil, fmt.Errorf("%s: bytes differ from the committed digest", name)
	}
	return raw, nil
}

// noticeName is the reviewed notice-file name set, read from the one constant the
// loader validates its evidence with (closure.NoticeNamePattern) rather than a second
// regular expression kept in step by hand.
var noticeName = regexp.MustCompile(closure.NoticeNamePattern)

// verifyModuleNotices proves the notice bytes the build used are the committed ones.
//
// The loader proves the committed framing, identity and digest; the cache is new
// input (it is what the child downloaded this run), so it needs its own check. Both
// directions are failures: a committed notice the cache lacks, and a cache notice the
// evidence does not name.
func verifyModuleNotices(data *closure.Data, moduleCache string) error {
	committed := map[string][]byte{}
	for _, section := range data.Notices {
		committed[sectionKey(section.ID, section.Name)] = section.Raw
	}
	actual := map[string][]byte{}
	for _, module := range data.Pin.Modules {
		directory := pathjoin.Join(moduleCache,
			escapeModule(string(module.ID.Path))+"@"+escapeModule(string(module.ID.Version)))
		found, err := collectNotices(directory, module.ID)
		if err != nil {
			return err
		}
		for name, raw := range found {
			actual[sectionKey(module.ID, name)] = raw
		}
	}
	for key, want := range committed {
		got, present := actual[key]
		if !present {
			return fmt.Errorf("license evidence does not match the build cache for %s", key)
		}
		if string(got) != string(want) {
			return fmt.Errorf("license evidence differs from the build cache: %s", key)
		}
	}
	for key := range actual {
		if _, present := committed[key]; !present {
			return fmt.Errorf("license evidence does not match the build cache for %s", key)
		}
	}
	return nil
}

// sectionKey is one notice's identity in the evidence and in the cache.
func sectionKey(id closure.ModuleID, name closure.NoticePath) string {
	return string(id.Path) + " " + string(id.Version) + " " + string(name)
}

// collectNotices is the notice files of one module cache directory, by canonical
// relative path. A module directory that is not there is missing evidence, never an
// empty set.
func collectNotices(directory string, module closure.ModuleID) (map[closure.NoticePath][]byte,
	error) {
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("license evidence missing for %s: the cache has no %s",
			module, directory)
	}
	found := map[closure.NoticePath][]byte{}
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: unsupported license layout; a notice path is a symlink",
				module)
		}
		if entry.IsDir() {
			return nil
		}
		if !noticeName.MatchString(entry.Name()) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		found[closure.NoticePath(filepath.ToSlash(relative))] = raw
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// escapeModule is the module cache's spelling of a module path or a version: an
// uppercase letter is written as `!` followed by its lowercase form.
func escapeModule(text string) string {
	var builder strings.Builder
	for _, character := range text {
		if character >= 'A' && character <= 'Z' {
			builder.WriteByte('!')
			builder.WriteRune(character - 'A' + 'a')
			continue
		}
		builder.WriteRune(character)
	}
	return builder.String()
}
