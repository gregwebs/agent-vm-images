package codex

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

// preflightFixture is one closure and the archive the preflight group is pointed at.
func preflightFixture(t *testing.T) *fixture {
	t.Helper()
	return newFixture(t, filepath.Join(scratch(t, "preflight"), "source"), Release, nil)
}

// selectedPreflight is one `preflight` invocation over the fixture's amd64 target,
// which must refuse.
func selectedPreflight(t *testing.T, built *fixture, archive string, needles ...string) string {
	t.Helper()
	return refuses(t, pinInvocation("preflight", built.dir, "--release", Release,
		"--target", string(targetAMD64), "--archive", archive), needles...)
}

// TestPreflightAcceptsTheReviewedPackage: the reviewed package layout, metadata and
// ELF machine pass, and the accepted line is frozen: the vendored installer's hook
// prints it.
func TestPreflightAcceptsTheReviewedPackage(t *testing.T) {
	built := preflightFixture(t)
	archive := built.archiveAt(t, scratch(t, "preflight-accept"), linuxAMD64)
	printed := accepts(t, pinInvocation("preflight", built.dir, "--release", Release,
		"--target", string(targetAMD64), "--archive", archive)).Stdout
	want := fmt.Sprintf("codex %s linux/amd64: committed identity, package metadata and "+
		"ELF machine verified before extraction\n", Version)
	if printed != want {
		t.Fatalf("preflight printed %q, want %q", printed, want)
	}
}

// TestPreflightRefusesAnArchiveThatIsNotTheCommittedBytes: the committed digest is
// recomputed first, so a substituted archive never reaches the member policy.
func TestPreflightRefusesAnArchiveThatIsNotTheCommittedBytes(t *testing.T) {
	built := preflightFixture(t)
	archive := filepath.Join(scratch(t, "preflight-substituted"), assetName(targetAMD64))
	harness.WriteFile(t, archive, append(append([]byte{}, built.archives[linuxAMD64]...), "extra"...))
	selectedPreflight(t, built, archive, "does not match the committed")
}

// TestPreflightRefusesAMissingArchive: a download that did not happen is named by
// where it should have been.
func TestPreflightRefusesAMissingArchive(t *testing.T) {
	built := preflightFixture(t)
	selectedPreflight(t, built, filepath.Join(scratch(t, "preflight-missing"), "none.tgz"),
		"no downloaded archive at")
}

// TestPreflightRefusesTheOtherMachinesELF: a closed closure whose archive is built for
// the other machine.
//
// The digest is committed, the package metadata agrees with itself, and the only thing
// left to refuse is the ELF machine: this is the check that no amount of corroboration
// can substitute for.
func TestPreflightRefusesTheOtherMachinesELF(t *testing.T) {
	built := preflightFixture(t)
	directory := scratch(t, "preflight-machine")
	archive := filepath.Join(directory, assetName(targetAMD64))
	harness.WriteFile(t, archive, buildArchive(t, archiveRequest{
		version:  built.version,
		platform: linuxAMD64,
		machine:  machineOf(targetARM64),
	}))
	local := newFixture(t, filepath.Join(directory, "source"), Release, map[platform][]byte{
		linuxAMD64: harness.ReadFile(t, archive),
		linuxARM64: built.archives[linuxARM64],
	})
	refuses(t, pinInvocation("preflight", local.dir, "--release", Release,
		"--target", string(targetAMD64), "--archive", archive),
		"ELF machine 183", string(targetAMD64))
}
