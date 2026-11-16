// Command avm-pin is the maintainer pin writer.
//
// One binary per audience (ADR 0017): every pin writer is a subcommand here, and
// maintainers reach it through the `.sh` wrappers at the paths CONTRIBUTING names.
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/artifactwriter"
	"github.com/gregwebs/agent-vm-images/internal/claudepin"
	"github.com/gregwebs/agent-vm-images/internal/claudewriter"
)

type arguments struct {
	Artifact *artifactCommand `arg:"subcommand:artifact" help:"refresh the Node or GitHub CLI release-artifact pin"`
	Claude   *claudeCommand   `arg:"subcommand:claude" help:"refresh the Claude native payload closure"`
}

func (arguments) Description() string {
	return "Maintainer pin writers. CI refuses all of them."
}

type artifactCommand struct {
	Name     string `arg:"--artifact" placeholder:"NAME" help:"node or gh"`
	Version  string `arg:"--version" placeholder:"X.Y.Z" help:"the exact upstream release"`
	Key      string `arg:"--key" placeholder:"FILE" help:"the Node release key (default: the committed one)"`
	Root     string `arg:"--root" placeholder:"DIR" help:"the repository to publish into (default: the working directory)"`
	Check    bool   `arg:"--check" help:"re-verify the committed pins offline; writes nothing"`
	Fixtures string `arg:"--fixture-upstream" placeholder:"DIR" help:"serve upstream bytes from DIR instead of the network (suites only)"`
}

func (artifactCommand) Description() string {
	return "Resolve one exact upstream release, verify its evidence and publish the pin."
}

type claudeCommand struct {
	Version  string `arg:"--version" placeholder:"X.Y.Z" help:"the exact Claude Code release"`
	Root     string `arg:"--root" placeholder:"DIR" help:"the repository to publish into (default: the working directory)"`
	Check    bool   `arg:"--check" help:"re-verify the committed closure offline; writes nothing"`
	Fixtures string `arg:"--fixture-upstream" placeholder:"DIR" help:"serve upstream bytes from DIR instead of the network (suites only)"`
	Signer   string `arg:"--signer" placeholder:"FINGERPRINT" help:"hold the pin to this primary instead of the reviewed signer (suites only)"`
}

func (claudeCommand) Description() string {
	return "Resolve one exact Claude Code release, verify its signature and publish the closure."
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	var parsed arguments
	parser, err := arg.NewParser(arg.Config{Program: "avm-pin"}, &parsed)
	if err != nil {
		return diagnose(err.Error())
	}
	if err := parser.Parse(args); err != nil {
		if errors.Is(err, arg.ErrHelp) {
			parser.WriteHelp(os.Stdout)
			return 0
		}
		_, _ = os.Stderr.WriteString(prefixFor(args) + err.Error() + "\n")
		parser.WriteHelp(os.Stderr)
		return 2
	}
	if parsed.Artifact == nil && parsed.Claude == nil {
		parser.WriteHelp(os.Stderr)
		return 2
	}
	if parsed.Claude != nil {
		return runClaude(parsed.Claude)
	}
	command := parsed.Artifact
	root, err := resolveRoot(command.Root)
	if err != nil {
		return diagnose(err.Error())
	}
	environment := artifactwriter.Environment{
		Root:       root,
		Artifacts:  filepath.Join(root, "images", "artifacts"),
		Dockerfile: filepath.Join(root, "images", "Dockerfile"),
		TempRoot:   os.Getenv("TMPDIR"),
		Variables:  variables(),
		Path:       os.Getenv("PATH"),
	}
	client := artifactwriter.New(environment, os.Stdout, os.Stderr)
	if command.Fixtures != "" {
		// The fixture source is a flag, not an inherited variable: a pin written
		// from fixture bytes still records the canonical upstream URL, so the seam
		// has to be something the maintainer typed.
		fixture, err := artifactwriter.NewFixtureFetcher(command.Fixtures)
		if err != nil {
			return diagnose(err.Error())
		}
		client.Fetch = fixture
	}
	if command.Check {
		if command.Name != "" || command.Version != "" || command.Key != "" {
			return usage("--check takes no other argument")
		}
		return client.Check()
	}
	if command.Name == "" || command.Version == "" {
		return usage("both --artifact and --version are required")
	}
	name, err := artifactpin.ParseName(command.Name)
	if err != nil {
		return usage(err.Error())
	}
	version, err := artifactpin.ParseVersion(command.Version, "--version")
	if err != nil {
		return usage(err.Error())
	}
	key := command.Key
	if key != "" {
		absolute, err := filepath.Abs(key)
		if err != nil {
			return diagnose(err.Error())
		}
		key = absolute
	}
	return client.Refresh(context.Background(), artifactwriter.Options{
		Name:    name,
		Version: version,
		Key:     key,
	})
}

// prefixFor is the prefix of the subcommand the arguments name, so a parse failure is
// reported by the tool the maintainer actually invoked.
func prefixFor(args []string) string {
	for _, argument := range args {
		if argument == "claude" {
			return claudewriter.Prefix
		}
	}
	return artifactwriter.Prefix
}

// usage writes one contract line for the artifact writer and returns the usage
// status: a malformed invocation, which the tool's statuses report apart from a
// refusal.
func usage(message string) int { return refuse(artifactwriter.Prefix, 2, message) }

// runClaude refreshes the Claude closure: the writer publishes the pin, its two
// evidence files and the Dockerfile ARG default as one transaction.
func runClaude(command *claudeCommand) int {
	root, err := resolveRoot(command.Root)
	if err != nil {
		return refuse(claudewriter.Prefix, 1, err.Error())
	}
	client := claudewriter.New(claudewriter.Environment{
		Root:      root,
		TempRoot:  os.Getenv("TMPDIR"),
		Variables: variables(),
	}, os.Stdout, os.Stderr)
	if command.Fixtures != "" {
		fixture, err := artifactwriter.NewFixtureFetcher(command.Fixtures)
		if err != nil {
			return refuse(claudewriter.Prefix, 1, err.Error())
		}
		client.Fetch = fixture
	}
	if command.Signer != "" {
		// A fixture closure is signed by a throwaway key. The seam is a flag, not an
		// inherited variable: the reviewed signer is the pin's whole authority.
		client.Signer = command.Signer
	}
	if command.Check {
		if command.Version != "" {
			return refuse(claudewriter.Prefix, 2, "--check takes only --root")
		}
		return client.Check()
	}
	if command.Version == "" {
		return refuse(claudewriter.Prefix, 2, "--version is required for a write run")
	}
	if _, err := claudepin.RequireVersion(command.Version, "--version"); err != nil {
		return refuse(claudewriter.Prefix, 2, err.Error())
	}
	return client.Refresh(context.Background(), claudewriter.Options{Version: command.Version})
}

// diagnose writes one contract line for the artifact writer and returns the refusal
// status.
func diagnose(message string) int { return refuse(artifactwriter.Prefix, 1, message) }

// refuse writes one contract line under the caller's own prefix. The prefix is part
// of each tool's contract -- the suites and the hooks match it -- so it is never
// borrowed from whichever writer was written last.
func refuse(prefix string, status int, message string) int {
	_, _ = os.Stderr.WriteString(prefix +
		strings.TrimSuffix(message, "\n") + "\n")
	return status
}

// resolveRoot is the repository a refresh publishes into. It is absolute because
// every path the transaction and gpg see is derived from it, and gpg runs in the
// work directory rather than in the caller's.
func resolveRoot(requested string) (string, error) {
	root := requested
	if root == "" {
		directory, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = directory
	}
	return filepath.Abs(root)
}

func variables() map[string]string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		if index := strings.IndexByte(entry, '='); index >= 0 {
			values[entry[:index]] = entry[index+1:]
		}
	}
	return values
}
