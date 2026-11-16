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
)

type arguments struct {
	Artifact *artifactCommand `arg:"subcommand:artifact" help:"refresh the Node or GitHub CLI release-artifact pin"`
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
		_, _ = os.Stderr.WriteString(artifactwriter.Prefix + err.Error() + "\n")
		parser.WriteHelp(os.Stderr)
		return 2
	}
	if parsed.Artifact == nil {
		parser.WriteHelp(os.Stderr)
		return 2
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

// usage writes one contract line and returns the usage status: a malformed
// invocation, which the tool's statuses report apart from a refusal.
func usage(message string) int { return refuse(2, message) }

// diagnose writes one contract line and returns the refusal status.
func diagnose(message string) int { return refuse(1, message) }

func refuse(status int, message string) int {
	_, _ = os.Stderr.WriteString(artifactwriter.Prefix +
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
