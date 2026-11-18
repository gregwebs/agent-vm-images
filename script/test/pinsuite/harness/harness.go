// Package harness is the fixture and process plumbing shared by the black-box
// pin suites under script/test/pinsuite.
//
// It exists so a suite can do what the Python suites it replaces did: build a
// fixture closure by hand, drive the real entry point as a child process, and
// assert on the exit status and on the bytes that survive. Nothing here imports a
// package under test. A suite must be able to fail because the program is wrong,
// and must never pass because a helper agreed with it.
//
// Every path a suite hands to a program is absolute. The programs chdir (the
// wrappers do, to reach their own checkout), so a relative path would silently
// name a different file after the chdir — which is how a suite stops testing what
// it thinks it tests.
package harness

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

// Timeout bounds one child process. A suite that hangs must fail the run rather
// than occupy a CI job until the job's own limit.
const Timeout = 10 * time.Minute

// Repository is the checkout root, found by walking up to the module file. It is
// the answer to "where is the program under test" and to "where is the committed
// bytes this fixture is built from".
func Repository(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cannot read the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// RequireTools skips the case when a prerequisite is missing. The whole-suite
// gate (`script/test/contracts.sh`) names its prerequisites and fails without
// them; this skip is for a developer running one package directly.
func RequireTools(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is required by this case", name)
		}
	}
}

// TempDir is a throwaway directory under the real temporary root, with symlinks
// resolved. It is not t.TempDir: macOS resolves /var to /private/var, and a
// program that prints an absolute path it retained must print the same string the
// suite compares against.
func TempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatalf("cannot create a temporary directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("cannot resolve %s: %v", dir, err)
	}
	return resolved
}

// Scratch is a named directory inside a case's temporary tree.
func Scratch(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("cannot create %s: %v", dir, err)
	}
	return dir
}

// ReadFile reads a file the case asserts on.
func ReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s: %v", path, err)
	}
	return body
}

// WriteFile writes a fixture file, creating its directory.
func WriteFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("cannot create the directory of %s: %v", path, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("cannot write %s: %v", path, err)
	}
}

// CopyFile copies one regular file, preserving nothing but its bytes: the mode a
// fixture file starts with is never the thing under test.
func CopyFile(t *testing.T, source, destination string) {
	t.Helper()
	body := ReadFile(t, source)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatalf("cannot create the directory of %s: %v", destination, err)
	}
	if err := os.WriteFile(destination, body, 0o644); err != nil {
		t.Fatalf("cannot write %s: %v", destination, err)
	}
}

// CopyFiles copies every regular file in one directory, not recursing. It is how
// the committed artifacts directory becomes a fixture root.
func CopyFiles(t *testing.T, source, destination string) {
	t.Helper()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatalf("cannot read %s: %v", source, err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		CopyFile(t, filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()))
	}
}

// Spec describes one child process.
type Spec struct {
	// Argv is the command and its arguments. Argv[0] is resolved on PATH.
	Argv []string
	// Dir is the working directory. Empty inherits the suite's.
	Dir string
	// Env is added to (and overrides) the inherited environment.
	Env map[string]string
	// Unset names inherited variables the child must not see. A suite that cares
	// about an ambient variable names it here rather than trusting the host.
	Unset []string
	// Stdin is fed to the child. Empty means /dev/null.
	Stdin string
}

// Result is everything a case asserts on: the status and the two streams.
type Result struct {
	Argv   []string
	Dir    string
	Status int
	Stdout string
	Stderr string
}

// Run executes a child process and captures its streams. A status other than
// zero is data, not an error: the suites assert on statuses.
func Run(t *testing.T, spec Spec) Result {
	t.Helper()
	return run(t, spec, Timeout)
}

func run(t *testing.T, spec Spec, timeout time.Duration) Result {
	t.Helper()
	if len(spec.Argv) == 0 {
		t.Fatalf("cannot run an empty command")
	}
	// The child is wrapped in a watchdog rather than using exec's own timeout,
	// because a gate that hangs must report the command it hung on.
	path, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		t.Fatalf("cannot find %s: %v", spec.Argv[0], err)
	}
	command := exec.Command(path, spec.Argv[1:]...)
	command.Dir = spec.Dir
	command.Env = environment(spec)
	if spec.Stdin != "" {
		command.Stdin = strings.NewReader(spec.Stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	result := Result{Argv: spec.Argv, Dir: spec.Dir}
	if err := command.Start(); err != nil {
		t.Fatalf("cannot start %s: %v", strings.Join(spec.Argv, " "), err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		result.Stdout, result.Stderr = stdout.String(), stderr.String()
		if err == nil {
			return result
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%s: %v", strings.Join(spec.Argv, " "), err)
		}
		result.Status = exit.ExitCode()
		return result
	case <-time.After(timeout):
		_ = command.Process.Kill()
		<-done
		t.Fatalf("%s did not finish in %s", strings.Join(spec.Argv, " "), timeout)
		return Result{}
	}
}

func environment(spec Spec) []string {
	unset := make(map[string]bool, len(spec.Unset))
	for _, name := range spec.Unset {
		unset[name] = true
	}
	environment := make([]string, 0, len(os.Environ())+len(spec.Env))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if unset[name] {
			continue
		}
		if _, replaced := spec.Env[name]; replaced {
			continue
		}
		environment = append(environment, entry)
	}
	for name, value := range spec.Env {
		environment = append(environment, name+"="+value)
	}
	return environment
}

// Report renders the command and its streams for a failure message.
func (r Result) Report() string {
	return fmt.Sprintf("command: %s\nstatus: %d\nstdout:\n%s\nstderr:\n%s",
		strings.Join(r.Argv, " "), r.Status, strings.TrimRight(r.Stdout, "\n"),
		strings.TrimRight(r.Stderr, "\n"))
}

// RequireStatus fails the case when the status is not the one the contract names.
func (r Result) RequireStatus(t *testing.T, want int) {
	t.Helper()
	if r.Status != want {
		t.Fatalf("status %d, want %d\n%s", r.Status, want, r.Report())
	}
}

// RequireStdout and RequireStderr assert on a substring a consumer parses. The
// suites freeze text only where something reads it.
func (r Result) RequireStdout(t *testing.T, needle string) {
	t.Helper()
	if !strings.Contains(r.Stdout, needle) {
		t.Fatalf("stdout does not carry %q\n%s", needle, r.Report())
	}
}

func (r Result) RequireStderr(t *testing.T, needle string) {
	t.Helper()
	if !strings.Contains(r.Stderr, needle) {
		t.Fatalf("stderr does not carry %q\n%s", needle, r.Report())
	}
}

// Upstream is the byte source a writer resolves: --fixture-upstream serves
// https://host/path from DIR/host/path. A case builds the exact bytes a release
// host would serve, which is why a case can make one architecture fail while the
// other resolves.
type Upstream struct {
	Dir string
}

func NewUpstream(t *testing.T) *Upstream {
	t.Helper()
	return &Upstream{Dir: TempDir(t, "upstream.")}
}

// Path is where the fixture keeps the body for one URL. A URL that is not a
// plain scheme-and-host is a fixture defect, not something to guess around.
func (u *Upstream) Path(t *testing.T, url string) string {
	t.Helper()
	scheme, rest, found := strings.Cut(url, "://")
	if !found || scheme == "" || rest == "" {
		t.Fatalf("not an absolute URL: %q", url)
	}
	if strings.Contains(rest, "..") {
		t.Fatalf("URL traverses: %q", url)
	}
	return filepath.Join(u.Dir, filepath.FromSlash(rest))
}

// Write serves body at url.
func (u *Upstream) Write(t *testing.T, url string, body []byte) {
	t.Helper()
	WriteFile(t, u.Path(t, url), body)
}

// Read returns the body currently served at url.
func (u *Upstream) Read(t *testing.T, url string) []byte {
	t.Helper()
	return ReadFile(t, u.Path(t, url))
}

// Remove deletes the body at url, so the next fetch is a transport failure.
func (u *Upstream) Remove(t *testing.T, url string) {
	t.Helper()
	path := u.Path(t, url)
	if err := os.Remove(path); err != nil {
		t.Fatalf("cannot remove %s: %v", path, err)
	}
}

// Member is one entry of a fixture archive. Typeflag defaults to a regular file,
// whose mode defaults to 0644.
type Member struct {
	Name     string
	Body     []byte
	Mode     int64
	Typeflag byte
	Linkname string
}

// TarArchive renders the members as an uncompressed tar stream.
func TarArchive(t *testing.T, members []Member) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	for _, member := range members {
		mode := member.Mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Name:     member.Name,
			Mode:     mode,
			Size:     int64(len(member.Body)),
			Typeflag: member.Typeflag,
			Linkname: member.Linkname,
		}
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Typeflag != tar.TypeReg {
			header.Size = 0
		}
		if header.Typeflag == tar.TypeDir && !strings.HasSuffix(header.Name, "/") {
			header.Name += "/"
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("cannot write the archive member %s: %v", member.Name, err)
		}
		if header.Typeflag == tar.TypeReg && len(member.Body) > 0 {
			if _, err := writer.Write(member.Body); err != nil {
				t.Fatalf("cannot write the archive member %s: %v", member.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("cannot close the archive: %v", err)
	}
	return buffer.Bytes()
}

// Gzip compresses raw bytes.
func Gzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(raw); err != nil {
		t.Fatalf("cannot gzip: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("cannot gzip: %v", err)
	}
	return buffer.Bytes()
}

// XZ compresses raw bytes with the vendored xz writer.
func XZ(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer, err := xz.NewWriter(&buffer)
	if err != nil {
		t.Fatalf("cannot start the xz writer: %v", err)
	}
	if _, err := writer.Write(raw); err != nil {
		t.Fatalf("cannot xz: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("cannot xz: %v", err)
	}
	return buffer.Bytes()
}

// TarXZ and TarGz render and compress in one step, which is the whole of what most
// fixture archives are.
func TarXZ(t *testing.T, members []Member) []byte {
	return XZ(t, TarArchive(t, members))
}

func TarGz(t *testing.T, members []Member) []byte {
	return Gzip(t, TarArchive(t, members))
}

// SHA256Hex and SHA512Hex are the digests the pins and manifests are written in.
func SHA256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func SHA512Hex(raw []byte) string {
	sum := sha512.Sum512(raw)
	return hex.EncodeToString(sum[:])
}

// Signer is one throwaway OpenPGP key and the home it lives in. The home is under
// the real temporary root because gpg-agent's socket path has a length limit: a
// deep TMPDIR-derived home fails the import, and that failure would look like the
// program under test refusing a good signature.
type Signer struct {
	Home        string
	Fingerprint string
}

// NewSigner generates an ed25519 signing key in a fresh 0700 home.
func NewSigner(t *testing.T, home string) *Signer {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatalf("cannot create the key home %s: %v", home, err)
	}
	signer := &Signer{Home: home}
	signer.gpg(t, "--batch", "--quiet", "--pinentry-mode", "loopback",
		"--passphrase", "", "--quick-generate-key",
		fmt.Sprintf("fixture %s <fixture-%s@example.invalid>", filepath.Base(home), filepath.Base(home)),
		"ed25519", "sign", "0")
	for _, line := range strings.Split(signer.gpg(t, "--batch", "--with-colons", "--list-keys"), "\n") {
		if strings.HasPrefix(line, "fpr:") {
			fields := strings.Split(line, ":")
			signer.Fingerprint = strings.ToUpper(fields[9])
			return signer
		}
	}
	t.Fatalf("the generated key has no fingerprint: %s", home)
	return nil
}

// Export returns the armored public key.
func (s *Signer) Export(t *testing.T) string {
	t.Helper()
	return s.gpg(t, "--batch", "--armor", "--export", s.Fingerprint)
}

// SignDetached writes path+".sig" for path.
func (s *Signer) SignDetached(t *testing.T, path string) {
	t.Helper()
	s.gpg(t, "--batch", "--quiet", "--pinentry-mode", "loopback", "--passphrase", "",
		"--armor", "--yes", "--detach-sign", "--output", path+".sig", path)
	if _, err := os.Stat(path + ".sig"); err != nil {
		t.Fatalf("the signature was not written: %v", err)
	}
}

// gpg runs gpg against this signer's home and fails the case on a non-zero status:
// a fixture that cannot sign must not be reported as the program refusing.
func (s *Signer) gpg(t *testing.T, argv ...string) string {
	t.Helper()
	result := Run(t, Spec{
		Argv:  append([]string{"gpg"}, argv...),
		Env:   map[string]string{"GNUPGHOME": s.Home, "LC_ALL": "C"},
		Unset: []string{"GPG_AGENT_INFO"},
	})
	if result.Status != 0 {
		t.Fatalf("the fixture key operation failed\n%s", result.Report())
	}
	return result.Stdout
}

// Walk lists the regular files and symlinks under root as slash-separated paths
// relative to root, sorted. It is how a case states "nothing was published".
func Walk(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatalf("cannot walk %s: %v", root, err)
	}
	return paths
}

// CopyTree copies a directory tree, keeping modes. Symlinks are copied as links,
// so a fixture that has to contain one can.
func CopyTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, body, info.Mode().Perm())
		}
	})
	if err != nil {
		t.Fatalf("cannot copy %s to %s: %v", source, destination, err)
	}
}
