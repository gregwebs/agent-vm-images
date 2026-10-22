package cli

import (
	"errors"
	"fmt"
	"strings"
	"syscall"

	"github.com/gregwebs/agent-vm-images/images/recipe-contract/dshupgrade"
	"github.com/gregwebs/agent-vm-images/images/recipe-contract/pnpmlock"
)

func errorOf(text string) error { return errors.New(text) }

// runDshLockUpdate is the argv port of check-lock-update.js with `--json` added.
func (p *process) runDshLockUpdate(args []string) int {
	if len(args) < 3 {
		return p.nodeUsage(2, dshLockUpdateUsage)
	}
	refresh, jsonOut := false, false
	seen := map[string]bool{}
	for _, option := range args[3:] {
		if seen[option] {
			return p.nodeUsage(2, dshLockUpdateUsage)
		}
		seen[option] = true
		switch option {
		case "--refresh-lock":
			refresh = true
		case "--json":
			jsonOut = true
		default:
			return p.nodeUsage(2, dshLockUpdateUsage)
		}
	}
	return p.nodeCommand(dshLockUpdatePrefix, func() (string, error) {
		requested, err := pnpmlock.ParseRequestedSelection(args[2])
		if err != nil {
			return "", err
		}
		mode := pnpmlock.NoRefresh
		if refresh {
			mode = pnpmlock.RefreshLock
		}
		comparison, after, err := pnpmlock.CompareDshUpdate(pnpmlock.UpdateRequest{
			Before:    pnpmlock.Prefix(args[0]),
			After:     pnpmlock.Prefix(args[1]),
			Requested: requested,
			Refresh:   mode,
		})
		if err != nil {
			return "", err
		}
		if jsonOut {
			return string(comparison.AppendJS(nil)) + "\n", nil
		}
		exact, err := after.ExactPeers()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("dsh lock update: %s %s -> %s; %d exact peers satisfied\n",
			comparison.Mode, comparison.BeforeVersion, comparison.AfterVersion, exact), nil
	})
}

// runDshCordisOverrides is the argv port of derive-cordis-overrides.mjs.
func (p *process) runDshCordisOverrides(args []string) int {
	prefix := ""
	if len(args) > 0 {
		prefix = args[0]
	}
	options := args[min(len(args), 1):]
	flags := map[string]string{}
	usage := prefix == ""
	for index := 0; index < len(options); index += 2 {
		flag := options[index]
		value := ""
		if index+1 < len(options) {
			value = options[index+1]
		}
		if !knownDerivationFlag(flag) || hasFlag(flags, flag) || value == "" || strings.HasPrefix(value, "--") {
			usage = true
		}
		flags[flag] = value
	}
	if hasFlag(flags, "--propose") && hasFlag(flags, "--cordis-overrides") {
		usage = true
	}
	if usage {
		return p.nodeUsage(2, dshDerivationUsage)
	}
	return p.nodeCommand(dshDerivationPrefix, func() (string, error) {
		cwd, err := syscall.Getwd()
		if err != nil {
			return "", err
		}
		request := dshupgrade.DeriveRequest{
			Tentative: pnpmlock.Prefix(prefix),
			Approved:  flags["--cordis-overrides"],
			Propose:   flags["--propose"],
			Cwd:       cwd,
		}
		if before, ok := flags["--before"]; ok {
			prefix := pnpmlock.Prefix(before)
			request.Before = &prefix
		}
		result, err := dshupgrade.Derive(request)
		if err != nil {
			return "", err
		}
		return string(result.AppendJS(nil)) + "\n", nil
	})
}

func knownDerivationFlag(flag string) bool {
	switch flag {
	case "--before", "--propose", "--cordis-overrides":
		return true
	default:
		return false
	}
}

func hasFlag(flags map[string]string, flag string) bool {
	_, ok := flags[flag]
	return ok
}

// runDshUpgradeStage is the argv port of upgrade-project.mjs. It has no usage
// path: every failure is status 1 with the `dsh upgrade: ` prefix.
func (p *process) runDshUpgradeStage(args []string) int {
	positional := func(index int) string {
		if index < len(args) {
			return args[index]
		}
		return ""
	}
	action, before, stage, argument, extra := positional(0), positional(1), positional(2), positional(3), positional(4)
	return p.nodeCommand(dshUpgradePrefix, func() (string, error) {
		// The retired adapter read the committed project before dispatching, so a
		// broken prefix wins over an unknown action.
		if before == "" {
			return "", errorOf("missing argument: before")
		}
		if stage == "" {
			return "", errorOf("missing argument: stage")
		}
		if argument == "" {
			return "", errorOf("missing argument: argument")
		}
		if _, err := pnpmlock.ReadDshProject(pnpmlock.Prefix(before)); err != nil {
			return "", err
		}
		request := dshupgrade.StageRequest{
			Before:   pnpmlock.Prefix(before),
			Stage:    stage,
			Argument: argument,
			Extra:    extra,
			ExtraSet: extra != "",
		}
		switch action {
		case "tentative":
			if request.ExtraSet {
				return "", errorOf("expected target exact selection")
			}
			return "", dshupgrade.StageTentative(request)
		case "final":
			return "", dshupgrade.StageFinal(request)
		case "check-final":
			report, err := dshupgrade.CheckFinal(request)
			if err != nil {
				return "", err
			}
			return string(report.AppendJS(nil)) + "\n", nil
		default:
			return "", errorOf("unknown developer staging action")
		}
	})
}
