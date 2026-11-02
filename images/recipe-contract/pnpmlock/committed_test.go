package pnpmlock

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// The adopted dsh lock is a reviewed artifact, and one class of drift can only
// reach it through a seeded resolution: pnpm keeps a locked version and records
// that package's *current* registry metadata, including a `deprecated` notice
// that appeared after review (#134). The comparator refuses such a record on
// every publication path, but only against the side it is handed. The ordinary
// changed-selection path (image builds and the CI dsh-changed leg) still seeds
// the resolver, so a lock carrying a deprecation that upstream added later is
// exactly what that path can still publish (FU4). This test is the committed
// tree's own guard: the packages this repository publishes and the `@smithy/core`
// family the ticket names must carry no `deprecated` metadata at all, and every
// `@smithy/core` record must be at least the version whose metadata was reviewed.
//
// It reads the same production prefix `TestWorkspaceWithOverridesProductionWorkspace`
// and `TestCommittedDshCheckFinal` read, and it derives every expectation from
// the committed bytes rather than from a restated lock snapshot.
const (
	smithyCorePackage  = "@smithy/core"
	smithyCoreReviewed = "3.38.1"
	ownedPackagePrefix = "@deepseek-ai/dsh"
)

func committedDshLock(t *testing.T) document.Value {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "..", "tools", "dsh", "pnpm-lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := document.Parse(source, document.YAML)
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

// deprecatedOwnedArchives names every archive record under `packages` whose
// package is one this repository publishes or the `@smithy/core` family, and
// whose record carries `deprecated`. Keys are returned in lock order.
func deprecatedOwnedArchives(t *testing.T, lock document.Value) []string {
	t.Helper()
	packages, err := recordOf(lock.Get("packages"))
	if err != nil {
		t.Fatal(err)
	}
	var deprecations []string
	for _, key := range packages.Keys() {
		id, err := parseSnapshotID(key.Display())
		if err != nil {
			t.Fatalf("%s: %v", key.Display(), err)
		}
		name := id.Package.Name.String()
		if name != smithyCorePackage && !strings.HasPrefix(name, ownedPackagePrefix) {
			continue
		}
		record, err := recordOf(packages.GetJS(key))
		if err != nil {
			t.Fatalf("%s: %v", key.Display(), err)
		}
		if !isUndefined(record.Get("deprecated")) {
			deprecations = append(deprecations, key.Display())
		}
	}
	return deprecations
}

// smithyCoreArchives returns the versions of every `@smithy/core` archive.
func smithyCoreArchives(t *testing.T, lock document.Value) []string {
	t.Helper()
	packages, err := recordOf(lock.Get("packages"))
	if err != nil {
		t.Fatal(err)
	}
	var versions []string
	for _, key := range packages.Keys() {
		id, err := parseSnapshotID(key.Display())
		if err != nil {
			t.Fatalf("%s: %v", key.Display(), err)
		}
		if id.Package.Name.String() == smithyCorePackage {
			versions = append(versions, id.Package.Version.String())
		}
	}
	return versions
}

// atLeastRelease compares the release triple of two canonical exact versions.
// The adopted `@smithy/core` releases carry no prerelease component; a
// prerelease would sort before its release, which the caller never passes.
func atLeastRelease(t *testing.T, version, want string) bool {
	t.Helper()
	parse := func(text string) [3]int {
		release := strings.SplitN(text, "-", 2)[0]
		parts := strings.Split(release, ".")
		if len(parts) != 3 {
			t.Fatalf("not a release version: %s", text)
		}
		var out [3]int
		for index, part := range parts {
			value, err := strconv.Atoi(part)
			if err != nil {
				t.Fatalf("not a release version: %s", text)
			}
			out[index] = value
		}
		return out
	}
	got, floor := parse(version), parse(want)
	for index := range got {
		if got[index] != floor[index] {
			return got[index] > floor[index]
		}
	}
	return true
}

func TestCommittedDshLockCarriesNoDeprecatedOwnedArchive(t *testing.T) {
	lock := committedDshLock(t)
	if deprecations := deprecatedOwnedArchives(t, lock); len(deprecations) != 0 {
		t.Errorf("the committed dsh lock records upstream deprecation metadata on %v; a seeded resolution published current registry metadata for a retained record (#134, FU4)", deprecations)
	}
	versions := smithyCoreArchives(t, lock)
	if len(versions) == 0 {
		t.Fatal("the committed dsh lock carries no " + smithyCorePackage + " record")
	}
	for _, version := range versions {
		if !atLeastRelease(t, version, smithyCoreReviewed) {
			t.Errorf("%s@%s is below the reviewed %s floor", smithyCorePackage, version, smithyCoreReviewed)
		}
	}
}

// The detector above is the product under test, so it must fire on the exact
// regression it names. This injects `deprecated` into the first `@smithy/core`
// record of the committed bytes without touching the committed tree: an
// assertion that only ever returns "clean" would pass the test above for the
// wrong reason.
func TestDeprecatedDetectorNamesTheRetainedRecord(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "tools", "dsh", "pnpm-lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	at := strings.Index(text, "\n  '"+smithyCorePackage+"@")
	if at < 0 {
		t.Fatalf("the committed lock carries no %s packages record", smithyCorePackage)
	}
	lineStart := at + 1
	lineEnd := strings.Index(text[lineStart:], "\n")
	if lineEnd < 0 {
		t.Fatal("the first " + smithyCorePackage + " packages record is unterminated")
	}
	key := strings.TrimSuffix(strings.TrimPrefix(strings.Trim(text[lineStart:lineStart+lineEnd], " "), "'"), "':")
	if key == "" {
		t.Fatalf("unexpected packages key spelling: %q", text[lineStart:lineStart+lineEnd])
	}
	injected := text[:lineStart+lineEnd+1] + "    deprecated: verification negative control\n" +
		text[lineStart+lineEnd+1:]
	lock, err := document.Parse([]byte(injected), document.YAML)
	if err != nil {
		t.Fatal(err)
	}
	deprecations := deprecatedOwnedArchives(t, lock)
	if len(deprecations) != 1 || deprecations[0] != key {
		t.Fatalf("deprecations after the injected record = %v, want [%s]", deprecations, key)
	}
}
