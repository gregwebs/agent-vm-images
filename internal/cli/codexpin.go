package cli

import (
	"errors"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/codexpin"
)

// codexPinUsage describes the Codex pin subcommand: the offline reader the Codex
// install hook and the vendored installer both use.
type codexPinUsage struct {
	Verb    string `arg:"positional,required" help:"check, select or preflight"`
	PinDir  string `arg:"--pin-dir" help:"the committed closure's directory"`
	Release string `arg:"--release" help:"the exact release tag, rust-v<version>"`
	Target  string `arg:"--target" help:"the upstream target, x86_64- or aarch64-unknown-linux-musl"`
	Archive string `arg:"--archive" help:"the downloaded package archive, for preflight"`
}

func (codexPinUsage) Description() string {
	return "Read the committed Codex payload closure offline."
}

// codexPinVerbs is each verb's argument set, decided before anything is read: a flag
// that does not belong to the verb is a malformed invocation, not something to
// ignore.
var codexPinVerbs = map[string]struct {
	required []string
	allowed  []string
}{
	"check":     {required: []string{"pin-dir", "release"}, allowed: []string{"pin-dir", "release"}},
	"select":    {required: []string{"pin-dir", "release", "target"}, allowed: []string{"pin-dir", "release", "target"}},
	"preflight": {required: []string{"pin-dir", "release", "target", "archive"}, allowed: []string{"pin-dir", "release", "target", "archive"}},
}

// runCodexPin is the only reader of images/tools/codex/pin.json. Every verb is
// offline: nothing here resolves a release, downloads a byte or writes a pin, and
// no verb repeats the signature check the maintenance host did once.
func (p *process) runCodexPin(args []string) int {
	var usage codexPinUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract codex-pin"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	spec, known := codexPinVerbs[usage.Verb]
	if !known {
		return p.codexUsage("unknown verb " + usage.Verb)
	}
	given := map[string]string{
		"pin-dir": usage.PinDir,
		"release": usage.Release,
		"target":  usage.Target,
		"archive": usage.Archive,
	}
	for _, flag := range codexPinFlags {
		if given[flag] == "" || contains(spec.allowed, flag) {
			continue
		}
		return p.codexUsage(usage.Verb + " does not take --" + flag)
	}
	for _, flag := range spec.required {
		if given[flag] == "" {
			return p.codexUsage(usage.Verb + " needs --" + flag)
		}
	}
	// The release is validated before the closure is read, and compared with it
	// before any verb runs: an exact but unreviewed tag is refused by `check` too.
	release, err := codexpin.RequireRelease(usage.Release, "requested release")
	if err != nil {
		return p.codexFailure(err)
	}
	pin, err := codexpin.Load(usage.PinDir)
	if err != nil {
		return p.codexFailure(err)
	}
	if err := pin.RequireRequested(release); err != nil {
		return p.codexFailure(err)
	}
	if usage.Verb == "check" {
		return p.stdoutStatus(pin.CheckReport())
	}
	target, err := codexpin.RequireTarget(usage.Target, "requested target")
	if err != nil {
		return p.codexFailure(err)
	}
	selected, err := pin.Selection(usage.Release, target)
	if err != nil {
		return p.codexFailure(err)
	}
	if usage.Verb == "select" {
		// Four newline-delimited fields the vendored installer parses
		// positionally: version, asset, url, digest. Frozen.
		return p.stdoutStatus(pin.Version + "\n" + selected.Asset + "\n" +
			selected.URL + "\n" + selected.SHA256 + "\n")
	}
	if err := codexpin.Preflight(pin, selected, usage.Archive); err != nil {
		return p.codexFailure(err)
	}
	return p.stdoutStatus("codex " + pin.Version + " " + selected.Platform +
		": committed identity, package metadata and ELF machine verified before extraction\n")
}

// codexPinFlags is every flag name one verb may carry.
var codexPinFlags = []string{"pin-dir", "release", "target", "archive"}

// codexFailure reports a refusal with the reader's own prefix, which the hooks and
// the vendored installer match.
func (p *process) codexFailure(err error) int {
	if errors.Is(err, arg.ErrHelp) {
		return 0
	}
	return p.stderrStatus(1, "codex_pin: "+err.Error()+"\n")
}

// codexUsage is an invalid invocation: status 2, like every other verb.
func (p *process) codexUsage(message string) int {
	return p.stderrStatus(2, "codex_pin: "+message+"\n")
}
