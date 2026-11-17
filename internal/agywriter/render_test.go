package agywriter

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// repositoryRoot is the checkout this test runs in.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot read the working directory: %s", err)
	}
	return filepath.Join(directory, "..", "..")
}

// committedProposal is the committed closure read from disk, so the renderer is held
// to the record a review accepted rather than to its own output.
func committedProposal(t *testing.T) (pin artifactpin.Pin, release []byte,
	manifests map[artifactpin.Platform][]byte) {
	t.Helper()
	directory := filepath.Join(repositoryRoot(t), PinDirectory)
	pin, err := artifactpin.Load(filepath.Join(directory, "pin.json"))
	if err != nil {
		t.Fatalf("the committed pin does not load: %s", err)
	}
	evidence := func(relative artifactpin.RelPath) []byte {
		raw, err := os.ReadFile(filepath.Join(directory, relative.String()))
		if err != nil {
			t.Fatalf("%s: %s", relative, err)
		}
		return raw
	}
	manifests = make(map[artifactpin.Platform][]byte, len(artifactpin.Platforms))
	for _, platform := range artifactpin.Platforms {
		manifests[platform] = evidence(artifactpin.AgyEvidence[platform])
	}
	return pin, evidence(artifactpin.AgyReleaseEvidence), manifests
}

// TestRenderReproducesTheCommittedPin is the whole point of the renderer: the same
// evidence has to produce the same document byte for byte, so a re-pin of an
// unchanged release is no diff at all. The committed file is also held to the
// reviewed shape the reviewer signs -- the payload's pair spelling, the empty links
// array, the hash-only block and this package's own reason text.
func TestRenderReproducesTheCommittedPin(t *testing.T) {
	pin, release, manifests := committedProposal(t)
	hashOnly := authenticationOf(pin.Version, release, manifests,
		pin.Authentication.HashOnly.VerifiedOn)
	if !reflect.DeepEqual(hashOnly, pin.Authentication.HashOnly) {
		t.Errorf("the rendered authentication block differs from the committed one\n--- committed\n%+v\n--- rendered\n%+v",
			pin.Authentication.HashOnly, hashOnly)
	}
	if pin.Authentication.HashOnly.Reason != Reason {
		t.Errorf("the committed reason is not this package's review record: %q",
			pin.Authentication.HashOnly.Reason)
	}

	rendered, err := artifactpin.Render(artifactpin.Agy, pin.Version, artifactpin.Review{
		VerifiedOn: hashOnly.VerifiedOn,
		Archives:   pin.Archives,
		HashOnly:   hashOnly,
	})
	if err != nil {
		t.Fatalf("render: %s", err)
	}
	committed, err := os.ReadFile(filepath.Join(repositoryRoot(t), PinFile))
	if err != nil {
		t.Fatalf("read the committed pin: %s", err)
	}
	if !bytes.Equal(rendered, committed) {
		t.Errorf("a rendered commitment differs from the committed record\n--- committed\n%s\n--- rendered\n%s",
			committed, rendered)
	}

	// The shape a reviewer reads, stated once so a change to any of it is a failing
	// test even if the round trip above were made to agree with itself.
	for _, needle := range []string{
		`"member": "antigravity"`,
		`"path": "bin/agy"`,
		`"links": []`,
		`"method": "hash-only"`,
		Reason,
	} {
		if !strings.Contains(string(rendered), needle) {
			t.Errorf("the rendered pin does not carry %q:\n%s", needle, rendered)
		}
	}
	if strings.Contains(string(rendered), `"bundled"`) {
		t.Errorf("the rendered pin records a bundled map:\n%s", rendered)
	}
	if _, err := artifactpin.Parse(rendered, PinDirectory, "agy"); err != nil {
		t.Fatalf("the rendered pin does not load: %s", err)
	}
}

// TestValidateProposalRefusesAWrongEvidenceDigest: the post-render validation reads
// the proposal's own bytes, not the inputs it was built from. A pin whose evidence
// digest disagrees with the evidence beside it parses, and its archive identities
// agree; only the corroboration check over the proposal can refuse it.
func TestValidateProposalRefusesAWrongEvidenceDigest(t *testing.T) {
	fx := newFixture(t)
	root := fixtureRoot(t, fixtureVersion)
	client, _, _ := newTestClient(t, root)
	dockerfile, err := os.ReadFile(filepath.Join(root, Dockerfile))
	if err != nil {
		t.Fatalf("read the fixture Dockerfile: %s", err)
	}
	work := t.TempDir()
	downloaded := fx.downloadedOf(t, work)
	authentication := authenticationOf(fx.version, fx.release, fx.manifests, "2026-01-02")

	proposal := func(hashOnly *artifactpin.HashOnly) []publication.Staged {
		document, err := artifactpin.Render(artifactpin.Agy, fx.version, artifactpin.Review{
			VerifiedOn: hashOnly.VerifiedOn,
			Archives:   fx.archivesOf(),
			HashOnly:   hashOnly,
		})
		if err != nil {
			t.Fatalf("render: %s", err)
		}
		return stagedFiles(document, fx.release, fx.manifests, dockerfile)
	}

	// The control: the same path accepts the honest proposal, so the refusal below is
	// the digest and nothing else.
	if err := client.validateProposal(proposal(authentication), fx.version, downloaded, work); err != nil {
		t.Fatalf("an honest proposal was refused: %s", err)
	}

	tampered := *authentication
	tampered.Release.Digest = artifactpin.Digest(strings.Repeat("ab", 32))
	err = client.validateProposal(proposal(&tampered), fx.version, downloaded, work)
	if err == nil {
		t.Fatalf("a proposal whose evidence digest is wrong was accepted")
	}
	if !strings.Contains(err.Error(), "the pin says") {
		t.Errorf("the refusal does not report the disagreement: %s", err)
	}
	if !strings.Contains(err.Error(), artifactpin.AgyReleaseEvidence.String()) {
		t.Errorf("the refusal does not name the evidence file: %s", err)
	}
}
