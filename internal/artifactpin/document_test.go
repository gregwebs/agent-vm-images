package artifactpin

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The committed pins are written by this package's own renderer, so loading one
// and marshalling it again has to reproduce the file byte for byte. That is what
// keeps a refresh to a one-line diff: field order, indentation, the empty `links`
// array and the absent `bundled` map are all part of the record a reviewer reads.
func TestDocumentRoundTripsTheCommittedPins(t *testing.T) {
	for _, relative := range []string{
		"images/artifacts/node.json",
		"images/artifacts/gh.json",
		"images/tools/agy/pin.json",
	} {
		path := filepath.Join(repositoryRoot(t), relative)
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", relative, err)
		}
		pin := committedPin(t, relative)
		rendered, err := documentOf(pin).Marshal()
		if err != nil {
			t.Fatalf("%s: %v", relative, err)
		}
		if !bytes.Equal(rendered, committed) {
			t.Errorf("%s: the rendered pin differs from the committed bytes\n--- committed\n%s\n--- rendered\n%s",
				relative, committed, rendered)
		}
	}
}

// Render builds a pin from what a refresh resolved, so it is the path a refresh
// actually publishes. Feeding it the committed pin's own reviewed data has to
// reproduce the committed bytes: that is what keeps the shape constants (payload,
// links, destination, the two reasons) honest, which the round trip above cannot
// see because it starts from a parsed pin.
func TestRenderReproducesTheCommittedPins(t *testing.T) {
	for _, relative := range []string{
		"images/artifacts/node.json",
		"images/artifacts/gh.json",
	} {
		pin := committedPin(t, relative)
		review := Review{
			VerifiedOn: pin.Authentication.VerifiedOn(),
			Archives:   pin.Archives,
			OpenPGP:    pin.Authentication.OpenPGP,
			Sigstore:   pin.Authentication.Sigstore,
			Bundled:    pin.Bundled,
		}
		rendered, err := Render(pin.Name, pin.Version, review)
		if err != nil {
			t.Fatalf("%s: %v", relative, err)
		}
		committed, err := os.ReadFile(filepath.Join(repositoryRoot(t), relative))
		if err != nil {
			t.Fatalf("%s: %v", relative, err)
		}
		if !bytes.Equal(rendered, committed) {
			t.Errorf("%s: a rendered refresh differs from the committed record\n--- committed\n%s\n--- rendered\n%s",
				relative, committed, rendered)
		}
	}
}

func TestRenderLabel(t *testing.T) {
	dockerfile := "FROM base\nLABEL org.agent-vm.version.node=\"22.23.4\"\nLABEL other=keep\n"
	version, err := ParseVersion("22.23.5", "--version")
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := RenderLabel(dockerfile, Node, version)
	if err != nil {
		t.Fatal(err)
	}
	want := "FROM base\nLABEL org.agent-vm.version.node=\"22.23.5\"\nLABEL other=keep\n"
	if rendered != want {
		t.Errorf("got %q, want %q", rendered, want)
	}
}

func TestRenderLabelRefusesTheWrongCount(t *testing.T) {
	version, err := ParseVersion("1.2.3", "--version")
	if err != nil {
		t.Fatal(err)
	}
	for name, dockerfile := range map[string]string{
		"absent":  "FROM base\nLABEL other=1\n",
		"doubled": "LABEL org.agent-vm.version.gh=\"1\"\nLABEL org.agent-vm.version.gh=\"2\"\n",
	} {
		if _, err := RenderLabel(dockerfile, GH, version); err == nil {
			t.Errorf("%s: the label count was accepted", name)
		}
	}
}

// A rendered pin has to be loadable: the writer refuses to publish bytes its own
// loader would reject.
func TestRenderProducesALoadablePin(t *testing.T) {
	version, err := ParseVersion("9.9.9", "--version")
	if err != nil {
		t.Fatal(err)
	}
	review := Review{
		VerifiedOn: "2026-01-02",
		Archives: map[Platform]Archive{
			LinuxAMD64: {
				Platform:     LinuxAMD64,
				URL:          CanonicalURL(GH, version, LinuxAMD64),
				Format:       TarGz,
				MemberPrefix: "gh_9.9.9_linux_amd64/",
				Digest:       Digest(hexOf('a')),
				Bundle: &Bundle{
					Path:   RelPath(BundleName(GH, version, LinuxAMD64)),
					Digest: Digest(hexOf('b')),
				},
			},
			LinuxARM64: {
				Platform:     LinuxARM64,
				URL:          CanonicalURL(GH, version, LinuxARM64),
				Format:       TarGz,
				MemberPrefix: "gh_9.9.9_linux_arm64/",
				Digest:       Digest(hexOf('c')),
				Bundle: &Bundle{
					Path:   RelPath(BundleName(GH, version, LinuxARM64)),
					Digest: Digest(hexOf('d')),
				},
			},
		},
		Sigstore: &Sigstore{
			ChecksumsURL:    ChecksumsURL(GH, version),
			ChecksumsDigest: Digest(hexOf('e')),
			Policy:          GHPolicy,
			VerifiedOn:      "2026-01-02",
			Evidence:        GHEvidence(version),
			Reason:          GHReason,
		},
	}
	raw, err := Render(GH, version, review)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if _, err := Parse(raw, directory, "gh"); err != nil {
		t.Fatalf("the rendered pin does not load: %v\n%s", err, raw)
	}
}

func hexOf(letter byte) string {
	digest := make([]byte, 64)
	for index := range digest {
		digest[index] = letter
	}
	return string(digest)
}
