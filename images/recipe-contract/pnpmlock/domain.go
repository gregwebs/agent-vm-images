// Package pnpmlock is the bounded pnpm graph domain: it reads a locked project
// under the reviewed policies, validates the graph and effective peers, writes
// the reviewed-input pins, and answers the ADR 0003 update comparison and the
// Cordis derivation. It never installs, resolves from a registry or contacts the
// network.
package pnpmlock

import (
	"fmt"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// Kind is the closed set of consumers whose policy the graph is checked against.
type Kind uint8

const (
	KindDsh Kind = iota
	KindPi
	KindBridge
)

// String is the wire name the CLI and the graph JSON use.
func (k Kind) String() string {
	switch k {
	case KindDsh:
		return "dsh"
	case KindPi:
		return "pi"
	case KindBridge:
		return "bridge"
	default:
		return fmt.Sprintf("Kind(%d)", k)
	}
}

// ParseKind reads the CLI's kind argument.
func ParseKind(text string) (Kind, bool) {
	switch text {
	case "dsh":
		return KindDsh, true
	case "pi":
		return KindPi, true
	case "bridge":
		return KindBridge, true
	default:
		return KindDsh, false
	}
}

// Root is the one dependency each consumer's manifest may declare.
func (k Kind) Root() PackageName {
	switch k {
	case KindDsh:
		return PackageName{value: "@deepseek-ai/dsh"}
	case KindPi:
		return PackageName{value: "@earendil-works/pi-coding-agent"}
	case KindBridge:
		return PackageName{value: "pi-claude-bridge"}
	default:
		panic("pnpmlock: unhandled kind")
	}
}

// Prefix is a caller-supplied project directory. It is deliberately not
// validated here: the filesystem policy and the caller's authorization decide
// whether it may be read.
type Prefix string

// Selection is the consumer and the exact version its manifest declares.
type Selection struct {
	Kind    Kind
	Version ExactVersion
}

// PackageID is a name and an exact version.
type PackageID struct {
	Name    PackageName
	Version ExactVersion
}

// Key is the `name@version` identity pnpm uses as an archive key.
func (id PackageID) Key() string { return id.Name.String() + "@" + id.Version.String() }

// PeerContext is a sealed peer-context element: either an opaque hash or a
// nested package identity. Sealing keeps a missing case from compiling.
type PeerContext interface{ peerContext() }

// OpaqueContext is pnpm's 32-hex peer hash.
type OpaqueContext struct{ key string }

func (OpaqueContext) peerContext() {}

// Key is the hash text.
func (c OpaqueContext) Key() string { return c.key }

// PeerContextID is a nested `name@version` peer context.
type PeerContextID struct {
	Package PackageID
	Context []PeerContext
	Key     string
}

func (PeerContextID) peerContext() {}

// SnapshotID is a snapshot's full identity.
type SnapshotID struct {
	Package PackageID
	Context []PeerContext
	Key     string
}

// Integrity is a canonical sha512 npm integrity string.
type Integrity struct{ value string }

// String is the integrity text.
func (i Integrity) String() string { return i.value }

// Edge is one dependency edge of a snapshot.
type Edge struct {
	Name     PackageName
	Target   string
	Optional bool
}

// PeerDeclaration is one `peerDependencies` entry.
type PeerDeclaration struct {
	Peer          PackageName
	Specification string
}

// PeerDeclarations is the peer map in name order. It is a slice rather than a
// map because the emitted JSON uses JavaScript property order.
type PeerDeclarations struct{ declarations []PeerDeclaration }

// Get returns the declared specification for a peer.
func (p PeerDeclarations) Get(peer PackageName) (string, bool) {
	for _, declaration := range p.declarations {
		if declaration.Peer == peer {
			return declaration.Specification, true
		}
	}
	return "", false
}

// Names returns the declared peers in sorted order.
func (p PeerDeclarations) Names() []PackageName {
	names := make([]PackageName, 0, len(p.declarations))
	for _, declaration := range p.declarations {
		names = append(names, declaration.Peer)
	}
	return names
}

// Empty reports whether the package declares no peers.
func (p PeerDeclarations) Empty() bool { return len(p.declarations) == 0 }

// Archive is one `packages` record.
type Archive struct {
	ID            PackageID
	Integrity     Integrity
	Tarball       string
	Peers         PeerDeclarations
	OptionalPeers []PackageName
}

// Snapshot is one `snapshots` record.
type Snapshot struct {
	ID    SnapshotID
	Edges []Edge
}

// Project is a validated locked project. It is only built by the readers, so a
// caller cannot forge one (the retired in-process assertions are now types).
type Project struct {
	prefix    Prefix
	selection Selection
	mode      readMode
	manifest  document.Value
	workspace document.Value
	lock      document.Value
	packages  []Archive
	snapshots []Snapshot
	importer  []Edge
}

// DshProject is a locked dsh project; peer-sensitive operations only accept this
// narrowed form.
type DshProject struct{ project *Project }

// Selection reports the validated consumer selection.
func (p *Project) Selection() Selection { return p.selection }

// Overrides reports the committed Cordis override map.
func (p *DshProject) Overrides() (OverrideMap, error) {
	return ParseOverrideMap(p.project.workspace.Get("overrides"))
}

func errorf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// Manifest exposes the validated manifest document for the developer staging
// path, which rewrites exactly one dependency entry.
func (p *DshProject) Manifest() document.Value { return p.project.manifest }

// Selection reports the validated selection of a locked dsh project.
func (p *DshProject) Selection() Selection { return p.project.selection }
