// Package gotool is the one owner of the Go child this layer may start: the
// closed command set, the program path, the constructed environment and the
// private scratch tree.
//
// #188 slice 2 introduced it with `verify-installed`; #251's installer must call
// the same owner instead of writing a second argv or a second environment down,
// because a rule added to one copy would be missing from the other. The rule table
// of the verb that uses it is in package installed.
//
// What the child is not: `go version -m -json` reports the metadata a binary
// carries about itself, and `go env` reports the tool's own configuration. Neither
// is a signature check. The pinned compiler is a metadata reader here, and a
// candidate that lies in its own build info is caught only where the lie
// contradicts the committed pin (see package buildinfo).
//
// Nothing is inherited: the caller hands this package a Target and Paths and gets
// back exactly the environment a child may see. `GOENV=off`, `GOWORK=off`,
// `GOTOOLCHAIN=local`, `GOPROXY=off` and `GOFLAGS=` are written here once, so no
// ambient GO* variable, `go env -w` file or workspace can change what these
// commands answer. The only additions are the reviewed transport keys, which carry
// bytes but not content decisions.
package gotool

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
)

// Architecture is one of the two Linux targets the closure is cross-built for. It
// is the `--arch` value, so it carries the go-arg text decoder: a bad value is a
// parse error, not a value passed through to `GOARCH`.
type Architecture int

const (
	// AMD64 is linux/amd64.
	AMD64 Architecture = iota
	// ARM64 is linux/arm64.
	ARM64
)

// Architectures is the supported set in the reader's order.
func Architectures() []Architecture { return []Architecture{AMD64, ARM64} }

// Value is the architecture's own name, as GOARCH spells it.
func (a Architecture) Value() string {
	if a == ARM64 {
		return "arm64"
	}
	return "amd64"
}

func (a Architecture) String() string { return a.Value() }

// UnmarshalText decodes `--arch amd64|arm64`.
func (a *Architecture) UnmarshalText(text []byte) error {
	switch string(text) {
	case "amd64":
		*a = AMD64
	case "arm64":
		*a = ARM64
	default:
		return fmt.Errorf("unsupported architecture %q: expected amd64 or arm64", string(text))
	}
	return nil
}

// Target is what the layer installed and which Linux target it was built for.
//
// Binary is the exact path the caller asked to verify. There is deliberately no
// fallback to BinDir/gopls: a verifier that substituted a sibling would say nothing
// about the path it was given.
type Target struct {
	GoRoot   string
	BinDir   string
	ShareDir string
	Arch     Architecture
	Binary   string
}

// Paths is the private tree one invocation owns. Every Go child runs with HOME,
// GOPATH, GOMODCACHE, GOCACHE and TMPDIR inside it, so neither the build cache nor
// a module cache of the guest can take part in a verification.
type Paths struct {
	Scratch string
	// BuildCache is a shared build cache when a suite reuses one; empty means the
	// cache belongs to this scratch tree.
	BuildCache string
}

// Home is the child's HOME.
func (p Paths) Home() string { return filepath.Join(p.Scratch, "home") }

// Gopath is the child's GOPATH.
func (p Paths) Gopath() string { return filepath.Join(p.Scratch, "gopath") }

// ModuleCache is the child's GOMODCACHE.
func (p Paths) ModuleCache() string { return filepath.Join(p.Gopath(), "pkg", "mod") }

// GoCache is the child's GOCACHE.
func (p Paths) GoCache() string {
	if p.BuildCache != "" {
		return p.BuildCache
	}
	return filepath.Join(p.Scratch, "gocache")
}

// Tmp is the child's TMPDIR.
func (p Paths) Tmp() string { return filepath.Join(p.Scratch, "tmp") }

// Work is the empty module directory the child runs in, so no go.mod above the
// scratch tree can take part in what the child reports.
func (p Paths) Work() string { return filepath.Join(p.Scratch, "module") }

// Prepare creates the four directories the environment names.
func (p Paths) Prepare() error {
	for _, directory := range []string{p.Home(), p.Gopath(), p.GoCache(), p.Tmp()} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			return fmt.Errorf("cannot create %s: %v", directory, err)
		}
	}
	return nil
}

// Command is one member of the closed set of Go invocations this program may make.
//
// The set is closed on purpose: there is no `install`, `get`, `mod tidy`, `mod
// download` or `mod vendor` member, so no caller can express a version resolution,
// a graph regeneration or a sum database lookup. #251 adds `build` to this set; it
// must not grow a resolution verb.
type Command struct {
	name string
	argv []string
}

// Name is the command's identity in a diagnostic or a fixture record.
func (c Command) Name() string { return c.name }

// Argv is the command's own argument vector, without the program.
func (c Command) Argv() []string { return append([]string{}, c.argv...) }

var (
	// EnvGoVersion reads the compiler's own version: `go env GOVERSION`.
	EnvGoVersion = Command{name: "EnvGoVersion", argv: []string{"env", "GOVERSION"}}
	// EnvGoRoot reads where the compiler thinks it lives: `go env GOROOT`.
	EnvGoRoot = Command{name: "EnvGoRoot", argv: []string{"env", "GOROOT"}}
	// BuildInfo reads a binary's build information: `go version -m -json`. The
	// binary is only ever an argument to this command; it is never executed.
	BuildInfo = Command{name: "BuildInfo", argv: []string{"version", "-m", "-json"}}
)

// Commands is the closed set, for a caller that holds it to a reviewed count or a
// reviewed absence of resolution verbs.
func Commands() []Command { return []Command{EnvGoVersion, EnvGoRoot, BuildInfo} }

// Environment is the whole Go environment a child may run in.
type Environment struct {
	proxy string
	sumdb string
}

// Offline is the installed-state environment: the proxy is off and there is no
// fallback origin, so a child that tried to fetch would fail instead of silently
// reaching the network. The sum database key is the closure's reviewed one rather
// than a second copy: with the proxy off it is part of a closed environment, not a
// fetch.
func Offline() Environment { return Environment{proxy: "off", sumdb: closure.SumDB} }

// childEnvironmentKeys is the exact key set of a child environment, in order. It is
// an interface: a test asserts the produced list, so a key that is not here cannot
// be inherited, and a key added here is a reviewed change.
var childEnvironmentKeys = []string{
	"PATH", "HOME", "GOPATH", "GOMODCACHE", "GOCACHE", "TMPDIR", "GOENV", "GOWORK",
	"GOFLAGS", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GONOSUMDB", "GONOPROXY",
	"GOPRIVATE", "GOINSECURE", "GOAUTH", "GOVCS", "CGO_ENABLED", "GOOS", "GOARCH",
}

// transportKeys may be added to a child environment. Transport is not content: a
// proxy URL or a build CA cannot change which module bytes a committed sum matches,
// so carrying them is safe where inheriting GOFLAGS would not be.
var transportKeys = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// imagePath is the distro search path the image provides beside the pinned Go root.
const imagePath = "/usr/bin:/bin"

// ChildEnv builds the one environment a Go child may see, as ordered KEY=value
// pairs. A transport key that is not in the reviewed list is refused rather than
// dropped: a caller asking to add GOOS is a caller whose assumption is wrong.
func (e Environment) ChildEnv(target Target, paths Paths, transport map[string]string) ([]string, error) {
	values := map[string]string{
		"PATH":        filepath.Join(target.GoRoot, "bin") + ":" + imagePath,
		"HOME":        paths.Home(),
		"GOPATH":      paths.Gopath(),
		"GOMODCACHE":  paths.ModuleCache(),
		"GOCACHE":     paths.GoCache(),
		"TMPDIR":      paths.Tmp(),
		"GOENV":       "off",
		"GOWORK":      "off",
		"GOFLAGS":     "",
		"GOTOOLCHAIN": "local",
		"GOPROXY":     e.proxy,
		"GOSUMDB":     e.sumdb,
		"GONOSUMDB":   "",
		"GONOPROXY":   "",
		"GOPRIVATE":   "",
		"GOINSECURE":  "",
		"GOAUTH":      "off",
		"GOVCS":       "*:off",
		"CGO_ENABLED": "0",
		"GOOS":        "linux",
		"GOARCH":      target.Arch.Value(),
	}
	extra := make([]string, 0, len(transport))
	for key := range transport {
		if !isTransportKey(key) {
			return nil, fmt.Errorf("%s is not a reviewed transport key", key)
		}
	}
	for _, key := range transportKeys {
		if value, present := transport[key]; present {
			extra = append(extra, key+"="+value)
		}
	}
	environment := make([]string, 0, len(childEnvironmentKeys)+len(extra))
	for _, key := range childEnvironmentKeys {
		environment = append(environment, key+"="+values[key])
	}
	return append(environment, extra...), nil
}

func isTransportKey(key string) bool {
	for _, allowed := range transportKeys {
		if key == allowed {
			return true
		}
	}
	return false
}

// Child is one closed invocation: the program, its argument vector, exactly the
// environment, and an existing directory.
type Child struct {
	Program string
	Argv    []string
	Env     []string
	Dir     string
}

// NewChild resolves the program the child PATH names first and appends the
// command's argv and its extra arguments.
//
// The program is the first PATH entry with `go` appended, not a search: the reader
// this ported from resolved it that way, and the entry is the pinned root's bin. A
// PATH that does not name one program would otherwise let a second `go` on the
// image decide what a pinned verification means.
func NewChild(environment []string, command Command, extra []string, dir string) (Child, error) {
	first, err := pathFirstEntry(environment)
	if err != nil {
		return Child{}, err
	}
	argv := append(command.Argv(), extra...)
	return Child{Program: filepath.Join(first, "go"), Argv: argv, Env: environment,
		Dir: dir}, nil
}

// pathFirstEntry is the first PATH entry of an environment list.
func pathFirstEntry(environment []string) (string, error) {
	for _, entry := range environment {
		if value, found := strings.CutPrefix(entry, "PATH="); found {
			if value == "" {
				break
			}
			return strings.Split(value, string(os.PathListSeparator))[0], nil
		}
	}
	return "", fmt.Errorf("the child environment names no PATH to resolve `go` from")
}

// Result is one finished child process.
type Result struct {
	Stdout string
	Stderr string
	Status int
}

// Runner runs one child with exactly its program, argv, environment and directory.
// It exists so a test can record what would have run: an assertion about the
// status of a child that was never observed is not evidence of what it ran.
type Runner interface {
	Run(child Child) (Result, error)
}

// ExecRunner is the real runner. It starts the program as given and does not
// resolve it again.
type ExecRunner struct{}

// Run executes the child.
func (ExecRunner) Run(child Child) (Result, error) {
	result, err := runProcess(child)
	if err != nil {
		return result, err
	}
	return result, nil
}

// RemoveTree removes an invocation's scratch tree, reporting anything it cannot
// remove.
//
// A retained tree is a resource leak, not a verification failure, so the caller
// reports it with the exact paths instead of turning it into a second error that
// would hide the failure being handled. Directories are opened up first because a
// Go child writes read-only directories into its module cache.
func RemoveTree(scratch string) error {
	if _, err := os.Lstat(scratch); err != nil {
		return nil
	}
	failures := []string{}
	directories := []string{}
	err := filepath.Walk(scratch, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			failures = append(failures, path+": "+err.Error())
			return nil
		}
		if info.IsDir() {
			directories = append(directories, path)
		}
		return nil
	})
	if err != nil {
		failures = append(failures, err.Error())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(directories)))
	for _, directory := range directories {
		if err := os.Chmod(directory, 0o700); err != nil {
			failures = append(failures, directory+": "+err.Error())
		}
	}
	if err := os.RemoveAll(scratch); err != nil {
		failures = append(failures, scratch+": "+err.Error())
	}
	if _, err := os.Lstat(scratch); err == nil {
		failures = append(failures, scratch+": still present")
	}
	if len(failures) > 0 {
		return fmt.Errorf("could not remove the invocation scratch tree %s: %s", scratch,
			strings.Join(failures, "; "))
	}
	return nil
}
