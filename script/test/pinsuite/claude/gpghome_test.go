package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// checkAcceptedAt is `check` under one environment, requiring a needle in its report.
func checkAcceptedAt(r harness.Reporter, directory string, env map[string]string, needle string) string {
	r.Helper()
	result := accepts(r, pinInvocation("check", directory, "--version", Version),
		func(invocation *invocation) { invocation.env = env })
	if !strings.Contains(result.Stdout, needle) {
		r.Fatalf("the check did not report %q\n%s", needle, result.Report())
	}
	return result.Stdout
}

// TestGPGHomeVerificationUnderALongAmbientTMPDIR: the reader allocates its private
// keyring under /tmp whatever TMPDIR is, and removes it.
//
// gpg-agent's socket path has a length limit, so a home derived from a deep TMPDIR
// would fail the import; the case gives the process a 120-character TMPDIR and holds
// the home the stub gpg saw to both rules.
func TestGPGHomeVerificationUnderALongAmbientTMPDIR(t *testing.T) {
	fixture := testFixture(t)
	base := scratch(t, "long-tmpdir")
	long := filepath.Join(base, strings.Repeat("x", 120))
	if err := os.Mkdir(long, 0o755); err != nil {
		t.Fatalf("cannot create the long TMPDIR: %v", err)
	}
	if len(long) < 120 {
		t.Fatalf("the long TMPDIR is not long enough: %d characters", len(long))
	}
	probe := newGPGStub(t, filepath.Join(base, "bin"), stubRequest{Mode: stubLog})
	// A scratch directory under the long TMPDIR is where the case lives, but no gpg
	// home may be created there.
	closure := filepath.Join(long, "source")
	harness.CopyTree(t, fixture.closure, closure)
	checkAcceptedAt(t, closure,
		map[string]string{"TMPDIR": long, "PATH": probe.Env["PATH"]},
		"committed closure verified offline")
	homes := probe.homes(t)
	if len(homes) == 0 {
		t.Fatal("the gpg wrapper logged no invocation")
	}
	for _, home := range homes {
		if !strings.HasPrefix(home, "/tmp/claude-pin-gpg.") {
			t.Fatalf("a gpg home was %s, not a private home under /tmp", home)
		}
		if _, err := os.Stat(home); err == nil {
			t.Fatalf("the gpg home %s was left behind", home)
		}
	}
}

// TestGPGHomeFailingKeyImportRefusedBeforeVerify: an import that cannot reach the agent
// is refused as that, before the signature is considered, and the home is gone.
func TestGPGHomeFailingKeyImportRefusedBeforeVerify(t *testing.T) {
	fixture := testFixture(t)
	probe := newGPGStub(t, scratch(t, "import-failure"), stubRequest{Mode: stubFailImport})
	checkRefusedAt(t, fixture.closure, Version, probe.Env, "cannot import the release key")
	logged := probe.homes(t)
	if len(logged) == 0 {
		t.Fatal("the gpg wrapper logged no invocation")
	}
	for _, home := range logged {
		if _, err := os.Stat(home); err == nil {
			t.Fatalf("the gpg home %s was left behind", home)
		}
	}
}
