// Command avm-pin is the maintainer pin writer.
//
// One binary per audience (ADR 0017): every pin writer is a subcommand here, and
// maintainers reach it through the `.sh` wrappers at the paths CONTRIBUTING names,
// which run `go run ./cmd/avm-pin`.
package main

import (
	"context"
	"os"

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
	Name    string `arg:"--artifact" placeholder:"NAME" help:"node or gh"`
	Version string `arg:"--version" placeholder:"X.Y.Z" help:"the exact upstream release"`
	Key     string `arg:"--key" placeholder:"FILE" help:"the Node release key (default: the committed one)"`
	Root    string `arg:"--root" placeholder:"DIR" help:"the repository to publish into (default: the working directory)"`
	Check   bool   `arg:"--check" help:"re-verify the committed pins offline; writes nothing"`
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
		_, _ = os.Stderr.WriteString(artifactwriter.Prefix + err.Error() + "\n")
		return 1
	}
	if err := parser.Parse(args); err != nil {
		_, _ = os.Stderr.WriteString(artifactwriter.Prefix + err.Error() + "\n")
		return 2
	}
	if parsed.Artifact == nil {
		parser.WriteHelp(os.Stderr)
		return 2
	}
	root := parsed.Artifact.Root
	if root == "" {
		root, err = os.Getwd()
	}
	if err != nil {
		_, _ = os.Stderr.WriteString(artifactwriter.Prefix + err.Error() + "\n")
		return 1
	}
	environment := artifactwriter.Environment{
		Root:       root,
		Artifacts:  root + "/images/artifacts",
		Dockerfile: root + "/images/Dockerfile",
		TempRoot:   os.Getenv("TMPDIR"),
		Variables:  variables(),
		Path:       os.Getenv("PATH"),
	}
	client := artifactwriter.New(environment, os.Stdout, os.Stderr)
	if fixture := artifactwriter.FixtureFetcher(environment.Variables); fixture != nil {
		client.Fetch = fixture
	}
	switch {
	case parsed.Artifact != nil:
		command := parsed.Artifact
		if command.Check {
			if command.Name != "" || command.Version != "" || command.Key != "" {
				_, _ = os.Stderr.WriteString(artifactwriter.Prefix + "--check takes no other argument\n")
				return 2
			}
			return client.Check()
		}
		if command.Name == "" || command.Version == "" {
			_, _ = os.Stderr.WriteString(artifactwriter.Prefix + "both --artifact and --version are required\n")
			return 2
		}
		name, err := artifactpin.ParseName(command.Name)
		if err != nil {
			_, _ = os.Stderr.WriteString(artifactwriter.Prefix + err.Error() + "\n")
			return 2
		}
		version, err := artifactpin.ParseVersion(command.Version, "--version")
		if err != nil {
			_, _ = os.Stderr.WriteString(artifactwriter.Prefix + err.Error() + "\n")
			return 2
		}
		return client.Refresh(context.Background(), artifactwriter.Options{
			Name:    name,
			Version: version,
			Key:     command.Key,
		})
	default:
		return 2
	}
}

func variables() map[string]string {
	values := map[string]string{}
	for _, entry := range os.Environ() {
		for index := 0; index < len(entry); index++ {
			if entry[index] != '=' {
				continue
			}
			values[entry[:index]] = entry[index+1:]
			break
		}
	}
	return values
}
