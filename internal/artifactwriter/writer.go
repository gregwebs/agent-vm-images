// Package artifactwriter refreshes one committed third-party release-artifact pin.
//
// It is the only writer of `images/artifacts/<name>.json`, of the GitHub CLI
// attestation bundles beside it and of the `org.agent-vm.version.<name>` label in
// `images/Dockerfile`. Builds only verify those bytes: no ordinary build resolves a
// URL, downloads a checksum or picks a version.
//
// One run resolves the requested exact version on the live upstream, hashes both
// supported architectures, verifies every piece of authentication evidence and only
// then publishes the pin, the evidence files and the label as one transaction. A
// partial upstream failure leaves the committed files byte-identical; the tool
// never commits, never runs in CI and never adopts a newer version by itself.
//
// Exit statuses: 0 published or unchanged, 1 failed with the sources unchanged or
// fully restored, 2 usage, 3 undecided or incomplete recovery (the work directory is
// retained and printed), 128+N after a signal whose rollback succeeded.
package artifactwriter

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/artifactpin"
	"github.com/gregwebs/agent-vm-images/internal/fetch"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// Prefix is frozen: three suites read these lines to find the retained recovery
// material, so the diagnostic is part of the tool's contract rather than a log.
const Prefix = "pin-artifact: "

// Fetcher is the upstream byte source. Production is internal/fetch; the
// maintenance harness substitutes a fixture directory, so no case reaches the
// network and the URL a pin records is still the one a build would fetch.
type Fetcher interface {
	Get(ctx context.Context, url string, limit int64) ([]byte, error)
}

// Runner runs one external command. gpg and gh are real programs, so the harness
// stubs them on PATH rather than here.
type Runner struct {
	// Run returns the finished command, and an error only when it could not be
	// started. A non-zero status is not an error: several checks here read it.
	Run func(ctx context.Context, argv []string, dir string,
		environment []string) (CommandResult, error)
}

// Environment is the process view the writer needs: the variables the CI refusal
// reads, PATH for the external commands, and the paths the transaction publishes.
type Environment struct {
	Root       string
	Artifacts  string
	Dockerfile string
	TempRoot   string
	Variables  map[string]string
	Path       string
}

// Client is one writer invocation.
type Client struct {
	Env    Environment
	Fetch  Fetcher
	Runner Runner
	Out    io.Writer
	Err    io.Writer
	// Now is the clock `verified_on` is stamped from. A pin is written on one
	// maintainer host and read by reviewers and builders in every zone, so the
	// stamp is one instant: the UTC date, never the writer's local calendar.
	Now func() time.Time
}

// Options is the requested refresh.
type Options struct {
	Name    artifactpin.Name
	Version artifactpin.Version
	// Key is the Node release key to verify against; empty means the committed one.
	Key string
}

// New returns a client with the production byte source and command runner.
func New(env Environment, out, errOut io.Writer) *Client {
	return &Client{
		Env:   env,
		Fetch: fetch.New(),
		Runner: Runner{Run: func(ctx context.Context, argv []string, dir string,
			environment []string) (CommandResult, error) {
			return runCommand(ctx, argv, dir, environment)
		}},
		Out: out,
		Err: errOut,
		Now: time.Now,
	}
}

// refuse is a case that failed before anything was published: a status-1 refusal
// whose message is the diagnostic.
func refuse(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// report writes one contract line.
func (c *Client) report(format string, args ...any) {
	_, _ = fmt.Fprintf(c.Out, Prefix+format+"\n", args...)
}

// diagnose writes one contract line to stderr.
func (c *Client) diagnose(format string, args ...any) {
	_, _ = fmt.Fprintf(c.Err, Prefix+format+"\n", args...)
}

// Check re-verifies the committed pins offline: schema, evidence digests and
// labels. It writes nothing.
func (c *Client) Check() int {
	problems := []string{}
	for _, name := range []artifactpin.Name{artifactpin.Node, artifactpin.GH} {
		path := filepath.Join(c.Env.Artifacts, name.String()+".json")
		pin, err := artifactpin.Load(path)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if pin.Name != name {
			problems = append(problems, fmt.Sprintf("%s: the pin names %s", name, pin.Name))
		}
		if pin.Authentication.OpenPGP != nil {
			openpgp := pin.Authentication.OpenPGP
			if _, err := artifactpin.CheckEvidence(pin, openpgp.KeyPath, openpgp.KeyDigest); err != nil {
				problems = append(problems, err.Error())
			}
		}
		for _, platform := range artifactpin.Platforms {
			archive, err := pin.Archive(platform)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			if archive.Bundle != nil {
				if _, err := artifactpin.CheckEvidence(pin, archive.Bundle.Path,
					archive.Bundle.Digest); err != nil {
					problems = append(problems, err.Error())
				}
			}
			c.report("%s %s %s: %s %s", name, pin.Version, platform, archive.Filename(), archive.Digest)
		}
		expected := "LABEL " + artifactpin.LabelPrefix + name.String() + `="` + pin.Version.String() + `"`
		dockerfile, err := os.ReadFile(c.Env.Dockerfile)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if !containsLine(string(dockerfile), expected) {
			problems = append(problems, "images/Dockerfile does not declare "+expected)
		}
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			_, _ = fmt.Fprintf(c.Err, "FAIL: %s\n", problem)
		}
		return 1
	}
	c.report("the committed pins, evidence files and labels agree")
	return 0
}

func containsLine(text, line string) bool {
	for _, candidate := range strings.Split(text, "\n") {
		if candidate == line {
			return true
		}
	}
	return false
}

// Refresh resolves one exact release, verifies every piece of evidence and
// publishes the pin, its evidence files and the label as one transaction.
func (c *Client) Refresh(ctx context.Context, options Options) int {
	if c.Env.Variables["GITHUB_ACTIONS"] == "true" {
		c.diagnose("refusing GitHub Actions: pins are maintainer-host maintenance only")
		return 1
	}
	if options.Name != artifactpin.Node && options.Name != artifactpin.GH {
		c.diagnose("%s: no pin writer builds this artifact", options.Name)
		return 2
	}
	key := options.Key
	if options.Name == artifactpin.Node && key == "" {
		key = filepath.Join(c.Env.Artifacts, artifactpin.NodeKey.String())
		if _, err := os.Stat(key); err != nil {
			c.diagnose("no committed node release key at %s", key)
			return 1
		}
	}
	work, err := os.MkdirTemp(c.Env.TempRoot, "pin-artifact.work.")
	if err != nil {
		c.diagnose("cannot create a work directory (%s); sources unchanged", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(work) }()

	staged, err := c.resolve(ctx, options, key, work)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	committed := map[string][]byte{}
	for _, entry := range staged {
		raw, err := os.ReadFile(filepath.Join(c.Env.Root, entry.Path))
		if err != nil {
			continue
		}
		committed[entry.Path] = raw
	}
	unchanged := true
	for _, entry := range staged {
		if raw, ok := committed[entry.Path]; !ok || string(raw) != string(entry.Raw) {
			unchanged = false
		}
	}
	if unchanged {
		c.report("unchanged (%s %s)", options.Name, options.Version)
		return 0
	}
	for _, entry := range staged {
		if raw, ok := committed[entry.Path]; !ok || string(raw) != string(entry.Raw) {
			c.diagnose("would publish %s", entry.Path)
		}
	}
	status := publication.Publish(publication.Request{
		Prefix:     Prefix,
		Root:       c.Env.Root,
		TempRoot:   c.Env.TempRoot,
		TempPrefix: "pin-artifact.",
		Stderr:     publication.StderrOf(c.Err),
		Files:      staged,
	})
	if status != 0 {
		return status
	}
	c.report("published %s %s: %s", options.Name, options.Version, stringsJoined(staged))
	c.report("now run: bash script/build/base-inputs.sh --write")
	return 0
}

func stringsJoined(staged []publication.Staged) string {
	paths := make([]string, 0, len(staged))
	for _, entry := range staged {
		paths = append(paths, entry.Path)
	}
	sort.Strings(paths)
	return strings.Join(paths, ", ")
}
