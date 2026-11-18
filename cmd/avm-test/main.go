// Command avm-test is the fixture builder the test suites and the suites above
// them share (ADR 0017).
//
// A pin suite needs synthetic release bytes: fake archives, manifests and release
// records, in both channels a writer corroborates, so that a refusal can be pinned
// on exactly one disagreement. The bumper suite needs the same bytes to serve an
// OLD and a NEW release to the writers it drives, and it needs them for every tool.
// That is why this exists as a command rather than as a flag on a test: the code
// that builds a fixture for another suite is the code the cases check against, so
// the closure handed to the bumper cannot be a hand-written lookalike.
//
// Every fixture here comes from a real writer run. Nothing in this binary writes a
// pin by hand.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/agy"
	"github.com/gregwebs/agent-vm-images/script/test/pinsuite/harness"
)

type arguments struct {
	WriteFixture *writeFixtureCommand `arg:"subcommand:write-fixture" help:"publish one release through its real writer and copy the closure out"`
}

func (arguments) Description() string {
	return "Fixture builders for the pin suites. Writes test fixtures; publishes nothing."
}

type writeFixtureCommand struct {
	Tool    string `arg:"--tool,required" placeholder:"NAME" help:"the pin family: agy"`
	Dir     string `arg:"--dir,required" placeholder:"DIR" help:"the directory to write: DIR/source and DIR/assets"`
	Version string `arg:"--version" placeholder:"X.Y.Z" help:"the release to publish (default: the committed one)"`
}

func (writeFixtureCommand) Description() string {
	return "Publish one release with the real writer and copy what it produced."
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	var parsed arguments
	parser, err := arg.NewParser(arg.Config{Program: "avm-test"}, &parsed)
	if err != nil {
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		return 2
	}
	if err := parser.Parse(argv); err != nil {
		if errors.Is(err, arg.ErrHelp) {
			parser.WriteHelp(os.Stdout)
			return 0
		}
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		parser.WriteHelp(os.Stderr)
		return 2
	}
	if parsed.WriteFixture == nil {
		parser.WriteHelp(os.Stderr)
		return 2
	}
	if err := writeFixture(*parsed.WriteFixture); err != nil {
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		return 1
	}
	return 0
}

func writeFixture(command writeFixtureCommand) error {
	switch command.Tool {
	case "agy":
		version := command.Version
		if version == "" {
			version = agy.Version
		}
		if err := harness.Command(func(r harness.Reporter) {
			agy.WriteFixture(r, command.Dir, version)
		}); err != nil {
			return err
		}
		fmt.Printf("avm-test: agy %s written to %s\n", version, command.Dir)
		return nil
	default:
		return fmt.Errorf("unknown tool %q: the fixtures are agy", command.Tool)
	}
}
