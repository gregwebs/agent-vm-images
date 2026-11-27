package closure

// The staging-side owner (#252): what a candidate closure *is*, derived once and
// read back by the loader.
//
// The maintainer stager (`avm-pin gopls stage`) resolves a new release with the
// pinned `go`, and then has to decide the same things the loader decides about the
// committed bytes: that the wrapper is in the reviewed canonical form, that both
// checksum kinds exist for every selected module, that the selection obeys the
// reviewed adoption restriction, that the notice layout is the one the ADR
// inspected, and that the signature inventory names this selection. The Python
// stager answered those questions in its own code, which is a second authority over
// the same rules; this file is the single answer both sides call. `stage.go` in
// script/dev/gopls-closure-update.py is gone when #254 lands; until then the
// differential in script/test/gopls-stage-parity.py holds both answers to each
// other.
//
// Nothing here runs a command or makes a request. Resolution (`go get -tool`, `mod
// tidy`, `mod download`) and the two offline cross-builds live in the writer's own
// package, which is never compiled into the image (owner ruling G8a: the layering
// is writer-only code, not layer grammar).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/pathjoin"
)

// Method is the authentication method every record names. The loader refuses
// anything else; it is written here because this file owns the record's write shape.
const Method = "go-checksum-database"

// DeterminationReason and DeterminationReassess are the reviewed rationale a
// renewed record carries. The loader only requires them to be recorded (a review
// change is the stager's renewal comparison, not the checker's rule), so this is
// their one writing.
const (
	DeterminationReason = "For this selected closure, the reviewed authentication channel for " +
		"Go module content is sum.golang.org under the explicit verifier key plus the committed " +
		"go.sum. Maintenance verifies newly proposed sums through that database; ordinary " +
		"builds accept only committed sums. The log establishes consistency, not publisher " +
		"authorization. VCS release-tag or release-commit signatures are available for some " +
		"selected modules (see the scoped inventory), but are not the module system's " +
		"content-authentication channel and are not used or verified here. A per-module VCS " +
		"signature policy, trusted identities and binding to module zip content remain in the " +
		"linked #94 follow-up under #103's owner amendment. Pinning does not establish that " +
		"the release is safe. License files are preserved as evidence, not a legal audit."
	DeterminationReassess = "Renew on a change of root, go.mod, go.sum, selected notice " +
		"layout, signing inventory, rationale/policy or checksum-database verifier key; review " +
		"supported gopls signing/attestation changes and the linked follow-up explicitly. " +
		"Never automatically extend this determination to new selected source-signature " +
		"evidence."
	// AmendmentURL is the owner amendment the determination is scoped under, and
	// SignatureEvidenceURLs are the release pages the inventory was read from.
	AmendmentURL = "https://github.com/gregwebs/agent-vm-images/issues/103#issuecomment-6422547991"
)

// SignatureEvidenceURLs is the reviewed availability evidence, in inventory order.
var SignatureEvidenceURLs = []string{
	"https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.6.0",
	"https://github.com/mvdan/gofumpt/releases/tag/v0.10.0",
	"https://github.com/fsnotify/fsnotify/releases/tag/v1.9.0",
}

// Renewal is the sentence a stager prints when the reviewed scope changed and no
// human supplied the day they reviewed the new one. The shell writer and the
// suites match it.
const Renewal = "module-channel determination requires renewed review"

// ReviewEvidence is the scoped evidence list a record carries for one release. It is
// a function of the release only, so a carried review and a renewed one compare as
// data.
func ReviewEvidence(release ReleaseVersion) []string {
	evidence := []string{
		Proxy + "/" + Gopls + "/@v/" + string(release) + ".info",
		"https://go.dev/ref/mod#checksum-database",
		"https://sum.golang.org/lookup/" + Gopls + "@" + string(release),
		"https://go.googlesource.com/tools/+/refs/tags/gopls/" + string(release),
		AmendmentURL,
	}
	evidence = append(evidence, SignatureEvidenceURLs...)
	return append(evidence, AuthenticationIssue)
}

// Candidate is a resolved closure before it is a record: the bytes, the parsed
// values, the selection with both sums and the notice evidence collected from the
// module cache, and the four digests a record binds.
type Candidate struct {
	GoMod      []byte
	GoSum      []byte
	RootGoMod  []byte
	Wrapper    WrapperGoMod
	Root       RootGoMod
	Sums       GoSum
	Modules    []PinnedModule
	Notices    []LicenseSection
	Licenses   []byte
	Signatures []SignatureEvidence
	Files      map[Filename]SHA256
}

// CandidateRequest is what the writer resolved and what the owner must be told.
type CandidateRequest struct {
	// Release is the exact gopls release the wrapper must select.
	Release ReleaseVersion
	// Compiler is the pinned compiler the wrapper's `go` directive must not exceed.
	Compiler GoVersion
	// GoMod, GoSum and RootGoMod are the bytes `go mod tidy` and the proxy's
	// `gopls.go.mod` produced. RootGoMod is upstream evidence, not our file.
	GoMod     []byte
	GoSum     []byte
	RootGoMod []byte
	// ModuleCache is the GOMODCACHE the resolution downloaded into. The notice
	// evidence is read from it, and it is the tree the build will compile from.
	ModuleCache string
}

// Candidate is derived by applying the loader's own rules to freshly resolved bytes.
// The candidate is re-loaded after it is written (the writer's recheck), so this is
// the pre-build half of one rule set rather than a verdict of its own.
func DeriveCandidate(request CandidateRequest) (*Candidate, error) {
	wrapper, err := parseWrapperGoMod(request.GoMod, string(FileGoMod))
	if err != nil {
		return nil, err
	}
	// A wrapper that parses but does not re-render is not the reviewed file: the
	// stager refuses to write bytes the loader would refuse to read.
	if err := requireCanonicalWrapper(wrapper, request.GoMod); err != nil {
		return nil, err
	}
	toolVersion, present := wrapper.Requires[ModulePath(Gopls)]
	if !present {
		return nil, refuse("go.mod: the tool module %s is not required", Gopls)
	}
	if toolVersion != ModuleVersion(request.Release) {
		return nil, refuse("go.mod: %s is %s, not %s", Gopls, toolVersion, request.Release)
	}
	sums, err := parseGoSum(request.GoSum, string(FileGoSum))
	if err != nil {
		return nil, err
	}
	root, err := parseRootGoMod(request.RootGoMod, string(FileRootGoMod))
	if err != nil {
		return nil, err
	}
	// The reviewed adoption restriction, the compiler/language relation and the
	// per-module sums are the loader's rules. The stager calls them before the first
	// cross-build instead of re-deriving a selection of its own.
	if err := requireAdoption(wrapper.Requires, root.Requires); err != nil {
		return nil, err
	}
	if wrapper.Go != root.Go {
		return nil, refuse("go directive %s does not equal the root module's %s", wrapper.Go,
			root.Go)
	}
	if !languageAtMost(wrapper.Go, request.Compiler) {
		return nil, refuse("go directive %s is newer than the pinned compiler go%s", wrapper.Go,
			request.Compiler)
	}
	modules, notices, err := deriveSelection(wrapper.Requires, sums, request.ModuleCache)
	if err != nil {
		return nil, err
	}
	signatures, err := ProjectSignatures(modules)
	if err != nil {
		return nil, err
	}
	licenses := RenderLicenses(notices, request.Release)
	// The four inputs a record binds. The pin cannot hash itself, so FilePin is
	// deliberately absent rather than forgotten.
	files := map[Filename]SHA256{}
	files[FileGoMod] = a256(request.GoMod)
	files[FileGoSum] = a256(request.GoSum)
	files[FileRootGoMod] = a256(request.RootGoMod)
	files[FileLicenses] = a256(licenses)
	return &Candidate{GoMod: request.GoMod, GoSum: request.GoSum, RootGoMod: request.RootGoMod,
		Wrapper: wrapper, Root: root, Sums: sums, Modules: modules, Notices: notices,
		Licenses: licenses, Signatures: signatures, Files: files}, nil
}

// deriveSelection is the candidate's selection and notice evidence: both sums for
// every module the wrapper requires, and the reviewed notice layout read from the
// module cache. A missing sum or an unfamiliar layout is a refusal here, before the
// cross-builds, rather than a difference discovered on the finished record.
func deriveSelection(requires map[ModulePath]ModuleVersion, sums GoSum,
	moduleCache string) ([]PinnedModule, []LicenseSection, error) {
	paths := make([]string, 0, len(requires))
	for path := range requires {
		paths = append(paths, string(path))
	}
	sort.Strings(paths)
	modules := make([]PinnedModule, 0, len(paths))
	notices := []LicenseSection{}
	for _, name := range paths {
		path := ModulePath(name)
		id := ModuleID{Path: path, Version: requires[path]}
		zip, present := sums.Zip[id]
		if !present {
			return nil, nil, refuse("go.sum lacks h1 for %s", id)
		}
		mod, present := sums.Mod[id]
		if !present {
			return nil, nil, refuse("go.sum lacks go.mod hash for %s", id)
		}
		found, err := CollectNotices(ModuleCacheDirectory(moduleCache, id), id)
		if err != nil {
			return nil, nil, err
		}
		licenses, err := reviewedLicenses(id, found)
		if err != nil {
			return nil, nil, err
		}
		names := make([]string, 0, len(found))
		for notice := range found {
			names = append(names, string(notice))
		}
		sort.Strings(names)
		for _, notice := range names {
			raw := found[NoticePath(notice)]
			notices = append(notices, LicenseSection{ID: id, Name: NoticePath(notice),
				SHA256: a256(raw), Raw: raw})
		}
		modules = append(modules, PinnedModule{ID: id, H1: zip, GoModH1: mod,
			Licenses: licenses})
	}
	sort.Slice(notices, func(left, right int) bool {
		if notices[left].ID.Path != notices[right].ID.Path {
			return notices[left].ID.Path < notices[right].ID.Path
		}
		return notices[left].Name < notices[right].Name
	})
	return modules, notices, nil
}

// reviewedLicenses applies the reviewed layout rule to one module's notice set:
// exactly one root notice at least, and a nested set equal to the ADR's inventory
// (or empty). A layout the record has not reviewed stops maintenance for review
// rather than being blessed in passing.
func reviewedLicenses(id ModuleID, found map[NoticePath][]byte) ([]LicenseFile, error) {
	roots, nested := []string{}, []string{}
	names := make([]string, 0, len(found))
	for notice := range found {
		names = append(names, string(notice))
	}
	sort.Strings(names)
	for _, notice := range names {
		if strings.Contains(notice, "/") {
			nested = append(nested, notice)
			continue
		}
		roots = append(roots, notice)
	}
	layout := "%s: unsupported license layout; review notices and update the " +
		"ADR/layout policy before staging"
	if len(roots) == 0 || !sameStrings(nested, sortedCopy(reviewedNestedNotices(id))) {
		return nil, refuse(layout, id)
	}
	licenses := make([]LicenseFile, 0, len(names))
	for _, notice := range names {
		licenses = append(licenses, LicenseFile{Name: NoticePath(notice),
			SHA256: a256(found[NoticePath(notice)])})
	}
	return licenses, nil
}

// ProjectSignatures projects the reviewed inventory onto the selection.
//
// A selected path at another version is not an absence of signatures; it is a
// change the inventory has not seen, so it refuses instead of silently omitting the
// row. Unlisted paths make no claim either way. The loader validates the *scope* of
// whatever inventory a record carries; this is the projection the writer proposes.
func ProjectSignatures(modules []PinnedModule) ([]SignatureEvidence, error) {
	selected := make(map[ModulePath]ModuleVersion, len(modules))
	for _, module := range modules {
		selected[module.ID.Path] = module.ID.Version
	}
	projected := []SignatureEvidence{}
	for _, record := range KnownSignatures {
		path := ModulePath(record.Path)
		version, present := selected[path]
		if !present {
			continue
		}
		if version != ModuleVersion(record.Version) {
			return nil, refuse("signature inventory needs review for %s@%s", path, version)
		}
		projected = append(projected, SignatureEvidence{
			ID: ModuleID{Path: path, Version: version}, Repository: record.Repository,
			Ref: record.Ref, TagObject: record.TagObject, Commit: record.Commit,
			Signed: record.Signed, Format: record.Format})
	}
	return projected, nil
}

// RenderLicenses renders the framed evidence the loader reads back. The frame is a
// byte count, so a UTF-8 notice body cannot desynchronise the reader.
func RenderLicenses(sections []LicenseSection, release ReleaseVersion) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, licenseHeader, string(release))
	for _, section := range sections {
		fmt.Fprintf(&out, "=== %s %s %s bytes=%d sha256=%s\n", section.ID.Path,
			section.ID.Version, section.Name, len(section.Raw), section.SHA256)
		out.Write(section.Raw)
		out.WriteByte('\n')
	}
	return []byte(out.String())
}

// ModuleCacheDirectory is the cache's spelling of one module: both the path and the
// version escaped as the `go` command writes them.
func ModuleCacheDirectory(cache string, id ModuleID) string {
	return pathjoin.Join(cache,
		EscapeModule(string(id.Path))+"@"+EscapeModule(string(id.Version)))
}

// EscapeModule is the module cache's escaping: an uppercase letter is written as
// `!` followed by its lowercase form. The installer reads the same cache through the
// same rule.
func EscapeModule(text string) string {
	var out strings.Builder
	for _, character := range text {
		if character >= 'A' && character <= 'Z' {
			out.WriteByte('!')
			out.WriteRune(character - 'A' + 'a')
			continue
		}
		out.WriteRune(character)
	}
	return out.String()
}

// a256 is the file digest a record binds.
func a256(raw []byte) SHA256 {
	digest := sha256.Sum256(raw)
	return SHA256(hex.EncodeToString(digest[:]))
}

// --- the record ----------------------------------------------------------------

// The document types are the record's write shape, in the committed key order. They
// are separate from Pin because a document is what the writer emits: the loader
// validates whatever bytes it is given, and one type serving both directions would
// make a shape error invisible to the other.
type document struct {
	Schema         int                `json:"schema"`
	Gopls          string             `json:"gopls"`
	Go             string             `json:"go"`
	Module         string             `json:"module"`
	Files          documentFiles      `json:"files"`
	Modules        []documentModule   `json:"modules"`
	Provenance     documentProvenance `json:"provenance"`
	Authentication documentAuth       `json:"authentication"`
}

type documentFiles struct {
	GoMod     string `json:"go.mod"`
	GoSum     string `json:"go.sum"`
	RootGoMod string `json:"gopls.go.mod"`
	Licenses  string `json:"THIRD_PARTY_LICENSES"`
}

type documentModule struct {
	Path     string            `json:"path"`
	Version  string            `json:"version"`
	H1       string            `json:"h1"`
	GoModH1  string            `json:"go_mod_h1"`
	Licenses []documentLicense `json:"licenses"`
}

type documentLicense struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type documentProvenance struct {
	Authenticated bool   `json:"authenticated"`
	Source        string `json:"source"`
	Vcs           string `json:"vcs"`
	URL           string `json:"url"`
	Subdir        string `json:"subdir"`
	Ref           string `json:"ref"`
	Hash          string `json:"hash"`
}

type documentAuth struct {
	Method          string              `json:"method"`
	SumDB           string              `json:"sumdb"`
	Proxy           string              `json:"proxy"`
	Determination   string              `json:"determination"`
	ReviewedOn      *string             `json:"reviewed_on"`
	Reviewer        *string             `json:"reviewer"`
	Scope           documentScope       `json:"scope"`
	Reason          string              `json:"reason"`
	SignaturePolicy string              `json:"vcs_signature_policy"`
	KnownSignatures []documentSignature `json:"known_vcs_signatures"`
	NoticePolicy    string              `json:"notice_policy"`
	FollowUp        string              `json:"follow_up"`
	Evidence        []string            `json:"evidence"`
	ReassessWhen    string              `json:"reassess_when"`
}

type documentScope struct {
	Root  string `json:"root"`
	GoMod string `json:"go.mod"`
	GoSum string `json:"go.sum"`
}

type documentSignature struct {
	Path       string `json:"path"`
	Version    string `json:"version"`
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	TagObject  string `json:"tag_object"`
	Commit     string `json:"commit"`
	Signed     string `json:"signed_object"`
	Format     string `json:"format"`
	Verified   bool   `json:"verified"`
}

// Document projects a candidate and its review onto the record bytes.
//
// review is nil for the retained proposal: a record with no human review yet keeps
// `reviewed_on` and `reviewer` null rather than inventing a date, and the writer
// never publishes it.
func Document(release ReleaseVersion, compiler GoVersion, candidate *Candidate,
	provenance Provenance, review *Review) ([]byte, error) {
	modules := make([]documentModule, 0, len(candidate.Modules))
	for _, module := range candidate.Modules {
		licenses := make([]documentLicense, 0, len(module.Licenses))
		for _, license := range module.Licenses {
			licenses = append(licenses, documentLicense{Name: string(license.Name),
				SHA256: string(license.SHA256)})
		}
		modules = append(modules, documentModule{Path: string(module.ID.Path),
			Version: string(module.ID.Version), H1: string(module.H1),
			GoModH1: string(module.GoModH1), Licenses: licenses})
	}
	signatures := candidate.Signatures
	auth := documentAuth{
		Method:        Method,
		SumDB:         SumDB,
		Proxy:         Proxy,
		Determination: Determination,
		Scope: documentScope{Root: Gopls + "@" + string(release),
			GoMod: string(candidate.Files[FileGoMod]),
			GoSum: string(candidate.Files[FileGoSum])},
		Reason:          DeterminationReason,
		SignaturePolicy: VcsSignaturePolicy,
		NoticePolicy:    NoticePolicy,
		FollowUp:        AuthenticationIssue,
		Evidence:        ReviewEvidence(release),
		ReassessWhen:    DeterminationReassess,
	}
	if review != nil {
		reviewedOn := string(review.ReviewedOn)
		auth.ReviewedOn = &reviewedOn
		reviewer := review.Reviewer
		auth.Reviewer = &reviewer
		// A renewed record carries the inventory it reviewed; a carried one already
		// equals the candidate projection (that is why it carried).
		signatures = review.Signatures
	}
	auth.KnownSignatures = make([]documentSignature, 0, len(signatures))
	for _, signature := range signatures {
		auth.KnownSignatures = append(auth.KnownSignatures, documentSignature{
			Path: string(signature.ID.Path), Version: string(signature.ID.Version),
			Repository: signature.Repository, Ref: signature.Ref,
			TagObject: signature.TagObject, Commit: signature.Commit,
			Signed: string(signature.Signed), Format: signature.Format, Verified: false})
	}
	return encodeDocument(document{
		Schema: 1, Gopls: string(release), Go: string(compiler), Module: Wrapper,
		Files: documentFiles{
			GoMod:     string(candidate.Files[FileGoMod]),
			GoSum:     string(candidate.Files[FileGoSum]),
			RootGoMod: string(candidate.Files[FileRootGoMod]),
			Licenses:  string(candidate.Files[FileLicenses]),
		},
		Modules: modules,
		Provenance: documentProvenance{Source: provenance.Source, Vcs: provenance.Vcs,
			URL: provenance.URL, Subdir: provenance.Subdir, Ref: provenance.Ref,
			Hash: provenance.Hash},
		Authentication: auth,
	})
}

// RenewalReview is the review a human supplied for this run.
func RenewalReview(release ReleaseVersion, candidate *Candidate, reviewedOn Date,
	reviewer string) Review {
	return Review{ReviewedOn: reviewedOn, Reviewer: reviewer,
		Determination: Determination, Reason: DeterminationReason,
		Evidence: ReviewEvidence(release), ReassessWhen: DeterminationReassess,
		SignatureRule: VcsSignaturePolicy, NoticeRule: NoticePolicy,
		FollowUp: AuthenticationIssue, Signatures: candidate.Signatures}
}

// Carried reports whether a previous record's review still covers this candidate.
//
// The whole reviewed scope must be unchanged: the selected modules with their
// content and go.mod identities, the notice identity of every module (and therefore
// the transported notice bytes), the signature inventory, and every
// determination/policy constant. A graph-only comparison would silently extend an
// old review over new evidence.
func Carried(previous *Data, release ReleaseVersion, candidate *Candidate) bool {
	if previous == nil {
		return false
	}
	review := previous.Pin.Review
	return previous.Pin.Gopls == release &&
		sameModules(previous.Pin.Modules, candidate.Modules) &&
		previous.Pin.Files[FileGoMod] == candidate.Files[FileGoMod] &&
		previous.Pin.Files[FileGoSum] == candidate.Files[FileGoSum] &&
		previous.Pin.Files[FileLicenses] == candidate.Files[FileLicenses] &&
		review.Determination == Determination &&
		review.Reason == DeterminationReason &&
		review.ReassessWhen == DeterminationReassess &&
		sameStrings(review.Evidence, ReviewEvidence(release)) &&
		review.SignatureRule == VcsSignaturePolicy &&
		review.NoticeRule == NoticePolicy &&
		review.FollowUp == AuthenticationIssue &&
		sameSignatures(review.Signatures, candidate.Signatures)
}

func sameModules(left, right []PinnedModule) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ID != right[index].ID || left[index].H1 != right[index].H1 ||
			left[index].GoModH1 != right[index].GoModH1 ||
			len(left[index].Licenses) != len(right[index].Licenses) {
			return false
		}
		for license := range left[index].Licenses {
			if left[index].Licenses[license] != right[index].Licenses[license] {
				return false
			}
		}
	}
	return true
}

func sameSignatures(left, right []SignatureEvidence) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// Identity is the review identity a renewal refusal names: who reviewed what, or
// that no determination exists yet.
func Identity(previous *Data, release ReleaseVersion) string {
	if previous == nil {
		return Gopls + "@" + string(release) + ": no previous reviewed determination"
	}
	return Gopls + "@" + string(previous.Pin.Gopls) + " reviewed by " +
		previous.Pin.Review.Reviewer +
		" on " + string(previous.Pin.Review.ReviewedOn) + " -> " + Gopls + "@" + string(release)
}

// Describe is the auditable proposal: the module table diff, the per-line go.sum
// delta, the notice and signature deltas and the scope digests. It is the summary
// the writer prints and the file it retains, so it must be a function of the old
// record and the candidate and nothing else.
func Describe(previous *Data, release ReleaseVersion, compiler GoVersion,
	candidate *Candidate, carried bool, review *Review) string {
	lines := []string{}
	oldRelease, oldCompiler := "(none)", "(none)"
	var previousModules []string
	if previous != nil {
		oldRelease = string(previous.Pin.Gopls)
		oldCompiler = "go" + string(previous.Pin.Go)
	}
	lines = append(lines, "gopls: "+oldRelease+" -> "+string(release))
	lines = append(lines, "go:    "+oldCompiler+" -> go"+string(compiler))
	oldDirective := "(none)"
	if previous != nil {
		oldDirective = string(previous.Wrapper.Go)
	}
	lines = append(lines, "go directive: "+oldDirective+" -> "+string(candidate.Wrapper.Go))

	previousVersions := map[string]string{}
	if previous != nil {
		for _, module := range previous.Pin.Modules {
			previousVersions[string(module.ID.Path)] = string(module.ID.Version)
		}
	}
	currentVersions := map[string]string{}
	for _, module := range candidate.Modules {
		currentVersions[string(module.ID.Path)] = string(module.ID.Version)
	}
	previousModules = sortedKeys(previousVersions, currentVersions)
	unchanged := 0
	for _, path := range previousModules {
		old, present := previousVersions[path]
		if !present {
			old = "(absent)"
		}
		now, present := currentVersions[path]
		if !present {
			now = "(absent)"
		}
		if old != now {
			lines = append(lines, "module "+path+": "+old+" -> "+now)
			continue
		}
		if _, ok := currentVersions[path]; ok {
			unchanged++
		}
	}
	lines = append(lines, fmt.Sprintf("modules: %d selected, %d unchanged",
		len(currentVersions), unchanged))

	// Both sides are the whole go.sum file, not the selection: the delta is what a
	// reviewer sees in the file, and a module the graph no longer selects is a removed
	// line even though it is not a removed requirement.
	previousSum := map[string]struct{}{}
	if previous != nil {
		for id, digest := range previous.Sums.Zip {
			previousSum[sumLine(id, "", digest)] = struct{}{}
		}
		for id, digest := range previous.Sums.Mod {
			previousSum[sumLine(id, "/go.mod", digest)] = struct{}{}
		}
	}
	currentSum := map[string]struct{}{}
	for id, digest := range candidate.Sums.Zip {
		currentSum[sumLine(id, "", digest)] = struct{}{}
	}
	for id, digest := range candidate.Sums.Mod {
		currentSum[sumLine(id, "/go.mod", digest)] = struct{}{}
	}
	added, removed := difference(currentSum, previousSum), difference(previousSum, currentSum)
	lines = append(lines, fmt.Sprintf("go.sum: %d -> %d lines, %d added, %d removed",
		len(previousSum), len(currentSum), len(added), len(removed)))
	for _, entry := range added {
		lines = append(lines, "  + "+entry)
	}
	for _, entry := range removed {
		lines = append(lines, "  - "+entry)
	}
	for _, name := range FileKeys {
		old := "(absent)"
		if previous != nil {
			old = string(previous.Pin.Files[name])
		}
		lines = append(lines, fmt.Sprintf("files[%s]: %s -> %s", name, old,
			candidate.Files[name]))
	}

	previousSections := map[string]string{}
	if previous != nil {
		for _, module := range previous.Pin.Modules {
			for _, license := range module.Licenses {
				previousSections[string(module.ID.Path)+" "+string(license.Name)] =
					string(license.SHA256)
			}
		}
	}
	currentSections := map[string]string{}
	for _, module := range candidate.Modules {
		for _, license := range module.Licenses {
			currentSections[string(module.ID.Path)+" "+string(license.Name)] =
				string(license.SHA256)
		}
	}
	for _, key := range sortedKeys(previousSections, currentSections) {
		old, present := previousSections[key]
		if !present {
			old = "(absent)"
		}
		now, present := currentSections[key]
		if !present {
			now = "(absent)"
		}
		if old != now {
			lines = append(lines, "notice "+key+": "+old+" -> "+now)
		}
	}
	lines = append(lines, fmt.Sprintf("notice files: %d transported", len(currentSections)))

	previousSignatures := map[ModuleID]SignatureEvidence{}
	if previous != nil {
		for _, signature := range previous.Pin.Review.Signatures {
			previousSignatures[signature.ID] = signature
		}
	}
	currentSignatures := map[ModuleID]SignatureEvidence{}
	for _, signature := range candidate.Signatures {
		currentSignatures[signature.ID] = signature
	}
	lines = append(lines, fmt.Sprintf("signature inventory: %d record(s)",
		len(currentSignatures)))
	identities := []ModuleID{}
	seen := map[ModuleID]struct{}{}
	for _, signature := range previousSignatures {
		if _, present := seen[signature.ID]; !present {
			seen[signature.ID] = struct{}{}
			identities = append(identities, signature.ID)
		}
	}
	for id := range currentSignatures {
		if _, present := seen[id]; !present {
			seen[id] = struct{}{}
			identities = append(identities, id)
		}
	}
	sort.Slice(identities, func(left, right int) bool {
		return identities[left].Path < identities[right].Path
	})
	for _, id := range identities {
		old, wasThere := previousSignatures[id]
		now, isThere := currentSignatures[id]
		switch {
		case !wasThere:
			lines = append(lines, "  + "+id.String()+" "+string(now.Signed)+" "+
				now.TagObject+" -> "+now.Commit+" ("+now.Ref+")")
		case !isThere:
			lines = append(lines, "  - "+id.String()+" "+string(old.Signed)+" "+
				old.TagObject+" -> "+old.Commit+" ("+old.Ref+")")
		case old != now:
			lines = append(lines, "  ~ "+id.String()+" "+string(old.Signed)+"/"+
				old.TagObject+"/"+old.Commit+" -> "+string(now.Signed)+"/"+now.TagObject+"/"+
				now.Commit)
		}
	}
	if previous != nil {
		for _, policy := range []struct{ label, old, now string }{
			{"determination", previous.Pin.Review.Determination, Determination},
			{"vcs signature policy", previous.Pin.Review.SignatureRule, VcsSignaturePolicy},
			{"notice policy", previous.Pin.Review.NoticeRule, NoticePolicy},
			{"reason", previous.Pin.Review.Reason, DeterminationReason},
			{"reassess_when", previous.Pin.Review.ReassessWhen, DeterminationReassess},
		} {
			if policy.old == policy.now {
				continue
			}
			lines = append(lines, "policy "+policy.label+": changed")
			lines = append(lines, "  - "+policy.old)
			lines = append(lines, "  + "+policy.now)
		}
	}
	if review != nil {
		lines = append(lines, fmt.Sprintf("determination: carried=%s reviewer=%s reviewed_on=%s",
			pythonBool(carried), review.Reviewer, review.ReviewedOn))
	} else {
		lines = append(lines, fmt.Sprintf("determination: proposed (carried=%s)",
			pythonBool(carried)))
	}
	lines = append(lines, "scope digests: go.mod="+string(candidate.Files[FileGoMod])+
		" go.sum="+string(candidate.Files[FileGoSum]))
	lines = append(lines, "git diff -- examples/layers/go-dev")
	return strings.Join(lines, "\n")
}

// sumLine is one go.sum line, in Go's own spelling.
func sumLine(id ModuleID, suffix string, digest H1) string {
	return string(id.Path) + " " + string(id.Version) + suffix + " " + string(digest)
}

// encodeDocument writes the two-space-indented record the committed pins have, with a
// trailing newline and no HTML escaping: the proposal is reviewed as a diff, and an
// escaped `<` would rewrite unrelated lines. It is Go's encoder where the Python
// stager used `json.dumps(indent=2)`; every field this record carries is ASCII by its
// own pattern, so the two agree byte for byte on anything the rules admit.
func encodeDocument(value document) ([]byte, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(buffer.String()), nil
}

// difference is one side of a set difference, in sorted order.
func difference(left, right map[string]struct{}) []string {
	only := []string{}
	for name := range left {
		if _, present := right[name]; !present {
			only = append(only, name)
		}
	}
	sort.Strings(only)
	return only
}

// sortedKeys is the union of two keyed maps' keys, sorted.
func sortedKeys[V any](maps ...map[string]V) []string {
	seen := map[string]struct{}{}
	keys := []string{}
	for _, entries := range maps {
		for key := range entries {
			if _, present := seen[key]; present {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// pythonBool spells a boolean the way the retained summary and the suite's
// fragments spell it.
func pythonBool(value bool) string {
	if value {
		return "True"
	}
	return "False"
}
