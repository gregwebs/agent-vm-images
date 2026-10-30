package pnpmlock

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/candidatestage"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
)

// InputDigest is one candidate input's SHA-256 over the captured bytes that were
// parsed.
type InputDigest struct {
	Name   string
	Digest string
}

// InputDigests is the candidate's three input digests in pin-file order.
type InputDigests struct{ entries []InputDigest }

// Entries returns the digests in PinnedInputs order.
func (d InputDigests) Entries() []InputDigest { return d.entries }

// Get returns one input digest.
func (d InputDigests) Get(name string) (string, bool) {
	for _, entry := range d.entries {
		if entry.Name == name {
			return entry.Digest, true
		}
	}
	return "", false
}

// Value renders the digest map in pin-file order.
func (d InputDigests) Value() document.Value {
	entries := make([]mappingEntry, 0, len(d.entries))
	for _, digest := range d.entries {
		entries = append(entries, entry(digest.Name, jsStringValue(digest.Digest)))
	}
	return mappingValue(entries...)
}

// ValidatedCandidate is a fully policy-validated, unpinned three-input project.
// It deliberately has no Project, pins, status, install or publication method:
// the only ways to consume it are the graph projection and the typed selection
// comparison, so a candidate can never be mistaken for reviewed input.
type ValidatedCandidate struct {
	kind       Kind
	graph      Graph
	digests    InputDigests
	manifest   document.Value
	workspace  []byte
	lock       document.Value
	inputBytes map[string][]byte
}

// Kind is the consumer policy the candidate was validated against.
func (c *ValidatedCandidate) Kind() Kind { return c.kind }

// Graph is the typed graph projection.
func (c *ValidatedCandidate) Graph() Graph { return c.graph }

// InputDigests are the captured digests the caller must recheck before pinning.
func (c *ValidatedCandidate) InputDigests() InputDigests { return c.digests }

// ValidateCandidate captures and validates a private candidate stage. The full
// consumer workspace, settings, graph and exact-peer policy applies; only the
// reviewed-pin verification is inapplicable to a three-file candidate.
func ValidateCandidate(stage candidatestage.Stage, kind Kind) (*ValidatedCandidate, error) {
	buffers := map[string][]byte{}
	for _, file := range stage.Files() {
		buffers[file.Name] = file.Bytes
	}
	project, err := parseProjectInputs(stageAcquirer(buffers), "", kind, readLocked)
	if err != nil {
		return nil, err
	}
	digests := make([]InputDigest, 0, len(PinnedInputs))
	for _, name := range PinnedInputs {
		source, ok := buffers[name]
		if !ok {
			return nil, errorf("candidate stage did not capture %s", name)
		}
		sum := sha256.Sum256(source)
		digests = append(digests, InputDigest{Name: name, Digest: hex.EncodeToString(sum[:])})
	}
	return &ValidatedCandidate{
		kind:       kind,
		graph:      project.Graph(),
		digests:    InputDigests{entries: digests},
		manifest:   project.manifest,
		workspace:  buffers["pnpm-workspace.yaml"],
		lock:       project.lock,
		inputBytes: buffers,
	}, nil
}

// stageAcquirer only ever answers from the captured candidate buffers; a name
// the stage did not capture is a stop, never a filesystem read.
func stageAcquirer(buffers map[string][]byte) inputAcquirer {
	return func(name string, format document.Format) (documentSource, error) {
		source, ok := buffers[name]
		if !ok {
			return documentSource{}, errorf("candidate stage did not capture %s", name)
		}
		value, err := document.Parse(source, format)
		if err != nil {
			return documentSource{}, err
		}
		return documentSource{source: source, value: value}, nil
	}
}

func (c *ValidatedCandidate) inputBytesFor(name string) ([]byte, error) {
	source, ok := c.inputBytes[name]
	if !ok {
		return nil, errorf("candidate stage did not capture %s", name)
	}
	return source, nil
}

// CandidateReport is the one-line candidate JSON. A nil delta is the
// initial-adoption variant: it has no predecessor comparison, so it omits
// selectionDelta rather than emitting a false "equal" result.
type CandidateReport struct {
	candidate *ValidatedCandidate
	delta     *UpdateComparison
}

// NewCandidateReport pairs a validated candidate with an accepted selection
// delta, or nil for the adoption verb.
func NewCandidateReport(candidate *ValidatedCandidate, delta *UpdateComparison) CandidateReport {
	return CandidateReport{candidate: candidate, delta: delta}
}

// Value renders the report: inputState, inputDigests, optional selectionDelta,
// graph.
func (r CandidateReport) Value() document.Value {
	graph := r.candidate.Graph()
	outer := []mappingEntry{
		entry("inputState", jsStringValue("candidate")),
		entry("inputDigests", r.candidate.InputDigests().Value()),
	}
	if r.delta != nil {
		outer = append(outer, entry("selectionDelta", r.delta.Value()))
	}
	outer = append(outer, entry("graph", graph.Value()))
	return mappingValue(outer...)
}

// AppendJS emits JSON.stringify(report).
func (r CandidateReport) AppendJS(dst []byte) []byte { return r.Value().AppendJS(dst) }

// CompareSelectionCandidate applies the same manifest/workspace/manager,
// retained-SRI and closure rules as a post-pin selection update. It is the only
// comparison a candidate may enter, and it never returns a Project.
//
// The one thing the request may change is the snapshot authority: the
// authenticated selected-shrinkwrap route names ShrinkwrapAuthority because it
// validates the imported graph against the archive's own shrinkwrap, and this
// comparator must not substitute the previous selection's frozen edges for it.
func CompareSelectionCandidate(before *Project, after *ValidatedCandidate, request SelectionRequest) (UpdateComparison, error) {
	if before.selection.Kind != after.kind {
		return UpdateComparison{}, errorf("candidate kind differs from the reviewed project")
	}
	return compareSelection(after.kind, projectSide(before), candidateSide(after), request.Requested,
		request.Refresh, request.Authority)
}
