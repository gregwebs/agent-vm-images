package artifactinstall

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
)

// gzipped builds one archive from headers, so a control can declare a member
// shape the reviewed pin would not accept.
type entry struct {
	name     string
	body     string
	mode     int64
	typeflag byte
	linkname string
}

func gzipped(t *testing.T, entries []entry) []byte {
	t.Helper()
	var raw bytes.Buffer
	compressed := gzip.NewWriter(&raw)
	writer := tar.NewWriter(compressed)
	for _, item := range entries {
		typeflag := item.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{Name: item.name, Mode: item.mode, Typeflag: typeflag,
			Linkname: item.linkname, Size: int64(len(item.body))}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(item.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func scratchArchive(t *testing.T, raw []byte) openArchive {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return archiveOpener(path, artifactpin.TarGz, "")
}

// preflight refuses each hostile member shape the suite names, with the fragment
// it matches.
func TestPreflightRefusals(t *testing.T) {
	cases := []struct {
		label   string
		entries []entry
		needle  string
	}{
		{
			label:   "an absolute name",
			entries: []entry{{name: "/etc/passwd", body: "x"}},
			needle:  "unusable member name",
		},
		{
			label:   "a parent component",
			entries: []entry{{name: "prefix/../../escape", body: "x"}},
			needle:  "escapes its prefix",
		},
		{
			label:   "a second spelling of one path",
			entries: []entry{{name: "prefix/bin/node", body: "x"}, {name: "prefix/bin/./node", body: "y"}},
			needle:  "not a canonical path",
		},
		{
			label:   "a repeated member",
			entries: []entry{{name: "prefix/bin/node", body: "x"}, {name: "prefix/bin/node", body: "y"}},
			needle:  "repeats a member",
		},
		{
			label: "a write through a link",
			entries: []entry{
				{name: "prefix/bin", body: "", typeflag: tar.TypeDir, mode: 0o755},
				{name: "prefix/bin/b", linkname: "a/../..", typeflag: tar.TypeSymlink, mode: 0o777},
				{name: "prefix/bin/b/escaped", body: "x"},
			},
			needle: "write through the link",
		},
		{
			label:   "a member outside the prefix",
			entries: []entry{{name: "other/bin/node", body: "x"}},
			needle:  "outside",
		},
		{
			label:   "a device node",
			entries: []entry{{name: "prefix/dev", typeflag: tar.TypeChar}},
			needle:  "not a file, directory or link",
		},
		{
			label:   "a setuid bit",
			entries: []entry{{name: "prefix/bin/node", body: "x", mode: 0o4755}},
			needle:  "setuid",
		},
		{
			label: "a hard link to a directory",
			entries: []entry{
				{name: "prefix/bin", typeflag: tar.TypeDir, mode: 0o755},
				{name: "prefix/link", linkname: "prefix/bin", typeflag: tar.TypeLink, mode: 0o644},
			},
			needle: "which is not a file",
		},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			members, err := membersOf(scratchArchive(t, gzipped(t, test.entries)))
			if err != nil {
				t.Fatal(err)
			}
			_, err = preflight(members, "prefix/")
			if err == nil {
				t.Fatal("accepted the hostile archive")
			}
			if !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("refusal does not mention %q: %v", test.needle, err)
			}
		})
	}
}

// A hash-only artifact's reviewed shape is part of its identity: a second member,
// a different mode or a payload past the decoded ceiling is refused from the
// metadata alone, before a byte is extracted.
func TestExactShapeRefusals(t *testing.T) {
	pin := artifactpin.Pin{Name: artifactpin.Agy}
	cases := []struct {
		label   string
		entries []entry
		needle  string
	}{
		{
			label:   "a second member",
			entries: []entry{{name: "antigravity", body: "x", mode: 0o755}, {name: "extra", body: "y", mode: 0o755}},
			needle:  "the reviewed archive carries",
		},
		{
			label:   "a group-writable member",
			entries: []entry{{name: "antigravity", body: "x", mode: 0o775}},
			needle:  "the reviewed archive carries 0755",
		},
	}
	for _, test := range cases {
		t.Run(test.label, func(t *testing.T) {
			members, err := membersOf(scratchArchive(t, gzipped(t, test.entries)))
			if err != nil {
				t.Fatal(err)
			}
			byName, err := preflight(members, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := checkMembers(pin, byName); err == nil ||
				!strings.Contains(err.Error(), test.needle) {
				t.Fatalf("checkMembers reported %v", err)
			}
		})
	}
}

// The reviewed archive's own member is the one member, so the shape check passes
// and the ceiling is the only thing between it and extraction.
func TestMemberCeiling(t *testing.T) {
	_, _, ceiling, reviewed := artifactpin.ExactShape(artifactpin.Agy)
	if !reviewed || ceiling != 512<<20 {
		t.Fatalf("the agy shape is %d/%v", ceiling, reviewed)
	}
	members := []member{{name: "antigravity", isFile: true, mode: 0o755, size: ceiling + 1}}
	if err := checkMembers(artifactpin.Pin{Name: artifactpin.Agy}, map[string]member{"antigravity": members[0]}); err == nil ||
		!strings.Contains(err.Error(), "byte ceiling") {
		t.Fatalf("a payload past the ceiling reported %v", err)
	}
}

// The archive ceilings are per artifact: the one that suits Node would refuse
// agy's real amd64 archive.
func TestArchiveCeilings(t *testing.T) {
	agyRealAMD64 := int64(67_990_723) // agy 1.4.11, the compressed asset.
	if artifactpin.MaxArchiveBytes(artifactpin.Agy) <= agyRealAMD64 {
		t.Fatalf("the agy ceiling is %d", artifactpin.MaxArchiveBytes(artifactpin.Agy))
	}
	for _, name := range []artifactpin.Name{artifactpin.Node, artifactpin.GH} {
		if artifactpin.MaxArchiveBytes(name) != 64<<20 {
			t.Fatalf("%s ceiling is %d", name, artifactpin.MaxArchiveBytes(name))
		}
	}
}

// The event log is what the suite reads to prove the ordering, so the report has
// to be one line per stage, in the order the stages run.
func TestEventOrder(t *testing.T) {
	var log strings.Builder
	events := NewEvents(&log)
	events.Fetch("https://example.invalid/a")
	events.Verify("corroboration")
	events.extract(2)
	events.Publish(3)
	want := "fetch https://example.invalid/a\nverify corroboration\nextract 2\npublish 3\n"
	if log.String() != want {
		t.Fatalf("event log is %q", log.String())
	}
}

// A refusal never publishes: the plan is collected before anything is written, so
// a conflict in the second file leaves the first one alone.
func TestRefusalPublishesNothing(t *testing.T) {
	root := t.TempDir()
	staging := t.TempDir()
	for _, name := range []string{"bin/gh", "share/man/man1/gh.1"} {
		path := filepath.Join(staging, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	installed := filepath.Join(root, "usr", "local", "share", "man", "man1", "gh.1")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := []artifactpin.Member{
		{Archive: "bin/gh", Published: "bin/gh"},
		{Archive: "share/man", Published: "share/man"},
	}
	_, err := planPublication(staging, root, "/usr/local", payload)
	if err == nil || !strings.Contains(err.Error(), "refusing to replace an installed path") {
		t.Fatalf("a publication conflict reported %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "usr", "local", "bin", "gh")); err == nil {
		t.Fatal("the first file was published before the conflict was found")
	}
}

// A symlinked destination ancestor would redirect the whole publication outside
// the root the caller named.
func TestSymlinkedDestinationAncestor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "usr", "local")); err != nil {
		t.Fatal(err)
	}
	err := checkDestination(root, filepath.Join(root, "usr", "local", "bin"))
	if err == nil || !strings.Contains(err.Error(), "symlinked directory") {
		t.Fatalf("a symlinked ancestor reported %v", err)
	}
}
