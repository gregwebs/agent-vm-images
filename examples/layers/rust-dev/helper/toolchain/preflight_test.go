package toolchain

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

// archiveFixture is one component archive under construction.
type archiveFixture struct {
	archive ComponentArchive
}

func newArchiveFixture(pkg string, target TargetTriple) archiveFixture {
	channel := Channel("1.98.1")
	memberPrefix := MemberPrefix(strings.ReplaceAll(pkg, "-preview", "") + "-1.98.1-" +
		string(target) + "/")
	return archiveFixture{archive: ComponentArchive{
		Package: pkg, Target: target,
		Content: Content{URL: CanonicalURL(ExpectedArchiveURL(channel, "2026-01-01", pkg, target)),
			Digest: Sha256(strings.Repeat("0", 64))},
		MemberPrefix: memberPrefix,
	}}
}

func (f archiveFixture) prefix() string { return string(f.archive.MemberPrefix) }
func (f archiveFixture) root() string   { return strings.TrimSuffix(f.prefix(), "/") }
func (f archiveFixture) component() string {
	return PackageComponentName(f.archive.Package, f.archive.Target)
}
func (f archiveFixture) componentRoot() string { return f.prefix() + f.component() + "/" }

func fileHeader(name string, size int64, mode int64) tar.Header {
	return tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: size}
}

func dirHeader(name string, mode int64) tar.Header {
	return tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode}
}

func elfHeader(machine uint16, class, endian byte) []byte {
	header := make([]byte, 20)
	copy(header, "\x7fELF")
	header[4] = class
	header[5] = endian
	header[18] = byte(machine)
	header[19] = byte(machine >> 8)
	return header
}

// metadataEntries are the three rust-installer files every archive must carry.
func (f archiveFixture) metadataEntries(manifest string) ([]tar.Header, map[string][]byte) {
	version := []byte(RustInstallerVersion)
	components := []byte(f.component() + "\n")
	entries := []tar.Header{
		dirHeader(f.root(), 0o755),
		fileHeader(f.prefix()+"rust-installer-version", int64(len(version)), 0o644),
		fileHeader(f.prefix()+"components", int64(len(components)), 0o644),
		dirHeader(f.componentRoot(), 0o755),
		fileHeader(f.componentRoot()+"manifest.in", int64(len(manifest)), 0o644),
	}
	bodies := map[string][]byte{
		f.prefix() + "rust-installer-version": version,
		f.prefix() + "components":             components,
		f.componentRoot() + "manifest.in":     []byte(manifest),
	}
	return entries, bodies
}

// write writes the archive with metadata and the given extra entries.
func (f archiveFixture) write(t *testing.T, manifest string, extra []tar.Header,
	bodies map[string][]byte) string {
	t.Helper()
	return f.writeOmitting(t, manifest, extra, bodies, nil)
}

func (f archiveFixture) writeOmitting(t *testing.T, manifest string, extra []tar.Header,
	bodies map[string][]byte, omit map[string]bool) string {
	t.Helper()
	entries, base := f.metadataEntries(manifest)
	kept := entries[:0]
	for _, entry := range entries {
		if !omit[entry.Name] {
			kept = append(kept, entry)
		}
	}
	entries = append(kept, extra...)
	for name, body := range bodies {
		base[name] = body
	}
	path := filepath.Join(t.TempDir(), f.archive.Content.URL.Filename())
	handle, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	xzWriter, err := xz.NewWriter(handle)
	if err != nil {
		t.Fatal(err)
	}
	writer := tar.NewWriter(xzWriter)
	for index := range entries {
		header := entries[index]
		if body, present := base[header.Name]; present {
			header.Size = int64(len(body))
		}
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatalf("write %q: %v", header.Name, err)
		}
		if body, present := base[header.Name]; present {
			if _, err := writer.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := xzWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f archiveFixture) preflight(t *testing.T, manifest string, extra []tar.Header,
	bodies map[string][]byte) error {
	t.Helper()
	return PreflightArchive(f.write(t, manifest, extra, bodies), f.archive, NewEvents(""))
}

func (f archiveFixture) preflightOmitting(t *testing.T, manifest string, extra []tar.Header,
	bodies map[string][]byte, omit map[string]bool) error {
	t.Helper()
	return PreflightArchive(f.writeOmitting(t, manifest, extra, bodies, omit), f.archive,
		NewEvents(""))
}

func requireRefusal(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("preflight accepted the archive, want %q", fragment)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("refusal is %q, want %q", err, fragment)
	}
}

func cargoPayload(t *testing.T, arch Architecture) ([]tar.Header, map[string][]byte) {
	header := elfHeader(arch.ELFMachine(), 2, 1)
	return []tar.Header{fileHeader("PAYLOAD-bin/cargo", int64(len(header)), 0o755)},
		map[string][]byte{"PAYLOAD-bin/cargo": header}
}

// withPayload rewrites a PAYLOAD- placeholder into the component root.
func withCargo(t *testing.T, f archiveFixture, arch Architecture) ([]tar.Header, map[string][]byte) {
	t.Helper()
	payload, payloadBodies := cargoPayload(t, arch)
	return withPayload(f, payload, payloadBodies)
}

func withPayload(f archiveFixture, entries []tar.Header,
	bodies map[string][]byte) ([]tar.Header, map[string][]byte) {
	outEntries := make([]tar.Header, 0, len(entries))
	outBodies := map[string][]byte{}
	for _, header := range entries {
		header.Name = strings.ReplaceAll(header.Name, "PAYLOAD-", f.componentRoot())
		outEntries = append(outEntries, header)
	}
	for name, body := range bodies {
		outBodies[strings.ReplaceAll(name, "PAYLOAD-", f.componentRoot())] = body
	}
	return outEntries, outBodies
}

func TestPreflightValidCargo(t *testing.T) {
	f := newArchiveFixture("cargo", AMD64.Host())
	payload, payloadBodies := cargoPayload(t, AMD64)
	entries, bodies := withPayload(f, payload, payloadBodies)
	if err := f.preflight(t, "file:bin/cargo\n", entries, bodies); err != nil {
		t.Fatalf("a valid cargo archive was refused: %v", err)
	}
}

func TestPreflightBounds(t *testing.T) {
	t.Run("member_count", func(t *testing.T) {
		f := newArchiveFixture("cargo", AMD64.Host())
		extra := make([]tar.Header, 0, MaxMembers)
		for index := 0; index <= MaxMembers; index++ {
			extra = append(extra, fileHeader(fmt.Sprintf("%spad-%d", f.componentRoot(), index),
				0, 0o644))
		}
		err := f.preflight(t, "file:bin/cargo\n", extra, nil)
		requireRefusal(t, err, "more than 100000 members")
	})
}

func TestPreflightMembers(t *testing.T) {
	arch := AMD64
	cases := []struct {
		name     string
		manifest string
		extra    func(f archiveFixture) ([]tar.Header, map[string][]byte)
		fragment string
	}{
		{"traversal", "file:bin/cargo\n", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{dirHeader(f.prefix()+"ok", 0o755),
				fileHeader(f.prefix()+"ok/../escape", 0, 0o644)}, nil
		}, "non-canonical archive member"},
		{"absolute", "file:bin/cargo\n", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{fileHeader("/abs", 0, 0o644)}, nil
		}, "non-canonical archive member"},
		{"backslash", "file:bin/cargo\n", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{fileHeader(f.prefix()+"a\\b", 0, 0o644)}, nil
		}, "non-canonical archive member"},
		{"outside_prefix", "file:bin/cargo\n",
			func(archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{fileHeader("other/file", 0, 0o644)}, nil
			}, "outside the expected prefix"},
		{"non_dir_root", "file:bin/cargo\n", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return nil, nil
		}, "top-level entry must be a directory"},
		{"duplicate_member", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{fileHeader(f.componentRoot()+"dup", 0, 0o644),
					fileHeader(f.componentRoot()+"dup", 0, 0o644)}, nil
			}, "duplicate archive member"},
		{"duplicate_root", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{dirHeader(f.root(), 0o755)}, nil
			}, "duplicate archive member"},
		{"file_ancestor", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{fileHeader(f.componentRoot()+"a", 0, 0o644),
					fileHeader(f.componentRoot()+"a/b", 0, 0o644)}, nil
			}, "non-directory ancestor"},
		{"linked_ancestor", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{fileHeader(f.componentRoot()+"c", 0, 0o644),
					{Name: f.componentRoot() + "a", Typeflag: tar.TypeSymlink, Linkname: "c",
						Mode: 0o777},
					fileHeader(f.componentRoot()+"a/b", 0, 0o644)}, nil
			}, "non-directory ancestor"},
		{"setuid_file", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{fileHeader(f.componentRoot()+"s", 0, 0o4755)}, nil
			}, "setuid/setgid"},
		{"setgid_dir", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{dirHeader(f.componentRoot()+"d", 0o2755)}, nil
			}, "setuid/setgid"},
		{"setgid_root", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{dirHeader(f.root(), 0o2755)}, nil
			}, "setuid/setgid"},
		{"setuid_symlink", "file:bin/cargo\n",
			func(f archiveFixture) ([]tar.Header, map[string][]byte) {
				return []tar.Header{fileHeader(f.componentRoot()+"c", 0, 0o644),
					{Name: f.componentRoot() + "l", Typeflag: tar.TypeSymlink, Linkname: "c",
						Mode: 0o4777}}, nil
			}, "setuid/setgid"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newArchiveFixture("cargo", arch.Host())
			extra, bodies := test.extra(f)
			if test.name == "non_dir_root" {
				extra = []tar.Header{fileHeader(f.root(), 0, 0o644)}
				requireRefusal(t, f.preflightOmitting(t, test.manifest, extra, bodies,
					map[string]bool{f.root(): true}), test.fragment)
				return
			}
			requireRefusal(t, f.preflight(t, test.manifest, extra, bodies), test.fragment)
		})
	}
}

// TestNonCanonicalMemberSegments covers the member-name rule a conforming tar
// writer cannot express: archive/tar refuses to encode a regular member whose
// name ends in "/". The empty final segment is what refuses it; a separate
// trailing-slash test would be a redundant second spelling of the same rule.
func TestNonCanonicalMemberSegments(t *testing.T) {
	for _, name := range []string{"prefix/a/", "prefix//a", "prefix/./a", "prefix/../a"} {
		requireRefusal(t, requireCanonicalMember(name, "cargo.tar.xz"),
			"non-canonical archive member")
	}
	if err := requireCanonicalMember("prefix/a", "cargo.tar.xz"); err != nil {
		t.Fatalf("a canonical member was refused: %v", err)
	}
}

func TestPreflightLinks(t *testing.T) {
	arch := AMD64
	cases := []struct {
		name     string
		extra    func(f archiveFixture) ([]tar.Header, map[string][]byte)
		manifest string
		fragment string
	}{
		{"special_fifo", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{{Name: f.componentRoot() + "pipe", Typeflag: tar.TypeFifo,
				Mode: 0o644}}, nil
		}, "file:bin/cargo\n", "special archive member"},
		{"symlink_escape", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{{Name: f.componentRoot() + "l", Typeflag: tar.TypeSymlink,
				Linkname: "../../etc/passwd", Mode: 0o777}}, nil
		}, "file:bin/cargo\n", "link escapes its tree"},
		{"symlink_forward", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{{Name: f.componentRoot() + "l", Typeflag: tar.TypeSymlink,
				Linkname: "target", Mode: 0o777},
				fileHeader(f.componentRoot()+"target", 0, 0o644)}, nil
		}, "file:bin/cargo\n", "not an earlier regular member"},
		{"symlink_to_dir", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{dirHeader(f.componentRoot()+"d", 0o755),
				{Name: f.componentRoot() + "l", Typeflag: tar.TypeSymlink, Linkname: "d",
					Mode: 0o777}}, nil
		}, "file:bin/cargo\n", "not an earlier regular member"},
		{"symlink_self", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{{Name: f.componentRoot() + "l", Typeflag: tar.TypeSymlink,
				Linkname: "l", Mode: 0o777}}, nil
		}, "file:bin/cargo\n", "not an earlier regular member"},
		{"hardlink_outside_prefix", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{{Name: f.componentRoot() + "l", Typeflag: tar.TypeLink,
				Linkname: "other/x", Mode: 0o644}}, nil
		}, "file:bin/cargo\n", "link escapes its tree"},
		{"hardlink_forward", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{{Name: f.componentRoot() + "l", Typeflag: tar.TypeLink,
				Linkname: f.componentRoot() + "target", Mode: 0o644},
				fileHeader(f.componentRoot()+"target", 0, 0o644)}, nil
		}, "file:bin/cargo\n", "not an earlier regular member"},
		{"hardlink_to_dir", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{dirHeader(f.componentRoot()+"d", 0o755),
				{Name: f.componentRoot() + "l", Typeflag: tar.TypeLink,
					Linkname: f.componentRoot() + "d", Mode: 0o644}}, nil
		}, "file:bin/cargo\n", "not an earlier regular member"},
		{"valid_earlier_symlink", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{fileHeader(f.componentRoot()+"target", 0, 0o644),
				{Name: f.componentRoot() + "l", Typeflag: tar.TypeSymlink, Linkname: "target",
					Mode: 0o777}}, nil
		}, "file:bin/cargo\nfile:l\nfile:target\n", ""},
		{"valid_earlier_hardlink", func(f archiveFixture) ([]tar.Header, map[string][]byte) {
			return []tar.Header{fileHeader(f.componentRoot()+"target", 0, 0o644),
				{Name: f.componentRoot() + "l", Typeflag: tar.TypeLink,
					Linkname: f.componentRoot() + "target", Mode: 0o644}}, nil
		}, "file:bin/cargo\nfile:l\nfile:target\n", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			f := newArchiveFixture("cargo", arch.Host())
			extra, bodies := test.extra(f)
			if strings.HasPrefix(test.name, "valid_") {
				payload, payloadBodies := withCargo(t, f, arch)
				extra = append(payload, extra...)
				if bodies == nil {
					bodies = map[string][]byte{}
				}
				for name, body := range payloadBodies {
					bodies[name] = body
				}
			}
			err := f.preflight(t, test.manifest, extra, bodies)
			if test.fragment == "" {
				if err != nil {
					t.Fatalf("valid link was refused: %v", err)
				}
				return
			}
			requireRefusal(t, err, test.fragment)
		})
	}
}

func TestPreflightMetadata(t *testing.T) {
	arch := AMD64
	t.Run("file_too_large", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		body := bytes.Repeat([]byte("x"), int(MaxMetadataFileBytes)+1)
		payload, payloadBodies := cargoPayload(t, arch)
		extra, bodies := withPayload(f, payload, payloadBodies)
		bodies[f.componentRoot()+"manifest.in"] = body
		requireRefusal(t, f.preflight(t, "file:bin/cargo\n", extra, bodies),
			"is not a bounded regular file")
	})
	t.Run("missing_version", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		requireRefusal(t, f.preflightOmitting(t, "file:bin/cargo\n", extra, bodies,
			map[string]bool{f.prefix() + "rust-installer-version": true}),
			"missing rust-installer-version")
	})
	t.Run("wrong_version", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		bodies[f.prefix()+"rust-installer-version"] = []byte("2\n")
		requireRefusal(t, f.preflight(t, "file:bin/cargo\n", extra, bodies),
			"rust-installer-version must be 3")
	})
	t.Run("missing_components", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		requireRefusal(t, f.preflightOmitting(t, "file:bin/cargo\n", extra, bodies,
			map[string]bool{f.prefix() + "components": true}), "missing components")
	})
	t.Run("wrong_component", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		bodies[f.prefix()+"components"] = []byte("rustc\n")
		requireRefusal(t, f.preflight(t, "file:bin/cargo\n", extra, bodies), "component list is")
	})
	t.Run("missing_manifest_in", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		requireRefusal(t, f.preflightOmitting(t, "file:bin/cargo\n", extra, bodies,
			map[string]bool{f.componentRoot() + "manifest.in": true}), "missing cargo/manifest.in")
	})
	t.Run("missing_payload", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		requireRefusal(t, f.preflight(t, "file:bin/cargo\nfile:bin/other\n", extra, bodies),
			"missing payload path")
	})
	t.Run("file_dir_mismatch", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		requireRefusal(t, f.preflight(t, "dir:bin/cargo\n", extra, bodies),
			"dir directive names a non-directory")
	})
	t.Run("uncovered_payload", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		extra = append(extra, fileHeader(f.componentRoot()+"extra.txt", 1, 0o644))
		bodies[f.componentRoot()+"extra.txt"] = []byte("x")
		requireRefusal(t, f.preflight(t, "file:bin/cargo\n", extra, bodies),
			"not covered by manifest.in")
	})
	t.Run("empty_unreferenced_dir_passes", func(t *testing.T) {
		f := newArchiveFixture("cargo", arch.Host())
		extra, bodies := withCargo(t, f, arch)
		extra = append(extra, dirHeader(f.componentRoot()+"empty", 0o755))
		if err := f.preflight(t, "file:bin/cargo\n", extra, bodies); err != nil {
			t.Fatalf("an empty unreferenced directory was refused: %v", err)
		}
	})
	t.Run("total_guard", func(t *testing.T) {
		// The three named metadata files cannot together exceed the 8 MiB total
		// (each is capped at 2 MiB), so the guard is exercised directly with an
		// already-near-cap total rather than through an unreachable archive.
		total := MaxMetadataTotalBytes - 1
		_, err := readMetadata(bytes.NewReader([]byte("ab")),
			archiveMember{name: "rust-installer-version", size: 2, kind: regularMember},
			&total, "cargo")
		requireRefusal(t, err, "rust-installer metadata exceeds")
	})
}

func TestParseDirectives(t *testing.T) {
	long := strings.Repeat("a", MaxLineBytes+1)
	control := "file\x01x"
	tooMany := strings.Repeat("file:a\n", MaxMetadataLines+1)
	cases := []struct {
		name     string
		raw      string
		fragment string
	}{
		{"bom", "\xef\xbb\xbffile:a\n", "leading BOM"},
		{"utf8", "file:\xff\n", "not valid UTF-8"},
		{"crlf", "file:a\r\n", "LF-terminated"},
		{"line_count", tooMany, "line count out of bounds"},
		{"line_length", "file:" + long + "\n", "line too long"},
		{"control_char", "file:" + control + "\n", "control character"},
		{"unknown_directive", "symlink:a\n", "unknown directive"},
		{"absolute", "file:/etc/passwd\n", "non-canonical directive path"},
		{"colon", "file:a:b\n", "non-canonical directive path"},
		{"traversal", "file:../x\n", "non-canonical directive path"},
		{"duplicate", "file:a\nfile:a\n", "duplicate directive"},
		{"file_under_file", "file:a\nfile:a/b\n", "overlapping directives"},
		{"dir_under_dir", "dir:a\ndir:a/b\n", "overlapping directory directives"},
		{"file_and_dir", "file:a\ndir:a\n", "is both file: and dir:"},
		{"file_under_dir", "dir:a\nfile:a/b\n", "covered twice"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseDirectives([]byte(test.raw), "cargo", "cargo.tar.xz")
			requireRefusal(t, err, test.fragment)
		})
	}
}

func TestELFIdentity(t *testing.T) {
	cases := []struct {
		name    string
		header  []byte
		wantErr string
	}{
		{"not_elf", []byte("not an elf binary at all!"), "not an ELF binary"},
		{"32bit", elfHeader(62, 1, 1), "not a 64-bit little-endian ELF"},
		{"big_endian", elfHeader(62, 2, 2), "not a 64-bit little-endian ELF"},
		{"wrong_machine", elfHeader(183, 2, 1), ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			machine, err := CheckELF(test.header)
			if test.wantErr != "" {
				requireRefusal(t, err, test.wantErr)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if machine != 183 {
				t.Fatalf("machine is %d, want 183", machine)
			}
		})
	}
}

func TestPreflightPayloadIdentity(t *testing.T) {
	t.Run("missing_executable", func(t *testing.T) {
		f := newArchiveFixture("cargo", AMD64.Host())
		extra := []tar.Header{fileHeader(f.componentRoot()+"bin/other", 0, 0o755)}
		requireRefusal(t, f.preflight(t, "file:bin/other\n", extra, nil),
			"missing executable bin/cargo")
	})
	t.Run("wrong_arch_elf", func(t *testing.T) {
		f := newArchiveFixture("cargo", AMD64.Host())
		header := elfHeader(183, 2, 1)
		extra := []tar.Header{fileHeader(f.componentRoot()+"bin/cargo", int64(len(header)), 0o755)}
		requireRefusal(t, f.preflight(t, "file:bin/cargo\n", extra,
			map[string][]byte{f.componentRoot() + "bin/cargo": header}),
			"targets ELF machine 183, expected 62")
	})
	t.Run("rust_std_wrong_target", func(t *testing.T) {
		f := newArchiveFixture("rust-std", AMD64.Musl())
		extra := []tar.Header{dirHeader(f.componentRoot()+"lib/rustlib", 0o755),
			dirHeader(f.componentRoot()+"lib/rustlib/aarch64-unknown-linux-musl", 0o755),
			dirHeader(f.componentRoot()+"lib/rustlib/aarch64-unknown-linux-musl/lib", 0o755),
			fileHeader(f.componentRoot()+
				"lib/rustlib/aarch64-unknown-linux-musl/lib/libstd.rlib", 0, 0o644)}
		requireRefusal(t, f.preflight(t, "file:lib/rustlib/aarch64-unknown-linux-musl/lib/libstd.rlib\n",
			extra, nil), "no x86_64-unknown-linux-musl standard library")
	})
}
