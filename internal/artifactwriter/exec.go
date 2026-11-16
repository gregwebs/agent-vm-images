package artifactwriter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// runCommand runs one external program and returns its stdout and status.
//
// The environment is *replaced*, never extended, when one is given: a gpg call
// must not inherit the host's keyring pointers (see gpgEnvironment), and a `gh`
// call must see the maintainer's credentials unchanged, which is why the
// production call sites pass nil for "inherit".
func runCommand(ctx context.Context, argv []string, dir string,
	environment []string) (CommandResult, error) {
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = dir
	if environment != nil {
		command.Env = environment
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := CommandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.Status = exit.ExitCode()
		return result, nil
	}
	result.Status = -1
	return result, err
}

// gpgEnvironment is the private keyring *and* the rest of the environment the
// check may see. A constructed GNUPGHOME that is never passed to gpg leaves the
// host's ambient keyring in charge of pin-time verification, so every gpg call
// gets this and nothing else.
func gpgEnvironment(home, path string) []string {
	return []string{"GNUPGHOME=" + home, "LC_ALL=C", "PATH=" + path}
}

// commandOutput is the combined stdout/stderr a failing command reports, which is
// what the Python tool printed after the status: `(stderr or stdout).strip()`.
func commandOutput(stdout, stderr string) string {
	if strings.TrimSpace(stderr) != "" {
		return strings.TrimSpace(stderr)
	}
	return strings.TrimSpace(stdout)
}

// CommandResult is one finished command: its two streams and its exit status. A
// non-zero status is not an error here; several checks read it.
type CommandResult struct {
	Stdout string
	Stderr string
	Status int
}

// failure reports a command that had to succeed.
func (r CommandResult) failure(argv []string) error {
	if r.Status == 0 {
		return nil
	}
	return fmt.Errorf("%s failed (%d): %s", strings.Join(argv, " "), r.Status,
		commandOutput(r.Stdout, r.Stderr))
}

// fixtureFetcher serves the maintenance harness's upstream bytes.
//
// It exists so the writer's cases are hermetic without weakening the record: the
// URL a pin names stays the one a build would fetch, and only the bytes' origin
// changes. It is reachable only through an explicit command-line flag, because a
// pin written from fixture bytes still records the canonical upstream URL — an
// environment variable would let an inherited value redirect a real refresh.
type fixtureFetcher struct{ root string }

// NewFixtureFetcher serves `https://host/path` from `<root>/host/path`.
func NewFixtureFetcher(root string) (Fetcher, error) {
	if root == "" {
		return nil, refuse("the fixture upstream needs a directory")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, refuse("%s", err)
	}
	if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
		return nil, refuse("the fixture upstream %s is not a directory", root)
	}
	return &fixtureFetcher{root: absolute}, nil
}

func (f *fixtureFetcher) Get(_ context.Context, rawURL string, limit int64) ([]byte, error) {
	rest := rawURL
	for _, scheme := range []string{"https://", "http://"} {
		if strings.HasPrefix(rest, scheme) {
			rest = strings.TrimPrefix(rest, scheme)
			break
		}
	}
	if rest == "" || strings.Contains(rest, "..") {
		return nil, refuse("cannot fetch %s: a fixture path may not traverse", rawURL)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, filepath.FromSlash(rest)))
	if err != nil {
		return nil, refuse("cannot fetch %s: %s", rawURL, err)
	}
	if int64(len(raw)) > limit {
		return nil, refuse("%s is larger than the %d byte ceiling", rawURL, limit)
	}
	return raw, nil
}
