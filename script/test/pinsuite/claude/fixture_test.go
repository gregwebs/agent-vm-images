package claude

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// TestMain removes the process template after every case has used it: the template is
// created once for the process, so no single case's cleanup may own it.
func TestMain(m *testing.M) {
	status := m.Run()
	processFixture.Lock()
	root := ""
	if processFixture.fixture != nil {
		root = processFixture.fixture.root
	}
	processFixture.Unlock()
	if root != "" {
		_ = os.RemoveAll(root)
	}
	os.Exit(status)
}

// TestWriteFixtureWritesTheLayoutTheBumperConsumes holds the fixture seam to its
// contract, because `script/test/agent-versions.sh` reads what it wrote and that script
// is not this package's gate.
//
// The layout and the returned release are the contract: `closure/` is a
// committed-shape closure (the pin, both evidence files and the key), `exec/` the two
// signed executables, and `upstream/` the five files `--fixture-upstream` serves. It
// runs through `harness.Command`, which is exactly what `avm-test write-fixture` calls,
// so the adapter is exercised too.
func TestWriteFixtureWritesTheLayoutTheBumperConsumes(t *testing.T) {
	dir := filepath.Join(harness.TempDir(t, "claude-fixture."), "out")
	printed := ""
	if err := harness.Command(func(r harness.Reporter) {
		printed = WriteFixture(r, Fixture{Dir: dir, Version: Version})
	}); err != nil {
		t.Fatalf("the fixture builder refused: %v", err)
	}
	if released := strings.TrimSpace(printed); released != Version {
		t.Fatalf("the builder reports the release %q, want %s", released, Version)
	}
	fingerprint := Fingerprint(strings.TrimSpace(string(harness.ReadFile(t,
		filepath.Join(dir, "signer", "fingerprint")))))
	if len(fingerprint) != 40 {
		t.Fatalf("the generated signer's fingerprint is %q", fingerprint)
	}
	closure := filepath.Join(dir, "closure")

	for _, name := range []string{"pin.json", "evidence/" + manifestName,
		"evidence/" + signatureName, keyName} {
		path := filepath.Join(closure, filepath.FromSlash(name))
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the fixture closure is missing %s: %v", name, err)
		}
	}
	for _, name := range []string{string(linuxX64), string(linuxARM64)} {
		if _, err := os.Stat(filepath.Join(dir, "exec", name)); err != nil {
			t.Fatalf("the fixture is missing exec/%s: %v", name, err)
		}
	}
	want := []string{
		upstreamBase + "/" + Version + "/" + manifestName,
		upstreamBase + "/" + Version + "/" + signatureName,
		upstreamKeyURL,
		upstreamBase + "/" + Version + "/" + string(linuxX64) + "/claude",
		upstreamBase + "/" + Version + "/" + string(linuxARM64) + "/claude",
	}
	sort.Strings(want)
	if served := filesBelow(t, filepath.Join(dir, "upstream")); strings.Join(served, "\n") !=
		strings.Join(want, "\n") {
		t.Fatalf("the served tree is %v, want %v", served, want)
	}

	// The signed manifest carries the two platforms this pin ignores as well, which is
	// what upstream serves: a fixture that dropped them would stop standing in for the
	// real document, and the reader's treatment of a platform it does not pin would go
	// unexercised.
	manifest := document(t, harness.ReadFile(t, filepath.Join(closure, "evidence", manifestName)))
	manifestPlatforms := object(t, manifest, "platforms")
	tokens := make([]string, 0, len(manifestPlatforms))
	for name := range manifestPlatforms {
		tokens = append(tokens, name)
	}
	sort.Strings(tokens)
	wantPlatforms := []string{"darwin-arm64", string(linuxARM64), string(linuxX64), string(linuxX64) + "-musl"}
	if strings.Join(tokens, ",") != strings.Join(wantPlatforms, ",") {
		t.Fatalf("the signed manifest carries %v, want %v", tokens, wantPlatforms)
	}

	pin := readPin(t, closure)
	if object(t, pin, "key")["fingerprint"] != string(fingerprint) {
		t.Fatalf("the pin names %v, not the printed %s", object(t, pin, "key")["fingerprint"],
			fingerprint)
	}
	if object(t, pin, "authentication")["reason"] != writerReason {
		t.Fatal("the fixture pin does not carry the writer's own reason text")
	}
}
