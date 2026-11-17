package codewriter

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// newClient is one writer invocation with nowhere to write but this test.
func newClient(t *testing.T, root string) (*Client, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	return New(Environment{Root: root, Variables: map[string]string{}}, out, errOut), out, errOut
}

// TestCheckAcceptsTheCommittedTree: the offline check names the release, both
// platforms, the evidence count and the single Dockerfile slot.
func TestCheckAcceptsTheCommittedTree(t *testing.T) {
	root := repositoryRoot(t)
	client, out, errOut := newClient(t, root)
	if status := client.Check(); status != 0 {
		t.Fatalf("check: status %d: %s", status, errOut)
	}
	pin, _ := committedPin(t)
	report := out.String()
	for _, needle := range []string{
		"codex " + pin.Release + ": offline committed-input check",
		"  linux/amd64 x86_64-unknown-linux-musl " + pin.Platforms["linux/amd64"].SHA256,
		"  linux/arm64 aarch64-unknown-linux-musl " + pin.Platforms["linux/arm64"].SHA256,
		"  evidence: 2 files plus 2 structurally consistent bundles",
		"  images/standard/Dockerfile: ARG AGENT_VERSION_CODEX=" + pin.Release +
			", LABEL org.agent-vm.version.codex references it",
	} {
		if !strings.Contains(report, needle) {
			t.Errorf("the check report does not mention %q:\n%s", needle, report)
		}
	}
	if errOut.Len() != 0 {
		t.Errorf("a passing check wrote a diagnostic: %s", errOut)
	}
}

// TestCheckRefusesADockerfileWithoutTheSlot: the check reads the Dockerfile, and a slot
// that does not name the pinned release is a refusal rather than a report.
func TestCheckRefusesADockerfileWithoutTheSlot(t *testing.T) {
	root := fixtureRoot(t)
	broken := strings.ReplaceAll(string(readFile(t, filepath.Join(root, Dockerfile))),
		"LABEL "+Label+`="${`+Argument+`}"`, "RUN true")
	writeFixture(t, filepath.Join(root, Dockerfile), broken)
	client, out, errOut := newClient(t, root)
	if status := client.Check(); status != 1 {
		t.Fatalf("check: status %d", status)
	}
	if out.Len() != 0 {
		t.Errorf("a refused check reported the closure: %s", out)
	}
	if !strings.Contains(errOut.String(), "must reference ${AGENT_VERSION_CODEX}") {
		t.Errorf("the refusal does not name the label: %s", errOut)
	}
}

// TestRefreshRefusesGitHubActions: every write mode refuses CI before it does anything,
// so the fetch seam here is never reached.
func TestRefreshRefusesGitHubActions(t *testing.T) {
	client, out, errOut := newClient(t, repositoryRoot(t))
	client.Env.Variables["GITHUB_ACTIONS"] = "true"
	client.Fetch = failingFetcher{t}
	client.Verify = func(_, _, _, _, _ string) error {
		t.Errorf("the verifier ran under GitHub Actions")
		return nil
	}
	// An unusable tag proves the CI refusal precedes every other decision.
	if status := client.Refresh(context.Background(), Options{Version: "not-a-release"}); status != 1 {
		t.Fatalf("refresh: status %d", status)
	}
	if out.Len() != 0 {
		t.Errorf("the refusal wrote to stdout: %s", out)
	}
	if !strings.Contains(errOut.String(), "refusing GitHub Actions") {
		t.Errorf("the refusal does not name GitHub Actions: %s", errOut)
	}
}

// TestPublishStageRefusesGitHubActions: the staged-proposal path publishes as much as
// the refresh path, so it carries the same guard before it reads the proposal.
func TestPublishStageRefusesGitHubActions(t *testing.T) {
	client, _, errOut := newClient(t, repositoryRoot(t))
	client.Env.Variables["GITHUB_ACTIONS"] = "true"
	stageDir := filepath.Join(t.TempDir(), "absent")
	if status := client.PublishStage(stageDir); status != 1 {
		t.Fatalf("publish stage: status %d", status)
	}
	if !strings.Contains(errOut.String(), "refusing GitHub Actions") {
		t.Errorf("the refusal does not name GitHub Actions: %s", errOut)
	}
}

// TestRequireSameReleaseRefusesEveryChangedIdentity: a same-release repeat is only a
// no-op when every committed identity and the metadata projection are unchanged.
func TestRequireSameReleaseRefusesEveryChangedIdentity(t *testing.T) {
	pin, directory := committedPin(t)
	committedMetadata := readFile(t, filepath.Join(directory, codexpin.MetadataPath))
	identical := sameRelease{
		platforms: committedIdentities(pin),
		checksums: pin.Checksums.SHA256,
		metadata:  committedMetadata,
	}
	if err := requireSameRelease(pin, committedMetadata, identical); err != nil {
		t.Fatalf("an identical repeat was refused: %s", err)
	}

	// The mutable part of the API document changes on every fetch of the same tag; it
	// must not turn a repeat into a rewrite.
	mutable := rewriteMetadata(t, committedMetadata, func(document map[string]any) {
		document["body"] = "a rewritten release note"
		document["stargazers_count"] = int64(1)
	})
	ignored := identical
	ignored.metadata = mutable
	if err := requireSameRelease(pin, committedMetadata, ignored); err != nil {
		t.Errorf("a mutable metadata change was treated as an identity change: %s", err)
	}

	changedArchive := identical
	changedArchive.platforms = cloneIdentities(identical.platforms)
	arm := changedArchive.platforms["linux/arm64"]
	changedArchive.platforms["linux/arm64"] = identity{archive: strings.Repeat("11", 32),
		bundle: arm.bundle}
	if err := requireSameRelease(pin, committedMetadata, changedArchive); err == nil ||
		!strings.Contains(err.Error(), "changed for the already-pinned "+pin.Release) {
		t.Errorf("a changed archive digest reported %v", err)
	}

	changedBundle := identical
	changedBundle.platforms = cloneIdentities(identical.platforms)
	amd := changedBundle.platforms["linux/amd64"]
	changedBundle.platforms["linux/amd64"] = identity{archive: amd.archive,
		bundle: strings.Repeat("22", 32)}
	if err := requireSameRelease(pin, committedMetadata, changedBundle); err == nil ||
		!strings.Contains(err.Error(), "same-release evidence rewrite") {
		t.Errorf("a changed bundle digest reported %v", err)
	}

	changedChecksums := identical
	changedChecksums.checksums = strings.Repeat("33", 32)
	if err := requireSameRelease(pin, committedMetadata, changedChecksums); err == nil ||
		!strings.Contains(err.Error(), "refusing to re-pin it") {
		t.Errorf("a changed checksums digest reported %v", err)
	}

	changedTag := identical
	changedTag.metadata = rewriteMetadata(t, committedMetadata, func(document map[string]any) {
		document["tag_name"] = "rust-v0.0.1"
	})
	if err := requireSameRelease(pin, committedMetadata, changedTag); err == nil ||
		!strings.Contains(err.Error(), "security projection") {
		t.Errorf("a changed metadata projection reported %v", err)
	}

	changedAsset := identical
	changedAsset.metadata = rewriteMetadata(t, committedMetadata, func(document map[string]any) {
		assets, _ := document["assets"].([]any)
		if len(assets) == 0 {
			t.Fatalf("the committed metadata has no assets")
		}
		first, _ := assets[0].(map[string]any)
		first["digest"] = "sha256:" + strings.Repeat("44", 32)
	})
	if err := requireSameRelease(pin, committedMetadata, changedAsset); err == nil ||
		!strings.Contains(err.Error(), "security projection") {
		t.Errorf("a changed asset digest reported %v", err)
	}
}

func cloneIdentities(source map[string]identity) map[string]identity {
	cloned := make(map[string]identity, len(source))
	for platform, value := range source {
		cloned[platform] = value
	}
	return cloned
}

// rewriteMetadata edits the release metadata and re-encodes it: only the projection is
// compared, so the encoding of the copy does not matter.
func rewriteMetadata(t *testing.T, raw []byte, edit func(map[string]any)) []byte {
	t.Helper()
	document, err := strictjson.Decode(raw, "release metadata")
	if err != nil {
		t.Fatalf("the committed metadata does not decode: %s", err)
	}
	members, ok := document.(map[string]any)
	if !ok {
		t.Fatalf("the committed metadata is not an object")
	}
	edit(members)
	encoded, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("cannot re-encode the metadata: %s", err)
	}
	return encoded
}

// failingFetcher is a byte source that must never be reached.
type failingFetcher struct{ t *testing.T }

func (f failingFetcher) Get(_ context.Context, url string, _ int64) ([]byte, error) {
	f.t.Errorf("the writer fetched %s", url)
	return nil, refusal("unexpected fetch")
}
