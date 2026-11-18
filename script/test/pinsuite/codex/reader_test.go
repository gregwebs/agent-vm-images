package codex

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// readerFixture is the closure the reader group is pointed at. It is built once per
// case, so a case that mutates nothing cannot be affected by another one.
func readerFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, filepath.Join(scratch(t, "reader"), "source"), Release, nil)
}

// TestReaderCheckPrintsTheOfflineTranscript: the closure the fixture builder wrote is
// one the reader accepts, and the transcript it prints starts and ends with the two
// lines a consumer parses.
func TestReaderCheckPrintsTheOfflineTranscript(t *testing.T) {
	built := readerFixture(t)
	printed := accepts(t, pinInvocation("check", built.dir, "--release", Release)).Stdout
	if !strings.HasPrefix(printed, "codex "+Release+": offline committed-input check") {
		t.Fatalf("check printed %q", printed)
	}
	if !strings.HasSuffix(printed, "structurally consistent bundles\n") {
		t.Fatalf("check printed %q", printed)
	}
}

// TestReaderSelectPrintsFourExactFieldsPerTarget: the four positional fields, taken
// from the bytes this fixture committed, for both maintained targets.
func TestReaderSelectPrintsFourExactFieldsPerTarget(t *testing.T) {
	built := readerFixture(t)
	for _, name := range platforms {
		triple := targetOf(name)
		asset := assetName(triple)
		printed := accepts(t, pinInvocation("select", built.dir, "--release", Release,
			"--target", string(triple))).Stdout
		want := fmt.Sprintf("%s\n%s\n%s\n%s\n", built.version, asset,
			assetURL(Release, asset), harness.SHA256Hex(built.archives[name]))
		if printed != want {
			t.Fatalf("select %s printed %q, want %q", triple, printed, want)
		}
	}
}

// TestReaderAnExactButUnpinnedReleaseIsRefused: an exact tag that is not the pinned
// one needs a reviewed pin, so it is refused rather than resolved.
func TestReaderAnExactButUnpinnedReleaseIsRefused(t *testing.T) {
	built := readerFixture(t)
	refuses(t, pinInvocation("check", built.dir, "--release", NextRelease),
		"not the pinned Codex release", NextRelease)
}

// TestReaderAClosureWithNoPinJSONIsRefused: a directory with no closure is named by
// where pin.json is missing.
func TestReaderAClosureWithNoPinJSONIsRefused(t *testing.T) {
	built := readerFixture(t)
	refuses(t, pinInvocation("check", filepath.Join(built.dir, "absent"), "--release", Release),
		"is missing at", "pin.json")
}

// TestReaderAReleasedTagThatIsNotExactIsRefused: `latest`, a bare version and a
// leading-zero version are all refused by name.
func TestReaderAReleasedTagThatIsNotExactIsRefused(t *testing.T) {
	built := readerFixture(t)
	for _, candidate := range []struct {
		release string
		needle  string
	}{
		{"rust-v0.159", "not an exact Codex version"},
		{"latest", "must be rust-v<version>"},
		{Version, "must be rust-v<version>"},
		{"rust-v01.2.3", "not an exact Codex version"},
	} {
		refuses(t, pinInvocation("check", built.dir, "--release", candidate.release), candidate.needle)
	}
}

// TestReaderAnUnmaintainedTargetIsRefused: only the two maintained triples exist, and
// the refusal names the ones that do.
func TestReaderAnUnmaintainedTargetIsRefused(t *testing.T) {
	built := readerFixture(t)
	refuses(t, pinInvocation("select", built.dir, "--release", Release,
		"--target", "x86_64-unknown-linux-gnu"), "must be one of", "unknown-linux-musl")
}

// TestReaderAMalformedInvocationExitsTwoWithTheReaderPrefix: a flag that does not
// belong to the verb is malformed, not ignored, and a usage error is status 2.
func TestReaderAMalformedInvocationExitsTwoWithTheReaderPrefix(t *testing.T) {
	built := readerFixture(t)
	pinned := []string{"--pin-dir", built.dir, "--release", Release}
	target := []string{"--target", string(targetAMD64)}
	for _, testCase := range []struct {
		argv   []string
		needle string
	}{
		{append([]string{"version"}, pinned...), "unknown verb version"},
		{[]string{"check", "--release", Release}, "check needs --pin-dir"},
		{append(append([]string{"check"}, pinned...), target...), "check does not take --target"},
		{append([]string{"select"}, pinned...), "select needs --target"},
		{append(append(append([]string{"select"}, pinned...), target...), "--archive", "/nonexistent"),
			"select does not take --archive"},
		{append(append([]string{"preflight"}, pinned...), target...), "preflight needs --archive"},
	} {
		usageRefused(t, testCase.argv, testCase.needle)
	}
}
