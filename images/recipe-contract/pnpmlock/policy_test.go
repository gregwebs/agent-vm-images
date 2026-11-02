package pnpmlock

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// The reviewed dsh workspace is YAML with a closed key set and a four-selector
// map, and it is what an adoption rewrites. This test pins the guarantee the
// seam relies on: the output is the exact output of the reviewed rewrite, every
// outside-map value is preserved, and the bytes belong to the encoder. The map is
// derived from the committed file with one selector bumped, so the rewrite always
// changes a value: a rewrite that returned its input, or one that pinned the map
// the file already carries, would fail the assertions below.
const bumpedTimerVersion = "1.1.8"

func TestWorkspaceWithOverridesProductionWorkspace(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "tools", "dsh", "pnpm-workspace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := document.Parse(source, document.YAML)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := ParseOverrideMap(before.Get("overrides"))
	if err != nil {
		t.Fatal(err)
	}
	// The reviewed map is the committed one with one selector moved, which is the
	// shape every adoption has: it is not restated here, so it cannot silently
	// become an identity rewrite when the file is next adopted.
	reviewed, err := withSelectorVersion(committed, SelectorTimer, bumpedTimerVersion)
	if err != nil {
		t.Fatal(err)
	}
	if reviewed.Equal(committed) {
		t.Fatalf("the reviewed map must differ from the committed one: %s", before.Get("overrides").Describe())
	}
	rewritten, err := WorkspaceWithOverrides(source, reviewed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(rewritten, source) {
		t.Fatal("the reviewed rewrite returned its input")
	}
	// The bumped value is in the bytes the writer emitted, located by its selector
	// rather than read back through the parser that produced them.
	bumpedLine := "'" + SelectorTimer.String() + "': " + bumpedTimerVersion + "\n"
	if !strings.Contains(string(rewritten), bumpedLine) {
		t.Errorf("the rewritten workspace does not carry %q:\n%s", bumpedLine, rewritten)
	}
	after, err := document.Parse(rewritten, document.YAML)
	if err != nil {
		t.Fatal(err)
	}
	if keys := after.Keys(); len(keys) != len(before.Keys()) {
		t.Fatalf("top-level keys changed: %s", after.Describe())
	}
	for index, key := range before.Keys() {
		if display := after.Keys()[index].Display(); display != key.Display() {
			t.Errorf("key %d moved: %s, want %s", index, display, key.Display())
		}
		if key.Display() == "overrides" {
			continue
		}
		if !document.DeepStrictEqual(after.GetJS(key), before.GetJS(key)) {
			t.Errorf("%s: outside-map value changed", key.Display())
		}
	}
	applied, err := ParseOverrideMap(after.Get("overrides"))
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Equal(reviewed) {
		t.Errorf("overrides = %s", after.Get("overrides").Describe())
	}
	// (a) the bumped selector carries the reviewed value and every other selector
	// still carries the committed one; (b) the key order is the canonical one.
	for _, selector := range selectorOrder {
		version, ok := applied.Get(selector)
		if !ok {
			t.Errorf("%s is missing", selector)
			continue
		}
		want, _ := reviewed.Get(selector)
		if version != want {
			t.Errorf("%s = %s, want %s", selector, version, want)
		}
		if selector == SelectorTimer {
			if version.String() != bumpedTimerVersion {
				t.Errorf("%s = %s, want the bumped %s", selector, version, bumpedTimerVersion)
			}
			continue
		}
		if committedVersion, _ := committed.Get(selector); version != committedVersion {
			t.Errorf("%s = %s, want the committed %s", selector, version, committedVersion)
		}
	}
	emitted := after.Get("overrides").Keys()
	if len(emitted) != len(selectorOrder) {
		t.Fatalf("overrides keys = %d", len(emitted))
	}
	for index, selector := range selectorOrder {
		if emitted[index].Display() != selector.String() {
			t.Errorf("override key %d = %s, want %s", index, emitted[index].Display(), selector)
		}
	}
	// The encoder owns the bytes: the rewrite is deterministic, so the comparator
	// and the staging path derive the same document from the same map.
	again, err := WorkspaceWithOverrides(rewritten, reviewed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, rewritten) {
		t.Errorf("rewrite is not deterministic:\n%s\n---\n%s", rewritten, again)
	}
}

// withSelectorVersion returns the map with one selector moved to another exact
// version, in canonical selector order.
func withSelectorVersion(reviewed OverrideMap, selector Selector, version string) (OverrideMap, error) {
	bumped, err := ParseExactVersion(version)
	if err != nil {
		return OverrideMap{}, err
	}
	entries := make([]OverrideEntry, 0, len(reviewed.Entries()))
	found := false
	for _, item := range reviewed.Entries() {
		if item.Selector == selector {
			item.Version = bumped
			found = true
		}
		entries = append(entries, item)
	}
	if !found {
		return OverrideMap{}, errorf("%s is not in the map", selector)
	}
	return OverrideMap{entries: entries}, nil
}
