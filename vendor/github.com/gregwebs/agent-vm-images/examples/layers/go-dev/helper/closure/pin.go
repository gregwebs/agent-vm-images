package closure

import (
	"net/url"
	"strings"
)

// The pin document: the reviewed selection, the provenance record, the
// authentication determination and the dated review that binds them. The key
// sets are closed, so a pin that grows a field is a code review rather than a
// silently ignored input.

var (
	pinKeys        = []string{"schema", "gopls", "go", "module", "files", "modules", "provenance", "authentication"}
	moduleKeys     = []string{"path", "version", "h1", "go_mod_h1", "licenses"}
	licenseKeys    = []string{"name", "sha256"}
	provenanceKeys = []string{"authenticated", "source", "vcs", "url", "subdir", "ref", "hash"}
	authKeys       = []string{"method", "sumdb", "proxy", "determination", "reviewed_on", "reviewer",
		"scope", "reason", "vcs_signature_policy", "known_vcs_signatures", "notice_policy",
		"follow_up", "evidence", "reassess_when"}
	scopeKeys     = []string{"root", "go.mod", "go.sum"}
	signatureKeys = []string{"path", "version", "repository", "ref", "tag_object", "commit",
		"signed_object", "format", "verified"}
)

// PinnedModule is one selected module with both checksum kinds and its notice
// evidence.
type PinnedModule struct {
	ID       ModuleID
	H1       H1
	GoModH1  H1
	Licenses []LicenseFile
}

// Provenance is the unauthenticated origin metadata the proxy served.
type Provenance struct {
	Source string
	Vcs    string
	URL    string
	Subdir string
	Ref    string
	Hash   string
}

// SignatureEvidence is one inventory row projected onto the selection.
type SignatureEvidence struct {
	ID         ModuleID
	Repository string
	Ref        string
	TagObject  string
	Commit     string
	Signed     SignedObject
	Format     string
}

// Review is the dated, scoped determination that makes this closure trusted.
type Review struct {
	ReviewedOn    Date
	Reviewer      string
	Determination string
	Reason        string
	Evidence      []string
	ReassessWhen  string
	SignatureRule string
	NoticeRule    string
	FollowUp      string
	Signatures    []SignatureEvidence
}

// Pin is the decoded `pin.json`.
type Pin struct {
	Gopls      ReleaseVersion
	Go         GoVersion
	Module     string
	Files      map[Filename]SHA256
	Modules    []PinnedModule
	Provenance Provenance
	Review     Review
}

func parseProvenance(node any) (Provenance, error) {
	members, err := requireKeys(node, provenanceKeys, nil, "provenance")
	if err != nil {
		return Provenance{}, err
	}
	authenticated, err := fieldOf(members, "authenticated", "provenance")
	if err != nil {
		return Provenance{}, err
	}
	if flag, ok := authenticated.(bool); !ok || flag {
		return Provenance{}, refuse("provenance.authenticated: the proxy origin is " +
			"unauthenticated metadata")
	}
	values := map[string]string{}
	for _, key := range []string{"source", "vcs", "url", "subdir", "ref", "hash"} {
		raw, err := fieldOf(members, key, "provenance")
		if err != nil {
			return Provenance{}, err
		}
		text, err := exactString(raw, "provenance."+key)
		if err != nil {
			return Provenance{}, err
		}
		if err := rejectControlCharacters(text, "provenance."+key); err != nil {
			return Provenance{}, err
		}
		values[key] = text
	}
	return Provenance{Source: values["source"], Vcs: values["vcs"], URL: values["url"],
		Subdir: values["subdir"], Ref: values["ref"], Hash: values["hash"]}, nil
}

func parseSignature(node any) (SignatureEvidence, error) {
	members, err := requireKeys(node, signatureKeys, nil, "authentication.known_vcs_signatures")
	if err != nil {
		return SignatureEvidence{}, err
	}
	pathValue, err := fieldOf(members, "path", "signature")
	if err != nil {
		return SignatureEvidence{}, err
	}
	path, err := parseModulePath(pathValue, "signature.path")
	if err != nil {
		return SignatureEvidence{}, err
	}
	versionValue, err := fieldOf(members, "version", "signature")
	if err != nil {
		return SignatureEvidence{}, err
	}
	version, err := parseModuleVersion(versionValue, "signature.version")
	if err != nil {
		return SignatureEvidence{}, err
	}
	repositoryValue, err := fieldOf(members, "repository", "signature")
	if err != nil {
		return SignatureEvidence{}, err
	}
	repository, err := exactString(repositoryValue, "signature.repository")
	if err != nil {
		return SignatureEvidence{}, err
	}
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return SignatureEvidence{}, refuse("signature.repository: expected an HTTPS "+
			"repository URL: %s", repository)
	}
	refValue, err := fieldOf(members, "ref", "signature")
	if err != nil {
		return SignatureEvidence{}, err
	}
	ref, err := exactString(refValue, "signature.ref")
	if err != nil {
		return SignatureEvidence{}, err
	}
	if !strings.HasPrefix(ref, "refs/tags/") {
		return SignatureEvidence{}, refuse("signature.ref: expected a tag ref")
	}
	identities := map[string]string{}
	for _, key := range []string{"tag_object", "commit"} {
		raw, err := fieldOf(members, key, "signature")
		if err != nil {
			return SignatureEvidence{}, err
		}
		text, ok := raw.(string)
		if !ok || !objectIDPattern.MatchString(text) {
			return SignatureEvidence{}, refuse("signature.%s: expected a 40-hex object id", key)
		}
		identities[key] = text
	}
	signedValue, err := fieldOf(members, "signed_object", "signature")
	if err != nil {
		return SignatureEvidence{}, err
	}
	signedText, err := exactString(signedValue, "signature.signed_object")
	if err != nil {
		return SignatureEvidence{}, err
	}
	signed := SignedObject(signedText)
	if signed != SignedTag && signed != SignedCommit {
		return SignatureEvidence{}, refuse("signature.signed_object: unsupported signed object %s",
			repr(signedText))
	}
	formatValue, err := fieldOf(members, "format", "signature")
	if err != nil {
		return SignatureEvidence{}, err
	}
	format, err := exactString(formatValue, "signature.format")
	if err != nil {
		return SignatureEvidence{}, err
	}
	if format != "ssh" {
		return SignatureEvidence{}, refuse("signature.format: unsupported signature format %s",
			repr(format))
	}
	verified, err := fieldOf(members, "verified", "signature")
	if err != nil {
		return SignatureEvidence{}, err
	}
	if flag, ok := verified.(bool); !ok || flag {
		// This record is an availability inventory. Claiming verification here
		// would be a policy claim no code in this repository made.
		return SignatureEvidence{}, refuse("signature.verified: this repository does not " +
			"verify VCS signatures; record the finding under the linked follow-up instead")
	}
	return SignatureEvidence{ID: ModuleID{Path: path, Version: version}, Repository: repository,
		Ref: ref, TagObject: identities["tag_object"], Commit: identities["commit"],
		Signed: signed, Format: format}, nil
}

func parseReview(node any, root ModuleID, goMod, goSum SHA256) (Review, error) {
	members, err := requireKeys(node, authKeys, nil, "authentication")
	if err != nil {
		return Review{}, err
	}
	methodValue, err := fieldOf(members, "method", "authentication")
	if err != nil {
		return Review{}, err
	}
	method, err := exactString(methodValue, "authentication.method")
	if err != nil {
		return Review{}, err
	}
	if method != "go-checksum-database" {
		return Review{}, refuse("module-channel determination: unsupported authentication method")
	}
	sumdb, err := exactStringMember(members, "sumdb", "authentication")
	if err != nil {
		return Review{}, err
	}
	if sumdb != SumDB {
		return Review{}, refuse("module-channel determination: sumdb is not the reviewed " +
			"verifier key")
	}
	proxy, err := exactStringMember(members, "proxy", "authentication")
	if err != nil {
		return Review{}, err
	}
	if proxy != Proxy {
		return Review{}, refuse("module-channel determination: proxy is not the reviewed origin")
	}
	determination, err := exactStringMember(members, "determination", "authentication")
	if err != nil {
		return Review{}, err
	}
	if !contains(SupportedDeterminations, determination) {
		return Review{}, refuse("module-channel determination does not cover this closure: "+
			"unsupported determination %s", repr(determination))
	}
	scopeNode, err := fieldOf(members, "scope", "authentication")
	if err != nil {
		return Review{}, err
	}
	scope, err := requireKeys(scopeNode, scopeKeys, nil, "authentication.scope")
	if err != nil {
		return Review{}, err
	}
	scopeRoot, err := exactStringMember(scope, "root", "authentication.scope")
	if err != nil {
		return Review{}, err
	}
	if scopeRoot != root.String() {
		return Review{}, refuse("module-channel determination does not cover this closure: "+
			"scope root %s != %s", scopeRoot, root)
	}
	scopeGoModValue, err := fieldOf(scope, "go.mod", "authentication.scope")
	if err != nil {
		return Review{}, err
	}
	scopeGoMod, err := parseSHA256(scopeGoModValue, "authentication.scope.go.mod")
	if err != nil {
		return Review{}, err
	}
	scopeGoSumValue, err := fieldOf(scope, "go.sum", "authentication.scope")
	if err != nil {
		return Review{}, err
	}
	scopeGoSum, err := parseSHA256(scopeGoSumValue, "authentication.scope.go.sum")
	if err != nil {
		return Review{}, err
	}
	if scopeGoMod != goMod || scopeGoSum != goSum {
		return Review{}, refuse("module-channel determination does not cover this closure: " +
			"the reviewed go.mod/go.sum digests differ from the committed files")
	}
	reviewedOnValue, err := fieldOf(members, "reviewed_on", "authentication")
	if err != nil {
		return Review{}, err
	}
	reviewedOn, err := parseDate(reviewedOnValue, "authentication.reviewed_on")
	if err != nil {
		return Review{}, err
	}
	reviewer, err := exactStringMember(members, "reviewer", "authentication")
	if err != nil {
		return Review{}, err
	}
	if !reviewerPattern.MatchString(reviewer) {
		return Review{}, refuse("authentication.reviewer: expected a reviewer handle")
	}
	reason, err := exactStringMember(members, "reason", "authentication")
	if err != nil {
		return Review{}, err
	}
	reassess, err := exactStringMember(members, "reassess_when", "authentication")
	if err != nil {
		return Review{}, err
	}
	for _, value := range []string{reason, reassess} {
		if err := rejectControlCharacters(value, "authentication"); err != nil {
			return Review{}, err
		}
	}
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(reassess) == "" {
		return Review{}, refuse("module-channel determination: reason and reassess_when must " +
			"be recorded")
	}
	policy, err := exactStringMember(members, "vcs_signature_policy", "authentication")
	if err != nil {
		return Review{}, err
	}
	if !contains(SupportedSignaturePolicies, policy) {
		return Review{}, refuse("module-channel determination: unsupported VCS signature policy")
	}
	noticePolicy, err := exactStringMember(members, "notice_policy", "authentication")
	if err != nil {
		return Review{}, err
	}
	if !contains(SupportedNoticePolicies, noticePolicy) {
		return Review{}, refuse("module-channel determination: unsupported notice policy")
	}
	followUp, err := exactStringMember(members, "follow_up", "authentication")
	if err != nil {
		return Review{}, err
	}
	if followUp != AuthenticationIssue {
		return Review{}, refuse("module-channel determination: follow-up must be the filed " +
			"#94 sub-issue")
	}
	evidenceValue, err := fieldOf(members, "evidence", "authentication")
	if err != nil {
		return Review{}, err
	}
	evidence, err := stringList(evidenceValue, "authentication.evidence")
	if err != nil {
		return Review{}, err
	}
	if len(evidence) == 0 {
		return Review{}, refuse("module-channel determination: evidence must be recorded")
	}
	for _, entry := range evidence {
		parsed, err := url.Parse(entry)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
			return Review{}, refuse("authentication.evidence: expected an HTTPS URL: %s", entry)
		}
	}
	signaturesValue, err := fieldOf(members, "known_vcs_signatures",
		"authentication.known_vcs_signatures")
	if err != nil {
		return Review{}, err
	}
	items, err := exactList(signaturesValue, "authentication.known_vcs_signatures")
	if err != nil {
		return Review{}, err
	}
	signatures := make([]SignatureEvidence, 0, len(items))
	seen := map[ModuleID]struct{}{}
	for _, item := range items {
		signature, err := parseSignature(item)
		if err != nil {
			return Review{}, err
		}
		if _, duplicate := seen[signature.ID]; duplicate {
			return Review{}, refuse("authentication.known_vcs_signatures: duplicate %s",
				signature.ID)
		}
		seen[signature.ID] = struct{}{}
		signatures = append(signatures, signature)
	}
	return Review{ReviewedOn: reviewedOn, Reviewer: reviewer, Determination: determination,
		Reason: reason, Evidence: evidence, ReassessWhen: reassess,
		SignatureRule: policy, NoticeRule: noticePolicy, FollowUp: followUp,
		Signatures: signatures}, nil
}

// exactStringMember reads one required string member: a missing key is reported
// against the object, and a wrong type against the field, as the Python reader
// does.
func exactStringMember(members members, key, object string) (string, error) {
	value, err := fieldOf(members, key, object)
	if err != nil {
		return "", err
	}
	return exactString(value, object+"."+key)
}
