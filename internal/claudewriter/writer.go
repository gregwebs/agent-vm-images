// Package claudewriter refreshes the one committed Claude native payload closure.
//
// It is the only writer of `images/tools/claude/pin.json`, of the two files under
// `images/tools/claude/evidence/` and of the `ARG AGENT_VERSION_CLAUDE` default in
// `images/standard/Dockerfile`. It never writes the release key, the hooks or the
// reader: the key is committed by hand so a key change is always a reviewed diff.
//
// One run fetches the signed manifest, its detached signature and the published
// release key, requires the published key to be byte-identical to the committed one,
// requires the committed key's single primary to be the reviewed signer, verifies
// the signature over the manifest, downloads both maintained Linux executables and
// requires each to match the signed manifest and to be an ELF for its machine, then
// publishes the pin, both evidence files and the ARG default as one transaction with
// an auditable payload delta. A partial upstream failure leaves every committed byte
// identical. The tool never commits, never runs under GitHub Actions and never
// adopts a newer version by itself.
//
// Exit statuses: 0 published or unchanged, 1 failed with the sources unchanged or
// fully restored, 2 usage, 3 undecided or incomplete recovery (the work directory is
// retained and printed), 128+N after a signal whose rollback restored the sources
// (the six signal windows and what each retains: `internal/publication`).
package claudewriter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/claudepin"
	"github.com/gregwebs/agent-vm-images/internal/fetch"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// Prefix is frozen: the suite reads these lines to find the retained recovery
// material, so the diagnostic is part of the tool's contract rather than a log.
const Prefix = "pin-claude: "

// The closure's own paths, relative to the repository.
const (
	PinDirectory = "images/tools/claude"
	PinFile      = PinDirectory + "/pin.json"
	Dockerfile   = "images/standard/Dockerfile"
	// Argument and Label are the Dockerfile slot and the label that reads it.
	Argument = "AGENT_VERSION_CLAUDE"
	Label    = "org.agent-vm.version.claude"
)

// Reason is the `authentication.reason` every pin this tool writes carries: the
// review record, in the record's own words.
const Reason = "The manifest is the selection authority: the build verifies the " +
	"detached OpenPGP signature over the committed manifest.json with the committed " +
	"release key, requires the pinned primary fingerprint, and refuses unless the " +
	"signed checksum and size for the platform equal the committed ones before the " +
	"executable is fetched. The executable is placed in the native layout and " +
	"`claude install` runs only with no network, so the native installer's own " +
	"downloads never happen. See ADR 0013."

// Fetcher is the upstream byte source. Production is internal/fetch; the maintenance
// harness substitutes a fixture directory, so no case reaches the network and the URL
// a pin records is still the one a build would fetch.
type Fetcher interface {
	Get(ctx context.Context, url string, limit int64) ([]byte, error)
}

// Environment is the process view the writer needs.
type Environment struct {
	// Root is the repository to publish into.
	Root string
	// TempRoot is where the work directory is created. Empty means the system
	// default.
	TempRoot string
	// Variables are the environment entries the CI refusal reads.
	Variables map[string]string
}

// Client is one writer invocation.
type Client struct {
	Env   Environment
	Fetch Fetcher
	Out   io.Writer
	Err   io.Writer
	// Now is the clock `verified_on` is stamped from. A pin is written on one
	// maintainer host and read by reviewers and builders in every zone, so the
	// stamp is the UTC date, never the writer's local calendar.
	Now func() time.Time
	// Warn receives a gpg diagnostic that is not itself a refusal.
	Warn func(format string, args ...any)
	// Signer is the primary the run holds the pin to. Empty means the reviewed
	// signer; a suite points it at its own throwaway key, and it is a field rather
	// than an environment variable so a plain refresh can never be redirected.
	Signer string
}

// Options is the requested refresh.
type Options struct {
	Version string
}

// New returns a client with the production byte source.
func New(env Environment, out, errOut io.Writer) *Client {
	return &Client{
		Env:   env,
		Fetch: fetch.New(),
		Out:   out,
		Err:   errOut,
		Now:   time.Now,
		Warn: func(format string, args ...any) {
			_, _ = fmt.Fprintf(errOut, Prefix+format+"\n", args...)
		},
	}
}

// refusal is a case that failed before anything was published: a status-1 refusal
// whose message is the diagnostic.
func refusal(format string, args ...any) error {
	return claudepin.Refuse(format, args...)
}

// withPath is a refusal that names the file it is about, in the writer's voice.
func withPath(path string, err error) error {
	return fmt.Errorf("%s: %w", path, err)
}

// report writes one prefixless line: the reviewed facts, which the maintainer reads
// as the tool's answer rather than as a diagnostic.
func (c *Client) report(format string, args ...any) {
	_, _ = fmt.Fprintf(c.Out, format+"\n", args...)
}

// note writes one line the tool signs with its own prefix.
func (c *Client) note(format string, args ...any) {
	_, _ = fmt.Fprintf(c.Out, Prefix+format+"\n", args...)
}

func (c *Client) diagnose(format string, args ...any) {
	_, _ = fmt.Fprintf(c.Err, Prefix+format+"\n", args...)
}

// RefuseCI is the guard every writer shares: pins are maintainer-host maintenance.
func (c *Client) refuseCI() error {
	if c.Env.Variables["GITHUB_ACTIONS"] == "true" {
		return refusal("refusing GitHub Actions: pins are maintainer-host maintenance only")
	}
	return nil
}

// signer is the primary this run enforces.
func (c *Client) signer() string {
	if c.Signer != "" {
		return strings.ToUpper(c.Signer)
	}
	return claudepin.ReviewedSignerFingerprint
}

// pinDirectory is the committed closure's directory in this repository.
func (c *Client) pinDirectory() string { return filepath.Join(c.Env.Root, PinDirectory) }

// Check re-verifies the committed closure offline: the pin, its evidence, the
// signature, the reviewed signer and the single ARG/label. It writes nothing.
func (c *Client) Check() int {
	directory := c.pinDirectory()
	if _, err := os.Stat(filepath.Join(directory, "pin.json")); err != nil {
		c.diagnose("%s is missing: there is no committed Claude selection to check",
			filepath.Join(directory, "pin.json"))
		return 1
	}
	pin, err := claudepin.Load(directory)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	verified, err := pin.Verify(pin.Version)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	if pin.Key.Fingerprint != c.signer() {
		c.diagnose("the pin names the signer %s, not the reviewed %s", pin.Key.Fingerprint,
			c.signer())
		return 1
	}
	dockerfilePath := filepath.Join(c.Env.Root, Dockerfile)
	raw, err := os.ReadFile(dockerfilePath)
	if err != nil {
		c.diagnose("%s: %s", dockerfilePath, err)
		return 1
	}
	if err := RequireSingleSlot(string(raw), pin.Version); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	c.report("claude %s: offline committed-input check (signature verified offline; "+
		"not a fresh fetch)", pin.Version)
	for _, platform := range claudepin.PlatformList() {
		pinned := verified.Pin.Executables[claudepin.Platforms[platform]]
		c.report("  %s %s %s %d", platform, pinned.Token, pinned.SHA256, pinned.Size)
	}
	c.report("  signer %s (signature created %s), manifest commit %s",
		verified.Signature.PrimaryFingerprint, verified.Signature.Created,
		verified.Manifest.Commit)
	c.report("  %s: ARG %s=%s, LABEL %s references it", Dockerfile, Argument, pin.Version, Label)
	return 0
}

// Refresh resolves, verifies and publishes one release.
func (c *Client) Refresh(ctx context.Context, options Options) int {
	version, err := claudepin.RequireVersion(options.Version, "--version")
	if err != nil {
		c.diagnose("%s", err)
		return 2
	}
	if err := c.refuseCI(); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	work, err := os.MkdirTemp(c.Env.TempRoot, "pin-claude.work.")
	if err != nil {
		c.diagnose("cannot create a work directory (%s); sources unchanged", err)
		return 1
	}
	if absolute, err := filepath.Abs(work); err == nil {
		work = absolute
	}
	staged, err := c.stage(ctx, version, work)
	if err != nil {
		_ = os.RemoveAll(work)
		c.diagnose("%s", err)
		return 1
	}
	if staged.unchanged {
		_ = os.RemoveAll(work)
		c.note("unchanged (claude %s)", version)
		return 0
	}
	_, _ = io.WriteString(c.Out, staged.delta)
	// A signal-driven rollback exits from inside the transaction, so the owner
	// scratch is named before publication starts, not after it. From here the
	// publisher owns that directory: it removes it on a decided outcome and retains
	// and prints it otherwise.
	c.diagnose("owner scratch: %s", work)
	return publication.Publish(publication.Request{
		Prefix:     Prefix,
		Root:       c.Env.Root,
		TempRoot:   c.Env.TempRoot,
		TempPrefix: "pin-claude.",
		Stderr:     publication.StderrOf(c.Err),
		Owner:      work,
		Report: publication.Report{To: c.Out, Lines: []string{
			fmt.Sprintf("published claude %s: %s", version, stringsJoined(staged.files)),
			fmt.Sprintf("now update script/test/fixtures/installer-egress/selections.tsv to "+
				"claude %s, then run: bash script/test/contracts.sh", version),
		}},
		Files: staged.files,
	})
}

func stringsJoined(files []publication.Staged) string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	sortSorted(paths)
	return strings.Join(paths, ", ")
}

func sortSorted(values []string) {
	for index := 1; index < len(values); index++ {
		for place := index; place > 0 && values[place] < values[place-1]; place-- {
			values[place], values[place-1] = values[place-1], values[place]
		}
	}
}

// isNotExist is the clean-first-adoption case: no committed pin at all.
func isNotExist(err error) bool { return errors.Is(err, os.ErrNotExist) }
