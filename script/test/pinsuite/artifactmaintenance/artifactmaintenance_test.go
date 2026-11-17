// Package artifactmaintenance is the black-box evidence for the artifact pin
// writer (#96, #179), ported from script/test/pin-artifact-maintenance.py.
//
// `script/dev/pin-artifact.sh` is the only writer of an artifact pin, and the
// guarantees it claims are the ones a successful run cannot show: a failing second
// architecture changes nothing, a symlinked destination is refused, CI is refused,
// an inexact version is a usage error, a backup that cannot be taken touches
// nothing, pin-time verification uses its own keyring, and `verified_on` is the UTC
// date rather than the maintainer host's.
//
// Every case drives the real wrapper, so the detached signature, the archive decode
// and the whole publication are real work. The upstream bytes come from a fixture
// directory passed as `--fixture-upstream` and the Node release key is a fixture key
// this suite generates. Nothing is monkeypatched, because nothing can be: the
// writer is a binary.
//
// The publication transaction's own fault paths (a partial write, a signal during
// publication, an incomplete restore, a recovery record that cannot be written) need
// injected failures inside the process, so they are Go tests in
// `internal/artifactwriter/publish_test.go`, where the same cases keep their names.
package artifactmaintenance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

const (
	version    = "9.9.9"
	npmVersion = "10.9.9"
)

// platforms maps the pin's platform name to the token the release file name uses.
var platforms = map[string]string{"linux/amd64": "linux-x64", "linux/arm64": "linux-arm64"}

// fixture is a throwaway repository root, the key it is verified with, and the
// upstream bytes a refresh resolves.
type fixture struct {
	root        string
	upstream    *harness.Upstream
	artifacts   string
	key         string
	committed   []byte
	original    map[string][]byte
	signer      *harness.Signer
	fingerprint string
	tool        string
	repository  string
}

func newFixture(t *testing.T, name string) *fixture {
	t.Helper()
	harness.RequireTools(t, "gpg", "bash")
	repository := harness.Repository(t)
	work := harness.TempDir(t, "pin-artifact-maintenance.")
	root := harness.Scratch(t, work, name)
	f := &fixture{
		root:       filepath.Join(root, "repo"),
		upstream:   harness.NewUpstream(t),
		tool:       filepath.Join(repository, "script", "dev", "pin-artifact.sh"),
		repository: repository,
	}
	f.artifacts = filepath.Join(f.root, "images", "artifacts")
	if err := os.MkdirAll(f.artifacts, 0o755); err != nil {
		t.Fatalf("cannot create the fixture artifacts directory: %v", err)
	}
	harness.CopyFiles(t, filepath.Join(repository, "images", "artifacts"), f.artifacts)
	harness.CopyFile(t, filepath.Join(repository, "images", "Dockerfile"),
		filepath.Join(f.root, "images", "Dockerfile"))
	f.original = f.snapshot(t)

	f.signer = harness.NewSigner(t, filepath.Join(work, "signer"))
	f.fingerprint = f.signer.Fingerprint
	f.key = filepath.Join(f.artifacts, "node-release-key.asc")
	f.committed = harness.ReadFile(t, f.key)
	harness.WriteFile(t, f.key, []byte(f.signer.Export(t)))
	f.serve(t)
	// The snapshot a case compares against is the fixture's, not the repository's:
	// the key file above is a fixture byte a clean refresh is allowed to keep.
	f.original = f.snapshot(t)
	return f
}

// destinations is anything a refresh may rewrite: both pins, the label and the
// release key.
func (f *fixture) destinations() []string {
	return []string{
		filepath.Join(f.artifacts, "node.json"),
		filepath.Join(f.root, "images", "Dockerfile"),
		filepath.Join(f.artifacts, "node-release-key.asc"),
	}
}

func (f *fixture) snapshot(t *testing.T) map[string][]byte {
	t.Helper()
	original := make(map[string][]byte, len(f.destinations()))
	for _, path := range f.destinations() {
		original[path] = harness.ReadFile(t, path)
	}
	return original
}

// unchanged lists the destinations whose bytes moved.
func (f *fixture) unchanged(t *testing.T) []string {
	t.Helper()
	var changed []string
	for path, raw := range f.original {
		if string(harness.ReadFile(t, path)) != string(raw) {
			changed = append(changed, path)
		}
	}
	return changed
}

func (f *fixture) currentNodeJSON(t *testing.T) map[string]any {
	t.Helper()
	var pin map[string]any
	if err := json.Unmarshal(harness.ReadFile(t, filepath.Join(f.artifacts, "node.json")), &pin); err != nil {
		t.Fatalf("the published pin is not JSON: %v", err)
	}
	return pin
}

// archive writes the reviewed npm payload for one platform and returns its bytes.
func (f *fixture) archive(t *testing.T, platform string) []byte {
	t.Helper()
	token := platforms[platform]
	filename := fmt.Sprintf("node-v%s-%s.tar.xz", version, token)
	body := harness.TarXZ(t, []harness.Member{{
		Name: fmt.Sprintf("node-v%s-%s/lib/node_modules/npm/package.json", version, token),
		Body: []byte(fmt.Sprintf(`{"version": %q}`, npmVersion)),
	}})
	f.upstream.Write(t, fmt.Sprintf("https://nodejs.org/dist/v%s/%s", version, filename), body)
	return body
}

// serve writes both archives, the signed manifest and the detached signature.
func (f *fixture) serve(t *testing.T) {
	t.Helper()
	var lines []string
	for platform := range platforms {
		filename := fmt.Sprintf("node-v%s-%s.tar.xz", version, platforms[platform])
		lines = append(lines, harness.SHA256Hex(f.archive(t, platform))+"  "+filename)
	}
	manifest := f.upstream.Path(t, fmt.Sprintf("https://nodejs.org/dist/v%s/SHASUMS256.txt", version))
	harness.WriteFile(t, manifest, []byte(strings.Join(lines, "\n")+"\n"))
	f.signer.SignDetached(t, manifest)
}

func (f *fixture) run(t *testing.T, arguments ...string) harness.Result {
	t.Helper()
	argv := append([]string{"bash", f.tool, "--root", f.root, "--fixture-upstream", f.upstream.Dir}, arguments...)
	return harness.Run(t, harness.Spec{
		Argv:  argv,
		Dir:   f.repository,
		Unset: []string{"GITHUB_ACTIONS", "GNUPGHOME", "TZ"},
	})
}

func (f *fixture) refresh(t *testing.T, spec harness.Spec, version string) harness.Result {
	t.Helper()
	spec.Argv = []string{"bash", f.tool, "--root", f.root, "--fixture-upstream", f.upstream.Dir,
		"--artifact", "node", "--version", version}
	spec.Dir = f.repository
	if spec.Unset == nil {
		spec.Unset = []string{"GITHUB_ACTIONS", "GNUPGHOME", "TZ"}
	}
	return harness.Run(t, spec)
}

func TestCleanRefreshPublishes(t *testing.T) {
	f := newFixture(t, "refresh")
	result := f.refresh(t, harness.Spec{}, version)
	result.RequireStatus(t, 0)
	result.RequireStdout(t, "published node "+version)

	pin := f.currentNodeJSON(t)
	if pin["version"] != version {
		t.Fatalf("the pin records version %v, want %s", pin["version"], version)
	}
	bundled, ok := pin["bundled"].(map[string]any)
	if !ok || bundled["npm"] != npmVersion {
		t.Fatalf("the pin records bundled %v, want npm %s", pin["bundled"], npmVersion)
	}
	dockerfile := string(harness.ReadFile(t, filepath.Join(f.root, "images", "Dockerfile")))
	if label := fmt.Sprintf("LABEL org.agent-vm.version.node=%q", version); !strings.Contains(dockerfile, label) {
		t.Fatalf("the refreshed label does not follow the pin: %s is missing", label)
	}
	authentication, ok := pin["authentication"].(map[string]any)
	if !ok {
		t.Fatalf("the pin carries no authentication block: %v", pin)
	}
	key, ok := authentication["key"].(map[string]any)
	if !ok {
		t.Fatalf("the pin does not name the key it was verified with: %v", authentication)
	}
	if digest := harness.SHA256Hex(harness.ReadFile(t, f.key)); key["sha256"] != digest {
		t.Fatalf("the pin binds key sha256 %v, but the key it was verified with is %s", key["sha256"], digest)
	}

	published := f.snapshot(t)
	again := f.refresh(t, harness.Spec{}, version)
	again.RequireStatus(t, 0)
	again.RequireStdout(t, "unchanged (node "+version+")")
	for path, raw := range published {
		if string(harness.ReadFile(t, path)) != string(raw) {
			t.Fatalf("a second identical refresh rewrote %s", path)
		}
	}
}

func TestSecondArchitectureFailurePublishesNothing(t *testing.T) {
	f := newFixture(t, "second")
	f.upstream.Remove(t, fmt.Sprintf("https://nodejs.org/dist/v%s/node-v%s-linux-arm64.tar.xz", version, version))
	result := f.refresh(t, harness.Spec{}, version)
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "cannot fetch")
	if changed := f.unchanged(t); len(changed) != 0 {
		t.Fatalf("a failed second architecture rewrote %v", changed)
	}
}

func TestManifestDisagreementIsRefused(t *testing.T) {
	f := newFixture(t, "disagrees")
	url := fmt.Sprintf("https://nodejs.org/dist/v%s/node-v%s-linux-arm64.tar.xz", version, version)
	f.upstream.Write(t, url, append(f.upstream.Read(t, url), []byte("drift")...))
	result := f.refresh(t, harness.Spec{}, version)
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "the signed manifest disagrees")
	if changed := f.unchanged(t); len(changed) != 0 {
		t.Fatalf("a manifest disagreement rewrote %v", changed)
	}
}

func TestForeignSignatureIsRefused(t *testing.T) {
	f := newFixture(t, "foreign")
	harness.RequireTools(t, "gpg")
	foreign := harness.NewSigner(t, filepath.Join(harness.TempDir(t, "foreign-signer."), "home"))
	manifest := f.upstream.Path(t, fmt.Sprintf("https://nodejs.org/dist/v%s/SHASUMS256.txt", version))
	foreign.SignDetached(t, manifest)

	result := f.refresh(t, harness.Spec{}, version)
	result.RequireStatus(t, 1)
	if !strings.Contains(result.Stderr, "does not verify") && !strings.Contains(result.Stderr, "signed by") {
		t.Fatalf("a manifest signed by another key is refused with no reason\n%s", result.Report())
	}
	if changed := f.unchanged(t); len(changed) != 0 {
		t.Fatalf("a foreign signature rewrote %v", changed)
	}
}

func TestSymlinkedDestinationsAreRefused(t *testing.T) {
	f := newFixture(t, "symlink")
	work := filepath.Dir(f.root)
	elsewhere := harness.Scratch(t, work, "elsewhere")
	real := filepath.Join(f.artifacts, "node.json")
	moved := filepath.Join(elsewhere, "node.json")
	if err := os.Rename(real, moved); err != nil {
		t.Fatalf("cannot move the pin aside: %v", err)
	}
	if err := os.Symlink(moved, real); err != nil {
		t.Fatalf("cannot link the pin: %v", err)
	}
	result := f.refresh(t, harness.Spec{}, version)
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "symlink")
	if string(harness.ReadFile(t, moved)) != string(f.original[real]) {
		t.Fatalf("a symlinked destination was written through")
	}
	if err := os.Remove(real); err != nil {
		t.Fatalf("cannot remove the link: %v", err)
	}
	if err := os.Rename(moved, real); err != nil {
		t.Fatalf("cannot restore the pin: %v", err)
	}

	target := filepath.Join(work, "artifacts-target")
	if err := os.Rename(f.artifacts, target); err != nil {
		t.Fatalf("cannot move the artifacts directory aside: %v", err)
	}
	if err := os.Symlink(target, f.artifacts); err != nil {
		t.Fatalf("cannot link the artifacts directory: %v", err)
	}
	result = f.refresh(t, harness.Spec{}, version)
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "symlink")
	if strings.Contains(string(harness.ReadFile(t, filepath.Join(target, "node.json"))), version) {
		t.Fatalf("a symlinked parent was written through: the staged bytes landed")
	}
}

func TestCIAndInexactVersionAreRefused(t *testing.T) {
	f := newFixture(t, "ci")
	result := f.refresh(t, harness.Spec{Env: map[string]string{"GITHUB_ACTIONS": "true"}}, version)
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "refusing GitHub Actions")
	if changed := f.unchanged(t); len(changed) != 0 {
		t.Fatalf("a refused CI run rewrote %v", changed)
	}
	result = f.refresh(t, harness.Spec{}, "9.9")
	result.RequireStatus(t, 2)
	result.RequireStderr(t, "X.Y.Z")
}

// TestUnrestorablePublicationExitsThree is the one case where the writer must not
// claim its sources are unchanged. The pin file is read-only, so the first copy
// fails *and* the restore of that same file fails: it has to keep its backups and
// say where they are.
func TestUnrestorablePublicationExitsThree(t *testing.T) {
	f := newFixture(t, "unrestorable")
	pin := filepath.Join(f.artifacts, "node.json")
	if err := os.Chmod(pin, 0o400); err != nil {
		t.Fatalf("cannot make the pin read-only: %v", err)
	}
	result := f.refresh(t, harness.Spec{}, version)
	if err := os.Chmod(pin, 0o644); err != nil {
		t.Fatalf("cannot restore the pin mode: %v", err)
	}
	result.RequireStatus(t, 3)
	result.RequireStderr(t, "restore incomplete")
	result.RequireStderr(t, "STOP: recover")

	// The retained directory is the one the message names. Taking it from the
	// message is the point: a maintainer has nothing else to go on.
	pattern := regexp.MustCompile(`(/[^\s:;]+/pin-artifact\.[A-Za-z0-9_]+)`)
	var retained []string
	for _, match := range pattern.FindAllStringSubmatch(result.Stderr, -1) {
		if info, err := os.Stat(match[1]); err == nil && info.IsDir() {
			retained = append(retained, match[1])
		}
	}
	if len(retained) == 0 {
		t.Fatalf("an incomplete recovery did not retain a work directory\n%s", result.Report())
	}
	record := filepath.Join(retained[len(retained)-1], "recovery.json")
	var value map[string]any
	if err := json.Unmarshal(harness.ReadFile(t, record), &value); err != nil {
		t.Fatalf("the retained recovery record is not JSON: %v", err)
	}
	if value["phase"] != "publication" {
		t.Fatalf("the recovery record names phase %v, want publication", value["phase"])
	}
}

func TestCheckAcceptsCommittedPinsAndRefusesMutatedOnes(t *testing.T) {
	f := newFixture(t, "check")
	// The committed pins bind the committed key; a refresh would have replaced it.
	harness.WriteFile(t, f.key, f.committed)
	committed := harness.Run(t, harness.Spec{
		Argv:  []string{"bash", f.tool, "--check"},
		Dir:   f.repository,
		Unset: []string{"GITHUB_ACTIONS"},
	})
	if committed.Status != 0 {
		t.Fatalf("--check refused the repository pins\n%s", committed.Report())
	}

	pin := f.currentNodeJSON(t)
	pin["platforms"].(map[string]any)["linux/arm64"].(map[string]any)["sha256"] = strings.Repeat("0", 64)
	pin["install"].(map[string]any)["payload"] = []string{"../escape"}
	mutated, err := json.MarshalIndent(pin, "", "  ")
	if err != nil {
		t.Fatalf("cannot encode the mutated pin: %v", err)
	}
	harness.WriteFile(t, filepath.Join(f.artifacts, "node.json"), append(mutated, '\n'))
	result := f.run(t, "--check")
	result.RequireStatus(t, 1)
	result.RequireStderr(t, "FAIL")

	harness.WriteFile(t, filepath.Join(f.artifacts, "node.json"), f.original[filepath.Join(f.artifacts, "node.json")])
	f.run(t, "--check").RequireStatus(t, 0)
}

// TestPinTimeVerificationUsesItsOwnKeyring checks the keyring the writer
// constructs. The writer builds a private GNUPGHOME; if it is never passed to gpg,
// the host's ambient keyring takes the import and decides the check (#96 review).
// The ambient home here is a decoy: after a successful verification it holds
// nothing.
func TestPinTimeVerificationUsesItsOwnKeyring(t *testing.T) {
	f := newFixture(t, "isolation")
	ambient := harness.Scratch(t, filepath.Dir(f.root), "ambient-gnupg")
	if err := os.Chmod(ambient, 0o700); err != nil {
		t.Fatalf("cannot restrict the ambient keyring: %v", err)
	}
	result := f.refresh(t, harness.Spec{Env: map[string]string{"GNUPGHOME": ambient}}, version)
	result.RequireStatus(t, 0)

	authentication := f.currentNodeJSON(t)["authentication"].(map[string]any)
	key := authentication["key"].(map[string]any)
	if key["fingerprint"] != f.fingerprint {
		t.Fatalf("the verification reports fingerprint %v, want %s", key["fingerprint"], f.fingerprint)
	}
	entries, err := os.ReadDir(ambient)
	if err != nil {
		t.Fatalf("cannot read the ambient keyring: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("the ambient keyring was touched: %v", names)
	}
}

// clockZones are the two extreme zones: Kiritimati is UTC+14 and Etc/GMT+12 is
// UTC-12, so at least one of the two always has a local date other than the UTC
// date. That is what makes the pair time-of-day independent instead of a coin flip.
var clockZones = []string{"Pacific/Kiritimati", "Etc/GMT+12"}

// TestVerifiedOnIsTheUTCDate is about the real process, not a date function: the
// stamp is read back from a published pin in each zone.
func TestVerifiedOnIsTheUTCDate(t *testing.T) {
	label := "clock_writer_stamps_the_utc_date"
	for attempt := 0; attempt < 2; attempt++ {
		today := time.Now().UTC().Format("2006-01-02")
		differing := false
		for _, zone := range clockZones {
			location, err := time.LoadLocation(zone)
			if err != nil {
				t.Fatalf("cannot load %s: %v", zone, err)
			}
			local := time.Now().In(location).Format("2006-01-02")
			if local == today {
				continue
			}
			differing = true
			f := newFixture(t, "clock-"+strings.ReplaceAll(zone, "/", "-"))
			result := f.refresh(t, harness.Spec{Env: map[string]string{"TZ": zone}}, version)
			result.RequireStatus(t, 0)
			stamped := f.currentNodeJSON(t)["authentication"].(map[string]any)["verified_on"]
			if stamped != today {
				t.Fatalf("%s: under TZ=%s, where the local date is %s, the pin is stamped %v, not the UTC date %s",
					label, zone, local, stamped, today)
			}
			break
		}
		if time.Now().UTC().Format("2006-01-02") != today {
			continue
		}
		if !differing {
			t.Fatalf("%s: no candidate zone differs from the UTC date", label)
		}
		return
	}
	t.Fatalf("%s: the UTC date rolled over twice while this case ran", label)
}
