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
	"path/filepath"
	"sort"
	"strings"

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
	Dir     string `arg:"--dir,required" placeholder:"DIR" help:"the absolute directory to write: DIR/source and DIR/assets"`
	Version string `arg:"--version" placeholder:"X.Y.Z" help:"the release to publish (default: the fixture release, 1.4.11)"`
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
	// A flag this tool cannot honour is a malformed invocation, not a failed run: a
	// caller that branches on the status must read a typo as a typo. The wrapper
	// chdirs to the checkout before exec, so a relative directory would silently name
	// a different tree here than it did for the caller.
	if _, known := tools[parsed.WriteFixture.Tool]; !known {
		fmt.Fprintf(os.Stderr, "avm-test: %q is not a fixture this tool builds: %s\n",
			parsed.WriteFixture.Tool, toolNames())
		return 2
	}
	if !filepath.IsAbs(parsed.WriteFixture.Dir) {
		fmt.Fprintf(os.Stderr, "avm-test: --dir must be absolute: %q, because this tool runs from the checkout\n",
			parsed.WriteFixture.Dir)
		return 2
	}
	if err := writeFixture(*parsed.WriteFixture); err != nil {
		fmt.Fprintf(os.Stderr, "avm-test: %v\n", err)
		return 1
	}
	return 0
}

// tools is the fixture families this command builds. One entry, and the union is
// the switch below: a family added to one and not the other does not compile.
var tools = map[string]func(harness.Reporter, agy.Fixture) string{
	"agy": func(r harness.Reporter, fixture agy.Fixture) string {
		agy.WriteFixture(r, fixture)
		return fmt.Sprintf("agy %s written to %s", fixture.Version, fixture.Dir)
	},
}

func toolNames() string {
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func writeFixture(command writeFixtureCommand) error {
	fixture := agy.Fixture{Dir: command.Dir, Version: command.Version}
	if fixture.Version == "" {
		fixture.Version = agy.Version
	}
	var summary string
	build := tools[command.Tool]
	if err := harness.Command(func(r harness.Reporter) { summary = build(r, fixture) }); err != nil {
		return err
	}
	fmt.Printf("avm-test: %s\n", summary)
	return nil
}
