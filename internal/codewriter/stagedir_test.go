package codewriter

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// fixtureRoot is a private copy of the committed closure and the Dockerfile, so a test
// can publish without touching the checkout.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source := repositoryRoot(t)
	for _, name := range stageNames() {
		if name == dockerfileName {
			continue
		}
		relative := destinationOf(name)
		destination := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			t.Fatalf("cannot create %s: %s", filepath.Dir(destination), err)
		}
		writeFixture(t, destination, string(readFile(t, filepath.Join(source, filepath.FromSlash(relative)))))
	}
	dockerfile := filepath.Join(root, Dockerfile)
	if err := os.MkdirAll(filepath.Dir(dockerfile), 0o700); err != nil {
		t.Fatalf("cannot create %s: %s", filepath.Dir(dockerfile), err)
	}
	writeFixture(t, dockerfile, string(readFile(t, filepath.Join(source, Dockerfile))))
	return root
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("%s: %s", path, err)
	}
}

// committedProposal is the six files the checkout would publish today.
func committedProposal(t *testing.T, root string) []proposalFile {
	t.Helper()
	files := make([]proposalFile, 0, len(stageNames()))
	for _, name := range stageNames() {
		destination := destinationOf(name)
		files = append(files, proposalFile{destination: destination,
			raw: readFile(t, filepath.Join(root, filepath.FromSlash(destination)))})
	}
	return files
}

// withDockerfile replaces the proposal's Dockerfile.
func withDockerfile(files []proposalFile, dockerfile string) []proposalFile {
	replaced := make([]proposalFile, 0, len(files))
	for _, file := range files {
		if file.destination == Dockerfile {
			file = proposalFile{destination: Dockerfile, raw: []byte(dockerfile)}
		}
		replaced = append(replaced, file)
	}
	return replaced
}

// TestStagingRoundTripsTheFixedLayout: the proposal is written through the staging
// discipline and read back through it, with the evidence child created once.
func TestStagingRoundTripsTheFixedLayout(t *testing.T) {
	root := fixtureRoot(t)
	files := committedProposal(t, root)
	directory := filepath.Join(t.TempDir(), "stage")
	if err := writeStage(directory, files); err != nil {
		t.Fatalf("write: %s", err)
	}
	read, err := readStage(directory, stageNames())
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	if len(read) != len(files) {
		t.Fatalf("read %d files, wrote %d", len(read), len(files))
	}
	for index, file := range read {
		if file.name != stageNames()[index] {
			t.Errorf("file %d is %s, not %s", index, file.name, stageNames()[index])
		}
		if string(file.raw) != string(files[index].raw) {
			t.Errorf("%s did not round-trip", file.name)
		}
	}
	info, err := os.Stat(filepath.Join(directory, evidenceName))
	if err != nil {
		t.Fatalf("the evidence directory is missing: %s", err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("the evidence directory is mode %04o", mode)
	}
}

// TestStagingRefusesACallerDirectory: only an absent or empty directory this invocation
// may fill is written to.
func TestStagingRefusesACallerDirectory(t *testing.T) {
	files := committedProposal(t, fixtureRoot(t))
	sentinel := filepath.Join(t.TempDir(), "stage")
	if err := os.Mkdir(sentinel, 0o700); err != nil {
		t.Fatalf("%s: %s", sentinel, err)
	}
	writeFixture(t, filepath.Join(sentinel, "keep.txt"), "do not delete me\n")
	if err := writeStage(sentinel, files); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Errorf("a non-empty staging directory reported %v", err)
	}
	if raw := readFile(t, filepath.Join(sentinel, "keep.txt")); string(raw) != "do not delete me\n" {
		t.Errorf("the caller's file was modified")
	}
}

// TestStagingRefusesASharedOrLinkedDirectory: a directory another user can reach, or a
// symlink to one, is not a staging area.
func TestStagingRefusesASharedOrLinkedDirectory(t *testing.T) {
	files := committedProposal(t, fixtureRoot(t))
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatalf("%s: %s", shared, err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatalf("%s: %s", shared, err)
	}
	if err := writeStage(shared, files); err == nil ||
		!strings.Contains(err.Error(), "private to this user") {
		t.Errorf("a shared staging directory reported %v", err)
	}
	if entries, err := os.ReadDir(shared); err != nil || len(entries) != 0 {
		t.Errorf("a refused staging directory was written to: %v %s", entries, err)
	}

	// A fresh, empty target: nothing may appear in it, because the link is refused
	// before the first write rather than after the evidence directory exists.
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("%s: %s", target, err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("%s: %s", link, err)
	}
	if err := writeStage(link, files); err == nil ||
		!strings.Contains(err.Error(), "not a real directory") {
		t.Errorf("a symlinked staging directory reported %v", err)
	}
	// The refusal must come before the first write: os.Root follows a link that stays
	// inside its own root, so without the explicit check the evidence directory would
	// appear in the link's target and only then be reported.
	targetEntries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("%s: %s", target, err)
	}
	for _, entry := range targetEntries {
		if entry.Name() == "evidence" {
			t.Errorf("writing through a symlinked staging directory created %q in its target",
				entry.Name())
		}
	}
}

// TestStagingRefusesSymlinkedProposalInputs: a staged file or evidence directory that
// is a link is not the bytes the transaction read, and the Python publisher opened
// every one of them without following a link.
func TestStagingRefusesSymlinkedProposalInputs(t *testing.T) {
	root := fixtureRoot(t)
	files := committedProposal(t, root)
	base := t.TempDir()
	elsewhere := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(elsewhere, 0o700); err != nil {
		t.Fatalf("%s: %s", elsewhere, err)
	}
	writeFixture(t, filepath.Join(elsewhere, "release.json"), "not the evidence\n")

	// A nested file replaced by a link is refused rather than read through.
	stageDir := filepath.Join(base, "stage")
	if err := writeStage(stageDir, files); err != nil {
		t.Fatalf("write: %s", err)
	}
	metadataPath := filepath.Join(stageDir, filepath.FromSlash(codexpin.MetadataPath))
	if err := os.Remove(metadataPath); err != nil {
		t.Fatalf("%s: %s", metadataPath, err)
	}
	if err := os.Symlink(filepath.Join(elsewhere, "release.json"), metadataPath); err != nil {
		t.Fatalf("%s: %s", metadataPath, err)
	}
	if _, err := readStage(stageDir, stageNames()); err == nil ||
		!strings.Contains(err.Error(), "a symlink is refused") {
		t.Errorf("a symlinked staged file reported %v", err)
	}

	// A pre-planted evidence directory that is a link is refused before any write.
	linked := filepath.Join(base, "linked")
	if err := os.Mkdir(linked, 0o700); err != nil {
		t.Fatalf("%s: %s", linked, err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(linked, "evidence")); err != nil {
		t.Fatalf("%s: %s", linked, err)
	}
	// Either refusal is right — the directory is not empty, and what fills it is a
	// link — as long as nothing is written through it.
	if err := writeStage(linked, files); err == nil {
		t.Errorf("a pre-planted evidence directory was accepted")
	}
	if entries, err := os.ReadDir(elsewhere); err != nil || len(entries) != 1 {
		t.Errorf("writing through a symlinked evidence directory changed its target: %v %s",
			entries, err)
	}
}

// TestPublishStageRefusesAnUnrelatedDockerfileEdit: the mask comparison is what keeps a
// Codex publication from carrying somebody else's Dockerfile change into a commit.
func TestPublishStageRefusesAnUnrelatedDockerfileEdit(t *testing.T) {
	root := fixtureRoot(t)
	before := string(readFile(t, filepath.Join(root, Dockerfile)))
	files := withDockerfile(committedProposal(t, root),
		strings.Replace(before, "FROM ${BASE_IMAGE}", "RUN true\nFROM ${BASE_IMAGE}", 1))
	stageDir := filepath.Join(t.TempDir(), "stage")
	if err := writeStage(stageDir, files); err != nil {
		t.Fatalf("write: %s", err)
	}
	client, out, errOut := newClient(t, root)
	if status := client.PublishStage(stageDir); status != 1 {
		t.Fatalf("publish stage: status %d", status)
	}
	if out.Len() != 0 {
		t.Errorf("a refused publication wrote to stdout: %s", out)
	}
	if !strings.Contains(errOut.String(), "changes more than the single-slot version defaults") {
		t.Errorf("the refusal does not name the Dockerfile: %s", errOut)
	}
	if string(readFile(t, filepath.Join(root, Dockerfile))) != before {
		t.Errorf("a refused publication changed the Dockerfile")
	}
}

// TestPublishStageCarriesTheUnownedArgEdits: the bumper stages its own single-slot ARG
// defaults into the Codex proposal, and the proposal this owner hands to the publisher
// must leave the Codex slot alone.
//
// The transaction itself is not run here: it ends the process on every outcome it
// decides, so the request is what a writer-level case can hold.
func TestPublishStageCarriesTheUnownedArgEdits(t *testing.T) {
	root := fixtureRoot(t)
	pin, _ := committedPin(t)
	before := string(readFile(t, filepath.Join(root, Dockerfile)))
	staged := rewriteSlots(t, before, pin.Release, "v9.9.9", "9.9.9")
	files := withDockerfile(committedProposal(t, root), staged)
	stageDir := filepath.Join(t.TempDir(), "stage")
	if err := writeStage(stageDir, files); err != nil {
		t.Fatalf("write: %s", err)
	}
	client, out, errOut := newClient(t, root)
	var published publication.Request
	client.Publish = func(request publication.Request) int {
		published = request
		return 0
	}
	if status := client.PublishStage(stageDir); status != 0 {
		t.Fatalf("publish stage: status %d: %s", status, errOut)
	}
	if out.Len() != 0 {
		t.Errorf("the writer reported an outcome the publisher owns: %s", out)
	}
	// The stage is the caller's, not this invocation's: it must never be handed over as
	// owner scratch, because the publisher removes what it is given.
	if published.Owner != "" {
		t.Errorf("the request claims an owner scratch it did not create: %s", published.Owner)
	}
	retained := strings.Join(published.Retain, "\n")
	if !strings.Contains(retained, "staged proposal retained at "+stageDir) {
		t.Errorf("the request does not name the caller's stage for retention: %q", retained)
	}
	proposed := map[string][]byte{}
	for _, file := range published.Files {
		proposed[file.Path] = file.Raw
	}
	if !bytes.Equal(proposed[PinFile], readFile(t, filepath.Join(repositoryRoot(t), PinFile))) {
		t.Errorf("the proposal changes %s, which this owner does not", PinFile)
	}
	dockerfile, ok := proposed[Dockerfile]
	if !ok {
		t.Fatalf("the proposal carries no %s: %v", Dockerfile, published.Files)
	}
	if err := RequireSingleSlot(string(dockerfile), pin.Release); err != nil {
		t.Errorf("the proposal lost the Codex slot: %s", err)
	}
	for _, needle := range []string{"ARG AGENT_VERSION_OPENCODE=v9.9.9", "ARG AGENT_VERSION_COPILOT=9.9.9"} {
		if !strings.Contains(string(dockerfile), needle) {
			t.Errorf("the proposal does not carry %q", needle)
		}
	}
}
