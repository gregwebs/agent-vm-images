package codex

import (
	"os"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// transportRefused removes one canonical document from the served tree and holds the
// writer to the refusal that names exactly the URL it asked for.
//
// The writer's whole upstream contract is six exact URLs. Serving five and removing
// the sixth is the black-box form of "this is the request set": a request the writer
// makes only for a document it names, and a refusal that names the URL it asked for.
func transportRefused(t *testing.T, url string) {
	t.Helper()
	w := newWriterFixture(t, "transport-"+harness.SHA256Hex([]byte(url))[:8])
	if err := os.Remove(upstreamPath(t, w.upstream, url)); err != nil {
		t.Fatalf("cannot remove %s from the served tree: %v", url, err)
	}
	repo := w.repository(t, "transport")
	stage := repo.stagePath()
	result := runWriter(t, repo, arguments("--version", Release, "--stage-dir", stage,
		"--cosign", w.cosign, "--fixture-upstream", w.upstream))
	requireStatus(t, result, 1)
	if !strings.HasPrefix(result.Stderr, writerPrefix) {
		t.Fatalf("the refusal is not prefixed with %q: %q", writerPrefix, result.Stderr)
	}
	if !strings.Contains(result.Stderr, "cannot fetch "+url) {
		t.Fatalf("the refusal did not name %s\n%s", url, result.Report())
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("a refused staging run left a staging directory at %s", stage)
	}
}

// The six cases below are the Python's one-per-canonical-document table: the
// exact-tag release document, the checksum manifest, and for each of the two targets
// the archive and the `.bundle`.

// TestWriterAMissingUpstreamDocumentIsRefusedByItsURLChecksumsManifest: the original
// `codex-package_SHA256SUMS` document.
func TestWriterAMissingUpstreamDocumentIsRefusedByItsURLChecksumsManifest(t *testing.T) {
	transportRefused(t, assetURL(Release, ChecksumsAsset))
}

// TestWriterAMissingUpstreamDocumentIsRefusedByItsURLExactTagReleaseDocument: the
// exact-tag release document.
func TestWriterAMissingUpstreamDocumentIsRefusedByItsURLExactTagReleaseDocument(t *testing.T) {
	transportRefused(t, apiURL(Release))
}

// TestWriterAMissingUpstreamDocumentIsRefusedByItsURLAmd64Archive: the amd64 package
// archive.
func TestWriterAMissingUpstreamDocumentIsRefusedByItsURLAmd64Archive(t *testing.T) {
	transportRefused(t, assetURL(Release, assetName(targetAMD64)))
}

// TestWriterAMissingUpstreamDocumentIsRefusedByItsURLAmd64Bundle: the amd64 legacy
// `.sigstore` bundle.
func TestWriterAMissingUpstreamDocumentIsRefusedByItsURLAmd64Bundle(t *testing.T) {
	transportRefused(t, assetURL(Release, bundleName(targetAMD64)))
}

// TestWriterAMissingUpstreamDocumentIsRefusedByItsURLARM64Archive: the arm64 package
// archive.
func TestWriterAMissingUpstreamDocumentIsRefusedByItsURLARM64Archive(t *testing.T) {
	transportRefused(t, assetURL(Release, assetName(targetARM64)))
}

// TestWriterAMissingUpstreamDocumentIsRefusedByItsURLARM64Bundle: the arm64 legacy
// `.sigstore` bundle.
func TestWriterAMissingUpstreamDocumentIsRefusedByItsURLARM64Bundle(t *testing.T) {
	transportRefused(t, assetURL(Release, bundleName(targetARM64)))
}
