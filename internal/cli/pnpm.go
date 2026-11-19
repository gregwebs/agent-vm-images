package cli

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/pnpm"
)

// pnpmBootstrapUsage describes the base pnpm bootstrap: the only path that puts
// the reviewed distribution into an image.
type pnpmBootstrapUsage struct {
	ValidateFixture string   `arg:"--validate-fixture" placeholder:"DIR" help:"check a fixture image root and exit"`
	FixtureArchive  string   `arg:"--fixture-archive" placeholder:"PATH" help:"absolute archive bytes to authenticate instead of downloading"`
	Arguments       []string `arg:"positional" help:"none: the pin and its mounts are the only inputs"`
}

func (pnpmBootstrapUsage) Description() string {
	return "Authenticate and publish the pinned pnpm distribution into the image."
}

// pnpmVerifyUsage describes the base pnpm certification. Without --certify it is
// the distribution audit alone, which is what the dsh hook re-runs after its own
// health gate; --certify adds the exact report, the exec-target access audit and
// the five policy defaults.
//
// The three path flags let a caller authenticate a tree it holds instead of the
// image's fixed paths — the shared project module hands the host manager an
// installation outside /opt, and the audit is the same audit. They are not a
// fixture seam: the fixture image root is still named by the environment the
// recipes mount.
type pnpmVerifyUsage struct {
	Certify     bool     `arg:"--certify" help:"also prove the report, the exec targets and the policy defaults"`
	Prefix      string   `arg:"--prefix" placeholder:"DIR" help:"the installed distribution prefix"`
	Commands    string   `arg:"--commands" placeholder:"DIR" help:"the directory the launcher lives in"`
	Node        string   `arg:"--node" placeholder:"PATH" help:"the interpreter the launcher execs"`
	Source      string   `arg:"--source" placeholder:"DIR" help:"the reviewed pin and launcher directory"`
	FixtureRoot string   `arg:"--fixture-root" placeholder:"DIR" help:"the invocation-owned root the tree lives in"`
	Extra       []string `arg:"positional" help:"none"`
}

func (pnpmVerifyUsage) Description() string {
	return "Audit the installed pnpm distribution and its published launcher."
}

// pnpmExtractUsage describes the host acquisition boundary: authenticate the
// pinned archive and unpack it, without the image's publication layout.
type pnpmExtractUsage struct {
	Archive     string `arg:"--archive" placeholder:"PATH" help:"absolute archive bytes to authenticate instead of downloading"`
	Download    bool   `arg:"--download" help:"fetch the pin's own URL through internal/fetch"`
	Destination string `arg:"--destination,required" placeholder:"DIR" help:"the absolute, existing directory to unpack into"`
	Source      string `arg:"--source" placeholder:"DIR" help:"the reviewed pin directory"`
}

func (pnpmExtractUsage) Description() string {
	return "Authenticate the pinned pnpm archive and unpack it for a host caller."
}

// pnpmPinVersionUsage describes the version reader the dsh hooks use to compare a
// base pin with the selection they assert.
type pnpmPinVersionUsage struct {
	Pin string `arg:"positional,required" help:"the pin file to read"`
}

func (pnpmPinVersionUsage) Description() string {
	return "Print the pnpm version one pin selects."
}

// runPnpmBootstrap installs the distribution, or -- with --validate-fixture --
// only authorizes the fixture root it would publish into. The validate mode is
// what lets a suite prove the escape refusals without publishing anything.
func (p *process) runPnpmBootstrap(args []string) int {
	var usage pnpmBootstrapUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract pnpm-bootstrap"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	// The predecessor accepted no positional argument at all -- its pin and its
	// mounts are the whole input -- and refused one rather than reporting usage.
	if len(usage.Arguments) > 0 || (usage.ValidateFixture != "" && usage.FixtureArchive != "") {
		return p.pnpmFailure("pnpm bootstrap: ", errors.New("no production positional arguments supported"))
	}
	if usage.ValidateFixture != "" {
		if _, _, err := pnpm.FixtureDestination(usage.ValidateFixture); err != nil {
			return p.pnpmFailure("pnpm bootstrap: ", err)
		}
		return 0
	}
	options := pnpm.Options{FixtureArchive: usage.FixtureArchive}
	if err := pnpm.Bootstrap(context.Background(), options); err != nil {
		return p.pnpmFailure("pnpm bootstrap: ", err)
	}
	return 0
}

// runPnpmVerify is the distribution audit, and with --certify the whole
// certification images/pnpm/verify-pnpm.sh used to perform.
func (p *process) runPnpmVerify(args []string) int {
	var usage pnpmVerifyUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract pnpm-verify"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	if len(usage.Extra) > 0 {
		return p.stderrStatus(2, pnpmVerifyUsageText)
	}
	// A caller either names the whole tree or none of it: mixing a caller's prefix
	// with an image default would audit a tree that does not exist.
	named := usage.Prefix != "" || usage.Commands != "" || usage.Node != ""
	complete := usage.Prefix != "" && usage.Commands != "" && usage.Node != ""
	if (named && !complete) || (usage.FixtureRoot != "" && !complete) {
		return p.stderrStatus(2, pnpmVerifyUsageText)
	}
	target, err := pnpm.ResolveTarget()
	if named {
		target = pnpm.ExplicitTarget(usage.Prefix, usage.Commands, usage.Node, usage.FixtureRoot)
		err = nil
	}
	if err != nil {
		return p.pnpmFailure("pnpm verification: ", err)
	}
	options := pnpm.VerifyOptions{Target: target, Source: usage.Source, PathIdentity: true}
	if err := pnpm.Verify(options); err != nil {
		return p.pnpmFailure("pnpm verification: ", err)
	}
	if !usage.Certify {
		return 0
	}
	certify := pnpm.CertifyOptions{Target: target, Source: usage.Source, Out: p.streams.Out, Audit: p.env.Environment}
	if err := pnpm.Certify(context.Background(), certify); err != nil {
		return p.pnpmFailure("pnpm verification: ", err)
	}
	return 0
}

const pnpmVerifyUsageText = "pnpm-verify: usage: recipe-contract pnpm-verify [--certify]" +
	" [--prefix DIR --commands DIR --node PATH] [--source DIR] [--fixture-root DIR]\n"

// runPnpmExtract authenticates the pinned archive and unpacks it for a host
// caller, printing the two absolute paths as one JSON object. It is the
// acquisition boundary the maintenance scripts use, so the pin, digest, member and
// package rules are the image's own rather than a second copy.
func (p *process) runPnpmExtract(args []string) int {
	var usage pnpmExtractUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract pnpm-extract"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	options := pnpm.ExtractOptions{
		Source:      usage.Source,
		Archive:     usage.Archive,
		Download:    usage.Download,
		Destination: usage.Destination,
	}
	unpacked, err := pnpm.Extract(context.Background(), options)
	if err != nil {
		return p.pnpmFailure("pnpm extract: ", err)
	}
	report, err := json.Marshal(struct {
		Package string `json:"package"`
		Archive string `json:"archive"`
	}{Package: unpacked.Package, Archive: unpacked.Archive})
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	return p.stdoutStatus(string(report) + "\n")
}

// runPnpmPinVersion prints the version one pin document names, which is what the
// dsh hooks compare with the selection their manifest asserts.
func (p *process) runPnpmPinVersion(args []string) int {
	var usage pnpmPinVersionUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract pnpm-pin-version"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	version, err := pnpm.PinVersion(usage.Pin)
	if err != nil {
		return p.pnpmFailure("pnpm-pin-version: ", err)
	}
	return p.stdoutStatus(version + "\n")
}

// pnpmFailure reports a refusal with the program's own prefix. A frozen
// diagnostic is one a consumer parses, so it keeps its exact bytes; a child that
// failed keeps the status and the stderr the shell entry point produced.
func (p *process) pnpmFailure(prefix string, err error) int {
	var frozen *pnpm.Frozen
	if errors.As(err, &frozen) {
		return p.stderrStatus(1, frozen.Text+"\n")
	}
	var child *pnpm.ChildFailure
	if errors.As(err, &child) {
		// The child's own bytes, verbatim: the shell inherited them, so this does
		// not add a prefix or a newline.
		if child.Stderr != "" {
			if writeErr := p.err.writeString(child.Stderr); writeErr != nil {
				return streamFailureStatus
			}
		}
		return child.Status
	}
	return p.stderrStatus(1, prefix+err.Error()+"\n")
}
