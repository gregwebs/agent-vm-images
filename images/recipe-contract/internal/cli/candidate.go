package cli

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/candidatestage"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

// candidateVerb is the closed candidate command set. The adoption verb has no
// predecessor and no selection update; it is not a missing-pin fallback.
type candidateVerb uint8

const (
	candidateGraph candidateVerb = iota
	candidateAdoptionGraph
)

// runPnpmLockCandidate is the private, capability-confined candidate command.
// The scratch descriptor is the authority: this command never mints one from a
// path, never accepts a prefix positional, and never unpins an ordinary
// `pnpm-lock` call.
func (p *process) runPnpmLockCandidate(args []string) int {
	verb, ok := parseCandidateVerb(first(args, 0))
	if !ok {
		return p.nodeUsage(2, pnpmLockCandidateUsage)
	}
	kind, knownKind := pnpmlock.ParseKind(first(args, 1))
	if !knownKind {
		return p.nodeUsage(2, pnpmLockCandidateUsage)
	}
	flags, ok := parseCandidateFlags(args[min(len(args), 2):])
	if !ok || flags.scratch == "" || flags.scratchFD == "" {
		return p.nodeUsage(2, pnpmLockCandidateUsage)
	}
	switch verb {
	case candidateGraph:
		if flags.before == "" || flags.requestedVersion == "" {
			return p.nodeUsage(2, pnpmLockCandidateUsage)
		}
	case candidateAdoptionGraph:
		if flags.before != "" || flags.requestedVersion != "" || flags.refresh || flags.shrinkwrapAuthority {
			return p.nodeUsage(2, pnpmLockCandidateUsage)
		}
	}
	// The shrinkwrap authority is the authenticated Pi selected-archive route's
	// statement that the archive's own shrinkwrap, compared separately, decides the
	// graph. No other kind publishes such an archive, so naming it for them is not a
	// policy relaxation a caller may reach for: it would trade the retained-edge
	// freeze (a bridge edge moving from mandatory to optional, for instance) for
	// nothing. The typed comparator refuses it too.
	if flags.shrinkwrapAuthority && kind != pnpmlock.KindPi {
		return p.nodeUsage(2, pnpmLockCandidateUsage)
	}
	return p.nodeCommand(pnpmLockCandidatePrefix, func() (string, error) {
		descriptor, _ := strconv.Atoi(flags.scratchFD)
		stage, err := candidatestage.Open(flags.scratch, descriptor)
		if err != nil {
			return "", err
		}
		candidate, err := pnpmlock.ValidateCandidate(stage, kind)
		if err != nil {
			return "", err
		}
		switch verb {
		case candidateAdoptionGraph:
			if err := stage.Recheck(); err != nil {
				return "", err
			}
			return string(pnpmlock.NewCandidateReport(candidate, nil).AppendJS(nil)) + "\n", nil
		case candidateGraph:
			if overlap(stage.Scratch(), flags.before) {
				return "", errorOf("reviewed prefix and candidate stage overlap")
			}
			// The predecessor is ordinary reviewed input: full pins and the
			// forbidden-file checks apply to it, never to the candidate.
			before, err := pnpmlock.ReadProject(pnpmlock.Prefix(flags.before), kind)
			if err != nil {
				return "", err
			}
			requested, err := pnpmlock.ParseRequestedSelection(flags.requestedVersion)
			if err != nil {
				return "", err
			}
			refresh := pnpmlock.NoRefresh
			if flags.refresh {
				refresh = pnpmlock.RefreshLock
			}
			// The authority is typed, never a caller-supplied policy string: the
			// driver that authenticated the selected archive names it on argv, and
			// the zero value keeps every other caller on the retained freeze.
			authority := pnpmlock.RetainedSnapshotFreeze
			if flags.shrinkwrapAuthority {
				authority = pnpmlock.ShrinkwrapAuthority
			}
			delta, err := pnpmlock.CompareSelectionCandidate(before, candidate,
				pnpmlock.SelectionRequest{Requested: requested, Refresh: refresh, Authority: authority})
			if err != nil {
				return "", err
			}
			// The compared digests are rechecked before the report can be used for
			// pinning: a candidate edited mid-validation is not published.
			if err := stage.Recheck(); err != nil {
				return "", err
			}
			return string(pnpmlock.NewCandidateReport(candidate, &delta).AppendJS(nil)) + "\n", nil
		default:
			panic("unhandled candidate verb")
		}
	})
}

// runPnpmSelectionUpdate is the post-pin comparator for any consumer kind. It
// shares its rules with the private candidate comparison.
func (p *process) runPnpmSelectionUpdate(args []string) int {
	positional := make([]string, 0, 4)
	index := 0
	for index < len(args) && !strings.HasPrefix(args[index], "--") {
		positional = append(positional, args[index])
		index++
	}
	if len(positional) < 3 || len(positional) > 4 {
		return p.nodeUsage(2, pnpmSelectionUpdateUsage)
	}
	kind, ok := pnpmlock.ParseKind(positional[0])
	if !ok {
		return p.nodeUsage(2, pnpmSelectionUpdateUsage)
	}
	refresh, jsonOut, ok := parseSelectionFlags(args[index:])
	if !ok {
		return p.nodeUsage(2, pnpmSelectionUpdateUsage)
	}
	argument := ""
	if len(positional) == 4 {
		argument = positional[3]
	}
	return p.nodeCommand(pnpmSelectionUpdatePrefix, func() (string, error) {
		refreshMode := pnpmlock.NoRefresh
		if refresh {
			refreshMode = pnpmlock.RefreshLock
		}
		comparison, _, err := pnpmlock.CompareSelectionUpdateArgument(pnpmlock.UpdateRequest{
			Kind:    kind,
			Before:  pnpmlock.Prefix(positional[1]),
			After:   pnpmlock.Prefix(positional[2]),
			Refresh: refreshMode,
		}, argument)
		if err != nil {
			return "", err
		}
		if jsonOut {
			return string(comparison.AppendJS(nil)) + "\n", nil
		}
		return "pnpm selection update: " + comparison.Mode.String() + " " +
			comparison.BeforeVersion.String() + " -> " + comparison.AfterVersion.String() + "\n", nil
	})
}

func parseSelectionFlags(args []string) (refresh, jsonOut, ok bool) {
	seen := map[string]bool{}
	for _, option := range args {
		if seen[option] {
			return false, false, false
		}
		seen[option] = true
		switch option {
		case "--refresh-lock":
			refresh = true
		case "--json":
			jsonOut = true
		default:
			return false, false, false
		}
	}
	return refresh, jsonOut, true
}

type candidateFlagValues struct {
	scratch             string
	scratchFD           string
	before              string
	requestedVersion    string
	refresh             bool
	shrinkwrapAuthority bool
}

func parseCandidateFlags(args []string) (candidateFlagValues, bool) {
	values := candidateFlagValues{}
	seen := map[string]bool{}
	for index := 0; index < len(args); index++ {
		flag := args[index]
		if seen[flag] {
			return candidateFlagValues{}, false
		}
		seen[flag] = true
		if flag == "--refresh-lock" {
			values.refresh = true
			continue
		}
		if flag == "--shrinkwrap-authority" {
			values.shrinkwrapAuthority = true
			continue
		}
		if index+1 >= len(args) {
			return candidateFlagValues{}, false
		}
		index++
		value := args[index]
		if value == "" || strings.HasPrefix(value, "--") {
			return candidateFlagValues{}, false
		}
		switch flag {
		case "--scratch":
			values.scratch = value
		case "--scratch-fd":
			if _, err := strconv.Atoi(value); err != nil {
				return candidateFlagValues{}, false
			}
			values.scratchFD = value
		case "--before":
			values.before = value
		case "--requested-version":
			values.requestedVersion = value
		default:
			return candidateFlagValues{}, false
		}
	}
	return values, true
}

func parseCandidateVerb(text string) (candidateVerb, bool) {
	switch text {
	case "graph":
		return candidateGraph, true
	case "adoption-graph":
		return candidateAdoptionGraph, true
	default:
		return candidateGraph, false
	}
}

func first(args []string, index int) string {
	if index < len(args) {
		return args[index]
	}
	return ""
}

// overlap reports whether two caller-supplied paths name one directory or one
// contains the other. It is lexical: the driver owns canonical paths, and this
// only refuses the obvious self-reference before a read.
func overlap(a, b string) bool {
	left, err := filepath.Abs(a)
	if err != nil {
		return true
	}
	right, err := filepath.Abs(b)
	if err != nil {
		return true
	}
	if left == right {
		return true
	}
	return strings.HasPrefix(left, right+string(filepath.Separator)) ||
		strings.HasPrefix(right, left+string(filepath.Separator))
}
