package installstatus

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRecordVocabulary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		data      string
		kind      RecordKind
		code      string
		wantValid bool
	}{
		{name: "installed", data: "installed\n", kind: Installed, wantValid: true},
		{name: "pending", data: "pending\n", kind: Pending, wantValid: true},
		{name: "absent-pi", data: "absent-pi\n", kind: AbsentPi, wantValid: true},
		{name: "absent-curl-5", data: "absent-transport 5\n", kind: AbsentTransport, code: "5", wantValid: true},
		{name: "absent-curl-60", data: "absent-transport 60\n", kind: AbsentTransport, code: "60", wantValid: true},
		// npm error codes retired with run-npm.sh. They are not translated into
		// curl codes and not kept as a second vocabulary: a record naming one
		// authorises nothing, exactly like any other unknown code.
		{name: "retired-npm-eai-again", data: "absent-transport EAI_AGAIN\n"},
		{name: "retired-npm-econnreset", data: "absent-transport ECONNRESET\n"},
		{name: "retired-npm-etimedout", data: "absent-transport ETIMEDOUT\n"},
		{name: "no-newline", data: "installed"},
		{name: "two-lines", data: "installed\ninstalled\n"},
		{name: "blank", data: "\n"},
		{name: "empty", data: ""},
		{name: "crlf", data: "installed\r\n"},
		{name: "leading-space", data: " installed\n"},
		{name: "trailing-space", data: "installed \n"},
		{name: "nul", data: "installed\x00\n"},
		{name: "bom", data: "\xef\xbb\xbfinstalled\n"},
		{name: "non-ascii", data: "installed\xc3\xa9\n"},
		{name: "case", data: "Installed\n"},
		{name: "unknown-code", data: "absent-transport 999\n"},
		{name: "lowercase-code", data: "absent-transport eai_again\n"},
		{name: "leading-zero", data: "absent-transport 05\n"},
		{name: "prefix-only", data: "absent-transport \n"},
		{name: "double-space", data: "absent-transport  28\n"},
		{name: "bare-transport", data: "absent-transport\n"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			record, err := ParseRecord([]byte(test.data))
			if !test.wantValid {
				if err == nil {
					t.Fatalf("ParseRecord(%q) = valid, want invalid", test.data)
				}
				if record.Text() != "" {
					t.Fatalf("invalid record rendered %q", record.Text())
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRecord(%q) = %v, want valid", test.data, err)
			}
			if record.Kind() != test.kind {
				t.Fatalf("kind = %d, want %d", record.Kind(), test.kind)
			}
			code, ok := record.Code()
			if test.kind == AbsentTransport {
				if !ok || code.String() != test.code {
					t.Fatalf("code = %q, %v, want %q", code, ok, test.code)
				}
				return
			}
			if ok {
				t.Fatalf("code = %q for a non-transport record", code)
			}
		})
	}
}

func TestReadRecordFailuresStayIndistinguishable(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing")
	if _, err := ReadRecord(missing); err == nil {
		t.Fatal("missing record read as valid")
	}
	if _, err := ReadRecord(directory); err == nil {
		t.Fatal("directory read as a valid record")
	}
}

func TestClassifyPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustMkdir := func(name string, mode os.FileMode) string {
		path := filepath.Join(root, name)
		if err := os.Mkdir(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writeFile := func(path string, mode os.FileMode) {
		if err := os.WriteFile(path, []byte("x\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink := func(target, name string) string {
		path := filepath.Join(root, name)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		return path
	}

	directory := mustMkdir("dir", 0o755)
	writeFile(filepath.Join(directory, "file"), 0o644)
	writeFile(filepath.Join(root, "plain"), 0o644)
	mustMkdir("closed", 0o500)
	symlinkLeaf := mustSymlink("plain", "link-file")
	symlinkDangling := mustSymlink("nowhere", "link-dangling")
	symlinkLoop := mustSymlink("loop-b", "loop-a")
	mustSymlink("loop-a", "loop-b")
	symlinkDirectory := mustSymlink("dir", "link-dir")
	symlinkToFile := mustSymlink("plain", "link-to-file")

	other := mustMkdir("outside", 0o755)

	cases := []struct {
		name string
		path string
		want PathState
	}{
		{name: "empty", path: "", want: Indeterminate},
		{name: "root", path: "/", want: Indeterminate},
		{name: "double-slash", path: "//", want: Present},
		{name: "quad-slash", path: "////", want: Present},
		{name: "dot", path: ".", want: Present},
		{name: "dotdot", path: "..", want: Indeterminate},
		{name: "plain", path: filepath.Join(root, "plain"), want: Present},
		{name: "directory", path: directory, want: Present},
		{name: "missing-leaf", path: filepath.Join(root, "absent"), want: Absent},
		{name: "missing-prefix", path: filepath.Join(root, "absent", "leaf"), want: Absent},
		{name: "missing-before-dotdot", path: root + "/absent/../plain", want: Absent},
		{name: "dotdot-decided-late", path: root + "/dir/../plain", want: Indeterminate},
		{name: "trailing-slash-directory", path: directory + "/", want: Present},
		{name: "trailing-slash-file", path: filepath.Join(root, "plain") + "/", want: Present},
		{name: "symlink-leaf", path: symlinkLeaf, want: Present},
		{name: "symlink-dangling", path: symlinkDangling, want: Present},
		{name: "symlink-loop", path: symlinkLoop, want: Present},
		{name: "symlink-directory-ancestor", path: filepath.Join(symlinkDirectory, "file"), want: Present},
		{name: "symlink-file-ancestor", path: filepath.Join(symlinkToFile, "leaf"), want: Present},
		{name: "dangling-symlink-ancestor", path: filepath.Join(symlinkDangling, "leaf"), want: Present},
		{name: "symlink-loop-ancestor", path: filepath.Join(symlinkLoop, "leaf"), want: Indeterminate},
		{name: "non-directory-ancestor", path: filepath.Join(root, "plain", "leaf"), want: Present},
		{name: "unreadable-directory-ancestor", path: filepath.Join(root, "closed", "absent"), want: Absent},
		{name: "other-directory", path: filepath.Join(other, "leaf"), want: Absent},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyPath(test.path); got != test.want {
				t.Fatalf("ClassifyPath(%q) = %d, want %d", test.path, got, test.want)
			}
		})
	}
}

func TestUninitialisedPathStateAuthorisesNothing(t *testing.T) {
	var state PathState
	if state != Indeterminate {
		t.Fatalf("zero state = %v, want Indeterminate", state)
	}
}
