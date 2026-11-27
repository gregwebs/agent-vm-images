package goplsstage

// The staging writer's own witnesses. The differential in
// script/test/gopls-stage-parity.py holds this writer and the Python stager to each
// other over the fixture closure and the real release; these tests hold the rules that
// a differential over accepted runs cannot see: the refusal before any child, the
// compiler that is not the pin, the release whose language is too new, the retained
// proposal, the carried review and the two Linux targets.
//
// The Go child is a fake here on purpose: no test in this file reaches the network or
// compiles gopls. What it records is the request the writer would have made, which is
// what the ordering rules are about (`gotool`'s own tests hold the environment).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
)

var (
	testRelease  = closure.ReleaseVersion("v1.2.3")
	testCompiler = closure.GoVersion("1.27.3")
)

const testGoMod = `module agent-vm.invalid/go-dev/gopls-closure

go 1.26.0

tool golang.org/x/tools/gopls

require (
	example.test/dep v1.0.0 // indirect
	golang.org/x/tools/gopls v1.2.3 // indirect
)
`

const testRootEvidence = `module golang.org/x/tools/gopls

go 1.26.0

require example.test/dep v1.0.0
`

// testGoSum records the one checksum a fixture cannot invent: the root module's
// `/go.mod` hash, which the loader derives from the evidence bytes themselves.
var testGoSum = `example.test/dep v1.0.0 h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
example.test/dep v1.0.0/go.mod h1:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=
golang.org/x/tools/gopls v1.2.3 h1:CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC=
golang.org/x/tools/gopls v1.2.3/go.mod ` + rootEvidenceHash(testRootEvidence) + "\n"

// rootEvidenceHash is h1: base64(sha256("<sha256 of the bytes>  go.mod\n")), the
// algorithm the go command records for a module's own go.mod.
func rootEvidenceHash(evidence string) string {
	inner := sha256.Sum256([]byte(evidence))
	line := hex.EncodeToString(inner[:]) + "  go.mod\n"
	outer := sha256.Sum256([]byte(line))
	return "h1:" + base64.StdEncoding.EncodeToString(outer[:])
}

// fakeGo answers the resolution and the two builds without a compiler. It records
// every child, so a rule about ordering can assert what had not run yet.
type fakeGo struct {
	work         string
	version      string
	goRoot       string
	rootEvidence string
	failBuild    string
	children     []gotool.Child
}

func newFakeGo(t *testing.T) *fakeGo {
	t.Helper()
	work := t.TempDir()
	fake := &fakeGo{work: work, version: "go" + string(testCompiler), goRoot: "/opt/go",
		rootEvidence: testRootEvidence}
	if err := os.WriteFile(filepath.Join(work, "root.mod"), []byte(fake.rootEvidence), 0o644); err != nil {
		t.Fatalf("write the evidence: %v", err)
	}
	info := `{"Version":"v1.2.3","Origin":{"VCS":"git","URL":"https://example.test/tools",` +
		`"Subdir":"gopls","Ref":"refs/tags/gopls/v1.2.3","Hash":"` + strings.Repeat("0", 40) + `"}}`
	if err := os.WriteFile(filepath.Join(work, "root.info"), []byte(info), 0o644); err != nil {
		t.Fatalf("write the origin metadata: %v", err)
	}
	return fake
}

// ran reports whether a child whose argv contains every fragment ran.
func (f *fakeGo) ran(fragments ...string) bool {
	for _, child := range f.children {
		joined := strings.Join(child.Argv, " ")
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(joined, fragment) {
				matched = false
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func (f *fakeGo) archs() []string {
	archs := []string{}
	for _, child := range f.children {
		if len(child.Argv) > 0 && child.Argv[0] == "build" {
			archs = append(archs, valueOf(child.Env, "GOARCH"))
		}
	}
	return archs
}

func valueOf(environment []string, key string) string {
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, key+"="); found {
			return value
		}
	}
	return ""
}

func (f *fakeGo) Run(child gotool.Child) (gotool.Result, error) {
	f.children = append(f.children, child)
	argv := child.Argv
	switch {
	case len(argv) == 2 && argv[0] == "env" && argv[1] == "GOVERSION":
		return gotool.Result{Stdout: f.version + "\n"}, nil
	case len(argv) == 2 && argv[0] == "env" && argv[1] == "GOROOT":
		return gotool.Result{Stdout: f.goRoot + "\n"}, nil
	case len(argv) >= 2 && argv[0] == "get" && argv[1] == "-tool":
		return gotool.Result{}, nil
	case len(argv) >= 2 && argv[0] == "mod" && argv[1] == "download":
		report, _ := json.Marshal(map[string]string{
			"Path": closure.Gopls, "Version": string(testRelease),
			"GoMod": filepath.Join(f.work, "root.mod"),
			"Info":  filepath.Join(f.work, "root.info")})
		return gotool.Result{Stdout: string(report)}, nil
	case len(argv) >= 2 && argv[0] == "mod" && argv[1] == "tidy" && len(argv) == 2:
		f.writeCandidate(child)
		return gotool.Result{}, nil
	case len(argv) >= 2 && (argv[1] == "edit" || argv[1] == "verify"):
		return gotool.Result{}, nil
	case len(argv) == 3 && argv[1] == "tidy" && argv[2] == "-diff":
		return gotool.Result{}, nil
	case len(argv) > 0 && argv[0] == "build":
		if f.failBuild != "" && valueOf(child.Env, "GOARCH") == f.failBuild {
			return gotool.Result{Status: 1, Stderr: "injected build failure"}, nil
		}
		return gotool.Result{}, nil
	case len(argv) >= 3 && argv[0] == "version" && argv[1] == "-m":
		return gotool.Result{Stdout: buildInfo(valueOf(child.Env, "GOARCH"))}, nil
	}
	return gotool.Result{Status: 1, Stderr: "fake: unexpected child " + strings.Join(argv, " ")},
		nil
}

// writeCandidate writes what the real `go mod tidy` would produce, and the cache
// notices the notice owner reads.
func (f *fakeGo) writeCandidate(child gotool.Child) {
	_ = os.WriteFile(filepath.Join(child.Dir, "go.mod"), []byte(testGoMod), 0o644)
	_ = os.WriteFile(filepath.Join(child.Dir, "go.sum"), []byte(testGoSum), 0o644)
	cache := valueOf(child.Env, "GOMODCACHE")
	for _, id := range []closure.ModuleID{
		{Path: "example.test/dep", Version: "v1.0.0"},
		{Path: closure.Gopls, Version: closure.ModuleVersion(testRelease)},
	} {
		directory := closure.ModuleCacheDirectory(cache, id)
		_ = os.MkdirAll(directory, 0o755)
		_ = os.WriteFile(filepath.Join(directory, "LICENSE"),
			[]byte(string(id.Path)+" license\n"), 0o644)
	}
}

// buildInfo is the document the pinned compiler would report for one target: the same
// selection, sums and reviewed Linux settings.
func buildInfo(arch string) string {
	architecture := gotool.AMD64
	if arch == "arm64" {
		architecture = gotool.ARM64
	}
	settings := []map[string]string{}
	for key, value := range expectedSettingsForTest(architecture) {
		settings = append(settings, map[string]string{"Key": key, "Value": value})
	}
	document := map[string]any{
		"GoVersion": "go" + string(testCompiler),
		"Main":      map[string]string{"Path": closure.Gopls, "Version": string(testRelease), "Sum": h1Of(testGoSum, closure.Gopls)},
		"Deps": []map[string]string{{"Path": "example.test/dep", "Version": "v1.0.0",
			"Sum": "h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}},
		"Settings": settings,
	}
	raw, _ := json.Marshal(document)
	return string(raw)
}

// h1Of is the zip checksum a fixture go.sum records for one module.
func h1Of(goSum, path string) string {
	for _, line := range strings.Split(goSum, "\n") {
		fields := strings.Split(line, " ")
		if len(fields) == 3 && fields[0] == path && !strings.HasSuffix(fields[1], "/go.mod") {
			return fields[2]
		}
	}
	return ""
}

// expectedSettingsForTest is buildinfo.ExpectedSettings in the map form the document
// needs, read from the owner so a settings change is one edit.
func expectedSettingsForTest(arch gotool.Architecture) map[string]string {
	settings := map[string]string{
		"-buildmode": "exe", "-compiler": "gc", "-trimpath": "true",
		"DefaultGODEBUG": "tracebacklabels=0,x509sslcertoverrideplatform=0",
		"CGO_ENABLED":    "0", "GOOS": "linux", "GOARCH": arch.Value(),
	}
	if arch == gotool.AMD64 {
		settings["GOAMD64"] = "v1"
	} else {
		settings["GOARM64"] = "v8.0"
	}
	return settings
}

// request is one staging run in a fresh directory, with the fixture's Dockerfile.
func request(t *testing.T, work string) Request {
	t.Helper()
	dockerfile := filepath.Join(work, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("ARG GO_VERSION=1.27.3\nFROM base\n"+
		"ARG GOPLS_VERSION=v0.0.1\n"), 0o644); err != nil {
		t.Fatalf("write the Dockerfile: %v", err)
	}
	return Request{Release: testRelease, Compiler: testCompiler,
		Current: filepath.Join(work, "no-current"), Out: filepath.Join(work, "out"),
		Dockerfile: dockerfile, Go: filepath.Join("/opt/go/bin", "go")}
}

// deps is the fixture's injected dependency set.
func deps(fake *fakeGo) Dependencies {
	return Dependencies{Environment: gotool.Install(), Runner: fake, ScratchRoot: fake.work}
}

func reviewed(request *Request) {
	date := closure.Date("2026-01-02")
	request.ReviewedOn = &date
	request.Reviewer = "fixture-reviewer"
}

// TestStageRefusesInGitHubActions: the refusal costs no child, no output and no
// request, and the same request without CI does run children -- the positive control
// that keeps "zero children" from passing over a writer that never works.
func TestStageRefusesInGitHubActions(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	refused := request(t, work)
	refused.Out = filepath.Join(work, "ci-out")
	dependencies := deps(fake)
	dependencies.GitHubActions = true
	_, err := Stage(context.Background(), refused, dependencies)
	if err == nil || !strings.Contains(err.Error(), "refused in GitHub Actions") {
		t.Fatalf("CI was not refused: %v", err)
	}
	if len(fake.children) != 0 {
		t.Fatalf("the CI refusal ran %d children", len(fake.children))
	}
	if _, err := os.Lstat(refused.Out); err == nil {
		t.Fatal("the CI refusal wrote its --out directory")
	}
	accepted := request(t, work)
	accepted.Out = filepath.Join(work, "local-out")
	reviewed(&accepted)
	if _, err := Stage(context.Background(), accepted, deps(fake)); err != nil {
		t.Fatalf("the same request without CI was refused: %v", err)
	}
	if len(fake.children) == 0 {
		t.Fatal("the control run made no child at all: the zero above is not evidence")
	}
}

// TestOutMustBeNew: a candidate directory is never reused.
func TestOutMustBeNew(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	pinned := request(t, work)
	reviewed(&pinned)
	if err := os.MkdirAll(pinned.Out, 0o755); err != nil {
		t.Fatalf("create --out: %v", err)
	}
	_, err := Stage(context.Background(), pinned, deps(fake))
	if err == nil || !strings.Contains(err.Error(), "must be a new invocation-owned path") {
		t.Fatalf("an existing --out was accepted: %v", err)
	}
	if len(fake.children) != 0 {
		t.Fatal("the --out refusal ran a child")
	}
}

// TestPartialCurrentRefuses: a pinned directory that does not load is recovered by a
// human, never overwritten.
func TestPartialCurrentRefuses(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	pinned := request(t, work)
	reviewed(&pinned)
	partial := filepath.Join(work, "partial")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatalf("create the partial pin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(partial, "go.mod"), []byte(testGoMod), 0o644); err != nil {
		t.Fatalf("write the partial pin: %v", err)
	}
	pinned.Current = partial
	_, err := Stage(context.Background(), pinned, deps(fake))
	if err == nil || !strings.Contains(err.Error(), "missing input:") {
		t.Fatalf("partial pinned state was accepted: %v", err)
	}
	if len(fake.children) != 0 {
		t.Fatal("the partial-state refusal ran a child")
	}
}

// TestWrongCompilerMakesNoResolution: the compiler is read from the child before any
// resolution, so a wrong one costs one `go env` and no request.
func TestWrongCompilerMakesNoResolution(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	fake.version = "go1.28.0"
	pinned := request(t, work)
	reviewed(&pinned)
	_, err := Stage(context.Background(), pinned, deps(fake))
	if err == nil || !strings.Contains(err.Error(),
		"maintenance compiler is go1.28.0, want go1.27.3") {
		t.Fatalf("a wrong compiler was accepted: %v", err)
	}
	if len(fake.children) != 1 {
		t.Fatalf("%d children ran before the refusal, want the one version read",
			len(fake.children))
	}
	if fake.ran("-tool") || fake.ran("download") {
		t.Fatal("the wrong compiler reached resolution")
	}
}

// TestGoDirectiveNewerThanThePinRefuses: the upstream release's own `go` directive is
// read from the downloaded evidence, and a release that needs a newer language is
// refused before the wrapper is retargeted.
func TestGoDirectiveNewerThanThePinRefuses(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	fake.rootEvidence = strings.Replace(testRootEvidence, "go 1.26.0", "go 1.28.0", 1)
	if err := os.WriteFile(filepath.Join(fake.work, "root.mod"), []byte(fake.rootEvidence),
		0o644); err != nil {
		t.Fatalf("rewrite the evidence: %v", err)
	}
	pinned := request(t, work)
	reviewed(&pinned)
	_, err := Stage(context.Background(), pinned, deps(fake))
	if err == nil || !strings.Contains(err.Error(),
		"gopls v1.2.3 requires go >= 1.28.0; the layer pins 1.27.3") {
		t.Fatalf("a release needing a newer compiler was accepted: %v", err)
	}
	if fake.ran("edit") {
		t.Fatal("the wrapper was retargeted before the language rule was applied")
	}
}

// TestStageCrossBuildsBothTargets: the candidate is written and both Linux targets are
// compiled, in the reviewed order, from the staged files.
func TestStageCrossBuildsBothTargets(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	pinned := request(t, work)
	reviewed(&pinned)
	proposal, err := Stage(context.Background(), pinned, deps(fake))
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if archs := fake.archs(); strings.Join(archs, ",") != "amd64,arm64" {
		t.Fatalf("built %v, want both Linux targets in the reader's order", archs)
	}
	for _, name := range []string{"gopls-closure/go.mod", "gopls-closure/go.sum",
		"gopls-closure/gopls.go.mod", "gopls-closure/pin.json",
		"gopls-closure/THIRD_PARTY_LICENSES", "Dockerfile", "summary.txt"} {
		if _, err := os.Stat(filepath.Join(pinned.Out, filepath.FromSlash(name))); err != nil {
			t.Fatalf("%s was not written: %v", name, err)
		}
	}
	if proposal.ReviewCarried {
		t.Fatal("a first adoption carried a review")
	}
	if !strings.Contains(proposal.Summary, "determination: carried=False reviewer=") {
		t.Fatalf("the summary does not name the review: %s", proposal.Summary)
	}
	document := readDocumentForTest(t, filepath.Join(pinned.Out, "gopls-closure", "pin.json"))
	authentication := document["authentication"].(map[string]any)
	if authentication["reviewer"] != "fixture-reviewer" ||
		authentication["reviewed_on"] != "2026-01-02" {
		t.Fatalf("the record does not carry the supplied review: %v", authentication)
	}
}

// TestRenewalNeedsReview: a candidate whose review is not carried and whose scope
// nobody reviewed is written as a proposal and refused -- never published as a pin.
func TestRenewalNeedsReview(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	pinned := request(t, work)
	_, err := Stage(context.Background(), pinned, deps(fake))
	if err == nil || !strings.Contains(err.Error(), closure.Renewal) {
		t.Fatalf("an unreviewed candidate was accepted: %v", err)
	}
	if _, stat := os.Stat(filepath.Join(pinned.Out, "gopls-closure", ProposalFile)); stat != nil {
		t.Fatalf("the proposal was not retained: %v", stat)
	}
	if _, stat := os.Stat(filepath.Join(pinned.Out, "gopls-closure", "pin.json")); stat == nil {
		t.Fatal("an unreviewed candidate wrote a readable pin")
	}
	if _, stat := os.Stat(filepath.Join(pinned.Out, "summary.txt")); stat == nil {
		t.Fatal("a refused proposal wrote a summary")
	}
}

// TestCarriedReviewIsRetained: a same-scope rerun derives the previous record's review
// and needs no flags.
func TestCarriedReviewIsRetained(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	first := request(t, work)
	reviewed(&first)
	first.Out = filepath.Join(work, "first")
	if _, err := Stage(context.Background(), first, deps(fake)); err != nil {
		t.Fatalf("the first adoption was refused: %v", err)
	}
	second := request(t, work)
	second.Current = filepath.Join(first.Out, "gopls-closure")
	second.Out = filepath.Join(work, "second")
	proposal, err := Stage(context.Background(), second, deps(fake))
	if err != nil {
		t.Fatalf("a same-release rerun was refused: %v", err)
	}
	if !proposal.ReviewCarried {
		t.Fatal("the same-release rerun did not carry the review")
	}
	if !strings.Contains(proposal.Summary, "carried=True reviewer=fixture-reviewer") {
		t.Fatalf("the summary does not name the carried review: %s", proposal.Summary)
	}
}

// TestDockerfileAssertion: exactly one ARG line is rewritten; none or two is a refusal,
// because the writer may not choose which one is the assertion.
func TestDockerfileAssertion(t *testing.T) {
	rewritten, err := rewriteDockerfile([]byte("FROM x\nARG GOPLS_VERSION=v0.0.1\n"), "v1.2.3")
	if err != nil || string(rewritten) != "FROM x\nARG GOPLS_VERSION=v1.2.3\n" {
		t.Fatalf("rewrite: %q (%v)", rewritten, err)
	}
	for _, raw := range []string{"FROM x\n", "ARG GOPLS_VERSION=v0.0.1\nARG GOPLS_VERSION=v0.0.2\n"} {
		if _, err := rewriteDockerfile([]byte(raw), "v1.2.3"); err == nil ||
			!strings.Contains(err.Error(), "expected exactly one ARG GOPLS_VERSION line") {
			t.Fatalf("%q was accepted (%v)", raw, err)
		}
	}
}

// TestFailedBuildIsRefused: a build failure is the caller's error, not a proposal.
func TestFailedBuildIsRefused(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	fake.failBuild = "arm64"
	pinned := request(t, work)
	reviewed(&pinned)
	_, err := Stage(context.Background(), pinned, deps(fake))
	if err == nil {
		t.Fatal("a failed cross-build was accepted")
	}
	if !fake.ran("build") || len(fake.archs()) != 2 {
		t.Fatalf("the second target was not attempted: %v", fake.archs())
	}
	if _, stat := os.Stat(filepath.Join(pinned.Out, "gopls-closure", "pin.json")); stat == nil {
		t.Fatal("a failed cross-build wrote a record")
	}
}

func readDocumentForTest(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return document
}

// TestResolveGoIsTheFirstExecutableOnPath: the writer names the compiler it runs; an
// empty PATH entry is not the current directory, and a non-executable file is skipped.
func TestResolveGoIsTheFirstExecutableOnPath(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "go"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write a non-executable go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(second, "go"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write the executable go: %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{"", first, second}, string(os.PathListSeparator)))
	resolved, err := ResolveGo()
	if err != nil {
		t.Fatalf("ResolveGo: %v", err)
	}
	if want := filepath.Join(second, "go"); resolved != want {
		t.Fatalf("resolved %s, want %s", resolved, want)
	}
	t.Setenv("PATH", first)
	if _, err := ResolveGo(); err == nil ||
		!strings.Contains(err.Error(), "missing prerequisite: go") {
		t.Fatalf("a non-executable go was accepted: %v", err)
	}
}

// TestScratchIsRemoved: the invocation's work tree belongs to this run. A retained tree
// is reported, not silently left, and a successful run leaves nothing behind.
func TestScratchIsRemoved(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	pinned := request(t, work)
	reviewed(&pinned)
	if _, err := Stage(context.Background(), pinned, deps(fake)); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	entries, err := os.ReadDir(fake.work)
	if err != nil {
		t.Fatalf("read the scratch root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "gopls-stage-") {
			t.Fatalf("the invocation scratch tree %s survived", entry.Name())
		}
	}
}

// TestCompilePhaseIsOffline: the two cross-builds run with the proxy off and no
// transport, whatever origin the resolution used. A staged candidate must be complete
// from its own files and the authenticated cache; a build that fetched would hide a
// missing module behind the network. The differential found this rule the hard way --
// the Go side requested `.info` objects Python's offline build never asked for.
func TestCompilePhaseIsOffline(t *testing.T) {
	work := t.TempDir()
	fake := newFakeGo(t)
	pinned := request(t, work)
	reviewed(&pinned)
	dependencies := deps(fake)
	// The resolution's own origin: a fixture recorder here, the reviewed proxy in
	// production. Either way the compile phase below must not see it.
	dependencies.Environment = gotool.NewEnvironment("http://127.0.0.1:1", "off")
	if _, err := Stage(context.Background(), pinned, dependencies); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	builds := 0
	for _, child := range fake.children {
		if valueOf(child.Env, "GOTOOLCHAIN") != "local" {
			continue
		}
		if proxy := valueOf(child.Env, "GOPROXY"); proxy != "off" {
			t.Fatalf("%v ran with GOPROXY=%q, want off", child.Argv, proxy)
		}
		for _, entry := range child.Env {
			if strings.HasPrefix(entry, "HTTP_PROXY=") || strings.HasPrefix(entry, "HTTPS_PROXY=") {
				t.Fatalf("%v carried a transport variable: %s", child.Argv, entry)
			}
		}
		if len(child.Argv) > 0 && child.Argv[0] == "build" {
			builds++
		}
	}
	if builds != 2 {
		t.Fatalf("%d offline builds, want both Linux targets", builds)
	}
}
