package cli

import "github.com/alexflint/go-arg"

// argParser builds a parser whose refusal belongs to this process. go-arg's Fail
// writes to Config.Out and calls Config.Exit, so both are handed the caller's stream
// and a recorded status instead of os.Stdout and os.Exit: a missing required flag is
// then a status argFailure returns, and its bytes land on stderr where the
// predecessors wrote every usage refusal.
func (p *process) argParser(program string, target any) (*arg.Parser, error) {
	return arg.NewParser(arg.Config{Program: program, Out: p.streams.Err, Exit: func(int) {}}, target)
}

// requiredPath is one path flag that must also carry a value.
//
// go-arg's `required` records only that the flag appeared, so `--source ""` parses.
// The driver would then compose a path from nothing: an empty `--source` runs
// `project.py` from the caller's working directory, and an empty `--contract` runs
// `run-pnpm.sh` from it. That is the seam the entry point's `--` closes from one side
// and this check closes from the other, before any driver sees the options.
type requiredPath struct {
	flag  string
	value string
}

// emptyRequiredPath names the first required path that carries no value, or "".
func emptyRequiredPath(paths ...requiredPath) string {
	for _, path := range paths {
		if path.value == "" {
			return path.flag
		}
	}
	return ""
}
