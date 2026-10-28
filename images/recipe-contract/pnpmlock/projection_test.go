package pnpmlock

import (
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// These are hand-authored vectors, not snapshots of the emitter: each expected
// JSON literal is written from the projection contract in the Phase 2 graph work
// so a changed default or key order fails here first.
func archivesOf(t *testing.T, source string) ([]Archive, error) {
	t.Helper()
	value, err := document.Parse([]byte(source), document.JSON)
	if err != nil {
		t.Fatal(err)
	}
	return readArchives(value)
}

func TestArchiveProjectionDefaults(t *testing.T) {
	archives, err := archivesOf(t, `{
		"@example/native@1.2.3": {
			"resolution": {
				"integrity": "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
			}
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("archives = %d", len(archives))
	}
	archive := archives[0]
	if archive.HasBin || archive.Optional || !archive.Platform.OS.Empty() ||
		!archive.Platform.CPU.Empty() || !archive.Platform.LibC.Empty() {
		t.Fatalf("defaults = %+v", archive)
	}
	want := `{"id":{"name":"@example/native","version":"1.2.3"},` +
		`"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",` +
		`"tarball":"https://registry.npmjs.org/@example/native/-/native-1.2.3.tgz",` +
		`"peers":{},"optionalPeers":[],"hasBin":false,` +
		`"platform":{"os":[],"cpu":[],"libc":[]},"optional":false}`
	if got := string(archive.Value().AppendJS(nil)); got != want {
		t.Fatalf("archive = %s\nwant      %s", got, want)
	}
}

func TestArchiveProjectionExplicit(t *testing.T) {
	archives, err := archivesOf(t, `{
		"@example/native@1.2.3": {
			"resolution": {
				"integrity": "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
			},
			"hasBin": true,
			"optional": true,
			"os": ["linux", "!darwin", "any", "!any", "linux"],
			"cpu": ["arm64"],
			"libc": ["glibc", "musl"]
		}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":{"name":"@example/native","version":"1.2.3"},` +
		`"integrity":"sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",` +
		`"tarball":"https://registry.npmjs.org/@example/native/-/native-1.2.3.tgz",` +
		`"peers":{},"optionalPeers":[],"hasBin":true,` +
		`"platform":{"os":["linux","!darwin","any","!any","linux"],"cpu":["arm64"],"libc":["glibc","musl"]},` +
		`"optional":true}`
	if got := string(archives[0].Value().AppendJS(nil)); got != want {
		t.Fatalf("archive = %s\nwant      %s", got, want)
	}
	// The retained list keeps its raw order and duplicates; `any` and `!any` stay
	// literal tokens because the omission evaluator, not the parser, decides them.
	os := archives[0].Platform.OS.Selectors()
	if len(os) != 5 || os[1].Negated() != true || os[1].Token() != "darwin" {
		t.Fatalf("os = %+v", os)
	}
}

func TestArchiveProjectionRejections(t *testing.T) {
	for _, vector := range []struct {
		name   string
		fields string
		match  string
	}{
		{"hasbin-null", `"hasBin": null`, "hasBin: expected boolean"},
		{"hasbin-string", `"hasBin": "true"`, "hasBin: expected boolean"},
		{"hasbin-number", `"hasBin": 1`, "hasBin: expected boolean"},
		{"hasbin-object", `"hasBin": {}`, "hasBin: expected boolean"},
		{"optional-null", `"optional": null`, "optional: expected boolean"},
		{"optional-string", `"optional": "false"`, "optional: expected boolean"},
		{"os-scalar", `"os": "linux"`, "platform.os: expected sequence"},
		{"os-null", `"os": null`, "platform.os: expected sequence"},
		{"os-number-item", `"os": [1]`, "platform.os[0]: expected string"},
		{"os-empty-token", `"os": [""]`, "malformed platform token"},
		{"os-bare-negation", `"os": ["!"]`, "malformed platform token"},
		{"os-double-negation", `"os": ["!!linux"]`, "malformed platform token"},
		{"os-leading-space", `"os": [" linux"]`, "malformed platform token"},
		{"os-trailing-newline", `"os": ["linux\n"]`, "malformed platform token"},
		{"os-path", `"os": ["../linux"]`, "malformed platform token"},
		{"os-slash", `"os": ["linux/arm64"]`, "malformed platform token"},
		{"os-uppercase", `"os": ["Linux"]`, "malformed platform token"},
		{"cpu-bool", `"cpu": true`, "platform.cpu: expected sequence"},
		{"libc-object", `"libc": {}`, "platform.libc: expected sequence"},
	} {
		t.Run(vector.name, func(t *testing.T) {
			_, err := archivesOf(t, `{"@example/native@1.2.3": {"resolution": {`+
				`"integrity": "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="},`+
				vector.fields+`}}`)
			if err == nil || !strings.Contains(err.Error(), vector.match) {
				t.Fatalf("%s = %v, want %q", vector.name, err, vector.match)
			}
		})
	}
}

func TestSnapshotProjectionOptional(t *testing.T) {
	archives, err := document.Parse([]byte(`{
		"@example/native@1.2.3": {
			"resolution": {
				"integrity": "sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=="
			}
		}
	}`), document.JSON)
	if err != nil {
		t.Fatal(err)
	}
	snapshots, err := document.Parse([]byte(`{
		"@example/native@1.2.3": {},
		"@example/native@1.2.3(@example/peer@1.0.0)": {"optional": true},
		"@example/native@1.2.3(@example/peer@2.0.0)": {}
	}`), document.JSON)
	if err != nil {
		t.Fatal(err)
	}
	read, err := readSnapshots(snapshots, archives)
	if err != nil {
		t.Fatal(err)
	}
	// Two peer contexts of one archive keep independent optional facts; the edge
	// flag is a third, separate fact this vector does not touch.
	for _, snapshot := range read {
		want := snapshot.ID.Key == "@example/native@1.2.3(@example/peer@1.0.0)"
		if snapshot.Optional != want {
			t.Errorf("%s optional = %v, want %v", snapshot.ID.Key, snapshot.Optional, want)
		}
		emitted := string(snapshot.Value().AppendJS(nil))
		if !strings.HasSuffix(emitted, `"optional":`+boolText(want)+`}`) {
			t.Errorf("%s = %s", snapshot.ID.Key, emitted)
		}
	}
	for _, source := range []string{`{"optional": null}`, `{"optional": "true"}`, `{"optional": 1}`} {
		one, err := document.Parse([]byte(`{"@example/native@1.2.3": `+source+`}`), document.JSON)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readSnapshots(one, archives); err == nil || !strings.Contains(err.Error(), "optional: expected boolean") {
			t.Errorf("%s = %v", source, err)
		}
	}
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
