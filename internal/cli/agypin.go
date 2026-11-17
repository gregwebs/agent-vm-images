package cli

import (
	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/agypin"
)

// agyPinUsage describes the agy subcommand: the offline reader the agy layer's two
// hooks use. Nothing here resolves a release, downloads a byte or writes a pin.
type agyPinUsage struct {
	Verb    string `arg:"positional,required" help:"check or installed"`
	PinDir  string `arg:"--pin-dir" help:"the committed agy pin's directory"`
	Bin     string `arg:"--bin" help:"the installed agy command, for installed"`
	Version string `arg:"--version" help:"the exact agy version the layer pins"`
	Arch    string `arg:"--arch" help:"amd64 or arm64"`
	Owner   string `arg:"--owner" help:"the expected owner of the installed command, as UID:GID"`
}

func (agyPinUsage) Description() string {
	return "Read the committed agy pin and the installed agy command offline."
}

// agyPinVerbs is each verb's argument set: a flag that does not belong to the verb is
// a malformed invocation, not something to ignore.
var agyPinVerbs = map[string]agyPinVerb{
	"check":     {required: []string{"pin-dir", "version", "arch"}, allowed: []string{"pin-dir", "version", "arch"}},
	"installed": {required: []string{"bin", "owner"}, allowed: []string{"bin", "owner"}},
}

// agyPinVerb is one verb's argument contract.
type agyPinVerb struct {
	required []string
	allowed  []string
}

// agyPinFlags is every flag name one verb may carry.
var agyPinFlags = []string{"pin-dir", "bin", "version", "arch", "owner"}

func (p *process) runAgyPin(args []string) int {
	var usage agyPinUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract agy-pin"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	spec, known := agyPinVerbs[usage.Verb]
	if !known {
		return p.agyUsage("unknown verb " + usage.Verb)
	}
	given := map[string]string{
		"pin-dir": usage.PinDir,
		"bin":     usage.Bin,
		"version": usage.Version,
		"arch":    usage.Arch,
		"owner":   usage.Owner,
	}
	for _, flag := range agyPinFlags {
		if given[flag] == "" || contains(spec.allowed, flag) {
			continue
		}
		return p.agyUsage(usage.Verb + " does not take --" + flag)
	}
	for _, flag := range spec.required {
		if given[flag] == "" {
			return p.agyUsage(usage.Verb + " needs --" + flag)
		}
	}
	if usage.Verb == "installed" {
		uid, gid, err := parseOwner(usage.Owner)
		if err != nil {
			return p.agyFailure(err)
		}
		if err := agypin.CheckInstalled(usage.Bin, uid, gid); err != nil {
			return p.agyFailure(err)
		}
		return p.stdoutStatus("agy: " + usage.Bin + " carries the reviewed mode and " +
			"owner and still names " + agypin.Marker + "\n")
	}
	version, err := agypin.CheckSelection(usage.PinDir, usage.Version, usage.Arch)
	if err != nil {
		return p.agyFailure(err)
	}
	return p.stdoutStatus("agy " + version.String() + ": the committed pin selects this " +
		"version for " + usage.Arch + "\n")
}

// agyFailure reports a refusal with the reader's own prefix, which the suites and
// the hooks match.
func (p *process) agyFailure(err error) int {
	return p.stderrStatus(1, "agy_pin: "+err.Error()+"\n")
}

// agyUsage reports a malformed invocation: status 2, like the other helpers.
func (p *process) agyUsage(message string) int {
	return p.stderrStatus(2, "agy_pin: "+message+"\n")
}
