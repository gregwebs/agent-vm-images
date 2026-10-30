package cli

import (
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmenv"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

// runPnpmLock is the argv port of the retired pnpm-lock helper:
// `verb kind PREFIX`.
func (p *process) runPnpmLock(args []string) int {
	verbArg := ""
	if len(args) > 0 {
		verbArg = args[0]
	}
	kindArg := ""
	if len(args) > 1 {
		kindArg = args[1]
	}
	prefix := ""
	if len(args) > 2 {
		prefix = args[2]
	}
	extra := args[min(len(args), 3):]
	verb, knownVerb := parseLockVerb(verbArg)
	kind, knownKind := pnpmlock.ParseKind(kindArg)
	if !knownVerb || !knownKind || prefix == "" || len(extra) != 0 {
		return p.nodeUsage(2, pnpmLockUsage)
	}
	return p.nodeCommand(pnpmLockPrefix, func() (string, error) {
		switch verb {
		case lockPin:
			return "", pnpmlock.WritePins(pnpmlock.Prefix(prefix), kind)
		case lockPeers:
			if kind != pnpmlock.KindDsh {
				return "", errorOf("peer reporting is dsh-only")
			}
			report, err := pnpmlock.ReportDshPeers(pnpmlock.Prefix(prefix))
			if err != nil {
				return "", err
			}
			return string(report.AppendJS(nil)) + "\n", nil
		case lockCheck, lockGraph:
			project, err := pnpmlock.ReadProject(pnpmlock.Prefix(prefix), kind)
			if err != nil {
				return "", err
			}
			if verb == lockCheck {
				return project.Selection().Version.String() + "\n", nil
			}
			graph := project.Graph()
			return string(graph.AppendJS(nil)) + "\n", nil
		}
		panic("unhandled lock verb")
	})
}

type lockVerb uint8

const (
	lockCheck lockVerb = iota
	lockGraph
	lockPin
	lockPeers
)

func parseLockVerb(text string) (lockVerb, bool) {
	switch text {
	case "check":
		return lockCheck, true
	case "graph":
		return lockGraph, true
	case "pin":
		return lockPin, true
	case "peers":
		return lockPeers, true
	default:
		return lockCheck, false
	}
}

// runPnpmEnvironment validates the process environment and every PREFIX.
func (p *process) runPnpmEnvironment(args []string) int {
	return p.nodeCommand(pnpmEnvironmentPrefix, func() (string, error) {
		prefixes := make([]pnpmlock.Prefix, 0, len(args))
		for _, arg := range args {
			prefixes = append(prefixes, pnpmlock.Prefix(arg))
		}
		return "", pnpmenv.Check(p.env.Variables, prefixes)
	})
}

// runPnpmYAML is the syntax adapter the tests and fixtures use instead of a YAML
// import: it prints the bounded parser's value as JSON and validates nothing.
func (p *process) runPnpmYAML(args []string) int {
	return p.syntaxAdapter(args, document.YAML, pnpmYAMLUsage, pnpmYAMLPrefix)
}

// runPnpmJSON is the strict JSON sibling of pnpm-yaml. It exists so an
// authenticated manifest or shrinkwrap is read through the same duplicate-key,
// depth, byte-bound and UTF-8 rules as the lock, instead of a permissive
// JSON.parse in a build helper.
func (p *process) runPnpmJSON(args []string) int {
	return p.syntaxAdapter(args, document.JSON, pnpmJSONUsage, pnpmJSONPrefix)
}

func (p *process) syntaxAdapter(args []string, format document.Format, usage, prefix string) int {
	if len(args) != 1 || args[0] == "" {
		return p.nodeUsage(2, usage)
	}
	return p.nodeCommand(prefix, func() (string, error) {
		value, err := document.ReadFile(args[0], format)
		if err != nil {
			return "", err
		}
		return string(value.AppendJS(nil)) + "\n", nil
	})
}
