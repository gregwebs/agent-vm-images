package closure

// The test table's seam: a copy of the committed closure, a mutation, and an
// optional re-pin.
//
// Every path here is absolute (the committed directory is resolved from the test
// binary's working directory and made absolute), because a comparison that names
// paths must be the same comparison from any checkout spelling (#188 R-e).
//
// A mutation that changes a file's bytes without re-pinning is testing the digest
// rule; a mutation that changes a *rule's* input re-pins first, so the rule under
// test is the one that fires rather than a stale digest. `repin` deliberately
// leaves `modules[].h1`/`go_mod_h1` and the reviewer/date alone: the tests that
// change them are testing exactly the disagreement an automatic repair would hide.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// committedDir is the real reviewed data, as an absolute path.
func committedDir(t *testing.T) string {
	t.Helper()
	directory, err := filepath.Abs(filepath.Join("..", "..", "data", "gopls-closure"))
	if err != nil {
		t.Fatalf("resolve the committed closure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, string(FilePin))); err != nil {
		t.Fatalf("the committed closure is not where this test expects it (%s): %v the cases "+
			"below would run over nothing", directory, err)
	}
	return directory
}

// copyCommitted gives one case its own tree.
func copyCommitted(t *testing.T) string {
	t.Helper()
	source := committedDir(t)
	directory := t.TempDir()
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatalf("read the committed closure: %v", err)
	}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), content, 0o644); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	return directory
}

// rewrite replaces one input with the mutated bytes and fails the case if the
// mutation did not apply, so a case can never pass by changing nothing.
func rewrite(t *testing.T, directory string, name Filename, mutate func([]byte) []byte) {
	t.Helper()
	path := filepath.Join(directory, string(name))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	after := mutate(append([]byte{}, before...))
	if string(after) == string(before) {
		t.Fatalf("%s: the mutation did not apply", name)
	}
	if err := os.WriteFile(path, after, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// repin updates the four file digests and the two scope digests a test-only tree
// needs to stay coherent.
func repin(t *testing.T, directory string) {
	t.Helper()
	pinPath := filepath.Join(directory, string(FilePin))
	raw, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatalf("read %s: %v", FilePin, err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode %s: %v", FilePin, err)
	}
	files := map[string]any{}
	for _, name := range FileKeys {
		content, err := os.ReadFile(filepath.Join(directory, string(name)))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		digest := sha256.Sum256(content)
		files[string(name)] = hex.EncodeToString(digest[:])
	}
	document["files"] = files
	authentication, ok := document["authentication"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no authentication object to re-pin", FilePin)
	}
	scope, ok := authentication["scope"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no authentication.scope object to re-pin", FilePin)
	}
	scope["go.mod"] = files[string(FileGoMod)]
	scope["go.sum"] = files[string(FileGoSum)]
	reencoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", FilePin, err)
	}
	if err := os.WriteFile(pinPath, append(reencoded, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", FilePin, err)
	}
}

// editPin decodes the record, applies one change and writes it back. It does not
// re-pin; the cases that need coherent bytes call repin after it.
func editPin(t *testing.T, directory string, edit func(document map[string]any)) {
	t.Helper()
	pinPath := filepath.Join(directory, string(FilePin))
	raw, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatalf("read %s: %v", FilePin, err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode %s: %v", FilePin, err)
	}
	edit(document)
	reencoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode %s: %v", FilePin, err)
	}
	if err := os.WriteFile(pinPath, append(reencoded, '\n'), 0o644); err != nil {
		t.Fatalf("write %s: %v", FilePin, err)
	}
}

// load runs the loader the way `check` does.
func load(t *testing.T, directory string) (*Data, error) {
	t.Helper()
	source, err := DirSource(directory)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	return Load(context.Background(), Request{}, source)
}

// requireRefusal asserts that a load refused with a fragment.
func requireRefusal(t *testing.T, directory, fragment string) {
	t.Helper()
	_, err := load(t, directory)
	if err == nil {
		t.Fatalf("accepted a closure it must refuse; wanted %q", fragment)
	}
	refusal, ok := err.(*Refusal)
	if !ok {
		t.Fatalf("refused with a non-closure error (%T): %v", err, err)
	}
	if !strings.Contains(refusal.Message, fragment) {
		t.Fatalf("refused with %q, wanted a message containing %q", refusal.Message, fragment)
	}
}
