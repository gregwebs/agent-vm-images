package artifactpin

import (
	"regexp"
	"strings"
)

// Member ceilings, link resolution and payload comparison are shared with the
// installer and the writer, so they live beside the model instead of in either.

var datePattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

func parseDate(value, what string) error {
	if !datePattern.MatchString(value) {
		return refuse("%s: expected YYYY-MM-DD, got %q", what, value)
	}
	return nil
}

func dateMember(value object, what string) (string, error) {
	text, err := requireText(value, what)
	if err != nil {
		return "", err
	}
	if err := parseDate(text, what); err != nil {
		return "", err
	}
	return text, nil
}

func digestMember(value object, what string) (Digest, error) {
	text, err := requireText(value, what)
	if err != nil {
		return "", err
	}
	return ParseDigest(text, what)
}

func relativeMember(value object, what string) (RelPath, error) {
	text, err := requireText(value, what)
	if err != nil {
		return "", err
	}
	return parseRelPath(text, what)
}

func sourceURLText(value object, what string) (string, error) {
	text, err := requireText(value, what)
	if err != nil {
		return "", err
	}
	return sourceURL(text, what)
}

func samePaths(left []RelPath, right []RelPath) bool {
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

func sameMembers(left, right []Member) bool {
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

func sameLinks(left, right []Link) bool {
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

// describe renders a payload or link list the way the refusal reads it.
func describe(members []Member) string {
	parts := make([]string, 0, len(members))
	for _, member := range members {
		parts = append(parts, "('"+member.Archive.String()+"', '"+member.Published.String()+"')")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func describePaths(paths []RelPath) string {
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		parts = append(parts, "'"+path.String()+"'")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func describeLinks(links []Link) string {
	parts := make([]string, 0, len(links))
	for _, link := range links {
		parts = append(parts, "('"+link.Path.String()+"', '"+link.Target.String()+"')")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func describePolicy(policy Policy) string {
	return "{'repository': '" + policy.Repository +
		"', 'signer_workflow': '" + policy.SignerWorkflow +
		"', 'cert_oidc_issuer': '" + policy.CertOIDCIssuer +
		"', 'deny_self_hosted_runners': " + boolText(policy.DenySelfHostedRunners) + "}"
}

func boolText(value bool) string {
	if value {
		return "True"
	}
	return "False"
}
