package gotool

// The environment, argv and scratch-tree contract. Each test is the witness for one
// rule of this owner: deleting the rule makes the named test fail.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testTarget(t *testing.T) Target {
	t.Helper()
	return Target{GoRoot: "/opt/go", BinDir: "/opt/go-tools/bin",
		ShareDir: "/opt/go-tools/share/gopls", Arch: ARM64,
		Binary: "/opt/go-tools/bin/gopls"}
}

func testPaths(t *testing.T) Paths {
	t.Helper()
	return Paths{Scratch: filepath.Join(t.TempDir(), "scratch")}
}

// TestChildEnvironmentIsTheWholeEnvironment holds the exact key order and every
// value that closes the child off from the host.
func TestChildEnvironmentIsTheWholeEnvironment(t *testing.T) {
	target, paths := testTarget(t), testPaths(t)
	environment, err := Offline().ChildEnv(target, paths, nil)
	if err != nil {
		t.Fatalf("ChildEnv: %v", err)
	}
	values := map[string]string{}
	keys := []string{}
	for _, entry := range environment {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			t.Fatalf("entry %q is not KEY=value", entry)
		}
		keys = append(keys, key)
		values[key] = value
	}
	if got, want := strings.Join(keys, ","), strings.Join(childEnvironmentKeys, ","); got != want {
		t.Fatalf("key order %s, want %s", got, want)
	}
	for key, want := range map[string]string{
		"PATH":        "/opt/go/bin:/usr/bin:/bin",
		"HOME":        filepath.Join(paths.Scratch, "home"),
		"GOPATH":      filepath.Join(paths.Scratch, "gopath"),
		"GOMODCACHE":  filepath.Join(paths.Scratch, "gopath", "pkg", "mod"),
		"GOCACHE":     filepath.Join(paths.Scratch, "gocache"),
		"TMPDIR":      filepath.Join(paths.Scratch, "tmp"),
		"GOENV":       "off",
		"GOWORK":      "off",
		"GOFLAGS":     "",
		"GOTOOLCHAIN": "local",
		"GOPROXY":     "off",
		"GONOSUMDB":   "",
		"GONOPROXY":   "",
		"GOPRIVATE":   "",
		"GOINSECURE":  "",
		"GOAUTH":      "off",
		"GOVCS":       "*:off",
		"CGO_ENABLED": "0",
		"GOOS":        "linux",
		"GOARCH":      "arm64",
	} {
		if values[key] != want {
			t.Errorf("%s = %q, want %q", key, values[key], want)
		}
	}
	// The sum database key is the closure's reviewed one, so a child cannot be
	// pointed at another database and still pass for the pinned environment.
	if values["GOSUMDB"] != Offline().sumdb || len(values["GOSUMDB"]) != len(
		"sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8") {
		t.Fatalf("GOSUMDB = %q, not the reviewed verifier key", values["GOSUMDB"])
	}
}

// TestTransportIsOnlyWhatIsReviewed: the transport keys may be added, and cannot
// override the target or reach a GO* policy. A caller asking for anything else is
// refused rather than silently dropped.
func TestTransportIsOnlyWhatIsReviewed(t *testing.T) {
	environment, err := Offline().ChildEnv(testTarget(t), testPaths(t),
		map[string]string{"HTTPS_PROXY": "http://h:1", "SSL_CERT_DIR": "/ca"})
	if err != nil {
		t.Fatalf("ChildEnv: %v", err)
	}
	if len(environment) != len(childEnvironmentKeys)+2 {
		t.Fatalf("got %d entries, want the closed set plus two transport keys",
			len(environment))
	}
	for _, want := range []string{"HTTPS_PROXY=http://h:1", "SSL_CERT_DIR=/ca"} {
		found := false
		for _, entry := range environment {
			if entry == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q is missing from %v", want, environment)
		}
	}
	if _, err := Offline().ChildEnv(testTarget(t), testPaths(t),
		map[string]string{"GOOS": "darwin"}); err == nil ||
		!strings.Contains(err.Error(), "GOOS is not a reviewed transport key") {
		t.Fatalf("a GO* key was accepted as transport: %v", err)
	}
}

// TestClosedCommandSet: the set has exactly the three reads this verb makes, and no
// member can resolve a version, regenerate the graph or fetch a sum.
func TestClosedCommandSet(t *testing.T) {
	commands := Commands()
	if len(commands) != 3 {
		t.Fatalf("%d commands, want 3", len(commands))
	}
	names := []string{}
	for _, command := range commands {
		names = append(names, command.Name())
		if joined := strings.Join(command.Argv(), " "); strings.Contains(joined,
			"install") || strings.Contains(joined, "get") ||
			strings.Contains(joined, "tidy") || strings.Contains(joined, "download") ||
			strings.Contains(joined, "vendor") || strings.Contains(joined, "edit") {
			t.Fatalf("%s can regenerate the graph: %v", command.Name(), command.Argv())
		}
	}
	for _, want := range []string{"EnvGoVersion", "EnvGoRoot", "BuildInfo"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("the set is missing %s: %v", want, names)
		}
	}
	if got := strings.Join(EnvGoVersion.Argv(), " "); got != "env GOVERSION" {
		t.Fatalf("EnvGoVersion argv %q", got)
	}
	if got := strings.Join(EnvGoRoot.Argv(), " "); got != "env GOROOT" {
		t.Fatalf("EnvGoRoot argv %q", got)
	}
	if got := strings.Join(BuildInfo.Argv(), " "); got != "version -m -json" {
		t.Fatalf("BuildInfo argv %q", got)
	}
	// Argv is a copy: a caller that appends to it cannot change the closed set.
	argv := BuildInfo.Argv()
	argv[0] = "build"
	if BuildInfo.Argv()[0] != "version" {
		t.Fatal("Argv returned the command's own slice")
	}
}

// TestProgramIsTheFirstPathEntry: the child runs the `go` the constructed PATH
// names first, not one found by searching, so no second toolchain can answer.
func TestProgramIsTheFirstPathEntry(t *testing.T) {
	environment, err := Offline().ChildEnv(testTarget(t), testPaths(t), nil)
	if err != nil {
		t.Fatalf("ChildEnv: %v", err)
	}
	child, err := NewChild(environment, BuildInfo, []string{"/opt/go-tools/bin/gopls"}, "/work")
	if err != nil {
		t.Fatalf("NewChild: %v", err)
	}
	if child.Program != "/opt/go/bin/go" {
		t.Fatalf("program %q, want /opt/go/bin/go", child.Program)
	}
	if strings.Join(child.Argv, " ") != "version -m -json /opt/go-tools/bin/gopls" {
		t.Fatalf("argv %v", child.Argv)
	}
	if child.Dir != "/work" || len(child.Env) != len(childEnvironmentKeys) {
		t.Fatalf("child %+v", child)
	}
	// No PATH entry means no program: a caller cannot fall back to the parent's.
	if _, err := NewChild([]string{"HOME=/root"}, BuildInfo, nil, "/work"); err == nil ||
		!strings.Contains(err.Error(), "names no PATH") {
		t.Fatalf("a PATH-less environment was accepted: %v", err)
	}
}

// TestArchitectureText: only the two reviewed targets parse, and anything else is
// a parse error (the CLI's status 2), not a value passed through to GOARCH.
func TestArchitectureText(t *testing.T) {
	for _, pair := range []struct {
		text  string
		value Architecture
	}{{"amd64", AMD64}, {"arm64", ARM64}} {
		var arch Architecture
		if err := arch.UnmarshalText([]byte(pair.text)); err != nil {
			t.Fatalf("%s: %v", pair.text, err)
		}
		if arch != pair.value || arch.Value() != pair.text {
			t.Fatalf("%s round-tripped to %s", pair.text, arch.Value())
		}
	}
	for _, text := range []string{"", "386", "linux/amd64", "AMD64"} {
		var arch Architecture
		if err := arch.UnmarshalText([]byte(text)); err == nil {
			t.Fatalf("%q was accepted", text)
		}
	}
	var unsupported Architecture
	if err := unsupported.UnmarshalText([]byte("386")); err == nil ||
		!strings.Contains(err.Error(), "expected amd64 or arm64") {
		t.Fatalf("the diagnostic does not name the admissible values: %v", err)
	}
}

// TestPathsPrepareCreateEveryDirectory: a child's HOME, GOPATH, GOCACHE and TMPDIR
// exist before it starts, and a shared build cache replaces the private one.
func TestPathsPrepareCreateEveryDirectory(t *testing.T) {
	paths := testPaths(t)
	if err := paths.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, directory := range []string{paths.Home(), paths.Gopath(), paths.GoCache(),
		paths.Tmp()} {
		if info, err := os.Stat(directory); err != nil || !info.IsDir() {
			t.Fatalf("%s is not a directory: %v", directory, err)
		}
	}
	shared := testPaths(t)
	shared.BuildCache = filepath.Join(t.TempDir(), "shared")
	if shared.GoCache() != shared.BuildCache {
		t.Fatalf("GoCache %q, want the shared cache", shared.GoCache())
	}
	if err := shared.Prepare(); err != nil {
		t.Fatalf("Prepare with a shared cache: %v", err)
	}
}

// TestRemoveTreeLeavesNothing: the invocation's tree is gone after a success and
// after a failure, and a tree it cannot remove is reported with its path rather
// than swallowed.
func TestRemoveTreeLeavesNothing(t *testing.T) {
	paths := testPaths(t)
	if err := paths.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	readOnly := filepath.Join(paths.ModuleCache(), "example.test", "dep@v1.0.0")
	if err := os.MkdirAll(readOnly, 0o755); err != nil {
		t.Fatalf("create the module cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(readOnly, "LICENSE"), []byte("bytes"), 0o444); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A Go child leaves the module cache read-only; a plain recursive remove would
	// stop at the first such directory and report success over a retained tree.
	if err := os.Chmod(readOnly, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := RemoveTree(paths.Scratch); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Lstat(paths.Scratch); err == nil {
		t.Fatalf("%s survived RemoveTree", paths.Scratch)
	}
	// A path that never existed is not a failure: the caller may report removal
	// after a failure that happened before the tree was created.
	if err := RemoveTree(paths.Scratch); err != nil {
		t.Fatalf("RemoveTree of an absent tree: %v", err)
	}
}

// TestRemoveTreeReportsWhatItCannotRemove drives the failure branch: a tree inside a
// directory the process may read but not write cannot be unlinked, so the diagnostic
// must name it instead of reporting success. Root ignores the permission bits, so
// there the mechanism does not exist and the same call must succeed; the assertion
// follows the process's own authority instead of being skipped.
func TestRemoveTreeReportsWhatItCannotRemove(t *testing.T) {
	parent := t.TempDir()
	scratch := filepath.Join(parent, "scratch")
	if err := os.MkdirAll(filepath.Join(scratch, "home"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(parent, 0o755); err != nil {
			t.Errorf("restore %s: %v", parent, err)
		}
	})
	err := RemoveTree(scratch)
	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatalf("root could not remove the tree: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("RemoveTree reported success over a tree it could not remove")
	}
	if !strings.Contains(err.Error(), scratch) || !strings.Contains(err.Error(),
		"could not remove the invocation scratch tree") {
		t.Fatalf("the diagnostic does not name the retained tree: %v", err)
	}
}
