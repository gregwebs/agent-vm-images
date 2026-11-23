// Package gpg runs the real gpg in a keyring private to one invocation, and
// reads the machine-readable verdict from its status lines.
//
// There is no bypass mode. A signature is authenticated only when gpg exits zero
// *and* every status line agrees: the exit status alone, or a VALIDSIG line
// alone, is not a verdict. The argv, the child environment and the keyring's
// permissions are byte-for-byte the source reader's, because the differential
// oracle compares gpg's transcript between the two implementations.
package gpg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/strictjson"
)

// ImagesPath is the tail of the child's PATH: the image's own directories. A
// resolved gpg keeps its own directory in front of them.
const ImagesPath = "/usr/bin:/bin"

// Result is one finished child process.
type Result struct {
	Stdout string
	Stderr string
	Status int
}

// Program is an absolute path to gpg.
type Program string

// Refusal is a signature, key or child-process failure.
type Refusal struct{ Message string }

func (r *Refusal) Error() string { return r.Message }

func refuse(format string, arguments ...any) error {
	return &Refusal{Message: fmt.Sprintf(format, arguments...)}
}

// Resolve finds gpg the way the source reader does: PATH first, then
// /usr/bin/gpg. The result must be absolute and executable, and exec.LookPath
// refuses a dot-relative answer, so the program a build runs is never a file in
// the working directory.
func Resolve() (Program, error) {
	found, err := exec.LookPath("gpg")
	if err != nil {
		found = "/usr/bin/gpg"
	}
	return NewProgram(found)
}

// NewProgram validates an absolute executable path.
func NewProgram(path string) (Program, error) {
	if !filepath.IsAbs(path) {
		return "", refuse("gpg is not an executable absolute path: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&0o111 == 0 || info.IsDir() {
		return "", refuse("gpg is not an executable absolute path: %s", path)
	}
	return Program(path), nil
}

// Policy is the reviewed trust: exactly one primary key and the signers a
// manifest may use.
type Policy struct {
	Primary string
	Signers []string
}

// Exec is the one process seam this package has. Production passes ExecProcess;
// a unit test passes a recorder. A prefix wrapper (the differential suite's
// recording gpg) is a *program*, not a seam: it is reached through PATH.
type Exec func(ctx context.Context, argv []string, env []string) (Result, error)

// ExecProcess runs one child with exactly env and no inherited variable.
func ExecProcess(ctx context.Context, argv []string, env []string) (Result, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = env
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.Status = exit.ExitCode()
			return result, nil
		}
		// The program could not start at all. The source reader raises here (a
		// reported P1 divergence); this reports a refusal with the same status.
		return result, err
	}
	return result, nil
}

// Verifier is one private keyring: import the committed key, then verify.
type Verifier struct {
	program Program
	keyFile string
	work    string
	home    string
	policy  Policy
	exec    Exec
	loaded  bool
}

// NewVerifier builds a verifier whose keyring lives in work (mode 0700), which
// the caller owns and removes on every return path.
func NewVerifier(program Program, keyFile string, work string, policy Policy) *Verifier {
	return &Verifier{program: program, keyFile: keyFile, work: work,
		home: filepath.Join(work, "gnupg"), policy: policy, exec: ExecProcess}
}

// WithExec replaces the process seam. Every other property stays.
func (v *Verifier) WithExec(exec Exec) *Verifier {
	v.exec = exec
	return v
}

// environment is the child environment: exactly these five variables, so an
// ambient GNUPGHOME, LC_ALL or TMPDIR cannot reach the verification.
func (v *Verifier) environment() []string {
	return []string{
		"PATH=" + filepath.Dir(string(v.program)) + ":" + ImagesPath,
		"GNUPGHOME=" + v.home,
		"HOME=" + filepath.Dir(v.home),
		"LC_ALL=C",
		"TMPDIR=" + filepath.Dir(v.home),
	}
}

// run executes one gpg call and applies the source reader's success rule:
// status 0, or a refusal naming the operation and the last line of output.
func (v *Verifier) run(ctx context.Context, what string, arguments ...string) (Result, error) {
	argv := append([]string{string(v.program), "--batch", "--no-options"}, arguments...)
	result, err := v.exec(ctx, argv, v.environment())
	if err != nil {
		return result, refuse("%s failed: %v", what, err)
	}
	if result.Status != 0 {
		return result, refuse("%s failed with status %d: %s", what, result.Status,
			tail(result))
	}
	return result, nil
}

// tail is the last non-empty line of stderr, else of stdout, else "no output".
func tail(result Result) string {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	lines := splitLines(detail)
	if len(lines) == 0 {
		return "no output"
	}
	return lines[len(lines)-1]
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// ensureKey imports the committed key file and requires the keyring to hold
// exactly the reviewed primary and every reviewed signer.
func (v *Verifier) ensureKey(ctx context.Context) error {
	if v.loaded {
		return nil
	}
	// One rule for the keyring's mode: MkdirAll creates it 0700. A second chmod
	// would be a rule with no caller that could fail.
	if err := os.MkdirAll(v.home, 0o700); err != nil {
		return refuse("trust root: cannot create the keyring: %v", err)
	}
	if _, err := v.run(ctx, "trust root: gpg --import", "--import", v.keyFile); err != nil {
		return err
	}
	listing, err := v.run(ctx, "trust root: gpg --list-keys", "--with-colons",
		"--fingerprint", "--list-keys")
	if err != nil {
		return err
	}
	primaries, signers := ParsePrimaries(listing.Stdout)
	if len(primaries) != 1 || primaries[0] != v.policy.Primary {
		return refuse("trust root: key file must hold exactly the reviewed primary key %s",
			v.policy.Primary)
	}
	have := make(map[string]bool, len(signers)+1)
	for _, signer := range signers {
		have[signer] = true
	}
	have[v.policy.Primary] = true
	for _, signer := range v.policy.Signers {
		if !have[signer] {
			return refuse("trust root: key file lacks reviewed signing key %s", signer)
		}
	}
	v.loaded = true
	return nil
}

// ParsePrimaries reads `gpg --with-colons --fingerprint --list-keys`: the
// primaries of the `pub` records and the signing keys of the `sub` records,
// in listing order.
func ParsePrimaries(listing string) (primaries []string, signers []string) {
	pending := ""
	seen := make(map[string]bool)
	for _, line := range strings.Split(listing, "\n") {
		record := strings.Split(line, ":")
		switch {
		case record[0] == "pub" || record[0] == "sub":
			pending = record[0]
		case record[0] == "fpr" && pending != "" && len(record) > 9:
			if pending == "pub" {
				primaries = append(primaries, record[9])
			} else if !seen[record[9]] {
				seen[record[9]] = true
				signers = append(signers, record[9])
			}
			pending = ""
		}
	}
	return primaries, signers
}

// Verify authenticates data against signature. Both the exit status and the
// status lines must agree, and every signer must be a reviewed key.
func (v *Verifier) Verify(ctx context.Context, data, signature, what string) error {
	if err := v.ensureKey(ctx); err != nil {
		return err
	}
	result, err := v.run(ctx, what+" check", "--status-fd", "1", "--verify", signature, data)
	if err != nil {
		return err
	}
	signers, err := ParseStatus(result.Stdout, what)
	if err != nil {
		return err
	}
	for _, signer := range signers {
		if !v.allows(signer) {
			return refuse("%s: signer %s is not a reviewed signing key", what, signer)
		}
	}
	return nil
}

func (v *Verifier) allows(signer string) bool {
	for _, allowed := range v.policy.Signers {
		if allowed == signer {
			return true
		}
	}
	return false
}

// RejectedStatuses are the gpg keywords that mean the machine-readable verdict
// was not a good signature. EXPSIG/SIGEXPIRED are an expired *signature* (gpg
// emits a VALIDSIG line and exits nonzero for them); FAILURE is gpg's own final
// refusal.
var RejectedStatuses = []string{"BADSIG", "ERRSIG", "NO_PUBKEY", "REVKEYSIG",
	"EXPKEYSIG", "EXPSIG", "SIGEXPIRED", "FAILURE", "NODATA"}

// ParseStatus returns the uppercased VALIDSIG fingerprints of a status stream,
// refusing a rejected keyword, a malformed VALIDSIG or no VALIDSIG at all.
func ParseStatus(status string, what string) ([]string, error) {
	var signers []string
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "[GNUPG:] ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, refuse("%s: malformed gpg status line", what)
		}
		keyword := fields[1]
		for _, rejected := range RejectedStatuses {
			if keyword == rejected {
				return nil, refuse("%s: signature rejected: %s", what, keyword)
			}
		}
		if keyword != "VALIDSIG" {
			continue
		}
		if len(fields) < 3 {
			return nil, refuse("%s: malformed VALIDSIG status", what)
		}
		signers = append(signers, strings.ToUpper(fields[2]))
	}
	if len(signers) == 0 {
		return nil, refuse("%s: no VALIDSIG status line", what)
	}
	return signers, nil
}

// WorkDirectory creates the invocation-private scratch directory and its keyring
// with the source reader's names and modes: `rust-verify-*` under TMPDIR, 0700,
// with `gnupg/` inside.
func WorkDirectory() (string, error) {
	work, err := os.MkdirTemp("", "rust-verify-")
	if err != nil {
		return "", refuse("cannot create the verification keyring: %v", err)
	}
	if err := os.Chmod(work, 0o700); err != nil {
		_ = os.RemoveAll(work)
		return "", refuse("cannot protect the verification keyring: %v", err)
	}
	return work, nil
}

// Repr is Python's repr, for a diagnostic that names a value.
func Repr(text string) string { return strictjson.Repr(text) }
