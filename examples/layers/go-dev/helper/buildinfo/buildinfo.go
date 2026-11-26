// Package buildinfo is the one owner of the build information a Go binary reports
// about itself: the strict decode, the shape rules, and the comparison against the
// committed closure and the reviewed Linux target settings.
//
// What this is not: authentication of the binary. `go version -m -json` reads the
// metadata the binary carries; nothing signs it and nothing verifies a signature
// here. What the check establishes is a contradiction test — a candidate that lies
// in its own build info is caught only where the lie disagrees with the pin (a
// different compiler, root, dependency set, content hash or target). A coherent
// forgery that reports the reviewed values is not detected by this program, and no
// claim here should be read as if it were.
//
// The Python reader used a permissive JSON decoder for this document (#188 G3: the
// pin is strict, the build info was not). This package uses the strict decoder, so
// a duplicate key, NaN, a leading BOM or trailing data is a refusal. That is a
// declared divergence from the oracle while it lives: script/test/gopls-closure-pins.py
// runs both readers over the same document and declares each side's verdict, and the
// two divergent cases (a duplicate key whose value agrees, a non-JSON constant in a
// member the oracle does not read) are named there. Deleting the strict decoder makes
// those cases red at verdict level.
//
// A third divergence runs the other way and is declared too: Python converts every
// number literal it parses and has a 4,300-digit limit, so a document with a 5,000-digit
// literal in an optional member it does not read has no oracle verdict at all
// (`ValueError`), while this reader accepts it. That is deliberate: the literal is kept
// as written and never converted, and the member is unread, so there is nothing to
// decide. It is also why Python's digit limit is not a denial-of-service surface here:
// this package never turns a literal into a big integer, and the pin loader's only
// conversion is `schema` through strconv.Atoi, which rejects an out-of-range literal
// without allocating one.
package buildinfo

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/closure"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/gotool"
	"github.com/gregwebs/agent-vm-images/examples/layers/go-dev/helper/internal/strictjson"
)

// Module is one module identity as a binary reports it: the path, the selected
// version and the zip content hash. It is the same triple the pin records, so the
// comparison is between two validated values and never between strings.
type Module struct {
	Path    closure.ModulePath
	Version closure.ModuleVersion
	H1      closure.H1
}

// ID is the module identity without its hash, `path@version`.
func (m Module) ID() closure.ModuleID {
	return closure.ModuleID{Path: m.Path, Version: m.Version}
}

// Info is one decoded build-info document.
type Info struct {
	GoVersion closure.GoVersion
	Main      Module
	Deps      []Module
	Settings  map[string]string
}

// keys are the members a build-info document carries, in the reader's order. Path
// is the binary's path, which a tool binary need not have.
var keys = []string{"GoVersion", "Main", "Deps", "Settings"}

// optionalKeys is the one optional member.
var optionalKeys = []string{"Path"}

// moduleKeys are the members of a main or dependency module row.
var moduleKeys = []string{"Path", "Version", "Sum"}

// moduleOptionalKeys is the one optional member: a replaced module names its
// replacement, which this closure never contains.
var moduleOptionalKeys = []string{"Replace"}

// Parse decodes one build-info document. It reads only the bytes it is given.
func Parse(raw []byte) (Info, error) {
	document, err := strictjson.Decode(raw, "build info")
	if err != nil {
		return Info{}, err
	}
	members, err := requireKeys(document, keys, optionalKeys, "build info")
	if err != nil {
		return Info{}, err
	}
	versionValue, err := fieldOf(members, "GoVersion", "build info")
	if err != nil {
		return Info{}, err
	}
	versionText, err := exactString(versionValue, "build info GoVersion")
	if err != nil {
		return Info{}, err
	}
	// The compiler prints `go1.27.3`; the pin records `1.27.3`.
	versionText = strings.TrimPrefix(versionText, "go")
	version, err := closure.ParseGoVersion(versionText, "build info GoVersion")
	if err != nil {
		return Info{}, err
	}
	mainValue, err := fieldOf(members, "Main", "build info")
	if err != nil {
		return Info{}, err
	}
	main, err := parseModule(mainValue, "build info Main")
	if err != nil {
		return Info{}, err
	}
	depsValue, err := fieldOf(members, "Deps", "build info")
	if err != nil {
		return Info{}, err
	}
	depItems, ok := depsValue.([]strictjson.Value)
	if !ok {
		return Info{}, fmt.Errorf("build info Deps: expected a list")
	}
	deps := make([]Module, 0, len(depItems))
	seen := map[closure.ModulePath]struct{}{}
	for _, item := range depItems {
		dep, err := parseModule(item, "build info Deps")
		if err != nil {
			return Info{}, err
		}
		if _, duplicate := seen[dep.Path]; duplicate {
			return Info{}, fmt.Errorf("build info: duplicate dependency")
		}
		seen[dep.Path] = struct{}{}
		deps = append(deps, dep)
	}
	settingsValue, err := fieldOf(members, "Settings", "build info")
	if err != nil {
		return Info{}, err
	}
	settingItems, ok := settingsValue.([]strictjson.Value)
	if !ok {
		return Info{}, fmt.Errorf("build info Settings: expected a list")
	}
	settings := map[string]string{}
	for _, item := range settingItems {
		node, err := requireKeys(item, []string{"Key", "Value"}, nil, "build info Settings")
		if err != nil {
			return Info{}, err
		}
		keyValue, err := fieldOf(node, "Key", "build info Settings")
		if err != nil {
			return Info{}, err
		}
		key, err := exactString(keyValue, "settings.Key")
		if err != nil {
			return Info{}, err
		}
		settingValue, err := fieldOf(node, "Value", "build info Settings")
		if err != nil {
			return Info{}, err
		}
		value, err := exactString(settingValue, "settings.Value")
		if err != nil {
			return Info{}, err
		}
		if _, duplicate := settings[key]; duplicate {
			return Info{}, fmt.Errorf("build info: duplicate setting %s", key)
		}
		settings[key] = value
	}
	return Info{GoVersion: version, Main: main, Deps: deps, Settings: settings}, nil
}

// parseModule reads one module row.
func parseModule(node any, what string) (Module, error) {
	members, err := requireKeys(node, moduleKeys, moduleOptionalKeys, what)
	if err != nil {
		return Module{}, err
	}
	if _, present := members["Replace"]; present {
		return Module{}, fmt.Errorf("%s: module replacement is not part of the reviewed "+
			"closure", what)
	}
	pathValue, err := fieldOf(members, "Path", what)
	if err != nil {
		return Module{}, err
	}
	path, err := closure.ParseModulePath(pathValue, what+".Path")
	if err != nil {
		return Module{}, err
	}
	versionValue, err := fieldOf(members, "Version", what)
	if err != nil {
		return Module{}, err
	}
	version, err := closure.ParseModuleVersion(versionValue, what+".Version")
	if err != nil {
		return Module{}, err
	}
	sumValue, err := fieldOf(members, "Sum", what)
	if err != nil {
		return Module{}, err
	}
	h1, err := closure.ParseH1(sumValue, what+".Sum")
	if err != nil {
		return Module{}, err
	}
	return Module{Path: path, Version: version, H1: h1}, nil
}

// Verify compares one build-info document against the reviewed selection and the
// settings this Linux target must show.
func Verify(selection []Module, info Info, arch gotool.Architecture,
	compiler closure.GoVersion) error {
	if info.GoVersion != compiler {
		return fmt.Errorf("compiler is go%s, want go%s", info.GoVersion, compiler)
	}
	root, present := find(selection, closure.ModulePath(closure.Gopls))
	if !present {
		return fmt.Errorf("the reviewed selection does not name the root module %s",
			closure.Gopls)
	}
	if info.Main != root {
		return fmt.Errorf("build used %s %s, not the reviewed root %s %s", info.Main.ID(),
			info.Main.H1, root.ID(), root.H1)
	}
	// The dependencies are every selected module but the root: the root is `Main`,
	// not a `Deps` row, so a selection's root must never be expected among them.
	dependencies := make([]Module, 0, len(selection))
	expected := map[closure.ModulePath]Module{}
	for _, module := range selection {
		if module.Path == root.Path {
			continue
		}
		dependencies = append(dependencies, module)
		expected[module.Path] = module
	}
	actual := map[closure.ModulePath]Module{}
	for _, module := range info.Deps {
		actual[module.Path] = module
	}
	if unknown := sortedDifference(actual, expected); len(unknown) > 0 {
		return fmt.Errorf("build used %s, not in the reviewed closure", actual[unknown[0]].ID())
	}
	if missing := sortedDifference(expected, actual); len(missing) > 0 {
		return fmt.Errorf("build omitted %s from the reviewed closure", missing[0])
	}
	for _, module := range dependencies {
		reported := actual[module.Path]
		if reported.Version != module.Version {
			return fmt.Errorf("build used %s, not in the reviewed closure", reported.ID())
		}
		if reported.H1 != module.H1 {
			return fmt.Errorf("build used %s %s, not the reviewed content", module.Path,
				reported.H1)
		}
	}
	if settingsDifference(info.Settings, ExpectedSettings(arch)) != "" {
		return fmt.Errorf("build settings differ from the reviewed Linux target settings: "+
			"%s != %s", renderSettings(info.Settings), renderSettings(ExpectedSettings(arch)))
	}
	return nil
}

// find is the reviewed selection's row for one module path.
func find(selection []Module, path closure.ModulePath) (Module, bool) {
	for _, module := range selection {
		if module.Path == path {
			return module, true
		}
	}
	return Module{}, false
}

// sortedDifference is the sorted paths of have that want does not carry.
func sortedDifference(have, want map[closure.ModulePath]Module) []closure.ModulePath {
	paths := []string{}
	index := map[string]closure.ModulePath{}
	for path := range have {
		if _, present := want[path]; !present {
			paths = append(paths, string(path))
			index[string(path)] = path
		}
	}
	sort.Strings(paths)
	difference := make([]closure.ModulePath, 0, len(paths))
	for _, path := range paths {
		difference = append(difference, index[path])
	}
	return difference
}

// ExpectedSettings is what this closure's Linux cross-build must report.
//
// An unknown key is a compatibility tripwire, not a difference to accept: a Go
// release that adds a setting needs a deliberate change here and in ADR 0008.
func ExpectedSettings(arch gotool.Architecture) map[string]string {
	settings := map[string]string{
		"-buildmode":     "exe",
		"-compiler":      "gc",
		"-trimpath":      "true",
		"DefaultGODEBUG": "tracebacklabels=0,x509sslcertoverrideplatform=0",
		"CGO_ENABLED":    "0",
		"GOOS":           "linux",
		"GOARCH":         arch.Value(),
	}
	if arch == gotool.AMD64 {
		settings["GOAMD64"] = "v1"
	} else {
		settings["GOARM64"] = "v8.0"
	}
	return settings
}

// settingsDifference names the first disagreement in sorted key order, including an
// unknown or missing key. It is a function rather than an inline comparison so a
// test can hold every direction of the tripwire.
func settingsDifference(actual, expected map[string]string) string {
	for _, key := range sortedSettingKeys(actual, expected) {
		reported, inActual := actual[key]
		wanted, inExpected := expected[key]
		if !inActual || !inExpected || reported != wanted {
			return key
		}
	}
	return ""
}

func sortedSettingKeys(maps ...map[string]string) []string {
	seen := map[string]struct{}{}
	for _, entries := range maps {
		for key := range entries {
			seen[key] = struct{}{}
		}
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// renderSettings prints a settings map in sorted key order. Python printed its
// insertion order; the difference is the diagnostic's rendering only, never a
// verdict, and a diagnostic whose order depended on a map iteration would differ
// between two runs.
func renderSettings(settings map[string]string) string {
	parts := make([]string, 0, len(settings))
	for _, key := range sortedSettingKeys(settings) {
		parts = append(parts, fmt.Sprintf("'%s': '%s'", key, settings[key]))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// requireKeys enforces an exact key set. It is the same rule closure applies to the
// pin document, restated here because a build-info document is a different
// contract: this package owns this one, and the wording is the Python reader's
// where a suite matches it.
func requireKeys(node any, required, optional []string, what string) (map[string]any, error) {
	members, ok := node.(map[string]strictjson.Value)
	if !ok {
		return nil, fmt.Errorf("%s: expected an object", what)
	}
	for _, key := range required {
		if _, present := members[key]; !present {
			return nil, fmt.Errorf("%s: missing %s", what, quote(key))
		}
	}
	allowed := make(map[string]struct{}, len(required)+len(optional))
	for _, key := range required {
		allowed[key] = struct{}{}
	}
	for _, key := range optional {
		allowed[key] = struct{}{}
	}
	unknown := []string{}
	for key := range members {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: unknown key %s", what, quote(unknown[0]))
	}
	return members, nil
}

// fieldOf reads one member and names the missing key with its own context.
func fieldOf(node any, key, what string) (any, error) {
	members, ok := node.(map[string]strictjson.Value)
	if !ok {
		return nil, fmt.Errorf("%s: expected an object", what)
	}
	value, present := members[key]
	if !present {
		return nil, fmt.Errorf("%s: missing %s", what, quote(key))
	}
	return value, nil
}

// exactString is Python's `exact_type(value, str)`.
func exactString(value any, what string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s: expected str", what)
	}
	return text, nil
}

// quote renders a key the way the Python reader's repr did.
func quote(text string) string { return "'" + text + "'" }
