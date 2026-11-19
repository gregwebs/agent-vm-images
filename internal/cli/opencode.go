package cli

import (
	"context"
	"errors"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/opencode"
)

// opencodeFailure reports one OpenCode hook outcome with the status its
// predecessor used: a malformed argument is usage (2), a well-formed request the
// program refuses is 1, a child that failed keeps its own status and its own
// bytes, and a signal keeps 130/143/129.
//
// There is no prefix parameter: the predecessors did not share one, so each
// message carries its whole line.
func (p *process) opencodeFailure(err error) int {
	var usage *opencode.Usage
	if errors.As(err, &usage) {
		// An empty text is the predecessor's `exit 2` with no diagnostic at all.
		if usage.Text == "" {
			return 2
		}
		return p.stderrStatus(2, usage.Text+"\n")
	}
	var interrupted *opencode.Interrupted
	if errors.As(err, &interrupted) {
		return interrupted.Status
	}
	var refused *opencode.Refusal
	if errors.As(err, &refused) {
		return p.stderrStatus(1, refused.Text+"\n")
	}
	var silent *opencode.Silent
	if errors.As(err, &silent) {
		// The child reported its own refusal and the predecessor added nothing.
		return silent.Status
	}
	return p.stderrStatus(1, err.Error()+"\n")
}

// opencodePrepareFlags are the driver's own inputs, separated from the caller's
// argv.
//
// They are parsed by hand rather than with go-arg because the positional words
// belong to the driver's grammar: `--refresh-lock` is one of them, and a flag
// parser refuses an option it does not declare before the driver ever sees it.
// Everything after these four reaches the driver unchanged, so a duplicate or a
// typo is that grammar's usage refusal -- which is the status and the silence the
// retired script produced.
type opencodePrepareFlags struct {
	Contract    string
	Check       string
	Pin         string
	FixtureRoot string
	Arguments   []string
}

const opencodePrepareFlagsUsage = "opencode-prepare-lock: usage: recipe-contract opencode-prepare-lock" +
	" --contract DIR --check PATH --pin PATH [--fixture-root DIR] PREFIX PLUGIN_VERSION [--refresh-lock]"

func parsePrepareFlags(args []string) (opencodePrepareFlags, error) {
	var flags opencodePrepareFlags
	for index := 0; index < len(args); index++ {
		name := args[index]
		switch name {
		case "--contract", "--check", "--pin", "--fixture-root":
			if index+1 >= len(args) {
				return opencodePrepareFlags{}, &opencode.Usage{Text: opencodePrepareFlagsUsage}
			}
			value := args[index+1]
			index++
			// An empty fixture root is a value, not a missing one: the entry point
			// passes the variable whether or not it is set, and unset means
			// "production only".
			if value == "" && name != "--fixture-root" {
				return opencodePrepareFlags{}, &opencode.Usage{Text: opencodePrepareFlagsUsage}
			}
			switch name {
			case "--contract":
				flags.Contract = value
			case "--check":
				flags.Check = value
			case "--pin":
				flags.Pin = value
			case "--fixture-root":
				flags.FixtureRoot = value
			}
		default:
			flags.Arguments = append(flags.Arguments, name)
		}
	}
	if flags.Contract == "" || flags.Check == "" || flags.Pin == "" {
		return opencodePrepareFlags{}, &opencode.Usage{Text: opencodePrepareFlagsUsage}
	}
	return flags, nil
}

func (p *process) runOpencodePrepareLock(args []string) int {
	flags, err := parsePrepareFlags(args)
	if err != nil {
		return p.opencodeFailure(err)
	}
	options := opencode.PrepareOptions{
		Contract:    flags.Contract,
		Check:       flags.Check,
		Pin:         flags.Pin,
		FixtureRoot: flags.FixtureRoot,
		Arguments:   flags.Arguments,
		Out:         p.streams.Out,
		Err:         p.streams.Err,
	}
	if err := opencode.PrepareLock(context.Background(), options); err != nil {
		return p.opencodeFailure(err)
	}
	return 0
}

// opencodeVerifyUsage is the installed-command gate for the opencode layer.
type opencodeVerifyUsage struct {
	Contract        string `arg:"--contract,required" placeholder:"DIR" help:"the authorized composed helper directory"`
	Prefix          string `arg:"--prefix,required" placeholder:"DIR" help:"the installer's HOME holding .opencode"`
	OpencodeVersion string `arg:"--opencode-version" placeholder:"vEXACT" help:"the layer's requested opencode slot"`
	StatusDir       string `arg:"--status-dir,required" placeholder:"DIR" help:"the install-status directory"`
}

func (opencodeVerifyUsage) Description() string {
	return "Gate the installed opencode command on its exact report and access."
}

func (p *process) runOpencodeVerify(args []string) int {
	var usage opencodeVerifyUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract opencode-verify"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	options := opencode.VerifyOptions{
		Contract:  usage.Contract,
		Prefix:    usage.Prefix,
		Slot:      usage.OpencodeVersion,
		StatusDir: usage.StatusDir,
		Access:    p.env.Environment,
		Out:       p.streams.Out,
		Err:       p.streams.Err,
	}
	if err := opencode.Verify(context.Background(), options); err != nil {
		return p.opencodeFailure(err)
	}
	return 0
}

// opencodePluginsUsage is the installed-plugin gate. Every value the container
// exports is a flag here, so the entry point is the only place the environment is
// read.
type opencodePluginsUsage struct {
	Source         string `arg:"--source,required" placeholder:"DIR" help:"the OpenCode build mount"`
	Contract       string `arg:"--contract,required" placeholder:"DIR" help:"the authorized composed helper directory"`
	Prefix         string `arg:"--prefix,required" placeholder:"DIR" help:"the installed plugin prefix"`
	Claude         string `arg:"--claude,required" placeholder:"PATH" help:"the Claude command the plugin hands work to"`
	Opencode       string `arg:"--opencode,required" placeholder:"PATH" help:"the installed opencode command"`
	Config         string `arg:"--config,required" placeholder:"PATH" help:"the image's OPENCODE_CONFIG file"`
	OpencodeConfig string `arg:"--opencode-config" placeholder:"PATH" help:"the guest's own OPENCODE_CONFIG value"`
	PluginVersion  string `arg:"--plugin-version" placeholder:"EXACT" help:"the layer's requested plugin slot"`
	StatusDir      string `arg:"--status-dir,required" placeholder:"DIR" help:"the install-status directory"`
}

func (opencodePluginsUsage) Description() string {
	return "Gate the installed OpenCode Claude plugin on its payload, runtime and access."
}

func (p *process) runOpencodeVerifyPlugins(args []string) int {
	var usage opencodePluginsUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract opencode-verify-plugins"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	options := opencode.PluginsOptions{
		Source:         usage.Source,
		Contract:       usage.Contract,
		Prefix:         usage.Prefix,
		Claude:         usage.Claude,
		Opencode:       usage.Opencode,
		Config:         usage.Config,
		OpenCodeConfig: usage.OpencodeConfig,
		Slot:           usage.PluginVersion,
		StatusDir:      usage.StatusDir,
		Access:         p.env.Environment,
		Out:            p.streams.Out,
		Err:            p.streams.Err,
	}
	if err := opencode.VerifyPlugins(context.Background(), options); err != nil {
		return p.opencodeFailure(err)
	}
	return 0
}
