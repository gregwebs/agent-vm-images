package cli

import (
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/internal/document"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmenv"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

// runPnpmLock is the argv port of pnpm-lock.mjs: `verb kind PREFIX`.
func (p *process) runPnpmLock(args []string) int {
	verb := ""
	if len(args) > 0 {
		verb = args[0]
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
	kind, knownKind := pnpmlock.ParseKind(kindArg)
	if !knownVerb(verb) || !knownKind || prefix == "" || len(extra) != 0 {
		return p.nodeUsage(2, pnpmLockUsage)
	}
	return p.nodeCommand(pnpmLockPrefix, func() (string, error) {
		switch verb {
		case "pin":
			return "", pnpmlock.WritePins(pnpmlock.Prefix(prefix), kind)
		case "peers":
			if kind != pnpmlock.KindDsh {
				return "", errorOf("peer reporting is dsh-only")
			}
			report, err := pnpmlock.ReportDshPeers(pnpmlock.Prefix(prefix))
			if err != nil {
				return "", err
			}
			return string(report.AppendJS(nil)) + "\n", nil
		default:
			project, err := pnpmlock.ReadProject(pnpmlock.Prefix(prefix), kind)
			if err != nil {
				return "", err
			}
			if verb == "check" {
				return project.Selection().Version.String() + "\n", nil
			}
			graph := project.Graph()
			return string(graph.AppendJS(nil)) + "\n", nil
		}
	})
}

func knownVerb(verb string) bool {
	switch verb {
	case "check", "graph", "pin", "peers":
		return true
	default:
		return false
	}
}

// runPnpmEnvironment validates the process environment and every PREFIX.
func (p *process) runPnpmEnvironment(args []string) int {
	return p.nodeCommand(pnpmEnvironmentPrefix, func() (string, error) {
		return "", pnpmenv.Check(p.env.Variables, args)
	})
}

// runPnpmYAML is the syntax adapter the tests and fixtures use instead of a YAML
// import: it prints the bounded parser's value as JSON and validates nothing.
func (p *process) runPnpmYAML(args []string) int {
	if len(args) != 1 || args[0] == "" {
		return p.nodeUsage(2, pnpmYAMLUsage)
	}
	return p.nodeCommand(pnpmYAMLPrefix, func() (string, error) {
		value, err := document.ReadFile(args[0], document.YAML)
		if err != nil {
			return "", err
		}
		return string(value.AppendJS(nil)) + "\n", nil
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
