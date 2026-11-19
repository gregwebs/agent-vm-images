package cli

import (
	"context"
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
type pnpmVerifyUsage struct {
	Certify bool     `arg:"--certify" help:"also prove the report, the exec targets and the policy defaults"`
	Extra   []string `arg:"positional" help:"none"`
}

func (pnpmVerifyUsage) Description() string {
	return "Audit the installed pnpm distribution and its published launcher."
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
		return p.stderrStatus(2, "pnpm-verify: usage: recipe-contract pnpm-verify [--certify]\n")
	}
	target, err := pnpm.ResolveTarget()
	if err != nil {
		return p.pnpmFailure("pnpm verification: ", err)
	}
	if err := pnpm.Verify(target, ""); err != nil {
		return p.pnpmFailure("pnpm verification: ", err)
	}
	if !usage.Certify {
		return 0
	}
	options := pnpm.CertifyOptions{Target: target, Out: p.streams.Out, Audit: p.env.Environment}
	if err := pnpm.Certify(context.Background(), options); err != nil {
		return p.pnpmFailure("pnpm verification: ", err)
	}
	return 0
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
// diagnostic is one a consumer parses, so it keeps its exact bytes.
func (p *process) pnpmFailure(prefix string, err error) int {
	var frozen *pnpm.Frozen
	if errors.As(err, &frozen) {
		return p.stderrStatus(1, frozen.Text+"\n")
	}
	return p.stderrStatus(1, prefix+err.Error()+"\n")
}
