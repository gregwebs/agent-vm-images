// Package goplsstage is the maintainer-only staging half of the go-dev example's
// gopls pin writer (#252): it resolves one exact release with the pinned Go,
// cross-builds both Linux targets offline, and writes a candidate directory.
//
// It is writer-only code on purpose. The rules are not here: the layer's `closure`
// package owns what a candidate closure is (canonical wrapper, both sums, the
// reviewed adoption restriction, the notice layout, the signature projection and the
// record's write shape), `gotool` owns the child environment and the command family,
// `install.Build` owns the one readonly build and its build-info proof, and
// `installed` owns the compiler and selection rules. What is left here is the
// resolution the image may never perform — `go get -tool`, `mod download`, `mod
// edit`, `mod tidy`, `mod verify` — plus the proposal that a human reviews.
//
// Publication is not in this slice: the stable shell writer
// (`script/dev/upgrade-gopls.sh`) remains the only publisher of the checkout until
// #253, and everything this package writes is under the caller's `--out` plus an
// invocation-owned scratch tree it removes. The Python stager
// (`script/dev/gopls-closure-update.py`) stays live as the oracle; the differential
// in script/test/gopls-stage-parity.py compares the two over the same fixture, and
// script/test/gopls-closure-maintenance.py keeps driving the shell writer.
//
// # Rule table
//
// In order, with the Python stager (script/dev/gopls-closure-update.py at 236b8b7)
// as the oracle until #254; line ranges are that file's. Every rule names the test
// that fails when it is deleted, and the deletion mutation is run in the #252 PR.
//
//	rule                                                       Python                    test
//	CI refuses staging before any child                        333-336, 699-709          TestStageRefusesInGitHubActions; cmd: TestGoplsStageGitHubActions
//	--out must be a new invocation-owned path                  309-310                   TestOutMustBeNew
//	--current loads, or is absent; partial state refuses        311-316                   TestPartialCurrentRefuses
//	the resolved compiler is the pinned one                    353-356                   TestWrongCompilerMakesNoResolution
//	the resolution environment is what was asked for            357-362                   TestResolutionEnvironmentIsTheChildEnvironmentMinusTheTarget (gotool)
//	the release's own go directive fits the layer pin            375-379                   TestGoDirectiveNewerThanThePinRefuses
//	one resolution, then the shared candidate derivation        464-491                   TestStageCrossBuildsBothTargets; closure: TestDeriveCandidateReproducesTheCommittedRecord
//	two Linux cross-builds, shared build owner                  505-511                   TestStageCrossBuildsBothTargets
//	the cross-builds are offline, whatever the resolution origin 512                       TestCompilePhaseIsOffline
//	carried review, or a dated one, or a retained proposal      543-565                   TestCarriedReviewIsRetained, TestRenewalNeedsReview
//	the candidate is written and re-loaded before it is shown   567-575                   TestStageCrossBuildsBothTargets (the re-read itself is unwitnessed defence in depth)
//	the Dockerfile assertion is rewritten exactly once          213-219                   TestDockerfileAssertion
//	the summary is the reviewed proposal                       596-692                   gopls-stage-parity.py (stdout, stderr and summary.txt byte for byte)
//	the scratch tree belongs to this invocation                 317-330                   TestScratchIsRemoved
//
// # Limits stated rather than implied
//
// The draft proposal's `pin.json` is rendered by Go's JSON encoder where the oracle
// used `json.dumps(indent=2)`: the two agree byte for byte on every field these
// rules admit (all are ASCII by their own patterns), and the differential compares
// the whole file, so a non-ASCII origin field would be red rather than silently
// different. The resolution contacts the reviewed proxy with the sum database on;
// the fixtures point the pinned `go` child at a loopback recorder with the sum
// database off, which proves the request set and the verdicts, never live
// checksum-database authentication. One rule is not re-derived here but asserted
// last: the candidate is re-loaded through `closure.Load`, so a record this package
// could write but the image could not read is refused before it is printed.
package goplsstage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/install"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/installed"
)

// Prefix is the writer's frozen diagnostic prefix: the Python stager caught every
// staging refusal in one handler and printed it under this prefix, and the shell
// writer's stage log is compared on it.
const Prefix = "gopls-closure-update"

// Refusal is a staging decision this writer will not make. Its message is the
// contract; the command prints it after the prefix and returns status 1.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

func refusal(format string, arguments ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, arguments...)}
}

// Request is one staging run. Every path is absolute and is used in the spelling the
// caller wrote: see internal/pathjoin.
type Request struct {
	// Release is the exact gopls release to stage, and Compiler the layer's pinned Go.
	Release  closure.ReleaseVersion
	Compiler closure.GoVersion
	// Current is the committed data directory this run derives from. It may be
	// absent, which is a first adoption; when it exists it must load, because a
	// partial pinned state is recovered by a human, never overwritten.
	Current string
	// Out is the new directory the candidate is written to. It must not exist.
	Out string
	// Dockerfile is the layer Dockerfile whose ARG assertion is rewritten into Out.
	Dockerfile string
	// ReviewedOn and Reviewer are the day a human reviewed the new scope and who
	// they are. They are given together or not at all.
	ReviewedOn *closure.Date
	Reviewer   string
	// Go is the resolved host compiler. It runs the resolution with the pinned
	// toolchain selected, so a different `go` on PATH cannot answer instead.
	Go string
}

// Dependencies is everything a run may execute or contact, injected once.
type Dependencies struct {
	// Environment is the origin the resolution may contact. Production takes
	// gotool.Install() (the reviewed proxy and verifier key); a fixture build takes
	// gotool.NewEnvironment(loopback, "off").
	Environment gotool.Environment
	// Transport is the reviewed transport passthrough. It carries a route and a CA,
	// never a content decision, and it is not passed to the compile phase: the build
	// runs with the proxy off.
	Transport map[string]string
	// Runner runs the children. nil means the real one.
	Runner gotool.Runner
	// GitHubActions refuses the run before any child: pin maintenance is
	// maintainer-host work.
	GitHubActions bool
	// ScratchRoot is where the invocation-owned work tree is created; empty means
	// the operating system's temporary directory.
	ScratchRoot string
}

// Proposal is what a successful staging run produced, described against the record it
// derives from.
type Proposal struct {
	Out           string
	Release       closure.ReleaseVersion
	Compiler      closure.GoVersion
	Modules       []closure.ModuleID
	Previous      []closure.ModuleID
	ReviewCarried bool
	Summary       string
}

// Stage resolves, proves and writes one candidate. It publishes nothing: Out is the
// only path it writes, and the checkout is read-only input.
func Stage(ctx context.Context, request Request, dependencies Dependencies) (*Proposal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dependencies.GitHubActions {
		return nil, refusal("stage is refused in GitHub Actions; pin maintenance is " +
			"maintainer-host work and CI only runs the offline check")
	}
	if request.Go == "" {
		return nil, errors.New("no resolved host compiler: the caller names the `go` it ran")
	}
	if _, err := os.Lstat(request.Out); err == nil {
		return nil, refusal("--out must be a new invocation-owned path: %s", request.Out)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, refusal("cannot inspect --out: %s", request.Out)
	}
	previous, err := current(request.Current)
	if err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(dependencies.ScratchRoot, "gopls-stage-")
	if err != nil {
		return nil, refusal("cannot create the invocation scratch tree: %v", err)
	}
	defer func() {
		// A retained scratch tree is a resource leak, not a staging failure: the
		// status is already decided, so it is reported with the exact path instead
		// of hiding the failure being handled.
		if err := gotool.RemoveTree(work); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", Prefix, err)
		}
	}()
	return stageInto(ctx, request, dependencies, previous, work)
}

// current is the record this run derives from. A directory that exists must load: a
// partial pin is recovered, not overwritten, and a `--current` that is a file is not
// a pin directory at all.
func current(directory string) (*closure.Data, error) {
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, refusal("cannot inspect --current: %s", directory)
	}
	if !info.IsDir() {
		return nil, refusal("--current is not a directory: %s", directory)
	}
	source, err := closure.DirSource(directory)
	if err != nil {
		return nil, err
	}
	data, err := closure.Load(context.Background(), closure.Request{}, source)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// stageInto is the run with its scratch tree already allocated.
func stageInto(ctx context.Context, request Request, dependencies Dependencies,
	previous *closure.Data, work string) (*Proposal, error) {
	paths := gotool.Paths{Scratch: filepath.Join(work, "build")}
	if err := paths.Prepare(); err != nil {
		return nil, refusal("%v", err)
	}
	module := filepath.Join(work, "module")
	if err := os.MkdirAll(module, 0o755); err != nil {
		return nil, refusal("cannot create %s: %v", module, err)
	}
	resolution := resolver{
		environment: dependencies.Environment,
		transport:   dependencies.Transport,
		runner:      dependencies.Runner,
		paths:       paths,
		goBinary:    request.Go,
		module:      module,
	}
	resolution.runner = orExec(resolution.runner)
	// The wrapper's own go directive is replaced from the root module's before tidying,
	// so this is only a starting value.
	placeholder := "module " + closure.Wrapper + "\n\ngo 1.26.0\n"
	if err := writeNew(filepath.Join(module, "go.mod"), []byte(placeholder)); err != nil {
		return nil, refusal("%v", err)
	}
	if err := resolution.prepare(request.Compiler); err != nil {
		return nil, err
	}

	// One resolution, with the exact release. Everything after this reads the bytes
	// it produced; nothing below re-resolves.
	if _, err := resolution.mustGotool(gotool.GetTool,
		[]string{closure.Gopls + "@" + string(request.Release)}, "go get -tool"); err != nil {
		return nil, err
	}
	downloaded, err := resolution.mustGotool(gotool.ModDownloadJSON,
		[]string{closure.Gopls + "@" + string(request.Release)}, "go mod download")
	if err != nil {
		return nil, err
	}
	evidence, err := parseDownload(downloaded)
	if err != nil {
		return nil, err
	}
	rootRaw, err := os.ReadFile(evidence.GoMod)
	if err != nil {
		return nil, refusal("go mod download: cannot read the root evidence: %v", err)
	}
	root, err := closure.ParseRootGoMod(rootRaw, string(closure.FileRootGoMod))
	if err != nil {
		return nil, err
	}
	if !closure.LanguageAtMost(root.Go, request.Compiler) {
		return nil, refusal("gopls %s requires go >= %s; the layer pins %s", request.Release,
			root.Go, request.Compiler)
	}
	// The wrapper records the upstream release's own language version, not the
	// maintenance host's, and never a toolchain selection.
	if _, err := resolution.mustGotool(gotool.ModEdit,
		[]string{"-go=" + string(root.Go), "-toolchain=none"}, "go mod edit"); err != nil {
		return nil, err
	}
	for _, step := range []struct {
		command gotool.Command
		extra   []string
		what    string
	}{
		{gotool.ModTidy, nil, "go mod tidy"},
		{gotool.ModVerify, nil, "go mod verify"},
		{gotool.ModTidyDiff, nil, "go mod tidy -diff"},
	} {
		if _, err := resolution.mustGotool(step.command, step.extra, step.what); err != nil {
			return nil, err
		}
	}

	goModRaw, err := os.ReadFile(filepath.Join(module, "go.mod"))
	if err != nil {
		return nil, refusal("cannot read the resolved go.mod: %v", err)
	}
	goSumRaw, err := os.ReadFile(filepath.Join(module, "go.sum"))
	if err != nil {
		return nil, refusal("cannot read the resolved go.sum: %v", err)
	}
	// The shared owner decides what this candidate is before the first cross-build:
	// canonical form, both sums, the adoption restriction, the notice layout and the
	// signature projection. This package derives none of that itself.
	candidate, err := closure.DeriveCandidate(closure.CandidateRequest{
		Release: request.Release, Compiler: request.Compiler, GoMod: goModRaw,
		GoSum: goSumRaw, RootGoMod: rootRaw, ModuleCache: paths.ModuleCache()})
	if err != nil {
		return nil, err
	}

	goRoot, err := resolution.mustGotool(gotool.EnvGoRoot, nil, "go env GOROOT")
	if err != nil {
		return nil, err
	}
	staged := filepath.Join(work, "staged")
	if err := os.MkdirAll(staged, 0o755); err != nil {
		return nil, refusal("cannot create %s: %v", staged, err)
	}
	for _, name := range []struct {
		filename closure.Filename
		raw      []byte
	}{{closure.FileGoMod, goModRaw}, {closure.FileGoSum, goSumRaw}} {
		if err := writeNew(filepath.Join(staged, string(name.filename)), name.raw); err != nil {
			return nil, refusal("%v", err)
		}
	}
	selection := installed.SelectionOf(candidate.Modules)
	for _, arch := range gotool.Architectures() {
		target := gotool.Target{GoRoot: goRoot, ShareDir: filepath.Join(work, "share"),
			Arch: arch}
		// The compile phase is offline regardless of where the resolution went: the
		// staged files plus the authenticated cache must be complete, so a build that
		// hid a missing module behind a fetch would be caught here rather than shipped.
		// It carries no transport for the same reason.
		environment, err := gotool.Offline().ChildEnv(target, paths, nil)
		if err != nil {
			return nil, refusal("%v", err)
		}
		out := filepath.Join(work, "gopls-"+arch.Value())
		if err := install.Build(environment, staged, out, target, selection,
			request.Compiler, resolution.runner); err != nil {
			return nil, err
		}
	}

	provenance := closure.Provenance{Source: closure.Proxy + "/" + closure.Gopls + "/@v/" +
		string(request.Release) + ".info", Vcs: evidence.Origin.VCS, URL: evidence.Origin.URL,
		Subdir: evidence.Origin.Subdir, Ref: evidence.Origin.Ref, Hash: evidence.Origin.Hash}
	carried := closure.Carried(previous, request.Release, candidate)
	var review *closure.Review
	switch {
	case carried:
		record := previous.Pin.Review
		review = &record
	case request.ReviewedOn == nil || request.Reviewer == "":
		// The proposal is the exact byte set an approved run would publish, so a human
		// reviews bytes rather than a digest summary. It is written under --out (never
		// the checkout), and `pin.proposal.json` is not a readable pin: it has no
		// review.
		if err := writeCandidate(request, candidate, provenance, nil, true); err != nil {
			return nil, err
		}
		summary := closure.Describe(previous, request.Release, request.Compiler, candidate,
			carried, nil)
		return nil, refusal("%s\n%s: %s", summary, closure.Renewal,
			closure.Identity(previous, request.Release))
	default:
		renewed := closure.RenewalReview(request.Release, candidate, *request.ReviewedOn,
			request.Reviewer)
		review = &renewed
	}
	if err := writeCandidate(request, candidate, provenance, review, false); err != nil {
		return nil, err
	}
	// The candidate is re-read through the loader before it is shown: a record this
	// package could write but the image could not read is refused here. It is
	// defence in depth while the derivation above and the loader overlap; the
	// differential's divergence register names the rows that make it visible.
	expectRelease, expectCompiler := request.Release, request.Compiler
	source, err := closure.DirSource(filepath.Join(request.Out, "gopls-closure"))
	if err != nil {
		return nil, err
	}
	if _, err := closure.Load(ctx, closure.Request{ExpectGopls: &expectRelease,
		ExpectGo: &expectCompiler}, source); err != nil {
		return nil, err
	}
	summary := closure.Describe(previous, request.Release, request.Compiler, candidate, carried,
		review)
	if err := writeNew(filepath.Join(request.Out, "summary.txt"), []byte(summary+"\n")); err != nil {
		return nil, refusal("%v", err)
	}
	selected := make([]closure.ModuleID, 0, len(candidate.Modules))
	for _, module := range candidate.Modules {
		selected = append(selected, module.ID)
	}
	previousModules := []closure.ModuleID{}
	if previous != nil {
		previousModules = previous.Selection()
	}
	return &Proposal{Out: request.Out, Release: request.Release, Compiler: request.Compiler,
		Modules: selected, Previous: previousModules, ReviewCarried: carried,
		Summary: summary}, nil
}

// writeCandidate writes the five closure files and the rewritten Dockerfile assertion
// under Out. A proposal writes `pin.proposal.json` instead of `pin.json`.
func writeCandidate(request Request, candidate *closure.Candidate,
	provenance closure.Provenance, review *closure.Review, proposal bool) error {
	document, err := closure.Document(request.Release, request.Compiler, candidate, provenance,
		review)
	if err != nil {
		return refusal("%v", err)
	}
	data := filepath.Join(request.Out, "gopls-closure")
	for _, file := range []struct {
		name string
		raw  []byte
	}{
		{string(closure.FileGoMod), candidate.GoMod},
		{string(closure.FileGoSum), candidate.GoSum},
		{string(closure.FileRootGoMod), candidate.RootGoMod},
		{string(closure.FileLicenses), candidate.Licenses},
	} {
		if err := writeNew(filepath.Join(data, file.name), file.raw); err != nil {
			return refusal("%v", err)
		}
	}
	name := string(closure.FilePin)
	if proposal {
		name = ProposalFile
	}
	if err := writeNew(filepath.Join(data, name), document); err != nil {
		return refusal("%v", err)
	}
	source, err := os.ReadFile(request.Dockerfile)
	if err != nil {
		return refusal("cannot read the Dockerfile: %v", err)
	}
	rewritten, err := rewriteDockerfile(source, request.Release)
	if err != nil {
		return err
	}
	if err := writeNew(filepath.Join(request.Out, "Dockerfile"), rewritten); err != nil {
		return refusal("%v", err)
	}
	return nil
}

// ProposalFile is the retained two-pass artifact: it is not a readable pin, because
// its review fields are null. The shell writer retains and later binds it by name.
const ProposalFile = "pin.proposal.json"

// rewriteDockerfile replaces the single ARG assertion with the staged release. An
// absent or duplicated line is a refusal: the writer may not choose which one is the
// assertion.
func rewriteDockerfile(raw []byte, release closure.ReleaseVersion) ([]byte, error) {
	lines := strings.Split(string(raw), "\n")
	matches := []int{}
	for index, line := range lines {
		if strings.HasPrefix(line, "ARG GOPLS_VERSION=") {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		return nil, refusal("Dockerfile: expected exactly one ARG GOPLS_VERSION line")
	}
	lines[matches[0]] = "ARG GOPLS_VERSION=" + string(release)
	return []byte(strings.Join(lines, "\n")), nil
}

// writeNew creates one candidate file. It never overwrites: Out is a new directory,
// and a file that already exists means the caller reused a path.
func writeNew(path string, raw []byte) error {
	if err := os.MkdirAll(directoryOf(path), 0o755); err != nil {
		return err
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := handle.Write(raw); err != nil {
		_ = handle.Close()
		return err
	}
	return handle.Close()
}

// directoryOf is the parent of a path this package built itself.
func directoryOf(path string) string {
	index := strings.LastIndexByte(path, os.PathSeparator)
	if index <= 0 {
		return "."
	}
	return path[:index]
}

func orExec(runner gotool.Runner) gotool.Runner {
	if runner != nil {
		return runner
	}
	return gotool.ExecRunner{}
}
