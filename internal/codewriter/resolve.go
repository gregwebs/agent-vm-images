package codewriter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
	"github.com/gregwebs/agent-vm-images/internal/evidence"
	"github.com/gregwebs/agent-vm-images/internal/strictjson"
)

// scratchFile is one downloaded byte stream: where this run wrote it, and its digest
// there. Everything below hashes the file it will read, never the bytes it received.
type scratchFile struct {
	path   string
	digest string
}

// platformBytes is both maintained payloads for one platform, as upstream served
// them, keyed by the maintained platform name.
type platformBytes struct {
	archive scratchFile
	bundle  scratchFile
}

// fetched is every upstream byte one refresh needs.
type fetched struct {
	metadata  scratchFile
	checksums scratchFile
	platforms map[string]platformBytes
}

// read is the bytes this run wrote, re-hashed now: the file the digest names, never
// the bytes a transport handed over.
func (f scratchFile) read(limit int64, what string) ([]byte, error) {
	raw, err := evidence.ReadBounded(f.path, limit, what)
	if err != nil {
		return nil, err
	}
	if evidence.Sha256Of(raw) != f.digest {
		return nil, refusal("%s changed in the work directory", what)
	}
	return raw, nil
}

// identity is one platform's downloaded digests: the only facts the pin records
// about a payload.
type identity struct {
	archive string
	bundle  string
}

// identities reduces a download to the digests the pin records.
func (f fetched) identities() map[string]identity {
	out := make(map[string]identity, len(f.platforms))
	for platform, bytes := range f.platforms {
		out[platform] = identity{archive: bytes.archive.digest, bundle: bytes.bundle.digest}
	}
	return out
}

// expectedAsset is one committed identity this run must corroborate, in the order the
// refusals report it.
type expectedAsset struct {
	name   string
	digest string
}

// download receives one URL into downloads, bounded, and hashes the file it wrote.
func (c *Client) download(ctx context.Context, url, path string, limit int64,
	what string) (scratchFile, error) {
	raw, err := c.Fetch.Get(ctx, url, limit)
	if err != nil {
		return scratchFile{}, err
	}
	if int64(len(raw)) > limit {
		return scratchFile{}, refusal("%s is larger than the %d byte ceiling", url, limit)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return scratchFile{}, refusal("cannot write %s: %s", path, err)
	}
	digest, _, err := evidence.FileDigest(path, limit, what)
	if err != nil {
		return scratchFile{}, err
	}
	return scratchFile{path: path, digest: digest}, nil
}

// fetchUpstream reads every upstream byte, bounded and into private scratch.
func (c *Client) fetchUpstream(ctx context.Context, release, downloads string) (fetched, error) {
	metadata, err := c.download(ctx, codexpin.APIURL(release),
		filepath.Join(downloads, metadataName), codexpin.MaxMetadataBytes,
		"the downloaded release metadata")
	if err != nil {
		return fetched{}, err
	}
	checksums, err := c.download(ctx, codexpin.AssetURL(release, codexpin.ChecksumsAsset),
		filepath.Join(downloads, codexpin.ChecksumsAsset), codexpin.MaxChecksumsBytes,
		"the downloaded checksums")
	if err != nil {
		return fetched{}, err
	}
	received := fetched{metadata: metadata, checksums: checksums,
		platforms: make(map[string]platformBytes, len(codexpin.Platforms))}
	for _, platform := range codexpin.Platforms {
		target := codexpin.Targets[platform]
		asset := codexpin.AssetName(target)
		archive, err := c.download(ctx, codexpin.AssetURL(release, asset),
			filepath.Join(downloads, asset), codexpin.MaxCompressedBytes,
			"the downloaded "+asset)
		if err != nil {
			return fetched{}, err
		}
		bundle, err := c.download(ctx, codexpin.AssetURL(release, codexpin.BundleName(target)),
			filepath.Join(downloads, codexpin.BundleName(target)), codexpin.MaxBundleBytes,
			"the downloaded "+codexpin.BundleName(target))
		if err != nil {
			return fetched{}, err
		}
		received.platforms[platform] = platformBytes{archive: archive, bundle: bundle}
	}
	return received, nil
}

// expectations is every committed identity this run must corroborate, in the order the
// refusals report it: the checksums manifest first, then each platform's payload and
// bundle.
func (f fetched) expectations() []expectedAsset {
	expected := []expectedAsset{{name: codexpin.ChecksumsAsset, digest: f.checksums.digest}}
	for _, platform := range codexpin.Platforms {
		target := codexpin.Targets[platform]
		received := f.platforms[platform]
		expected = append(expected,
			expectedAsset{name: codexpin.AssetName(target), digest: received.archive.digest},
			expectedAsset{name: codexpin.BundleName(target), digest: received.bundle.digest})
	}
	return expected
}

// payloads is the subset the original checksum manifest must corroborate: the payload
// archives only.
func (f fetched) payloads() []expectedAsset {
	archives := make([]expectedAsset, 0, len(codexpin.Platforms))
	for _, platform := range codexpin.Platforms {
		target := codexpin.Targets[platform]
		archives = append(archives, expectedAsset{name: codexpin.AssetName(target),
			digest: f.platforms[platform].archive.digest})
	}
	return archives
}

// requireMetadataCovers demands every downloaded identity be exactly what the exact-tag
// API reports.
func requireMetadataCovers(raw []byte, expected []expectedAsset) error {
	document, err := strictjson.Decode(raw, "release metadata")
	if err != nil {
		return err
	}
	members, ok := document.(map[string]any)
	if !ok {
		return refusal("release metadata is not a JSON object")
	}
	assets, ok := members["assets"].([]any)
	if !ok {
		return refusal("release metadata has no assets array")
	}
	digests := make(map[string]any, len(assets))
	for _, entry := range assets {
		asset, ok := entry.(map[string]any)
		if !ok {
			return refusal("release metadata has an unreadable asset")
		}
		name, ok := asset["name"].(string)
		if !ok {
			return refusal("release metadata has an unreadable asset")
		}
		if _, duplicate := digests[name]; duplicate {
			return refusal("release metadata repeats the asset %q", name)
		}
		digests[name] = asset["digest"]
	}
	for _, want := range expected {
		found, present := digests[want.name]
		if !present || found == nil {
			return refusal("release metadata does not list the required asset %q", want.name)
		}
		if text, isText := found.(string); !isText || text != "sha256:"+want.digest {
			return refusal("release metadata digests %s as %s, but this run downloaded %s",
				want.name, describe(found), want.digest)
		}
	}
	return nil
}

// requireChecksumsCover demands the original checksum manifest corroborate exactly
// these digests.
//
// The grammar and its duplicate rejection live in the reader (`ParseChecksums`), which
// is also what parses the committed evidence, so the two security parsers cannot
// drift; only the corroboration below is this writer's own.
func requireChecksumsCover(raw []byte, expected []expectedAsset) error {
	lines, err := codexpin.ParseChecksums(raw, codexpin.ChecksumsAsset)
	if err != nil {
		return err
	}
	declared := make(map[string]string, len(lines))
	for _, line := range lines {
		declared[line.Name] = line.Digest
	}
	for _, want := range expected {
		found, present := declared[want.name]
		if !present {
			return refusal("%s has no line for %q", codexpin.ChecksumsAsset, want.name)
		}
		if found != want.digest {
			return refusal("%s disagrees with the downloaded %s: %s vs %s",
				codexpin.ChecksumsAsset, want.name, found, want.digest)
		}
	}
	return nil
}

// projection is the part of the release metadata a substitution would have to change.
// Download counts, body text, ids and timestamps change on every fetch of the same tag;
// the tag and the asset names with their digests are the security projection.
type projection struct {
	tag    string
	assets []projectionAsset
}

type projectionAsset struct {
	name   string
	digest string
}

// securityProjection reduces release metadata to the fields a same-tag rewrite cannot
// keep. Ignoring the mutable part is what makes a repeat run of the same pin print
// `unchanged` instead of a daily date-only diff.
func securityProjection(raw []byte) (projection, error) {
	document, err := strictjson.Decode(raw, "release metadata")
	if err != nil {
		return projection{}, err
	}
	members, ok := document.(map[string]any)
	if !ok {
		return projection{}, refusal("release metadata is not a JSON object")
	}
	assets, ok := members["assets"].([]any)
	if !ok {
		return projection{}, refusal("release metadata has no assets array")
	}
	out := projection{tag: describe(members["tag_name"]), assets: make([]projectionAsset, 0, len(assets))}
	seen := make(map[string]bool, len(assets))
	for _, entry := range assets {
		asset, ok := entry.(map[string]any)
		if !ok {
			return projection{}, refusal("release metadata has an asset that is not an object")
		}
		name, ok := asset["name"].(string)
		if !ok || name == "" {
			return projection{}, refusal("release metadata has an asset with no name")
		}
		if seen[name] {
			return projection{}, refusal("release metadata repeats the asset %q", name)
		}
		seen[name] = true
		out.assets = append(out.assets, projectionAsset{name: name, digest: describe(asset["digest"])})
	}
	sort.Slice(out.assets, func(left, right int) bool { return out.assets[left].name < out.assets[right].name })
	return out, nil
}

// sameRelease is what this run's bytes say, held against the committed record.
type sameRelease struct {
	platforms map[string]identity
	checksums string
	metadata  []byte
}

// requireSameRelease refuses a same-release upstream rewrite, whatever changed. An
// identical repeat keeps the committed raw metadata and the original verification
// date, so the proposal is byte-identical; a changed identity is refused here, never
// re-pinned.
func requireSameRelease(committed codexpin.Pin, committedMetadata []byte,
	proposed sameRelease) error {
	for _, platform := range codexpin.Platforms {
		pinned := committed.Platforms[platform]
		target := codexpin.Targets[platform]
		received := proposed.platforms[platform]
		if received.archive != pinned.SHA256 {
			return refusal("%s changed for the already-pinned %s: committed %s, upstream "+
				"%s. A same-release payload rewrite needs a separate reviewed adoption.",
				codexpin.AssetName(target), committed.Release, pinned.SHA256, received.archive)
		}
		if received.bundle != pinned.Bundle.SHA256 {
			return refusal("%s changed for the already-pinned %s. A same-release evidence "+
				"rewrite needs a separate reviewed adoption.",
				codexpin.BundleName(target), committed.Release)
		}
	}
	if proposed.checksums != committed.Checksums.SHA256 {
		return refusal("%s changed for the already-pinned %s; refusing to re-pin it.",
			codexpin.ChecksumsAsset, committed.Release)
	}
	committedProjection, err := securityProjection(committedMetadata)
	if err != nil {
		return err
	}
	proposedProjection, err := securityProjection(proposed.metadata)
	if err != nil {
		return err
	}
	if committedProjection.tag != proposedProjection.tag ||
		!slices.Equal(committedProjection.assets, proposedProjection.assets) {
		return refusal("the security projection of the %s release metadata changed; a "+
			"same-tag identity change needs a separate reviewed adoption.", committed.Release)
	}
	return nil
}

// committedPin is the validated committed closure, or an empty result on a clean first
// adoption.
//
// A half-populated directory is refused rather than repaired: this writer publishes a
// reviewed closure, it does not make a broken one look better. Only the closure's own
// files count: the directory also holds the reader, the installer and the vendor tree.
func (c *Client) committedPin() (codexpin.Pin, bool, error) {
	directory := filepath.Join(c.Env.Root, PinDirectory)
	present := false
	for _, name := range stageNames() {
		if name == dockerfileName {
			continue
		}
		_, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(name)))
		if err == nil {
			present = true
			continue
		}
		if !os.IsNotExist(err) {
			return codexpin.Pin{}, false, refusal("cannot read %s: %s", directory, err)
		}
	}
	if !present {
		return codexpin.Pin{}, false, nil
	}
	pinPath := filepath.Join(directory, pinName)
	info, err := os.Lstat(pinPath)
	if err != nil || !info.Mode().IsRegular() {
		return codexpin.Pin{}, false, refusal("%s holds Codex evidence with no pin.json; "+
			"refusing a partial old state", directory)
	}
	pin, err := codexpin.Load(directory)
	if err != nil {
		return codexpin.Pin{}, false, err
	}
	return pin, true, nil
}

// boundedFile is one bounded read from a directory this run already owns.
func boundedFile(root, relative string, limit int64, what string) ([]byte, error) {
	path, err := evidence.RegularFile(root, relative, what)
	if err != nil {
		return nil, err
	}
	return evidence.ReadBounded(path, limit, what)
}

// describe is how a decoded JSON value is reported: text in quotes, everything else as
// the decoder handed it over.
func describe(value any) string {
	if text, ok := value.(string); ok {
		return `"` + text + `"`
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "an unreadable value"
	}
	return string(raw)
}

// destinationOf is where one stage name is published.
func destinationOf(stageName string) string {
	switch stageName {
	case pinName:
		return PinFile
	case dockerfileName:
		return Dockerfile
	}
	return PinDirectory + "/" + stageName
}

// stageNameOf is the stage name one published destination had.
func stageNameOf(destination string) (string, error) {
	switch destination {
	case Dockerfile:
		return dockerfileName, nil
	case PinFile:
		return pinName, nil
	}
	prefix := PinDirectory + "/"
	if strings.HasPrefix(destination, prefix) {
		return strings.TrimPrefix(destination, prefix), nil
	}
	return "", refusal("%s is not part of the Codex closure", destination)
}
