package pnpmlock

import (
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// Graph is the reviewed graph projection: the only project data a caller may
// publish. Its JSON keeps the retired `graph(project)` shape and adds the typed
// hasBin/platform/optional fields every archive and snapshot now carries; the
// additive keys are emitted for every record so no consumer has to distinguish
// "absent" from a default.
type Graph struct {
	Selection Selection
	Packages  []Archive
	Snapshots []Snapshot
	Importer  []Edge
}

// Graph projects the validated project.
func (p *Project) Graph() Graph {
	return Graph{
		Selection: p.selection,
		Packages:  p.packages,
		Snapshots: p.snapshots,
		Importer:  p.importer,
	}
}

// AppendJS emits JSON.stringify(graph).
func (g Graph) AppendJS(dst []byte) []byte { return g.Value().AppendJS(dst) }

// Value renders the graph as a JavaScript-ordered value.
func (g Graph) Value() document.Value {
	packages := make([]document.Value, 0, len(g.Packages))
	for _, pkg := range g.Packages {
		packages = append(packages, pkg.Value())
	}
	snapshots := make([]document.Value, 0, len(g.Snapshots))
	for _, snapshot := range g.Snapshots {
		snapshots = append(snapshots, snapshot.Value())
	}
	importer := make([]document.Value, 0, len(g.Importer))
	for _, edge := range g.Importer {
		importer = append(importer, edge.Value())
	}
	return mappingValue(
		entry("selection", g.Selection.Value()),
		entry("packages", sequenceValue(packages)),
		entry("snapshots", sequenceValue(snapshots)),
		entry("importer", sequenceValue(importer)),
	)
}

// Value renders the selection.
func (s Selection) Value() document.Value {
	return mappingValue(
		entry("kind", jsStringValue(s.Kind.String())),
		entry("version", jsStringValue(s.Version.String())),
	)
}

// Value renders the archive record. `peers` is emitted in JavaScript property
// order because the retired map was an Object.create(null) with arbitrary names.
func (a Archive) Value() document.Value {
	declared := make([]document.JSString, 0, len(a.Peers.declarations))
	for _, declaration := range a.Peers.declarations {
		declared = append(declared, document.MustStringFromUTF8(declaration.Peer.String()))
	}
	peers := mappingValue()
	for _, name := range document.JSKeyOrder(declared) {
		specification, _ := a.Peers.Get(PackageName{value: name.Display()})
		peers = peers.With(name.Display(), jsStringValue(specification))
	}
	optional := make([]document.Value, 0, len(a.OptionalPeers))
	for _, name := range a.OptionalPeers {
		optional = append(optional, jsStringValue(name.String()))
	}
	return mappingValue(
		entry("id", a.ID.Value()),
		entry("integrity", jsStringValue(a.Integrity.String())),
		entry("tarball", jsStringValue(a.Tarball)),
		entry("peers", peers),
		entry("optionalPeers", sequenceValue(optional)),
		entry("hasBin", boolValue(a.HasBin)),
		entry("platform", a.Platform.Value()),
		entry("optional", boolValue(a.Optional)),
	)
}

// Value renders the platform selector triple. Every axis is emitted, so a
// consumer never has to distinguish "absent" from an empty list.
func (p PlatformSelectors) Value() document.Value {
	return mappingValue(
		entry("os", p.OS.Value()),
		entry("cpu", p.CPU.Value()),
		entry("libc", p.LibC.Value()),
	)
}

// Value renders one selector list in retained order.
func (l SelectorList) Value() document.Value {
	items := make([]document.Value, 0, len(l.selectors))
	for _, selector := range l.selectors {
		items = append(items, jsStringValue(selector.String()))
	}
	return sequenceValue(items)
}

// Value renders the package identity.
func (id PackageID) Value() document.Value {
	return mappingValue(
		entry("name", jsStringValue(id.Name.String())),
		entry("version", jsStringValue(id.Version.String())),
	)
}

// Value renders the snapshot record.
func (s Snapshot) Value() document.Value {
	edges := make([]document.Value, 0, len(s.Edges))
	for _, edge := range s.Edges {
		edges = append(edges, edge.Value())
	}
	return mappingValue(
		entry("id", s.ID.Value()),
		entry("edges", sequenceValue(edges)),
		entry("optional", boolValue(s.Optional)),
	)
}

// Value renders the snapshot identity.
func (s SnapshotID) Value() document.Value {
	context := make([]document.Value, 0, len(s.Context))
	for _, element := range s.Context {
		context = append(context, peerContextValue(element))
	}
	return mappingValue(
		entry("packageId", s.Package.Value()),
		entry("peerContext", sequenceValue(context)),
		entry("key", jsStringValue(s.Key)),
	)
}

func peerContextValue(element PeerContext) document.Value {
	switch typed := element.(type) {
	case OpaqueContext:
		return mappingValue(
			entry("opaque", boolValue(true)),
			entry("key", jsStringValue(typed.Key())),
		)
	case PeerContextID:
		context := make([]document.Value, 0, len(typed.Context))
		for _, nested := range typed.Context {
			context = append(context, peerContextValue(nested))
		}
		return mappingValue(
			entry("packageId", typed.Package.Value()),
			entry("peerContext", sequenceValue(context)),
			entry("key", jsStringValue(typed.Key)),
		)
	default:
		panic("pnpmlock: unhandled peer context")
	}
}

// Value renders the edge.
func (e Edge) Value() document.Value {
	return mappingValue(
		entry("name", jsStringValue(e.Name.String())),
		entry("target", jsStringValue(e.Target)),
		entry("optional", boolValue(e.Optional)),
	)
}
