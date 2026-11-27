package closure

// The staging owner's rules, held to the committed record and to named refusals.
//
// The round-trip test is the important one: the committed five files plus a module
// cache holding exactly the committed notice bytes must re-derive the committed
// `pin.json` byte for byte. That is the same statement the maintainer stager makes
// about a new release, taken over bytes a human already reviewed, and it fails when
// the canonicalization, the sum derivation, the notice layout, the signature
// projection, the digests or the document's key order drifts.
//
// Every rule below also names the test that fails when it is deleted, and the
// deletion mutations are run in the #252 PR (owner ruling R-c).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loaded is the committed closure, with the case failing rather than running over
// nothing when it does not load.
func loaded(t *testing.T) *Data {
	t.Helper()
	data, err := load(t, committedDir(t))
	if err != nil {
		t.Fatalf("the committed closure does not load: %v", err)
	}
	return data
}

// noticeCache writes the committed notice bytes into a module cache the way the
// resolution leaves it: one directory per module, the reviewed relative paths inside.
func noticeCache(t *testing.T, data *Data) string {
	t.Helper()
	cache := filepath.Join(t.TempDir(), "mod")
	for _, section := range data.Notices {
		directory := ModuleCacheDirectory(cache, section.ID)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("create %s: %v", directory, err)
		}
		path := filepath.Join(directory, filepath.FromSlash(string(section.Name)))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, section.Raw, 0o644); err != nil {
			t.Fatalf("write %s: %v", section.Name, err)
		}
	}
	return cache
}

// candidateRequest is the committed closure as the stager would have resolved it.
func candidateRequest(t *testing.T, data *Data, cache string) CandidateRequest {
	t.Helper()
	raw := func(name Filename) []byte {
		content, err := os.ReadFile(filepath.Join(committedDir(t), string(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return content
	}
	return CandidateRequest{Release: data.Pin.Gopls, Compiler: data.Pin.Go,
		GoMod: raw(FileGoMod), GoSum: raw(FileGoSum), RootGoMod: raw(FileRootGoMod),
		ModuleCache: cache}
}

// TestDeriveCandidateReproducesTheCommittedRecord: the staging owner derives the
// committed selection, notice evidence, digests and signatures, and the document it
// renders is the committed `pin.json`, byte for byte.
func TestDeriveCandidateReproducesTheCommittedRecord(t *testing.T) {
	data := loaded(t)
	request := candidateRequest(t, data, noticeCache(t, data))
	candidate, err := DeriveCandidate(request)
	if err != nil {
		t.Fatalf("DeriveCandidate: %v", err)
	}
	if len(candidate.Modules) != len(data.Pin.Modules) || len(candidate.Modules) < 20 {
		t.Fatalf("%d derived modules, want the committed %d", len(candidate.Modules),
			len(data.Pin.Modules))
	}
	if len(candidate.Notices) != len(data.Notices) || len(candidate.Notices) < 30 {
		t.Fatalf("%d derived notice sections, want the committed %d", len(candidate.Notices),
			len(data.Notices))
	}
	if string(candidate.Licenses) != string(readInput(t, FileLicenses)) {
		t.Fatal("the rendered notice evidence differs from the committed file")
	}
	for _, name := range FileKeys {
		if candidate.Files[name] != data.Pin.Files[name] {
			t.Fatalf("files[%s] = %s, want the committed %s", name, candidate.Files[name],
				data.Pin.Files[name])
		}
	}
	if !sameSignatures(candidate.Signatures, data.Pin.Review.Signatures) {
		t.Fatalf("the signature projection %v differs from the committed inventory %v",
			candidate.Signatures, data.Pin.Review.Signatures)
	}
	if !Carried(data, data.Pin.Gopls, candidate) {
		t.Fatal("the committed record does not carry its own review: the carry predicate " +
			"would refuse a same-release rerun")
	}
	review := data.Pin.Review
	rendered, err := Document(data.Pin.Gopls, data.Pin.Go, candidate, data.Pin.Provenance,
		&review)
	if err != nil {
		t.Fatalf("Document: %v", err)
	}
	committed := readInput(t, FilePin)
	if string(rendered) != string(committed) {
		t.Fatalf("the rendered record differs from the committed pin.json:\n%s",
			firstDifference(string(committed), string(rendered)))
	}
	// The proposal form keeps the review fields null rather than inventing a date.
	proposed, err := Document(data.Pin.Gopls, data.Pin.Go, candidate, data.Pin.Provenance, nil)
	if err != nil {
		t.Fatalf("Document (proposal): %v", err)
	}
	if !strings.Contains(string(proposed), "\"reviewed_on\": null") ||
		!strings.Contains(string(proposed), "\"reviewer\": null") {
		t.Fatal("a proposal records a review it does not have")
	}
}

// readInput is one committed input's bytes.
func readInput(t *testing.T, name Filename) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(committedDir(t), string(name)))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return content
}

// firstDifference names the first differing line, so a failure is actionable.
func firstDifference(want, got string) string {
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	for index := 0; index < len(wantLines) && index < len(gotLines); index++ {
		if wantLines[index] != gotLines[index] {
			return "line " + itoa(index+1) + ":\n  committed: " + wantLines[index] +
				"\n  rendered:  " + gotLines[index]
		}
	}
	return "the documents differ in length: " + itoa(len(wantLines)) + " vs " +
		itoa(len(gotLines)) + " lines"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

// TestDeriveCandidateRefuses: one rule per case, each with the fragment it must name.
// A candidate the loader would refuse must be refused before the first cross-build,
// so a stager that writes it can never present it as reviewed bytes.
func TestDeriveCandidateRefuses(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(t *testing.T, request *CandidateRequest, cache string)
		fragment string
	}{
		{"a wrapper that is not in the canonical form", func(t *testing.T, request *CandidateRequest,
			_ string) {
			request.GoMod = append(request.GoMod, []byte("\n")...)
			request.GoMod = []byte(strings.Replace(string(request.GoMod), "tool "+Gopls+"\n\n",
				"tool "+Gopls+"\n", 1))
		}, "not in the reviewed canonical form"},
		{"a wrapper that selects another release", func(t *testing.T, request *CandidateRequest,
			_ string) {
			request.Release = "v0.24.0"
		}, "is v0.23.0, not v0.24.0"},
		{"a wrapper without the tool module", func(t *testing.T, request *CandidateRequest,
			_ string) {
			lines := []string{}
			for _, line := range strings.Split(string(request.GoMod), "\n") {
				if strings.HasPrefix(line, "\tgolang.org/x/tools/gopls ") {
					continue
				}
				lines = append(lines, line)
			}
			request.GoMod = []byte(strings.Join(lines, "\n"))
		}, "the tool module " + Gopls + " is not required"},
		{"a go.sum without a zip hash", func(t *testing.T, request *CandidateRequest, _ string) {
			lines := []string{}
			for _, line := range strings.Split(string(request.GoSum), "\n") {
				if strings.HasPrefix(line, "mvdan.cc/gofumpt v0.10.0 h1:") {
					continue
				}
				lines = append(lines, line)
			}
			request.GoSum = []byte(strings.Join(lines, "\n"))
		}, "go.sum lacks h1 for mvdan.cc/gofumpt@v0.10.0"},
		{"a go.sum without a go.mod hash", func(t *testing.T, request *CandidateRequest,
			_ string) {
			lines := []string{}
			for _, line := range strings.Split(string(request.GoSum), "\n") {
				if strings.HasPrefix(line, "mvdan.cc/gofumpt v0.10.0/go.mod h1:") {
					continue
				}
				lines = append(lines, line)
			}
			request.GoSum = []byte(strings.Join(lines, "\n"))
		}, "go.sum lacks go.mod hash for mvdan.cc/gofumpt@v0.10.0"},
		{"a compiler older than the go directive", func(t *testing.T, request *CandidateRequest,
			_ string) {
			request.Compiler = "1.25.0"
		}, "newer than the pinned compiler go1.25.0"},
		{"upstream evidence that does not name a selected module", func(t *testing.T,
			request *CandidateRequest, _ string) {
			lines := []string{}
			for _, line := range strings.Split(string(request.RootGoMod), "\n") {
				if strings.HasPrefix(line, "\tgithub.com/fatih/structtag ") {
					continue
				}
				lines = append(lines, line)
			}
			request.RootGoMod = []byte(strings.Join(lines, "\n"))
		}, "is not required by the root module"},
		{"a module cache missing one module's notices", func(t *testing.T, request *CandidateRequest,
			cache string) {
			if err := os.RemoveAll(filepath.Join(cache, "mvdan.cc", "gofumpt@v0.10.0")); err != nil {
				t.Fatalf("remove the module directory: %v", err)
			}
		}, "license evidence missing for mvdan.cc/gofumpt"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			data := loaded(t)
			cache := noticeCache(t, data)
			request := candidateRequest(t, data, cache)
			testCase.mutate(t, &request, cache)
			_, err := DeriveCandidate(request)
			if err == nil {
				t.Fatalf("the candidate was accepted: this rule has no witness")
			}
			if testCase.fragment != "" && !strings.Contains(err.Error(), testCase.fragment) {
				t.Fatalf("refusal %q does not name %q", err, testCase.fragment)
			}
		})
	}
}

// TestDeriveCandidateRefusesAnUnreviewedNestedNotice: the layout rule is the one the
// ADR inspected. A notice the record has not seen stops maintenance instead of being
// blessed in passing, and the refusal names the module.
func TestDeriveCandidateRefusesAnUnreviewedNestedNotice(t *testing.T) {
	data := loaded(t)
	cache := noticeCache(t, data)
	directory := ModuleCacheDirectory(cache,
		ModuleID{Path: "github.com/fatih/camelcase", Version: "v1.0.0"})
	if err := os.MkdirAll(filepath.Join(directory, "sub"), 0o755); err != nil {
		t.Fatalf("create the nested directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, "sub", "LICENSE"), []byte("nested\n"),
		0o644); err != nil {
		t.Fatalf("write the nested notice: %v", err)
	}
	request := candidateRequest(t, data, cache)
	_, err := DeriveCandidate(request)
	if err == nil || !strings.Contains(err.Error(), "unsupported license layout") {
		t.Fatalf("an unreviewed nested notice was accepted: %v", err)
	}
}

// TestCarriedRefusesADifferentSelection: a review covers the selection it was taken
// over. One changed content hash, one changed notice digest or one changed policy
// constant must stop the carry, or an old review would silently extend over new bytes.
func TestCarriedRefusesADifferentSelection(t *testing.T) {
	data := loaded(t)
	request := candidateRequest(t, data, noticeCache(t, data))
	candidate, err := DeriveCandidate(request)
	if err != nil {
		t.Fatalf("DeriveCandidate: %v", err)
	}
	if !Carried(data, data.Pin.Gopls, candidate) {
		t.Fatal("the committed record does not carry its own review")
	}
	changedHash := *candidate
	changedHash.Modules = append([]PinnedModule{}, candidate.Modules...)
	changedHash.Modules[0].H1 = H1("h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if Carried(data, data.Pin.Gopls, &changedHash) {
		t.Fatal("a different content hash did not stop the carry")
	}
	changedNotice := *candidate
	changedNotice.Modules = append([]PinnedModule{}, candidate.Modules...)
	changedNotice.Modules[0].Licenses = append([]LicenseFile{}, candidate.Modules[0].Licenses...)
	changedNotice.Modules[0].Licenses[0].SHA256 = SHA256("0" +
		string(candidate.Modules[0].Licenses[0].SHA256)[1:])
	if Carried(data, data.Pin.Gopls, &changedNotice) {
		t.Fatal("a different notice digest did not stop the carry")
	}
	if Carried(data, ReleaseVersion("v0.24.0"), candidate) {
		t.Fatal("a different release did not stop the carry")
	}
	if Carried(nil, data.Pin.Gopls, candidate) {
		t.Fatal("a first adoption carried a review")
	}
}

// TestProjectSignaturesRefusesAVersionTheInventoryHasNotSeen: a selected path at
// another version is a change, not an absence.
func TestProjectSignaturesRefusesAVersionTheInventoryHasNotSeen(t *testing.T) {
	modules := []PinnedModule{{ID: ModuleID{Path: "github.com/fsnotify/fsnotify",
		Version: "v1.8.0"}, H1: "h1:deadbeef"}}
	_, err := ProjectSignatures(modules)
	if err == nil || !strings.Contains(err.Error(),
		"signature inventory needs review for github.com/fsnotify/fsnotify@v1.8.0") {
		t.Fatalf("a stale inventory row was projected: %v", err)
	}
	// An unlisted path makes no claim either way.
	projected, err := ProjectSignatures([]PinnedModule{{ID: ModuleID{Path: "example.test/dep",
		Version: "v1.0.0"}}})
	if err != nil || len(projected) != 0 {
		t.Fatalf("an unlisted module produced %v (%v)", projected, err)
	}
}

// TestEscapeModule: the module cache spelling of an uppercase component. The
// installer reads the same cache through this rule.
func TestEscapeModule(t *testing.T) {
	for input, want := range map[string]string{
		"github.com/BurntSushi/toml": "github.com/!burnt!sushi/toml",
		"example.test/dep":           "example.test/dep",
		"v1.0.0":                     "v1.0.0",
	} {
		if got := EscapeModule(input); got != want {
			t.Fatalf("EscapeModule(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestCollectNoticesRefusesASymlink: a notice path that is a symlink is an
// unsupported layout, not a file to follow into somewhere the record never named.
func TestCollectNoticesRefusesASymlink(t *testing.T) {
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "LICENSE")
	if err := os.WriteFile(outside, []byte("outside\n"), 0o644); err != nil {
		t.Fatalf("write the target: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "LICENSE")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := CollectNotices(directory, ModuleID{Path: "example.test/dep", Version: "v1.0.0"})
	if err == nil || !strings.Contains(err.Error(), "a notice path is a symlink") {
		t.Fatalf("a symlinked notice was followed: %v", err)
	}
}
