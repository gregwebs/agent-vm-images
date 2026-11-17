package codewriter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
)

// verifierTimeout is the one bound on a host verifier run.
const verifierTimeout = 300 * time.Second

// commandResult is one finished host-tool run.
type commandResult struct {
	stdout string
	stderr string
	code   int
}

// verifier is the payload/bundle verifier this run will use: the injected seam when a
// suite provided one, otherwise the reviewed cosign at exactly the reviewed version.
func (c *Client) verifier(requested string) (BlobVerifier, error) {
	if c.Verify != nil {
		return c.Verify, nil
	}
	path := requested
	if path == "" {
		found, ok := lookPath("cosign", c.searchPath())
		if !ok {
			return nil, refusal("the reviewed cosign is required for a write run; install "+
				"%s or pass --cosign /absolute/path", codexpin.VerifierVersion)
		}
		path = found
	}
	cosign, err := c.requireCosign(path)
	if err != nil {
		return nil, err
	}
	return c.cosignVerifier(cosign), nil
}

// requireCosign is the reviewed host verifier, at exactly the reviewed version.
//
// cosign is a maintainer prerequisite like gpg: not an image input, not downloaded by
// this tool and not replaced by a different version. A failed or unparseable version
// report is a refusal, never a reason to continue with a different trust story.
func (c *Client) requireCosign(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", refusal("--cosign must be an absolute path, got %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", refusal("%s is not an executable file", path)
	}
	result, err := c.runBounded([]string{path, "version", "--json"},
		[]string{"PATH=" + c.searchPath(), "LC_ALL=C"}, string(filepath.Separator), "cosign version")
	if err != nil {
		return "", err
	}
	if result.code != 0 {
		return "", refusal("%s version --json failed (%d)", path, result.code)
	}
	reported, ok := gitVersion(result.stdout)
	if !ok {
		return "", refusal("%s version --json did not report a gitVersion", path)
	}
	if reported != codexpin.VerifierVersion {
		return "", refusal("%s is %s, but the reviewed verifier is %s", path, reported,
			codexpin.VerifierVersion)
	}
	return path, nil
}

// gitVersion is the `gitVersion` a `cosign version --json` report carries.
func gitVersion(stdout string) (string, bool) {
	var report map[string]any
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		return "", false
	}
	version, ok := report["gitVersion"].(string)
	return version, ok
}

// cosignVerifier is the exact reviewed command line, with every default security check
// on.
//
// No `--insecure-ignore-tlog`, no `--insecure-ignore-sct`, no identity regexp, no bare
// public key and no rewritten bundle: certificate chain, SCT and Rekor verification
// stay at their defaults, and the signer is pinned to the exact workflow and issuer the
// schema records.
func (c *Client) cosignVerifier(cosign string) BlobVerifier {
	return func(archive, bundle, identity, issuer, work string) error {
		environment, err := c.cosignEnvironment(work)
		if err != nil {
			return err
		}
		argv := []string{cosign, "verify-blob", "--bundle", bundle,
			"--certificate-identity", identity, "--certificate-oidc-issuer", issuer, archive}
		name := filepath.Base(archive)
		result, err := c.runBounded(argv, environment, work, name)
		if err != nil {
			return err
		}
		if result.code == 0 {
			return nil
		}
		detail := strings.TrimSpace(result.stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.stdout)
		}
		return refusal("%s: cosign refused the committed bundle (%d): %s", name, result.code,
			detail)
	}
}

// cosignEnvironment is the complete environment a pin-time cosign run may see.
//
// Nothing is inherited except the executable search path: an ambient
// `SIGSTORE_*`/`COSIGN_*` override, a token, a keyring pointer or proxy configuration
// could silently change what "verified" means, so this constructed mapping is the whole
// environment. The private HOME and XDG cache are what keep the TUF trust root
// genuinely invocation-private.
func (c *Client) cosignEnvironment(work string) ([]string, error) {
	home := filepath.Join(work, "home")
	cache := filepath.Join(work, "cache")
	for _, directory := range []string{home, cache} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, refusal("cannot create %s: %s", directory, err)
		}
	}
	return []string{
		"PATH=" + c.searchPath(),
		"HOME=" + home,
		"XDG_CACHE_HOME=" + cache,
		"LC_ALL=C",
	}, nil
}

// runBounded runs a host tool with a process-group kill on timeout.
//
// The child gets its own process group, so a hung verifier and anything it spawned are
// signalled together; killing only the child would leave grandchildren behind.
func (c *Client) runBounded(argv []string, environment []string, work, label string) (commandResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), verifierTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = work
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String()}
	if command.ProcessState != nil {
		result.code = command.ProcessState.ExitCode()
	}
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return commandResult{}, refusal("%s: the verifier did not finish within %s",
			label, verifierTimeout)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return commandResult{}, refusal("%s: cannot run %s: %s", label, argv[0], err)
	}
	return result, nil
}

// lookPath is `shutil.which`: an absolute or slash-bearing name is checked directly,
// anything else is searched in this process's recorded PATH.
func lookPath(name, path string) (string, bool) {
	if strings.ContainsRune(name, filepath.Separator) {
		return name, true
	}
	for _, directory := range strings.Split(path, string(os.PathListSeparator)) {
		if directory == "" {
			continue
		}
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() &&
			info.Mode().Perm()&0o111 != 0 {
			return candidate, true
		}
	}
	return "", false
}
