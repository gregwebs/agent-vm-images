package pnpm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// member is one archive entry the cases assemble. A declared size with no body is
// how a case writes a member whose content would not fit in a test's memory: the
// writer streams zeros for exactly that many bytes.
type member struct {
	name     string
	body     []byte
	typeflag byte
	linkname string
	declared int64
}

func regular(name string, body string) member {
	return member{name: name, body: []byte(body), typeflag: tar.TypeReg}
}

// reviewedMembers is a pnpm tarball of the reviewed shape: one package/ prefix,
// the four upstream files, and a package.json that selects this pin's version.
func reviewedMembers() []member {
	return []member{
		regular("package/package.json", `{"name":"pnpm","version":"11.11.0"}`),
		regular("package/bin/pnpm.mjs", "upstream pnpm entry"),
		regular("package/bin/pnpx.mjs", "upstream alias entry"),
		regular("package/dist/pnpm.mjs", "pristine fixture bundle"),
		regular("package/LICENSE", "MIT License"),
	}
}

// writeArchive writes the members as a gzipped tarball and returns its path.
func writeArchive(t *testing.T, members []member) string {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range members {
		size := int64(len(entry.body))
		if entry.declared > size {
			size = entry.declared
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o644,
			Size:     size,
			Typeflag: entry.typeflag,
			Linkname: entry.linkname,
		}
		if entry.typeflag == tar.TypeDir || entry.typeflag == tar.TypeSymlink || entry.typeflag == tar.TypeChar {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("cannot write archive header %q: %v", entry.name, err)
		}
		switch {
		case header.Size == 0:
		case len(entry.body) > 0:
			if _, err := tarWriter.Write(entry.body); err != nil {
				t.Fatalf("cannot write archive member %q: %v", entry.name, err)
			}
		default:
			if _, err := io.CopyN(tarWriter, endlessZeros{}, header.Size); err != nil {
				t.Fatalf("cannot stream archive member %q: %v", entry.name, err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("cannot close the archive: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("cannot close the archive stream: %v", err)
	}
	path := filepath.Join(t.TempDir(), "pnpm.tgz")
	if err := os.WriteFile(path, compressed.Bytes(), 0o600); err != nil {
		t.Fatalf("cannot write the archive: %v", err)
	}
	return path
}

// archivePin derives the pin the reviewed archive implies, so a case can change
// the archive and keep the authentication honest about the firmware's own bytes.
func archivePin(t *testing.T, archivePath string, members []member) Pin {
	t.Helper()
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("cannot read the fixture archive: %v", err)
	}
	bundle := []byte("pristine fixture bundle")
	for _, entry := range members {
		if entry.name == "package/dist/pnpm.mjs" {
			bundle = entry.body
		}
	}
	pin, err := LoadPin(writePin(t, map[string]any{
		"version":        "11.11.0",
		"url":            "https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz",
		"integrity":      "sha512-" + base64Of(Sum512(raw)),
		"sha512":         Sum512(raw),
		"bundleSha256":   Sum256(bundle),
		"upstreamTag":    "v11.11.0",
		"upstreamCommit": strings.Repeat("b", 40),
	}))
	if err != nil {
		t.Fatalf("cannot build the fixture pin: %v", err)
	}
	return pin
}

func TestExtract(t *testing.T) {
	cases := []struct {
		name   string
		change func(*[]member)
		// pinChange runs after the archive is written, so a case can make the pin
		// disagree with the bytes it authenticated.
		pinChange func(*Pin)
		want      string
	}{
		{name: "reviewed archive"},
		{
			name:   "single package prefix",
			change: func(members *[]member) { *members = append(*members, regular("other/index.js", "x")) },
			want:   "unsafe archive member: other/index.js",
		},
		{
			name:   "no parent traversal",
			change: func(members *[]member) { *members = append(*members, regular("package/../../outside", "x")) },
			want:   "unsafe archive member: package/../../outside",
		},
		{
			name:   "no absolute names",
			change: func(members *[]member) { *members = append(*members, regular("/package/outside", "x")) },
			want:   "unsafe archive member: /package/outside",
		},
		{
			name:   "no duplicate members",
			change: func(members *[]member) { *members = append(*members, regular("package/LICENSE", "MIT License")) },
			want:   "unsafe archive member: package/LICENSE",
		},
		{
			name: "no links",
			change: func(members *[]member) {
				*members = append(*members, member{name: "package/link", typeflag: tar.TypeSymlink, linkname: "/tmp"})
			},
			want: "unsafe archive member: package/link",
		},
		{
			name: "no devices",
			change: func(members *[]member) {
				*members = append(*members, member{name: "package/dev", typeflag: tar.TypeChar})
			},
			want: "unsafe archive member: package/dev",
		},
		{
			name:   "no empty member name",
			change: func(members *[]member) { *members = append(*members, regular("", "x")) },
			want:   "unsafe archive member: ",
		},
		{
			name: "the selection matches the pin",
			change: func(members *[]member) {
				(*members)[0] = regular("package/package.json", `{"name":"pnpm","version":"1.0.0"}`)
			},
			want: "package selection mismatch",
		},
		{
			name: "every required upstream file is present",
			change: func(members *[]member) {
				*members = append((*members)[:2:2], (*members)[3:]...)
			},
			want: "missing regular upstream file: bin/pnpx.mjs",
		},
		{
			name: "the license is present",
			change: func(members *[]member) {
				*members = append((*members)[:4:4], (*members)[5:]...)
			},
			want: "missing regular upstream file: LICENSE",
		},
		{
			name: "the license is MIT",
			change: func(members *[]member) {
				(*members)[4] = regular("package/LICENSE", "All rights reserved")
			},
			want: "upstream MIT license missing",
		},
		{
			name:      "the bundle is pristine",
			pinChange: func(pin *Pin) { pin.BundleSHA256 = "0" + pin.BundleSHA256[1:] },
			want:      "pristine bundle SHA-256 mismatch",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			members := reviewedMembers()
			if testCase.change != nil {
				testCase.change(&members)
			}
			archive := writeArchive(t, members)
			pin := archivePin(t, archive, members)
			if testCase.pinChange != nil {
				testCase.pinChange(&pin)
			}
			scratch := t.TempDir()
			packageDir, err := extract(archive, scratch, pin)
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("extract refused a reviewed archive: %v", err)
				}
				published, err := os.ReadFile(filepath.Join(packageDir, "dist/pnpm.mjs"))
				if err != nil {
					t.Fatalf("cannot read the extracted bundle: %v", err)
				}
				// Publication never edits upstream bytes: the extracted bundle is the
				// archive member, byte for byte.
				if string(published) != "pristine fixture bundle" {
					t.Fatalf("extracted bundle is %q", published)
				}
				return
			}
			if err == nil {
				t.Fatalf("extract accepted an archive it must refuse: want %q", testCase.want)
			}
			if err.Error() != testCase.want {
				t.Fatalf("refusal is %q, want %q", err.Error(), testCase.want)
			}
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("refusal %v is not a *Refusal", err)
			}
		})
	}
}

// TestArchiveCeilings is the difference between bounding a download and bounding
// extraction: a bomb and a member flood are refused before anything is unpacked.
func TestArchiveCeilings(t *testing.T) {
	t.Run("an expanded-size bomb is refused", func(t *testing.T) {
		members := reviewedMembers()
		// A single member that claims more than the expanded ceiling. The archive
		// itself stays small, so only the decompressed bound can catch it.
		members = append(members, member{
			name:     "package/bomb.bin",
			typeflag: tar.TypeReg,
			declared: MaxExpandedBytes + 1,
		})
		archive := writeArchive(t, members)
		pin := archivePin(t, archive, members)
		_, err := extract(archive, t.TempDir(), pin)
		if err == nil || err.Error() != "the archive expands past the 536870912 byte ceiling" {
			t.Fatalf("refusal is %v, want the expanded-size ceiling", err)
		}
	})
	t.Run("a member flood is refused", func(t *testing.T) {
		members := reviewedMembers()
		for index := 0; index <= MaxMembers; index++ {
			members = append(members, regular("package/flood/"+itoa(index), "x"))
		}
		archive := writeArchive(t, members)
		pin := archivePin(t, archive, members)
		_, err := extract(archive, t.TempDir(), pin)
		if err == nil || err.Error() != "the archive carries more than 20000 members" {
			t.Fatalf("refusal is %v, want the member ceiling", err)
		}
	})
	t.Run("the file seam applies the compressed ceiling", func(t *testing.T) {
		oversized := filepath.Join(t.TempDir(), "oversized.tgz")
		if err := os.WriteFile(oversized, make([]byte, MaxArchiveBytes+1), 0o600); err != nil {
			t.Fatalf("cannot write the oversized archive: %v", err)
		}
		_, err := readArchiveFile(oversized, "archive")
		if err == nil || err.Error() != oversized+" is larger than the 67108864 byte ceiling" {
			t.Fatalf("refusal is %v, want the compressed ceiling", err)
		}
	})
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
