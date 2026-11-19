// Package pnpm installs the base image's reviewed pnpm distribution and
// certifies every published byte afterwards.
//
// The two directions are separate on purpose. Bootstrap is the only writer: it
// authenticates the pinned archive against the committed pin, extracts it into
// private scratch and publishes from there, and it refuses a destination that
// already exists. Verification reads the published tree and proves it is still
// the reviewed distribution, so a build cannot certify bytes that an
// unprivileged caller could replace later.
package pnpm

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Refusal is a pnpm install or certification this repository will not accept.
// Its message is the observable contract: the image's own certification log and
// the fixtures quote fragments of it.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

func refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// refuseText is the single-argument form, for a message the caller already built.
func refuseText(text string) error { return &Refusal{Message: text} }

// refuseCause reports an operating-system error as the message itself, which is
// what the shell diagnostics of this component have always shown.
func refuseCause(cause error) error { return &Refusal{Message: cause.Error()} }

// Pin is the reviewed selection and provenance record. Field names are the JSON
// member names, because the record is compared as a whole in two places.
type Pin struct {
	Version        string
	URL            string
	Integrity      string
	SHA512         string
	BundleSHA256   string
	UpstreamTag    string
	UpstreamCommit string
}

// pinFieldNames is the closed field set. A pin with a missing or extra member is
// a different record shape, not a newer one.
var pinFieldNames = []string{
	"version", "url", "integrity", "sha512", "bundleSha256", "upstreamTag", "upstreamCommit",
}

// stableVersion is an exact stable release: no range, no dist-tag, no
// pre-release suffix. npm serves a dist-tag from a mutable pointer, so only an
// exact version names the bytes the pin authenticated.
var stableVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// digestLengths is each hex digest's committed width.
var digestLengths = []struct {
	name   string
	length int
}{
	{name: "sha512", length: 128},
	{name: "bundleSha256", length: 64},
	{name: "upstreamCommit", length: 40},
}

var lowercaseHex = regexp.MustCompile(`^[0-9a-f]+$`)

// LoadPin reads and validates one pin document.
//
// The pin is the only input the bootstrap trusts, so every field is checked here
// rather than where it is used: a version that is not exact would make the URL
// ambiguous, an SRI that disagrees with the committed hex digest would let two
// consumers authenticate different bytes, and a tag that does not name the
// version would record provenance for another release.
func LoadPin(path string) (Pin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Pin{}, refuseCause(err)
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return Pin{}, refuse("cannot read %s: %s", path, err)
	}
	values, err := pinFields(document)
	if err != nil {
		return Pin{}, err
	}
	pin := Pin{
		Version:        values["version"],
		URL:            values["url"],
		Integrity:      values["integrity"],
		SHA512:         values["sha512"],
		BundleSHA256:   values["bundleSha256"],
		UpstreamTag:    values["upstreamTag"],
		UpstreamCommit: values["upstreamCommit"],
	}
	if !stableVersion.MatchString(pin.Version) {
		return Pin{}, refuseText("pin must be exact stable version")
	}
	if pin.URL != "https://registry.npmjs.org/pnpm/-/pnpm-"+pin.Version+".tgz" {
		return Pin{}, refuseText("unexpected archive URL")
	}
	for _, digest := range digestLengths {
		if value := values[digest.name]; len(value) != digest.length || !lowercaseHex.MatchString(value) {
			return Pin{}, refuseText("invalid digest/commit: " + digest.name)
		}
	}
	decoded, err := hex.DecodeString(pin.SHA512)
	if err != nil {
		return Pin{}, refuseText("invalid digest/commit: sha512")
	}
	if pin.Integrity != "sha512-"+base64.StdEncoding.EncodeToString(decoded) {
		return Pin{}, refuseText("SRI and hex digest disagree")
	}
	if pin.UpstreamTag != "v"+pin.Version {
		return Pin{}, refuseText("upstream tag disagrees")
	}
	return pin, nil
}

// pinFields enforces the closed shape and the string types before any field is
// interpreted.
func pinFields(document any) (map[string]string, error) {
	members, ok := document.(map[string]any)
	if !ok || len(members) != len(pinFieldNames) {
		return nil, refuseText("invalid pin fields")
	}
	values := make(map[string]string, len(members))
	for _, name := range pinFieldNames {
		member, present := members[name]
		if !present {
			return nil, refuseText("invalid pin fields")
		}
		text, ok := member.(string)
		if !ok {
			return nil, refuseText("pin fields must be strings")
		}
		values[name] = text
	}
	return values, nil
}

// PinVersion reads the selection one pin document names.
//
// This is a field reader, not the validator: the dsh hooks compare this value with
// the selection their manifest asserts, and a fixture's confined base-pin record
// carries nothing but the version. Authentication belongs to the callers of
// LoadPin, which is what actually accepts or refuses a pin.
func PinVersion(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", refuseCause(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", refuse("cannot read %s: %s", path, err)
	}
	version, ok := document["version"].(string)
	if !ok {
		return "", refuseText("invalid pin fields")
	}
	return version, nil
}

// Sum256 and Sum512 are the committed digest spellings: lowercase hex, no
// separator, which is what the pin records and what every consumer compares.
func Sum256(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func Sum512(payload []byte) string {
	sum := sha512.Sum512(payload)
	return hex.EncodeToString(sum[:])
}

// containsString reports whether values holds value.
func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// withEnvironment returns entries with the named variables replaced or removed,
// so a child process sees one decided environment instead of an inherited one. A
// nil replacement removes the variable, and a variable the caller's environment
// did not carry is still set.
func withEnvironment(entries []string, overrides map[string]*string) []string {
	applied := make(map[string]bool, len(overrides))
	result := make([]string, 0, len(entries)+len(overrides))
	for _, entry := range entries {
		name, _, _ := strings.Cut(entry, "=")
		replacement, replaced := overrides[name]
		if !replaced {
			result = append(result, entry)
			continue
		}
		applied[name] = true
		if replacement != nil {
			result = append(result, name+"="+*replacement)
		}
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if applied[name] || overrides[name] == nil {
			continue
		}
		result = append(result, name+"="+*overrides[name])
	}
	return result
}

// textValue returns a pointer to text, for withEnvironment.
func textValue(text string) *string { return &text }
