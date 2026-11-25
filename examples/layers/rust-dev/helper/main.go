// Command rust-dev-helper is the rust-dev example layer's own program.
//
// ADR 0017 (example layers stay self-contained) makes this a module of its own,
// compiled by the layer's pinned `golang:` stage and mounted into the RUN that
// uses it: an example build has no `--build-context`, so nothing from the host or
// from ./internal can reach it. Slice 1 of #187 moved the one verb no other
// program imports; slice 2 added the two offline reads; slice 3 (#231) adds the
// downloading installer, so zero Python runs in the rust-dev image build. The
// Python loader it replaced is deleted (#245): this program is the only reader.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/alexflint/go-arg"

	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/internal/gpg"
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/kacheprobe"
	"github.com/gregwebs/agent-vm-images/examples/layers/rust-dev/helper/toolchain"
)

// prefix is the diagnostic prefix the kache-probe verb shares with the Python
// script it replaced: the verifier's log lines and the shell suites match on it.
const prefix = kacheprobe.Prefix

// toolchainPrefix is the frozen prefix the moved reader verbs keep: the shell
// suites and the pins suite match `rust_toolchain: ` on every refusal.
const toolchainPrefix = "rust_toolchain"

type arguments struct {
	KacheProbe      *kacheProbeCommand      `arg:"subcommand:kache-probe" help:"decide a kache doctor --probe --json report"`
	Check           *checkCommand           `arg:"subcommand:check" help:"verify the committed pin data offline"`
	Install         *installCommand         `arg:"subcommand:install" help:"fetch, verify and install the pinned toolchain"`
	VerifyInstalled *verifyInstalledCommand `arg:"subcommand:verify-installed" help:"verify the committed pin data and the installed toolchain"`
	LogLevel        string                  `arg:"--log-level" default:"warn" placeholder:"LEVEL" help:"debug, info, warn or error; diagnostics go to stderr"`
	// The fixture grammar is embedded: in the shipped build the type is empty, so
	// the parser has no fixture flag to accept or to refuse (see seams_shipped.go).
	fixtureArguments
}

func (arguments) Description() string {
	return "rust-dev example layer helper. Runs from the layer's own build stages."
}

type kacheProbeCommand struct {
	Report string `arg:"positional,required" placeholder:"REPORT" help:"the report file kache wrote"`
}

func (kacheProbeCommand) Description() string {
	return "Refuse a kache doctor report that fails, or is missing a required finding."
}

type checkCommand struct {
	InputDir string `arg:"positional,required" placeholder:"INPUT_DIR" help:"the committed pin directory"`
	// A pointer, so `--expect-channel ''` is an assertion that fails rather than
	// "unset": the source reader's argparse default is None and it compares an
	// empty string like any other.
	ExpectChannel *string `arg:"--expect-channel" placeholder:"EXACT" help:"assert this exact X.Y.Z channel"`
}

func (checkCommand) Description() string {
	return "Verify the committed pin data offline."
}

type verifyInstalledCommand struct {
	InputDir      string                 `arg:"positional,required" placeholder:"INPUT_DIR" help:"the committed pin directory"`
	Arch          toolchain.Architecture `arg:"--arch,required" placeholder:"AMD64|ARM64" help:"the installed architecture"`
	ExpectChannel string                 `arg:"--expect-channel,required" placeholder:"EXACT" help:"assert this exact X.Y.Z channel"`
	RustupHome    string                 `arg:"--rustup-home,required" placeholder:"DIR" help:"the installed RUSTUP_HOME"`
	CargoHome     string                 `arg:"--cargo-home,required" placeholder:"DIR" help:"the installed CARGO_HOME"`
}

func (verifyInstalledCommand) Description() string {
	return "Verify the committed pin data and the installed toolchain."
}

type installCommand struct {
	InputDir      string                 `arg:"positional,required" placeholder:"INPUT_DIR" help:"the committed pin directory"`
	Arch          toolchain.Architecture `arg:"--arch,required" placeholder:"AMD64|ARM64" help:"the installed architecture"`
	ExpectChannel string                 `arg:"--expect-channel,required" placeholder:"EXACT" help:"assert this exact X.Y.Z channel"`
	RustupHome    string                 `arg:"--rustup-home,required" placeholder:"DIR" help:"the RUSTUP_HOME to create"`
	CargoHome     string                 `arg:"--cargo-home,required" placeholder:"DIR" help:"the CARGO_HOME to create"`
	Events        *string                `arg:"--events" placeholder:"FILE" help:"append the frozen operation log to FILE"`
	WorkDir       *string                `arg:"--work-dir" placeholder:"DIR" help:"an existing directory for the private dist layout"`
}

func (installCommand) Description() string {
	return "Fetch, verify and install the pinned toolchain."
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run maps every outcome to the source reader's status: 0 success, 1 refusal,
// and 2 for a malformed invocation of a verb that exists here.
func run(args []string) (status int) {
	defer func() {
		// A panic must not read as a usage error (status 2): the verifier treats a
		// non-zero status as a failed build either way, but a wrong *status* would
		// tell the suites the wrong story about which rule fired.
		if recovered := recover(); recovered != nil {
			status = refuseToolchain(fmt.Errorf("internal error: %v", recovered))
		}
	}()
	var parsed arguments
	parser, err := arg.NewParser(arg.Config{Program: "rust-dev-helper", IgnoreEnv: true},
		&parsed)
	if err != nil {
		return refuse(err.Error())
	}
	if err := parser.Parse(args); err != nil {
		// The verb comes from the subcommand chain go-arg recorded before it
		// reported the error: a malformed invocation of `check` or
		// `verify-installed` is argparse's status 2, and everything else is this
		// binary's one refusal status, which is slice 1's contract.
		if isToolchainVerb(parser.SubcommandNames()) {
			return refuseUsage(toolchainPrefix)
		}
		return refuse("usage: " + usageText)
	}
	if parsed.fixtureSeamsRequested() {
		for _, seam := range fixtureSeams {
			fmt.Println(seam)
		}
		return 0
	}
	configureLogging(parsed.LogLevel)
	trust, err := parsed.trust()
	if err != nil {
		return refuseToolchain(err)
	}
	switch {
	case parsed.Check != nil:
		return runCheck(parsed.Check, trust)
	case parsed.Install != nil:
		return runInstall(parsed.Install, trust, parsed.fixtureArguments)
	case parsed.VerifyInstalled != nil:
		return runVerifyInstalled(parsed.VerifyInstalled, trust)
	case parsed.KacheProbe != nil:
		return kacheprobe.Decide(parsed.KacheProbe.Report, os.Stdout, os.Stderr)
	default:
		// No verb: slice 1's contract, a usage line and status 1.
		return refuse("usage: " + usageText)
	}
}

// runCheck runs the offline read and prints the channel.
func runCheck(command *checkCommand, trust toolchain.TrustPolicy) int {
	program, err := gpg.Resolve()
	if err != nil {
		return refuseToolchain(err)
	}
	channel, err := toolchain.Check(context.Background(), toolchain.CheckRequest{
		InputDir: command.InputDir, ExpectChannel: command.ExpectChannel, Trust: trust,
		GPG: program,
	})
	if err != nil {
		return refuseToolchain(err)
	}
	fmt.Println(channel)
	return 0
}

// runInstall runs the downloading installer. The fixture grammar supplies its
// transport and runner; the shipped build's two methods return the production
// client and ExecRunner, and carry no seam name.
func runInstall(command *installCommand, trust toolchain.TrustPolicy,
	seams fixtureArguments) int {
	program, err := gpg.Resolve()
	if err != nil {
		return refuseToolchain(err)
	}
	factory, err := seams.transport()
	if err != nil {
		return refuseToolchain(err)
	}
	runner, err := seams.runner()
	if err != nil {
		return refuseToolchain(err)
	}
	request := toolchain.InstallRequest{
		InputDir: command.InputDir, Arch: command.Arch,
		ExpectChannel: command.ExpectChannel,
		Homes:         toolchain.Homes{Rustup: command.RustupHome, Cargo: command.CargoHome},
		Trust:         trust, GPG: program,
		TransportFactory: factory, Runner: runner,
	}
	if command.Events != nil {
		request.EventsPath = *command.Events
	}
	if command.WorkDir != nil {
		request.WorkDir, request.WorkDirSet = *command.WorkDir, true
	}
	channel, err := toolchain.Install(context.Background(), request)
	if err != nil {
		var signal *toolchain.SignalError
		if errors.As(err, &signal) {
			return 128 + int(signal.Signal)
		}
		return refuseToolchain(err)
	}
	fmt.Println(channel)
	return 0
}

// runVerifyInstalled runs the offline read and then the installed-state checks.
func runVerifyInstalled(command *verifyInstalledCommand, trust toolchain.TrustPolicy) int {
	program, err := gpg.Resolve()
	if err != nil {
		return refuseToolchain(err)
	}
	channel, err := toolchain.VerifyInstalled(context.Background(),
		toolchain.VerifyInstalledRequest{
			InputDir: command.InputDir, Arch: command.Arch,
			ExpectChannel: command.ExpectChannel,
			Homes:         toolchain.Homes{Rustup: command.RustupHome, Cargo: command.CargoHome},
			Trust:         trust, GPG: program,
		})
	if err != nil {
		return refuseToolchain(err)
	}
	fmt.Println(channel)
	return 0
}

// configureLogging installs the one diagnostic sink: a text handler on stderr.
// Contract lines are printed separately and never through this logger, because a
// logger quotes and escapes.
func configureLogging(level string) {
	parsed := slog.LevelWarn
	switch level {
	case "debug":
		parsed = slog.LevelDebug
	case "info":
		parsed = slog.LevelInfo
	case "warn", "":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed})))
}

// usageText is the one usage line. The shell suites match on it.
const usageText = "rust-dev-helper {kache-probe,check,install,verify-installed} ARGS"

// refuse reports one contract line on stderr and returns the refusal status. It is
// the kache-probe verb's prefix, kept as slice 1 wrote it.
func refuse(message string) int {
	fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, message)
	return 1
}

// refuseToolchain reports one `rust_toolchain: ` line: the frozen prefix of the
// reader verbs, and the one the suites match.
func refuseToolchain(err error) int {
	fmt.Fprintf(os.Stderr, "%s: %s\n", toolchainPrefix, err)
	return 1
}

// isToolchainVerb reports whether the parsed subcommand chain names one of the
// reader verbs ported from the deleted Python loader.
func isToolchainVerb(names []string) bool {
	return len(names) > 0 && (names[0] == "check" || names[0] == "install" ||
		names[0] == "verify-installed")
}

// refuseUsage reports a malformed invocation of a verb and returns 2, the status
// the source reader's argparse returned.
func refuseUsage(which string) int {
	fmt.Fprintf(os.Stderr, "%s: usage: %s\n", which, usageText)
	return 2
}
