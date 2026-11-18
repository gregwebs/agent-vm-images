// Package codex is the black-box suite for the committed Codex payload closure
// (#97, #203).
//
// Every case but the fixture seam drives a real program. The reader cases run
// `recipe-contract codex-pin check|select|preflight …` (`internal/codexpin` +
// `internal/cli`), the writer cases run the `avm-pin codex` binary that
// `script/dev/pin-codex.sh` compiles, and the closures they are pointed at are
// synthesized here: real tar/gzip archives, real package metadata, real ELF headers,
// real evidence digests. Nothing reaches the network, nothing runs Docker, no fixture
// binary is executed and no committed evidence is touched.
//
// Two deliberate narrowings, because a suite that claims more than it drives stops
// being read. The wrapper `pin-codex.sh` itself (its build, its `cd`, its forwarding) is
// driven by one entry-point case, `TestCommittedWriterCheckAcceptsTheTree`; the other
// writer cases use the binary the wrapper would run, and a write-only wrapper flag is
// not covered here. And the three fixture seam cases run their builder in-process
// through `harness.Command` -- the same adapter `avm-test` calls -- because the builder,
// not a program, is their subject.
//
// Three seams are used, and no others:
//
//   - a mutated or freshly built closure, for everything the reader decides from
//     committed bytes;
//   - `--fixture-upstream DIR`, which serves `https://host/path` from
//     `DIR/host/path`, so the writer's whole request set is answered from a
//     directory this file wrote (and, with one file removed, its refusals name the
//     exact URL it asked for);
//   - an executable fixture cosign per case, which reports
//     `{"gitVersion":"v2.4.1"}`, either accepts `verify-blob` or refuses it on
//     demand, and can record the argv and environment it was handed. The reviewed
//     version is a closed constant in the writer, so a fixture verifier is the only
//     way to reach the "cosign refused" refusal without the host tool, and the
//     child is the only observer of the environment production constructs for it.
//
// The pin date is UTC on both sides, so the clock case runs the real writer as a
// process under `TZ=Pacific/Kiritimati` and `TZ=Etc/GMT+12`: a host's local date only
// exists inside a real zone, and it is exactly a local-date comparison that the UTC
// pin date replaced.
//
// There is no monkeypatching and no import of a production package: the programs
// are binaries, so a case that cannot be expressed this way does not exist here.
// The cases that used to patch production internals live in Go instead, and each
// group has exactly one owner:
//
//   - `internal/codexpin/pin_test.go` — the pin schema and its evidence re-hashing,
//     the bundle shape, the ELF check, the archive member policy, the raw-block
//     bounds, PAX size overrides, the decoded-stream budget, gzip truncation and the
//     physical-header budget;
//   - `internal/codewriter/{render,stagedir,writer}_test.go` — the rendered pin's
//     byte shape, the staging directory rules, the CI refusal and the same-release
//     identity comparisons;
//   - `internal/publication/publish_test.go` — the publication fault cases: a
//     partial write, a signal, a refused restore and an unwritable recovery record;
//   - `script/test/image-sources/tests/image_sources.rs` — the independent
//     re-derivation of the pin schema and of every URL and digest mutation.
//
// `--real-signatures` is gone and does not come back: it drove a Python verifier
// against retained payloads and the network; the Go reader never verifies a
// signature (it checks committed bytes only), so the mode has no black-box
// equivalent and no owner.
//
// Two fixture modes keep the shell suites working, and both are `avm-test` verbs
// reached through `script/dev/avm-test.sh`:
// `write-fixture --tool codex --dir DIR [--version TAG]` writes a committed-shape
// closure (pin plus its four evidence files) that `script/test/codex-installer.sh`
// mounts as its fixture pin directory, and
// `write-installer-fixture --tool codex --dir DIR --release TAG --target TRIPLE
// --native-script FILE [--package-version V] [--package-target TRIPLE]` writes the
// fixture the two installer suites consume: `source/` (the closure they mount as the
// pin directory), `assets/` (the six upstream documents, by their canonical asset
// names), `archive.tgz` for the selected target, and `upstream/` laid out exactly as
// `--fixture-upstream` serves it. The entrypoint of that fixture package is the
// reviewed ELF header followed by the given script body: the header is what the
// production preflight reads, and the body is what a suite that runs the payload
// executes.
package codex

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

const (
	// Release is the exact tag every fixture closure pins unless a case is proving a
	// refusal.
	Release = "rust-v0.159.3"
	// Version is that tag's numeric version.
	Version = "0.159.3"
	// NextRelease is an exact but unpinned tag: the "reviewed bytes only" refusal.
	NextRelease = "rust-v0.159.4"

	// The reviewed signer policy, as the pin spells it.
	Workflow        = "https://github.com/openai/codex/.github/workflows/rust-release.yml"
	Issuer          = "https://token.actions.githubusercontent.com"
	VerifierVersion = "v2.4.1"

	// ChecksumsAsset is the original checksum manifest's upstream asset name.
	ChecksumsAsset = "codex-package_SHA256SUMS"

	// releasePrefix is the only tag spelling the reader accepts.
	releasePrefix = "rust-v"

	// The closure's directory and the Dockerfile slot, at the paths production uses.
	PinDirectory  = "images/tools/codex"
	Dockerfile    = "images/standard/Dockerfile"
	Argument      = "AGENT_VERSION_CODEX"
	dockerfileArg = "ARG AGENT_VERSION_CODEX="

	// The reader's and writer's frozen prefixes: the hooks and the shell suites
	// match them, so a refusal that lost one would be a change of contract.
	readerPrefix = "codex_pin: "
	writerPrefix = "pin-codex: "

	elfHeaderBytes = 20
)

// platform is one maintained architecture, keyed the pin's way.
type platform string

const (
	linuxAMD64 platform = "linux/amd64"
	linuxARM64 platform = "linux/arm64"
)

// platforms are the two maintained architectures, in the pin's order.
var platforms = []platform{linuxAMD64, linuxARM64}

// target is one upstream build triple.
type target string

const (
	targetAMD64 target = "x86_64-unknown-linux-musl"
	targetARM64 target = "aarch64-unknown-linux-musl"
)

// Targets is the two upstream triples an installer fixture may name, in platform
// order. Exported because the fixture CLI validates --target/--package-target
// against exactly this table.
var Targets = []string{string(targetAMD64), string(targetARM64)}

// machineOf is each triple's ELF e_machine, and targetOf each platform's triple. One
// table, because a triple that arrived without its machine would otherwise be a zero
// the archive policy reads as "no executable at all".
var maintained = map[target]struct {
	platform platform
	machine  int
}{
	targetAMD64: {platform: linuxAMD64, machine: 62},
	targetARM64: {platform: linuxARM64, machine: 183},
}

func targetOf(name platform) target {
	for triple, entry := range maintained {
		if entry.platform == name {
			return triple
		}
	}
	panic("no maintained target for platform " + string(name))
}

func machineOf(triple target) int {
	entry, known := maintained[triple]
	if !known {
		panic("no maintained machine for target " + string(triple))
	}
	return entry.machine
}

// KnownTarget reports whether an installer fixture may name this triple.
func KnownTarget(value string) bool {
	_, known := maintained[target(value)]
	return known
}

// releasePattern mirrors the reader's reviewed exact-release grammar
// (`internal/codexpin`'s releaseVersionPattern). It is mirrored because the fixture
// builder may not import a production package: a fixture release the reader would
// refuse cannot be written, so the mirror is asserted by every `check` case rather
// than trusted.
var releasePattern = regexp.MustCompile(`^rust-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(-alpha(\.(0|[1-9][0-9]*)){0,2}|-beta(\.(0|[1-9][0-9]*))?)?$`)

// --- scratch, documents and the clock -----------------------------------------

// scratch is a per-case directory under the real temporary root, with symlinks
// resolved: a program that prints an absolute path it retained must print the same
// string this file compares against.
func scratch(r harness.Reporter, label string) string {
	r.Helper()
	return harness.TempDir(r, "codex-suite."+fileName(label)+".")
}

// fileName turns a case label into one path segment.
func fileName(label string) string {
	replaced := strings.NewReplacer(" ", "-", "/", "-", ":", "-", "'", "").Replace(label)
	return strings.Trim(replaced, "-")
}

// document parses a fixture JSON file. Numbers keep the spelling they were written
// with, so a case that rewrites one field does not renumber another.
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

// object is a nested document, named by the keys that reached it, so a lookup that
// names `platforms`/`linux/amd64`/`sha256` cannot silently miss.
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

// encodeDocument renders a fixture document the way the writer does: no HTML
// escaping, two-space indentation and one trailing newline.
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

// fixtureVerifiedOn is the date a synthesized closure says it was verified on. It is
// in the past on any host this suite runs on, which is the point: see the comment on
// the pin's authentication document.
const fixtureVerifiedOn = "2026-01-02"

// utcToday is the UTC date, which is the only date a pin has.
func utcToday() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// --- canonical URLs and names -------------------------------------------------

// apiURL is the exact-tag release document's canonical URL.
func apiURL(release string) string {
	return "https://api.github.com/repos/openai/codex/releases/tags/" + release
}

// assetURL is one release asset's canonical URL.
func assetURL(release, asset string) string {
	return "https://github.com/openai/codex/releases/download/" + release + "/" + asset
}

func assetName(triple target) string { return "codex-package-" + string(triple) + ".tar.gz" }

func bundleName(triple target) string { return assetName(triple) + ".sigstore" }

// identity is the exact workflow identity the pin names for one release.
func identity(release string) string { return Workflow + "@refs/tags/" + release }

// requireFixtureRelease is the builder's own copy of the exact-tag grammar.
func requireFixtureRelease(r harness.Reporter, release string) string {
	r.Helper()
	if err := ReleaseSupported(release); err != nil {
		r.Fatalf("%v", err)
	}
	return release
}

// ReleaseSupported reports why a fixture cannot pin release, and nil when it can.
//
// It is exported because the command that writes fixtures has to answer with a
// malformed invocation (2) rather than a fixture that could not be built (1): a caller
// that branches on the status must read a mistyped tag as a mistyped tag. The reader's
// own grammar lives in internal/codexpin; this mirrors it, and every `check` case
// asserts the mirror, so a fixture the reader would refuse cannot be written.
func ReleaseSupported(release string) error {
	if release == "" {
		return errors.New("a fixture release must be a non-empty string")
	}
	if !releasePattern.MatchString(release) {
		return fmt.Errorf("the fixture release is not an exact Codex release: %q", release)
	}
	return nil
}

// --- ELF, signatures and the archive's bytes ----------------------------------

// elfHeader is the 64-byte ELF header every fixture entrypoint starts with: ELF64
// little-endian, one machine. Only its first 20 bytes are ever read.
func elfHeader(machine int) []byte {
	header := make([]byte, 64)
	copy(header, "\x7fELF")
	header[4] = 2 // ELFCLASS64
	header[5] = 1 // ELFDATA2LSB
	header[6] = 1 // EV_CURRENT
	header[16] = 3
	header[18] = byte(machine)
	header[19] = byte(machine >> 8)
	return header
}

func certificate() []byte {
	return []byte("-----BEGIN CERTIFICATE-----\nMIIBfixtureCertificate\n-----END CERTIFICATE-----\n")
}

// hashedRekord is the legacy cosign bundle's entry, which the reader checks for
// shape: kind, a sha256 payload hash and the signature the certificate made.
type hashedRekord struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Spec       hashedRekordSpec `json:"spec"`
}

type hashedRekordSpec struct {
	Data      hashedRekordData      `json:"data"`
	Signature hashedRekordSignature `json:"signature"`
}

type hashedRekordData struct {
	Hash hashDocument `json:"hash"`
}

type hashDocument struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

type hashedRekordSignature struct {
	Content   string            `json:"content"`
	PublicKey publicKeyDocument `json:"publicKey"`
}

type publicKeyDocument struct {
	Content string `json:"content"`
}

// sigstoreBundle is a structurally valid legacy bundle for one payload digest.
type sigstoreBundle struct {
	Base64Signature string      `json:"base64Signature"`
	Cert            string      `json:"cert"`
	RekorBundle     rekorBundle `json:"rekorBundle"`
}

type rekorBundle struct {
	SignedEntryTimestamp string       `json:"SignedEntryTimestamp"`
	Payload              rekorPayload `json:"Payload"`
}

type rekorPayload struct {
	Body           string `json:"body"`
	IntegratedTime int    `json:"integratedTime"`
	LogID          string `json:"logID"`
	LogIndex       int    `json:"logIndex"`
}

// makeBundle renders one legacy bundle for one payload digest. It is not a
// signature: the reader never verifies one, it only proves the committed bytes are a
// bundle for *this* digest.
func makeBundle(r harness.Reporter, payloadDigest string) []byte {
	cert := certificate()
	signature := []byte("fixture-signature")
	body := hashedRekord{
		APIVersion: "0.0.1",
		Kind:       "hashedrekord",
		Spec: hashedRekordSpec{
			Data: hashedRekordData{Hash: hashDocument{Algorithm: "sha256", Value: payloadDigest}},
			Signature: hashedRekordSignature{
				Content:   base64.StdEncoding.EncodeToString(signature),
				PublicKey: publicKeyDocument{Content: base64.StdEncoding.EncodeToString(cert)},
			},
		},
	}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		r.Fatalf("cannot encode the fixture bundle body: %v", err)
	}
	return encodeDocument(r, sigstoreBundle{
		Base64Signature: base64.StdEncoding.EncodeToString(signature),
		Cert:            base64.StdEncoding.EncodeToString(cert),
		RekorBundle: rekorBundle{
			SignedEntryTimestamp: base64.StdEncoding.EncodeToString([]byte("fixture-set")),
			Payload: rekorPayload{
				Body:           base64.StdEncoding.EncodeToString(encodedBody),
				IntegratedTime: 1,
				LogID:          strings.Repeat("a", 64),
				LogIndex:       1,
			},
		},
	})
}

// releaseMetadata is the exact-tag API document's security projection: the tag and
// the per-asset digests.
type releaseMetadata struct {
	TagName string          `json:"tag_name"`
	Name    string          `json:"name"`
	Assets  []metadataAsset `json:"assets"`
}

type metadataAsset struct {
	Name          string `json:"name"`
	Digest        string `json:"digest"`
	DownloadCount int    `json:"download_count"`
}

// makeMetadata renders the release document for one set of digests. The order is the
// canonical one (checksums, then each platform's archive and bundle), so a fixture is
// byte-identical whenever its bytes are.
func makeMetadata(r harness.Reporter, release string, digests map[string]string) []byte {
	assets := make([]metadataAsset, 0, len(digests))
	for _, name := range digestOrder(digests) {
		assets = append(assets, metadataAsset{
			Name:          name,
			Digest:        "sha256:" + digests[name],
			DownloadCount: 1,
		})
	}
	return encodeDocument(r, releaseMetadata{TagName: release, Name: release, Assets: assets})
}

// digestOrder is the canonical asset order: the checksum manifest first, then each
// platform's archive and bundle in platform order.
func digestOrder(digests map[string]string) []string {
	order := make([]string, 0, len(digests))
	appendIfPresent := func(name string) {
		if _, present := digests[name]; present {
			order = append(order, name)
		}
	}
	appendIfPresent(ChecksumsAsset)
	for _, name := range platforms {
		triple := targetOf(name)
		appendIfPresent(assetName(triple))
		appendIfPresent(bundleName(triple))
	}
	if len(order) != len(digests) {
		// A digest this order does not name would otherwise vanish from the document
		// the reader checks, which would be a fixture that silently drops evidence.
		remaining := make([]string, 0, len(digests))
		for name := range digests {
			if !contains(order, name) {
				remaining = append(remaining, name)
			}
		}
		sort.Strings(remaining)
		order = append(order, remaining...)
	}
	return order
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// makeChecksums renders a `sha256sum`-style manifest. The decoy row is upstream's:
// only the maintained payload lines carry install authority, and a fixture that had
// no other row would not exercise that.
func makeChecksums(digests map[string]string, extra map[string]string) []byte {
	lines := make([]string, 0, len(digests)+len(extra))
	for _, name := range digestOrder(digests) {
		lines = append(lines, digests[name]+"  "+name)
	}
	extraNames := make([]string, 0, len(extra))
	for name := range extra {
		extraNames = append(extraNames, name)
	}
	sort.Strings(extraNames)
	for _, name := range extraNames {
		lines = append(lines, extra[name]+"  "+name)
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

// --- the pin's documents ------------------------------------------------------

// The field order is the committed file's, so a fixture pin and a pin the writer
// renders from the same facts are the same bytes. That is what makes a byte-identical
// re-stage print `unchanged`.
type pinDocument struct {
	Schema         int                    `json:"schema"`
	Name           string                 `json:"name"`
	Kind           string                 `json:"kind"`
	Release        string                 `json:"release"`
	Version        string                 `json:"version"`
	Package        packageDocument        `json:"package"`
	Metadata       evidenceDocument       `json:"metadata"`
	Checksums      evidenceDocument       `json:"checksums"`
	Platforms      platformsDocument      `json:"platforms"`
	Authentication authenticationDocument `json:"authentication"`
}

type packageDocument struct {
	LayoutVersion int    `json:"layout_version"`
	Variant       string `json:"variant"`
	Entrypoint    string `json:"entrypoint"`
}

type evidenceDocument struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type platformDocument struct {
	Target string           `json:"target"`
	Asset  string           `json:"asset"`
	URL    string           `json:"url"`
	SHA256 string           `json:"sha256"`
	Bundle evidenceDocument `json:"bundle"`
}

type platformsDocument struct {
	AMD64 platformDocument `json:"linux/amd64"`
	ARM64 platformDocument `json:"linux/arm64"`
}

type verifierDocument struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type authenticationDocument struct {
	Method                string           `json:"method"`
	Verifier              verifierDocument `json:"verifier"`
	TrustRoot             string           `json:"trust_root"`
	CertificateIdentity   string           `json:"certificate_identity"`
	CertificateOIDCIssuer string           `json:"certificate_oidc_issuer"`
	VerifiedOn            string           `json:"verified_on"`
}

// packageMetadata is the archive's own record of what it is. The reader checks it
// against the pin and the selected target before extraction.
type packageMetadata struct {
	LayoutVersion int    `json:"layoutVersion"`
	Version       string `json:"version"`
	Target        string `json:"target"`
	Variant       string `json:"variant"`
	Entrypoint    string `json:"entrypoint"`
	ResourcesDir  string `json:"resourcesDir"`
	PathDir       string `json:"pathDir"`
}

// packageMetadataDocument is the reviewed package layout for one version and target.
func packageMetadataDocument(version string, triple target) packageMetadata {
	return packageMetadata{
		LayoutVersion: 1,
		Version:       version,
		Target:        string(triple),
		Variant:       "codex",
		Entrypoint:    "bin/codex",
		ResourcesDir:  "codex-resources",
		PathDir:       "codex-path",
	}
}

// --- the archive's members ----------------------------------------------------

// defaultMembers is the reviewed package layout: four explicit top-level names, real
// package metadata and four executables. The entitlement bits matter: a member the
// preflight reviews for 0755 is not executable at 0644.
func defaultMembers(r harness.Reporter, version string, name platform, machine int) []harness.Member {
	payload := append(elfHeader(machine), make([]byte, 64)...)
	return []harness.Member{
		{Name: "bin", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "bin/codex", Body: payload, Mode: 0o755},
		{Name: "bin/codex-code-mode-host", Body: []byte("host\n"), Mode: 0o755},
		{Name: "codex-package.json", Body: encodeDocument(r, packageMetadataDocument(version, targetOf(name))), Mode: 0o644},
		{Name: "codex-path", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "codex-path/rg", Body: []byte("rg\n"), Mode: 0o755},
		{Name: "codex-resources", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "codex-resources/bwrap", Body: []byte("bwrap\n"), Mode: 0o755},
	}
}

// archiveRequest is one fixture archive's members and the platform it is built for.
type archiveRequest struct {
	version  string
	platform platform
	machine  int
	members  []harness.Member
}

// buildArchive renders one package archive, byte-deterministically: the harness gzip
// header carries no timestamp, so a repeat of the same members is the same bytes.
func buildArchive(r harness.Reporter, request archiveRequest) []byte {
	members := request.members
	if members == nil {
		machine := request.machine
		if machine == 0 {
			machine = machineOf(targetOf(request.platform))
		}
		members = defaultMembers(r, request.version, request.platform, machine)
	}
	return harness.TarGz(r, members)
}

// --- the fixture closure ------------------------------------------------------

// fixture is one synthetic closure: the pin plus its four evidence files.
//
// Every digest in the pin and in the release metadata is derived from the bytes this
// object holds, so the closure is internally consistent by construction and a case
// that changes one payload gets a closure that commits the changed payload -- the
// digest is then never the reason a case refuses.
type fixture struct {
	dir       string
	release   string
	version   string
	archives  map[platform][]byte
	bundles   map[platform][]byte
	metadata  []byte
	checksums []byte
	pin       []byte
}

// newFixture builds and writes the closure at dir. A nil archives map is the default
// package layout for this release.
func newFixture(r harness.Reporter, dir, release string, archives map[platform][]byte) *fixture {
	r.Helper()
	built := &fixture{dir: dir, release: requireFixtureRelease(r, release)}
	built.version = strings.TrimPrefix(built.release, releasePrefix)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", dir, err)
	}
	built.archives = archives
	if built.archives == nil {
		built.archives = map[platform][]byte{}
		for _, name := range platforms {
			built.archives[name] = buildArchive(r, archiveRequest{version: built.version, platform: name})
		}
	}
	built.bundles = map[platform][]byte{}
	for _, name := range platforms {
		built.bundles[name] = makeBundle(r, harness.SHA256Hex(built.archives[name]))
	}
	// The checksum manifest is over the payloads alone -- the two packages and one
	// decoy row, exactly as the upstream file is -- and the release document names it:
	// computing the manifest first is what keeps the two from depending on each other.
	// The bundles are declared by the release document, not by this file, and the
	// reader only grants authority to the payload rows in any case.
	built.checksums = makeChecksums(built.payloadDigests(), map[string]string{"decoy": strings.Repeat("1", 64)})
	built.metadata = makeMetadata(r, built.release, built.releaseDigests())
	built.pin = encodeDocument(r, built.pinDocument())
	built.write(r)
	return built
}

// payloadDigests is the digest of each package archive: those two rows are what the
// upstream checksum manifest carries (beside rows the review keeps and the installer
// never trusts), so a fixture manifest that also corroborated the bundles would be a
// shape no upstream document has.
func (f *fixture) payloadDigests() map[string]string {
	digests := map[string]string{}
	for _, name := range platforms {
		digests[assetName(targetOf(name))] = harness.SHA256Hex(f.archives[name])
	}
	return digests
}

// releaseDigests is what the exact-tag release document must declare for these bytes.
func (f *fixture) releaseDigests() map[string]string {
	digests := map[string]string{ChecksumsAsset: harness.SHA256Hex(f.checksums)}
	for _, name := range platforms {
		triple := targetOf(name)
		digests[assetName(triple)] = harness.SHA256Hex(f.archives[name])
		digests[bundleName(triple)] = harness.SHA256Hex(f.bundles[name])
	}
	return digests
}

// pinDocument is the closure's pin, in the committed field order. Every URL is
// computed from the release, as the reader recomputes it.
func (f *fixture) pinDocument() pinDocument {
	pinned := func(name platform) platformDocument {
		triple := targetOf(name)
		asset := assetName(triple)
		return platformDocument{
			Target: string(triple),
			Asset:  asset,
			URL:    assetURL(f.release, asset),
			SHA256: harness.SHA256Hex(f.archives[name]),
			Bundle: evidenceDocument{
				URL:    assetURL(f.release, bundleName(triple)),
				Path:   "evidence/" + bundleName(triple),
				SHA256: harness.SHA256Hex(f.bundles[name]),
			},
		}
	}
	return pinDocument{
		Schema:  1,
		Name:    "codex",
		Kind:    "codex-standalone-package",
		Release: f.release,
		Version: f.version,
		Package: packageDocument{LayoutVersion: 1, Variant: "codex", Entrypoint: "bin/codex"},
		Metadata: evidenceDocument{
			URL:    apiURL(f.release),
			Path:   "evidence/release.json",
			SHA256: harness.SHA256Hex(f.metadata),
		},
		Checksums: evidenceDocument{
			URL:    assetURL(f.release, ChecksumsAsset),
			Path:   "evidence/" + ChecksumsAsset,
			SHA256: harness.SHA256Hex(f.checksums),
		},
		Platforms: platformsDocument{AMD64: pinned(linuxAMD64), ARM64: pinned(linuxARM64)},
		Authentication: authenticationDocument{
			Method:                "sigstore-blob-pin-time",
			Verifier:              verifierDocument{Name: "cosign", Version: VerifierVersion},
			TrustRoot:             "sigstore-production-tuf",
			CertificateIdentity:   identity(f.release),
			CertificateOIDCIssuer: Issuer,
			// A fixed date, deliberately not today: the writer retains the date a
			// closure was verified on, so committing today's date would make a
			// re-stage byte-identical even for a writer that restamps the proposal.
			// Commit a date that has already passed and a restamping writer is caught
			// by every repeat case; the writer's own date is the clock case's subject.
			VerifiedOn: fixtureVerifiedOn,
		},
	}
}

// write writes the closure into its directory, in the exact shape the writer renders.
func (f *fixture) write(r harness.Reporter) {
	r.Helper()
	harness.WriteFile(r, filepath.Join(f.dir, "pin.json"), f.pin)
	harness.WriteFile(r, filepath.Join(f.dir, "evidence", "release.json"), f.metadata)
	harness.WriteFile(r, filepath.Join(f.dir, "evidence", ChecksumsAsset), f.checksums)
	for _, name := range platforms {
		harness.WriteFile(r, filepath.Join(f.dir, "evidence", bundleName(targetOf(name))), f.bundles[name])
	}
}

// served is every canonical URL this closure's release answers, and its bytes.
func (f *fixture) served() map[string][]byte {
	served := map[string][]byte{
		apiURL(f.release):                   f.metadata,
		assetURL(f.release, ChecksumsAsset): f.checksums,
	}
	for _, name := range platforms {
		triple := targetOf(name)
		served[assetURL(f.release, assetName(triple))] = f.archives[name]
		served[assetURL(f.release, bundleName(triple))] = f.bundles[name]
	}
	return served
}

// writeUpstream lays a served map out the way `--fixture-upstream` serves it:
// `https://host/path` from `DIR/host/path`.
func writeUpstream(r harness.Reporter, dir string, served map[string][]byte) string {
	r.Helper()
	for url, body := range served {
		harness.WriteFile(r, upstreamPath(r, dir, url), body)
	}
	return dir
}

// upstreamPath is where `--fixture-upstream DIR` serves url from.
func upstreamPath(r harness.Reporter, dir, url string) string {
	r.Helper()
	scheme, rest, found := strings.Cut(url, "://")
	if !found || scheme == "" || rest == "" {
		r.Fatalf("not an absolute URL: %q", url)
	}
	if strings.Contains(rest, "..") {
		r.Fatalf("URL traverses: %q", url)
	}
	return filepath.Join(dir, filepath.FromSlash(rest))
}

// archiveAt writes one platform's archive under its canonical asset name, which is
// what a preflight is pointed at.
func (f *fixture) archiveAt(r harness.Reporter, dir string, name platform) string {
	r.Helper()
	path := filepath.Join(dir, assetName(targetOf(name)))
	harness.WriteFile(r, path, f.archives[name])
	return path
}

// --- the writer's repository --------------------------------------------------

// dockerfileTemplate is the fixture Dockerfile: the Codex slot and label the writer
// maintains, beside the single-slot defaults this owner does not own.
var dockerfileTemplate = "FROM scratch\n" +
	dockerfileArg + "%s\n" +
	`LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"` + "\n" +
	"ARG AGENT_VERSION_OPENCODE=v2.0.29\n" +
	"ARG AGENT_VERSION_CLAUDE=2.1.286\n" +
	"ARG AGENT_VERSION_COPILOT=1.0.90\n"

// repository is the tree a Codex transaction reads and writes: the pin directory and
// the Dockerfile.
//
// Those are the only two things a Codex transaction reads from the tree, so the
// fixture repository carries exactly them and a case can never pass by accident on a
// file the real repository happens to have.
type repository struct {
	// work is the case's own directory: the checkout is work/repo, the scratch the
	// writer announces is work/tmp, and a staged proposal is work/stage.
	work string
	root string
}

// newRepository writes the fixture checkout with the default Dockerfile at release.
func newRepository(r harness.Reporter, work, release string) *repository {
	r.Helper()
	return newRepositoryWithDockerfile(r, work, fmt.Sprintf(dockerfileTemplate, release))
}

// newRepositoryWithDockerfile is the same checkout with a Dockerfile a case needs to
// be wrong in one specific way. It carries only an empty pin directory and that
// Dockerfile: the two things a Codex transaction reads, and nothing a real checkout
// happens to hold.
func newRepositoryWithDockerfile(r harness.Reporter, work, text string) *repository {
	r.Helper()
	repo := &repository{work: work, root: filepath.Join(work, "repo")}
	for _, directory := range []string{
		filepath.Join(repo.root, PinDirectory),
		filepath.Join(repo.root, filepath.Dir(Dockerfile)),
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			r.Fatalf("cannot create %s: %v", directory, err)
		}
	}
	harness.WriteFile(r, filepath.Join(repo.root, Dockerfile), []byte(text))
	return repo
}

// install copies a closure into the checkout, so the committed state a case starts
// from is the fixture's own bytes.
func (repo *repository) install(r harness.Reporter, built *fixture) *repository {
	r.Helper()
	for _, relative := range []string{"pin.json", "evidence/release.json",
		"evidence/" + ChecksumsAsset, "evidence/" + bundleName(targetAMD64),
		"evidence/" + bundleName(targetARM64)} {
		harness.CopyFile(r, filepath.Join(built.dir, filepath.FromSlash(relative)),
			filepath.Join(repo.root, PinDirectory, filepath.FromSlash(relative)))
	}
	return repo
}

// committed is the six proposal destinations as the repository holds them.
func (repo *repository) committed(r harness.Reporter) map[string][]byte {
	r.Helper()
	names := []string{
		PinDirectory + "/pin.json",
		PinDirectory + "/evidence/release.json",
		PinDirectory + "/evidence/" + ChecksumsAsset,
		PinDirectory + "/evidence/" + bundleName(targetAMD64),
		PinDirectory + "/evidence/" + bundleName(targetARM64),
		Dockerfile,
	}
	found := make(map[string][]byte, len(names))
	for _, name := range names {
		found[name] = harness.ReadFile(r, filepath.Join(repo.root, filepath.FromSlash(name)))
	}
	return found
}

// snapshot is every file below the checkout, relative and slash-separated, for "a
// refused run changed no byte".
func (repo *repository) snapshot(r harness.Reporter) map[string][]byte {
	r.Helper()
	found := map[string][]byte{}
	err := filepath.WalkDir(repo.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == repo.root || entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(repo.root, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found[filepath.ToSlash(relative)] = body
		return nil
	})
	if err != nil {
		r.Fatalf("cannot walk %s: %v", repo.root, err)
	}
	return found
}

// difference describes how the tree differs from before, so a refusal that wrote
// something says what it wrote.
func difference(before, after map[string][]byte) string {
	var added, removed, changed []string
	for name, body := range after {
		old, existed := before[name]
		switch {
		case !existed:
			added = append(added, name)
		case !bytes.Equal(old, body):
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, found := after[name]; !found {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return fmt.Sprintf("added %v, removed %v, changed %v", added, removed, changed)
}

func sameSnapshot(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for name, body := range left {
		if !bytes.Equal(body, right[name]) {
			return false
		}
	}
	return true
}

// pinDirectory is this checkout's committed closure.
func (repo *repository) pinDirectory() string { return filepath.Join(repo.root, PinDirectory) }

// temporary is this case's TMPDIR, so the scratch directory the writer announces is
// inside the case rather than in the host's shared temporary root.
func (repo *repository) temporary(r harness.Reporter) string {
	r.Helper()
	dir := filepath.Join(repo.work, "tmp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", dir, err)
	}
	return dir
}

// stagePath is where this case stages its proposal. It is absent: the writer creates
// it 0700 itself.
func (repo *repository) stagePath() string { return filepath.Join(repo.work, "stage") }

// --- running the writer -------------------------------------------------------

// writerEntry is the compiled writer, built once for this test process, with the
// subcommand the wrapper execs.
func writerEntry(r harness.Reporter) []string {
	r.Helper()
	return []string{harness.Compile(r, "codex", "./cmd/avm-pin"), "codex"}
}

// wrapperEntry is `script/dev/pin-codex.sh` itself: the compile, the `cd` to the
// checkout and the `exec avm-pin codex` CONTRIBUTING tells a maintainer to run.
func wrapperEntry(r harness.Reporter) []string {
	r.Helper()
	return []string{"bash", filepath.Join(harness.Repository(r), "script", "dev", "pin-codex.sh")}
}

// writerOption adjusts one writer invocation.
type writerOption func(*harness.Spec)

// arguments passes the writer's own flags.
func arguments(values ...string) writerOption {
	return func(spec *harness.Spec) { spec.Argv = append(spec.Argv, values...) }
}

// withEnv adds one environment variable, which is how the CI refusal and the two
// clock zones are driven.
func withEnv(name, value string) writerOption {
	return func(spec *harness.Spec) { spec.Env[name] = value }
}

// maintainerSpec is the environment a maintainer host would have: no
// GITHUB_ACTIONS (the aggregate usually runs on a runner, and the CI refusal has its
// own case), a TMPDIR inside the case, and a working directory outside every fixture
// tree. One function, because the writer is reached two ways -- its binary and the
// wrapper that builds it -- and the two share this contract, not a copy of it.
func maintainerSpec(r harness.Reporter, repo *repository, entry []string, options ...writerOption) harness.Spec {
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

// runWriter is one writer process, through the binary the wrapper builds.
func runWriter(r harness.Reporter, repo *repository, options ...writerOption) harness.Result {
	r.Helper()
	return harness.Run(r, maintainerSpec(r, repo, writerEntry(r), options...))
}

// runWriterWrapper is the same run through `script/dev/pin-codex.sh`.
func runWriterWrapper(r harness.Reporter, repo *repository, options ...writerOption) harness.Result {
	r.Helper()
	return harness.Run(r, maintainerSpec(r, repo, wrapperEntry(r), options...))
}

// requireStatus asserts the status and renders the run when it is not the one the
// contract names.
func requireStatus(r harness.Reporter, result harness.Result, want int) {
	r.Helper()
	if result.Status != want {
		r.Fatalf("status %d, want %d\n%s", result.Status, want, result.Report())
	}
}

// requireRefusal is the whole shape of a writer refusal: its status, its own prefix,
// the needle that names the reason, and a checkout no byte of which moved.
func requireRefusal(r harness.Reporter, result harness.Result, needle string, repo *repository, before map[string][]byte) {
	r.Helper()
	requireStatus(r, result, 1)
	text, found := strings.CutPrefix(result.Stderr, writerPrefix)
	if !found {
		r.Fatalf("the refusal is not prefixed with %q: %q\n%s", writerPrefix, result.Stderr,
			result.Report())
	}
	if !strings.Contains(text, needle) {
		r.Fatalf("the refusal does not mention %q: %s\n%s", needle, strings.TrimSpace(text),
			result.Report())
	}
	if after := repo.snapshot(r); !sameSnapshot(after, before) {
		r.Fatalf("the refusal changed the sources: %s", difference(before, after))
	}
}

// --- the signature seam -------------------------------------------------------

// cosignRequest is one fixture verifier: the version it reports, the status
// `verify-blob` exits with, and where it records each run.
type cosignRequest struct {
	version string
	verify  int
	record  string
}

// writeCosign writes an executable that is the reviewed verifier, or refuses.
//
// It is a fixture program, never a production flag: `--probe-cosign` reports whatever
// the file it is given reports, and `verify-blob` is how the writer learns whether the
// committed bundle verifies. `verify` is the status it exits with instead of 0, which
// is the whole "cosign refused" case.
func writeCosign(r harness.Reporter, dir string, request cosignRequest) string {
	r.Helper()
	version := request.version
	if version == "" {
		version = VerifierVersion
	}
	script := "#!/bin/sh\n" +
		"if [ \"${1:-}\" = version ]; then\n" +
		"  printf '{\"gitVersion\":\"" + version + "\"}\\n'\n" +
		"  exit 0\n" +
		"fi\n"
	if request.record != "" {
		script += "{\n" +
			"  printf 'argv'\n" +
			"  for argument in \"$@\"; do printf ' %s' \"$argument\"; done\n" +
			"  printf '\\n'\n" +
			"  printf 'env'\n" +
			"  env | sed 's/=.*//' | sort | while read -r name; do\n" +
			"    printf ' %s' \"$name\"\n" +
			"  done\n" +
			"  printf '\\n'\n" +
			"} >> " + shellQuote(request.record) + "\n"
	}
	script += fmt.Sprintf("exit %d\n", request.verify)
	path := filepath.Join(dir, "cosign")
	harness.WriteFile(r, path, []byte(script))
	if err := os.Chmod(path, 0o755); err != nil {
		r.Fatalf("cannot make %s executable: %v", path, err)
	}
	return path
}

// shellQuote is one POSIX single-quoted word, so a record path with a space (a host
// TMPDIR) is still one argument.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// cosignRun is one recorded verifier run: the argv it was handed and the names in its
// environment.
type cosignRun struct {
	argv []string
	env  []string
}

// readCosignRecord is the recorded argv and environment of every verifier run, in
// order.
func readCosignRecord(r harness.Reporter, path string) []cosignRun {
	r.Helper()
	lines := []string{}
	for _, line := range strings.Split(string(harness.ReadFile(r, path)), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	runs := make([]cosignRun, 0, len(lines)/2)
	for len(lines) > 0 {
		argv, found := strings.CutPrefix(lines[0], "argv")
		if !found {
			r.Fatalf("the verifier record is not argv: %q", lines[0])
		}
		if len(lines) < 2 {
			r.Fatalf("the verifier record has an argv line and no env line")
		}
		env, found := strings.CutPrefix(lines[1], "env")
		if !found {
			r.Fatalf("the verifier record is not env: %q", lines[1])
		}
		lines = lines[2:]
		runs = append(runs, cosignRun{argv: strings.Fields(argv), env: strings.Fields(env)})
	}
	return runs
}

// shellLocals are the environment entries a POSIX shell adds for itself, which say
// nothing about what the parent passed on.
var shellLocals = []string{"PWD", "OLDPWD", "SHLVL", "_"}

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

// runReader is one reader invocation.
func runReader(r harness.Reporter, argv []string, env map[string]string) harness.Result {
	r.Helper()
	return harness.Run(r, harness.Spec{
		Argv: append([]string{readerBinary(r), "codex-pin"}, argv...),
		Env:  env,
	})
}

// refusalText is the refusal's message, which exits 1 and names its own reason.
func refusalText(result harness.Result, needles []string) (string, error) {
	if result.Status != 1 {
		return "", fmt.Errorf("expected a refusal, got %d\n%s", result.Status, result.Report())
	}
	text, found := strings.CutPrefix(result.Stderr, readerPrefix)
	if !found {
		return "", fmt.Errorf("the refusal is not prefixed with %q: %q", readerPrefix, result.Stderr)
	}
	text = strings.TrimSpace(text)
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			return text, fmt.Errorf("the refusal did not mention %q: %s", needle, text)
		}
	}
	return text, nil
}

// refuses asserts a refusal and returns its message.
func refuses(r harness.Reporter, argv []string, needles ...string) string {
	r.Helper()
	text, err := refusalText(runReader(r, argv, nil), needles)
	if err != nil {
		r.Fatalf("%v", err)
	}
	return text
}

// accepts asserts a successful invocation and returns its result.
func accepts(r harness.Reporter, argv []string) harness.Result {
	r.Helper()
	result := runReader(r, argv, nil)
	if result.Status != 0 {
		r.Fatalf("unexpected refusal (%d)\n%s", result.Status, result.Report())
	}
	return result
}

// usageRefused is a malformed invocation: status 2, the same prefix, and its own
// reason.
func usageRefused(r harness.Reporter, argv []string, needle string) {
	r.Helper()
	result := runReader(r, argv, nil)
	requireStatus(r, result, 2)
	text, found := strings.CutPrefix(result.Stderr, readerPrefix)
	if !found {
		r.Fatalf("the usage error is not prefixed with %q: %q\n%s", readerPrefix, result.Stderr,
			result.Report())
	}
	if !strings.Contains(text, needle) {
		r.Fatalf("the usage error did not mention %q: %s\n%s", needle, strings.TrimSpace(text),
			result.Report())
	}
}

// pinInvocation is a verb over a pin directory, with any further arguments.
func pinInvocation(verb, directory string, extra ...string) []string {
	return append([]string{verb, "--pin-dir", directory}, extra...)
}

// nonEmptyLines is a transcript's lines, so a case can assert on every one.
func nonEmptyLines(text string) []string {
	lines := []string{}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// --- the fixture CLI ----------------------------------------------------------

// Fixture names what `avm-test write-fixture --tool codex` writes and for which
// release.
type Fixture struct {
	Dir     string
	Release string
}

// WriteFixture is `avm-test write-fixture --tool codex`: a committed-shape closure --
// the pin and its four evidence files -- in Dir itself, which is the directory
// `script/test/codex-installer.sh` mounts as its pin directory.
//
// It returns the release it wrote, which is the default when the caller named none.
func WriteFixture(r harness.Reporter, fixture Fixture) string {
	r.Helper()
	release := fixture.Release
	if release == "" {
		release = Release
	}
	newFixture(r, fixture.Dir, release, nil)
	return release
}

// InstallerFixture names one `avm-test write-installer-fixture --tool codex` request.
type InstallerFixture struct {
	Dir          string
	Release      string
	Target       string
	NativeScript string
	// PackageVersion and PackageTarget override the archive's own package metadata,
	// which is how a suite builds a payload the maintenance preflight must refuse.
	PackageVersion string
	PackageTarget  string
}

// WriteInstallerFixture is `avm-test write-installer-fixture --tool codex`: the
// fixture the two installer suites consume.
//
// The package layout is the real one -- four explicit top-level names, real package
// metadata, an entrypoint whose first twenty bytes are the reviewed ELF header -- and
// every pin/evidence digest is derived from these exact bytes. The body after the ELF
// header is the given script, so a suite that runs the payload gets a recording
// command instead of a foreign-architecture binary.
func WriteInstallerFixture(r harness.Reporter, request InstallerFixture) string {
	r.Helper()
	release := request.Release
	if release == "" {
		release = Release
	}
	release = requireFixtureRelease(r, release)
	if !KnownTarget(request.Target) {
		r.Fatalf("unknown target: %s", request.Target)
	}
	selected := target(request.Target)
	version := strings.TrimPrefix(release, releasePrefix)
	body := harness.ReadFile(r, request.NativeScript)
	dir := request.Dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", dir, err)
	}
	archives := map[platform][]byte{}
	for _, name := range platforms {
		triple := targetOf(name)
		metadata := packageMetadataDocument(version, triple)
		if request.PackageVersion != "" {
			metadata.Version = request.PackageVersion
		}
		if request.PackageTarget != "" {
			metadata.Target = request.PackageTarget
		}
		payload := make([]byte, 0, elfHeaderBytes+1+len(body))
		payload = append(payload, elfHeader(machineOf(triple))[:elfHeaderBytes]...)
		payload = append(payload, '\n')
		payload = append(payload, body...)
		archives[name] = buildArchive(r, archiveRequest{
			version:  version,
			platform: name,
			members: []harness.Member{
				{Name: "bin", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "bin/codex", Body: payload, Mode: 0o755},
				{Name: "bin/codex-code-mode-host", Body: []byte("#!/bin/sh\nexit 0\n"), Mode: 0o755},
				{Name: "codex-package.json", Body: encodeDocument(r, metadata), Mode: 0o644},
				{Name: "codex-path", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "codex-path/rg", Body: []byte("#!/bin/sh\nexit 0\n"), Mode: 0o755},
				{Name: "codex-resources", Typeflag: tar.TypeDir, Mode: 0o755},
				{Name: "codex-resources/bwrap", Body: []byte("#!/bin/sh\nexit 0\n"), Mode: 0o755},
			},
		})
	}
	built := newFixture(r, filepath.Join(dir, "source"), release, archives)
	// Both payloads are exported under their canonical asset names, so a suite can
	// answer the writer's exact URLs from a directory without re-deriving anything.
	// `archive.tgz` stays the selected platform for the vendored suite.
	selectedPlatform := maintained[selected].platform
	for _, name := range platforms {
		triple := targetOf(name)
		harness.WriteFile(r, filepath.Join(dir, "assets", assetName(triple)), archives[name])
		harness.WriteFile(r, filepath.Join(dir, "assets", bundleName(triple)), built.bundles[name])
	}
	harness.WriteFile(r, filepath.Join(dir, "assets", "release.json"), built.metadata)
	harness.WriteFile(r, filepath.Join(dir, "assets", ChecksumsAsset), built.checksums)
	harness.WriteFile(r, filepath.Join(dir, "archive.tgz"), archives[selectedPlatform])
	writeUpstream(r, filepath.Join(dir, "upstream"), built.served())
	return release
}

// ArchiveMembers is one fixture archive decoded for a case that asserts its layout:
// the member names in tar order, and each member's mode and bytes.
type ArchiveMembers struct {
	Names  []string
	Modes  map[string]int64
	Bodies map[string][]byte
}

// ReadArchive decodes one fixture archive with the standard library alone, so a case
// can assert the bytes the preflight will read without importing the reader.
func ReadArchive(r harness.Reporter, path string) ArchiveMembers {
	r.Helper()
	compressed := bytes.NewReader(harness.ReadFile(r, path))
	decompressed, err := gzip.NewReader(compressed)
	if err != nil {
		r.Fatalf("%s is not a gzip stream: %v", path, err)
	}
	defer func() { _ = decompressed.Close() }()
	found := ArchiveMembers{Modes: map[string]int64{}, Bodies: map[string][]byte{}}
	stream := tar.NewReader(decompressed)
	for {
		header, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			r.Fatalf("cannot read the archive %s: %v", path, err)
		}
		name := strings.TrimSuffix(header.Name, "/")
		found.Names = append(found.Names, name)
		found.Modes[name] = header.Mode
		if header.Typeflag == tar.TypeReg {
			body, err := io.ReadAll(stream)
			if err != nil {
				r.Fatalf("cannot read %s from %s: %v", name, path, err)
			}
			found.Bodies[name] = body
		}
	}
	return found
}
