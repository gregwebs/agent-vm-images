// Command rust-dev-helper is the rust-dev example layer's own program.
//
// ADR 0017 (example layers stay self-contained) makes this a module of its own,
// compiled by the layer's pinned `golang:` stage and mounted into the RUN that
// uses it: an example build has no `--build-context`, so nothing from the host or
// from ./internal can reach it. Slice 1 of #187 moves the one verb no other
// program imports; `check`, `install` and `verify-installed` follow in slices 2
// and 3.
package main

import (
	"fmt"
	"os"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/kacheprobe"
)

// prefix is the one diagnostic prefix this binary shares with the Python script
// it replaces: the verifier's log lines and the shell suites match on it.
const prefix = kacheprobe.Prefix

type arguments struct {
	KacheProbe *kacheProbeCommand `arg:"subcommand:kache-probe" help:"decide a kache doctor --probe --json report"`
	// The fixture grammar is embedded: in the shipped build the type is empty, so
	// the parser has no fixture flag to accept or to refuse (see seams_shipped.go).
	fixtureArguments
}

func (arguments) Description() string {
	return "rust-dev example layer helper. Runs from the layer's own build stages."
}

type kacheProbeCommand struct {
	Report string `arg:"positional,required" placeholder:"REPORT" help:"the report file kache wrote"`
}

func (kacheProbeCommand) Description() string {
	return "Refuse a kache doctor report that fails, or is missing a required finding."
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	var parsed arguments
	parser, err := arg.NewParser(arg.Config{Program: "rust-dev-helper"}, &parsed)
	if err != nil {
		return refuse(err.Error())
	}
	if err := parser.Parse(args); err != nil {
		// Every failure of this binary is status 1, usage included: the Python
		// script it replaces printed a usage line and returned 1, and the verifier
		// treats any non-zero status as a failed build. A go-arg refusal is a
		// malformed invocation, so it is reported as usage rather than as a
		// diagnostic from a verb that never ran.
		return refuse("usage: " + usageText)
	}
	if parsed.fixtureSeamsRequested() {
		for _, seam := range fixtureSeams {
			fmt.Println(seam)
		}
		return 0
	}
	switch {
	case parsed.KacheProbe != nil:
		return kacheprobe.Decide(parsed.KacheProbe.Report, os.Stdout, os.Stderr)
	default:
		return refuse("usage: " + usageText)
	}
}

const usageText = "rust-dev-helper kache-probe REPORT"

// refuse reports one contract line on stderr and returns the refusal status. It is
// the only status this binary returns besides success: the Python predecessor
// returned 1 for every failure, including a usage error, so a caller cannot tell
// a malformed invocation from a refused report by status alone -- and the
// verifier, which is the only caller, does not need to.
func refuse(message string) int {
	fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, message)
	return 1
}
