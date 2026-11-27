// Package vendordrift is the gate over the copies of the example layers' helper
// modules that the root module commits in vendor/.
//
// Each linked example layer has two copies of its source: the layer module, which the
// image's `golang:` stage compiles, and the copy inside the root `vendor/` tree, which
// `avm-pin` links. Editing one and not the other changes a binary silently -- the image
// would install one writer and the maintainer would run another -- so this gate compares
// them byte for byte. The rust-dev link arrived in slice 4a of #187; the go-dev link
// arrived with `avm-pin gopls` (#252), and the same facts are checked for both.
//
// Why it compares an *imported package closure* and not "the whole tree": Go's
// vendoring copies only the packages the main module imports, and only their
// non-test files. `main.go`, the kache probe package, `internal/fetch`, the fixture
// seams and every `_test.go` are therefore legitimately absent from `vendor/`. A
// whole-tree comparison would be red on its first run, and the two ways to silence it
// -- a hand-written exemption list, or copying the whole tree -- are both a second
// authority that rots. So each closure is derived and asserted, and the files inside
// it are compared in both directions.
//
// It runs offline, and it asserts that it did: `go list -deps` is asked under
// `GOFLAGS=-mod=vendor GOPROXY=off`, `go env GOPROXY` is read back inside that
// environment, and every module dependency in the resulting closure must live under
// `vendor/`. That last assertion is what makes "no module cache" real: a package
// resolved from a cache would carry the cache's directory, not the committed copy.
// (The empty-GOMODCACHE form does not work here: the toolchain switch that go.mod's
// `toolchain` line selects is itself a module, and with no cache and no proxy it
// cannot resolve at all.)
//
// The gate also measures ADR 0017's link 5 offline: for every requirement a helper
// module's own `go.mod` names, the root `go.mod` must require the same version, so
// minimal version selection in the root cannot raise a version and compile the writer
// against a library the layer's build never selects. `script/test/example-layers.sh`
// states the same rule in shell, which ADR 0017 names as link 5's home and whose
// self-test rows mutate the `go.mod` files of a synthetic tree that has no root module
// for this gate to read. The two are measurements of one fact from two harnesses, not
// two owners: this one runs in the guard-only mode, where the shell gate cannot.
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

// MainModule is the module that builds `avm-pin`.
const MainModule = "github.com/gregwebs/agent-vm-images"

// Module is one example layer's helper module: the path the writer imports and the
// directory the layer compiles, relative to the repository root in slash form.
type Module struct {
	Path      string
	Directory string
}

// Modules is the one committed record of which modules this gate is about; the
// packages inside each are derived. Each sits under the main module's path, which is
// why the main module's own packages are excluded from the vendor-residency assertion
// below and these are not.
var Modules = []Module{
	{Path: MainModule + "/examples/layers/go-dev/helper",
		Directory: "examples/layers/go-dev/helper"},
	{Path: MainModule + "/examples/layers/rust-dev/helper",
		Directory: "examples/layers/rust-dev/helper"},
}

// HelperModule is the rust-dev module, named by the shell gate the rust-dev slice
// introduced. It is the second entry of Modules; nothing below keys off it.
const HelperModule = MainModule + "/examples/layers/rust-dev/helper"

// Floors below which a green result is not believable. Each module's derivation must
// reach the packages its writer actually imports -- the go-dev closure and its
// installer stack, the rust-dev toolchain and its decoders -- so a walk that returns
// nothing, or almost nothing, is red rather than a clean comparison over an empty set.
const (
	MinFiles = 20
	// MinClosurePackages is the floor for the whole import graph `go list` reports, so
	// a command that returned nothing cannot look like a small module.
	MinClosurePackages = 20
	// MinSharedRequirements is the floor for the link-5 comparison: the root module
	// shares at least two requirements with each helper. A comparison over nothing is
	// not a pass.
	MinSharedRequirements = 3
)

// MinPackages is the per-module package floor.
var MinPackages = map[string]int{
	MainModule + "/examples/layers/go-dev/helper":   5,
	MainModule + "/examples/layers/rust-dev/helper": 3,
}

// ModuleReport is what one run derived for one module.
type ModuleReport struct {
	Module      Module
	Imported    []string
	Vendored    []string
	Replacement string
	Files       int
}

// Report is what one run derived, so a test can hold the derivation itself and not
// only the comparison.
type Report struct {
	Modules []ModuleReport
	// Files is every file compared across all modules.
	Files int
	// Proxy is what `go env GOPROXY` reported inside the gate's own environment.
	Proxy string
	// VendorOnly is every module dependency in the closure resolved to a file under
	// vendor/, so nothing came from a module cache.
	VendorOnly bool
	// Shared is every requirement the root and a helper module share, compared at one
	// version.
	Shared int
}

// Options are the two knobs a test needs. Production takes DefaultOptions.
type Options struct {
	Root string
	// Go is the toolchain to ask for the import graph. Offline enforces
	// GOPROXY=off and an empty GOMODCACHE.
	Go func(root string, environment []string, args ...string) ([]byte, error)
}

// Check derives each module's closure, asserts it, and compares every file in it.
func Check(options Options) (Report, error) {
	var report Report
	if options.Root == "" {
		return report, fmt.Errorf("vendordrift: no repository root")
	}
	// One spelling for every path this gate compares. `go list` reports resolved
	// directories, while the caller's root is whatever it typed, so a checkout
	// reached through a symlinked prefix (`/tmp` and `$HOME` on macOS, t.TempDir
	// under either) would otherwise be compared against itself and reported as
	// "not under vendor/". See canonical.
	root := canonical(options.Root)
	environment := OfflineEnvironment()
	proxy, err := proxyInEffect(root, environment, options.Go)
	if err != nil {
		return report, err
	}
	report.Proxy = proxy
	if report.Proxy != "off" {
		return report, fmt.Errorf("vendordrift: the run is not offline (GOPROXY=%q)", report.Proxy)
	}
	closure, vendored, err := vendoredPackages(root)
	if err != nil {
		return report, err
	}
	vendorOnly, importedModules, err := importedPackages(root, environment, options.Go)
	if err != nil {
		return report, err
	}
	report.VendorOnly = vendorOnly
	if !vendorOnly {
		return report, fmt.Errorf("vendordrift: the closure is not entirely under vendor/, so the " +
			"gate is not reading the committed tree")
	}
	for _, module := range Modules {
		packages, present := vendored[module.Path]
		if !present {
			return report, fmt.Errorf("vendordrift: vendor/modules.txt records no packages for %s",
				module.Path)
		}
		imported := importedModules[module.Path]
		floor := MinPackages[module.Path]
		if len(packages) < floor {
			return report, fmt.Errorf("vendordrift: derived %d vendored %s packages, want at least "+
				"%d: the derivation is not looking at the module", len(packages), module.Path, floor)
		}
		// No second floor on the imported set: set equality below and the vendored floor
		// above already decide it, and a second check for one fact is not a witness.
		if problem := compareSets(imported, packages); problem != "" {
			return report, fmt.Errorf("vendordrift: %s", problem)
		}
		layerDir := filepath.Join(root, filepath.FromSlash(module.Directory))
		entry := ModuleReport{Module: module, Imported: imported, Vendored: packages,
			Replacement: closure[module.Path]}
		for _, pkg := range packages {
			relative := strings.TrimPrefix(strings.TrimPrefix(pkg, module.Path), "/")
			differences, files, err := ComparePackageDirectories(
				filepath.Join(layerDir, filepath.FromSlash(relative)),
				filepath.Join(root, "vendor", filepath.FromSlash(pkg)))
			entry.Files += files
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
		report.Modules = append(report.Modules, entry)
	}
	if report.Files < MinFiles {
		return report, fmt.Errorf("vendordrift: compared %d files, want at least %d",
			report.Files, MinFiles)
	}
	shared, err := sharedRequirements(root)
	if err != nil {
		return report, err
	}
	report.Shared = shared
	if report.Shared < MinSharedRequirements {
		return report, fmt.Errorf("vendordrift: compared %d shared requirements, want at least %d",
			report.Shared, MinSharedRequirements)
	}
	return report, nil
}

// canonical is the one spelling of a path this gate compares: absolute, with every
// symlink resolved. It is the comparison's requirement, not a relaxation -- both
// sides mean the same file, and only the strings differed.
func canonical(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return absolute
	}
	return resolved
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
// skipped because vendor never carries them, and that is the only exclusion --
// platform-tagged and ignored-by-this-host files are compared like any other, so a
// `_windows.go` edit in the layer and not in vendor is drift here.
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
// packages the build compiles for each helper module, and the local directory the
// module was replaced with.
func vendoredPackages(root string) (map[string]string, map[string][]string, error) {
	raw, err := os.ReadFile(filepath.Join(root, "vendor", "modules.txt"))
	if err != nil {
		return nil, nil, fmt.Errorf("vendordrift: %w", err)
	}
	packages := map[string][]string{}
	replacements := map[string]string{}
	module := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "# ") {
			fields := strings.SplitN(strings.TrimPrefix(line, "# "), " ", 2)
			module = fields[0]
			if rest := strings.TrimSpace(strings.Join(fields[1:], " ")); rest != "" {
				if _, replacement, found := strings.Cut(rest, "=>"); found {
					replacements[module] = strings.TrimSpace(replacement)
				}
			}
			continue
		}
		if strings.HasPrefix(line, "## ") || line == "" {
			continue
		}
		packages[module] = append(packages[module], line)
	}
	for _, helper := range Modules {
		local, present := replacements[helper.Path]
		if !present {
			return nil, nil, fmt.Errorf("vendordrift: vendor/modules.txt has no local replacement "+
				"for %s", helper.Path)
		}
		if want := "./" + helper.Directory; local != want {
			return nil, nil, fmt.Errorf("vendordrift: %s is replaced with %s, want %s", helper.Path,
				local, want)
		}
		sort.Strings(packages[helper.Path])
	}
	return replacements, packages, nil
}

// importedPackages is the writer's own import graph: each helper module's packages in
// it, and whether every other module dependency resolved under vendor/.
func importedPackages(root string, environment []string, run func(string, []string,
	...string) ([]byte, error)) (bool, map[string][]string, error) {
	if run == nil {
		run = goCommand
	}
	raw, err := run(root, environment, "list", "-deps", "-f",
		"{{.ImportPath}}\t{{.Dir}}", "./cmd/avm-pin")
	if err != nil {
		return false, nil, fmt.Errorf("vendordrift: go list: %w", err)
	}
	vendorRoot := filepath.Join(root, "vendor") + string(filepath.Separator)
	imported := map[string][]string{}
	vendorOnly := true
	seen := 0
	for _, line := range strings.Split(string(raw), "\n") {
		path, directory, found := strings.Cut(strings.TrimSpace(line), "\t")
		if !found || path == "" {
			continue
		}
		seen++
		for _, helper := range Modules {
			if path == helper.Path || strings.HasPrefix(path, helper.Path+"/") {
				imported[helper.Path] = append(imported[helper.Path], path)
			}
		}
		if !isModulePath(path) || strings.HasPrefix(path, MainModule+"/") &&
			!isHelperPackage(path) {
			continue
		}
		if !strings.HasPrefix(canonical(directory), vendorRoot) {
			vendorOnly = false
		}
	}
	if seen < MinClosurePackages {
		return false, nil, fmt.Errorf("vendordrift: go list named %d packages, want at least %d",
			seen, MinClosurePackages)
	}
	for _, helper := range Modules {
		sort.Strings(imported[helper.Path])
	}
	return vendorOnly, imported, nil
}

// isHelperPackage reports whether a package path belongs to one of the helper modules.
func isHelperPackage(path string) bool {
	for _, helper := range Modules {
		if path == helper.Path || strings.HasPrefix(path, helper.Path+"/") {
			return true
		}
	}
	return false
}

// sharedRequirements is ADR 0017's link 5, measured at the gate: every requirement a
// helper module names must be required at the same version by the root module, so MVS
// in the root cannot raise a version and compile the writer against a library the
// layer's own build never selects. It returns how many pairs it compared.
func sharedRequirements(root string) (int, error) {
	rootPins, err := modulePins(filepath.Join(root, "go.mod"))
	if err != nil {
		return 0, err
	}
	compared := 0
	for _, helper := range Modules {
		helperPins, err := modulePins(filepath.Join(root, filepath.FromSlash(helper.Directory),
			"go.mod"))
		if err != nil {
			return 0, err
		}
		// The link itself: the root requires the module at v0.0.0, and the local
		// replacement is checked with the vendored record above.
		if version, present := rootPins[helper.Path]; !present || version != "v0.0.0" {
			return 0, fmt.Errorf("vendordrift: the root go.mod must require %s at v0.0.0, found %q",
				helper.Path, version)
		}
		paths := make([]string, 0, len(helperPins))
		for path := range helperPins {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			rootVersion, present := rootPins[path]
			if !present {
				continue
			}
			if rootVersion != helperPins[path] {
				return 0, fmt.Errorf("vendordrift: the two go.mod files require different versions "+
					"of a shared module: %s is %s in %s and %s in the root", path,
					helperPins[path], helper.Path, rootVersion)
			}
			compared++
		}
	}
	return compared, nil
}

// modulePins reads one go.mod's requirements, both the block form and the one-line
// form. An unreadable or empty go.mod is a refusal: a link-5 comparison over nothing is
// not a pass.
func modulePins(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("vendordrift: %w", err)
	}
	pins := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch {
		case fields[0] == "require" && len(fields) == 2 && fields[1] == "(":
			inBlock = true
			continue
		case inBlock && fields[0] == ")":
			inBlock = false
			continue
		case inBlock:
			if len(fields) >= 2 {
				pins[fields[0]] = fields[1]
			}
			continue
		case fields[0] == "require" && len(fields) >= 3:
			pins[fields[1]] = fields[2]
		}
	}
	if len(pins) == 0 {
		return nil, fmt.Errorf("vendordrift: %s records no requirements to compare", path)
	}
	return pins, nil
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
