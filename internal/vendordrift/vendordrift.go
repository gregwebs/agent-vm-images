// Package vendordrift is the gate over the copy of the rust-dev example's helper
// module that the root module commits in vendor/ (slice 4a of #187).
//
// After slice 4a two copies of that source exist: the layer module, which the
// image's `golang:` stage compiles, and the copy inside the root `vendor/` tree,
// which `avm-pin rust-toolchain` links. Editing one and not the other changes a
// binary silently -- the image would install one writer and the maintainer would
// run another -- so this gate compares them byte for byte.
//
// Why it compares an *imported package closure* and not "the whole tree": Go's
// vendoring copies only the packages the main module imports, and only their
// non-test files. `main.go`, the kache probe package, `internal/fetch` and every
// `_test.go` are therefore legitimately absent from `vendor/`. A whole-tree
// comparison would be red on its first run, and the two ways to silence it -- a
// hand-written exemption list, or copying the whole tree -- are both a second
// authority that rots. So the closure itself is derived and asserted, and the
// files inside it are compared in both directions.
//
// It runs offline, and it asserts that it did: `go list -deps` is asked under
// `GOFLAGS=-mod=vendor GOPROXY=off`, `go env GOPROXY` is read back inside that
// environment, and every module dependency in the resulting closure must live
// under `vendor/`. That last assertion is what makes "no module cache" real: a
// package resolved from a cache would carry the cache's directory, not the
// committed copy. (The empty-GOMODCACHE form does not work here: the toolchain
// switch that go.mod's `toolchain` line selects is itself a module, and with no
// cache and no proxy it cannot resolve at all.)
package vendordrift

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// HelperModule is the example layer's own module. It is the one committed record
// of which module this gate is about; the packages inside it are derived. It sits
// under the main module's path, which is why the main module's own packages are
// excluded from the vendor-residency assertion below and this one is not.
const (
	HelperModule = "github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper"
	MainModule   = "github.com/gregwebs/agent-vm-images"
)

// MinPackages and MinFiles are the floor below which a green result is not
// believable: the writer reaches at least the toolchain package, the gpg verifier
// and the strict decoders. A derivation that returns nothing must be red.
const (
	MinPackages = 3
	MinFiles    = 10
	// MinClosurePackages is the floor for the whole import graph `go list`
	// reports, so a command that returned nothing cannot look like a small module.
	MinClosurePackages = 10
)

// Report is what one run derived, so a test can hold the derivation itself and
// not only the comparison.
type Report struct {
	Imported []string
	Vendored []string
	Files    int
	// Proxy is what `go env GOPROXY` reported inside the gate's own environment.
	Proxy string
	// VendorOnly is every module dependency in the closure resolved to a file
	// under vendor/, so nothing came from a module cache.
	VendorOnly bool
}

// Options are the two knobs a test needs. Production takes DefaultOptions.
type Options struct {
	Root string
	// Go is the toolchain to ask for the import graph. Offline enforces
	// GOPROXY=off and an empty GOMODCACHE.
	Go func(root string, environment []string, args ...string) ([]byte, error)
}

// Check derives the closure, asserts it, and compares every file in it.
func Check(options Options) (Report, error) {
	var report Report
	root := options.Root
	if root == "" {
		return report, fmt.Errorf("vendordrift: no repository root")
	}
	vendored, layerDir, err := vendoredPackages(root)
	if err != nil {
		return report, err
	}
	report.Vendored = vendored
	if len(vendored) < MinPackages {
		return report, fmt.Errorf("vendordrift: derived %d vendored %s packages, want at least %d: "+
			"the derivation is not looking at the module", len(vendored), HelperModule,
			MinPackages)
	}
	environment := OfflineEnvironment()
	proxy, err := proxyInEffect(root, environment, options.Go)
	if err != nil {
		return report, err
	}
	report.Proxy = proxy
	if report.Proxy != "off" {
		return report, fmt.Errorf("vendordrift: the run is not offline (GOPROXY=%q)", report.Proxy)
	}
	imported, vendorOnly, err := importedPackages(root, environment, options.Go)
	if err != nil {
		return report, err
	}
	report.VendorOnly = vendorOnly
	if !vendorOnly {
		return report, fmt.Errorf("vendordrift: the closure is not entirely under vendor/, so the " +
			"gate is not reading the committed tree")
	}
	report.Imported = imported
	if len(imported) < MinPackages {
		return report, fmt.Errorf("vendordrift: the writer's import graph names %d %s packages, "+
			"want at least %d", len(imported), HelperModule, MinPackages)
	}
	if problem := compareSets(report.Imported, report.Vendored); problem != "" {
		return report, fmt.Errorf("vendordrift: %s", problem)
	}
	for _, pkg := range report.Vendored {
		relative := strings.TrimPrefix(strings.TrimPrefix(pkg, HelperModule), "/")
		differences, files, err := ComparePackageDirectories(
			filepath.Join(layerDir, filepath.FromSlash(relative)),
			filepath.Join(root, "vendor", filepath.FromSlash(pkg)))
		report.Files += files
		if err != nil {
			return report, fmt.Errorf("vendordrift: %s: %w", pkg, err)
		}
		if len(differences) > 0 {
			return report, fmt.Errorf("vendordrift: the vendored copy of %s is stale:\n  %s\n"+
				"run `go mod vendor` in a clean checkout and commit the result",
				pkg, strings.Join(differences, "\n  "))
		}
	}
	if report.Files < MinFiles {
		return report, fmt.Errorf("vendordrift: compared %d files, want at least %d",
			report.Files, MinFiles)
	}
	return report, nil
}

// OfflineEnvironment is the gate's environment: vendor mode and no proxy.
func OfflineEnvironment() []string {
	return []string{"GOFLAGS=-mod=vendor", "GOPROXY=off"}
}

// proxyInEffect asks go what proxy it will use inside the gate's environment,
// rather than trusting the slice we built.
func proxyInEffect(root string, environment []string, run func(string, []string,
	...string) ([]byte, error)) (string, error) {
	if run == nil {
		run = goCommand
	}
	raw, err := run(root, environment, "env", "GOPROXY")
	if err != nil {
		return "", fmt.Errorf("vendordrift: go env: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// ComparePackageDirectories compares every file in two package directories, in
// both directions, and returns one line per difference. `_test.go` files are
// skipped because vendor never carries them, and that is the only exclusion.
func ComparePackageDirectories(layerDir, vendorDir string) ([]string, int, error) {
	layer, err := packageFiles(layerDir)
	if err != nil {
		return nil, 0, err
	}
	vendored, err := packageFiles(vendorDir)
	if err != nil {
		return nil, 0, err
	}
	differences := []string{}
	names := []string{}
	seen := map[string]bool{}
	for name := range layer {
		names = append(names, name)
		seen[name] = true
	}
	for name := range vendored {
		if !seen[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		layerRaw, inLayer := layer[name]
		vendorRaw, inVendor := vendored[name]
		switch {
		case !inVendor:
			differences = append(differences, fmt.Sprintf("%s: present in the layer module, "+
				"absent from vendor", name))
		case !inLayer:
			differences = append(differences, fmt.Sprintf("%s: present in vendor, absent from "+
				"the layer module", name))
		case !bytes.Equal(layerRaw, vendorRaw):
			differences = append(differences, fmt.Sprintf("%s: bytes differ between the layer "+
				"module and vendor", name))
		}
	}
	compared := 0
	for _, name := range names {
		if _, inLayer := layer[name]; inLayer {
			compared++
		}
	}
	return differences, compared, nil
}

// packageFiles is every non-test regular file directly in one package directory.
// It does not recurse: a subdirectory is a different package, with its own entry
// in the derived closure, and testdata is never vendored.
func packageFiles(directory string) (map[string][]byte, error) {
	files := map[string][]byte{}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		files[entry.Name()] = raw
	}
	return files, nil
}

// vendoredPackages reads the committed build record: vendor/modules.txt names the
// packages the build compiles from the helper module, and the local directory the
// module was replaced with.
func vendoredPackages(root string) ([]string, string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "vendor", "modules.txt"))
	if err != nil {
		return nil, "", fmt.Errorf("vendordrift: %w", err)
	}
	packages := []string{}
	local := ""
	module := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "# ") {
			fields := strings.SplitN(strings.TrimPrefix(line, "# "), " ", 2)
			module = fields[0]
			if rest := strings.TrimSpace(strings.Join(fields[1:], " ")); rest != "" {
				if _, replacement, found := strings.Cut(rest, "=>"); found {
					local = strings.TrimSpace(replacement)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "## ") || line == "" {
			continue
		}
		if module == HelperModule {
			packages = append(packages, line)
		}
	}
	if local == "" {
		return nil, "", fmt.Errorf("vendordrift: vendor/modules.txt has no local replacement for %s",
			HelperModule)
	}
	layerDir := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(local, "./")))
	sort.Strings(packages)
	return packages, layerDir, nil
}

// importedPackages is the writer's own import graph: this module's packages in
// it, and whether every other module dependency resolved under vendor/.
func importedPackages(root string, environment []string, run func(string, []string,
	...string) ([]byte, error)) ([]string, bool, error) {
	if run == nil {
		run = goCommand
	}
	raw, err := run(root, environment, "list", "-deps", "-f",
		"{{.ImportPath}}\t{{.Dir}}", "./cmd/avm-pin")
	if err != nil {
		return nil, false, fmt.Errorf("vendordrift: go list: %w", err)
	}
	vendorRoot := filepath.Join(root, "vendor") + string(filepath.Separator)
	imported := []string{}
	vendorOnly := true
	seen := 0
	for _, line := range strings.Split(string(raw), "\n") {
		path, directory, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || path == "" {
			continue
		}
		seen++
		if path == HelperModule || strings.HasPrefix(path, HelperModule+"/") {
			imported = append(imported, path)
		}
		if !isModulePath(path) || strings.HasPrefix(path, MainModule+"/") &&
			!strings.HasPrefix(path, HelperModule) {
			continue
		}
		if !strings.HasPrefix(directory, vendorRoot) {
			vendorOnly = false
		}
	}
	if seen < MinClosurePackages {
		return nil, false, fmt.Errorf("vendordrift: go list named %d packages, want at least %d",
			seen, MinClosurePackages)
	}
	sort.Strings(imported)
	return imported, vendorOnly, nil
}

// isModulePath reports whether a package path is a third-party module's: a first
// element containing a dot is a domain, which the standard library never has.
func isModulePath(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return strings.Contains(first, ".")
}

func goCommand(root string, environment []string, args ...string) ([]byte, error) {
	command := exec.Command("go", args...)
	command.Dir = root
	command.Env = append(os.Environ(), environment...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	raw, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return raw, nil
}

// compareSets holds the derivation to equality: a vendored package the writer no
// longer imports is a stale vendor tree too, and it is the one case a file
// comparison cannot see.
func compareSets(imported, vendored []string) string {
	if sameStrings(imported, vendored) {
		return ""
	}
	return fmt.Sprintf("the writer imports %v but vendor carries %v", imported, vendored)
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
