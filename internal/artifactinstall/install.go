// Package artifactinstall installs one committed release-artifact pin into an
// image root.
//
// The installer is the only path that puts Node, the GitHub CLI or `agy` into the
// image. It reads nothing but the pin, so an ordinary build cannot resolve a URL,
// a checksum or a version of its own: the pinned URL is the only place it fetches
// from, the pinned digest is the only bytes it accepts, and there is no apt, npm,
// mirror or latest fallback.
//
// Three orderings are load-bearing:
//
//   - The reviewed evidence is checked before the payload is fetched. For Node
//     that means the detached OpenPGP signature over SHASUMS256.txt against the
//     committed release key, and the signed line for this archive must equal the
//     committed digest; for `agy`, whose upstream publishes no signature, it means
//     the two committed corroboration records must still agree with the pin. Only
//     then is the tarball requested at all.
//   - Nothing reaches the image until the archive has been preflighted, extracted
//     into a private staging tree and published from there. A hostile member, a
//     duplicate path, an escaping link, a device node, a setuid bit, a second
//     member or a binary for another architecture stops the run with the
//     destination untouched.
//   - `agy`'s archive carries exactly one prefixless binary that the image
//     publishes under another name, so the archive's shape is part of the pin and
//     is checked from the member metadata before any byte is decoded into staging.
package artifactinstall

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/fetch"
)

// Refusal is an install this repository will not act on. Its message is the
// contract: the suites match fragments of it.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

func refuse(format string, args ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, args...)}
}

// MaxEvidenceBytes bounds one checksums manifest or signature.
const MaxEvidenceBytes = 1 << 20

// Options is one install.
type Options struct {
	Pin      artifactpin.Pin
	Arch     string
	Root     string
	Events   *Events
	Stderr   io.Writer
	Fetch    *fetch.Client
	Compress string // the xz command; empty means "xz"
}

// Install installs the pin's archive for one architecture into the image root.
func Install(ctx context.Context, options Options) error {
	platform, ok := platformForArch(options.Arch)
	if !ok {
		return refuse("%s: unsupported architecture %q", options.Pin.Name, options.Arch)
	}
	pin := options.Pin
	archive, err := pin.Archive(platform)
	if err != nil {
		return err
	}
	options.report(fmt.Sprintf("%s %s: %s for %s", pin.Name, pin.Version, archive.Filename(), platform))
	scratch, err := os.MkdirTemp(os.Getenv("TMPDIR"), "install-"+pin.Name.String()+".")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	staging := filepath.Join(scratch, "staging")
	if err := os.Mkdir(staging, 0o750); err != nil {
		return err
	}
	client := options.Fetch
	if client == nil {
		client = fetch.New()
	}
	if options.Events != nil {
		client.Log = options.Events.Fetch
	}
	if err := checkEvidence(ctx, client, pin, archive, scratch, options.Events); err != nil {
		return err
	}
	payload, err := client.Get(ctx, archive.URL, artifactpin.MaxArchiveBytes(pin.Name))
	if err != nil {
		return err
	}
	if actual := artifactpin.Sum256(payload); actual != archive.Digest {
		return refuse("%s is %s, the pin says %s", archive.Filename(), actual, archive.Digest)
	}
	if archive.SHA512 != "" && Sum512(payload) != archive.SHA512 {
		return refuse("%s does not match the corroborated sha512", archive.Filename())
	}
	archiveFile := filepath.Join(scratch, archive.Filename())
	if err := os.WriteFile(archiveFile, payload, 0o600); err != nil {
		return err
	}
	if err := extractArchive(options, archiveFile, archive, staging); err != nil {
		return err
	}
	if err := checkExecutable(pin, platform, staging); err != nil {
		return err
	}
	if pin.Bundled != nil {
		found, err := bundledNPM(staging)
		if err != nil {
			return err
		}
		if recorded := pin.Bundled["npm"]; found != recorded.String() {
			return refuse("the archive ships npm %s, the pin records %s", found, recorded)
		}
	}
	if authentication := pin.Authentication.Sigstore; authentication != nil {
		// The verifier is the artifact, so it runs from the staged tree *before*
		// anything reaches the image: a failed attestation must not leave a
		// published CLI behind.
		verifier := filepath.Join(staging, "bin", pin.Name.String())
		if info, err := os.Stat(verifier); err != nil || !info.Mode().IsRegular() {
			return refuse("the reviewed archive carries no bin/%s to verify", pin.Name)
		}
		var bundle string
		if archive.Bundle != nil {
			bundle, err = artifactpin.CheckEvidence(pin, archive.Bundle.Path, archive.Bundle.Digest)
			if err != nil {
				return err
			}
		}
		if err := verifyAttestation(authentication, archiveFile, bundle, verifier); err != nil {
			return err
		}
		if options.Events != nil {
			options.Events.Verify("attestation")
		}
	}
	plan, err := planPublication(staging, options.Root, pin.Install.Destination, pin.Install.Payload)
	if err != nil {
		return err
	}
	files, directories, err := publish(options.Root, plan)
	if err != nil {
		return err
	}
	if options.Events != nil {
		options.Events.Publish(len(files))
	}
	var linked []string
	for _, target := range pin.Install.Links {
		path, err := link(rootPath(options.Root), target.Path, target.Target)
		if err != nil {
			for index := len(linked) - 1; index >= 0; index-- {
				_ = os.Remove(linked[index])
			}
			rollback(files, directories)
			return err
		}
		linked = append(linked, path)
	}
	return nil
}

func (o Options) report(message string) {
	if o.Stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(o.Stderr, "install-artifact: %s\n", message)
}

// Report writes the closing line a successful install leaves behind.
func (o Options) Report() {
	o.report(fmt.Sprintf("%s %s installed for %s", o.Pin.Name, o.Pin.Version, o.Arch))
}

func platformForArch(arch string) (artifactpin.Platform, bool) {
	for _, platform := range artifactpin.Platforms {
		if platform.DockerArch() == arch {
			return platform, true
		}
	}
	return "", false
}

// checkEvidence verifies the authentication evidence before any archive byte is
// requested.
func checkEvidence(ctx context.Context, client *fetch.Client, pin artifactpin.Pin,
	archive artifactpin.Archive, scratch string, events *Events) error {
	switch {
	case pin.Authentication.OpenPGP != nil:
		authentication := pin.Authentication.OpenPGP
		key, err := checkKeyEvidence(pin, authentication)
		if err != nil {
			return err
		}
		manifest, err := checkManifest(ctx, client, authentication)
		if err != nil {
			return err
		}
		signature, err := checkSignature(ctx, client, authentication)
		if err != nil {
			return err
		}
		if err := verifySignature(authentication, key, manifest, signature, scratch); err != nil {
			return err
		}
		digest, err := artifactpin.SignedDigest(manifest, archive.Filename())
		if err != nil {
			return refuse("the signed manifest does not carry the committed digest for %s",
				archive.Filename())
		}
		if digest != archive.Digest {
			return refuse("the signed manifest does not carry the committed digest for %s",
				archive.Filename())
		}
	case pin.Authentication.Sigstore != nil:
		if archive.Bundle == nil {
			return refuse("%s: the pin carries no attestation bundle", pin.Name)
		}
		if _, err := artifactpin.CheckEvidence(pin, archive.Bundle.Path, archive.Bundle.Digest); err != nil {
			return err
		}
	case pin.Authentication.HashOnly != nil:
		// No signature exists upstream: the build's only authority is the two
		// committed channels, which must still agree with each other and the pin.
		if err := artifactpin.CheckCorroboration(pin); err != nil {
			return err
		}
		if events != nil {
			events.Verify("corroboration")
		}
	}
	return nil
}

func checkKeyEvidence(pin artifactpin.Pin, authentication *artifactpin.OpenPGP) (string, error) {
	key, err := artifactpin.CheckEvidence(pin, authentication.KeyPath, authentication.KeyDigest)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(key)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(string(raw), "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		return "", refuse("%s: %s is not an ASCII-armoured key", pin.Name, authentication.KeyPath)
	}
	return key, nil
}

func checkManifest(ctx context.Context, client *fetch.Client,
	authentication *artifactpin.OpenPGP) ([]byte, error) {
	manifest, err := client.Get(ctx, authentication.ManifestURL, MaxEvidenceBytes)
	if err != nil {
		return nil, err
	}
	if actual := artifactpin.Sum256(manifest); actual != authentication.ManifestDigest {
		return nil, refuse("%s does not match the committed sha256", authentication.ManifestURL)
	}
	return manifest, nil
}

func checkSignature(ctx context.Context, client *fetch.Client,
	authentication *artifactpin.OpenPGP) ([]byte, error) {
	signature, err := client.Get(ctx, authentication.SignatureURL, MaxEvidenceBytes)
	if err != nil {
		return nil, err
	}
	if actual := artifactpin.Sum256(signature); actual != authentication.SignatureDigest {
		return nil, refuse("%s does not match the committed sha256", authentication.SignatureURL)
	}
	return signature, nil
}

// verifySignature imports the committed key into a private GNUPGHOME and requires
// the detached signature to verify under the pinned fingerprint.
func verifySignature(authentication *artifactpin.OpenPGP, key string, manifest, signature []byte,
	scratch string) error {
	home := filepath.Join(scratch, "gnupg")
	if err := os.Mkdir(home, 0o700); err != nil {
		return err
	}
	manifestFile := filepath.Join(scratch, "SHASUMS256.txt")
	signatureFile := manifestFile + ".sig"
	if err := os.WriteFile(manifestFile, manifest, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(signatureFile, signature, 0o600); err != nil {
		return err
	}
	environment := []string{"GNUPGHOME=" + home, "PATH=" + pathEnvironment(), "LC_ALL=C"}
	if err := runCommand("gpg", environment, "--batch", "--quiet", "--import", key); err != nil {
		return err
	}
	status, err := runCommandOutput("gpg", environment, "--batch", "--status-fd=1", "--verify",
		signatureFile, manifestFile)
	if err != nil {
		return err
	}
	if !status.ok {
		return refuse("the manifest signature does not verify: %s", strings.TrimSpace(status.stdout))
	}
	// VALIDSIG carries the signing key and, last, the primary key it belongs to: a
	// signing subkey is still the pinned identity.
	signers := map[string]bool{}
	for _, line := range strings.Split(status.stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !contains(fields, "VALIDSIG") {
			continue
		}
		// The status line is `[GNUPG:] VALIDSIG <fpr> ... <primary>`.
		for _, field := range append([]string{fields[2]}, fields[len(fields)-1]) {
			if len(field) == 40 || len(field) == 64 {
				signers[strings.ToUpper(field)] = true
			}
		}
	}
	if !signers[authentication.KeyFingerprint] {
		return refuse("the manifest was signed by %s, not the pinned %s",
			describeSigners(signers), authentication.KeyFingerprint)
	}
	return nil
}

func describeSigners(signers map[string]bool) string {
	names := make([]string, 0, len(signers))
	for name := range signers {
		names = append(names, "'"+name+"'")
	}
	if len(names) == 0 {
		return "no key"
	}
	sortStrings(names)
	return "[" + strings.Join(names, ", ") + "]"
}

// verifyAttestation binds the committed bundle to the committed archive with the
// installed GitHub CLI, which is the only verifier that can read those bundles.
func verifyAttestation(authentication *artifactpin.Sigstore, archive, bundle, verifier string) error {
	policy := authentication.Policy
	arguments := []string{"attestation", "verify", archive, "--bundle", bundle,
		"--repo", policy.Repository,
		"--signer-workflow", policy.SignerWorkflow,
		"--cert-oidc-issuer", policy.CertOIDCIssuer}
	if policy.DenySelfHostedRunners {
		arguments = append(arguments, "--deny-self-hosted-runners")
	}
	status, err := runCommandOutputPath(verifier, verifierEnvironment(), arguments...)
	if err != nil {
		return err
	}
	if !status.ok {
		detail := status.stderr
		if strings.TrimSpace(detail) == "" {
			detail = status.stdout
		}
		return refuse("the committed attestation bundle does not verify the archive: %s",
			strings.TrimSpace(detail))
	}
	return nil
}

// verifierEnvironment drops every credential: --bundle reads only the committed
// bytes, so the token variables are removed rather than trusted.
func verifierEnvironment() []string {
	dropped := map[string]bool{"GH_TOKEN": true, "GITHUB_TOKEN": true,
		"GH_ENTERPRISE_TOKEN": true, "GH_HOST": true}
	environment := make([]string, 0, len(os.Environ())+1)
	home := false
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if dropped[name] {
			continue
		}
		if name == "HOME" {
			home = true
		}
		environment = append(environment, entry)
	}
	if !home {
		environment = append(environment, "HOME="+os.TempDir())
	}
	return environment
}

func pathEnvironment() string {
	if value := os.Getenv("PATH"); value != "" {
		return value
	}
	return "/usr/bin:/bin"
}

type commandStatus struct {
	ok     bool
	stdout string
	stderr string
}

func runCommand(name string, environment []string, arguments ...string) error {
	command := exec.Command(name, arguments...) //nolint:gosec // the argv is the reviewed verifier call.
	command.Env = environment
	if output, err := command.CombinedOutput(); err != nil {
		return refuse("%s failed (%v): %s", strings.Join(append([]string{name}, arguments...), " "),
			err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runCommandOutput(name string, environment []string, arguments ...string) (commandStatus, error) {
	return runCommandStatus(name, func(command *exec.Cmd) { command.Env = environment }, arguments...)
}

func runCommandOutputPath(path string, environment []string, arguments ...string) (commandStatus, error) {
	return runCommandStatus(path, func(command *exec.Cmd) { command.Env = environment }, arguments...)
}

func runCommandStatus(name string, prepare func(*exec.Cmd), arguments ...string) (commandStatus, error) {
	command := exec.Command(name, arguments...) //nolint:gosec // the argv is the reviewed verifier call.
	prepare(command)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	status := commandStatus{ok: err == nil, stdout: stdout.String(), stderr: stderr.String()}
	var exit *exec.ExitError
	if err != nil && !asExitError(err, &exit) {
		return status, refuse("%s failed: %s", name, err)
	}
	return status, nil
}

func asExitError(err error, target **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError) //nolint:errorlint // exec returns the concrete type.
	if ok {
		*target = exit
	}
	return ok
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for inner := index; inner > 0 && values[inner] < values[inner-1]; inner-- {
			values[inner], values[inner-1] = values[inner-1], values[inner]
		}
	}
}
