// Package agywriter refreshes the one committed agy selection.
//
// It is the only writer of `images/tools/agy/pin.json`, of the three files under
// `images/tools/agy/evidence/` and of the `ARG AGENT_VERSION_AGY` default in
// `images/standard/Dockerfile`. Ordinary builds only verify those bytes: they fetch
// the one computed release URL, re-check the committed corroboration against the pin
// and accept only the committed digests.
//
// Google publishes no signature or attestation for these assets, so this package is
// the whole of the trust decision and it is deliberately narrow: a version is
// pinnable only when the downloaded bytes equal **both** GitHub's asset digest and
// the sha512 in Google's updater manifest, and only when that manifest names the same
// release the run was asked for. The manifest names only the latest release, so an
// older version cannot be re-pinned and a same-release upstream rewrite is refused
// rather than silently adopted. Everything is then published as one transaction; a
// partial upstream failure leaves every committed byte identical.
//
// Statuses: 0 published or unchanged, 1 failed with the sources unchanged or fully
// restored, 3 undecided or incomplete recovery (the retained directories are
// printed), 128+N after a signal whose rollback restored the sources
// (the six signal windows and what each retains: `internal/publication`).
package agywriter

import (
	"context"
	"errors"
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

// Prefix is frozen: the maintenance suite reads these lines to find the retained
// recovery material, so the diagnostic is part of the tool's contract.
const Prefix = "pin-agy: "

const (
	// PinDirectory is the committed closure's own directory.
	PinDirectory = "images/tools/agy"
	// PinFile is the committed selection.
	PinFile = PinDirectory + "/pin.json"
	// Dockerfile carries the ARG default and the label that reads it.
	Dockerfile = "images/standard/Dockerfile"
	// Argument is the layer argument the pin's version is published through.
	Argument = "AGENT_VERSION_AGY"
	// Label is the image label that reads the argument.
	Label = "org.agent-vm.version.agy"
	// MaxJSONBytes bounds one JSON document read from upstream.
	MaxJSONBytes = 1 << 20
)

// Reason is the reviewer's justification the pin records. It is part of the
// committed bytes, so a re-render that changed a word would turn every repeat run
// into a churn-only diff: it is stated once, here.
const Reason = "Google publishes no signature or attestation for these assets and the " +
	"GitHub release is mutable and uploaded by an individual account. The pin is " +
	"hash-only: at pin time the downloaded bytes had to equal both GitHub's asset " +
	"digest and the sha512 in Google's updater manifest, which names only the latest " +
	"release. The build re-checks the committed evidence against the pin and accepts " +
	"only the committed digests. See ADR 0012."

// Fetcher is the upstream byte source. Production is internal/fetch; the maintenance
// suite substitutes a directory, so no case reaches the network and the URL a pin
// records is still the one a build would fetch.
type Fetcher interface {
	Get(ctx context.Context, url string, limit int64) ([]byte, error)
}

// Environment is the process view the writer needs: the variables the CI refusal
// reads and the paths the transaction publishes.
type Environment struct {
	Root      string
	TempRoot  string
	Variables map[string]string
}

// Client is one writer invocation.
type Client struct {
	Env   Environment
	Fetch Fetcher
	Out   io.Writer
	Err   io.Writer
	// Now is the clock `verified_on` is stamped from: the UTC date, never the
	// writer's local calendar (#136).
	Now func() time.Time
	// Publish is the publication transaction. Production is publication.Publish; a
	// test substitutes a status so the retained-material contract is reachable
	// without injecting a fault into the transaction itself.
	Publish func(publication.Request) int
}

// Options is the requested refresh.
type Options struct {
	Version artifactpin.Version
}

// New returns a client with the production byte source.
func New(env Environment, out, errOut io.Writer) *Client {
	return &Client{Env: env, Fetch: fetch.New(), Out: out, Err: errOut, Now: time.Now,
		Publish: publication.Publish}
}

// report writes one contract line on stdout.
func (c *Client) report(format string, args ...any) {
	_, _ = fmt.Fprintf(c.Out, Prefix+format+"\n", args...)
}

// diagnose writes one contract line on stderr.
func (c *Client) diagnose(format string, args ...any) {
	_, _ = fmt.Fprintf(c.Err, Prefix+format+"\n", args...)
}

// announce names a directory before a publication that a signal may end, so the way
// back is on the record even when nothing after it is printed.
func (c *Client) announce(what, directory string) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		absolute = directory
	}
	_, _ = fmt.Fprintf(c.Err, "%s%s %s\n", Prefix, what, absolute)
	if handle, ok := c.Err.(interface{ Sync() error }); ok {
		_ = handle.Sync()
	}
}

func (c *Client) refuseCI() error {
	if c.Env.Variables["GITHUB_ACTIONS"] == "true" {
		return errors.New("refusing GitHub Actions: pins are maintainer-host " +
			"maintenance only")
	}
	return nil
}

// Refresh resolves one exact release, corroborates it on both channels, preflights
// both archives and publishes the pin, its three evidence files and the Dockerfile
// ARG default as one transaction.
func (c *Client) Refresh(ctx context.Context, options Options) int {
	if err := c.refuseCI(); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	work, err := os.MkdirTemp(c.Env.TempRoot, "pin-agy.work.")
	if err != nil {
		c.diagnose("cannot create a work directory (%s); sources unchanged", err)
		return 1
	}
	absolute, err := filepath.Abs(work)
	if err == nil {
		work = absolute
	}
	staged, err := c.resolve(ctx, options.Version, work)
	if err != nil {
		_ = os.RemoveAll(work)
		c.diagnose("%s", err)
		return 1
	}
	if c.unchanged(staged) {
		_ = os.RemoveAll(work)
		c.report("unchanged (agy %s)", options.Version)
		return 0
	}
	// A signal-driven rollback exits from inside the publisher, so the owner scratch
	// is named before publication starts, not after it. From here the publisher owns
	// that directory: it removes it on a decided outcome and retains and prints it
	// otherwise.
	c.announce("owner scratch:", work)
	return c.Publish(publication.Request{
		Prefix:     Prefix,
		Root:       c.Env.Root,
		TempRoot:   c.Env.TempRoot,
		TempPrefix: "pin-agy.",
		Stderr:     publication.StderrOf(c.Err),
		Owner:      work,
		Report: publication.Report{To: c.Out, Lines: []string{
			fmt.Sprintf("published agy %s: %s", options.Version, destinations(staged)),
			fmt.Sprintf("now update script/test/fixtures/installer-egress/selections.tsv to agy "+
				"%s, then run: bash script/test/contracts.sh", options.Version),
		}},
		Files: staged,
	})
}

// unchanged reports whether every publication byte already equals the committed
// byte, which is what makes a repeat run print `unchanged` instead of churn.
func (c *Client) unchanged(staged []publication.Staged) bool {
	for _, entry := range staged {
		raw, err := os.ReadFile(filepath.Join(c.Env.Root, entry.Path))
		if err != nil || string(raw) != string(entry.Raw) {
			return false
		}
	}
	return true
}

func destinations(staged []publication.Staged) string {
	paths := make([]string, 0, len(staged))
	for _, entry := range staged {
		paths = append(paths, entry.Path)
	}
	sort.Strings(paths)
	return strings.Join(paths, ", ")
}

// Check re-verifies the committed pin, its evidence and the single agy ARG offline.
// It reports, and does not prepare a rewrite.
func (c *Client) Check() int {
	path := filepath.Join(c.Env.Root, PinFile)
	if !plainFile(path) {
		c.diagnose("%s is missing: there is no committed agy selection to check", path)
		return 1
	}
	pin, err := artifactpin.Load(path)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	if pin.Name != artifactpin.Agy {
		c.diagnose("%s names artifact %s", path, pin.Name)
		return 1
	}
	if err := artifactpin.CheckCorroboration(pin); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	dockerfile, err := os.ReadFile(filepath.Join(c.Env.Root, Dockerfile))
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	// The committed ARG must already be the pin's version: --check reports, it does
	// not prepare a rewrite.
	if err := requireSingleSlot(string(dockerfile), pin.Version); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	c.report("agy %s: offline committed-input check (committed bytes only; not a "+
		"fresh corroboration)", pin.Version)
	for _, platform := range artifactpin.Platforms {
		archive, err := pin.Archive(platform)
		if err != nil {
			c.diagnose("%s", err)
			return 1
		}
		c.report("  %s %s %s", platform, archive.Filename(), archive.Digest)
	}
	c.report("  evidence: %d files, all agreeing with the pin", len(pin.Authentication.
		HashOnly.Manifests)+1)
	c.report("  %s: ARG %s=%s, LABEL %s references it", Dockerfile, Argument,
		pin.Version, Label)
	return 0
}

// plainFile reports whether path is a regular file reached without following a
// symlink, which is the only thing a committed input may be.
func plainFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}
