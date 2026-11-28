// Package codewriter refreshes the one committed Codex payload closure (#97).
//
// It is the only writer of `images/tools/codex/pin.json`, of the four files under
// `images/tools/codex/evidence/` and of the `ARG AGENT_VERSION_CODEX` default in
// `images/standard/Dockerfile`. Ordinary builds only verify those bytes
// (internal/codexpin): the vendored installer never resolves a release, never
// downloads a checksum, never picks a version and never contacts Sigstore.
//
// One run resolves the exact release, downloads both maintained Linux packages with
// the original release metadata, the original `codex-package_SHA256SUMS` and both
// original legacy `.sigstore` bundles, verifies **both** payload/bundle pairs with
// the reviewed host verifier under the exact signer policy, preflights both archives
// through the production package/ELF checks, and only then publishes the six files
// as one transaction. A partial upstream failure leaves every committed byte
// identical. The tool never commits, never runs under GitHub Actions and never
// adopts a newer version by itself.
//
// Exit statuses: 0 published or unchanged, 1 failed with the sources unchanged or
// fully restored, 2 usage, 3 undecided or incomplete recovery (the retained
// directories are printed), 128+N after a signal whose rollback restored the
// sources (the six signal windows and what each retains: `internal/publication`).
package codewriter

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
	"github.com/gregwebs/agent-vm-images/internal/fetch"
	"github.com/gregwebs/agent-vm-images/internal/publication"
)

// Prefix is frozen: the owner suite and the bumper read these lines to find the
// retained recovery material, so the diagnostic is part of the tool's contract
// rather than a log.
const Prefix = "pin-codex: "

// The closure's own paths, relative to the repository.
const (
	PinDirectory      = "images/tools/codex"
	EvidenceDirectory = PinDirectory + "/evidence"
	PinFile           = PinDirectory + "/pin.json"
	Dockerfile        = "images/standard/Dockerfile"
	// Argument and Label are the Dockerfile slot and the label that reads it.
	Argument = "AGENT_VERSION_CODEX"
	Label    = "org.agent-vm.version.codex"
)

// The staged proposal's own names: the pin directory's shape plus one Dockerfile.
const (
	pinName        = "pin.json"
	metadataName   = "release.json"
	dockerfileName = "Dockerfile"
	evidenceName   = "evidence"
)

// UnownedArgs are the single-slot ARG defaults this owner does not own, masked before
// and after the staged Dockerfile so a Codex transaction cannot carry somebody else's
// edit into a commit. Claude is deliberately *not* here: it has its own owner
// transaction (ADR 0013), so an unmasked Claude ARG difference in a Codex proposal is
// a refusal rather than a masked no-op. Adding it would restore a second writer of
// that closure.
var UnownedArgs = []string{"AGENT_VERSION_OPENCODE", "AGENT_VERSION_COPILOT"}

// Fetcher is the upstream byte source. Production is internal/fetch; the suites
// substitute bytes, so no case reaches the network and the URL a pin records is still
// the one a build would fetch.
type Fetcher interface {
	Get(ctx context.Context, url string, limit int64) ([]byte, error)
}

// BlobVerifier verifies one archive against one legacy bundle under the exact signer
// policy. Production runs the reviewed cosign; a suite injects a function so no case
// needs the host tool.
type BlobVerifier func(archive, bundle, identity, issuer, work string) error

// Environment is the process view the writer needs.
type Environment struct {
	// Root is the repository to publish into.
	Root string
	// TempRoot is where work directories are created. Empty means the system
	// default.
	TempRoot string
	// Variables are the environment entries the CI refusal and the verifier's
	// executable search path read.
	Variables map[string]string
}

// Client is one writer invocation.
type Client struct {
	Env   Environment
	Fetch Fetcher
	// Verify is the payload/bundle verifier. Empty means this run resolves the
	// reviewed cosign and runs it; it is a field rather than an environment
	// variable so a plain refresh can never be redirected to another verifier.
	Verify BlobVerifier
	// Publish is the publication transaction. Production is publication.Publish; a
	// test substitutes one, because the publisher ends the process on every outcome
	// it decides.
	Publish func(publication.Request) int
	Out     io.Writer
	Err     io.Writer
	// Now is the clock `verified_on` is stamped from. A pin is written on one
	// maintainer host and read by reviewers and builders in every zone, so the stamp
	// is the UTC date, never the writer's local calendar.
	Now func() time.Time
}

// Options is one refresh request.
type Options struct {
	// Version is the exact release tag, rust-v<version>.
	Version string
	// Cosign is the reviewed host verifier, absolute. Empty means `cosign` on PATH.
	Cosign string
	// StageDir writes the fixed six-file proposal there instead of publishing.
	StageDir string
}

// New returns a client with the production byte source.
func New(env Environment, out, errOut io.Writer) *Client {
	return &Client{
		Env:     env,
		Fetch:   fetch.New(),
		Publish: publication.Publish,
		Out:     out,
		Err:     errOut,
		Now:     codexpin.UTCToday,
	}
}

// refusal is a case that failed before anything was published: a status-1 refusal
// whose message is the diagnostic.
func refusal(format string, args ...any) error {
	return codexpin.Refuse(format, args...)
}

// report writes one prefixless line: the reviewed facts, which the maintainer reads
// as the tool's answer rather than as a diagnostic.
func (c *Client) report(format string, args ...any) {
	_, _ = io.WriteString(c.Out, fmt.Sprintf(format, args...)+"\n")
}

// note writes one line the tool signs with its own prefix.
func (c *Client) note(format string, args ...any) {
	_, _ = io.WriteString(c.Out, Prefix+fmt.Sprintf(format, args...)+"\n")
}

func (c *Client) diagnose(format string, args ...any) {
	_, _ = io.WriteString(c.Err, Prefix+fmt.Sprintf(format, args...)+"\n")
}

// announce says where a directory is before a publication that may exit from a
// signal, and flushes first: anything written after the publisher returns is lost
// when a rollback exits from inside it.
func (c *Client) announce(what, directory string) {
	c.diagnose("%s %s", what, directory)
	if flusher, ok := c.Err.(interface{ Flush() error }); ok {
		_ = flusher.Flush()
	}
}

// RefuseCI is the guard every writer shares: pins are maintainer-host maintenance.
func (c *Client) refuseCI() error {
	if c.Env.Variables["GITHUB_ACTIONS"] == "true" {
		return refusal("refusing GitHub Actions: pins are maintainer-host maintenance only")
	}
	return nil
}

// searchPath is the executable search path the verifier and this process see, as the
// checkout recorded it rather than as an ambient shell redefines it.
func (c *Client) searchPath() string {
	if value := c.Env.Variables["PATH"]; value != "" {
		return value
	}
	return "/usr/bin:/bin"
}

// today is the `verified_on` stamp, from the reader's own definition of today.
func (c *Client) today() time.Time {
	if c.Now == nil {
		return codexpin.UTCToday()
	}
	now := c.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// Check validates the committed inputs, its hashes and the single ARG/label offline.
// No network, no payload archive, no cosign and no writes: this is not a fresh
// signature check, and it says so.
func (c *Client) Check() int {
	pin, err := codexpin.Load(filepath.Join(c.Env.Root, PinDirectory))
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	path := filepath.Join(c.Env.Root, Dockerfile)
	raw, err := os.ReadFile(path)
	if err != nil {
		c.diagnose("%s: %s", path, err)
		return 1
	}
	if err := RequireSingleSlot(string(raw), pin.Release); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	_, _ = io.WriteString(c.Out, pin.CheckReport())
	c.report("  %s: ARG %s=%s, LABEL %s references it", Dockerfile, Argument, pin.Release, Label)
	return 0
}

// ProbeCosign reports the exact reviewed verifier version, without any write or
// fetch. The owner bumper needs this before it resolves a single upstream tag: an
// incompatible verifier must stop the run before discovery, not after it.
func (c *Client) ProbeCosign(path string) int {
	verifier, err := c.requireCosign(path)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	c.report("cosign %s: %s", codexpin.VerifierVersion, verifier)
	return 0
}

// Refresh resolves, verifies and publishes one release. With Options.StageDir it
// writes the fixed six-file proposal there and publishes nothing.
func (c *Client) Refresh(ctx context.Context, options Options) int {
	if err := c.refuseCI(); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	verify, err := c.verifier(options.Cosign)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	release, err := codexpin.RequireRelease(options.Version, "--version")
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	work, err := os.MkdirTemp(c.Env.TempRoot, "pin-codex.work.")
	if err != nil {
		c.diagnose("cannot create a work directory (%s); sources unchanged", err)
		return 1
	}
	if work, err = absolute(work); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	proposed, err := c.stage(ctx, release, work, verify)
	if err != nil {
		_ = os.RemoveAll(work)
		c.diagnose("%s", err)
		return 1
	}
	if options.StageDir != "" {
		defer func() { _ = os.RemoveAll(work) }()
		directory, err := absolute(options.StageDir)
		if err != nil {
			c.diagnose("%s", err)
			return 1
		}
		if err := writeStage(directory, proposed.files); err != nil {
			c.diagnose("%s", err)
			return 1
		}
		if c.unchanged(proposed) {
			c.note("unchanged (%s); the staged proposal is byte-identical to the "+
				"committed files (%s)", release, directory)
		} else {
			c.note("staged %s in %s", release, directory)
		}
		return 0
	}
	if c.unchanged(proposed) {
		_ = os.RemoveAll(work)
		c.note("unchanged (%s)", release)
		return 0
	}
	directory := filepath.Join(work, "publish")
	if err := writeStage(directory, proposed.files); err != nil {
		_ = os.RemoveAll(work)
		c.diagnose("%s", err)
		return 1
	}
	// From here the publisher owns this invocation's scratch: it removes it on a
	// decided outcome and retains and prints it otherwise.
	c.announce("owner scratch:", work)
	return c.publishStaged(directory, work, nil)
}

// PublishStage publishes the fixed six-file proposal the owner bumper edited, as one
// transaction.
func (c *Client) PublishStage(stageDir string) int {
	if err := c.refuseCI(); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	// The bumper hands its own scratch here, and a maintainer may hand any directory:
	// it is read where it was named, and it is never this invocation's to remove.
	directory, err := absolute(stageDir)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	// The stage directory is the caller's: this invocation was given it and did not
	// create it, so the publisher must never remove it. Its path is announced (the
	// publisher may exit from a signal), and it is named for retention instead — the
	// publication keeps only its own work directory.
	c.announce("stage directory:", directory)
	return c.publishStaged(directory, "",
		[]publication.Retention{{Label: "staged proposal", Path: directory}})
}

// publishStaged publishes the proposal in directory. owner is a scratch directory
// this invocation created, which the publisher disposes of and, when the outcome is
// undecided, prints beside its own work directory. retain names directories the
// caller must keep but this invocation did not create.
func (c *Client) publishStaged(directory, owner string, retain []publication.Retention) int {
	files, err := readStage(directory, stageNames())
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	c.diagnose("publishing the staged proposal in %s", directory)
	// The production loader runs over a private copy of exactly the bytes read
	// above, so a directory swapped under the path between the check and the
	// publication cannot substitute a different proposal.
	pin, err := c.loadProposal(files)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	stagedDockerfile, err := decodeStaged(files, dockerfileName)
	if err != nil {
		c.diagnose("%s", err)
		return 1
	}
	if err := RequireSingleSlot(stagedDockerfile, pin.Release); err != nil {
		c.diagnose("%s", err)
		return 1
	}
	committedPath := filepath.Join(c.Env.Root, Dockerfile)
	committedDockerfile, err := os.ReadFile(committedPath)
	if err != nil {
		c.diagnose("%s: %s", committedPath, err)
		return 1
	}
	if !slices.Equal(unownedView(stagedDockerfile), unownedView(string(committedDockerfile))) {
		c.diagnose("%s", refusal("the staged %s changes more than the single-slot "+
			"version defaults", Dockerfile))
		return 1
	}
	staged := make([]publication.Staged, 0, len(files))
	for _, file := range files {
		staged = append(staged, publication.Staged{Path: destinationOf(file.name), Raw: file.raw})
	}
	return c.Publish(publication.Request{
		Prefix:     Prefix,
		Root:       c.Env.Root,
		TempRoot:   c.Env.TempRoot,
		TempPrefix: "pin-codex.work.",
		Stderr:     publication.StderrOf(c.Err),
		Owner:      owner,
		Retain:     retain,
		Files:      staged,
	})
}
