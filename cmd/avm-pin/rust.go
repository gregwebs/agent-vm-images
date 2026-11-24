package main

// `avm-pin rust-toolchain`: the staging half of the rust-dev example's pin writer
// (slice 4a of #187). It authenticates one exact channel's whole
// two-architecture closure into a new directory and prints the proposal; the
// publisher that copies it as one transaction is
// `script/dev/upgrade-rust-toolchain.sh`, which owns the rollback and the
// recovery material.
//
// The rules are not here. They are in the layer's own `toolchain` package, which
// the image's helper compiles and this binary imports through the root go.mod
// replace, so the writer and the image cannot disagree about what a pin means.
//
// The diagnostic prefix is frozen at `rust-toolchain-update: `, the prefix of the
// Python writer this replaces: the shell wrapper and the maintenance suite match
// it, and the differential compares it byte for byte until the Python is deleted.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/toolchain"
)

// rustToolchainPrefix is the frozen prefix of the staging verb: the Python stager
// this port replaces caught every staging refusal in one handler and printed it
// under this prefix, and the publisher's stage log is compared on it.
const rustToolchainPrefix = "rust-toolchain-update"

// rustToolchainCheckPrefix is the frozen prefix of the two reader verbs. They came
// from the model's own entrypoint, not the stager's, so a refusal keeps the
// `rust_toolchain: ` prefix the shell and both suites already match; one binary
// carries both because the two verbs are ports of two different programs.
const rustToolchainCheckPrefix = "rust_toolchain"

type rustToolchainCommand struct {
	Stage   *rustToolchainStageCommand   `arg:"subcommand:stage" help:"authenticate one exact channel and write a candidate directory"`
	Check   *rustToolchainCheckCommand   `arg:"subcommand:check" help:"verify a committed pin directory offline"`
	Current *rustToolchainCurrentCommand `arg:"subcommand:current" help:"print the selection a committed pin names"`
	// The fixture grammar is embedded: in the shipped build the type is empty, so
	// the parser has no fixture flag to accept or to refuse (see rust_shipped.go).
	rustToolchainFixtureArguments
}

func (rustToolchainCommand) Description() string {
	return "Stage the rust-dev example's toolchain pin. Never in CI."
}

type rustToolchainStageCommand struct {
	Channel    string `arg:"--channel,required" placeholder:"X.Y.Z" help:"the exact release channel"`
	Rustup     string `arg:"--rustup" placeholder:"VERSION" help:"the audited rustup-init version (default: the committed one)"`
	Key        string `arg:"--key" placeholder:"FILE" help:"the reviewed Rust release key (default: the committed one)"`
	Current    string `arg:"--current,required" placeholder:"DIR" help:"the committed pin directory"`
	Out        string `arg:"--out,required" placeholder:"DIR" help:"a new directory for the candidate"`
	Dockerfile string `arg:"--dockerfile,required" placeholder:"FILE" help:"the layer Dockerfile whose channel assertion is rewritten"`
	ReviewedOn string `arg:"--verify-bootstrap-exception" placeholder:"YYYY-MM-DD" help:"renew the hash-only bootstrap review on this date"`
	Reviewer   string `arg:"--reviewer" placeholder:"NAME" help:"the reviewer who renewed that review"`
}

func (rustToolchainStageCommand) Description() string {
	return "Fetch, verify and stage the full two-architecture closure, publishing nothing."
}

type rustToolchainCheckCommand struct {
	InputDir string `arg:"positional,required" placeholder:"INPUT_DIR" help:"the committed pin directory"`
}

func (rustToolchainCheckCommand) Description() string {
	return "Verify the committed pin data offline."
}

type rustToolchainCurrentCommand struct {
	InputDir string `arg:"positional,required" placeholder:"INPUT_DIR" help:"the committed pin directory"`
}

func (rustToolchainCurrentCommand) Description() string {
	return "Print the channel, rustup version and bootstrap review a committed pin names."
}

// runRustToolchain dispatches the three verbs. A refusal is status 1 and a
// malformed invocation is status 2, the statuses the source writer used.
func runRustToolchain(command *rustToolchainCommand) int {
	seams, err := command.seams()
	if err != nil {
		return refuseRustToolchain(rustToolchainCheckPrefix, 1, err.Error())
	}
	if seams.Requested {
		for _, name := range seams.Names {
			fmt.Println(name)
		}
		return 0
	}
	trust, err := command.trust()
	if err != nil {
		return refuseRustToolchain(rustToolchainCheckPrefix, 1, err.Error())
	}
	switch {
	case command.Check != nil:
		channel, err := toolchain.CheckOffline(context.Background(), command.Check.InputDir,
			nil, trust)
		if err != nil {
			return refuseRustToolchain(rustToolchainCheckPrefix, 1, err.Error())
		}
		fmt.Println(channel)
		return 0
	case command.Current != nil:
		return runRustToolchainCurrent(command.Current, trust)
	case command.Stage != nil:
		return runRustToolchainStage(command.Stage, command.rustToolchainFixtureArguments, trust)
	default:
		fmt.Fprintln(os.Stderr, "usage: avm-pin rust-toolchain {stage,check,current} ARGS")
		return 2
	}
}

func runRustToolchainCurrent(command *rustToolchainCurrentCommand, trust toolchain.TrustPolicy) int {
	pin, err := toolchain.LoadPin(command.InputDir, trust)
	if err != nil {
		return refuseRustToolchain(rustToolchainCheckPrefix, 1, err.Error())
	}
	fmt.Printf("channel=%s\n", pin.Channel)
	fmt.Printf("rustup=%s\n", pin.RustupVersion)
	fmt.Printf("reviewed_on=%s\n", pin.BootstrapException.ReviewedOn)
	fmt.Printf("reviewer=%s\n", pin.BootstrapException.Reviewer)
	return 0
}

func runRustToolchainStage(command *rustToolchainStageCommand,
	seams rustToolchainFixtureArguments, trust toolchain.TrustPolicy) int {
	for _, path := range []string{command.Current, command.Out, command.Dockerfile, command.Key} {
		if path != "" && !filepath.IsAbs(path) {
			return stageRefuse(2, "every path must be absolute: "+path)
		}
	}
	channel, err := toolchain.ParseChannel(command.Channel, "--channel")
	if err != nil {
		return stageRefuse(2, err.Error())
	}
	// One read of the committed pin: it is the default for --rustup and it is what
	// the proposal is described against, so a run that cannot read it refuses here
	// rather than half-way through staging.
	previous, err := previousRustPin(command.Current, trust)
	if err != nil {
		return stageRefuse(1, err.Error())
	}
	request := toolchain.StageRequest{
		Channel:    channel,
		Current:    command.Current,
		Out:        command.Out,
		Dockerfile: command.Dockerfile,
		KeyFile:    command.Key,
		Reviewer:   command.Reviewer,
	}
	request.Rustup, err = stageRustupVersion(command.Rustup, previous)
	if err != nil {
		return stageRefuse(2, err.Error())
	}
	if command.ReviewedOn != "" || command.Reviewer != "" {
		if command.ReviewedOn == "" || command.Reviewer == "" {
			return stageRefuse(2,
				"--verify-bootstrap-exception and --reviewer are given together")
		}
		reviewedOn, err := toolchain.ParseDate(command.ReviewedOn, "--verify-bootstrap-exception")
		if err != nil {
			return stageRefuse(2, err.Error())
		}
		request.ReviewedOn = &reviewedOn
	}
	transport, err := seams.transport()
	if err != nil {
		return stageRefuse(1, err.Error())
	}
	proposal, err := toolchain.Stage(context.Background(), request, toolchain.StageDependencies{
		Transport:     transport,
		Trust:         trust,
		GitHubActions: os.Getenv("GITHUB_ACTIONS") == "true",
	})
	if err != nil {
		return stageRefuse(1, err.Error())
	}
	fmt.Println(proposal.Describe(previous))
	return 0
}

// previousRustPin is the committed pin this run derives from, or nil for a first
// adoption. It is a refusal -- not a first adoption -- when a directory exists and
// does not load: partial state must be recovered, never overwritten.
func previousRustPin(directory string, trust toolchain.TrustPolicy) (*toolchain.Pin, error) {
	if _, err := os.Stat(directory); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	pin, err := toolchain.LoadPin(directory, trust)
	if err != nil {
		return nil, err
	}
	return &pin, nil
}

// stageRustupVersion is the requested audited installer, or the committed one a
// later bump retains.
func stageRustupVersion(requested string, previous *toolchain.Pin) (toolchain.Semver, error) {
	if requested != "" {
		return toolchain.ParseSemver(requested, "--rustup")
	}
	if previous == nil {
		return "", errors.New("first adoption needs --rustup VERSION")
	}
	return previous.RustupVersion, nil
}

// refuseRustToolchain writes one contract line under the verb's frozen prefix.
func refuseRustToolchain(prefix string, status int, message string) int {
	_, _ = os.Stderr.WriteString(prefix + ": " + strings.TrimSuffix(message, "\n") + "\n")
	return status
}

// stageRefuse is the staging verb's refusal: one prefix, every staging failure.
func stageRefuse(status int, message string) int {
	return refuseRustToolchain(rustToolchainPrefix, status, message)
}
