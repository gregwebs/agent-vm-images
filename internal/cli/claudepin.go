package cli

import (
	"errors"
	"strconv"
	"strings"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/internal/claudepin"
)

// claudePinUsage describes the Claude pin subcommand: the offline reader the
// Claude hooks, the vendored installer and the build gate all use.
type claudePinUsage struct {
	Verb     string `arg:"positional,required" help:"version, digest, check, select, preflight, installed or native-state"`
	PinDir   string `arg:"--pin-dir" help:"the committed closure's directory"`
	Version  string `arg:"--version" help:"the exact Claude Code version"`
	Platform string `arg:"--platform" help:"linux-x64 or linux-arm64"`
	Prefix   string `arg:"--prefix" help:"the install prefix (the native installer's HOME)"`
	Owner    string `arg:"--owner" help:"the expected owner of the installed payload, as UID:GID"`
	File     string `arg:"positional" help:"the downloaded executable, for preflight"`
}

func (claudePinUsage) Description() string {
	return "Read the committed Claude native payload closure offline."
}

// runClaudePin is the only reader of images/tools/claude/pin.json. Every verb is
// offline: nothing here resolves a release, downloads a byte or writes a pin.
func (p *process) runClaudePin(args []string) int {
	var usage claudePinUsage
	parser, err := arg.NewParser(arg.Config{Program: "recipe-contract claude-pin"}, &usage)
	if err != nil {
		return p.reportSynchronousFailure(err)
	}
	if err := parser.Parse(args); err != nil {
		return p.argFailure(parser, err)
	}
	if usage.Verb == "native-state" {
		if usage.Prefix == "" {
			return p.claudeUsage("native-state needs --prefix")
		}
		if err := claudepin.CheckNativeState(usage.Prefix); err != nil {
			return p.claudeFailure(err)
		}
		return p.stdoutStatus("claude: native state at " + usage.Prefix +
			" matches the offline install\n")
	}
	if usage.PinDir == "" {
		return p.claudeUsage(usage.Verb + " needs --pin-dir")
	}
	pin, err := claudepin.Load(usage.PinDir)
	if err != nil {
		return p.claudeFailure(err)
	}
	switch usage.Verb {
	case "version":
		return p.stdoutStatus(pin.Version + "\n")
	case "digest":
		if usage.Platform == "" {
			return p.claudeUsage("digest needs --platform")
		}
		pinned, err := pin.Executable(usage.Platform)
		if err != nil {
			return p.claudeFailure(err)
		}
		return p.stdoutStatus(pinned.SHA256 + "\n")
	case "check", "select", "preflight", "installed":
	default:
		return p.claudeUsage("unknown verb " + usage.Verb)
	}
	if usage.Version == "" {
		return p.claudeUsage(usage.Verb + " needs --version")
	}
	verified, err := pin.Verify(usage.Version)
	if err != nil {
		return p.claudeFailure(err)
	}
	if usage.Verb == "check" {
		return p.stdoutStatus(claudepin.CheckReport(verified))
	}
	if usage.Platform == "" {
		return p.claudeUsage(usage.Verb + " needs --platform")
	}
	pinned, err := pin.Executable(usage.Platform)
	if err != nil {
		return p.claudeFailure(err)
	}
	switch usage.Verb {
	case "select":
		return p.stdoutStatus(pinned.URL + " " + pinned.SHA256 + " " +
			strconv.FormatInt(pinned.Size, 10) + "\n")
	case "preflight":
		if usage.File == "" {
			return p.claudeUsage("preflight needs the downloaded file")
		}
		if err := verified.Preflight(usage.Platform, usage.File); err != nil {
			return p.claudeFailure(err)
		}
		return p.stdoutStatus("claude " + pin.Version + " " + pinned.Platform +
			": downloaded executable matches the committed pin and is an executable " +
			"for this machine\n")
	default: // installed
		if usage.Prefix == "" || usage.Owner == "" {
			return p.claudeUsage("installed needs --prefix and --owner")
		}
		uid, gid, err := parseOwner(usage.Owner)
		if err != nil {
			return p.claudeFailure(err)
		}
		if err := verified.CheckInstalled(usage.Platform, usage.Prefix, uid, gid); err != nil {
			return p.claudeFailure(err)
		}
		return p.stdoutStatus("claude " + pin.Version + " " + pinned.Platform +
			": installed payload, launcher and layout match the committed pin\n")
	}
}

// parseOwner reads the UID:GID the installed layout must carry.
func parseOwner(text string) (int, int, error) {
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return 0, 0, claudepin.Refuse("--owner must be UID:GID, found %q", text)
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil || uid < 0 {
		return 0, 0, claudepin.Refuse("--owner must be UID:GID, found %q", text)
	}
	gid, err := strconv.Atoi(parts[1])
	if err != nil || gid < 0 {
		return 0, 0, claudepin.Refuse("--owner must be UID:GID, found %q", text)
	}
	return uid, gid, nil
}

// claudeFailure reports a refusal with the reader's own prefix, which the suites and
// the hooks match.
func (p *process) claudeFailure(err error) int {
	if errors.Is(err, arg.ErrHelp) {
		return 0
	}
	return p.stderrStatus(1, "claude_pin: "+err.Error()+"\n")
}

// claudeUsage reports a malformed invocation: status 2, like the other helpers.
func (p *process) claudeUsage(message string) int {
	return p.stderrStatus(2, "claude_pin: "+message+"\n")
}
