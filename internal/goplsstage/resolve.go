package goplsstage

// The resolution phase: the only place in this repository that may change a Go
// module graph. `go get -tool`, `mod download`, `mod edit`, `mod tidy`, `mod verify`
// and `mod tidy -diff` are the writer's command family from package gotool, and the
// image's program cannot name them (owner ruling G8a: the layer holds no resolver).
//
// Who speaks HTTP: the pinned `go` child, through the reviewed proxy, for the module
// objects a new release selects. This package holds no client and composes no URL; the
// sum database is the reviewed verifier key unless a fixture build supplies "off",
// and the record the run emits always names the reviewed production origins.

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
)

// resolver is one run's child plumbing: exactly one environment, one module
// directory and the runner the caller injected.
type resolver struct {
	environment gotool.Environment
	transport   map[string]string
	runner      gotool.Runner
	paths       gotool.Paths
	goBinary    string
	module      string
	env         []string
}

// prepare builds the resolution environment and holds it to what was asked for.
//
// The compiler version is read from the child, because that is the value a different
// `go` on PATH would change. The three configuration keys are read from the
// environment this program constructed, which is meaningful because `GOENV=off` in
// that same environment means no `go env -w` file can supply a different one: a
// check against the child's answer would read the same bytes.
func (r *resolver) prepare(compiler closure.GoVersion) error {
	environment, err := r.environment.ResolutionEnv(r.goBinary, r.paths,
		compiler.Toolchain(), r.transport)
	if err != nil {
		return refusal("%v", err)
	}
	r.env = environment
	version, err := r.mustGotool(gotool.EnvGoVersion, nil, "go env GOVERSION")
	if err != nil {
		return err
	}
	if version != compiler.Toolchain() {
		return refusal("maintenance compiler is %s, want %s", version, compiler.Toolchain())
	}
	for _, expected := range []struct{ key, want string }{
		{"GOSUMDB", r.environment.SumDB()},
		{"GOPROXY", r.environment.Proxy()},
		{"GOFLAGS", "-mod=mod"},
	} {
		actual, present := r.envValue(expected.key)
		if !present {
			return refusal("maintenance environment: %s is not set, want %q", expected.key,
				expected.want)
		}
		if actual != expected.want {
			return refusal("maintenance environment: %s is %q, want %q", expected.key, actual,
				expected.want)
		}
	}
	return nil
}

// envValue reads one key of the constructed environment.
func (r *resolver) envValue(key string) (string, bool) {
	for _, entry := range r.env {
		if value, found := strings.CutPrefix(entry, key+"="); found {
			return value, true
		}
	}
	return "", false
}

// mustGotool runs one member of the writer's command family in the module directory
// and returns its trimmed stdout. The command's own name, its status and whatever the
// child said are the refusal: the Python stager printed exactly that shape.
func (r *resolver) mustGotool(command gotool.Command, extra []string, what string) (string,
	error) {
	child, err := gotool.NewChild(r.env, command, extra, r.module)
	if err != nil {
		return "", refusal("%v", err)
	}
	result, err := r.runner.Run(child)
	if err != nil {
		return "", refusal("%s: %v", what, err)
	}
	if result.Status != 0 {
		detail := strings.TrimSpace(result.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(result.Stdout)
		}
		return "", refusal("%s failed (status %d): %s", what, result.Status, detail)
	}
	return strings.TrimSpace(result.Stdout), nil
}

// origin is the proxy's `.info` origin block: unauthenticated metadata that agrees
// with the selection, recorded as such.
type origin struct {
	VCS    string `json:"VCS"`
	URL    string `json:"URL"`
	Subdir string `json:"Subdir"`
	Ref    string `json:"Ref"`
	Hash   string `json:"Hash"`
}

// download is one `go mod download -json` answer for the root module.
type download struct {
	Path     string
	Version  string
	Info     string
	GoMod    string
	Zip      string
	Dir      string
	Sum      string
	GoModSum string
	Origin   origin
}

// parseDownload reads the download report and the origin metadata beside it.
//
// Absent paths are a refusal, not an empty record: the origin block is what makes the
// provenance record say where the bytes came from, and a report without an `Info`
// file is a report this writer will not turn into a pin.
func parseDownload(report string) (download, error) {
	var evidence download
	if err := json.Unmarshal([]byte(report), &evidence); err != nil {
		return download{}, refusal("go mod download: unexpected report: %v", err)
	}
	if evidence.GoMod == "" || evidence.Info == "" {
		return download{}, refusal("go mod download: the report names no downloaded evidence " +
			"for the root module")
	}
	raw, err := os.ReadFile(evidence.Info)
	if err != nil {
		return download{}, refusal("go mod download: cannot read the origin metadata: %v", err)
	}
	var info struct {
		Origin origin `json:"Origin"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return download{}, refusal("go mod download: unexpected origin metadata: %v", err)
	}
	evidence.Origin = info.Origin
	return evidence, nil
}
