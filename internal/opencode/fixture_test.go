package opencode

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests in this package drive the real program with real child programs:
// every assertion below is about what the process did, not about a mock's
// expectations. A child that touches CALLS is the evidence that it started, which
// is how the ordering guards are proved.

// clearEnvironment removes the variables the two guards read, so a developer's own
// shell cannot decide a test. The guards read this process's environment because
// that is what the composed subcommands did; there is no seam to inherit instead.
func clearEnvironment(t *testing.T) {
	t.Helper()
	managed := func(key string) bool {
		lowered := strings.ToLower(key)
		return strings.HasPrefix(lowered, "npm_config_") ||
			strings.HasPrefix(lowered, "pnpm_config_") ||
			strings.HasPrefix(lowered, "corepack_") ||
			lowered == "pnpm_home" ||
			lowered == "node_options" ||
			lowered == "node_path" ||
			lowered == "bun_options" ||
			lowered == "node_tls_reject_unauthorized"
	}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if !managed(key) {
			continue
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		t.Cleanup(func() { _ = os.Setenv(key, value) })
	}
}

// fixture is one invocation's private tree.
type fixture struct {
	root   string
	calls  string
	bin    string
	prefix string
}

// newFixture creates the authorized root, the command directory that carries the
// fake children and the prefix beneath the root.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	// The access audit requires every ancestor to be traversable by all, and a
	// per-test TMPDIR is 0700 by default. The fixture therefore lives under /tmp,
	// the parent the Python harness used for the same reason, and is opened up.
	created, err := os.MkdirTemp("/tmp", "opencode-fixture-")
	if err != nil {
		t.Fatalf("create fixture root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(created) })
	if err := os.Chmod(created, 0o755); err != nil {
		t.Fatalf("open the fixture root: %v", err)
	}
	root, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatalf("resolve fixture root: %v", err)
	}
	tree := &fixture{
		root:   root,
		calls:  filepath.Join(root, "calls"),
		bin:    filepath.Join(root, "bin"),
		prefix: filepath.Join(root, "prefix"),
	}
	for _, directory := range []string{tree.bin, tree.prefix} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	// A fake child appends one line per invocation, so its absence proves it never
	// started and its order proves the guards' order.
	t.Setenv("CALLS", tree.calls)
	t.Setenv("PATH", tree.bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	clearEnvironment(t)
	return tree
}

// program writes one executable fixture child.
func (f *fixture) program(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(f.bin, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// write creates one file beneath the fixture root and returns its path.
func (f *fixture) write(t *testing.T, name, body string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// invocations returns the recorded child invocations.
func (f *fixture) invocations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read calls: %v", err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

// expectInvocations asserts the exact child sequence: the order is the contract
// the reviewed hooks documented, so a reordering has to fail here.
func (f *fixture) expectInvocations(t *testing.T, want ...string) {
	t.Helper()
	got := f.invocations(t)
	if len(got) != len(want) {
		t.Fatalf("child invocations = %q, want %q", got, want)
	}
	for index := range want {
		if !strings.Contains(got[index], want[index]) {
			t.Fatalf("child invocation %d = %q, want it to contain %q (all: %q)", index, got[index], want[index], got)
		}
	}
}

// expectNoChild asserts nothing started at all.
func (f *fixture) expectNoChild(t *testing.T) {
	t.Helper()
	if got := f.invocations(t); len(got) > 0 {
		t.Fatalf("a child started (%q); the guard it must precede did not hold", got)
	}
}

// refusalText returns the one diagnostic line a Refusal carries, or "".
func refusalText(err error) (string, bool) {
	var refused *Refusal
	if errors.As(err, &refused) {
		return refused.Text, true
	}
	return "", false
}

// silentStatus returns the status a Silent carries, or 0.
func silentStatus(err error) (int, bool) {
	var silent *Silent
	if errors.As(err, &silent) {
		return silent.Status, true
	}
	return 0, false
}

// errOf drops the value of a two-result call so an error can be asserted alone.
func errOf(_ string, err error) error { return err }

func mustRefusal(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("status = success, want the refusal %q", want)
	}
	got, ok := refusalText(err)
	if !ok {
		t.Fatalf("error = %v (%T), want a Refusal", err, err)
	}
	if got != want {
		t.Fatalf("refusal = %q, want %q", got, want)
	}
}

// mustUsage asserts a usage refusal and the one line it carries.
func mustUsage(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("status = success, want the usage refusal %q", want)
	}
	var usage *Usage
	if !errors.As(err, &usage) {
		t.Fatalf("error = %v (%T), want a Usage", err, err)
	}
	if usage.Text != want {
		t.Fatalf("usage = %q, want %q", usage.Text, want)
	}
}

func mustSilent(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("status = success, want the silent status %d", want)
	}
	got, ok := silentStatus(err)
	if !ok {
		t.Fatalf("error = %v (%T), want a Silent", err, err)
	}
	if got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}
