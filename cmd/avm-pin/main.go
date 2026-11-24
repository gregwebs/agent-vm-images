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

	"github.com/gregwebs/agent-vm-images/internal/agywriter"
	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/artifactwriter"
	"github.com/gregwebs/agent-vm-images/internal/claudepin"
	"github.com/gregwebs/agent-vm-images/internal/claudewriter"
	"github.com/gregwebs/agent-vm-images/internal/codewriter"
)

type arguments struct {
	Artifact *artifactCommand      `arg:"subcommand:artifact" help:"refresh the Node or GitHub CLI release-artifact pin"`
	Agy      *agyCommand           `arg:"subcommand:agy" help:"refresh the agy release-artifact pin"`
	Claude   *claudeCommand        `arg:"subcommand:claude" help:"refresh the Claude native payload closure"`
	Codex    *codexCommand         `arg:"subcommand:codex" help:"refresh the Codex payload closure"`
	Rust     *rustToolchainCommand `arg:"subcommand:rust-toolchain" help:"stage the rust-dev example's toolchain pin"`
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

type agyCommand struct {
	Version  string `arg:"--version" placeholder:"X.Y.Z" help:"the exact agy release"`
	Root     string `arg:"--root" placeholder:"DIR" help:"the repository to publish into (default: the working directory)"`
	Check    bool   `arg:"--check" help:"re-verify the committed pin offline; writes nothing"`
	Fixtures string `arg:"--fixture-upstream" placeholder:"DIR" help:"serve upstream bytes from DIR instead of the network (suites only)"`
}

func (agyCommand) Description() string {
	return "Resolve one exact agy release, corroborate it on both channels and publish the pin."
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

type codexCommand struct {
	Version      string `arg:"--version" placeholder:"TAG" help:"the exact openai/codex release tag, rust-v<VERSION>"`
	Root         string `arg:"--root" placeholder:"DIR" help:"the repository to publish into (default: the working directory)"`
	Fixtures     string `arg:"--fixture-upstream" placeholder:"DIR" help:"serve upstream bytes from DIR instead of the network (suites only)"`
	Cosign       string `arg:"--cosign" placeholder:"FILE" help:"the reviewed cosign (v2.4.1); required for a write run"`
	ProbeCosign  string `arg:"--probe-cosign" placeholder:"FILE" help:"require FILE to be the reviewed verifier and print it; writes nothing"`
	StageDir     string `arg:"--stage-dir" placeholder:"DIR" help:"write the six-file proposal into DIR instead of publishing it"`
	PublishStage string `arg:"--publish-stage" placeholder:"DIR" help:"publish that staged proposal as one transaction"`
	Check        bool   `arg:"--check" help:"validate the committed inputs offline; writes nothing"`
}

func (codexCommand) Description() string {
	return "Resolve one exact Codex release, verify both bundles and publish the closure."
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
	if parsed.Artifact == nil && parsed.Agy == nil && parsed.Claude == nil &&
		parsed.Codex == nil && parsed.Rust == nil {
		parser.WriteHelp(os.Stderr)
		return 2
	}
	if parsed.Agy != nil {
		return runAgy(parsed.Agy)
	}
	if parsed.Claude != nil {
		return runClaude(parsed.Claude)
	}
	if parsed.Codex != nil {
		return runCodex(parsed.Codex)
	}
	if parsed.Rust != nil {
		return runRustToolchain(parsed.Rust)
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
		switch argument {
		case "claude":
			return claudewriter.Prefix
		case "codex":
			return codewriter.Prefix
		case "agy":
			return agywriter.Prefix
		case "rust-toolchain":
			return rustToolchainPrefix
		}
	}
	return artifactwriter.Prefix
}

// usage writes one contract line for the artifact writer and returns the usage
// status: a malformed invocation, which the tool's statuses report apart from a
// refusal.
func usage(message string) int { return refuse(artifactwriter.Prefix, 2, message) }

// runAgy refreshes the agy selection: the writer publishes the pin, its three
// evidence files and the Dockerfile ARG default as one transaction.
func runAgy(command *agyCommand) int {
	root, err := resolveRoot(command.Root)
	if err != nil {
		return refuse(agywriter.Prefix, 1, err.Error())
	}
	client := agywriter.New(agywriter.Environment{
		Root:      root,
		TempRoot:  os.Getenv("TMPDIR"),
		Variables: variables(),
	}, os.Stdout, os.Stderr)
	if command.Fixtures != "" {
		// The fixture source is a flag, not an inherited variable: a pin written from
		// fixture bytes still records the canonical upstream URL, so the seam has to
		// be something the maintainer typed.
		fixture, err := artifactwriter.NewFixtureFetcher(command.Fixtures)
		if err != nil {
			return refuse(agywriter.Prefix, 1, err.Error())
		}
		client.Fetch = fixture
	}
	if command.Check {
		if command.Version != "" {
			return refuse(agywriter.Prefix, 2, "--check takes only --root")
		}
		return client.Check()
	}
	if command.Version == "" {
		return refuse(agywriter.Prefix, 2, "--version is required for a write run")
	}
	// A version that cannot name a release is a refusal, not a malformed
	// invocation: the flag was given and is well formed as an argument, so the tool's
	// own status for "this cannot be pinned" is 1 -- which is the status the writer
	// this replaced used, and the one `avm-pin codex` uses for the same input.
	version, err := artifactpin.ParseVersion(command.Version, "--version")
	if err != nil {
		return refuse(agywriter.Prefix, 1, err.Error())
	}
	return client.Refresh(context.Background(), agywriter.Options{Version: version})
}

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

// runCodex refreshes the Codex closure: the writer publishes the pin, its four
// evidence files and the Dockerfile ARG default as one transaction, and the bumper
// stages that proposal instead so it can publish several owners together.
func runCodex(command *codexCommand) int {
	root, err := resolveRoot(command.Root)
	if err != nil {
		return refuse(codewriter.Prefix, 1, err.Error())
	}
	client := codewriter.New(codewriter.Environment{
		Root:      root,
		TempRoot:  os.Getenv("TMPDIR"),
		Variables: variables(),
	}, os.Stdout, os.Stderr)
	if command.Fixtures != "" {
		// The fixture source is a flag, not an inherited variable: a pin written from
		// fixture bytes still records the canonical upstream URL, so the seam has to
		// be something the maintainer typed.
		fixture, err := artifactwriter.NewFixtureFetcher(command.Fixtures)
		if err != nil {
			return refuse(codewriter.Prefix, 1, err.Error())
		}
		client.Fetch = fixture
	}
	if command.Check {
		if command.Version != "" || command.Cosign != "" || command.ProbeCosign != "" ||
			command.StageDir != "" || command.PublishStage != "" {
			return refuse(codewriter.Prefix, 2, "--check takes no other argument")
		}
		return client.Check()
	}
	if command.ProbeCosign != "" {
		if command.Version != "" || command.Cosign != "" || command.StageDir != "" ||
			command.PublishStage != "" {
			return refuse(codewriter.Prefix, 2, "--probe-cosign takes only --root")
		}
		return client.ProbeCosign(command.ProbeCosign)
	}
	if command.PublishStage != "" {
		if command.Version != "" || command.Cosign != "" || command.StageDir != "" {
			return refuse(codewriter.Prefix, 2, "--publish-stage takes only --root")
		}
		return client.PublishStage(command.PublishStage)
	}
	if command.Version == "" {
		return refuse(codewriter.Prefix, 2, "--version is required for a write run")
	}
	return client.Refresh(context.Background(), codewriter.Options{
		Version:  command.Version,
		Cosign:   command.Cosign,
		StageDir: command.StageDir,
	})
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
