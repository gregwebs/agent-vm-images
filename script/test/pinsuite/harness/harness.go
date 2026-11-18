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
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"
)

// Timeout bounds one child process. A suite that hangs must fail the run rather
// than occupy a CI job until the job's own limit.
const Timeout = 10 * time.Minute

// Reporter is the failure surface a fixture builder writes to. *testing.T
// implements it, so the code that builds a fixture in a case is the same code a
// command runs: a fixture generated for another suite must not be a lookalike of
// the one the cases check against.
//
// Fatalf does not return, and a Reporter made by Command stops the builder there.
type Reporter interface {
	Helper()
	Cleanup(func())
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Command runs a fixture builder outside a test and returns its first failure,
// nil when it finished. The builder runs on its own goroutine because that is
// where a reporter may stop it: Go does not let the main goroutine exit that way.
func Command(build func(Reporter)) error {
	reporter := &commandReporter{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		build(reporter)
	}()
	<-done
	for index := len(reporter.cleanups) - 1; index >= 0; index-- {
		reporter.cleanups[index]()
	}
	return reporter.err
}

// commandReporter records the first failure instead of printing it, and runs the
// builder's cleanups whatever happened.
type commandReporter struct {
	err      error
	cleanups []func()
}

func (c *commandReporter) Helper() {}

func (c *commandReporter) Cleanup(cleanup func()) { c.cleanups = append(c.cleanups, cleanup) }

// Skipf fails instead of skipping: a command has no suite to skip a case of, and a
// fixture builder that cannot do its job must say so in its own vocabulary rather
// than in a test's.
func (c *commandReporter) Skipf(format string, args ...any) {
	c.Fatalf("a fixture builder cannot skip: "+format, args...)
}

func (c *commandReporter) Fatalf(format string, args ...any) {
	if c.err == nil {
		c.err = fmt.Errorf(format, args...)
	}
	runtime.Goexit()
}

// Compile builds one program in this checkout, once per test process, and returns
// the path to it.
//
// The suites drive the maintainer wrapper for the one case that is about the
// wrapper, because a per-case `go build` costs more than the run it starts. The
// build goes to a per-suite path rather than the wrapper's shared one so that two
// suites under one `go test` do not both relink the same file: `go build -o`
// installs by rename and would stay correct, but two suites relinking one path is
// two builds paid for one binary.
var compiled = struct {
	sync.Mutex
	paths map[string]string
}{paths: map[string]string{}}

func Compile(r Reporter, name, packagePath string) string {
	r.Helper()
	compiled.Lock()
	defer compiled.Unlock()
	if path, built := compiled.paths[name]; built {
		return path
	}
	root := Repository(r)
	path := filepath.Join(root, "target", "pin-suite", name, filepath.Base(packagePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.Fatalf("cannot create the build directory for %s: %v", packagePath, err)
	}
	result := run(r, Spec{Argv: []string{"go", "build", "-o", path, packagePath}, Dir: root}, Timeout)
	if result.Status != 0 {
		r.Fatalf("cannot build %s\n%s", packagePath, result.Report())
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		r.Fatalf("the build of %s wrote no binary at %s", packagePath, path)
	}
	compiled.paths[name] = path
	return path
}

// Repository is the checkout root, found by walking up to the module file. It is
// the answer to "where is the program under test" and to "where is the committed
// bytes this fixture is built from".
func Repository(r Reporter) string {
	r.Helper()
	dir, err := os.Getwd()
	if err != nil {
		r.Fatalf("cannot read the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			r.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// RequireTools skips the case when a prerequisite is missing. The whole-suite
// gate (`script/test/contracts.sh`) names its prerequisites and fails without
// them; this skip is for a developer running one package directly.
func RequireTools(r Reporter, names ...string) {
	r.Helper()
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			r.Skipf("%s is required by this case", name)
		}
	}
}

// TempDir is a throwaway directory under the real temporary root, with symlinks
// resolved. It is not t.TempDir: macOS resolves /var to /private/var, and a
// program that prints an absolute path it retained must print the same string the
// suite compares against.
func TempDir(r Reporter, prefix string) string {
	r.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		r.Fatalf("cannot create a temporary directory: %v", err)
	}
	r.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		r.Fatalf("cannot resolve %s: %v", dir, err)
	}
	return resolved
}

// Scratch is a named directory inside a case's temporary tree.
func Scratch(r Reporter, parent, name string) string {
	r.Helper()
	dir := filepath.Join(parent, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		r.Fatalf("cannot create %s: %v", dir, err)
	}
	return dir
}

// ReadFile reads a file the case asserts on.
func ReadFile(r Reporter, path string) []byte {
	r.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		r.Fatalf("cannot read %s: %v", path, err)
	}
	return body
}

// WriteFile writes a fixture file, creating its directory.
func WriteFile(r Reporter, path string, body []byte) {
	r.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.Fatalf("cannot create the directory of %s: %v", path, err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		r.Fatalf("cannot write %s: %v", path, err)
	}
}

// CopyFile copies one regular file, preserving nothing but its bytes: the mode a
// fixture file starts with is never the thing under test.
func CopyFile(r Reporter, source, destination string) {
	r.Helper()
	body := ReadFile(r, source)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		r.Fatalf("cannot create the directory of %s: %v", destination, err)
	}
	if err := os.WriteFile(destination, body, 0o644); err != nil {
		r.Fatalf("cannot write %s: %v", destination, err)
	}
}

// CopyFiles copies every regular file in one directory, not recursing. It is how
// the committed artifacts directory becomes a fixture root.
func CopyFiles(r Reporter, source, destination string) {
	r.Helper()
	entries, err := os.ReadDir(source)
	if err != nil {
		r.Fatalf("cannot read %s: %v", source, err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		CopyFile(r, filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name()))
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
func Run(r Reporter, spec Spec) Result {
	r.Helper()
	return run(r, spec, Timeout)
}

func run(r Reporter, spec Spec, timeout time.Duration) Result {
	r.Helper()
	if len(spec.Argv) == 0 {
		r.Fatalf("cannot run an empty command")
	}
	// The child is wrapped in a watchdog rather than using exec's own timeout,
	// because a gate that hangs must report the command it hung on.
	path, err := exec.LookPath(spec.Argv[0])
	if err != nil {
		r.Fatalf("cannot find %s: %v", spec.Argv[0], err)
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
		r.Fatalf("cannot start %s: %v", strings.Join(spec.Argv, " "), err)
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
			r.Fatalf("%s: %v", strings.Join(spec.Argv, " "), err)
		}
		result.Status = exit.ExitCode()
		return result
	case <-time.After(timeout):
		_ = command.Process.Kill()
		<-done
		r.Fatalf("%s did not finish in %s", strings.Join(spec.Argv, " "), timeout)
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
func (r Result) RequireStatus(reporter Reporter, want int) {
	reporter.Helper()
	if r.Status != want {
		reporter.Fatalf("status %d, want %d\n%s", r.Status, want, r.Report())
	}
}

// RequireStdout and RequireStderr assert on a substring a consumer parses. The
// suites freeze text only where something reads it.
func (r Result) RequireStdout(reporter Reporter, needle string) {
	reporter.Helper()
	if !strings.Contains(r.Stdout, needle) {
		reporter.Fatalf("stdout does not carry %q\n%s", needle, r.Report())
	}
}

func (r Result) RequireStderr(reporter Reporter, needle string) {
	reporter.Helper()
	if !strings.Contains(r.Stderr, needle) {
		reporter.Fatalf("stderr does not carry %q\n%s", needle, r.Report())
	}
}

// Upstream is the byte source a writer resolves: --fixture-upstream serves
// https://host/path from DIR/host/path. A case builds the exact bytes a release
// host would serve, which is why a case can make one architecture fail while the
// other resolves.
type Upstream struct {
	Dir string
}

func NewUpstream(r Reporter) *Upstream {
	r.Helper()
	return &Upstream{Dir: TempDir(r, "upstream.")}
}

// Path is where the fixture keeps the body for one URL. A URL that is not a
// plain scheme-and-host is a fixture defect, not something to guess around.
func (u *Upstream) Path(r Reporter, url string) string {
	r.Helper()
	scheme, rest, found := strings.Cut(url, "://")
	if !found || scheme == "" || rest == "" {
		r.Fatalf("not an absolute URL: %q", url)
	}
	if strings.Contains(rest, "..") {
		r.Fatalf("URL traverses: %q", url)
	}
	return filepath.Join(u.Dir, filepath.FromSlash(rest))
}

// Write serves body at url.
func (u *Upstream) Write(r Reporter, url string, body []byte) {
	r.Helper()
	WriteFile(r, u.Path(r, url), body)
}

// Read returns the body currently served at url.
func (u *Upstream) Read(r Reporter, url string) []byte {
	r.Helper()
	return ReadFile(r, u.Path(r, url))
}

// Remove deletes the body at url, so the next fetch is a transport failure.
func (u *Upstream) Remove(r Reporter, url string) {
	r.Helper()
	path := u.Path(r, url)
	if err := os.Remove(path); err != nil {
		r.Fatalf("cannot remove %s: %v", path, err)
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
func TarArchive(r Reporter, members []Member) []byte {
	r.Helper()
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
			r.Fatalf("cannot write the archive member %s: %v", member.Name, err)
		}
		if header.Typeflag == tar.TypeReg && len(member.Body) > 0 {
			if _, err := writer.Write(member.Body); err != nil {
				r.Fatalf("cannot write the archive member %s: %v", member.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		r.Fatalf("cannot close the archive: %v", err)
	}
	return buffer.Bytes()
}

// Gzip compresses raw bytes.
func Gzip(r Reporter, raw []byte) []byte {
	r.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(raw); err != nil {
		r.Fatalf("cannot gzip: %v", err)
	}
	if err := writer.Close(); err != nil {
		r.Fatalf("cannot gzip: %v", err)
	}
	return buffer.Bytes()
}

// XZ compresses raw bytes with the vendored xz writer.
func XZ(r Reporter, raw []byte) []byte {
	r.Helper()
	var buffer bytes.Buffer
	writer, err := xz.NewWriter(&buffer)
	if err != nil {
		r.Fatalf("cannot start the xz writer: %v", err)
	}
	if _, err := writer.Write(raw); err != nil {
		r.Fatalf("cannot xz: %v", err)
	}
	if err := writer.Close(); err != nil {
		r.Fatalf("cannot xz: %v", err)
	}
	return buffer.Bytes()
}

// TarXZ and TarGz render and compress in one step, which is the whole of what most
// fixture archives are.
func TarXZ(r Reporter, members []Member) []byte {
	return XZ(r, TarArchive(r, members))
}

func TarGz(r Reporter, members []Member) []byte {
	return Gzip(r, TarArchive(r, members))
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
func NewSigner(r Reporter, home string) *Signer {
	r.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		r.Fatalf("cannot create the key home %s: %v", home, err)
	}
	signer := &Signer{Home: home}
	signer.gpg(r, "--batch", "--quiet", "--pinentry-mode", "loopback",
		"--passphrase", "", "--quick-generate-key",
		fmt.Sprintf("fixture %s <fixture-%s@example.invalid>", filepath.Base(home), filepath.Base(home)),
		"ed25519", "sign", "0")
	for _, line := range strings.Split(signer.gpg(r, "--batch", "--with-colons", "--list-keys"), "\n") {
		if strings.HasPrefix(line, "fpr:") {
			fields := strings.Split(line, ":")
			signer.Fingerprint = strings.ToUpper(fields[9])
			return signer
		}
	}
	r.Fatalf("the generated key has no fingerprint: %s", home)
	return nil
}

// Export returns the armored public key.
func (s *Signer) Export(r Reporter) string {
	r.Helper()
	return s.gpg(r, "--batch", "--armor", "--export", s.Fingerprint)
}

// SignDetached writes path+".sig" for path.
func (s *Signer) SignDetached(r Reporter, path string) {
	r.Helper()
	s.gpg(r, "--batch", "--quiet", "--pinentry-mode", "loopback", "--passphrase", "",
		"--armor", "--yes", "--detach-sign", "--output", path+".sig", path)
	if _, err := os.Stat(path + ".sig"); err != nil {
		r.Fatalf("the signature was not written: %v", err)
	}
}

// gpg runs gpg against this signer's home and fails the case on a non-zero status:
// a fixture that cannot sign must not be reported as the program refusing.
func (s *Signer) gpg(r Reporter, argv ...string) string {
	r.Helper()
	result := Run(r, Spec{
		Argv:  append([]string{"gpg"}, argv...),
		Env:   map[string]string{"GNUPGHOME": s.Home, "LC_ALL": "C"},
		Unset: []string{"GPG_AGENT_INFO"},
	})
	if result.Status != 0 {
		r.Fatalf("the fixture key operation failed\n%s", result.Report())
	}
	return result.Stdout
}

// Walk lists the regular files and symlinks under root as slash-separated paths
// relative to root, sorted. It is how a case states "nothing was published".
func Walk(r Reporter, root string) []string {
	r.Helper()
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
		r.Fatalf("cannot walk %s: %v", root, err)
	}
	return paths
}

// Census is every file and directory under root as slash-separated paths relative
// to root, sorted, with a trailing slash on a directory. It is what "the served
// tree is untouched" means: Walk alone cannot see a directory a refused run created,
// and an empty tree that stays empty is a claim about both.
func Census(r Reporter, root string) []string {
	r.Helper()
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			relative += "/"
		}
		paths = append(paths, relative)
		return nil
	})
	if err != nil {
		r.Fatalf("cannot walk %s: %v", root, err)
	}
	sort.Strings(paths)
	return paths
}

// CopyTree copies a directory tree, keeping modes. Symlinks are copied as links,
// so a fixture that has to contain one can.
func CopyTree(r Reporter, source, destination string) {
	r.Helper()
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
		r.Fatalf("cannot copy %s to %s: %v", source, destination, err)
	}
}
