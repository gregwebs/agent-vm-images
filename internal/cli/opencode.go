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

// opencodePrepareUsage is the preparation driver.
//
// `Arguments` carries the caller's positional words, which include
// `--refresh-lock`: that word belongs to the driver's grammar, not to this flag
// set. The entry point therefore ends its own flags with `--`, so nothing a caller
// writes can be read as a flag here -- which is what stops `PREFIX VERSION --check
// other.mjs` from replacing the reviewed checker. The three paths are executed, so
// `required` is not enough and the empty check below owns their emptiness.
type opencodePrepareUsage struct {
	Contract    string   `arg:"--contract,required" placeholder:"DIR" help:"the composed helper directory run-pnpm.sh runs from"`
	Check       string   `arg:"--check,required" placeholder:"PATH" help:"the reviewed-bytes checker"`
	Pin         string   `arg:"--pin,required" placeholder:"PATH" help:"the developer-only pin writer"`
	FixtureRoot string   `arg:"--fixture-root" placeholder:"DIR" help:"authorize a non-production prefix"`
	Arguments   []string `arg:"positional" help:"-- PREFIX PLUGIN_VERSION [--refresh-lock]"`
}

func (opencodePrepareUsage) Description() string {
	return "Re-pin the reviewed OpenCode plugin inputs after a changed selection."
}

func (p *process) runOpencodePrepareLock(args []string) int {
	var usage opencodePrepareUsage
	parser, err := p.argParser("recipe-contract opencode-prepare-lock", &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	// The three reviewed inputs are paths the driver executes, so presence is not
	// enough: an empty --contract runs `run-pnpm.sh` from the caller's working
	// directory and an empty --check runs whatever `node` resolves from there.
	if flag := emptyRequiredPath(
		requiredPath{"--contract", usage.Contract},
		requiredPath{"--check", usage.Check},
		requiredPath{"--pin", usage.Pin},
	); flag != "" {
		return p.opencodeFailure(&opencode.Usage{Text: opencode.EmptyPathUsage(flag)})
	}
	options := opencode.PrepareOptions{
		Contract:    usage.Contract,
		Check:       usage.Check,
		Pin:         usage.Pin,
		FixtureRoot: usage.FixtureRoot,
		Arguments:   usage.Arguments,
		Out:         p.streams.Out,
		Err:         p.streams.Err,
	}
	if err := opencode.PrepareLock(context.Background(), options); err != nil {
		return p.opencodeFailure(err)
	}
	return 0
}

// opencodeInstallUsage is the owning install hook for the opencode layer.
type opencodeInstallUsage struct {
	Vendor          string `arg:"--vendor,required" placeholder:"DIR" help:"the read-only vendor mount holding install.sh"`
	Prefix          string `arg:"--prefix,required" placeholder:"DIR" help:"the installer's HOME"`
	Link            string `arg:"--link,required" placeholder:"PATH" help:"the command link this layer publishes"`
	OpencodeVersion string `arg:"--opencode-version,required" placeholder:"vEXACT" help:"the layer's requested opencode slot"`
	Receipt         string `arg:"--receipt" placeholder:"PATH" help:"the transport receipt the vendored installer writes"`
	SoftFail        string `arg:"--soft-fail" placeholder:"VALUE" help:"non-empty allows softening a classified transport failure"`
	StatusDir       string `arg:"--status-dir,required" placeholder:"DIR" help:"the install-status directory"`
}

func (opencodeInstallUsage) Description() string {
	return "Install the reviewed opencode command and publish the layer's link."
}

func (p *process) runOpencodeInstall(args []string) int {
	var usage opencodeInstallUsage
	parser, err := p.argParser("recipe-contract opencode-install", &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	// The prefix, the link and the record directory are created or written, so an
	// empty one would address the caller's working directory instead of the
	// container's. The entry point applies the container defaults and can never pass
	// an empty value; a direct caller can.
	if flag := emptyRequiredPath(
		requiredPath{"--vendor", usage.Vendor},
		requiredPath{"--prefix", usage.Prefix},
		requiredPath{"--link", usage.Link},
		requiredPath{"--status-dir", usage.StatusDir},
	); flag != "" {
		return p.opencodeFailure(&opencode.Usage{Text: opencode.EmptyFlagUsage("opencode-install", flag)})
	}
	options := opencode.InstallOptions{
		Vendor:    usage.Vendor,
		Prefix:    usage.Prefix,
		Link:      usage.Link,
		Slot:      usage.OpencodeVersion,
		Receipt:   usage.Receipt,
		SoftFail:  usage.SoftFail,
		StatusDir: usage.StatusDir,
		Out:       p.streams.Out,
		Err:       p.streams.Err,
	}
	if err := opencode.Install(context.Background(), options); err != nil {
		return p.opencodeFailure(err)
	}
	return 0
}

// opencodeInstallPluginsUsage is the installed-plugin install hook.
type opencodeInstallPluginsUsage struct {
	Contract      string `arg:"--contract,required" placeholder:"DIR" help:"the composed helper directory"`
	Source        string `arg:"--source,required" placeholder:"DIR" help:"the OpenCode build mount holding plugins/prepare-lock.sh"`
	Prefix        string `arg:"--prefix,required" placeholder:"DIR" help:"the installed plugin directory"`
	FixtureRoot   string `arg:"--fixture-root" placeholder:"DIR" help:"authorize a non-production prefix"`
	FixturePnpm   string `arg:"--fixture-pnpm" placeholder:"PATH" help:"the manager a fixture prefix runs"`
	PluginVersion string `arg:"--plugin-version" placeholder:"EXACT" help:"the layer's requested plugin slot"`
}

func (opencodeInstallPluginsUsage) Description() string {
	return "Install the reviewed OpenCode plugin inputs from the committed lock."
}

func (p *process) runOpencodeInstallPlugins(args []string) int {
	var usage opencodeInstallPluginsUsage
	parser, err := p.argParser("recipe-contract opencode-install-plugins", &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	// The contract and the source are executed, so presence is not enough: an empty
	// --contract runs `recipe-contract pnpm-environment` from the caller's working
	// directory, and an empty --source runs whatever `plugins/prepare-lock.sh`
	// resolves there.
	if flag := emptyRequiredPath(
		requiredPath{"--contract", usage.Contract},
		requiredPath{"--source", usage.Source},
		requiredPath{"--prefix", usage.Prefix},
	); flag != "" {
		return p.opencodeFailure(&opencode.Usage{Text: opencode.EmptyFlagUsage("opencode-install-plugins", flag)})
	}
	options := opencode.InstallPluginsOptions{
		Contract:      usage.Contract,
		Source:        usage.Source,
		Prefix:        usage.Prefix,
		FixtureRoot:   usage.FixtureRoot,
		FixturePnpm:   usage.FixturePnpm,
		PluginVersion: usage.PluginVersion,
		Out:           p.streams.Out,
		Err:           p.streams.Err,
	}
	if err := opencode.InstallPlugins(context.Background(), options); err != nil {
		return p.opencodeFailure(err)
	}
	return 0
}

// opencodeVerifyUsage is the installed-command gate for the opencode layer.
type opencodeVerifyUsage struct {
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
