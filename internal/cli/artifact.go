package cli

import (
	"context"
	"errors"
	"os"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/artifactinstall"
	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
)

// installArtifactUsage describes the install subcommand.
type installArtifactUsage struct {
	Name   string `arg:"positional,required" help:"artifact name: node, gh or agy"`
	Pin    string `arg:"positional,required" help:"the committed pin file"`
	Arch   string `arg:"positional,required" help:"target architecture: amd64 or arm64"`
	Root   string `arg:"--root" default:"/" help:"the image root to install into"`
	Events string `arg:"--events" help:"append every fetch, extraction, attestation and publication to this file"`
}

func (installArtifactUsage) Description() string {
	return "Install one committed release-artifact pin into an image root."
}

// artifactPinUsage describes the pin inspection subcommand.
type artifactPinUsage struct {
	Command string `arg:"positional,required" help:"version"`
	Pin     string `arg:"positional,required" help:"the committed pin file"`
	NPM     bool   `arg:"--npm" help:"print the npm version the reviewed archive ships"`
}

func (artifactPinUsage) Description() string {
	return "Print a value one committed artifact pin selects."
}

// runInstallArtifact installs a pin: the only path that puts Node, the GitHub CLI
// or agy into an image.
func (p *process) runInstallArtifact(args []string) int {
	var usage installArtifactUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract install-artifact"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	pin, err := artifactpin.Load(usage.Pin)
	if err != nil {
		return p.artifactFailure(err)
	}
	if pin.Name.String() != usage.Name {
		return p.artifactFailure(refuseName(usage.Pin, pin.Name.String(), usage.Name))
	}
	options := artifactinstall.Options{
		Pin:    pin,
		Arch:   usage.Arch,
		Root:   usage.Root,
		Stderr: p.streams.Err,
	}
	if usage.Events != "" {
		// The suite reads this log to prove the ordering, so it starts empty.
		if err := os.WriteFile(usage.Events, nil, 0o600); err != nil {
			return p.artifactFailure(err)
		}
		handle, err := os.OpenFile(usage.Events, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return p.artifactFailure(err)
		}
		defer func() { _ = handle.Close() }()
		options.Events = artifactinstall.NewEvents(handle)
	}
	if err := artifactinstall.Install(context.Background(), options); err != nil {
		return p.artifactFailure(err)
	}
	options.Report()
	return 0
}

// runArtifactPin prints one value a pin selects, read through the same validator
// the installer uses instead of matching JSON text.
func (p *process) runArtifactPin(args []string) int {
	var usage artifactPinUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract artifact-pin"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	if usage.Command != "version" {
		return p.stderrStatus(2, "artifact-pin: usage: recipe-contract artifact-pin version PIN.json [--npm]\n")
	}
	pin, err := artifactpin.Load(usage.Pin)
	if err != nil {
		return p.artifactFailure(err)
	}
	if !usage.NPM {
		return p.stdoutStatus(pin.Version.String() + "\n")
	}
	npm, ok := pin.Bundled["npm"]
	if !ok {
		return p.stderrStatus(1, "read-pin-version: "+usage.Pin+" records no bundled npm\n")
	}
	return p.stdoutStatus(npm.String() + "\n")
}

func refuseName(path, pinned, asked string) error {
	return &artifactpin.Refusal{Message: path + " pins " + pinned + ", not " + asked}
}

// argFailure reports a parse failure: help and usage read as the caller's own
// mistake, so they keep status 2.
func (p *process) argFailure(parser *arg.Parser, err error) int {
	if errors.Is(err, arg.ErrHelp) {
		parser.WriteHelp(p.streams.Err)
		return 0
	}
	parser.Fail(err.Error())
	return 2
}

// artifactFailure reports a refusal the way the installers do, with the program's
// own prefix.
func (p *process) artifactFailure(err error) int {
	return p.stderrStatus(1, "install-artifact: "+err.Error()+"\n")
}
