package toolaccess

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

type fixture struct {
	root string
	cwd  string
}

func (f fixture) path(name string) string { return filepath.Join(f.cwd, name) }

func (f fixture) write(name, content string, mode os.FileMode) string {
	path := f.path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		panic(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		panic(err)
	}
	return path
}

func (f fixture) mkdir(name string, mode os.FileMode) string {
	path := f.path(name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		panic(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		panic(err)
	}
	return path
}

func (f fixture) symlink(target, name string) string {
	path := f.path(name)
	if err := os.Symlink(target, path); err != nil {
		panic(err)
	}
	return path
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	// os.TempDir() is world-traversable; t.TempDir() may sit beneath a 0700
	// parent, which every audit would reject before reaching the fixture.
	root, err := os.MkdirTemp("", "recipe-contract-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		releaseTree(root)
		_ = os.RemoveAll(root)
	})
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// The provider must hand out the physical path: a lexical spelling through a
	// symlinked /tmp would add a symlink hop the audit has to count.
	physical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(physical, "work")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return fixture{root: root, cwd: cwd}
}

// releaseTree restores owner permissions so cleanup can remove audit fixtures.
func releaseTree(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
}

func (f fixture) environment() Environment {
	return Environment{
		WorkingDirectory: func() (PhysicalDirectory, error) { return PhysicalDirectoryOf(f.cwd), nil },
		Path:             PathValue("/usr/bin:/bin"),
	}
}

func (f fixture) audit(t *testing.T, target string, mode Mode) (ResolvedPath, error) {
	t.Helper()
	return NewAuditor(f.environment()).Audit(Target(target), mode)
}

func violationOf(t *testing.T, err error) *Violation {
	t.Helper()
	var failure *Violation
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a Violation", err)
	}
	return failure
}

func TestExecutablePredicates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("plain", "plain\n", 0o755)
	f.write("nonexec", "plain\n", 0o644)
	f.write("owner-only-exec", "plain\n", 0o700)
	f.write("d1701/plain", "plain\n", 0o755)
	if err := os.Chmod(f.path("d1701"), 0o701|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	f.write("binary", "\x7fELF\x00\x01", 0o755)
	f.mkdir("d0701", 0o701)
	f.write("d0701/plain", "plain\n", 0o755)
	f.write("f0001", "plain\n", 0o001)
	f.mkdir("private", 0o700)
	f.write("private/plain", "plain\n", 0o755)
	f.mkdir("dir", 0o755)
	f.symlink("plain", "link-file")
	f.symlink("missing", "link-dangling")
	f.symlink("loop-b", "loop-a")
	f.symlink("loop-a", "loop-b")
	f.symlink("dir", "link-dir")

	cases := []struct {
		name       string
		target     string
		wantReason string
	}{
		{name: "executable", target: "./plain"},
		{name: "non-executable", target: "./nonexec", wantReason: "not executable by all (mode 644)"},
		{name: "owner-only", target: "./owner-only-exec", wantReason: "not executable by all (mode 700)"},
		{name: "special-bits-in-mode", target: "./d1701/plain", wantReason: "directory is not traversable by all (mode 1701)"},
		{name: "binary", target: "./binary"},
		{name: "all-class-directory", target: "d0701/plain", wantReason: "directory is not traversable by all (mode 701)"},
		{name: "owner-only-file", target: "./f0001", wantReason: "not executable by all (mode 1)"},
		{name: "lexical-dotdot-audits-parent", target: "./private/../plain", wantReason: "directory is not traversable by all (mode 700)"},
		{name: "private-parent", target: "private/plain", wantReason: "directory is not traversable by all (mode 700)"},
		{name: "symlink-file", target: "./link-file"},
		{name: "symlink-dangling", target: "./link-dangling", wantReason: "does not exist"},
		{name: "symlink-loop", target: "./loop-a", wantReason: "symlink chain exceeds 40 links (cycle?)"},
		{name: "directory-target", target: "./dir", wantReason: "final target is not a regular file"},
		{name: "missing", target: "./nowhere", wantReason: "does not exist"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.audit(t, test.target, Executable)
			if test.wantReason == "" {
				if err != nil {
					t.Fatalf("audit(%q) = %v, want success", test.target, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("audit(%q) succeeded, want %q", test.target, test.wantReason)
			}
			if got := violationOf(t, err).Reason(); got != test.wantReason {
				t.Fatalf("reason = %q, want %q", got, test.wantReason)
			}
		})
	}
}

func TestSymlinkChainBound(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("plain", "plain\n", 0o755)
	f.symlink("plain", "c0")
	for index := 1; index <= 41; index++ {
		f.symlink("c"+strconv.Itoa(index-1), "c"+strconv.Itoa(index))
	}
	// c39 is 40 followed links; c40 is 41 and must be refused.
	if _, err := f.audit(t, "./c39", Executable); err != nil {
		t.Fatalf("40-link chain rejected: %v", err)
	}
	_, err := f.audit(t, "./c40", Executable)
	if err == nil {
		t.Fatal("41-link chain accepted")
	}
	if got := violationOf(t, err).Reason(); got != "symlink chain exceeds 40 links (cycle?)" {
		t.Fatalf("reason = %q", got)
	}
}

func TestPathLookupFirstExistingCandidate(t *testing.T) {
	f := newFixture(t)
	t.Chdir(f.cwd)
	first := f.mkdir("bin1", 0o755)
	second := f.mkdir("bin2", 0o755)
	f.write("bin1/mytool", "plain\n", 0o644)
	f.write("bin2/mytool", "plain\n", 0o755)
	f.write("bin2/only-second", "plain\n", 0o755)
	f.write("plain", "plain\n", 0o755)

	auditor := func(path string) *Auditor {
		return NewAuditor(Environment{
			WorkingDirectory: func() (PhysicalDirectory, error) { return PhysicalDirectoryOf(f.cwd), nil },
			Path:             PathValue(path),
		})
	}
	if _, err := auditor(first+":"+second).Audit("mytool", Executable); err == nil {
		t.Fatal("first present-but-unusable candidate was skipped")
	} else if got := violationOf(t, err).Reason(); got != "not executable by all (mode 644)" {
		t.Fatalf("reason = %q", got)
	}
	if _, err := auditor(second+":"+first).Audit("mytool", Executable); err != nil {
		t.Fatalf("first executable candidate rejected: %v", err)
	}
	if _, err := auditor(first+":"+second).Audit("only-second", Executable); err != nil {
		t.Fatalf("later candidate rejected: %v", err)
	}
	if _, err := auditor(first+":"+second).Audit("absent", Executable); err == nil {
		t.Fatal("absent command accepted")
	} else if got := violationOf(t, err).Reason(); got != "not found on PATH" {
		t.Fatalf("reason = %q", got)
	}
	// An empty PATH entry is the working directory, as Python's split produced.
	if _, err := auditor(":"+first).Audit("plain", Executable); err != nil {
		t.Fatalf("empty PATH entry rejected: %v", err)
	}
	if _, err := auditor("").Audit("plain", Executable); err != nil {
		t.Fatalf("empty PATH rejected: %v", err)
	}
}

func TestShebangInterpreters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("plain", "plain\n", 0o755)
	f.write("bin/sh", "plain\n", 0o755)
	f.write("bin/env", "plain\n", 0o755)
	f.write("bin/chosen", "plain\n", 0o755)
	f.write("bin/other", "plain\n", 0o755)
	path := PathValue(f.path("bin") + ":/usr/bin:/bin")

	scripts := map[string]string{
		"no-shebang":       "plain\n",
		"empty-shebang":    "#!\n",
		"plain-sh":         "#!/bin/sh\n",
		"crlf":             "#!/bin/sh\r\n",
		"env-sh":           "#!/usr/bin/env sh\n",
		"env-no-arg":       "#!/usr/bin/env\n",
		"env-bare-S":       "#!/usr/bin/env -S\n",
		"env-S-attached":   "#!/usr/bin/env -Ssh\n",
		"env-S-quoted":     "#!/usr/bin/env -S 'sh'\n",
		"env-S-theirs":     "#!/usr/bin/env -S chosen\n",
		"env-S-foreign":    "#!/usr/bin/env -S PATH=" + f.path("bin") + " chosen\n",
		"env-i":            "#!/usr/bin/env -i sh\n",
		"env-u":            "#!/usr/bin/env -u FOO sh\n",
		"env-u-path":       "#!/usr/bin/env -u PATH sh\n",
		"env-unset-path":   "#!/usr/bin/env --unset=PATH sh\n",
		"env-unset-foo":    "#!/usr/bin/env --unset=FOO sh\n",
		"env-C":            "#!/usr/bin/env -C / sh\n",
		"env-chdir":        "#!/usr/bin/env --chdir=/ sh\n",
		"env-unknown":      "#!/usr/bin/env --bogus sh\n",
		"env-leading-eq":   "#!/usr/bin/env =NAME sh\n",
		"env-dashdash":     "#!/usr/bin/env -- sh\n",
		"env-assignment":   "#!/usr/bin/env FOO=bar sh\n",
		"env-two-words":    "#!/usr/bin/env sh -e\n",
		"env-S-two-words":  "#!/usr/bin/env -S sh -e\n",
		"env-S-tabs":       "#!/usr/bin/env -S\tsh\n",
		"env-S-unicode-ws": "#!/usr/bin/env -S\u00a0sh\n",
		"env-S-control-ws": "#!/usr/bin/env -S\x1csh\n",
	}
	for name, content := range scripts {
		f.write("scripts/"+name, content, 0o755)
	}
	scriptNotReadable := f.write("scripts/not-readable", "#!/bin/sh\n", 0o711)
	_ = scriptNotReadable

	cases := []struct {
		name       string
		target     string
		wantReason string
	}{
		{name: "no-shebang", target: "scripts/no-shebang"},
		{name: "empty-shebang", target: "scripts/empty-shebang", wantReason: "shebang names no interpreter"},
		{name: "plain-sh", target: "scripts/plain-sh"},
		{name: "crlf-names-the-carriage-return", target: "scripts/crlf", wantReason: "does not exist"},
		{name: "env-sh", target: "scripts/env-sh"},
		{name: "env-no-arg", target: "scripts/env-no-arg", wantReason: "env shebang names no interpreter"},
		{name: "env-bare-S", target: "scripts/env-bare-S", wantReason: "env -S needs its split string in the same argument"},
		{name: "env-S-attached", target: "scripts/env-S-attached"},
		{name: "env-S-quoted", target: "scripts/env-S-quoted", wantReason: "unsupported env -S quoting"},
		{name: "env-S-theirs", target: "scripts/env-S-theirs"},
		{name: "env-S-path-assignment", target: "scripts/env-S-foreign"},
		// Without -S the whole remainder is ONE argument, so an option run is not
		// parsed and the interpreter is literally named `-u FOO sh`.
		{name: "env-i-without-S", target: "scripts/env-i", wantReason: "unsupported env option '-i sh'"},
		{name: "env-u-without-S", target: "scripts/env-u", wantReason: "unsupported env option '-u FOO sh'"},
		{name: "env-u-path-without-S", target: "scripts/env-u-path", wantReason: "unsupported env option '-u PATH sh'"},
		{name: "env-unset-path-without-S", target: "scripts/env-unset-path", wantReason: "env shebang names no interpreter"},
		{name: "env-unset-foo-without-S", target: "scripts/env-unset-foo", wantReason: "env shebang names no interpreter"},
		{name: "env-C-without-S", target: "scripts/env-C", wantReason: "unsupported env option '-C / sh'"},
		{name: "env-chdir-prefix", target: "scripts/env-chdir", wantReason: "env -C changes the interpreter lookup"},
		{name: "env-unknown", target: "scripts/env-unknown", wantReason: "unsupported env option '--bogus sh'"},
		{name: "env-leading-equals", target: "scripts/env-leading-eq", wantReason: "not found on PATH"},
		{name: "env-dashdash-without-S", target: "scripts/env-dashdash", wantReason: "unsupported env option '-- sh'"},
		{name: "env-assignment-without-S", target: "scripts/env-assignment", wantReason: "env shebang names no interpreter"},
		{name: "env-two-words-is-one-name", target: "scripts/env-two-words", wantReason: "not found on PATH"},
		{name: "env-S-two-words", target: "scripts/env-S-two-words"},
		{name: "env-S-tabs", target: "scripts/env-S-tabs"},
		{name: "env-S-unicode-whitespace", target: "scripts/env-S-unicode-ws"},
		{name: "env-S-control-whitespace", target: "scripts/env-S-control-ws"},
		{name: "script-not-readable", target: "scripts/not-readable", wantReason: "script is not readable by all (mode 711)"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			auditor := NewAuditor(Environment{
				WorkingDirectory: func() (PhysicalDirectory, error) { return PhysicalDirectoryOf(f.cwd), nil },
				Path:             path,
			})
			_, err := auditor.Audit(Target(test.target), Executable)
			if test.wantReason == "" {
				if err != nil {
					t.Fatalf("audit(%q) = %v, want success", test.target, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("audit(%q) succeeded, want %q", test.target, test.wantReason)
			}
			if got := violationOf(t, err).Reason(); got != test.wantReason {
				t.Fatalf("reason = %q, want %q", got, test.wantReason)
			}
		})
	}
}

func TestInterpreterNestingAndCycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("plain", "plain\n", 0o755)
	f.write("bin/target", "#!/bin/sh\n", 0o755)
	f.write("bin/sh", "plain\n", 0o755)
	// Five nested scripts run; the boundary is the sixth interpreter hop.
	bin := f.path("bin")
	f.write("bin/depth1", "#!"+bin+"/depth2\n", 0o755)
	f.write("bin/depth2", "#!"+bin+"/depth3\n", 0o755)
	f.write("bin/depth3", "#!"+bin+"/depth4\n", 0o755)
	f.write("bin/depth4", "#!"+bin+"/depth5\n", 0o755)
	f.write("bin/depth5", "#!"+bin+"/sh\n", 0o755)
	f.write("bin/deep1", "#!"+bin+"/deep2\n", 0o755)
	f.write("bin/deep2", "#!"+bin+"/deep3\n", 0o755)
	f.write("bin/deep3", "#!"+bin+"/deep4\n", 0o755)
	f.write("bin/deep4", "#!"+bin+"/deep5\n", 0o755)
	f.write("bin/deep5", "#!"+bin+"/deep6\n", 0o755)
	f.write("bin/deep6", "#!"+bin+"/sh\n", 0o755)
	f.write("bin/cycle-a", "#!"+bin+"/cycle-b\n", 0o755)
	f.write("bin/cycle-b", "#!"+bin+"/cycle-a\n", 0o755)
	environment := Environment{
		WorkingDirectory: func() (PhysicalDirectory, error) { return PhysicalDirectoryOf(f.cwd), nil },
		Path:             PathValue(f.path("bin")),
	}
	if _, err := NewAuditor(environment).Audit("depth1", Executable); err != nil {
		t.Fatalf("five-script chain rejected: %v", err)
	}
	if _, err := NewAuditor(environment).Audit("deep1", Executable); err == nil {
		t.Fatal("six-script chain accepted")
	} else if got := violationOf(t, err).Reason(); got != "interpreter nesting exceeds 5 scripts" {
		t.Fatalf("reason = %q", got)
	}
	if _, err := NewAuditor(environment).Audit("cycle-a", Executable); err == nil {
		t.Fatal("interpreter cycle accepted")
	} else if got := violationOf(t, err).Reason(); got != "interpreter cycle (already being audited)" {
		t.Fatalf("reason = %q", got)
	}
}

func TestContentAndTree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("data", "data\n", 0o444)
	f.write("data-owner-only", "data\n", 0o640)
	f.write("script", "#!/bin/sh\n", 0o444)
	f.write("executable-data", "data\n", 0o755)
	f.mkdir("tree", 0o755)
	f.write("tree/file", "data\n", 0o644)
	f.write("tree/inside", "data\n", 0o444)
	f.symlink("inside", "tree/link-inside")
	f.symlink("file", "tree/dirlink-inside")
	f.mkdir("tree-0711/child", 0o711)
	f.write("tree-0711/child/file", "data\n", 0o644)
	for _, name := range []string{"tree-link-absolute", "tree-link-relative", "tree-link-parent"} {
		f.mkdir(name, 0o755)
		f.write(name+"/file", "data\n", 0o644)
	}
	f.symlink(f.path("data"), "tree-link-absolute/link-outside-absolute")
	f.symlink("../data", "tree-link-relative/link-outside-relative")
	f.symlink("..", "tree-link-parent/link-parent")
	f.mkdir("tree-0701", 0o701)
	f.write("plain", "plain\n", 0o755)

	if _, err := f.audit(t, "./data", Content); err != nil {
		t.Fatalf("readable data rejected: %v", err)
	}
	if _, err := f.audit(t, "./data-owner-only", Content); err == nil {
		t.Fatal("owner-only data accepted")
	} else if got := violationOf(t, err).Reason(); got != "not readable by all (mode 640)" {
		t.Fatalf("reason = %q", got)
	}
	if _, err := f.audit(t, "./script", Content); err != nil {
		t.Fatalf("content mode resolved a shebang: %v", err)
	}
	if _, err := f.audit(t, "./executable-data", Content); err != nil {
		t.Fatalf("readable executable rejected as content: %v", err)
	}

	treeCases := []struct {
		name       string
		target     string
		wantReason string
	}{
		{name: "tree", target: "./tree"},
		{name: "tree-dot", target: "./tree/."},
		{name: "tree-dotdot", target: "./tree/../tree"},
		{name: "tree-0711-inside", target: "./tree-0711", wantReason: "directory is not readable/traversable by all"},
		{name: "tree-0701-root", target: "./tree-0701", wantReason: "directory is not traversable by all (mode 701)"},
		{name: "tree-file-root", target: "./plain", wantReason: "tree root is not a directory"},
		{name: "tree-missing", target: "./absent", wantReason: "does not exist"},
	}
	for _, test := range treeCases {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.audit(t, test.target, Tree)
			if test.wantReason == "" {
				if err != nil {
					t.Fatalf("tree audit(%q) = %v, want success", test.target, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("tree audit(%q) succeeded, want %q", test.target, test.wantReason)
			}
			if got := violationOf(t, err).Reason(); got != test.wantReason {
				t.Fatalf("reason = %q, want %q", got, test.wantReason)
			}
		})
	}

	// A single escaping symlink fails the whole tree, however it escapes.
	for _, name := range []string{"tree-link-absolute", "tree-link-relative", "tree-link-parent"} {
		if _, err := f.audit(t, "./"+name, Tree); err == nil {
			t.Fatalf("%s: escaping symlink accepted", name)
		}
	}

	// Tree targets are always beneath cwd, never PATH commands.
	if _, err := NewAuditor(Environment{
		WorkingDirectory: func() (PhysicalDirectory, error) { return PhysicalDirectoryOf(f.cwd), nil },
		Path:             PathValue(f.cwd),
	}).Audit("./tree", Tree); err != nil {
		t.Fatalf("tree target used PATH: %v", err)
	}
}

func TestTreeContainmentSymlinks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		target  string
		escapes bool
	}{
		{name: "inside", target: "file"},
		{name: "absolute-reentry", target: "ABSOLUTE"},
		{name: "outside-link", target: "../outside/link", escapes: true},
		{name: "outside-parent-and-back", target: "../root/file", escapes: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.mkdir("root", 0o755)
			f.write("root/file", "data\n", 0o644)
			f.mkdir("outside", 0o755)
			f.symlink(f.path("root/file"), "outside/link")
			target := test.target
			if target == "ABSOLUTE" {
				target = f.path("root/file")
			}
			f.symlink(target, "root/link")
			_, err := f.audit(t, "./root", Tree)
			if test.escapes {
				if err == nil {
					t.Fatal("tree accepted an escaping hop")
				}
				if got := violationOf(t, err).Reason(); got != "symlink waypoint escapes tree root" {
					t.Fatalf("reason = %q", got)
				}
			} else if err != nil {
				t.Fatalf("in-root link rejected: %v", err)
			}
		})
	}
}

// TestTreeRootEstablishment replaces the audit's only enumeration so "/" can be
// certified without walking the host filesystem: the legacy control mocked
// scandir for exactly this reason.
func TestTreeRootEstablishment(t *testing.T) {
	var enumerated []string
	auditor := NewAuditor(Environment{
		ReadDirectory:    func(path string) ([]string, error) { enumerated = append(enumerated, path); return nil, nil },
		WorkingDirectory: func() (PhysicalDirectory, error) { return PhysicalDirectoryOf("/"), nil },
		Path:             PathValue("/usr/bin:/bin"),
	})
	final, err := auditor.Audit("/", Tree)
	if err != nil {
		t.Fatalf("root tree audit = %v", err)
	}
	if string(final) != "/" {
		t.Fatalf("resolved root = %q, want /", final)
	}
	if len(enumerated) != 1 || enumerated[0] != "/" {
		t.Fatalf("enumeration = %v, want exactly [/]", enumerated)
	}
}

func TestTreeSpecialFileRejected(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("FIFO creation is a Linux fixture; the Linux run owns this vector")
	}
	f := newFixture(t)
	f.mkdir("tree", 0o755)
	f.write("tree/file", "data\n", 0o644)
	if err := makeFIFO(filepath.Join(f.cwd, "tree", "fifo"), 0o644); err != nil {
		t.Fatalf("cannot create FIFO: %v", err)
	}
	_, err := f.audit(t, "./tree", Tree)
	if err == nil {
		t.Fatal("FIFO accepted in tree")
	}
	if got := violationOf(t, err).Reason(); got != "special file in tree" {
		t.Fatalf("reason = %q", got)
	}
}

func TestPhysicalCwdIsRequiredOnlyWhenRelative(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.write("plain", "plain\n", 0o755)
	failing := func() (PhysicalDirectory, error) { return PhysicalDirectory{}, os.ErrNotExist }
	environment := Environment{WorkingDirectory: failing, Path: PathValue("/usr/bin:/bin")}
	if _, err := NewAuditor(environment).Audit(Target(f.path("plain")), Executable); err != nil {
		t.Fatalf("absolute target consulted the working directory: %v", err)
	}
	if _, err := NewAuditor(environment).Audit("plain", Executable); err == nil {
		t.Fatal("relative target succeeded without a working directory")
	}
}
