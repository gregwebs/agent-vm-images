// Package agy is the black-box suite for the agy owner writer (#131, #182).
//
// An agy pin has no upstream signature: the trust decision is that two independent
// channels agree — the vendor's auto-updater manifests and the GitHub release
// record — and that the bytes they name are the bytes the installer will extract.
// So a case here builds both channels by hand, serves them through
// `--fixture-upstream`, and then holds the program to its status, its frozen
// transcript and the sources it did or did not touch.
//
// The bytes are synthesized: real tar/gzip archives, real ELF headers, real
// digests. The archive's tar flavour is nobody's contract — the digests in both
// channels are taken over whatever bytes this file serves — so the fixture does not
// reproduce the deleted Python builder's GNU headers byte for byte. What is a
// contract is the served JSON: it ends with a newline, like the documents upstream
// serves, because a consumer that reads those bytes exists.
//
// Nothing here imports a production package, and no case patches a function: the
// writer is a compiled program reached through its own entry point.
package agy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

const (
	// Version is the release every case publishes unless it is proving a refusal.
	Version = "1.4.11"
	// Older and Newer bracket Version: one row needs a manifest that goes
	// backwards, another needs a release the pin has never seen.
	Older = "1.4.10"
	Newer = "1.4.12"

	// PinDirectory and PinFile are where a publication writes, at the same relative
	// paths production uses.
	PinDirectory = "images/tools/agy"
	PinFile      = PinDirectory + "/pin.json"
	// Dockerfile carries the ARG and the LABEL the writer keeps in step with the pin.
	Dockerfile = "images/standard/Dockerfile"

	repositorySlug = "google-antigravity/antigravity-cli"
	manifestOrigin = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/"
)

// platform is one architecture a release ships.
//
// A named type because this file keys six maps by it: a bare string key that no row
// supplies fails silently — the asset name comes out as `agy_cli_linux_.tar.gz` and
// the case fails somewhere unrelated to the omission. Adding a platform here forces
// every spelling of it to arrive with it, or the first lookup names the gap.
type platform string

const (
	linuxAMD64 platform = "linux/amd64"
	linuxARM64 platform = "linux/arm64"
)

// platforms are the two architectures a release carries, in the order the release
// record lists them: the vended asset order is the fixture's, so it is fixed here
// rather than taken from a map's iteration.
var platforms = []platform{linuxAMD64, linuxARM64}

// spellings is the three ways one architecture is named: the vendor's asset suffix,
// the manifest's file suffix, and the ELF header's e_machine.
type spellings struct {
	vendor  string
	arch    string
	machine int
}

var platformSpellings = map[platform]spellings{
	linuxAMD64: {vendor: "x64", arch: "amd64", machine: 62},
	linuxARM64: {vendor: "arm64", arch: "arm64", machine: 183},
}

func (p platform) spell(t harness.Reporter) spellings {
	t.Helper()
	found, known := platformSpellings[p]
	if !known {
		t.Fatalf("platform %q has no fixture spellings", p)
	}
	return found
}

// Destinations are the five files one publication touches.
var Destinations = []string{
	PinFile,
	PinDirectory + "/evidence/release.json",
	PinDirectory + "/evidence/manifest-linux-amd64.json",
	PinDirectory + "/evidence/manifest-linux-arm64.json",
	Dockerfile,
}

// reviewedMode is the permission the installer reviews the extracted binary for.
const reviewedMode = 0o755

// elfHeader is the 20 bytes every reader of this archive looks at: an ELF64
// little-endian header naming one machine.
func elfHeader(target int) []byte {
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4] = 2 // ELFCLASS64
	header[5] = 1 // ELFDATA2LSB
	header[6] = 1 // EV_CURRENT
	header[18] = byte(target)
	header[19] = byte(target >> 8)
	return header
}

// binary renders the one prefixless executable member an agy release ships. The
// gzip header carries no timestamp, so two calls with the same arguments are
// byte-identical: a fixture that varied between runs could corroborate by accident.
func binary(t harness.Reporter, target int, mode int64, body []byte) []byte {
	if body == nil {
		body = elfHeader(target)
	}
	return harness.TarGz(t, []harness.Member{{Name: "antigravity", Body: body, Mode: mode}})
}

// assetName is the name one architecture's tarball has, in both channels: the
// vendor's asset name and the file name the release record prints.
func assetName(t harness.Reporter, p platform) string {
	return "agy_cli_linux_" + p.spell(t).vendor + ".tar.gz"
}

// manifestFile is the served (and committed) file name of one platform's manifest.
func manifestFile(t harness.Reporter, p platform) string {
	return "linux_" + p.spell(t).arch + ".json"
}

// manifestEvidence is the path a platform's manifest is committed at: the served
// file name with a hyphen, which is the spelling the writer's closure uses.
func manifestEvidence(t harness.Reporter, p platform) string {
	return PinDirectory + "/evidence/manifest-linux-" + p.spell(t).arch + ".json"
}

func releaseAPIURL(version string) string {
	return "https://api.github.com/repos/" + repositorySlug + "/releases/tags/" + version
}

func assetURL(version, filename string) string {
	return "https://github.com/" + repositorySlug + "/releases/download/" + version + "/" + filename
}

func manifestURL(t harness.Reporter, p platform) string {
	return manifestOrigin + manifestFile(t, p)
}

// manifest is one served updater document.
type manifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

// releaseAsset is one entry of the GitHub release record.
type releaseAsset struct {
	Name          string        `json:"name"`
	Size          int           `json:"size"`
	Digest        string        `json:"digest"`
	DownloadCount int           `json:"download_count"`
	Uploader      releaseUpload `json:"uploader"`
}

type releaseUpload struct {
	Login string `json:"login"`
}

// releaseRecord is the served GitHub release document. The fields a case tampers
// are the fields the writer corroborates, and the ones it does not are the ones
// that must not be able to change a pin.
type releaseRecord struct {
	TagName   string         `json:"tag_name"`
	Draft     bool           `json:"draft"`
	Immutable bool           `json:"immutable"`
	Assets    []releaseAsset `json:"assets"`
}

// release is one release as both channels describe it: the two manifests, the
// GitHub record and the two archives they name.
type release struct {
	version string
	// downloadCount is not part of any security projection: a case changes it to
	// prove a repeat run is byte-identical.
	downloadCount int
	bodies        map[platform][]byte
	// manifestVersions, manifestSHA512 and manifestURLs are per platform, because a
	// case tampers with one architecture at a time.
	manifestVersions map[platform]string
	manifestSHA512   map[platform]string
	manifestURLs     map[platform]string
	releaseDigests   map[platform]string
	releaseTag       string
	draft            bool
	dropAssets       map[platform]bool
}

func newRelease(t harness.Reporter, version string) *release {
	served := &release{
		version:          version,
		downloadCount:    4102,
		bodies:           map[platform][]byte{},
		manifestVersions: map[platform]string{},
		manifestSHA512:   map[platform]string{},
		manifestURLs:     map[platform]string{},
		releaseDigests:   map[platform]string{},
		releaseTag:       version,
		dropAssets:       map[platform]bool{},
	}
	for _, p := range platforms {
		served.setBody(t, p, binary(t, p.spell(t).machine, reviewedMode, nil))
		served.manifestVersions[p] = version
		served.manifestURLs[p] = "https://storage.googleapis.com/antigravity-public/" +
			version + "/" + assetName(t, p)
	}
	return served
}

// setBody serves one platform's archive and the two digests that name it, so a case
// that changes an archive cannot leave a channel describing the old bytes.
func (r *release) setBody(t harness.Reporter, p platform, body []byte) {
	r.bodies[p] = body
	r.manifestSHA512[p] = harness.SHA512Hex(body)
	r.releaseDigests[p] = "sha256:" + harness.SHA256Hex(body)
}

// documents are the five served files by their canonical names.
func (r *release) documents(t harness.Reporter) map[string][]byte {
	documents := make(map[string][]byte, len(platforms)*2+1)
	for _, p := range platforms {
		documents[manifestFile(t, p)] = encodeDocument(t, manifest{
			Version: r.manifestVersions[p],
			URL:     r.manifestURLs[p],
			SHA512:  r.manifestSHA512[p],
		})
		documents[assetName(t, p)] = r.bodies[p]
	}
	record := releaseRecord{TagName: r.releaseTag, Draft: r.draft, Immutable: false}
	for _, p := range platforms {
		if r.dropAssets[p] {
			continue
		}
		record.Assets = append(record.Assets, releaseAsset{
			Name:          assetName(t, p),
			Size:          len(r.bodies[p]),
			Digest:        r.releaseDigests[p],
			DownloadCount: r.downloadCount,
			Uploader:      releaseUpload{Login: "fixture"},
		})
	}
	documents["release.json"] = encodeDocument(t, record)
	return documents
}

// served is every canonical URL this release answers, and its bytes.
func (r *release) served(t harness.Reporter) map[string][]byte {
	documents := r.documents(t)
	served := map[string][]byte{releaseAPIURL(r.version): documents["release.json"]}
	for _, p := range platforms {
		served[manifestURL(t, p)] = documents[manifestFile(t, p)]
		served[assetURL(r.version, assetName(t, p))] = documents[assetName(t, p)]
	}
	return served
}

// writeServed lays the release out the way `--fixture-upstream` serves it:
// https://host/path from DIR/host/path.
func (r *release) writeServed(t harness.Reporter, dir string) string {
	for url, body := range r.served(t) {
		harness.WriteFile(t, servedPath(t, dir, url), body)
	}
	return dir
}

// writeAssets writes the same bytes by their canonical asset and file names, which
// is the shape the bumper suite consumes.
func (r *release) writeAssets(t harness.Reporter, dir string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("cannot create %s: %v", dir, err)
	}
	for name, body := range r.documents(t) {
		harness.WriteFile(t, filepath.Join(dir, name), body)
	}
	return dir
}

// servedPath is where `--fixture-upstream DIR` serves url from.
func servedPath(t harness.Reporter, dir, url string) string {
	scheme, rest, found := strings.Cut(url, "://")
	if !found || scheme == "" || rest == "" {
		t.Fatalf("not an absolute URL: %q", url)
	}
	return filepath.Join(dir, filepath.FromSlash(rest))
}

// repository is the tree a publication writes into: the Dockerfile and, once a
// case has published, the pin closure. Those are the only two things an agy
// transaction reads from a checkout.
type repository struct {
	path string
}

// dockerfileText is the shape the writer maintains: one ARG, one LABEL naming it.
func dockerfileText(arg string) string {
	return "ARG BASE_IMAGE=agent-vm-base:local\n" +
		"FROM ${BASE_IMAGE}\n" +
		"ARG AGENT_VERSION_AGY=" + arg + "\n" +
		"\n" +
		"LABEL org.agent-vm.version.agy=\"${AGENT_VERSION_AGY}\"\n" +
		"\n" +
		"RUN sh /tmp/install-agy.sh\n"
}

func newRepository(t harness.Reporter, dir, arg string) *repository {
	repo := &repository{path: dir}
	harness.WriteFile(t, filepath.Join(dir, Dockerfile), []byte(dockerfileText(arg)))
	return repo
}

func (r *repository) read(t harness.Reporter, relative string) []byte {
	return harness.ReadFile(t, filepath.Join(r.path, relative))
}

func (r *repository) write(t harness.Reporter, relative string, body []byte) {
	harness.WriteFile(t, filepath.Join(r.path, relative), body)
}

// install folds a golden publication's destinations into this repository.
func (r *repository) install(t harness.Reporter, closure map[string][]byte) *repository {
	for _, name := range Destinations {
		if body, found := closure[name]; found {
			r.write(t, name, body)
		}
	}
	return r
}

// tree is every source byte a publication may change, by relative path.
func (r *repository) tree(t harness.Reporter) map[string][]byte {
	found := map[string][]byte{}
	for _, name := range Destinations {
		path := filepath.Join(r.path, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			found[name] = harness.ReadFile(t, path)
		}
	}
	return found
}

// temporary is this case's TMPDIR, so the scratch directory the writer announces
// is inside the case rather than in the host's shared temporary root.
func (r *repository) temporary(t harness.Reporter) string {
	dir := filepath.Join(filepath.Dir(r.path), "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("cannot create %s: %v", dir, err)
	}
	return dir
}

// state is every file and directory below the checkout: what "no partial state"
// means after a refusal.
type state struct {
	files       map[string][]byte
	directories []string
}

func (r *repository) state(t harness.Reporter) state {
	found := state{files: map[string][]byte{}}
	err := filepath.WalkDir(r.path, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == r.path {
			return nil
		}
		relative, err := filepath.Rel(r.path, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			found.directories = append(found.directories, relative)
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found.files[relative] = body
		return nil
	})
	if err != nil {
		t.Fatalf("cannot walk %s: %v", r.path, err)
	}
	sort.Strings(found.directories)
	return found
}

// difference describes how the tree differs from before, so a refusal that wrote
// something says what it wrote.
func (s state) difference(before state) string {
	var added, removed, changed []string
	for name := range s.files {
		old, existed := before.files[name]
		switch {
		case !existed:
			added = append(added, name)
		case !bytes.Equal(old, s.files[name]):
			changed = append(changed, name)
		}
	}
	for name := range before.files {
		if _, found := s.files[name]; !found {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return fmt.Sprintf("added %v, removed %v, changed %v, directories %v",
		added, removed, changed, s.directories)
}

func sameState(left, right state) bool {
	if len(left.files) != len(right.files) || len(left.directories) != len(right.directories) {
		return false
	}
	for name, body := range left.files {
		if !bytes.Equal(body, right.files[name]) {
			return false
		}
	}
	for index, name := range left.directories {
		if right.directories[index] != name {
			return false
		}
	}
	return true
}

// writerScript is the maintainer entry point, which is also the fixture builder's
// entry point: `avm-test` publishes through it so the bytes it copies out are bytes
// a maintainer's run produces.
func writerScript(t harness.Reporter) string {
	return filepath.Join(harness.Repository(t), "script", "dev", "pin-agy.sh")
}

// tool is the compiled writer, built once for this test process. Compiling per case
// would dominate the run; each suite builds into its own path, so two suites in one
// `go test` never write the same binary.
func tool(t harness.Reporter) string {
	return harness.Compile(t, "agy", "./cmd/avm-pin")
}

// writerOption adjusts one writer invocation.
type writerOption func(*harness.Spec)

// serving answers the writer's requests from a fixture upstream directory.
func serving(dir string) writerOption {
	return func(spec *harness.Spec) { spec.Argv = append(spec.Argv, "--fixture-upstream", dir) }
}

// arguments passes the writer's own flags.
func arguments(arguments ...string) writerOption {
	return func(spec *harness.Spec) { spec.Argv = append(spec.Argv, arguments...) }
}

// withEnv adds one environment variable, which is how the CI refusal and the two
// clock zones are driven.
func withEnv(name, value string) writerOption {
	return func(spec *harness.Spec) { spec.Env[name] = value }
}

// maintainerSpec is the environment a maintainer host would have: no
// GITHUB_ACTIONS (the aggregate usually runs on a runner, and the CI refusal has
// its own case), a TMPDIR inside the case, and a working directory outside both
// trees. One function, because the writer is reached two ways — its binary and the
// wrapper that builds it — and the two share this contract, not a copy of it.
func maintainerSpec(t harness.Reporter, repo *repository, entry []string, options ...writerOption) harness.Spec {
	spec := harness.Spec{
		Argv:  append(entry, "--root", repo.path),
		Dir:   os.TempDir(),
		Env:   map[string]string{"TMPDIR": repo.temporary(t)},
		Unset: []string{"GITHUB_ACTIONS", "PYTHONPATH"},
	}
	for _, option := range options {
		option(&spec)
	}
	return spec
}

// runWriter is one writer process, through the binary the wrapper builds.
func runWriter(t harness.Reporter, repo *repository, options ...writerOption) harness.Result {
	t.Helper()
	return harness.Run(t, maintainerSpec(t, repo, []string{tool(t), "agy"}, options...))
}

// wrapperEntry is `script/dev/pin-agy.sh` itself: the compile, the `cd` to the
// checkout and the `exec avm-pin agy` CONTRIBUTING tells a maintainer to run.
func wrapperEntry(t harness.Reporter) []string {
	return []string{"bash", writerScript(t)}
}

// runWrapper drives that wrapper, which every other case skips in favour of the
// binary it builds.
func runWrapper(t harness.Reporter, repo *repository, options ...writerOption) harness.Result {
	t.Helper()
	return harness.Run(t, maintainerSpec(t, repo, wrapperEntry(t), options...))
}

// requireStatus asserts the status and renders the run when it is not the one the
// contract names.
func requireStatus(t harness.Reporter, result harness.Result, want int) {
	t.Helper()
	if result.Status != want {
		t.Fatalf("status %d, want %d\n%s", result.Status, want, result.Report())
	}
}

// requireRefusal is the whole shape of a refusal: its status, the needle that names
// the reason, and a checkout no byte of which moved.
func requireRefusal(t harness.Reporter, result harness.Result, needle string, repo *repository, before state) {
	t.Helper()
	requireStatus(t, result, 1)
	text := result.Stdout + result.Stderr
	if !strings.Contains(text, needle) {
		t.Fatalf("the refusal does not mention %q\n%s", needle, result.Report())
	}
	if after := repo.state(t); !sameState(after, before) {
		t.Fatalf("the refusal changed the sources: %s", after.difference(before))
	}
}

// published publishes one release for real and returns the tree it produced, which
// is the committed state the tampering cases start from.
func published(t harness.Reporter, repo *repository, served *release, work string, version string) map[string][]byte {
	t.Helper()
	upstream := served.writeServed(t, filepath.Join(work, "upstream"))
	result := runWriter(t, repo, arguments("--version", version), serving(upstream))
	requireStatus(t, result, 0)
	return repo.tree(t)
}

// document parses a fixture JSON file. Numbers keep the spelling they were served
// with, so a case that rewrites one field does not renumber another.
func document(t harness.Reporter, raw []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var parsed map[string]any
	if err := decoder.Decode(&parsed); err != nil {
		t.Fatalf("a fixture document is not JSON: %v", err)
	}
	return parsed
}

// object is a nested document, named by the key that reached it.
func object(t harness.Reporter, parent map[string]any, name string) map[string]any {
	t.Helper()
	found, ok := parent[name].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object in this fixture document", name)
	}
	return found
}

// list is a nested array.
func list(t harness.Reporter, parent map[string]any, name string) []any {
	t.Helper()
	found, ok := parent[name].([]any)
	if !ok {
		t.Fatalf("%q is not an array in this fixture document", name)
	}
	return found
}

// encodeDocument renders a fixture document the way upstream serves one: two-space
// indentation and one trailing newline.
func encodeDocument(t harness.Reporter, doc any) []byte {
	t.Helper()
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("cannot encode a fixture document: %v", err)
	}
	return append(body, '\n')
}

// encodeIndented is the same document in another encoding, which is how a case
// proves that a manifest reformatted by its own host is still the same manifest.
func encodeIndented(t harness.Reporter, doc any, indent string) []byte {
	t.Helper()
	body, err := json.MarshalIndent(doc, "", indent)
	if err != nil {
		t.Fatalf("cannot encode a fixture document: %v", err)
	}
	return append(body, '\n')
}

// Fixture names what `avm-test write-fixture` writes and for which release. Two
// strings named, rather than two strings in a row.
type Fixture struct {
	Dir     string
	Version string
}

// WriteFixture is `avm-test write-fixture --tool agy`: one release published by the
// real writer, copied into a directory another suite checks in.
//
// DIR/source/ holds the committed closure at the same relative path production
// uses, and DIR/assets/ the served documents by their canonical names. Both come
// from a real run, so the bumper suite holds the bytes production publishes rather
// than a hand-written lookalike.
func WriteFixture(t harness.Reporter, fixture Fixture) {
	t.Helper()
	if fixture.Version == "" {
		fixture.Version = Version
	}
	dir, err := filepath.Abs(fixture.Dir)
	if err != nil {
		t.Fatalf("cannot resolve %s: %v", fixture.Dir, err)
	}
	for _, name := range []string{"source", "assets"} {
		path := filepath.Join(dir, name)
		switch _, err := os.Stat(path); {
		case err == nil:
			t.Fatalf("%s already holds a fixture: %s exists", dir, name)
		case !os.IsNotExist(err):
			t.Fatalf("cannot inspect %s: %v", path, err)
		}
	}
	work, err := os.MkdirTemp("", "agy-fixture.")
	if err != nil {
		t.Fatalf("cannot create a temporary directory: %v", err)
	}
	t.Cleanup(func() {
		// After the failure is read, not before: the message may name this tree.
		_ = os.RemoveAll(work)
	})
	// Resolved, because a program that prints an absolute path under it must print
	// the same string this file hands it: macOS resolves /var to /private/var.
	if work, err = filepath.EvalSymlinks(work); err != nil {
		t.Fatalf("cannot resolve %s: %v", work, err)
	}

	served := newRelease(t, fixture.Version)
	repo := newRepository(t, filepath.Join(work, "repo"), fixture.Version)
	upstream := served.writeServed(t, filepath.Join(work, "upstream"))
	result := harness.Run(t, maintainerSpec(t, repo, wrapperEntry(t),
		arguments("--version", fixture.Version), serving(upstream)))
	requireStatus(t, result, 0)

	harness.CopyTree(t, filepath.Join(repo.path, PinDirectory), filepath.Join(dir, "source", PinDirectory))
	served.writeAssets(t, filepath.Join(dir, "assets"))
}
