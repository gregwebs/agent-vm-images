package pnpmlock

import (
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// PeerReport is the non-throwing peer result so a caller can state both numbers
// and decide for itself; each entry names the declaration it could not satisfy.
type PeerReport struct {
	Exact                int
	TotalOptionalMissing int
	CordisExact          int
	Unsatisfied          []string
}

// ExactPeerSummary is the asserting form the install paths publish.
type ExactPeerSummary struct {
	Exact                int
	TotalOptionalMissing int
	CordisExact          int
	Unsatisfied          int
}

// Value renders the report as JSON in the retired key order.
func (r PeerReport) Value() document.Value {
	unsatisfied := make([]document.Value, 0, len(r.Unsatisfied))
	for _, item := range r.Unsatisfied {
		unsatisfied = append(unsatisfied, jsStringValue(item))
	}
	return mappingValue(
		entry("exact", document.NewNumber(float64(r.Exact))),
		entry("totalOptionalMissing", document.NewNumber(float64(r.TotalOptionalMissing))),
		entry("cordisExact", document.NewNumber(float64(r.CordisExact))),
		entry("unsatisfied", sequenceValue(unsatisfied)),
	)
}

// AppendJS emits JSON.stringify(report).
func (r PeerReport) AppendJS(dst []byte) []byte { return r.Value().AppendJS(dst) }

// Value renders the summary.
func (s ExactPeerSummary) Value() document.Value {
	return mappingValue(
		entry("exact", document.NewNumber(float64(s.Exact))),
		entry("totalOptionalMissing", document.NewNumber(float64(s.TotalOptionalMissing))),
		entry("cordisExact", document.NewNumber(float64(s.CordisExact))),
		entry("unsatisfied", document.NewNumber(float64(s.Unsatisfied))),
	)
}

// AppendJS emits JSON.stringify(summary).
func (s ExactPeerSummary) AppendJS(dst []byte) []byte { return s.Value().AppendJS(dst) }

// ReportDshPeers is the report-only reader: it reuses the peer-assertion-free
// read so a negative leg can state both numbers instead of guessing from a
// diagnostic. Nothing is pinned, installed or published.
func ReportDshPeers(prefix Prefix) (PeerReport, error) {
	project, err := readProjectInputs(prefix, KindDsh, skipPins, readReport)
	if err != nil {
		return PeerReport{}, err
	}
	return exactPeerReport(project)
}

// ExactPeers is checkExactPeers(after).exact for a validated dsh project.
func (p *DshProject) ExactPeers() (int, error) {
	summary, err := checkExactPeers(p.project)
	if err != nil {
		return 0, err
	}
	return summary.Exact, nil
}

// exactPeerReport walks every snapshot's peer declarations and records each one
// it cannot satisfy, without stopping.
func exactPeerReport(project *Project) (PeerReport, error) {
	archives := map[string]*Archive{}
	for position := range project.packages {
		pkg := &project.packages[position]
		archives[pkg.ID.Key()] = pkg
	}
	index := indexSnapshots(project.snapshots)
	report := PeerReport{Unsatisfied: []string{}}
	for _, snapshot := range project.snapshots {
		pkg, ok := archives[snapshot.ID.Package.Key()]
		if !ok {
			return PeerReport{}, errorf("%s: missing archive", snapshot.ID.Key)
		}
		for _, declaration := range pkg.Peers.declarations {
			peer, wanted := declaration.Peer, declaration.Specification
			pinned, exact := exactDeclaration(wanted)
			// A declared range is a compatibility constraint, not an exact pin; any
			// syntax outside the range grammar could hide an exact pin, so it stops.
			if !exact {
				class, err := peerSpecClass(wanted)
				if err != nil {
					return PeerReport{}, err
				}
				if class == specRange {
					continue
				}
			}
			report.Exact++
			if peer.String() == "@deepseek-ai/cordis" {
				report.CordisExact++
			}
			var edge *Edge
			for position := range snapshot.Edges {
				if snapshot.Edges[position].Name == peer {
					edge = &snapshot.Edges[position]
					break
				}
			}
			var context PeerContext
			for _, element := range snapshot.ID.Context {
				named, ok := element.(PeerContextID)
				if ok && named.Package.Name == peer {
					context = element
					break
				}
			}
			var got *PackageID
			var gotKey string
			if edge != nil {
				target, err := parseSnapshotID(edge.Target)
				if err != nil {
					return PeerReport{}, err
				}
				got = &target.Package
				gotKey = target.Package.Key()
			}
			diagnostic := snapshot.ID.Key + ": peer " + peer.String() + " wanted " + wanted + ", got " + gotDisplay(gotKey, got)
			if got == nil {
				if containsPackage(pkg.OptionalPeers, peer) && context == nil {
					report.TotalOptionalMissing++
				} else {
					report.Unsatisfied = append(report.Unsatisfied, diagnostic)
				}
				continue
			}
			if got.Name != peer || got.Version.String() != pinned {
				report.Unsatisfied = append(report.Unsatisfied, diagnostic)
				continue
			}
			if context != nil {
				provider, err := index.resolve(context, snapshot.ID.Key)
				if err != nil {
					return PeerReport{}, err
				}
				if provider.ID.Key != edge.Target {
					report.Unsatisfied = append(report.Unsatisfied, diagnostic+"; ambiguous context")
				}
			}
		}
	}
	return report, nil
}

func gotDisplay(key string, got *PackageID) string {
	if got == nil {
		return "missing"
	}
	return key
}

func containsPackage(names []PackageName, name PackageName) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// checkExactPeers is the asserting form used by every publication path.
func checkExactPeers(project *Project) (ExactPeerSummary, error) {
	report, err := exactPeerReport(project)
	if err != nil {
		return ExactPeerSummary{}, err
	}
	if len(report.Unsatisfied) != 0 {
		return ExactPeerSummary{}, errorf("%s", strings.Join(report.Unsatisfied, "; "))
	}
	if report.CordisExact == 0 {
		return ExactPeerSummary{}, errorf("dsh must cover declared exact Cordis peers")
	}
	return ExactPeerSummary{
		Exact:                report.Exact,
		TotalOptionalMissing: report.TotalOptionalMissing,
		CordisExact:          report.CordisExact,
	}, nil
}
