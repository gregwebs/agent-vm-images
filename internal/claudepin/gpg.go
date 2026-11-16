// Package claudepin's gpg boundary: one private keyring per invocation.
package claudepin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gpg-agent's control sockets are named after GNUPGHOME (`S.gpg-agent*`, up to 20
// more characters) inside sun_path, which is 104 bytes on macOS and 108 on Linux.
// TMPDIR is often a deep per-case scratch path, so the home lives under a fixed
// short root and its length is bounded here rather than left to a gpg failure.
var (
	// GPGHomeRoot is the one place a verification keyring is created. It is never
	// the caller's TMPDIR and never the user's keyring. It is a variable only so a
	// test can prove the refusal: nothing in the program assigns it.
	GPGHomeRoot = "/tmp"
)

const (
	// GPGHomePrefix names a keyring directory.
	GPGHomePrefix = "claude-pin-gpg."
	// MaxGPGHomeChars bounds the path before gpg-agent's socket would not fit.
	MaxGPGHomeChars = 64
)

// GPGHome is a private 0700 keyring for exactly one invocation, with the complete
// environment gpg sees: no ambient GNUPGHOME and no HOME keyring.
//
// The caller must call the returned function; it stops any agent started for this
// home and removes the home, and it reports a removal it could not do.
type GPGHome struct {
	// Path is the private directory.
	Path string
	// Environment is the complete environment for every gpg call in this home.
	Environment []string
	// Warnings receives a failure that is not itself a refusal.
	Warnings func(format string, args ...any)

	removed bool
}

// NewGPGHome creates the private keyring. There is no fallback location: an
// unusable root is a refusal.
func NewGPGHome(warn func(format string, args ...any)) (*GPGHome, error) {
	home, err := os.MkdirTemp(GPGHomeRoot, GPGHomePrefix)
	if err != nil {
		return nil, Refuse("cannot create a private gpg home under %s: %s", GPGHomeRoot, err)
	}
	if len(home) > MaxGPGHomeChars {
		_ = os.RemoveAll(home)
		return nil, Refuse("the private gpg home %s is too long for gpg-agent's socket", home)
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	if warn == nil {
		warn = func(string, ...any) {}
	}
	return &GPGHome{
		Path:        home,
		Environment: []string{"GNUPGHOME=" + home, "LC_ALL=C", "PATH=" + path},
		Warnings:    warn,
	}, nil
}

// Close stops the agent and removes the home. A failed removal after a successful
// verification is still a refusal: a keyring left behind is a keyring a later run
// could import into.
func (h *GPGHome) Close(cause error) error {
	if h.removed {
		return nil
	}
	h.removed = true
	h.killAgent()
	if err := os.RemoveAll(h.Path); err != nil {
		if cause != nil {
			h.Warnings("cannot remove the private gpg home %s: %s", h.Path, err)
			return nil
		}
		return Refuse("cannot remove the private gpg home %s: %s", h.Path, err)
	}
	return nil
}

// killAgent stops any agent this home started before the home disappears.
func (h *GPGHome) killAgent() {
	command := exec.Command("gpgconf", "--kill", "all")
	command.Env = h.Environment
	output, err := command.CombinedOutput()
	if err != nil {
		h.Warnings("cannot stop the gpg agent for %s: %s", h.Path, err)
		return
	}
	if len(strings.TrimSpace(string(output))) > 0 && command.ProcessState != nil &&
		!command.ProcessState.Success() {
		h.Warnings("gpgconf --kill all exited %d: %s", command.ProcessState.ExitCode(),
			strings.TrimSpace(string(output)))
	}
}

// GPGResult is one finished gpg call.
type GPGResult struct {
	Stdout string
	Stderr string
	Status int
}

// Run executes one gpg call in this home. The caller interprets every status: a
// non-zero exit is expected for a signature that does not verify.
func (h *GPGHome) Run(arguments ...string) (GPGResult, error) {
	command := exec.Command("gpg", arguments...)
	command.Env = h.Environment
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if _, ok := err.(*exec.Error); ok {
			return GPGResult{}, Refuse("gpg is required to verify the Claude manifest")
		}
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() < 0 {
			return GPGResult{}, Refuse("cannot run gpg: %s", err)
		}
	}
	status := 0
	if command.ProcessState != nil {
		status = command.ProcessState.ExitCode()
	}
	return GPGResult{Stdout: stdout.String(), Stderr: stderr.String(), Status: status}, nil
}

// MustRun is a gpg call whose non-zero status is itself a refusal, with the
// program's own detail: an unimported key would otherwise surface as a misleading
// "the signature does not verify".
func (h *GPGHome) MustRun(what string, arguments ...string) (GPGResult, error) {
	result, err := h.Run(arguments...)
	if err != nil {
		return GPGResult{}, err
	}
	if result.Status != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return GPGResult{}, Refuse("%s (gpg exit %d): %s", what, result.Status, detail)
	}
	return result, nil
}

// ParsePrimaries is the fingerprints of the key file's primary keys, in listing
// order.
//
// Only the `fpr` record that *follows a `pub` record* is a primary: the `fpr` after
// a `sub` record is a signing subkey, and a second primary would let an import add
// a signer the pin does not name. A `pub` record without its own `fpr` is refused
// rather than guessed at.
//
// A `sec`/`ssb` record is a refusal rather than a primary. `show-only` lists secret
// keys in their own records, so a key file that carried both the reviewed public
// key and any secret key would pass a `pub`-only count and then let the import
// install a second, unnamed signer; only the later VALIDSIG-primary comparison would
// stop a manifest signed by it. A committed release key never carries secret
// material, so this is a refusal and not a wider primary test.
func ParsePrimaries(colons string) ([]string, error) {
	primaries := []string{}
	pending := false
	for _, line := range strings.Split(colons, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		switch fields[0] {
		case "pub":
			if pending {
				return nil, Refuse("the release key file has a pub record without an fpr")
			}
			pending = true
		case "fpr":
			if !pending {
				continue
			}
			if len(fields) < 10 {
				return nil, Refuse("the release key file has an unreadable fpr record")
			}
			fingerprint := strings.ToUpper(fields[9])
			if !fingerprintPattern.MatchString(fingerprint) {
				return nil, Refuse("the release key file names the unusable fingerprint %q", fields[9])
			}
			primaries = append(primaries, fingerprint)
			pending = false
		case "sec", "ssb":
			return nil, Refuse("the release key file carries secret key material")
		case "sub", "uid", "uat":
			if pending {
				return nil, Refuse("the release key file has a pub record without an fpr")
			}
		}
	}
	if pending {
		return nil, Refuse("the release key file has a pub record without an fpr")
	}
	return primaries, nil
}

// KeyPrimaries asks gpg what primary keys a key file holds, without importing it.
func (h *GPGHome) KeyPrimaries(keyPath string) ([]string, error) {
	result, err := h.MustRun("cannot read the release key", "--batch", "--with-colons",
		"--import-options", "show-only", "--import", keyPath)
	if err != nil {
		return nil, err
	}
	return ParsePrimaries(result.Stdout)
}

// VerifySignature proves the detached signature over `manifest` was made by
// `fingerprint`'s key.
//
// Both the key file and the signed bytes are copied into the private home, so gpg
// never reads through a checkout path and a later swap cannot change what was
// verified.
func VerifySignature(key []byte, fingerprint string, manifest, signature []byte,
	warn func(format string, args ...any)) (Signature, error) {
	home, err := NewGPGHome(warn)
	if err != nil {
		return Signature{}, err
	}
	verified, verifyErr := verifyInHome(home, key, fingerprint, manifest, signature)
	if err := home.Close(verifyErr); err != nil && verifyErr == nil {
		return Signature{}, err
	}
	return verified, verifyErr
}

func verifyInHome(home *GPGHome, key []byte, fingerprint string, manifest,
	signature []byte) (Signature, error) {
	keyPath := filepath.Join(home.Path, "release-key.asc")
	manifestPath := filepath.Join(home.Path, "manifest.json")
	signaturePath := filepath.Join(home.Path, "manifest.json.sig")
	for path, raw := range map[string][]byte{
		keyPath:       key,
		manifestPath:  manifest,
		signaturePath: signature,
	} {
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return Signature{}, Refuse("cannot write the private verified copy %s: %s", path, err)
		}
	}
	primaries, err := home.KeyPrimaries(keyPath)
	if err != nil {
		return Signature{}, err
	}
	if len(primaries) != 1 || primaries[0] != fingerprint {
		return Signature{}, Refuse("the release key file must hold exactly the primary %s, found [%s]",
			fingerprint, strings.Join(primaries, " "))
	}
	if _, err := home.MustRun("cannot import the release key", "--batch", "--quiet",
		"--import", keyPath); err != nil {
		return Signature{}, err
	}
	status, err := home.Run("--batch", "--status-fd=1", "--verify", signaturePath, manifestPath)
	if err != nil {
		return Signature{}, err
	}
	verified, err := parseStatus(status, fingerprint)
	if err != nil {
		return Signature{}, Refuse("verify the manifest signature: %s", err)
	}
	return verified, nil
}

// parseStatus reads gpg's machine-readable status lines.
//
// VALIDSIG fields: [2] the signing key (a subkey, if one signed), [3] the signature
// date, and the last field the primary key it belongs to. The primary is the pinned
// identity, so a signature from a reviewed subkey is accepted and a pin that names a
// subkey is not.
func parseStatus(status GPGResult, fingerprint string) (Signature, error) {
	tokens := map[string]bool{}
	valid := [][]string{}
	for _, line := range strings.Split(status.Stdout, "\n") {
		if !strings.HasPrefix(line, "[GNUPG:] ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tokens[fields[1]] = true
		if fields[1] == "VALIDSIG" {
			valid = append(valid, fields)
		}
	}
	bad := tokens["BADSIG"] || tokens["ERRSIG"] || tokens["EXPSIG"] ||
		tokens["EXPKEYSIG"] || tokens["REVKEYSIG"]
	if status.Status != 0 || !tokens["GOODSIG"] || bad {
		detail := strings.TrimSpace(status.Stdout)
		if detail == "" {
			detail = strings.TrimSpace(status.Stderr)
		}
		return Signature{}, Refuse("the manifest signature does not verify: %s", detail)
	}
	if len(valid) != 1 || strings.ToUpper(valid[0][len(valid[0])-1]) != fingerprint {
		found := make([]string, 0, len(valid))
		for _, fields := range valid {
			found = append(found, strings.ToUpper(fields[len(fields)-1]))
		}
		return Signature{}, Refuse("the manifest signature is not by the primary %s; found [%s]",
			fingerprint, strings.Join(found, " "))
	}
	return Signature{
		PrimaryFingerprint: strings.ToUpper(valid[0][len(valid[0])-1]),
		Created:            valid[0][3],
	}, nil
}
