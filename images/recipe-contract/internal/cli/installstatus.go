package cli

import "github.com/gregwebs/agent-vm-images/images/recipe-contract/installstatus"

// runInstallStatus implements the record/path predicates. Arity is checked before
// the subcommand so that a misspelled subcommand with the wrong number of
// arguments keeps reporting usage.
func (p *process) runInstallStatus(args []string) int {
	if len(args) != 2 {
		return p.stderrStatus(2, installStatusUsage)
	}
	sub, arg := args[0], args[1]
	switch sub {
	case "path-state":
		switch state := installstatus.ClassifyPath(arg); state {
		case installstatus.Absent:
			return 0
		case installstatus.Present:
			return 1
		case installstatus.Indeterminate:
			return 2
		default:
			// No success fallback for an impossible state.
			return 2
		}
	case "code":
		record, err := installstatus.ReadRecord(arg)
		if err != nil {
			return 1
		}
		code, ok := record.Code()
		if !ok {
			return 1
		}
		return p.stdoutStatus(code.String() + "\n")
	case "record":
		record, err := installstatus.ReadRecord(arg)
		if err != nil {
			return 1
		}
		text := record.Text()
		return p.stdoutStatus(text + "\n")
	default:
		return p.stderrStatus(2, installStatusUnknown+sub+"\n")
	}
}
