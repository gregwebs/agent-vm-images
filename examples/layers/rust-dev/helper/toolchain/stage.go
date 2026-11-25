package toolchain

// The staging side of this pin: resolve one exact channel from the upstream
// distribution, authenticate the whole two-architecture closure into an
// invocation-owned directory, and write a candidate data directory there. The
// publisher (`script/dev/upgrade-rust-toolchain.sh`) copies that candidate as one
// transaction; nothing here touches the source tree.
//
// Slice 4a of #187 moved this out of `script/dev/rust-toolchain-update.py`. Every
// rule it applies already lives in this package, so the port is a sequence of
// calls: the manifest grammar and the selection are `decodeTOML`,
// `DeriveSelection` and `manifestEntry`; the canonical origins are
// `ExpectedArchiveURL` and its siblings; the bounds are the `Max*Bytes`
// constants; the bootstrap's rule is `CheckELF` and the hash-only exception is
// the `BootstrapException` the loader validates. What is added here is the
// transaction's own order and the document that order produces, because no
// shipped build ever writes a pin.
//
// Two properties are deliberate and load-bearing:
//
//   - the candidate is re-read through the reader before it is returned
//     (`LoadPin` + `VerifyInputs`), which is defence in depth rather than a rule a
//     test can reach: see the declaration at that call for what that costs; and
//   - the document is emitted in the committed key order with two-space indent
//     (`encodePinDocument`), so re-staging the committed pin reproduces it byte
//     for byte instead of churning the review diff.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/fetch"
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/gpg"
)

// StageRequest is one `avm-pin rust-toolchain stage` invocation. Every path is
// absolute: the publisher runs from the checkout but the scratch directory is the
// host's, and a relative path would name a different tree than the caller meant.
type StageRequest struct {
	Channel    Channel
	Rustup     Semver
	KeyFile    string
	Current    string
	Out        string
	ReviewedOn *IsoDate
	Reviewer   string
	Dockerfile string
}

// VerifierFactory builds the signature verifier once the reviewed key and the
// scratch directory are known: the keyring lives in that directory, so the
// verifier cannot exist before the key does.
type VerifierFactory func(keyFile, work string) SignatureVerifier

// StageDependencies are the seams one staging run has: the byte source, the
// signature verifier factory and the trust the candidate is held to.
type StageDependencies struct {
	Transport     Transport
	Verifier      VerifierFactory
	Trust         TrustPolicy
	GitHubActions bool
}

// VerifierFor is the production verifier factory: the reviewed trust over the
// reviewed gpg program.
func VerifierFor(program gpg.Program, trust TrustPolicy) VerifierFactory {
	return func(keyFile, work string) SignatureVerifier {
		return gpg.NewVerifier(program, keyFile, work, PolicyFor(trust))
	}
}

// ArchiveDigest is one selected archive in the proposal's summary.
type ArchiveDigest struct {
	Package string
	Target  TargetTriple
	Digest  Sha256
}

// Proposal is what a staged run produced: enough for the maintainer to review the
// change without reading the scratch directory.
type Proposal struct {
	Channel       Channel
	Rustup        Semver
	Manifest      Sha256
	ArchiveDigest [2][]ArchiveDigest
	Exception     BootstrapException
}

// The reviewed hash-only exception text. Both the reason and the reassessment
// condition are source, like the fingerprint policy: a bootstrap identity change
// requires a human to renew the review, and this writer refuses to invent it.
const bootstrapExceptionReason = "rustup publishes versioned per-target rustup-init " +
	"binaries with a .sha256 sidecar but no detached OpenPGP signature, attestation or " +
	"release object for 1.28.2 (the .asc endpoints return 404 and the GitHub tag has no " +
	"release). The reviewed exception binds each architecture to the exact versioned URL " +
	"and SHA-256 in this record, cross-checked against the upstream .sha256 sidecar. It is " +
	"hash-only: it does not authenticate the bootstrap publisher, and pinning does not " +
	"establish that the release is safe."

const bootstrapExceptionReassess = "On each rustup-init content or identity change, or on " +
	"publication of a supported upstream signing, attestation or release-provenance scheme; " +
	"renew the review explicitly rather than extending this record."

const bootstrapInstallationDoc = "https://rust-lang.github.io/rustup/installation/other.html"

// Stage authenticates the whole closure for one exact channel and writes the
// candidate data directory into request.Out. It never publishes: the caller's
// transaction owns that, and a failure here leaves the tree untouched.
func Stage(ctx context.Context, request StageRequest, dependencies StageDependencies) (Proposal, error) {
	var proposal Proposal
	if dependencies.Transport == nil {
		dependencies.Transport = fetch.NewClient()
	}
	if dependencies.Verifier == nil {
		program, err := gpg.Resolve()
		if err != nil {
			return proposal, adopt(err)
		}
		dependencies.Verifier = VerifierFor(program, dependencies.Trust)
	}
	if dependencies.GitHubActions {
		return proposal, refuse("stage mode is refused in GitHub Actions; pin maintenance is " +
			"maintainer-host work and ordinary CI only runs the offline check")
	}
	if !contains(SupportedRustupVersions, string(request.Rustup)) {
		// The exception text, format preference and installation behaviour are
		// bound to the audited installer; a different version cannot inherit them.
		return proposal, refuse("rustup %s is not an audited installer; only %s has a "+
			"reviewed format/trust policy, so a new version needs code and policy review",
			request.Rustup, strings.Join(SupportedRustupVersions, ", "))
	}
	if _, err := os.Lstat(request.Out); err == nil {
		return proposal, refuse("--out must be a new invocation-owned path: %s", request.Out)
	}
	previous, err := previousPin(request.Current, dependencies.Trust)
	if err != nil {
		return proposal, err
	}
	key, err := stageKey(request.KeyFile, request.Current, previous)
	if err != nil {
		return proposal, err
	}
	keyRaw, err := os.ReadFile(key)
	if err != nil {
		return proposal, adopt(err)
	}
	keyDigest, err := hashFile(key)
	if err != nil {
		return proposal, err
	}
	// All transport, keyring and scratch state lives outside Out: the output
	// directory holds only the files the publisher will copy.
	work, err := os.MkdirTemp("", "rust-stage-")
	if err != nil {
		return proposal, adopt(err)
	}
	if err := os.Chmod(work, 0o700); err != nil {
		return proposal, adopt(err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	return stageInto(ctx, request, dependencies, previous, key, keyRaw, keyDigest, work)
}

// previousPin is the committed (or complete but uncommitted) pin this run is
// derived from, or nil for a first adoption.
func previousPin(directory string, policy TrustPolicy) (*Pin, error) {
	info, err := os.Stat(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, adopt(err)
	}
	if !info.IsDir() {
		return nil, refuse("--current is not a directory: %s", directory)
	}
	pin, err := LoadPin(directory, policy)
	if err != nil {
		return nil, err
	}
	return &pin, nil
}

// stageKey is the reviewed key: the one the caller named, or the committed one a
// later bump retains.
func stageKey(requested, current string, previous *Pin) (string, error) {
	if requested == "" {
		if previous == nil {
			return "", refuse("first adoption needs --key with the reviewed Rust release key")
		}
		requested = filepath.Join(current, KeyPath)
	}
	info, err := os.Lstat(requested)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", refuse("--key must be a regular file: %s", requested)
	}
	return requested, nil
}

func stageInto(ctx context.Context, request StageRequest, dependencies StageDependencies,
	previous *Pin, key string, keyRaw []byte, keyDigest Sha256, work string) (Proposal, error) {
	var proposal Proposal
	verifier := dependencies.Verifier(key, work)
	downloads := filepath.Join(work, "downloads")
	if err := makePrivate(downloads); err != nil {
		return proposal, err
	}

	manifestURL := CanonicalURL(fmt.Sprintf("%s/dist/channel-rust-%s.toml", DistServer,
		request.Channel))
	for _, suffix := range []string{"", ".sha256", ".asc"} {
		url := string(manifestURL) + suffix
		if err := dependencies.Transport.Fetch(ctx, url,
			filepath.Join(downloads, manifestName(suffix)), MaxEvidenceBytes); err != nil {
			return proposal, adopt(err)
		}
	}
	manifestPath := filepath.Join(downloads, ManifestPath)
	checksumPath := filepath.Join(downloads, ManifestChecksumPath)
	signaturePath := filepath.Join(downloads, ManifestSignaturePath)
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return proposal, adopt(err)
	}
	checksumRaw, err := os.ReadFile(checksumPath)
	if err != nil {
		return proposal, adopt(err)
	}
	signatureRaw, err := os.ReadFile(signaturePath)
	if err != nil {
		return proposal, adopt(err)
	}
	manifestDigest, err := hashFile(manifestPath)
	if err != nil {
		return proposal, err
	}
	checksumLine, err := oneChecksumLine(checksumRaw, "manifest checksum")
	if err != nil {
		return proposal, err
	}
	if !strings.HasPrefix(checksumLine, string(manifestDigest)+" ") {
		return proposal, refuse("upstream manifest does not match its published .sha256 line")
	}
	if err := verifier.Verify(ctx, manifestPath, signaturePath, "manifest signature"); err != nil {
		return proposal, err
	}
	manifest, err := decodeTOML(manifestRaw, "upstream manifest")
	if err != nil {
		return proposal, err
	}
	releaseDateNode, err := fieldOf(manifest, "date", "upstream manifest")
	if err != nil {
		return proposal, err
	}
	releaseDate, err := ParseDate(releaseDateNode, "manifest.date")
	if err != nil {
		return proposal, err
	}
	// The manifest is handed to the package that owns its rules rather than
	// re-read here: `VerifyManifestClosure` is the one place that decides the
	// supported manifest version, that the release version is the channel asked
	// for, the reviewed profile and renames, the availability of every selected
	// row, and that each selected row's `xz_url` is the canonical dated origin.
	// The pin it checks against is provisional -- its archive URLs are the
	// canonical ones and its digests come from the signed rows -- because the
	// writer's job at this point is to decide whether it may fetch, and fetching
	// from the canonical origin is itself the rule under test. The candidate
	// re-read below repeats the same call against the *written* pin, which is what
	// catches a non-canonical `xz_url` in a signed manifest.
	provisional, err := provisionalPin(request, dependencies.Trust, manifest, releaseDate)
	if err != nil {
		return proposal, err
	}
	if err := VerifyManifestClosure(manifestRaw, provisional); err != nil {
		return proposal, err
	}

	bootstrapDigests := map[Architecture]Sha256{}
	bootstrapChecksums := map[Architecture][]byte{}
	for _, arch := range Architectures() {
		url := ExpectedBootstrapURL(request.Rustup, arch)
		target := filepath.Join(downloads, "rustup-init-"+arch.Value())
		if err := dependencies.Transport.Fetch(ctx, url, target, MaxBootstrapBytes); err != nil {
			return proposal, adopt(err)
		}
		digest, err := hashFile(target)
		if err != nil {
			return proposal, err
		}
		bootstrapDigests[arch] = digest
		// The source writer named the file it had downloaded, which carries the
		// architecture, and the platform again when the machine is wrong.
		if err := checkELFFile(target, arch, elfLabels{Prefix: "rustup-init-" + arch.Value(),
			Machine: "bootstrap " + arch.Value()}); err != nil {
			return proposal, err
		}
		checksumTarget := filepath.Join(downloads, "rustup-init-"+arch.Value()+".sha256")
		if err := dependencies.Transport.Fetch(ctx, url+".sha256", checksumTarget,
			MaxEvidenceBytes); err != nil {
			return proposal, adopt(err)
		}
		raw, err := os.ReadFile(checksumTarget)
		if err != nil {
			return proposal, adopt(err)
		}
		bootstrapChecksums[arch] = raw
		if err := verifyBootstrapChecksumLine(raw, arch, digest); err != nil {
			return proposal, err
		}
	}

	exception, err := stageException(request, previous, bootstrapDigests)
	if err != nil {
		return proposal, err
	}

	signatureBytes := map[string][]byte{}
	closures := [2]HostClosure{}
	for _, arch := range Architectures() {
		closure := provisional.Closure(arch)
		closure.Bootstrap = Content{URL: CanonicalURL(ExpectedBootstrapURL(request.Rustup, arch)),
			Digest: bootstrapDigests[arch]}
		closure.BootstrapChecksum = Evidence{
			Path:   EvidencePath(ExpectedBootstrapChecksumPath(arch)),
			Digest: shaOfBytes(bootstrapChecksums[arch])}
		digests := make([]ArchiveDigest, 0, len(closure.Archives))
		for index := range closure.Archives {
			archive := &closure.Archives[index]
			raw, err := stageArchive(ctx, dependencies, verifier, downloads, *archive)
			if err != nil {
				return proposal, err
			}
			// The signature's digest is the one thing this loop adds: the provisional
			// record named the evidence path, and the bytes that arrived are what the
			// pin records.
			archive.Signature.Digest = shaOfBytes(raw)
			signatureBytes[string(archive.Signature.Path)] = raw
			digests = append(digests, ArchiveDigest{Package: archive.Package,
				Target: archive.Target, Digest: archive.Content.Digest})
		}
		closures[int(arch)] = closure
		proposal.ArchiveDigest[int(arch)] = digests
	}

	document := encodeDocument(Pin{
		Channel:            request.Channel,
		ReleaseDate:        releaseDate,
		Profile:            Profile,
		RustupVersion:      request.Rustup,
		Primary:            dependencies.Trust.Primary(),
		Signers:            dependencies.Trust.Signers(),
		KeySha256:          keyDigest,
		Manifest:           Content{URL: manifestURL, Digest: manifestDigest},
		ManifestChecksum:   Content{URL: CanonicalURL(string(manifestURL) + ".sha256"), Digest: shaOfBytes(checksumRaw)},
		ManifestSignature:  Content{URL: CanonicalURL(string(manifestURL) + ".asc"), Digest: shaOfBytes(signatureRaw)},
		Closures:           closures,
		BootstrapException: exception,
	})
	pinBytes, err := jsonBytes(document)
	if err != nil {
		return proposal, err
	}
	data := filepath.Join(request.Out, "rust-toolchain")
	files := []candidateFile{
		{filepath.Join(data, ManifestPath), manifestRaw},
		{filepath.Join(data, ManifestChecksumPath), checksumRaw},
		{filepath.Join(data, ManifestSignaturePath), signatureRaw},
		{filepath.Join(data, KeyPath), keyRaw},
	}
	for _, arch := range Architectures() {
		files = append(files, candidateFile{
			filepath.Join(data, ExpectedBootstrapChecksumPath(arch)), bootstrapChecksums[arch]})
	}
	evidencePaths := make([]string, 0, len(signatureBytes))
	for path := range signatureBytes {
		evidencePaths = append(evidencePaths, path)
	}
	sort.Strings(evidencePaths)
	for _, path := range evidencePaths {
		files = append(files, candidateFile{filepath.Join(data, path), signatureBytes[path]})
	}
	files = append(files, candidateFile{filepath.Join(data, PinFile), pinBytes})
	for _, file := range files {
		if err := writeNewFile(file.path, file.raw); err != nil {
			return proposal, err
		}
	}
	rewritten, err := rewriteToolchainArgument(request.Dockerfile, request.Channel)
	if err != nil {
		return proposal, err
	}
	if err := writeNewFile(filepath.Join(request.Out, "Dockerfile"), rewritten); err != nil {
		return proposal, err
	}

	// The candidate is held to the reader before it is offered: a proposal the
	// image would refuse fails here, not after publication.
	//
	// Declared: this re-read is *not* witnessed by deletion. No fixture the suites
	// have today distinguishes these two calls from their absence, so no test turns
	// red when they are removed, and this comment is where that is stated rather
	// than only in a PR body. It stays because a writer regression is exactly what
	// it is for -- the document, the evidence file set and the digests are the only
	// part of this path that is written rather than called, and this is the one
	// check that reads them back from disk with the reader the image runs.
	candidate, err := LoadPin(data, dependencies.Trust)
	if err != nil {
		return proposal, err
	}
	if _, err := VerifyInputs(ctx, data, candidate, verifier); err != nil {
		return proposal, err
	}
	proposal.Channel, proposal.Rustup = request.Channel, request.Rustup
	proposal.Manifest = manifestDigest
	proposal.Exception = exception
	return proposal, nil
}

// stageArchive fetches one selected archive from the canonical origin its record
// names, requires the bytes to be the ones the signed manifest described, verifies
// the detached signature and preflights the payload. It returns the signature bytes
// to publish as evidence.
//
// It reads nothing out of the manifest: the record it is given was validated by
// `VerifyManifestClosure` (including that the URL is the canonical dated origin),
// so the only rule applied here is the one no earlier step can apply -- whether the
// bytes that arrived are the bytes the signed manifest named.
func stageArchive(ctx context.Context, dependencies StageDependencies,
	verifier SignatureVerifier, downloads string, archive ComponentArchive) ([]byte, error) {
	url := archive.Content.URL
	targetPath := filepath.Join(downloads, url.Filename())
	if err := dependencies.Transport.Fetch(ctx, string(url), targetPath,
		MaxArchiveBytes); err != nil {
		return nil, adopt(err)
	}
	digest, err := hashFile(targetPath)
	if err != nil {
		return nil, err
	}
	if digest != archive.Content.Digest {
		return nil, refuse("%s: downloaded bytes do not match the manifest hash", url.Filename())
	}
	signaturePath := filepath.Join(downloads, url.Filename()+".asc")
	if err := dependencies.Transport.Fetch(ctx, string(url)+".asc", signaturePath,
		MaxEvidenceBytes); err != nil {
		return nil, adopt(err)
	}
	if err := verifier.Verify(ctx, targetPath, signaturePath,
		url.Filename()+" signature"); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(signaturePath)
	if err != nil {
		return nil, adopt(err)
	}
	if err := PreflightArchive(targetPath, archive, NewEvents("")); err != nil {
		return nil, err
	}
	return raw, nil
}

// provisionalPin is the pin this run *would* write, built from the signed
// manifest's selection with the canonical origins the policy derives. It exists
// only so the manifest's own rules are applied by their owner before anything is
// downloaded; the digests and the member prefixes are the manifest's, and a
// mismatch between them and the downloaded bytes is the one rule `stageArchive`
// keeps.
func provisionalPin(request StageRequest, trust TrustPolicy, manifest map[string]any,
	releaseDate IsoDate) (Pin, error) {
	pin := Pin{Channel: request.Channel, ReleaseDate: releaseDate, Profile: Profile,
		RustupVersion: request.Rustup, Primary: trust.Primary(), Signers: trust.Signers()}
	for _, arch := range Architectures() {
		selection, err := DeriveSelection(manifest, arch)
		if err != nil {
			return pin, err
		}
		closure := HostClosure{Arch: arch, Host: arch.Host(), Musl: arch.Musl(),
			Bootstrap: Content{URL: CanonicalURL(ExpectedBootstrapURL(request.Rustup, arch))}}
		for _, pair := range selection {
			pkg, target := pair[0], TargetTriple(pair[1])
			entry, err := manifestEntry(manifest, pkg, target)
			if err != nil {
				return pin, err
			}
			digest, ok := entry["xz_hash"].(string)
			if !ok {
				return pin, refuse("manifest: %s %s lacks a full xz pair", pkg, target)
			}
			stem := ArchiveFilename(request.Channel, pkg, target)
			closure.Archives = append(closure.Archives, ComponentArchive{
				Package: pkg,
				Target:  target,
				Content: Content{URL: CanonicalURL(ExpectedArchiveURL(request.Channel,
					releaseDate, pkg, target)), Digest: Sha256(digest)},
				MemberPrefix: MemberPrefix(stem[:len(stem)-len(".tar.xz")] + "/"),
				Signature:    Evidence{Path: EvidencePath(ExpectedSignaturePath(arch, pkg, target))},
			})
		}
		pin.Closures[int(arch)] = closure
	}
	return pin, nil
}

// Describe is the proposal summary the maintainer reviews. It is a contract line
// for the suites as much as a report, so its shape is frozen.
func (p Proposal) Describe(previous *Pin) string {
	lines := []string{
		fmt.Sprintf("channel: %s -> %s", fieldOfPin(previous, func(pin *Pin) string {
			return string(pin.Channel)
		}), p.Channel),
		fmt.Sprintf("rustup:  %s -> %s", fieldOfPin(previous, func(pin *Pin) string {
			return string(pin.RustupVersion)
		}), p.Rustup),
		"manifest sha256: " + string(p.Manifest),
	}
	for _, arch := range Architectures() {
		for _, each := range p.ArchiveDigest[int(arch)] {
			lines = append(lines, fmt.Sprintf("%s %s %s: %s", arch.Platform(), each.Package,
				each.Target, each.Digest))
		}
	}
	lines = append(lines, fmt.Sprintf("bootstrap exception: %s %s", p.Exception.Reviewer,
		p.Exception.ReviewedOn))
	for _, arch := range Architectures() {
		content := p.Exception.Scope[arch.Platform()]
		lines = append(lines, fmt.Sprintf("%s bootstrap: %s %s", arch.Platform(), content.URL,
			content.Digest))
	}
	return strings.Join(lines, "\n")
}

func fieldOfPin(previous *Pin, field func(*Pin) string) string {
	if previous == nil {
		return "(none)"
	}
	return field(previous)
}

// stageException keeps the committed review when it still describes the
// bootstrap, and otherwise demands a new one: a changed identity is a policy
// change, never an automatic edit.
func stageException(request StageRequest, previous *Pin,
	digests map[Architecture]Sha256) (BootstrapException, error) {
	if previous != nil && exceptionUnchanged(previous.BootstrapException, request.Rustup, digests) {
		return previous.BootstrapException, nil
	}
	if request.ReviewedOn == nil || request.Reviewer == "" {
		lines := []string{"hash-only bootstrap exception requires renewed review"}
		for _, arch := range Architectures() {
			fresh := fmt.Sprintf("%s %s", ExpectedBootstrapURL(request.Rustup, arch),
				digests[arch])
			if previous == nil {
				lines = append(lines, fmt.Sprintf("  %s: new %s", arch.Platform(), fresh))
				continue
			}
			old := previous.BootstrapException.Scope[arch.Platform()]
			lines = append(lines, fmt.Sprintf("  %s: old %s %s -> new %s", arch.Platform(),
				old.URL, old.Digest, fresh))
		}
		return BootstrapException{}, refuse("%s", strings.Join(lines, "\n"))
	}
	return buildException(request.Rustup, *request.ReviewedOn, request.Reviewer, digests), nil
}

// exceptionUnchanged reports whether the committed review still binds the exact
// bootstrap URLs this run resolved and the bytes it downloaded.
func exceptionUnchanged(previous BootstrapException, version Semver,
	digests map[Architecture]Sha256) bool {
	for _, arch := range Architectures() {
		content, ok := previous.Scope[arch.Platform()]
		if !ok {
			return false
		}
		if string(content.URL) != ExpectedBootstrapURL(version, arch) {
			return false
		}
		if content.Digest != digests[arch] {
			return false
		}
	}
	return true
}

func buildException(version Semver, reviewedOn IsoDate, reviewer string,
	digests map[Architecture]Sha256) BootstrapException {
	scope := make(map[string]Content, len(Architectures()))
	evidence := []string{bootstrapInstallationDoc}
	for _, arch := range Architectures() {
		url := CanonicalURL(ExpectedBootstrapURL(version, arch))
		scope[arch.Platform()] = Content{URL: url, Digest: digests[arch]}
		evidence = append(evidence, string(url), string(url)+".sha256")
	}
	return BootstrapException{ReviewedOn: reviewedOn, Reviewer: reviewer,
		Reason: bootstrapExceptionReason, Evidence: evidence,
		ReassessWhen: bootstrapExceptionReassess, Scope: scope}
}

// rewriteToolchainArgument is the one Dockerfile edit the writer makes: the
// reviewer's channel assertion, which must be exactly one line.
func rewriteToolchainArgument(path string, channel Channel) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, adopt(err)
	}
	lines := strings.Split(string(raw), "\n")
	matches := []int{}
	for index, line := range lines {
		if strings.HasPrefix(line, "ARG RUST_TOOLCHAIN=") {
			matches = append(matches, index)
		}
	}
	if len(matches) != 1 {
		return nil, refuse("Dockerfile: expected exactly one ARG RUST_TOOLCHAIN line")
	}
	lines[matches[0]] = "ARG RUST_TOOLCHAIN=" + string(channel)
	return []byte(strings.Join(lines, "\n")), nil
}

// --- the document -------------------------------------------------------------

// pinDocument is the pin's write shape, in the committed key order. It is a
// separate type from Pin because a document is what the writer emits, not what
// the model means: coupling them would make one spelling serve both directions,
// and the loader's job is to validate whatever bytes it is given.
type pinDocument struct {
	Schema             int                     `json:"schema"`
	Channel            string                  `json:"channel"`
	ReleaseDate        string                  `json:"release_date"`
	Profile            string                  `json:"profile"`
	RustupVersion      string                  `json:"rustup_version"`
	Trust              documentTrust           `json:"trust"`
	Manifest           documentManifest        `json:"manifest"`
	Platforms          documentPlatforms       `json:"platforms"`
	BootstrapException documentBootstrapReview `json:"bootstrap_exception"`
}

type documentTrust struct {
	Key                 documentEvidence `json:"key"`
	PrimaryFingerprint  string           `json:"primary_fingerprint"`
	SigningFingerprints []string         `json:"signing_fingerprints"`
}

type documentEvidence struct {
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

type documentContent struct {
	URL    string `json:"url"`
	Sha256 string `json:"sha256"`
}

type documentManifest struct {
	URL       string                  `json:"url"`
	Path      string                  `json:"path"`
	Sha256    string                  `json:"sha256"`
	Checksum  documentEvidenceContent `json:"checksum"`
	Signature documentEvidenceContent `json:"signature"`
}

// documentEvidenceContent is an evidence file that also records the origin it was
// fetched from, which is what makes the manifest triplet re-fetchable.
type documentEvidenceContent struct {
	URL    string `json:"url"`
	Path   string `json:"path"`
	Sha256 string `json:"sha256"`
}

type documentPlatforms struct {
	AMD64 documentClosure `json:"linux/amd64"`
	ARM64 documentClosure `json:"linux/arm64"`
}

type documentClosure struct {
	Host      string            `json:"host"`
	Musl      string            `json:"musl"`
	Bootstrap documentBootstrap `json:"bootstrap"`
	Archives  []documentArchive `json:"archives"`
}

type documentBootstrap struct {
	URL      string           `json:"url"`
	Sha256   string           `json:"sha256"`
	Checksum documentEvidence `json:"checksum"`
}

type documentArchive struct {
	Package      string           `json:"package"`
	Target       string           `json:"target"`
	URL          string           `json:"url"`
	Sha256       string           `json:"sha256"`
	MemberPrefix string           `json:"member_prefix"`
	Signature    documentEvidence `json:"signature"`
}

type documentBootstrapReview struct {
	Method       string              `json:"method"`
	ReviewedOn   string              `json:"reviewed_on"`
	Reviewer     string              `json:"reviewer"`
	Scope        documentReviewScope `json:"scope"`
	Reason       string              `json:"reason"`
	Evidence     []string            `json:"evidence"`
	ReassessWhen string              `json:"reassess_when"`
}

type documentReviewScope struct {
	AMD64 documentContent `json:"linux/amd64"`
	ARM64 documentContent `json:"linux/arm64"`
}

// encodeDocument projects the model onto the document. It is the only place the
// two shapes meet, so a field added to one and not the other is visible here.
func encodeDocument(pin Pin) pinDocument {
	document := pinDocument{
		Schema:        1,
		Channel:       string(pin.Channel),
		ReleaseDate:   string(pin.ReleaseDate),
		Profile:       pin.Profile,
		RustupVersion: string(pin.RustupVersion),
		Trust: documentTrust{
			Key:                 documentEvidence{Path: KeyPath, Sha256: string(pin.KeySha256)},
			PrimaryFingerprint:  string(pin.Primary),
			SigningFingerprints: fingerprints(pin.Signers),
		},
		Manifest: documentManifest{
			URL:    string(pin.Manifest.URL),
			Path:   ManifestPath,
			Sha256: string(pin.Manifest.Digest),
			Checksum: documentEvidenceContent{URL: string(pin.ManifestChecksum.URL),
				Path: ManifestChecksumPath, Sha256: string(pin.ManifestChecksum.Digest)},
			Signature: documentEvidenceContent{URL: string(pin.ManifestSignature.URL),
				Path: ManifestSignaturePath, Sha256: string(pin.ManifestSignature.Digest)},
		},
		BootstrapException: documentBootstrapReview{
			Method:     "reviewed-hash-only",
			ReviewedOn: string(pin.BootstrapException.ReviewedOn),
			Reviewer:   pin.BootstrapException.Reviewer,
			Scope: documentReviewScope{
				AMD64: reviewContent(pin.BootstrapException, AMD64),
				ARM64: reviewContent(pin.BootstrapException, ARM64),
			},
			Reason:       pin.BootstrapException.Reason,
			Evidence:     append([]string{}, pin.BootstrapException.Evidence...),
			ReassessWhen: pin.BootstrapException.ReassessWhen,
		},
	}
	for _, arch := range Architectures() {
		closure := closureDocument(pin.Closure(arch))
		if arch == AMD64 {
			document.Platforms.AMD64 = closure
		} else {
			document.Platforms.ARM64 = closure
		}
	}
	return document
}

func closureDocument(closure HostClosure) documentClosure {
	document := documentClosure{
		Host: string(closure.Host),
		Musl: string(closure.Musl),
		Bootstrap: documentBootstrap{
			URL:    string(closure.Bootstrap.URL),
			Sha256: string(closure.Bootstrap.Digest),
			Checksum: documentEvidence{Path: string(closure.BootstrapChecksum.Path),
				Sha256: string(closure.BootstrapChecksum.Digest)},
		},
		Archives: make([]documentArchive, 0, len(closure.Archives)),
	}
	for _, archive := range closure.Archives {
		document.Archives = append(document.Archives, documentArchive{
			Package:      archive.Package,
			Target:       string(archive.Target),
			URL:          string(archive.Content.URL),
			Sha256:       string(archive.Content.Digest),
			MemberPrefix: string(archive.MemberPrefix),
			Signature: documentEvidence{Path: string(archive.Signature.Path),
				Sha256: string(archive.Signature.Digest)},
		})
	}
	return document
}

func reviewContent(review BootstrapException, arch Architecture) documentContent {
	content := review.Scope[arch.Platform()]
	return documentContent{URL: string(content.URL), Sha256: string(content.Digest)}
}

func fingerprints(signers []Fingerprint) []string {
	result := make([]string, 0, len(signers))
	for _, signer := range signers {
		result = append(result, string(signer))
	}
	return result
}

// jsonBytes writes the two-space-indented document the pin has always had, with
// a trailing newline and no HTML escaping: the stager's output is reviewed as a
// diff, and an escaped `<` would rewrite unrelated lines.
func jsonBytes(document pinDocument) ([]byte, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, adopt(err)
	}
	return []byte(buffer.String()), nil
}

// --- scratch helpers ----------------------------------------------------------

type candidateFile struct {
	path string
	raw  []byte
}

// manifestName is the committed file name each upstream manifest artefact is
// downloaded to: the suffix the URL carries is the suffix the pin records.
func manifestName(suffix string) string {
	switch suffix {
	case ".sha256":
		return ManifestChecksumPath
	case ".asc":
		return ManifestSignaturePath
	default:
		return ManifestPath
	}
}

// writeNewFile creates one candidate file. It never overwrites: Out is a new
// directory, and a file that already exists means the caller reused a path.
func writeNewFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return adopt(err)
	}
	handle, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return adopt(err)
	}
	if _, err := handle.Write(raw); err != nil {
		_ = handle.Close()
		return adopt(err)
	}
	return adopt(handle.Close())
}

func shaOfBytes(raw []byte) Sha256 {
	digest := sha256.Sum256(raw)
	return Sha256(hex.EncodeToString(digest[:]))
}
