package toolchain

// Manifest semantics: what rustup 1.28.2 would install from the signed channel
// manifest, compared against the committed closure.
//
// rustup 1.28.2 derives an installation set from the *rust* package's per-host
// tables, not from package availability alone. `src/dist/manifest.rs` merges the
// host's `extensions` rows into its `components` vector with `is_extension =
// true`, and every later lookup — the minimal profile in `get_profile_components`
// and the explicit-component wildcard check in `src/dist/mod.rs` — searches that
// single merged vector. A check that models two independent tables, or that only
// asks "is this package available", can approve a selection rustup will never
// install or miss one it would.

import (
	"sort"
	"strings"
)

// RustComponent is one row of rustup's merged rust-package component table. A
// nil Target is rustup's target-independent `*` row, which stays
// target-independent (it is never rewritten to the host triple).
type RustComponent struct {
	Package     string
	Target      *TargetTriple
	IsExtension bool
}

// VerifyManifestClosure requires the manifest to be the reviewed shape and to
// derive exactly the committed selection for both architectures.
func VerifyManifestClosure(raw []byte, pin Pin) error {
	manifest, err := decodeTOML(raw, ManifestPath)
	if err != nil {
		return err
	}
	if _, err := requireKeys(manifest, []string{"manifest-version", "date", "pkg", "renames",
		"profiles"}, []string{"artifacts"}, "manifest"); err != nil {
		return err
	}
	if value, present := manifest["manifest-version"]; !present || value != "2" {
		return refuse("manifest: unsupported manifest-version")
	}
	dateNode, err := fieldOf(manifest, "date", "manifest")
	if err != nil {
		return err
	}
	date, err := ParseDate(dateNode, "manifest.date")
	if err != nil {
		return err
	}
	if date != pin.ReleaseDate {
		return refuse("manifest: release date does not match the pin")
	}
	version, present, err := lookup(manifest, "pkg", "rust", "version")
	if err != nil {
		return err
	}
	text, isText := version.(string)
	if !present || !isText || !strings.HasPrefix(text, string(pin.Channel)+" ") {
		return refuse("manifest: release version does not match the pinned channel")
	}
	if err := reviewedRenames(manifest); err != nil {
		return err
	}
	if _, err := reviewedProfile(manifest); err != nil {
		return err
	}
	for _, arch := range Architectures() {
		closure := pin.Closure(arch)
		derived, err := deriveSelection(manifest, arch)
		if err != nil {
			return err
		}
		committed := make([][2]string, 0, len(closure.Archives))
		for _, archive := range closure.Archives {
			committed = append(committed, [2]string{archive.Package, string(archive.Target)})
		}
		sortPairs(committed)
		if !samePairs(derived, committed) {
			return refuse("%s: the committed selection is not the manifest-derived Linux "+
				"minimal + clippy/rustfmt + musl closure", arch.Platform())
		}
		for _, archive := range closure.Archives {
			entry, err := selectedEntry(manifest, archive.Package, archive.Target)
			if err != nil {
				return err
			}
			if available, ok := entry["available"]; !ok || available != true {
				return refuse("manifest: %s %s is not available", archive.Package, archive.Target)
			}
			url, urlOK := entry["xz_url"].(string)
			hash, hashOK := entry["xz_hash"].(string)
			if !urlOK || !hashOK {
				return refuse("manifest: %s %s lacks a full xz pair", archive.Package,
					archive.Target)
			}
			if url != string(archive.Content.URL) {
				return refuse("manifest: xz_url for %s %s differs from the pin", archive.Package,
					archive.Target)
			}
			if hash != string(archive.Content.Digest) {
				return refuse("manifest: xz_hash for %s %s differs from the pin", archive.Package,
					archive.Target)
			}
			if err := verifyAlternativeFormatPair(entry, archive); err != nil {
				return err
			}
		}
		hostEntry, err := rustHostEntry(manifest, arch)
		if err != nil {
			return err
		}
		if available, ok := hostEntry["available"]; !ok || available != true {
			return refuse("manifest: no available rust package for %s", closure.Host)
		}
	}
	return nil
}

func samePairs(left, right [][2]string) bool {
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

// verifyAlternativeFormatPair requires a selected entry's gzip pair to be
// all-or-nothing and on the same dated origin. rustup would fall back to the
// format whose pair is complete, so a dropped half changes what a future rustup
// can fetch from bytes the pin never names.
func verifyAlternativeFormatPair(entry map[string]any, archive ComponentArchive) error {
	what := "manifest: " + archive.Package + " " + string(archive.Target)
	url, hasURL := entry["url"]
	hash, hasHash := entry["hash"]
	if hasURL != hasHash {
		return refuse("%s carries a half gzip format pair", what)
	}
	if !hasURL {
		return nil
	}
	urlText, urlOK := url.(string)
	hashText, hashOK := hash.(string)
	if !urlOK || !hashOK {
		return refuse("%s gzip pair is not a string url and hash", what)
	}
	expected := strings.TrimSuffix(string(archive.Content.URL), ".tar.xz") + ".tar.gz"
	if urlText != expected {
		return refuse("%s gzip url is not the same dated origin as its xz pair", what)
	}
	if !isLowerHex(hashText, 64) {
		return refuse("%s gzip hash is not a lowercase SHA-256 digest", what)
	}
	return nil
}

// reviewedRenames requires both reviewed renames and nothing else about them.
func reviewedRenames(manifest map[string]any) error {
	renames, present, err := lookup(manifest, "renames")
	if err != nil {
		return err
	}
	if !present {
		renames = map[string]any{}
	}
	for _, rename := range ReviewedRenames {
		short, renamed := rename[0], rename[1]
		entry, present, err := lookup(renames, short)
		if err != nil {
			return refuse("manifest: missing rename for %s; schema review required", short)
		}
		if !present {
			return refuse("manifest: missing rename for %s; schema review required", short)
		}
		table, err := requireKeys(entry, []string{"to"}, nil, "manifest.renames."+short)
		if err != nil {
			return err
		}
		if table["to"] != renamed {
			return refuse("manifest: unexpected rename for %s; schema review required", short)
		}
	}
	return nil
}

// reviewedProfile requires rustup's minimal profile to be exactly the reviewed
// one, and returns it.
func reviewedProfile(manifest map[string]any) ([]string, error) {
	profiles, present, err := lookup(manifest, "profiles")
	if err != nil {
		return nil, err
	}
	table, ok := profiles.(map[string]any)
	if !present || !ok {
		return nil, refuse("manifest: no profiles table")
	}
	minimal, ok := table["minimal"].([]any)
	if !ok || len(minimal) != len(ReviewedProfile) {
		return nil, refuse("manifest: the minimal profile changed; schema/policy review required")
	}
	for index, want := range ReviewedProfile {
		if minimal[index] != want {
			return nil, refuse("manifest: the minimal profile changed; schema/policy review " +
				"required")
		}
	}
	return ReviewedProfile, nil
}

// rustHostEntry is the rust package's entry for one host.
func rustHostEntry(manifest map[string]any, arch Architecture) (map[string]any, error) {
	pkg, present, err := lookup(manifest, "pkg", "rust")
	if err != nil {
		return nil, refuse("manifest: no rust package")
	}
	if !present {
		return nil, refuse("manifest: no rust package")
	}
	if _, ok := pkg.(map[string]any); !ok {
		return nil, refuse("manifest: no rust package")
	}
	entry, present, err := lookup(pkg, "target", string(arch.Host()))
	if err != nil {
		return nil, refuse("manifest: no rust package entry for %s", arch.Host())
	}
	if !present {
		return nil, refuse("manifest: no rust package entry for %s", arch.Host())
	}
	return requireKeys(entry, []string{"available", "components", "extensions"},
		RustHostEntryKeys, "manifest rust "+string(arch.Host()))
}

// componentRows reads one component table, refusing the invented singular
// `extension` flag: rustup's field is `is_extension`, its own merge forces it
// true on every extensions row, and no profile lookup filters on it, so
// honouring the singular flag here would diverge from the installer both ways.
func componentRows(rows any, what string, isExtension bool) ([]RustComponent, error) {
	items, ok := rows.([]any)
	if !ok {
		return nil, refuse("manifest: %s is not a list", what)
	}
	result := make([]RustComponent, 0, len(items))
	for _, raw := range items {
		if table, ok := raw.(map[string]any); ok {
			if _, invented := table["extension"]; invented {
				return nil, refuse("manifest %s: `extension` is not a rustup component "+
					"field; schema/policy review required", what)
			}
		}
		row, err := requireKeys(raw, []string{"pkg"}, ComponentKeys, "manifest "+what)
		if err != nil {
			return nil, err
		}
		pkgNode, err := fieldOf(row, "pkg", "manifest "+what)
		if err != nil {
			return nil, err
		}
		pkg, err := exactTextField(pkgNode, "manifest "+what+".pkg")
		if err != nil {
			return nil, err
		}
		if pkg == "" {
			return nil, refuse("manifest %s.pkg: empty package name", what)
		}
		component := RustComponent{Package: pkg, IsExtension: isExtension}
		if targetNode, present := row["target"]; present {
			value, err := exactTextField(targetNode, "manifest "+what+".target")
			if err != nil {
				return nil, err
			}
			if value != "*" {
				target, err := ParseTriple(value, "manifest "+what+".target")
				if err != nil {
					return nil, err
				}
				component.Target = &target
			}
		}
		result = append(result, component)
	}
	return result, nil
}

// rustComponentTable is the host's components and extensions as rustup merges
// them: components first, then extensions, because the merged table is searched
// with `find` and the first match wins. An exact duplicate row is refused as a
// reviewed-schema error even though the first would win.
func rustComponentTable(manifest map[string]any, arch Architecture) ([]RustComponent, error) {
	entry, err := rustHostEntry(manifest, arch)
	if err != nil {
		return nil, err
	}
	host := string(arch.Host())
	components, err := componentRows(entry["components"], host+" components", false)
	if err != nil {
		return nil, err
	}
	extensions, err := componentRows(entry["extensions"], host+" extensions", true)
	if err != nil {
		return nil, err
	}
	seen := map[componentKey]bool{}
	for _, row := range components {
		key := componentKey{row.Package, targetValue(row.Target)}
		if seen[key] {
			return nil, refuse("manifest: %s is listed twice in the %s components table",
				row.Package, host)
		}
		seen[key] = true
	}
	return append(components, extensions...), nil
}

type componentKey struct {
	packet string
	target string
}

func targetValue(target *TargetTriple) string {
	if target == nil {
		return ""
	}
	return string(*target)
}

// hostProvides reports whether a package is available for a host.
func hostProvides(manifest map[string]any, name string, host TargetTriple) bool {
	entry, present, err := lookup(manifest, "pkg", name, "target", string(host))
	if err != nil || !present {
		return false
	}
	table, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	return table["available"] == true
}

// profileComponents is `get_profile_components`: the first merged row per
// profile name, host or wildcard, keeping the row's own target.
func profileComponents(table []RustComponent, profile []string,
	host TargetTriple) [][2]any {
	var selected [][2]any
	for _, name := range profile {
		for _, row := range table {
			if row.Package != name {
				continue
			}
			if row.Target != nil && *row.Target != host {
				continue
			}
			selected = append(selected, [2]any{name, row.Target})
			break
		}
	}
	return selected
}

// explicitComponents is `dist/mod.rs`: rename the requested component, then
// honour a wildcard row.
func explicitComponents(table []RustComponent, arch Architecture) [][2]any {
	renames := map[string]string{}
	for _, rename := range ReviewedRenames {
		renames[rename[0]] = rename[1]
	}
	var selected [][2]any
	for _, short := range CLIComponents {
		renamed := renames[short]
		var first *RustComponent
		for index, row := range table {
			if row.Package == renamed {
				first = &table[index]
				break
			}
		}
		var target *TargetTriple
		host := arch.Host()
		target = &host
		if first != nil && first.Target == nil {
			target = nil
		}
		selected = append(selected, [2]any{renamed, target})
	}
	return selected
}

// rustupSelection is exactly what rustup 1.28.2 resolves for the pinned request,
// keyed by (package, target) as rustup's HashSet<Component> equality does.
func rustupSelection(manifest map[string]any, arch Architecture) (map[[2]string]bool, error) {
	table, err := rustComponentTable(manifest, arch)
	if err != nil {
		return nil, err
	}
	profile, err := reviewedProfile(manifest)
	if err != nil {
		return nil, err
	}
	selected := map[[2]string]bool{}
	for _, pair := range profileComponents(table, profile, arch.Host()) {
		selected[[2]string{pair[0].(string), targetValue(pair[1].(*TargetTriple))}] = true
	}
	for _, pair := range explicitComponents(table, arch) {
		selected[[2]string{pair[0].(string), targetValue(pair[1].(*TargetTriple))}] = true
	}
	selected[[2]string{"rust-std", string(arch.Musl())}] = true
	return selected, nil
}

// requireExtension requires exactly one host-extension row for a package and
// target.
func requireExtension(extensions []RustComponent, pkg string, target TargetTriple,
	arch Architecture) error {
	matches := 0
	for _, row := range extensions {
		if row.Package == pkg && row.Target != nil && *row.Target == target {
			matches++
		}
	}
	if matches != 1 {
		return refuse("manifest: %s for %s is not a single %s extension; the committed "+
			"closure needs it or a schema/policy review", pkg, target, arch.Host())
	}
	return nil
}

// deriveSelection is the reviewed closure derivable from this manifest before
// any fetch, sorted by (package, target) as the source reader sorts it.
func deriveSelection(manifest map[string]any, arch Architecture) ([][2]string, error) {
	selection, err := rustupSelection(manifest, arch)
	if err != nil {
		return nil, err
	}
	var wildcard []string
	for pair := range selection {
		if pair[1] == "" {
			wildcard = append(wildcard, pair[0])
		}
	}
	sort.Strings(wildcard)
	if len(wildcard) != 0 {
		return nil, refuse("manifest: rustup would install the target-independent "+
			"component(s) %s for %s; a wildcard package is not representable in the "+
			"reviewed pin and needs a schema/policy review", strings.Join(wildcard, ", "),
			arch.Host())
	}
	profile, err := reviewedProfile(manifest)
	if err != nil {
		return nil, err
	}
	for _, name := range profile {
		selected := false
		for pair := range selection {
			if pair[0] == name {
				selected = true
				break
			}
		}
		if !selected && hostProvides(manifest, name, arch.Host()) {
			return nil, refuse("manifest: %s is available for %s but absent from its "+
				"components table; schema/policy review required", name, arch.Host())
		}
	}
	table, err := rustComponentTable(manifest, arch)
	if err != nil {
		return nil, err
	}
	var extensions []RustComponent
	for _, row := range table {
		if row.IsExtension {
			extensions = append(extensions, row)
		}
	}
	renames := map[string]string{}
	for _, rename := range ReviewedRenames {
		renames[rename[0]] = rename[1]
	}
	for _, short := range CLIComponents {
		if err := requireExtension(extensions, renames[short], arch.Host(), arch); err != nil {
			return nil, err
		}
	}
	if err := requireExtension(extensions, "rust-std", arch.Musl(), arch); err != nil {
		return nil, err
	}
	derived := make([][2]string, 0, len(selection))
	for pair := range selection {
		if pair[1] == "" {
			continue
		}
		derived = append(derived, pair)
	}
	sortPairs(derived)
	return derived, nil
}

// selectedEntry is one selected archive's manifest entry, refusing a zstd
// advertisement: rustup 1.28.2 prefers zstd, so the xz pin would be bypassed.
func selectedEntry(manifest map[string]any, pkg string, target TargetTriple) (map[string]any, error) {
	entry, err := manifestEntry(manifest, pkg, target)
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"zst_url", "zst_hash"} {
		if _, present := entry[key]; present {
			return nil, refuse("manifest: %s %s advertises zstd; rustup 1.28.2 prefers "+
				"zstd, so the xz pin would be bypassed", pkg, target)
		}
	}
	return requireKeys(entry, []string{"available"}, SelectedEntryKeys,
		"manifest "+pkg+" "+string(target))
}

// manifestEntry is the package's entry for one target triple.
func manifestEntry(manifest map[string]any, pkg string, target TargetTriple) (map[string]any, error) {
	pkgNode, present, err := lookup(manifest, "pkg", pkg)
	if err != nil || !present {
		return nil, refuse("manifest: no package %s", pkg)
	}
	if _, ok := pkgNode.(map[string]any); !ok {
		return nil, refuse("manifest: no package %s", pkg)
	}
	entry, present, err := lookup(pkgNode, "target", string(target))
	if err != nil || !present {
		return nil, refuse("manifest: no %s entry for %s", pkg, target)
	}
	table, ok := entry.(map[string]any)
	if !ok {
		return nil, refuse("manifest: no %s entry for %s", pkg, target)
	}
	return table, nil
}
