package agywriter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
)

// fixtureVersion is the release these tests synthesize. It is a version of the
// reviewed shape, not the committed one: no case here reads or writes the tree.
const fixtureVersion = "1.4.11"

// fixture is one synthesized release: the archives, the GitHub release record and
// Google's two manifests, all derived from the same bytes so both channels agree by
// construction. A case that changes a payload changes its evidence with it, so a
// refusal is never a digest accident.
type fixture struct {
	version   artifactpin.Version
	archives  map[artifactpin.Platform][]byte
	release   []byte
	manifests map[artifactpin.Platform][]byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	version, err := artifactpin.ParseVersion(fixtureVersion, "test fixture")
	if err != nil {
		t.Fatalf("the fixture version is not a reviewed release: %s", err)
	}
	fx := &fixture{
		version:   version,
		archives:  map[artifactpin.Platform][]byte{},
		manifests: map[artifactpin.Platform][]byte{},
	}
	assets := make([]map[string]any, 0, len(artifactpin.Platforms))
	for _, platform := range artifactpin.Platforms {
		archive := archiveFor(t, artifactpin.ELFMachines[artifactpin.Agy][platform])
		fx.archives[platform] = archive
		filename := artifactpin.ExpectedFilename(artifactpin.Agy, version, platform)
		fx.manifests[platform] = encode(t, map[string]any{
			"version": version.String(),
			"url": "https://storage.googleapis.com/antigravity-public/" +
				version.String() + "/" + filename,
			"sha512": sum512(archive),
		})
		assets = append(assets, map[string]any{
			"name":           filename,
			"size":           len(archive),
			"digest":         "sha256:" + string(artifactpin.Sum256(archive)),
			"download_count": 4102,
			"uploader":       map[string]any{"login": "fixture"},
		})
	}
	fx.release = encode(t, map[string]any{
		"tag_name":  version.String(),
		"draft":     false,
		"immutable": false,
		"assets":    assets,
	})
	return fx
}

// archiveFor is the one reviewed archive shape: a single prefixless `antigravity`
// member, mode 0755, whose first bytes are the ELF header of the chosen machine.
// The gzip header carries no timestamp, so two calls are byte-identical.
func archiveFor(t *testing.T, machine uint16) []byte {
	t.Helper()
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4], header[5], header[6] = 2, 1, 1
	header[18], header[19] = byte(machine), byte(machine>>8)
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	tarball := tar.NewWriter(compressor)
	entry := &tar.Header{
		Name:     "antigravity",
		Mode:     0o755,
		Size:     int64(len(header)),
		Typeflag: tar.TypeReg,
	}
	if err := tarball.WriteHeader(entry); err != nil {
		t.Fatalf("write the archive member: %s", err)
	}
	if _, err := tarball.Write(header); err != nil {
		t.Fatalf("write the archive payload: %s", err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatalf("close the archive: %s", err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatalf("close the archive stream: %s", err)
	}
	return buffer.Bytes()
}

func encode(t *testing.T, document map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode the fixture document: %s", err)
	}
	return append(raw, '\n')
}

// archivesOf is the pin's per-platform archive block for these bytes.
func (f *fixture) archivesOf() map[artifactpin.Platform]artifactpin.Archive {
	archives := make(map[artifactpin.Platform]artifactpin.Archive, len(artifactpin.Platforms))
	for _, platform := range artifactpin.Platforms {
		archive := f.archives[platform]
		archives[platform] = artifactpin.Archive{
			Platform:     platform,
			URL:          artifactpin.CanonicalURL(artifactpin.Agy, f.version, platform),
			Format:       artifactpin.TarGz,
			MemberPrefix: artifactpin.MemberPrefix(artifactpin.Agy, f.version, platform),
			Digest:       artifactpin.Sum256(archive),
			SHA512:       sum512(archive),
		}
	}
	return archives
}

// downloadedOf is what step 4 hands the post-render validation: one corroborated
// archive per platform, written where a refresh would have written it.
func (f *fixture) downloadedOf(t *testing.T, work string) map[artifactpin.Platform]corroborated {
	t.Helper()
	downloaded := make(map[artifactpin.Platform]corroborated, len(artifactpin.Platforms))
	for _, platform := range artifactpin.Platforms {
		archive := f.archives[platform]
		path := filepath.Join(work,
			artifactpin.ExpectedFilename(artifactpin.Agy, f.version, platform))
		if err := os.WriteFile(path, archive, 0o600); err != nil {
			t.Fatalf("write %s: %s", path, err)
		}
		downloaded[platform] = corroborated{
			platform: platform,
			archive:  path,
			sha256:   artifactpin.Sum256(archive),
			sha512:   sum512(archive),
		}
	}
	return downloaded
}

// fetcher serves this release by its canonical URLs and records every request, so a
// case can hold the writer to the exact request set -- including "no request at all".
type fixtureFetcher struct {
	version   artifactpin.Version
	release   []byte
	manifests map[artifactpin.Platform][]byte
	archives  map[artifactpin.Platform][]byte
	fetched   []string
}

func (f *fixture) fetcher() *fixtureFetcher {
	return &fixtureFetcher{
		version:   f.version,
		release:   f.release,
		manifests: f.manifests,
		archives:  f.archives,
	}
}

func (f *fixtureFetcher) Get(_ context.Context, url string, _ int64) ([]byte, error) {
	f.fetched = append(f.fetched, url)
	if url == artifactpin.AgyReleaseAPI+f.version.String() {
		return f.release, nil
	}
	for _, platform := range artifactpin.Platforms {
		switch url {
		case artifactpin.ManifestURL(platform):
			return f.manifests[platform], nil
		case artifactpin.CanonicalURL(artifactpin.Agy, f.version, platform):
			return f.archives[platform], nil
		}
	}
	return nil, fmt.Errorf("cannot fetch %s: the fixture serves no such document", url)
}

// fixtureRoot is a repository the writer may publish into: the Dockerfile it reads
// and the pin directory it creates, and nothing else. A case that passed on a file
// the real checkout happens to have could not be reproduced here.
func fixtureRoot(t *testing.T, arg string) string {
	t.Helper()
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, Dockerfile), dockerfileFor(arg))
	return root
}

func dockerfileFor(arg string) string {
	return "ARG BASE_IMAGE=agent-vm-base:local\n" +
		"FROM ${BASE_IMAGE}\n" +
		"ARG " + Argument + "=" + arg + "\n" +
		"\n" +
		"LABEL " + Label + `="${` + Argument + `}"` + "\n" +
		"\n" +
		"RUN sh /tmp/install-agy.sh\n"
}

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create %s: %s", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %s", path, err)
	}
}

// snapshot is every file below root, which is what "the sources are unchanged" means.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found[relative] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %s", root, err)
	}
	return found
}

// newTestClient is one writer invocation with nowhere to write but this test.
func newTestClient(t *testing.T, root string) (*Client, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	client := New(Environment{
		Root:      root,
		TempRoot:  t.TempDir(),
		Variables: map[string]string{},
	}, out, errOut)
	return client, out, errOut
}
