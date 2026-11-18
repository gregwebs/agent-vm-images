// Package claude is the black-box suite for the committed Claude native payload
// closure (#98).
//
// Every case but one drives a real program. The reader cases run
// `recipe-contract claude-pin` (`internal/claudepin` + `internal/cli`), the writer
// cases run `script/dev/pin-claude.sh` / `avm-pin claude`, and the closures they are
// pointed at are synthesized here: real OpenPGP keys, real signatures, real ELF
// headers, real layout checks. Nothing reaches the network and no committed evidence
// is touched. The exception is the fixture seam: the case that holds
// `avm-test write-fixture` to its layout runs the builder in-process, through the same
// `harness.Command` adapter the command calls, because the builder -- not a program --
// is what that case is about.
//
// The negative cases are the point: a rewritten manifest, a stale or foreign
// signature, a substituted key, a signing subkey pinned as the primary, a wrong-machine
// ELF, a group-writable directory, a launcher that points elsewhere, a native state
// file that says updates are on.
//
// Three kinds of seam are used, and no others:
//
//   - a mutated pin or closure, for everything the reader decides from committed
//     bytes;
//   - a stub `gpg`/`gpgconf` on PATH, for the two things only the keyring boundary
//     can express (an unusable listing, a failed import) and for proving a refusal
//     happened before gpg was reached;
//   - two writer flags, `--fixture-upstream DIR` (serve `https://host/path` from
//     `DIR/host/path`) and `--signer FPR` (hold the pin to a throwaway primary
//     instead of the reviewed one), because a fixture closure is signed by a key the
//     review has never seen.
//
// There is no monkeypatching and no import of the program under test: the programs
// are binaries, so a case that cannot be expressed this way does not exist here.
//
// The pin date is UTC on both sides, so the clock cases run the real programs as
// separate processes under `TZ=Pacific/Kiritimati` and `TZ=Etc/GMT+12`: a host's
// local date only exists inside a real zone, and it is exactly a local-date
// comparison that the UTC pin date replaced.
//
// gpg homes: the reader allocates its own under /tmp, never an inherited TMPDIR, so
// the suite gives the same answer whether TMPDIR is short or long; the `gpg home`
// cases cover the long TMPDIR and assert the home is a private directory under /tmp
// even then.
//
// The fixture's own gpg homes are deliberately separate from the reader's
// (`internal/claudepin`'s `GPGHome`): a short transient 0700 home under /tmp for key
// generation, export, signing and two-key assembly. A defect in the program under
// test must not be able to mask a fixture defect, and vice versa.
package claude

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

const (
	// Version is the release every fixture closure pins unless a case is proving a
	// refusal.
	Version = "9.9.9"

	// The pin directory and the Dockerfile slot, at the same relative paths
	// production uses.
	pinDirectory      = "images/tools/claude"
	pinFile           = pinDirectory + "/pin.json"
	evidenceDirectory = pinDirectory + "/evidence"
	dockerfile        = "images/standard/Dockerfile"
	dockerfileArg     = "ARG AGENT_VERSION_CLAUDE="

	// The closure's committed file names and the upstream origin they are named
	// after. The reader recomputes every URL from the version, so these are the
	// fixture's own copy of a fact the pin is held to.
	manifestName  = "manifest.json"
	signatureName = "manifest.json.sig"
	keyName       = "claude-code-release-key.asc"
	downloadBase  = "https://downloads.claude.ai/claude-code-releases"
	keyURL        = "https://downloads.claude.ai/keys/claude-code.asc"
	// upstreamBase is `downloadBase` without its scheme: `--fixture-upstream` serves
	// `https://host/path` from `DIR/host/path`.
	upstreamBase   = "downloads.claude.ai/claude-code-releases"
	upstreamKeyURL = "downloads.claude.ai/keys/claude-code.asc"

	fixtureUID = "fixture claude signer <fixture@example.invalid>"

	// fixtureReason is the `authentication.reason` a fixture closure carries.
	fixtureReason = "Fixture closure: the manifest is the selection authority and " +
		"the detached signature is verified with the committed key."

	// gpg-agent's socket path has a length limit, so a fixture home lives under the
	// short fixed root and its length is checked rather than left to a gpg failure.
	// It is a separate copy from the reader's rule on purpose.
	gpgHomeRoot     = "/tmp"
	gpgHomePrefix   = "claude-fixture-gpg."
	maxGPGHomeChars = 64

	elfHeaderBytes = 20
)

// writerReason is the `authentication.reason` the writer stamps, copied rather than
// imported: a fixture closure is written with it so a writer run over that closure at
// the same version is byte-identical and prints `unchanged`, and importing the
// writer's own constant would let the fixture and the program agree by construction.
const writerReason = "The manifest is the selection authority: the build verifies " +
	"the detached OpenPGP signature over the committed manifest.json with the " +
	"committed release key, requires the pinned primary fingerprint, and refuses " +
	"unless the signed checksum and size for the platform equal the committed ones " +
	"before the executable is fetched. The executable is placed in the native layout " +
	"and `claude install` runs only with no network, so the native installer's own " +
	"downloads never happen. See ADR 0013."

// Fingerprint is one OpenPGP primary identity: 40 upper-case hex characters. A named
// type because a fingerprint and a key file path are both strings and mean opposite
// things to every gpg call in this file.
type Fingerprint string

// token is a manifest platform token, which is also the CDN path segment.
type token string

const (
	linuxX64     token = "linux-x64"
	linuxARM64   token = "linux-arm64"
	linuxX64Musl token = "linux-x64-musl"
	darwinARM64  token = "darwin-arm64"
)

// machineNumber is an ELF `e_machine`. A named type because the class and the endian
// word are also small integers in the same header.
type machineNumber int

const (
	amd64Machine machineNumber = 62
	arm64Machine machineNumber = 183
)

// platform is one maintained architecture, keyed the pin's way and tokenized the
// manifest's way.
type platform struct {
	key   string
	token token
}

// platforms are the two maintained architectures, in the order the pin lists them.
var platforms = []platform{
	{key: "linux/amd64", token: linuxX64},
	{key: "linux/arm64", token: linuxARM64},
}

// --- scratch and the shared template ------------------------------------------

// scratch is a per-case directory inside this process's temporary tree.
func scratch(r harness.Reporter, label string) string {
	r.Helper()
	return harness.TempDir(r, "claude-pins."+fileName(label)+".")
}

// fileName turns a case label into one path segment.
func fileName(label string) string {
	replaced := strings.NewReplacer(" ", "-", "/", "-", ":", "-").Replace(label)
	return strings.Trim(replaced, "-")
}

// fixture is one signer, two ELF blobs and a consistent closure, built once for this
// test process: key generation dominates the setup cost of every group otherwise.
type fixture struct {
	root    string
	signer  *Signer
	x64     string
	arm64   string
	closure string
	counter int
}

var processFixture struct {
	sync.Mutex
	fixture *fixture
}

// testFixture is the process template, created on first use. Its directory is
// removed by TestMain rather than by a case, because a case's cleanup would take the
// template away from the case after it.
func testFixture(r harness.Reporter) *fixture {
	r.Helper()
	processFixture.Lock()
	defer processFixture.Unlock()
	if processFixture.fixture != nil {
		return processFixture.fixture
	}
	root, err := os.MkdirTemp("", "claude-pins.template.")
	if err != nil {
		r.Fatalf("cannot create the fixture template directory: %v", err)
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		r.Fatalf("cannot resolve %s: %v", root, err)
	}
	processFixture.fixture = buildFixture(r, root)
	return processFixture.fixture
}

func buildFixture(r harness.Reporter, root string) *fixture {
	r.Helper()
	built := &fixture{root: root}
	built.signer = makeSigner(r, filepath.Join(root, "signer"), false)
	built.x64 = elfBlob(r, filepath.Join(root, "assets", string(linuxX64)), amd64Machine, elfShape{})
	built.arm64 = elfBlob(r, filepath.Join(root, "assets", string(linuxARM64)), arm64Machine, elfShape{})
	built.closure = buildClosure(r, closureRequest{
		Out:     filepath.Join(root, "closure"),
		Signer:  built.signer,
		Version: Version,
		AMD64:   built.x64,
		ARM64:   built.arm64,
	})
	return built
}

// copy is a fresh copy of the template closure, which a case may mutate.
func (f *fixture) copy(r harness.Reporter, label string) string {
	r.Helper()
	f.counter++
	destination := filepath.Join(scratch(r, fmt.Sprintf("%s.%d", label, f.counter)), "source")
	harness.CopyTree(r, f.closure, destination)
	return destination
}

// mutate is a copy whose pin has been changed. The evidence still matches the pin's
// other fields, so only the mutation can be the refusal.
func (f *fixture) mutate(r harness.Reporter, label string, mutate func(map[string]any)) string {
	r.Helper()
	f.counter++
	destination := filepath.Join(scratch(r, fmt.Sprintf("%s.%d", label, f.counter)), "source")
	harness.CopyTree(r, f.closure, destination)
	rewritePin(r, destination, mutate)
	return destination
}

// resign is a copy whose signed manifest has been changed and re-signed, with the
// pin's digests brought along: a consistent closure that says something else.
func (f *fixture) resign(r harness.Reporter, label string, mutate func(map[string]any)) string {
	r.Helper()
	directory := f.copy(r, label)
	rewriteManifest(r, directory, f.signer, mutate)
	return directory
}

// --- signer -------------------------------------------------------------------

// Signer is one throwaway OpenPGP key and the files it was written to.
type Signer struct {
	Directory   string
	Fingerprint Fingerprint
	// Signing is the fingerprint that signs: the primary, or its signing subkey.
	Signing Fingerprint
	Key     string
	Secret  string
}

// gpgHome is a short transient 0700 keyring under the fixed root, removed by the
// reporter's cleanup.
type gpgHome struct {
	Path string
	Env  map[string]string

	raw     string
	removed bool
}

func newGPGHome(r harness.Reporter) *gpgHome {
	r.Helper()
	raw, err := os.MkdirTemp(gpgHomeRoot, gpgHomePrefix)
	if err != nil {
		r.Fatalf("cannot create a fixture gpg home under %s: %v", gpgHomeRoot, err)
	}
	home := &gpgHome{raw: raw}
	r.Cleanup(home.close)
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		r.Fatalf("cannot resolve %s: %v", raw, err)
	}
	home.Path = resolved
	if len(resolved) > maxGPGHomeChars {
		r.Fatalf("the fixture gpg home %s is longer than %d characters", resolved, maxGPGHomeChars)
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	home.Env = map[string]string{"GNUPGHOME": resolved, "LC_ALL": "C", "PATH": path}
	return home
}

// close stops any agent this home started and removes the home.
func (h *gpgHome) close() {
	if h.removed {
		return
	}
	h.removed = true
	kill := exec.Command("gpgconf", "--kill", "all")
	kill.Env = gpgEnvironment(h.Env)
	_ = kill.Run()
	_ = os.RemoveAll(h.raw)
}

func gpgEnvironment(values map[string]string) []string {
	entries := make([]string, 0, len(values))
	for name, value := range values {
		entries = append(entries, name+"="+value)
	}
	sort.Strings(entries)
	return entries
}

// mustRun is one gpg call whose non-zero status is a fixture defect: a fixture that
// cannot sign must not be reported as the program refusing.
func (h *gpgHome) mustRun(r harness.Reporter, what string, argv ...string) string {
	r.Helper()
	result := harness.Run(r, harness.Spec{Argv: append([]string{"gpg"}, argv...), Env: h.Env})
	if result.Status != 0 {
		r.Fatalf("the fixture could not %s\n%s", what, result.Report())
	}
	return result.Stdout
}

// primaries and subkeys are the fingerprints gpg lists, each from the record it
// follows: a `pub` then its `fpr`, a `sub` then its `fpr`.
func (h *gpgHome) fingerprints(r harness.Reporter, records []string) []Fingerprint {
	r.Helper()
	listed := h.mustRun(r, "list the fixture keys", "--batch", "--with-colons", "--list-keys")
	found := []Fingerprint{}
	pending := false
	for _, line := range strings.Split(listed, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) == 0 {
			continue
		}
		if contains(records, fields[0]) {
			pending = true
			continue
		}
		if fields[0] != "fpr" || !pending {
			continue
		}
		pending = false
		if len(fields) < 10 {
			r.Fatalf("the fixture key listing has an unreadable fpr record: %q", line)
		}
		found = append(found, Fingerprint(strings.ToUpper(fields[9])))
	}
	return found
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// makeSigner generates one throwaway key. With subkey the primary is cert-only and a
// signing subkey does the signing, which is the shape Anthropic's key has.
func makeSigner(r harness.Reporter, out string, subkey bool) *Signer {
	r.Helper()
	if err := os.MkdirAll(out, 0o755); err != nil {
		r.Fatalf("cannot create the signer directory %s: %v", out, err)
	}
	home := newGPGHome(r)
	usage := "default"
	if subkey {
		usage = "cert"
	}
	home.mustRun(r, "generate the fixture key", "--batch", "--quiet",
		"--pinentry-mode", "loopback", "--passphrase", "",
		"--quick-generate-key", fixtureUID, "ed25519", usage, "0")
	primaries := home.fingerprints(r, []string{"pub"})
	if len(primaries) != 1 {
		r.Fatalf("the fixture key generation left %d primary keys, want 1", len(primaries))
	}
	fingerprint := primaries[0]
	signing := fingerprint
	if subkey {
		home.mustRun(r, "add the signing subkey", "--batch", "--quiet",
			"--pinentry-mode", "loopback", "--passphrase", "",
			"--quick-add-key", string(fingerprint), "ed25519", "sign", "0")
		subkeys := home.fingerprints(r, []string{"sub", "ssb"})
		if len(subkeys) != 1 {
			r.Fatalf("the fixture signing subkey addition left %d subkeys, want 1", len(subkeys))
		}
		signing = subkeys[0]
	}
	public := home.mustRun(r, "export the fixture public key", "--batch", "--armor",
		"--export", string(fingerprint))
	secret := home.mustRun(r, "export the fixture secret key", "--batch", "--armor",
		"--export-secret-keys", string(fingerprint))
	signer := &Signer{
		Directory:   out,
		Fingerprint: fingerprint,
		Signing:     signing,
		Key:         filepath.Join(out, "key.asc"),
		Secret:      filepath.Join(out, "secret.asc"),
	}
	harness.WriteFile(r, signer.Key, []byte(public))
	harness.WriteFile(r, signer.Secret, []byte(secret))
	harness.WriteFile(r, filepath.Join(out, "fingerprint"), []byte(string(fingerprint)+"\n"))
	if subkey {
		harness.WriteFile(r, filepath.Join(out, "signing-subkey"), []byte(string(signing)+"\n"))
	}
	return signer
}

// readSigner is a signer already written by makeSigner, which is how a second process
// (the fixture CLI, the bumper) reaches the same key.
func readSigner(r harness.Reporter, directory string, subkey bool) *Signer {
	r.Helper()
	fingerprint := Fingerprint(strings.TrimSpace(string(harness.ReadFile(r,
		filepath.Join(directory, "fingerprint")))))
	signing := fingerprint
	if subkey {
		signing = Fingerprint(strings.TrimSpace(string(harness.ReadFile(r,
			filepath.Join(directory, "signing-subkey")))))
	}
	return &Signer{
		Directory:   directory,
		Fingerprint: fingerprint,
		Signing:     signing,
		Key:         filepath.Join(directory, "key.asc"),
		Secret:      filepath.Join(directory, "secret.asc"),
	}
}

// sign is one detached OpenPGP signature over the bytes of data.
//
// fakedTime moves the signature packet's creation time, which is how a case produces
// a *different* but valid signature over the same bytes: EdDSA is deterministic, so
// re-signing at the same time would reproduce the original bytes.
func sign(r harness.Reporter, signer *Signer, data string, fakedTime int64) []byte {
	r.Helper()
	home := newGPGHome(r)
	home.mustRun(r, "import the fixture secret key", "--batch", "--quiet", "--import", signer.Secret)
	output := filepath.Join(home.Path, "data.sig")
	argv := []string{"--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase", ""}
	if fakedTime != 0 {
		argv = append(argv, fmt.Sprintf("--faked-system-time=%d!", fakedTime))
	}
	argv = append(argv, "--detach-sign", "--output", output, data)
	home.mustRun(r, "sign the fixture manifest", argv...)
	return harness.ReadFile(r, output)
}

// --- ELF, manifest and closure ------------------------------------------------

// elfShape is one fixture ELF's header. A zero field means the default the suite's
// blobs carry: ELFCLASS64, little-endian, 4096 bytes, no marker.
type elfShape struct {
	Class  int
	Endian int
	Size   int
	Marker []byte
}

func elfBlob(r harness.Reporter, path string, machine machineNumber, shape elfShape) string {
	r.Helper()
	if shape.Class == 0 {
		shape.Class = 2
	}
	if shape.Endian == 0 {
		shape.Endian = 1
	}
	if shape.Size == 0 {
		shape.Size = 4096
	}
	order := binary.ByteOrder(binary.BigEndian)
	if shape.Endian == 1 {
		order = binary.LittleEndian
	}
	// e_ident is 16 bytes: magic, class, data, version, osabi, abiversion, pad.
	header := make([]byte, elfHeaderBytes)
	copy(header, "\x7fELF")
	header[4] = byte(shape.Class)
	header[5] = byte(shape.Endian)
	header[6] = 1
	order.PutUint16(header[16:18], 2)
	order.PutUint16(header[18:20], uint16(machine))
	body := append(header, shape.Marker...)
	raw := make([]byte, max(shape.Size, len(body)))
	copy(raw, body)
	harness.WriteFile(r, path, raw)
	return path
}

// manifestDocument is the signed manifest's real shape, including the platforms this
// pin ignores. The byte order of a JSON object is nobody's contract: the pin and the
// signature are taken over whatever bytes this file serves.
type manifestDocument struct {
	Version                      string                     `json:"version"`
	ManifestSignatureEnforcement string                     `json:"manifestSignatureEnforcement"`
	Commit                       string                     `json:"commit"`
	ModsCommit                   string                     `json:"modsCommit"`
	BuildDate                    string                     `json:"buildDate"`
	Platforms                    map[token]manifestPlatform `json:"platforms"`
	SDKCompat                    sdkCompatDocument          `json:"sdkCompat"`
}

type manifestPlatform struct {
	Binary   string `json:"binary"`
	Checksum string `json:"checksum"`
	Size     int    `json:"size"`
}

type sdkCompatDocument struct {
	HarnessSchema         int      `json:"harnessSchema"`
	TestedWrapperVersions []string `json:"testedWrapperVersions"`
}

// manifestBytes renders the manifest for one release: the two maintained platforms
// plus the two this pin ignores, which is what upstream serves.
func manifestBytes(r harness.Reporter, version string, executables map[token]string) []byte {
	r.Helper()
	served := map[token]manifestPlatform{}
	for name, path := range executables {
		raw := harness.ReadFile(r, path)
		served[name] = manifestPlatform{
			Binary:   "claude",
			Checksum: harness.SHA256Hex(raw),
			Size:     len(raw),
		}
	}
	return encodeDocument(r, manifestDocument{
		Version:                      version,
		ManifestSignatureEnforcement: "flag",
		Commit:                       strings.Repeat("0", 40),
		ModsCommit:                   strings.Repeat("1", 40),
		BuildDate:                    "2026-09-30T15:55:40Z",
		Platforms:                    served,
		SDKCompat: sdkCompatDocument{
			HarnessSchema:         1,
			TestedWrapperVersions: []string{"0.3.285"},
		},
	})
}

// pinDocument is the closure's pin, in the committed field order. Every URL is
// computed from the version, as the reader recomputes it.
type pinDocument struct {
	Schema         int                    `json:"schema"`
	Name           string                 `json:"name"`
	Kind           string                 `json:"kind"`
	Version        string                 `json:"version"`
	Manifest       evidenceDocument       `json:"manifest"`
	Signature      evidenceDocument       `json:"signature"`
	Key            keyDocument            `json:"key"`
	Platforms      pinPlatformsDocument   `json:"platforms"`
	Authentication authenticationDocument `json:"authentication"`
}

type evidenceDocument struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type keyDocument struct {
	URL         string `json:"url"`
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	SHA256      string `json:"sha256"`
}

type pinPlatformsDocument struct {
	AMD64 executableDocument `json:"linux/amd64"`
	ARM64 executableDocument `json:"linux/arm64"`
}

type executableDocument struct {
	ManifestPlatform string `json:"manifest_platform"`
	URL              string `json:"url"`
	SHA256           string `json:"sha256"`
	Size             int    `json:"size"`
}

type authenticationDocument struct {
	Method     string `json:"method"`
	VerifiedOn string `json:"verified_on"`
	Reason     string `json:"reason"`
}

// closureRequest is one closure to build. A key, a fingerprint and a signature path
// may each be overridden, which is how a case builds a closure whose pin and evidence
// are internally consistent while one link is wrong.
type closureRequest struct {
	Out         string
	Signer      *Signer
	Version     string
	AMD64       string
	ARM64       string
	Key         string
	Fingerprint Fingerprint
	Signature   string
	Reason      string
}

func buildClosure(r harness.Reporter, request closureRequest) string {
	r.Helper()
	reason := request.Reason
	if reason == "" {
		reason = fixtureReason
	}
	evidence := filepath.Join(request.Out, "evidence")
	if err := os.MkdirAll(evidence, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", evidence, err)
	}
	keyPath := request.Key
	if keyPath == "" {
		keyPath = request.Signer.Key
	}
	keyBytes := harness.ReadFile(r, keyPath)
	manifest := manifestBytes(r, request.Version, map[token]string{
		linuxX64:     request.AMD64,
		linuxARM64:   request.ARM64,
		linuxX64Musl: request.AMD64,
		darwinARM64:  request.ARM64,
	})
	manifestPath := filepath.Join(evidence, manifestName)
	harness.WriteFile(r, manifestPath, manifest)
	// A caller may supply a signature made over *other* bytes: that is how a case
	// builds a closure whose pin and evidence digests are internally consistent while
	// the signature is stale.
	detached := []byte(nil)
	if request.Signature != "" {
		detached = harness.ReadFile(r, request.Signature)
	} else {
		detached = sign(r, request.Signer, manifestPath, 0)
	}
	signaturePath := filepath.Join(evidence, signatureName)
	harness.WriteFile(r, signaturePath, detached)
	harness.WriteFile(r, filepath.Join(request.Out, keyName), keyBytes)
	pinnedAMD64 := harness.ReadFile(r, request.AMD64)
	pinnedARM64 := harness.ReadFile(r, request.ARM64)
	fingerprint := request.Fingerprint
	if fingerprint == "" {
		fingerprint = request.Signer.Fingerprint
	}
	executable := func(name string, payload []byte) executableDocument {
		return executableDocument{
			ManifestPlatform: name,
			URL:              downloadBase + "/" + request.Version + "/" + name + "/claude",
			SHA256:           harness.SHA256Hex(payload),
			Size:             len(payload),
		}
	}
	document := pinDocument{
		Schema:  1,
		Name:    "claude",
		Kind:    "claude-native-release",
		Version: request.Version,
		Manifest: evidenceDocument{
			URL:    downloadBase + "/" + request.Version + "/" + manifestName,
			Path:   "evidence/" + manifestName,
			SHA256: harness.SHA256Hex(manifest),
		},
		Signature: evidenceDocument{
			URL:    downloadBase + "/" + request.Version + "/" + signatureName,
			Path:   "evidence/" + signatureName,
			SHA256: harness.SHA256Hex(detached),
		},
		Key: keyDocument{
			URL:         keyURL,
			Path:        keyName,
			Fingerprint: strings.ToUpper(string(fingerprint)),
			SHA256:      harness.SHA256Hex(keyBytes),
		},
		Platforms: pinPlatformsDocument{
			AMD64: executable(string(linuxX64), pinnedAMD64),
			ARM64: executable(string(linuxARM64), pinnedARM64),
		},
		Authentication: authenticationDocument{
			Method: "openpgp-signed-manifest",
			// The fixture stamps what the writer stamps (the UTC date), or a
			// maintainer east of UTC would build closures the reader refuses as
			// future.
			VerifiedOn: utcDate(),
			Reason:     reason,
		},
	}
	harness.WriteFile(r, filepath.Join(request.Out, "pin.json"), encodeDocument(r, document))
	return request.Out
}

// utcDate is today's UTC date in the pin's spelling.
func utcDate() string { return time.Now().UTC().Format(time.DateOnly) }

// twoPrimaries concatenates two armored public keys into one file.
func twoPrimaries(r harness.Reporter, out, first, second string) string {
	r.Helper()
	harness.WriteFile(r, out, append(harness.ReadFile(r, first), harness.ReadFile(r, second)...))
	return out
}

// --- closure mutation ---------------------------------------------------------

// readPin parses a fixture pin. Numbers keep the spelling they were written with, so
// a case that rewrites one field does not renumber another.
func readPin(r harness.Reporter, directory string) map[string]any {
	r.Helper()
	return document(r, harness.ReadFile(r, filepath.Join(directory, "pin.json")))
}

// writePin renders a fixture pin the way the writer does: two-space indentation and
// one trailing newline.
func writePin(r harness.Reporter, directory string, pin map[string]any) {
	r.Helper()
	harness.WriteFile(r, filepath.Join(directory, "pin.json"), encodeDocument(r, pin))
}

// rewritePin changes one pin in place.
func rewritePin(r harness.Reporter, directory string, mutate func(map[string]any)) {
	r.Helper()
	pin := readPin(r, directory)
	mutate(pin)
	writePin(r, directory, pin)
}

// rewriteManifest changes the signed manifest, re-signs it and keeps the pin's
// digests consistent with the new bytes.
func rewriteManifest(r harness.Reporter, directory string, signer *Signer, mutate func(map[string]any)) {
	r.Helper()
	path := filepath.Join(directory, "evidence", manifestName)
	manifest := document(r, harness.ReadFile(r, path))
	mutate(manifest)
	raw := encodeDocument(r, manifest)
	harness.WriteFile(r, path, raw)
	signature := sign(r, signer, path, 0)
	harness.WriteFile(r, filepath.Join(directory, "evidence", signatureName), signature)
	rewritePin(r, directory, func(pin map[string]any) {
		object(r, pin, "manifest")["sha256"] = harness.SHA256Hex(raw)
		object(r, pin, "signature")["sha256"] = harness.SHA256Hex(signature)
	})
}

// --- JSON helpers -------------------------------------------------------------

// document parses a fixture JSON file.
func document(r harness.Reporter, raw []byte) map[string]any {
	r.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var parsed map[string]any
	if err := decoder.Decode(&parsed); err != nil {
		r.Fatalf("a fixture document is not JSON: %v", err)
	}
	return parsed
}

// object is a nested document, named by the keys that reached it, so a mutation that
// names `platforms`/`linux/amd64`/`size` cannot silently miss.
func object(r harness.Reporter, parent map[string]any, keys ...string) map[string]any {
	r.Helper()
	current := parent
	for _, key := range keys {
		found, ok := current[key].(map[string]any)
		if !ok {
			r.Fatalf("%q is not an object in this fixture document", key)
		}
		current = found
	}
	return current
}

// encodeDocument renders a fixture document the way the writer does.
func encodeDocument(r harness.Reporter, doc any) []byte {
	r.Helper()
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		r.Fatalf("cannot encode a fixture document: %v", err)
	}
	return buffer.Bytes()
}

// --- the gpg boundary ---------------------------------------------------------

// gpgStubMode is the one hostile thing a stub gpg does after it logs its home.
type gpgStubMode int

const (
	// stubLog delegates to the real gpg.
	stubLog gpgStubMode = iota
	// stubBomb fails loudly, for a case that must never reach gpg.
	stubBomb
	// stubFailImport fails a real (non-`show-only`) key import.
	stubFailImport
	// stubValidSig rewrites the primary fingerprint in every VALIDSIG status line.
	stubValidSig
	// stubListing answers the `show-only --import` listing from a file.
	stubListing
)

// stubRequest is one stub gpg.
type stubRequest struct {
	Mode gpgStubMode
	// ValidSigPrimary is the primary every VALIDSIG line is rewritten to.
	ValidSigPrimary Fingerprint
	// Listing is the file the listing mode prints.
	Listing string
}

// gpgStub is a PATH directory holding a `gpg` that logs its GNUPGHOME and then does
// one hostile thing.
//
// The reader hands every gpg call its own private home in the environment, so the log
// is also the record of which homes were used -- which is how a case proves a home
// lived under /tmp and was removed, or that gpg was never reached at all.
type gpgStub struct {
	Dir string
	Env map[string]string
}

func newGPGStub(r harness.Reporter, directory string, request stubRequest) *gpgStub {
	r.Helper()
	real, err := exec.LookPath("gpg")
	if err != nil {
		r.Fatalf("gpg is required to run this suite: %v", err)
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", directory, err)
	}
	log := filepath.Join(directory, "homes.log")
	header := "#!/bin/sh\nprintf '%s\\n' \"$GNUPGHOME\" >>'" + log + "'\n"
	body := ""
	switch request.Mode {
	case stubLog:
		body = header + "exec '" + real + "' \"$@\"\n"
	case stubBomb:
		body = header + "echo 'gpg: called when it must not be' >&2\nexit 99\n"
	case stubFailImport:
		body = header + `for argument in "$@"; do
    if [ "$argument" = '--import' ]; then
        case " $* " in
            *' --import-options show-only '*) ;;
            *) echo 'gpg: cannot connect to the gpg-agent' >&2; exit 2 ;;
        esac
    fi
done
exec '` + real + `' "$@"
`
	case stubValidSig:
		body = header + `for argument in "$@"; do
    if [ "$argument" = '--verify' ]; then
        '` + real + `' "$@" >"$GNUPGHOME/verify.out" 2>"$GNUPGHOME/verify.err"
        status=$?
        awk -v foreign='` + string(request.ValidSigPrimary) + `' 'BEGIN { OFS = " " }
             $1 == "[GNUPG:]" && $2 == "VALIDSIG" { $NF = foreign }
             { print }' "$GNUPGHOME/verify.out"
        cat "$GNUPGHOME/verify.err" >&2
        exit $status
    fi
done
exec '` + real + `' "$@"
`
	case stubListing:
		if request.Listing == "" {
			r.Fatalf("the listing stub needs a listing file")
		}
		body = header + `for argument in "$@"; do
    if [ "$argument" = '--import-options' ]; then
        cat '` + request.Listing + `'
        exit 0
    fi
done
exec '` + real + `' "$@"
`
	default:
		r.Fatalf("no stub gpg mode %d exists", request.Mode)
	}
	script := filepath.Join(directory, "gpg")
	harness.WriteFile(r, script, []byte(body))
	if err := os.Chmod(script, 0o755); err != nil {
		r.Fatalf("cannot make %s executable: %v", script, err)
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	return &gpgStub{Dir: directory, Env: map[string]string{"PATH": directory + string(os.PathListSeparator) + path}}
}

// homes is every GNUPGHOME the stub was called with, in order.
func (s *gpgStub) homes(r harness.Reporter) []string {
	r.Helper()
	path := filepath.Join(s.Dir, "homes.log")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	logged := []string{}
	for _, line := range strings.Split(string(harness.ReadFile(r, path)), "\n") {
		if line != "" {
			logged = append(logged, line)
		}
	}
	return logged
}

// --- the native install layout ------------------------------------------------

// installShape is one prefix in the native installer's shape. A zero field means the
// default the accepted case carries: a shell-script payload at 0755, the launcher
// pointing at that payload, no extra versions.
type installShape struct {
	Payload       []byte
	PayloadMode   os.FileMode
	Launcher      string
	ExtraVersions []string
}

// installLayout writes a prefix the native installer would have produced, owned by
// this process, and its state file.
func installLayout(r harness.Reporter, directory string, shape installShape) string {
	r.Helper()
	prefix := filepath.Join(directory, "prefix")
	versions := filepath.Join(prefix, ".local/share/claude/versions")
	if err := os.MkdirAll(versions, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", versions, err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, ".local/bin"), 0o755); err != nil {
		r.Fatalf("cannot create the launcher directory: %v", err)
	}
	payload := shape.Payload
	if payload == nil {
		payload = []byte("#!/bin/sh\nexit 0\n")
	}
	mode := shape.PayloadMode
	if mode == 0 {
		mode = 0o755
	}
	installed := filepath.Join(versions, Version)
	harness.WriteFile(r, installed, payload)
	if err := os.Chmod(installed, mode); err != nil {
		r.Fatalf("cannot set the payload mode: %v", err)
	}
	for _, extra := range shape.ExtraVersions {
		harness.WriteFile(r, filepath.Join(versions, extra), []byte("x"))
	}
	target := shape.Launcher
	if target == "" {
		target = installed
	}
	if err := os.Symlink(target, filepath.Join(prefix, ".local/bin/claude")); err != nil {
		r.Fatalf("cannot write the launcher: %v", err)
	}
	writeNativeState(r, prefix, nativeState{InstallMethod: "native", AutoUpdates: false})
	return prefix
}

// nativeState is the `.claude.json` the native installer writes. Both fields are
// `any` because a case has to write the wrong *type* too ("false" as a string, a
// missing key as nil).
type nativeState struct {
	InstallMethod any
	AutoUpdates   any
}

func writeNativeState(r harness.Reporter, prefix string, state nativeState) {
	r.Helper()
	harness.WriteFile(r, filepath.Join(prefix, ".claude.json"), encodeDocument(r, map[string]any{
		"installMethod":     state.InstallMethod,
		"autoUpdates":       state.AutoUpdates,
		"firstStartVersion": Version,
		"migrationVersion":  14,
	}))
}

// --- the writer's repository --------------------------------------------------

// proposal is one release's fixture closure and its two executables.
type proposal struct {
	closure     string
	executables map[token]string
}

// newProposal is a consistent closure for one release, with its executable files.
func newProposal(r harness.Reporter, base, version string, signer *Signer) proposal {
	r.Helper()
	x64 := elfBlob(r, filepath.Join(base, "exec", string(linuxX64)), amd64Machine,
		elfShape{Marker: []byte(version)})
	arm64 := elfBlob(r, filepath.Join(base, "exec", string(linuxARM64)), arm64Machine,
		elfShape{Size: 8192, Marker: []byte(version)})
	closure := buildClosure(r, closureRequest{
		Out:     filepath.Join(base, "closure"),
		Signer:  signer,
		Version: version,
		AMD64:   x64,
		ARM64:   arm64,
		Reason:  writerReason,
	})
	//nolint:exhaustive // the two maintained tokens; the manifest's musl and darwin platforms reuse these files.
	return proposal{closure: closure, executables: map[token]string{linuxX64: x64, linuxARM64: arm64}}
}

// writerRepo is a repository whose Claude closure, served upstream and Dockerfile this
// file owns.
//
// Everything the writer reads is here: the committed closure under
// `images/tools/claude`, the single Claude ARG of `images/standard/Dockerfile`, and a
// tree laid out the way `--fixture-upstream` serves it, so a case reaches the writer
// through flags alone -- the bytes it fetches, the signer it holds the pin to, and the
// tree it publishes into.
type writerRepo struct {
	base        string
	root        string
	upstream    string
	signer      Fingerprint
	version     string
	closure     string
	executables map[token]string
}

// repoConfig is the shape one writer repository is built in. A case that adopts,
// signs or commits the key is the default, so the zero option set is the honest one.
type repoConfig struct {
	adopted           bool
	version           string
	dockerfileVersion string
	signed            bool
	key               bool
}

// repoOption changes one thing about a fixture repository.
type repoOption func(*repoConfig)

// unadopted starts from an empty pin directory: a clean first adoption.
func unadopted() repoOption {
	return func(config *repoConfig) { config.adopted = false }
}

// dockerfileAt sets the Dockerfile's Claude ARG to a version other than the closure's.
func dockerfileAt(version string) repoOption {
	return func(config *repoConfig) { config.dockerfileVersion = version }
}

// unsigned drops the suite's `--signer`, so the fixture closure is an unreviewed one.
func unsigned() repoOption {
	return func(config *repoConfig) { config.signed = false }
}

// keyless removes the committed release key, which is committed by hand.
func keyless() repoOption {
	return func(config *repoConfig) { config.key = false }
}

func newWriterRepo(r harness.Reporter, base string, options ...repoOption) *writerRepo {
	r.Helper()
	config := repoConfig{adopted: true, version: Version, dockerfileVersion: Version,
		signed: true, key: true}
	for _, option := range options {
		option(&config)
	}
	signer := testFixture(r).signer
	repo := &writerRepo{
		base:     base,
		root:     filepath.Join(base, "repo"),
		upstream: filepath.Join(base, "upstream"),
		version:  config.version,
	}
	if config.signed {
		repo.signer = signer.Fingerprint
	}
	built := newProposal(r, base, config.version, signer)
	repo.closure, repo.executables = built.closure, built.executables
	directory := filepath.Join(repo.root, pinDirectory)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", directory, err)
	}
	harness.CopyFile(r, filepath.Join(repo.closure, "pin.json"), filepath.Join(directory, "pin.json"))
	harness.CopyTree(r, filepath.Join(repo.closure, "evidence"), filepath.Join(directory, "evidence"))
	harness.CopyFile(r, filepath.Join(repo.closure, keyName), filepath.Join(directory, keyName))
	repo.writeDockerfile(r, config.dockerfileVersion)
	if !config.key {
		remove(r, filepath.Join(directory, keyName))
	}
	if !config.adopted {
		remove(r, filepath.Join(directory, "pin.json"))
		remove(r, filepath.Join(directory, "evidence"))
	}
	repo.serve(r, served{closure: repo.closure, executables: repo.executables, version: repo.version})
	return repo
}

func remove(r harness.Reporter, path string) {
	r.Helper()
	if err := os.RemoveAll(path); err != nil {
		r.Fatalf("cannot remove %s: %v", path, err)
	}
}

// writeDockerfile is the committed Dockerfile with its one Claude ARG set to version.
//
// The fixture rewrites whatever exact Claude ARG the committed Dockerfile carries, so
// a reviewed bump does not leave this suite pinned to the release it last saw.
func (w *writerRepo) writeDockerfile(r harness.Reporter, version string) {
	r.Helper()
	raw := harness.ReadFile(r, filepath.Join(harness.Repository(r), dockerfile))
	lines := strings.Split(string(raw), "\n")
	matches := []int{}
	for index, line := range lines {
		if strings.HasPrefix(line, dockerfileArg) {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		r.Fatalf("the committed Dockerfile must carry exactly one %s line, found %d",
			dockerfileArg, len(matches))
	}
	lines[matches[0]] = dockerfileArg + version
	harness.WriteFile(r, filepath.Join(w.root, dockerfile), []byte(strings.Join(lines, "\n")))
}

// served is one release's served tree: the closure's evidence and key plus the two
// executables, which a case may replace.
type served struct {
	closure     string
	executables map[token]string
	version     string
}

// serve writes the `--fixture-upstream` tree: `https://host/path` from `host/path`.
func (w *writerRepo) serve(r harness.Reporter, tree served) {
	r.Helper()
	version := tree.version
	if version == "" {
		version = w.version
	}
	remove(r, w.upstream)
	base := upstreamBase + "/" + version
	files := map[string]string{
		base + "/" + manifestName:  filepath.Join(tree.closure, "evidence", manifestName),
		base + "/" + signatureName: filepath.Join(tree.closure, "evidence", signatureName),
		upstreamKeyURL:             filepath.Join(tree.closure, keyName),
	}
	for name, path := range tree.executables {
		files[base+"/"+string(name)+"/claude"] = path
	}
	for relative, source := range files {
		harness.CopyFile(r, source, filepath.Join(w.upstream, filepath.FromSlash(relative)))
	}
}

// serving is this repository's own release, with the executables a case replaced.
func (w *writerRepo) serving(closure string, executables map[token]string) served {
	return served{closure: closure, executables: executables, version: w.version}
}

// evidencePath is one committed evidence file, as the writer names it.
func (w *writerRepo) evidencePath(name string) string {
	return filepath.Join(w.root, evidenceDirectory, name)
}

func (w *writerRepo) dockerfile(r harness.Reporter) string {
	r.Helper()
	return string(harness.ReadFile(r, filepath.Join(w.root, dockerfile)))
}

func (w *writerRepo) pin(r harness.Reporter) map[string]any {
	r.Helper()
	return readPin(r, filepath.Join(w.root, pinDirectory))
}

// published is one file in the served tree.
func (w *writerRepo) published(name string) string { return filepath.Join(w.upstream, name) }

// snapshot is every file below the checkout, for `a refused run changed no byte`.
type snapshot struct {
	files       map[string][]byte
	directories []string
}

func (w *writerRepo) snapshot(r harness.Reporter) snapshot {
	r.Helper()
	found := snapshot{files: map[string][]byte{}}
	err := filepath.WalkDir(w.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == w.root {
			return nil
		}
		relative, err := filepath.Rel(w.root, path)
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
		r.Fatalf("cannot walk %s: %v", w.root, err)
	}
	sort.Strings(found.directories)
	return found
}

// difference describes how the tree differs from before, so a refusal that wrote
// something says what it wrote.
func (s snapshot) difference(before snapshot) string {
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
	return fmt.Sprintf("added %v, removed %v, changed %v, directories added %v",
		added, removed, changed, addedDirectories(before.directories, s.directories))
}

func addedDirectories(before, after []string) []string {
	known := map[string]bool{}
	for _, name := range before {
		known[name] = true
	}
	added := []string{}
	for _, name := range after {
		if !known[name] {
			added = append(added, name)
		}
	}
	return added
}

func sameSnapshot(left, right snapshot) bool {
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

// temporary is this case's TMPDIR, so the scratch directory the writer announces is
// inside the case rather than in the host's shared temporary root.
func (w *writerRepo) temporary(r harness.Reporter) string {
	r.Helper()
	directory := filepath.Join(w.base, "tmp")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", directory, err)
	}
	return directory
}

// --- running the writer -------------------------------------------------------

// writerEntry is the compiled writer, built once for this test process.
func writerEntry(r harness.Reporter) []string {
	r.Helper()
	return []string{harness.Compile(r, "claude", "./cmd/avm-pin"), "claude"}
}

// wrapperEntry is `script/dev/pin-claude.sh` itself: the compile, the `cd` to the
// checkout and the `exec avm-pin claude` CONTRIBUTING tells a maintainer to run.
func wrapperEntry(r harness.Reporter) []string {
	r.Helper()
	return []string{"bash", filepath.Join(harness.Repository(r), "script", "dev", "pin-claude.sh")}
}

// writerOption adjusts one writer invocation.
type writerOption func(*harness.Spec)

// flags appends the writer's own arguments.
func flags(arguments ...string) writerOption {
	return func(spec *harness.Spec) { spec.Argv = append(spec.Argv, arguments...) }
}

// withEnv adds one environment variable, which is how the CI refusal and the two clock
// zones are driven.
func withEnv(name, value string) writerOption {
	return func(spec *harness.Spec) { spec.Env[name] = value }
}

// maintainerSpec is the environment a maintainer host would have: no GITHUB_ACTIONS
// (the aggregate usually runs on a runner, and the CI refusal has its own case), a
// TMPDIR inside the case, and a working directory outside every fixture tree. One
// function, because the writer is reached two ways -- its binary and the wrapper that
// builds it -- and the two share this contract, not a copy of it.
func maintainerSpec(r harness.Reporter, repo *writerRepo, entry []string, options ...writerOption) harness.Spec {
	r.Helper()
	spec := harness.Spec{
		Argv:  append(append([]string{}, entry...), "--root", repo.root),
		Dir:   os.TempDir(),
		Env:   map[string]string{"TMPDIR": repo.temporary(r)},
		Unset: []string{"GITHUB_ACTIONS"},
	}
	for _, option := range options {
		option(&spec)
	}
	return spec
}

// servedFlags are the two seams every writer invocation carries: the served tree and,
// unless the case is about an unreviewed signer, the signer the pin is held to. An
// empty field is no flag at all rather than an empty one: a case built straight from a
// committed closure has no upstream to serve, and `--fixture-upstream ""` would make it
// assert the seam instead of the behaviour it is about.
func (w *writerRepo) servedFlags() []string {
	arguments := []string{}
	if w.upstream != "" {
		arguments = append(arguments, "--fixture-upstream", w.upstream)
	}
	if w.signer != "" {
		arguments = append(arguments, "--signer", string(w.signer))
	}
	return arguments
}

// run is one writer process through the binary the wrapper builds.
func (w *writerRepo) run(r harness.Reporter, options ...writerOption) harness.Result {
	r.Helper()
	return harness.Run(r, maintainerSpec(r, w, writerEntry(r),
		append([]writerOption{flags(w.servedFlags()...)}, options...)...))
}

// runWrapper is the same run through `script/dev/pin-claude.sh`.
func (w *writerRepo) runWrapper(r harness.Reporter, options ...writerOption) harness.Result {
	r.Helper()
	return harness.Run(r, maintainerSpec(r, w, wrapperEntry(r),
		append([]writerOption{flags(w.servedFlags()...)}, options...)...))
}

// requireRefusal is the whole shape of a refusal: its status, the needle that names
// the reason on **stderr** -- the writer's diagnostics are a separate stream from the
// notes it prints about what it did, and a refusal that arrived on stdout would be a
// change of contract -- and a checkout no byte of which moved.
func requireRefusal(r harness.Reporter, result harness.Result, needle string, repo *writerRepo, before snapshot) {
	r.Helper()
	result.RequireStatus(r, 1)
	result.RequireStderr(r, needle)
	if after := repo.snapshot(r); !sameSnapshot(after, before) {
		r.Fatalf("the refusal changed the sources: %s", after.difference(before))
	}
}

// publishedFiles is the destinations a run reported it published.
func publishedFiles(stdout string) []string {
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "pin-claude: published claude ") {
			continue
		}
		parts := strings.SplitN(line, ": ", 3)
		if len(parts) != 3 {
			return nil
		}
		found := strings.Split(parts[2], ", ")
		sort.Strings(found)
		return found
	}
	return nil
}

// --- the reader ---------------------------------------------------------------

// reader is the compiled reader: `script/build/recipe-contract.sh --native` prints the
// composed directory it built (its own build log goes to stderr), which is the same
// helper the image receives through a named build context.
var reader struct {
	sync.Mutex
	path string
}

func readerBinary(r harness.Reporter) string {
	r.Helper()
	reader.Lock()
	defer reader.Unlock()
	if reader.path != "" {
		return reader.path
	}
	root := harness.Repository(r)
	result := harness.Run(r, harness.Spec{
		Argv: []string{"bash", filepath.Join(root, "script", "build", "recipe-contract.sh"), "--native"},
		Dir:  root,
	})
	if result.Status != 0 {
		r.Fatalf("cannot build recipe-contract\n%s", result.Report())
	}
	lines := strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n")
	context := lines[len(lines)-1]
	path := filepath.Join(context, "out", "recipe-contract")
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		r.Fatalf("the composed context holds no reader: %s", path)
	}
	reader.path = path
	return path
}

// refusalPrefix is frozen: `script/test/vendored-installers.sh` matches it, so a
// refusal that lost the prefix would be a change of contract rather than a reword.
const refusalPrefix = "claude_pin: "

// runReader is one reader invocation.
func runReader(r harness.Reporter, argv []string, env map[string]string) harness.Result {
	r.Helper()
	return harness.Run(r, harness.Spec{
		Argv: append([]string{readerBinary(r), "claude-pin"}, argv...),
		Env:  env,
	})
}

// refusalText is the refusal's message, which exits 1 and names its own reason. It
// returns the failure instead of stopping the case, because the clock cases retry
// while the UTC date is moving.
func refusalText(result harness.Result, needles []string) (string, error) {
	if result.Status != 1 {
		return "", fmt.Errorf("expected a refusal, got %d\n%s", result.Status, result.Report())
	}
	text, found := strings.CutPrefix(result.Stderr, refusalPrefix)
	if !found {
		return "", fmt.Errorf("the refusal is not prefixed with %q: %q", refusalPrefix,
			result.Stderr)
	}
	text = strings.TrimSpace(text)
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return text, fmt.Errorf("the refusal did not mention %q: %s", needle, text)
		}
	}
	return text, nil
}

// invocation is one reader invocation's inputs.
type invocation struct {
	env map[string]string
}

// invokeOption adjusts one reader invocation.
type invokeOption func(*invocation)

// inZone runs the reader under one TZ.
func inZone(zone string) invokeOption {
	return inEnv("TZ", zone)
}

// inEnv adds one environment variable to the reader's environment.
func inEnv(name, value string) invokeOption {
	return func(invocation *invocation) {
		if invocation.env == nil {
			invocation.env = map[string]string{}
		}
		invocation.env[name] = value
	}
}

func invoked(options []invokeOption) *invocation {
	found := &invocation{}
	for _, option := range options {
		option(found)
	}
	return found
}

// refuses asserts a refusal and returns its message.
func refuses(r harness.Reporter, argv []string, needles []string, options ...invokeOption) string {
	r.Helper()
	result := runReader(r, argv, invoked(options).env)
	text, err := refusalText(result, needles)
	if err != nil {
		r.Fatalf("%v", err)
	}
	return text
}

// accepts asserts a successful invocation and returns its result.
func accepts(r harness.Reporter, argv []string, options ...invokeOption) harness.Result {
	r.Helper()
	result := runReader(r, argv, invoked(options).env)
	if result.Status != 0 {
		r.Fatalf("unexpected refusal (%d)\n%s", result.Status, result.Report())
	}
	return result
}

// pinInvocation is a verb over a pin directory, with any further arguments.
func pinInvocation(verb, directory string, extra ...string) []string {
	return append([]string{verb, "--pin-dir", directory}, extra...)
}

// loadRefusal is a closure refused by its own structure, before any evidence is read.
func loadRefusal(r harness.Reporter, directory string, needles ...string) string {
	r.Helper()
	return refuses(r, pinInvocation("version", directory), needles)
}

// checkAccepted is `check --version` on a closure.
func checkAccepted(r harness.Reporter, directory, version string) harness.Result {
	r.Helper()
	return accepts(r, pinInvocation("check", directory, "--version", version))
}

// checkRefused is `check` on a closure refused for the named reason.
func checkRefused(r harness.Reporter, directory string, needles ...string) string {
	r.Helper()
	return checkRefusedAt(r, directory, Version, nil, needles...)
}

// checkRefusedAt is the same check at another version and in another environment.
func checkRefusedAt(r harness.Reporter, directory, version string, env map[string]string, needles ...string) string {
	r.Helper()
	options := []invokeOption{}
	if env != nil {
		options = append(options, func(invocation *invocation) { invocation.env = env })
	}
	return refuses(r, pinInvocation("check", directory, "--version", version), needles, options...)
}

// --- the fixture CLI ----------------------------------------------------------

// Fixture names what `avm-test write-fixture --tool claude` writes and for which
// release. Two strings named, rather than two strings in a row.
type Fixture struct {
	Dir     string
	Version string
	// Signer is the directory holding the signer, or empty for DIR/signer, which is
	// generated when it holds no secret key yet.
	Signer string
}

// WriteFixture is `avm-test write-fixture --tool claude`: a committed-shape closure
// plus the upstream tree `script/test/agent-versions.sh` serves.
//
// The pin is rendered with the writer's own reason text and today's UTC date, so a
// writer run over this closure at the same version is byte-identical and prints
// `unchanged` -- which is what the bumper suite asserts. It returns the release it
// published, which is the default when the caller named none.
func WriteFixture(r harness.Reporter, fixture Fixture) string {
	r.Helper()
	if fixture.Version == "" {
		fixture.Version = Version
	}
	destination := fixture.Dir
	signerDirectory := fixture.Signer
	if signerDirectory == "" {
		signerDirectory = filepath.Join(destination, "signer")
	}
	signer := func() *Signer {
		if _, err := os.Stat(filepath.Join(signerDirectory, "secret.asc")); err == nil {
			return readSigner(r, signerDirectory, false)
		}
		return makeSigner(r, signerDirectory, false)
	}()
	built := newProposal(r, destination, fixture.Version, signer)
	// A caller reusing the directory gets the tree for this release, not a merge with
	// whatever the last one left behind.
	remove(r, filepath.Join(destination, "upstream"))
	for relative, source := range map[string]string{
		upstreamBase + "/" + fixture.Version + "/" + manifestName:  filepath.Join(built.closure, "evidence", manifestName),
		upstreamBase + "/" + fixture.Version + "/" + signatureName: filepath.Join(built.closure, "evidence", signatureName),
		upstreamKeyURL: filepath.Join(built.closure, keyName),
		upstreamBase + "/" + fixture.Version + "/" + string(linuxX64) + "/claude":   built.executables[linuxX64],
		upstreamBase + "/" + fixture.Version + "/" + string(linuxARM64) + "/claude": built.executables[linuxARM64],
	} {
		harness.CopyFile(r, source, filepath.Join(destination, "upstream", filepath.FromSlash(relative)))
	}
	return fixture.Version
}

// ClosureArguments is one `closure` invocation: the closure to write and the signer to
// read it from.
type ClosureArguments struct {
	Out         string
	Signer      string
	Version     string
	AMD64       string
	ARM64       string
	Key         string
	Fingerprint string
	Signature   string
	Subkey      bool
}

// Closure is the `closure` verb: one consistent closure written into Out.
func Closure(r harness.Reporter, arguments ClosureArguments) string {
	r.Helper()
	return buildClosure(r, closureRequest{
		Out:         arguments.Out,
		Signer:      readSigner(r, arguments.Signer, arguments.Subkey),
		Version:     arguments.Version,
		AMD64:       arguments.AMD64,
		ARM64:       arguments.ARM64,
		Key:         arguments.Key,
		Fingerprint: Fingerprint(arguments.Fingerprint),
		Signature:   arguments.Signature,
	})
}

// Keygen is the `keygen` verb: one throwaway key, returning its primary fingerprint.
func Keygen(r harness.Reporter, out string, subkey bool) Fingerprint {
	r.Helper()
	return makeSigner(r, out, subkey).Fingerprint
}

// ElfArguments is one `elf` invocation: the OUT path and the header it gets. A zero
// class, endian or size is the fixture default, and that default lives in elfBlob
// alone -- the CLI declares no `default:` of its own, so the two cannot disagree.
type ElfArguments struct {
	Out     string
	Machine int
	Class   int
	Endian  int
	Size    int
}

// Elf is the `elf` verb: one ELF-shaped blob with no marker.
func Elf(r harness.Reporter, arguments ElfArguments) string {
	r.Helper()
	return elfBlob(r, arguments.Out, machineNumber(arguments.Machine), elfShape{
		Class:  arguments.Class,
		Endian: arguments.Endian,
		Size:   arguments.Size,
	})
}

// TwoPrimaries is the `two-primaries` verb: one key file holding two primaries.
func TwoPrimaries(r harness.Reporter, out, first, second string) string {
	r.Helper()
	return twoPrimaries(r, out, first, second)
}
