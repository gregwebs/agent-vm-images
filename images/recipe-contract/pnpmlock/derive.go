package pnpmlock

import (
	"sort"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// RequirementKind is the closed set of derivation requirement rows.
type RequirementKind uint8

const (
	ExactPeerRequirement RequirementKind = iota
	RangePeerRequirement
	DependencyRequirement
)

// String is the wire name the proof JSON carries.
func (k RequirementKind) String() string {
	switch k {
	case ExactPeerRequirement:
		return "exact-peer"
	case RangePeerRequirement:
		return "range-peer"
	case DependencyRequirement:
		return "dependency"
	default:
		panic("pnpmlock: unhandled requirement kind")
	}
}

// Requirement is one declaration the tentative graph carries.
type Requirement struct {
	Selector       Selector
	Version        string
	Package        PackageName
	PackageVersion ExactVersion
	Integrity      Integrity
	Peer           PackageName
	Kind           RequirementKind
}

// Value renders the row in the retired key order.
func (r Requirement) Value() document.Value {
	return mappingValue(
		entry("selector", jsStringValue(r.Selector.String())),
		entry("version", jsStringValue(r.Version)),
		entry("package", jsStringValue(r.Package.String())),
		entry("packageVersion", jsStringValue(r.PackageVersion.String())),
		entry("integrity", jsStringValue(r.Integrity.String())),
		entry("peer", jsStringValue(r.Peer.String())),
		entry("kind", jsStringValue(r.Kind.String())),
	)
}

// AppendJS emits JSON.stringify(row), which is also the derivation's dedup key.
func (r Requirement) AppendJS(dst []byte) []byte { return r.Value().AppendJS(dst) }

// OverrideDelta is one overrides entry and its committed predecessor.
type OverrideDelta struct {
	Selector Selector
	Before   *ExactVersion
	After    ExactVersion
}

// Value renders the delta row; a nil Before is the JSON null.
func (d OverrideDelta) Value() document.Value {
	before := document.NullValue()
	if d.Before != nil {
		before = jsStringValue(d.Before.String())
	}
	return mappingValue(
		entry("selector", jsStringValue(d.Selector.String())),
		entry("before", before),
		entry("after", jsStringValue(d.After.String())),
	)
}

// Derivation is the reviewable result: the override map, every declaration that
// justified it, and the change against the committed map.
type Derivation struct {
	Overrides    OverrideMap
	Requirements []Requirement
	Delta        []OverrideDelta
}

// Value renders the derivation in the retired key order.
func (d Derivation) Value() document.Value {
	requirements := make([]document.Value, 0, len(d.Requirements))
	for _, requirement := range d.Requirements {
		requirements = append(requirements, requirement.Value())
	}
	delta := make([]document.Value, 0, len(d.Delta))
	for _, row := range d.Delta {
		delta = append(delta, row.Value())
	}
	return mappingValue(
		entry("overrides", d.Overrides.Value()),
		entry("requirements", sequenceValue(requirements)),
		entry("delta", sequenceValue(delta)),
	)
}

// AppendJS emits JSON.stringify(derivation).
func (d Derivation) AppendJS(dst []byte) []byte { return d.Value().AppendJS(dst) }

// ReviewedMaps are the only tie-breaks a graph cannot decide for itself: the
// committed map is the legitimate no-op re-pin, the approved map is a reviewer's
// explicit decision and outranks it.
type ReviewedMaps struct {
	Committed OverrideMap
	Approved  OverrideMap
}

type family struct {
	peer         PackageName
	selector     Selector
	selections   []ExactVersion
	declarations []Requirement
}

type constraintRow struct {
	owner   *PackageName
	version ExactVersion
	wanted  string
	source  string
}

// DeriveCordisOverrides reads the tentative graph and derives the Cordis pins it
// supports. It never contacts a registry and never picks a version the graph did
// not select.
func DeriveCordisOverrides(tentative Prefix, reviewed ReviewedMaps) (Derivation, error) {
	project, err := readProjectInputs(tentative, KindDsh, skipPins, readTentative)
	if err != nil {
		return Derivation{}, err
	}
	archives := map[string]*Archive{}
	for position := range project.packages {
		pkg := &project.packages[position]
		archives[pkg.ID.Key()] = pkg
	}
	families := make([]family, 0, len(selectorOrder))
	for _, selector := range selectorOrder {
		peer := selector.Peer()
		build := family{peer: peer, selector: selector}
		for _, pkg := range project.packages {
			if pkg.ID.Name == peer {
				build.selections = append(build.selections, pkg.ID.Version)
			}
		}
		for _, pkg := range project.packages {
			specification, declared := pkg.Peers.Get(peer)
			if !declared {
				continue
			}
			class, err := peerSpecClass(specification)
			if err != nil {
				return Derivation{}, err
			}
			kind := ExactPeerRequirement
			if class == specRange {
				kind = RangePeerRequirement
			}
			build.declarations = append(build.declarations, Requirement{
				Selector: selector, Version: specification, Package: pkg.ID.Name,
				PackageVersion: pkg.ID.Version, Integrity: pkg.Integrity, Peer: peer, Kind: kind,
			})
		}
		for _, snapshot := range project.snapshots {
			for _, edge := range snapshot.Edges {
				if edge.Name != peer {
					continue
				}
				pkg := archives[snapshot.ID.Package.Key()]
				if pkg == nil {
					return Derivation{}, errorf("%s: missing archive", snapshot.ID.Package.Key())
				}
				target, err := parseSnapshotID(edge.Target)
				if err != nil {
					return Derivation{}, err
				}
				build.declarations = append(build.declarations, Requirement{
					Selector: selector, Version: target.Package.Version.String(), Package: pkg.ID.Name,
					PackageVersion: pkg.ID.Version, Integrity: pkg.Integrity, Peer: peer, Kind: DependencyRequirement,
				})
			}
		}
		families = append(families, build)
	}
	for _, build := range families {
		if len(build.selections) == 0 {
			return Derivation{}, errorf("missing family: %s; requirements: %s", build.peer.String(), proof(build))
		}
		hasPeerRow := false
		for _, row := range build.declarations {
			if row.Kind != DependencyRequirement {
				hasPeerRow = true
			}
		}
		if !hasPeerRow {
			return Derivation{}, errorf("missing family peer requirements: %s", build.peer.String())
		}
	}
	// A declaration constrains the map only while its own package is still in the
	// graph: an override pins one version per family, so an instance of a family
	// at any other version is pruned together with its peer declarations. Each
	// family's domains are therefore solved together instead of one at a time.
	familyOf := map[PackageName]*family{}
	for position := range families {
		familyOf[families[position].peer] = &families[position]
	}
	constraints := map[PackageName][]constraintRow{}
	for _, build := range families {
		constraints[build.peer] = nil
	}
	for _, pkg := range project.packages {
		for _, declaration := range pkg.Peers.declarations {
			rows, tracked := constraints[declaration.Peer]
			if !tracked {
				continue
			}
			row := constraintRow{
				version: pkg.ID.Version,
				wanted:  declaration.Specification,
				source:  pkg.ID.Name.String() + "@" + pkg.ID.Version.String(),
			}
			if owner, ok := familyOf[pkg.ID.Name]; ok {
				peer := owner.peer
				row.owner = &peer
			}
			constraints[declaration.Peer] = append(rows, row)
		}
	}
	assignment := map[PackageName]ExactVersion{}
	domains := make([][]ExactVersion, 0, len(families))
	for _, build := range families {
		seen := map[ExactVersion]bool{}
		var versions []ExactVersion
		for _, version := range build.selections {
			if !seen[version] {
				seen[version] = true
				versions = append(versions, version)
			}
		}
		sortVersions(versions)
		domains = append(domains, versions)
	}
	var solutions []map[PackageName]ExactVersion
	var solve func(index int, current map[PackageName]ExactVersion) error
	solve = func(index int, current map[PackageName]ExactVersion) error {
		if index == len(families) {
			for _, build := range families {
				problems, err := familyProblems(constraints, current, build, current[build.peer])
				if err != nil {
					return err
				}
				if len(problems) != 0 {
					return nil
				}
			}
			snapshot := make(map[PackageName]ExactVersion, len(current))
			for peer, version := range current {
				snapshot[peer] = version
			}
			solutions = append(solutions, snapshot)
			return nil
		}
		for _, version := range domains[index] {
			next := make(map[PackageName]ExactVersion, len(current)+1)
			for peer, existing := range current {
				next[peer] = existing
			}
			next[families[index].peer] = version
			if err := solve(index+1, next); err != nil {
				return err
			}
		}
		return nil
	}
	if err := solve(0, assignment); err != nil {
		return Derivation{}, err
	}
	if len(solutions) == 0 {
		return Derivation{}, unsatisfiedReport(families, domains, constraints)
	}
	overrides := OverrideMap{entries: make([]OverrideEntry, 0, len(families))}
	derived := map[PackageName]ExactVersion{}
	for _, build := range families {
		candidates := candidateVersions(solutions, build.peer)
		if len(candidates) > 1 {
			// Two selections can satisfy every declaration (a root range selecting a
			// newer instance beside an exact peer's older one). A lockfile records no
			// dependency specifier, so the graph cannot choose between them: only a
			// reviewed value, while it is one of the survivors, can decide.
			approved, hasApproved := reviewed.Approved.Get(build.selector)
			keep, hasKeep := reviewed.Committed.Get(build.selector)
			if hasApproved {
				keep, hasKeep = approved, true
			}
			if !hasKeep || !containsVersion(candidates, keep) {
				why := ""
				if hasKeep {
					label := "committed"
					if hasApproved {
						label = "reviewed"
					}
					why = label + " value " + keep.String() + " is not one of the candidates; "
				}
				hint := "; pass --cordis-overrides after review"
				if hasApproved {
					hint = ""
				}
				return Derivation{}, errorf("ambiguous family selection: %s; candidates: %s; %srequirements: %s%s",
					build.peer.String(), joinVersions(candidates), why, proof(build), hint)
			}
			candidates = []ExactVersion{keep}
		}
		derived[build.peer] = candidates[0]
		overrides.entries = append(overrides.entries, OverrideEntry{Selector: build.selector, Version: candidates[0]})
	}
	conflicts := make([]string, 0)
	for _, build := range families {
		problems, err := familyProblems(constraints, derived, build, derived[build.peer])
		if err != nil {
			return Derivation{}, err
		}
		for _, problem := range problems {
			conflicts = append(conflicts, build.peer.String()+": "+problem)
		}
	}
	if len(conflicts) != 0 {
		return Derivation{}, errorf("family selection conflict: %s", strings.Join(conflicts, "; "))
	}
	unique := map[string]Requirement{}
	var order []string
	for _, build := range families {
		for _, row := range build.declarations {
			key := string(row.AppendJS(nil))
			if _, ok := unique[key]; !ok {
				order = append(order, key)
			}
			unique[key] = row
		}
	}
	for index := 1; index < len(order); index++ {
		for cursor := index; cursor > 0 && order[cursor] < order[cursor-1]; cursor-- {
			order[cursor], order[cursor-1] = order[cursor-1], order[cursor]
		}
	}
	requirements := make([]Requirement, 0, len(order))
	for _, key := range order {
		requirements = append(requirements, unique[key])
	}
	delta := make([]OverrideDelta, 0, len(overrides.entries))
	for _, item := range overrides.entries {
		delta = append(delta, OverrideDelta{Selector: item.Selector, After: item.Version})
	}
	return Derivation{Overrides: overrides, Requirements: requirements, Delta: delta}, nil
}

func proof(build family) string {
	items := make([]document.Value, 0, len(build.declarations))
	for _, row := range build.declarations {
		items = append(items, row.Value())
	}
	return string(document.NewSequence(items).AppendJS(nil))
}

func familyProblems(constraints map[PackageName][]constraintRow, assignment map[PackageName]ExactVersion, build family, value ExactVersion) ([]string, error) {
	var problems []string
	for _, row := range constraints[build.peer] {
		if row.owner != nil {
			assigned, ok := assignment[*row.owner]
			if !ok || assigned != row.version {
				continue
			}
		}
		ok, err := satisfiesFamilyRange(value, row.wanted)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !ok {
			problems = append(problems, row.source+" wants "+row.wanted)
		}
	}
	return problems, nil
}

type derivationRejection struct {
	text string
	met  bool
}

type derivationFamilyReport struct {
	build    family
	rejected []derivationRejection
}

func unsatisfiedReport(families []family, domains [][]ExactVersion, constraints map[PackageName][]constraintRow) error {
	reports := make([]derivationFamilyReport, 0, len(families))
	for position, build := range families {
		var rejected []derivationRejection
		for _, value := range domains[position] {
			unmet, err := familyProblems(constraints, map[PackageName]ExactVersion{}, build, value)
			if err != nil {
				return err
			}
			text := value.String() + " (" + strings.Join(unmet, "; ") + ")"
			if len(unmet) == 0 {
				text = value.String() + " (no declared requirement)"
			}
			rejected = append(rejected, derivationRejection{text: text, met: len(unmet) == 0})
		}
		reports = append(reports, derivationFamilyReport{build: build, rejected: rejected})
	}
	chosen := -1
	for position, report := range reports {
		allRejected := true
		for _, row := range report.rejected {
			if row.met {
				allRejected = false
			}
		}
		if allRejected {
			chosen = position
			break
		}
	}
	if chosen < 0 {
		// The most constrained family: the fewest candidates that meet something.
		order := make([]int, len(reports))
		for index := range order {
			order[index] = index
		}
		sort.SliceStable(order, func(a, b int) bool {
			return metCount(reports[order[a]]) < metCount(reports[order[b]])
		})
		chosen = order[0]
	}
	report := reports[chosen]
	texts := make([]string, 0, len(report.rejected))
	for _, row := range report.rejected {
		texts = append(texts, row.text)
	}
	return errorf("no family selection satisfies the declared requirements: %s; rejected: %s; requirements: %s",
		report.build.peer.String(), strings.Join(texts, ", "), proof(report.build))
}

func metCount(report derivationFamilyReport) int {
	count := 0
	for _, row := range report.rejected {
		if row.met {
			count++
		}
	}
	return count
}

func candidateVersions(solutions []map[PackageName]ExactVersion, peer PackageName) []ExactVersion {
	seen := map[ExactVersion]bool{}
	var versions []ExactVersion
	for _, solution := range solutions {
		version := solution[peer]
		if !seen[version] {
			seen[version] = true
			versions = append(versions, version)
		}
	}
	sortVersions(versions)
	return versions
}

func sortVersions(versions []ExactVersion) {
	for index := 1; index < len(versions); index++ {
		for cursor := index; cursor > 0 && versions[cursor].String() < versions[cursor-1].String(); cursor-- {
			versions[cursor], versions[cursor-1] = versions[cursor-1], versions[cursor]
		}
	}
}

func containsVersion(versions []ExactVersion, wanted ExactVersion) bool {
	for _, version := range versions {
		if version == wanted {
			return true
		}
	}
	return false
}

func joinVersions(versions []ExactVersion) string {
	parts := make([]string, 0, len(versions))
	for _, version := range versions {
		parts = append(parts, version.String())
	}
	return strings.Join(parts, ", ")
}

// ParseRequirement validates one proof row read back from a proposal file.
func ParseRequirement(value document.Value) (Requirement, error) {
	if _, err := recordOf(value); err != nil {
		return Requirement{}, err
	}
	selectorName, err := stringOf(value.Get("selector"))
	if err != nil {
		return Requirement{}, err
	}
	selector, ok := ParseSelector(selectorName)
	if !ok {
		return Requirement{}, errorf("unexpected key: %s", selectorName)
	}
	version, err := stringOf(value.Get("version"))
	if err != nil {
		return Requirement{}, err
	}
	packageName, err := packageNameOf(value.Get("package"))
	if err != nil {
		return Requirement{}, err
	}
	packageVersion, err := exactVersionOf(value.Get("packageVersion"))
	if err != nil {
		return Requirement{}, err
	}
	integrityText, err := stringOf(value.Get("integrity"))
	if err != nil {
		return Requirement{}, err
	}
	integrity, err := parseIntegrity(integrityText)
	if err != nil {
		return Requirement{}, errorf("%s: full sha512 required", packageName.String())
	}
	peer, err := packageNameOf(value.Get("peer"))
	if err != nil {
		return Requirement{}, err
	}
	kindText, err := stringOf(value.Get("kind"))
	if err != nil {
		return Requirement{}, err
	}
	var kind RequirementKind
	switch kindText {
	case "exact-peer":
		kind = ExactPeerRequirement
	case "range-peer":
		kind = RangePeerRequirement
	case "dependency":
		kind = DependencyRequirement
	default:
		return Requirement{}, errorf("unexpected key: %s", kindText)
	}
	return Requirement{
		Selector: selector, Version: version, Package: packageName,
		PackageVersion: packageVersion, Integrity: integrity, Peer: peer, Kind: kind,
	}, nil
}
